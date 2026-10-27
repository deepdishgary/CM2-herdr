package herdr

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"cm2-claude/internal/herdr/herdrtest"
)

// connCounter wraps a client's dialer. It counts connections opened and closed.
type connCounter struct{ opened, closed atomic.Int64 }

func countConns(c *Client) *connCounter {
	cc := &connCounter{}
	c.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		cc.opened.Add(1)
		return &countConn{Conn: conn, closed: &cc.closed}, nil
	}
	return cc
}

func (cc *connCounter) check(t *testing.T, what string) {
	t.Helper()
	if o, c := cc.opened.Load(), cc.closed.Load(); o != c || o == 0 {
		t.Fatalf("%s: opened %d connections, closed %d", what, o, c)
	}
}

// awaitLive waits until exactly want connections are open: opened minus closed.
// The context hook closes a leaked connection when Run ends, so the count has to
// be right while the Watcher is still running.
func (cc *connCounter) awaitLive(t *testing.T, what string, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for cc.opened.Load()-cc.closed.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d connections open (opened %d, closed %d), want %d",
				what, cc.opened.Load()-cc.closed.Load(), cc.opened.Load(), cc.closed.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// settleFDs waits for the server side of closed connections to go away, then
// requires the open fd count to be back at (or below) the starting count.
func settleFDs(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for openFDs(t) > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := openFDs(t); after > before {
		t.Fatalf("fd leak: %d open before, %d after", before, after)
	}
}

// T3: every way a subscribe can fail closes the connection it opened.
func TestSubscribeFailurePathsCloseTheConnection(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*herdrtest.Server, *Client)
	}{
		{"rejection", func(s *herdrtest.Server, c *Client) { s.RejectNextSubscribe(1 << 20) }},
		{"unacknowledged reply", func(s *herdrtest.Server, c *Client) {
			s.SetRaw("events.subscribe", `{"result":{"type":"pong"}}`+"\n")
		}},
		{"error reply", func(s *herdrtest.Server, c *Client) {
			s.FailMethod("events.subscribe", "internal_error", "boom")
		}},
		{"garbage reply", func(s *herdrtest.Server, c *Client) { s.SetRaw("events.subscribe", "not json\n") }},
		{"truncated reply", func(s *herdrtest.Server, c *Client) { s.SetRaw("events.subscribe", `{"id":"x"`) }},
		{"I/O timeout", func(s *herdrtest.Server, c *Client) {
			s.Stall(true)
			c.IOTimeout = 20 * time.Millisecond
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, srv := newClient(t)
			srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
			cc := countConns(c)
			tc.setup(srv, c)
			ctx := ctxT(t)
			c.subscribe(ctx, []string{"w1:p1"}) // warm up
			before := openFDs(t)
			cc.opened.Store(0)
			cc.closed.Store(0)
			for i := 0; i < 20; i++ {
				if sub, err := c.subscribe(ctx, []string{"w1:p1"}); err == nil {
					sub.close()
					t.Fatal("subscribe should have failed")
				}
			}
			if cc.opened.Load() != 20 {
				t.Fatalf("opened %d connections, want 20", cc.opened.Load())
			}
			cc.check(t, tc.name)
			settleFDs(t, before)
		})
	}
}

// T3: the success path and the cancel path close too.
func TestSubscribeSuccessAndCancelCloseTheConnection(t *testing.T) {
	c, srv := newClient(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	cc := countConns(c)
	sub, err := c.subscribe(ctxT(t), []string{"w1:p1"})
	if err != nil {
		t.Fatal(err)
	}
	sub.close()
	sub.close() // twice is fine
	cc.check(t, "close")

	cc.opened.Store(0)
	cc.closed.Store(0)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := c.subscribe(ctx, []string{"w1:p1"}); err != nil {
		t.Fatal(err)
	}
	cancel() // closes the connection through the context hook
	deadline := time.Now().Add(5 * time.Second)
	for cc.closed.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cc.check(t, "cancel")
}

// T3: a Watcher that runs through rejections, events_lost, a dropped stream and
// resubscribes closes every connection it opens, and leaves no fd behind.
func TestWatcherClosesEveryConnection(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	{ // warm up the runtime's pollers so they do not count as a leak
		c := NewClient(srv.Path())
		c.Ping(ctxT(t))
	}
	before := openFDs(t)
	startAccepted := srv.State().Accepted

	var cc *connCounter
	srv.RejectNextSubscribe(3)
	h := startHarness(t, srv, func(w *Watcher) { cc = countConns(w.Client) })
	h.next()
	srv.Await(t, "subscribed after 3 rejections", func(s herdrtest.State) bool { return len(s.SubRequests) == 4 && s.Subs == 1 })
	cc.awaitLive(t, "after 3 rejections", 1) // only the live stream is open

	srv.InjectEventsLost()
	srv.Await(t, "resubscribed after events_lost", func(s herdrtest.State) bool { return len(s.SubRequests) == 5 && s.Subs == 1 })
	cc.awaitLive(t, "after events_lost", 1)

	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"), herdrtest.NewAgent("w1:p2", "claude", "idle"))
	srv.Push("pane.created", map[string]any{"pane_id": "w1:p2"})
	srv.Await(t, "resubscribed for the new pane", func(s herdrtest.State) bool { return len(s.SubRequests) == 6 && s.Subs == 1 })
	cc.awaitLive(t, "after the new-pane resubscribe", 1)

	srv.DropSubscribers()
	srv.Await(t, "resubscribed after a drop", func(s herdrtest.State) bool { return len(s.SubRequests) == 7 && s.Subs == 1 })
	cc.awaitLive(t, "after a dropped stream", 1)

	h.stop()
	cc.check(t, "watcher")
	if got := srv.State().Accepted - startAccepted; got != int(cc.opened.Load()) {
		t.Fatalf("server accepted %d, watcher opened %d", got, cc.opened.Load())
	}
	srv.Await(t, "all streams closed", subscribed(0))
	settleFDs(t, before)
}
