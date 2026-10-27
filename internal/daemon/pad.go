package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"cm2-claude/internal/device"
	"cm2-claude/internal/painter"
)

var errNoPad = errors.New("daemon: pad not connected")

// padLights is the painter's Lights. It sends to whichever client is current.
type padLights struct{ cur atomic.Pointer[device.Client] }

func (l *padLights) SetThreads(ctx context.Context, th []painter.Thread) error {
	c := l.cur.Load()
	if c == nil {
		return errNoPad
	}
	return painter.NewDeviceLights(c).SetThreads(ctx, th)
}

// onceLog logs a message only when it differs from the last one.
type onceLog struct {
	mu   sync.Mutex
	last string
}

func (o *onceLog) warn(log *slog.Logger, msg string, err error, args ...any) {
	if err == nil {
		o.mu.Lock()
		o.last = ""
		o.mu.Unlock()
		return
	}
	o.mu.Lock()
	same := o.last == err.Error()
	o.last = err.Error()
	o.mu.Unlock()
	if !same {
		log.Warn(msg, append(args, "err", err)...)
	}
}

// padKey is a pad notification with the time it arrived. The run loop may be
// busy when it reads the key, and the wide-key debounce needs the arrival time.
type padKey struct {
	n  device.Notification
	at time.Time
}

// pad supervises the connection to the pad: open, read, reopen.
type pad struct {
	open    func() (device.Transport, error)
	log     *slog.Logger
	lights  *padLights
	retry   time.Duration
	pause   time.Duration
	keys    chan<- padKey
	repaint chan<- struct{}
	onGone  func() // called after the pad is gone; may be nil

	connected   atomic.Bool
	pausedUntil atomic.Int64 // unix nano
	openErr     onceLog
}

// paused reports whether painting is stopped because another program is
// writing to the pad.
func (p *pad) paused(now time.Time) bool { return now.UnixNano() < p.pausedUntil.Load() }

// kick closes the current client. Its serve loop then ends and run opens the
// pad again. The daemon uses it when the pad takes calls but does not answer.
func (p *pad) kick() {
	if c := p.lights.cur.Load(); c != nil {
		c.Close()
	}
}

// onForeign runs on the client's reader goroutine.
func (p *pad) onForeign(id int64, raw []byte) {
	now := time.Now()
	prev := p.pausedUntil.Swap(now.Add(p.pause).UnixNano())
	if prev < now.UnixNano() {
		p.log.Warn("another program is talking to the pad; painting paused",
			"reply_id", id, "pause", p.pause)
	}
}

// run keeps a client open until ctx ends. On exit it closes the client.
func (p *pad) run(ctx context.Context) {
	for ctx.Err() == nil {
		tr, err := p.open()
		if err != nil {
			attrs := []any{}
			if errors.Is(err, device.ErrInputMonitoring) {
				attrs = append(attrs, "hint", "grant Input Monitoring to this binary in System Settings > Privacy & Security")
			}
			p.openErr.warn(p.log, "cannot open the pad; retrying", err, attrs...)
			if !sleepCtx(ctx, p.retry) {
				return
			}
			continue
		}
		p.openErr.warn(p.log, "", nil)
		p.serve(ctx, tr)
		if !sleepCtx(ctx, p.retry) {
			return
		}
	}
}

// serve runs one connection until it ends or ctx ends.
func (p *pad) serve(ctx context.Context, tr device.Transport) {
	c := device.NewClient(tr, device.ClientOptions{OnForeign: p.onForeign})
	defer c.Close()
	p.lights.cur.Store(c)
	p.connected.Store(true)
	p.log.Info("pad connected")
	select {
	case p.repaint <- struct{}{}:
	default:
	}
	defer func() {
		p.connected.Store(false)
		p.lights.cur.Store(nil)
		p.log.Info("pad disconnected")
		if p.onGone != nil {
			p.onGone()
		}
	}()
	for {
		select {
		case n, ok := <-c.Notifications():
			if !ok {
				return
			}
			select {
			case p.keys <- padKey{n: n, at: time.Now()}:
			case <-ctx.Done():
				return
			}
		case <-c.Done():
			return
		case <-ctx.Done():
			return
		}
	}
}

// sleepCtx waits for d or ctx. It reports whether the time passed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
