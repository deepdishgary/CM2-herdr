package herdr

import (
	"context"
	"errors"
	"slices"
	"time"
)

const (
	defaultPollInterval     = 3 * time.Second
	defaultDebounce         = 100 * time.Millisecond
	defaultBackoffMin       = 500 * time.Millisecond
	defaultBackoffMax       = 10 * time.Second
	defaultUnavailableAfter = 3
	maxResubscribes         = 5 // per refresh, so churning panes cannot spin
)

// Watcher keeps a live Snapshot. It follows Herdr's documented recovery
// pattern: subscribe, then read an authoritative snapshot, and treat events
// only as a signal to read again.
type Watcher struct {
	Client       *Client
	PollInterval time.Duration // safety-net re-read, default 3s
	Debounce     time.Duration // wait after the first event before a re-read, default 100ms
	BackoffMin   time.Duration // first retry delay, default 500ms
	BackoffMax   time.Duration // retry delay cap, default 10s

	// OnError, if set, hears every error that does not mean "Herdr is not
	// running": a failed snapshot read, a rejected or lost subscription, a
	// stream that ends. It never hears context cancellation. Run calls it from
	// its own goroutine, like onChange, so it should return quickly.
	OnError func(error)

	// UnavailableAfter is how many snapshot reads in a row must fail with such
	// an error before Run publishes Available=false. Default 3. "Herdr is not
	// running" publishes Available=false at once.
	UnavailableAfter int

	sleepFn func(ctx context.Context, d time.Duration) bool // test hook for the retry delay
}

func (w *Watcher) pollInterval() time.Duration { return orDefault(w.PollInterval, defaultPollInterval) }
func (w *Watcher) debounce() time.Duration     { return orDefault(w.Debounce, defaultDebounce) }
func (w *Watcher) backoffMin() time.Duration   { return orDefault(w.BackoffMin, defaultBackoffMin) }
func (w *Watcher) backoffMax() time.Duration {
	return max(orDefault(w.BackoffMax, defaultBackoffMax), w.backoffMin())
}

