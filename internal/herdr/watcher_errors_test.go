package herdr

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"cm2-claude/internal/herdr/herdrtest"
)

func healthy(t *testing.T, tune func(*Watcher)) (*herdrtest.Server, *harness) {
	t.Helper()
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	h := startHarness(t, srv, tune)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read built", reads(2))
	return srv, h
}

// F1: a cancel during a stalled snapshot ends Run with no publish and no report.
// UnavailableAfter is 1, so a failed read would publish Available=false if the
// cancel were treated as an ordinary failure.
func TestWatcherNoPublishAfterCancelDuringStalledSnapshot(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	srv.Stall(true)
	h := startHarness(t, srv, func(w *Watcher) {
		w.UnavailableAfter = 1
		w.Client.IOTimeout = time.Minute
	})
	srv.Await(t, "snapshot request stuck", func(s herdrtest.State) bool { return s.Calls["session.snapshot"] >= 1 })
	if err := h.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	select {
	case s := <-h.out:
		t.Fatalf("published after cancel: %+v", s)
	default:
	}
	if errs := h.errors(); len(errs) != 0 {
		t.Fatalf("cancellation reported to OnError: %v", errs)
	}
}

// F2: Herdr is up but a read fails once. The last snapshot stays, OnError hears
// of it, and nothing is published.
func TestWatcherSingleAPIErrorKeepsLastSnapshot(t *testing.T) {
	srv, h := healthy(t, nil)
	srv.FailNext("session.snapshot", "server_unavailable", "busy", 1)
	srv.DropSubscribers() // sends the watcher back to its top-level read, which gets the failure
	h.awaitErrors("server_unavailable", func(e []error) bool { return apiCodes(e)["server_unavailable"] >= 1 })
	srv.Await(t, "resubscribed", func(s herdrtest.State) bool { return len(s.SubRequests) == 2 && s.Subs == 1 })
	srv.Await(t, "reconcile after recovery", reads(4))
	h.expectNoPublish(func() {})
	if n := apiCodes(h.errors())["server_unavailable"]; n != 1 {
		t.Fatalf("server_unavailable reported %d times, want 1", n)
	}
}

// F2: three failures in a row publish Available=false, not fewer. A success
// after that publishes the real state again.
func TestWatcherThreeConsecutiveFailuresPublishUnavailable(t *testing.T) {
	srv, h := healthy(t, nil)
	srv.FailMethod("session.snapshot", "server_unavailable", "boom")
	srv.DropSubscribers()
	s := h.nextWhere("unavailable", func(s Snapshot) bool { return !s.Available })
	if len(s.Agents) != 0 {
		t.Fatalf("unavailable snapshot has agents: %+v", s)
	}
	h.mu.Lock()
	at := h.pubErr[len(h.pubErr)-1]
	seen := h.errs[:at]
	h.mu.Unlock()
	if n := apiCodes(seen)["server_unavailable"]; n != 3 {
		t.Fatalf("Available=false came after %d server_unavailable failures, want 3", n)
	}

	srv.ClearFail("session.snapshot")
	if s := h.nextWhere("recovered", func(s Snapshot) bool { return s.Available }); len(s.Agents) != 1 {
		t.Fatalf("recovered snapshot: %+v", s)
	}
}

// F2: the failure count is a field.
func TestWatcherUnavailableAfterIsConfigurable(t *testing.T) {
	srv, h := healthy(t, func(w *Watcher) { w.UnavailableAfter = 5 })
	srv.FailMethod("session.snapshot", "internal_error", "boom")
	srv.DropSubscribers()
	h.nextWhere("unavailable", func(s Snapshot) bool { return !s.Available })
	h.mu.Lock()
	at := h.pubErr[len(h.pubErr)-1]
	seen := h.errs[:at]
	h.mu.Unlock()
	if n := apiCodes(seen)["internal_error"]; n != 5 {
		t.Fatalf("Available=false came after %d failures, want 5", n)
	}
}

// F2: "Herdr is not running" is not a failure to count. It publishes at once,
// even when the failure count is very high.
func TestWatcherNotRunningPublishesUnavailableImmediately(t *testing.T) {
	srv, h := healthy(t, func(w *Watcher) { w.UnavailableAfter = 1000 })
	srv.Stop()
	h.nextWhere("unavailable", func(s Snapshot) bool { return !s.Available })
}

// F2: with nothing published yet, errors count the same way. A path that is too
// long is not "not running", so the first publish waits for the third failure.
func TestWatcherFirstPublishWaitsForThreeFailures(t *testing.T) {
	c := NewClient("/tmp/" + strings.Repeat("x", 150) + "/herdr.sock")
	h := startHarnessClient(t, nil, c, nil)
	s := h.next()
	if s.Available {
		t.Fatalf("first publish: %+v", s)
	}
	h.mu.Lock()
	at := h.pubErr[0]
	seen := h.errs[:at]
	h.mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("first publish after %d errors, want 3: %v", len(seen), seen)
	}
	for _, e := range seen {
		if errors.Is(e, ErrNotRunning) || !strings.Contains(e.Error(), "too long") {
			t.Errorf("unexpected error %v", e)
		}
	}
}

// F2: cancellation is not an error to report, and Run still returns ctx.Err().
func TestWatcherDoesNotReportCancellation(t *testing.T) {
	_, h := healthy(t, nil)
	if err := h.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if errs := h.errors(); len(errs) != 0 {
		t.Fatalf("OnError got %v on a clean cancel", errs)
	}
}

