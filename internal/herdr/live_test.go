//go:build herdrlive

package herdr

import (
	"context"
	"errors"
	"testing"
	"time"
)

// These tests talk to the real Herdr on this Mac. They send only ping,
// session.snapshot and events.subscribe. They never focus anything.

func liveClient(t *testing.T) *Client {
	t.Helper()
	c := NewClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Ping(ctx); err != nil {
		if errors.Is(err, ErrNotRunning) {
			t.Skipf("Herdr is not running: %v", err)
		}
		t.Fatalf("ping: %v", err)
	}
	return c
}

func TestLivePingAndSnapshot(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, p, err := c.Ping(ctx)
	if err != nil || v == "" || p == 0 {
		t.Fatalf("ping = %q %d %v", v, p, err)
	}
	snap, err := c.Snapshot(ctx)
	if err != nil || !snap.Available {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	t.Logf("herdr %s protocol %d, %d agents, focused %q", snap.Version, snap.Protocol, len(snap.Agents), snap.FocusedPaneID)
	for _, a := range snap.Agents {
		if a.PaneID == "" || a.WorkspaceID == "" || a.Status == "" {
			t.Errorf("incomplete agent: %+v", a)
		}
		t.Logf("  %s kind=%q status=%s focused=%v session=%q cwd=%q", a.PaneID, a.Kind, a.Status, a.Focused, a.ClaudeSessionID(), a.CWD)
	}
}

func TestLiveSubscribeThreeSeconds(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	snap, err := c.Snapshot(ctx)
	if err != nil || !snap.Available {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	sub, err := c.subscribe(ctx, paneSet(snap))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.close()
	events := 0
	for {
		kind, err := sub.next()
		if err != nil {
			break // the 3 s context ended and closed the connection
		}
		events++
		t.Logf("event: %s", kind)
	}
	if ctx.Err() == nil {
		t.Fatal("stream ended before 3 s")
	}
	t.Logf("%d events in 3 s", events)
}

func TestLiveWatcherThreeSeconds(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w := &Watcher{Client: c}
	var got []Snapshot
	err := w.Run(ctx, func(s Snapshot) { got = append(got, s) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v", err)
	}
	if len(got) == 0 || !got[0].Available {
		t.Fatalf("publishes: %+v", got)
	}
	t.Logf("%d publishes; first had %d agents", len(got), len(got[0].Agents))
}