func (w *Watcher) unavailableAfter() int {
	if w.UnavailableAfter > 0 {
		return w.UnavailableAfter
	}
	return defaultUnavailableAfter
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Run watches Herdr until ctx ends, then returns ctx.Err(). It calls onChange
// from this goroutine, once at start and again each time the state changes in
// a way that matters. onChange should return quickly.
//
// The first onChange call comes after the first snapshot read ends. If Herdr
// answers slowly or not at all (a hung Herdr), that read can take up to the
// client's IOTimeout, and Run publishes no placeholder in the meantime.
//
// Run publishes Available=false only when Herdr is not running (the socket is
// missing or refuses). Any other failure of a snapshot read (an error reply
// such as server_unavailable, an I/O timeout, a bad or over-long reply, a
// socket path that is too long) keeps the last published snapshot and goes to
// OnError. After UnavailableAfter such failures in a row (default 3), Run
// publishes Available=false. When nothing has been published yet, the first
// call waits for that third failure or for a success.
//
// Errors from the subscription and from refreshes (a rejected subscribe,
// events_lost, a stream that ends) go to OnError too. After one, Run starts
// over at the snapshot read. The wait before that is exponential, from
// BackoffMin to BackoffMax, while Herdr is not reachable. While Herdr is
// reachable (the last snapshot read succeeded) the wait never exceeds
// PollInterval, so state still refreshes at the poll rate when the
// subscription keeps failing.
func (w *Watcher) Run(ctx context.Context, onChange func(Snapshot)) error {
	var last *Snapshot
	publish := func(s Snapshot) {
		if ctx.Err() != nil {
			return // cancelled: a read that finished late must not publish
		}
		if last == nil || !sameState(*last, s) {
			last = &s
			onChange(s)
		}
	}
	report := func(err error) {
		if err != nil && w.OnError != nil && ctx.Err() == nil {
			w.OnError(err)
		}
	}
	wait := sleep
	if w.sleepFn != nil {
		wait = w.sleepFn
	}
	backoff := w.backoffMin()
	failures := 0 // snapshot reads in a row that failed for a reason other than "not running"
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		snap, err := w.Client.Snapshot(ctx)
		if err := ctx.Err(); err != nil {
			return err // the read was cut short or finished too late: publish nothing
		}
		reachable := false
		switch {
		case errors.Is(err, ErrNotRunning), err == nil && !snap.Available:
			failures = 0
			publish(Snapshot{})
		case err != nil:
			failures++
			report(err)
			if failures >= w.unavailableAfter() {
				publish(Snapshot{})
			}
		default:
			failures = 0
			reachable = true
			publish(snap)
			// Any error: start over. "Not running" is not reported; the next
			// read publishes Available=false instead.
			if err := w.watch(ctx, snap, publish, func() { backoff = w.backoffMin() }); !errors.Is(err, ErrNotRunning) {
				report(err)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		delay := backoff
		if reachable {
			delay = min(delay, w.pollInterval())
		}
		if !wait(ctx, delay) {
			return ctx.Err()
		}
		backoff = min(backoff*2, w.backoffMax())
	}
}

// sleep waits for d or until ctx ends. It reports whether the full time passed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// liveSub is a subscription plus the goroutine that reads it. Events only set
// the dirty flag, so a slow refresh never makes Herdr drop us for lag.
type liveSub struct {
	sub   *subscription
	panes []string
	dirty chan struct{} // capacity 1: "something happened"
	errc  chan error    // capacity 1: why the stream ended
	done  chan struct{} // closed when the reader goroutine exits
}

func (w *Watcher) open(ctx context.Context, panes []string) (*liveSub, error) {
	sub, err := w.Client.subscribe(ctx, panes)
	if err != nil {
		return nil, err
	}
	ls := &liveSub{
		sub: sub, panes: panes,
		dirty: make(chan struct{}, 1), errc: make(chan error, 1), done: make(chan struct{}),
	}
	go func() {
		defer close(ls.done)
		for {
			if _, err := sub.next(); err != nil {
				ls.errc <- err
				return
			}
			select {
			case ls.dirty <- struct{}{}:
			default:
			}
		}
	}()
	return ls, nil
}

// close stops the stream and waits for the reader goroutine.
func (ls *liveSub) close() {
	ls.sub.close()
	<-ls.done
}

// watch runs steps 2 to 7 of the algorithm. It returns when the stream or a
// read fails, or when ctx ends. The caller then starts again at step 1.
func (w *Watcher) watch(ctx context.Context, first Snapshot, publish func(Snapshot), started func()) error {
	cur, err := w.open(ctx, paneSet(first))
	if err != nil {
		return err
	}
	defer func() { cur.close() }()
	started()

	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	armDebounce := func() {
		if timerC == nil {
			timer = time.NewTimer(w.debounce())
			timerC = timer.C
		}
	}

	// refresh reads the authoritative snapshot, publishes it, and moves the
	// subscription to the new pane set if the set changed.
	refresh := func() error {
		for i := 0; ; i++ {
			snap, err := w.Client.Snapshot(ctx)
			if err != nil {
				return err
			}
			if !snap.Available {
				return ErrNotRunning
			}
			publish(snap)
			panes := paneSet(snap)
			if slices.Equal(panes, cur.panes) {
				return nil
			}
			if i >= maxResubscribes {
				armDebounce()
				return nil
			}
			next, err := w.open(ctx, panes) // open the new stream before closing the old one
			if err != nil {
				return err
			}
			old := cur
			cur = next
			old.close()
			// Loop: read again to cover anything between the snapshot and the subscribe.
		}
	}

	if err := refresh(); err != nil { // step 3
		return err
	}
	poll := time.NewTicker(w.pollInterval())
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-cur.errc:
			return err
		case <-cur.dirty:
			armDebounce()
		case <-timerC:
			timerC = nil
			if err := refresh(); err != nil {
				return err
			}
		case <-poll.C:
			if err := refresh(); err != nil {
				return err
			}
		}
	}
}