// F3: errors from the subscription and refresh paths reach OnError.
func TestWatcherReportsWatchErrors(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	srv.RejectNextSubscribe(1)
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed after one rejection", subscribed(1))
	h.awaitErrors("subscribe rejection", func(e []error) bool { return apiCodes(e)["pane_not_found"] == 1 })

	srv.Await(t, "reconcile read built", reads(3))
	srv.InjectEventsLost()
	h.awaitErrors("events_lost", func(e []error) bool { return apiCodes(e)["events_lost"] == 1 })
	srv.Await(t, "resubscribed", func(s herdrtest.State) bool { return len(s.SubRequests) == 3 && s.Subs == 1 })
	srv.Await(t, "reconcile read built", reads(5))

	before := len(h.errors())
	srv.DropSubscribers()
	h.awaitErrors("stream EOF", func(e []error) bool {
		for _, err := range e[before:] {
			var ae *APIError
			if !errors.As(err, &ae) {
				return true
			}
		}
		return false
	})
	srv.Await(t, "resubscribed", func(s herdrtest.State) bool { return len(s.SubRequests) == 4 && s.Subs == 1 })
	srv.Await(t, "reconcile read built", reads(7))

	srv.FailNext("session.snapshot", "server_unavailable", "busy", 1)
	srv.Push("pane.created", map[string]any{"pane_id": "w1:p9"})
	h.awaitErrors("refresh error", func(e []error) bool { return apiCodes(e)["server_unavailable"] == 1 })
}

// F3: while Herdr answers snapshots but the subscription keeps failing, state
// still refreshes at the poll rate. The backoff would be much longer by then.
func TestWatcherRefreshesAtPollRateWhileSubscribeFails(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	srv.RejectNextSubscribe(1 << 20)
	h := startHarness(t, srv, func(w *Watcher) {
		w.PollInterval = 30 * time.Millisecond
		w.BackoffMin = 5 * time.Millisecond
		w.BackoffMax = 10 * time.Second
	})
	h.next()
	srv.Await(t, "10 failed subscribe attempts", func(s herdrtest.State) bool { return len(s.SubRequests) >= 10 })
	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "blocked" })
	select {
	case s := <-h.out:
		if s.Agents[0].Status != StatusBlocked {
			t.Fatalf("status = %s", s.Agents[0].Status)
		}
	case <-time.After(time.Second):
		t.Fatal("no refresh within 1s although the poll interval is 30ms")
	}
}

// delayLog is the Watcher's sleep hook. It records each requested delay and
// then waits a real millisecond, so a loop cannot spin.
type delayLog struct {
	mu sync.Mutex
	ds []time.Duration
}

func (l *delayLog) sleep(ctx context.Context, d time.Duration) bool {
	l.mu.Lock()
	l.ds = append(l.ds, d)
	l.mu.Unlock()
	return sleep(ctx, time.Millisecond)
}

func (l *delayLog) all() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Duration(nil), l.ds...)
}

func (l *delayLog) await(t *testing.T, n int) []time.Duration {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ds := l.all(); len(ds) >= n {
			for _, d := range ds {
				if d <= 0 {
					t.Fatalf("non-positive delay in %v", ds)
				}
			}
			return ds[:n]
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d delays, have %v", n, l.all())
		}
		time.Sleep(time.Millisecond)
	}
}

func ms(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, v := range n {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func tuneBackoff(l *delayLog, poll time.Duration) func(*Watcher) {
	return func(w *Watcher) {
		w.sleepFn = l.sleep
		w.BackoffMin = 5 * time.Millisecond
		w.BackoffMax = 40 * time.Millisecond
		w.PollInterval = poll
	}
}

// T4: while Herdr is down the delay doubles from BackoffMin up to BackoffMax.
// A short PollInterval must not cap it, because Herdr is not reachable.
func TestWatcherBackoffGrowsToMaxWhileDown(t *testing.T) {
	for _, poll := range []time.Duration{time.Hour, 15 * time.Millisecond} {
		srv := herdrtest.New(t)
		srv.Stop()
		var l delayLog
		startHarness(t, srv, tuneBackoff(&l, poll))
		got := l.await(t, 7)
		if want := ms(5, 10, 20, 40, 40, 40, 40); !equalDurations(got, want) {
			t.Errorf("poll %v: delays %v, want %v", poll, got, want)
		}
	}
}

// T4: a successful subscribe resets the delay to BackoffMin.
func TestWatcherBackoffResetsAfterSuccessfulSubscribe(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	srv.Stop()
	var l delayLog
	startHarness(t, srv, tuneBackoff(&l, time.Hour))
	grown := l.await(t, 4)
	if want := ms(5, 10, 20, 40); !equalDurations(grown, want) {
		t.Fatalf("delays %v, want %v", grown, want)
	}
	srv.Start()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read built", reads(2)) // the subscribe has returned to the watcher
	n := len(l.all())                              // no sleeps happen while the stream is up
	srv.DropSubscribers()
	after := l.await(t, n+1)
	if after[n] != 5*time.Millisecond {
		t.Fatalf("delay after a successful subscribe = %v, want 5ms (all: %v)", after[n], after)
	}
}

// T4: Herdr answers snapshots but the subscribe keeps failing. The delay grows
// from BackoffMin but stops at PollInterval, not BackoffMax.
func TestWatcherBackoffCapsAtPollIntervalWhileReachable(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	srv.RejectNextSubscribe(1 << 20)
	var l delayLog
	startHarness(t, srv, func(w *Watcher) {
		tuneBackoff(&l, 15*time.Millisecond)(w)
		w.BackoffMax = 100 * time.Millisecond
	})
	got := l.await(t, 7)
	if want := ms(5, 10, 15, 15, 15, 15, 15); !equalDurations(got, want) {
		t.Fatalf("delays %v, want %v", got, want)
	}
}
