package herdr

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"cm2-claude/internal/herdr/herdrtest"
)

// cancelConn cancels the Watcher's context as soon as a full snapshot reply
// has been read. The read itself succeeds, but it finishes after cancellation.
type cancelConn struct {
	net.Conn
	arm    *atomic.Bool
	cancel func()
	isSnap atomic.Bool
}

func (c *cancelConn) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte(`"session.snapshot"`)) {
		c.isSnap.Store(true)
	}
	return c.Conn.Write(b)
}

func (c *cancelConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.isSnap.Load() && bytes.IndexByte(b[:n], '\n') >= 0 && c.arm.CompareAndSwap(true, false) {
		c.cancel()
	}
	return n, err
}

// A refresh read that completes just as Run is cancelled must not publish.
func TestWatcherNoPublishWhenCancelledDuringRefreshRead(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	c := NewClient(srv.Path())
	var arm atomic.Bool
	var cancel atomic.Value // func()
	c.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &cancelConn{Conn: conn, arm: &arm, cancel: func() { cancel.Load().(func())() }}, nil
	}
	h := startHarnessClient(t, srv, c, nil)
	cancel.Store(func() { h.cancel() })
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read built", reads(2))
	time.Sleep(50 * time.Millisecond) // let the client finish consuming the reconcile reply

	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "blocked" })
	arm.Store(true)
	srv.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "blocked"})

	select {
	case err := <-h.done:
		h.done <- err
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not end after the armed read")
	}
	if arm.Load() {
		t.Fatal("the refresh read never happened")
	}
	for {
		select {
		case s := <-h.out:
			if len(s.Agents) == 1 && s.Agents[0].Status == StatusBlocked {
				t.Fatalf("published after cancel: %+v", s)
			}
		default:
			return
		}
	}
}

// When a refresh finds Herdr gone, the watcher publishes Available=false and
// does not also report "not running" to OnError.
func TestWatcherRefreshNotRunningIsNotReported(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	c := NewClient(srv.Path())
	var refuse atomic.Bool
	c.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if refuse.Load() {
			return nil, &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		}
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
	}
	h := startHarnessClient(t, srv, c, nil)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read built", reads(2))

	refuse.Store(true) // the open stream stays up; new connections are refused
	srv.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "idle"})
	h.nextWhere("Available=false", func(s Snapshot) bool { return !s.Available })
	for _, err := range h.errors() {
		if errors.Is(err, ErrNotRunning) {
			t.Fatalf("OnError got %q; 'not running' must only publish Available=false", err)
		}
	}
}
