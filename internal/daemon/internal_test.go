package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"cm2-claude/internal/device"
	"cm2-claude/internal/device/devicetest"
)

// The wide key reports its two halves about 5 ms apart. They must carry the
// time they arrived, so a busy run loop does not turn them into two presses.
func TestPadStampsKeysOnArrival(t *testing.T) {
	keys := make(chan padKey, 8)
	f := devicetest.New()
	p := &pad{
		open:    func() (device.Transport, error) { return f, nil },
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		lights:  &padLights{},
		retry:   time.Hour,
		pause:   time.Second,
		keys:    keys,
		repaint: make(chan struct{}, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(3 * time.Second)
	for !p.connected.Load() {
		if time.Now().After(deadline) {
			t.Fatal("pad not connected")
		}
		time.Sleep(time.Millisecond)
	}

	f.Inject([]byte(`{"m":"v.oai.hid","p":{"k":"AG10","act":1}}`))
	time.Sleep(5 * time.Millisecond)
	f.Inject([]byte(`{"m":"v.oai.hid","p":{"k":"AG11","act":1}}`))
	time.Sleep(250 * time.Millisecond) // the consumer is busy

	a, b := <-keys, <-keys
	if gap := b.at.Sub(a.at); gap < 0 || gap > 80*time.Millisecond {
		t.Fatalf("the halves are stamped %v apart, want about 5 ms", gap)
	}
	if time.Since(a.at) < 200*time.Millisecond {
		t.Fatalf("stamp is the read time, not the arrival time")
	}
}

// kick closes the current client, so serve returns and the supervisor reopens.
func TestPadKickClosesTheClient(t *testing.T) {
	opens := make(chan *devicetest.Fake, 4)
	p := &pad{
		open: func() (device.Transport, error) {
			f := devicetest.New()
			opens <- f
			return f, nil
		},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		lights:  &padLights{},
		retry:   10 * time.Millisecond,
		pause:   time.Second,
		keys:    make(chan padKey, 8),
		repaint: make(chan struct{}, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	first := <-opens
	deadline := time.Now().Add(3 * time.Second)
	for !p.connected.Load() {
		if time.Now().After(deadline) {
			t.Fatal("pad not connected")
		}
		time.Sleep(time.Millisecond)
	}
	p.kick()
	select {
	case <-opens:
	case <-time.After(3 * time.Second):
		t.Fatal("no second open after kick")
	}
	if err := first.Write(make([]byte, 64)); err == nil {
		t.Fatal("the first pad was not closed")
	}
}

func TestConfigDefaults(t *testing.T) {
	var c Config
	c.defaults()
	if c.Addr != "127.0.0.1:47731" || DefaultAddr != "127.0.0.1:47731" {
		t.Errorf("Addr = %q, DefaultAddr = %q", c.Addr, DefaultAddr)
	}
	if c.RetryEvery != 3*time.Second {
		t.Errorf("RetryEvery = %v, want 3s", c.RetryEvery)
	}
	if c.ForeignPause != 30*time.Second {
		t.Errorf("ForeignPause = %v, want 30s", c.ForeignPause)
	}
	if c.TickEvery != time.Second {
		t.Errorf("TickEvery = %v, want 1s", c.TickEvery)
	}
	if c.CallTimeout != 2*time.Second {
		t.Errorf("CallTimeout = %v, want 2s", c.CallTimeout)
	}
	if c.Table.Slots != 10 {
		t.Errorf("Slots = %d, want 10", c.Table.Slots)
	}
}
