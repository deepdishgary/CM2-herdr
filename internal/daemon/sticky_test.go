package daemon_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"cm2-claude/internal/daemon"
	"cm2-claude/internal/herdr/herdrtest"
)

// Seeded Herdr sessions keep their keys while the daemon clock runs for
// hours. Each call of the fake clock moves it on by one minute.
func TestSeededHerdrSessionsKeepKeysAcrossHours(t *testing.T) {
	hs := herdrtest.New(t)
	var as []herdrtest.Agent
	for i := 0; i < 7; i++ {
		a := herdrtest.NewAgent(fmt.Sprintf("w1:p%d", i+1), "claude", "idle")
		a["agent_session"] = map[string]any{"source": "hook", "agent": "claude", "kind": "id", "value": fmt.Sprintf("H%d", i)}
		as = append(as, a)
	}
	hs.SetAgents(as...)
	var calls atomic.Int64
	base := time.Now()
	h := newHarness(t, opts{herdr: hs, cfg: func(c *daemon.Config) {
		c.Now = func() time.Time {
			return base.Add(time.Duration(calls.Add(1)) * time.Minute)
		}
	}})
	eventually(t, "seven sessions", func() bool { return len(h.status().Sessions) == 7 })
	// Select a key: freeing and retaking a key would clear the selection.
	h.press("AG03")
	eventually(t, "key 3 selected", func() bool { return h.status().Selected == 3 })
	start := calls.Load()
	// Many 20 ms ticks and Herdr polls: well over 30 minutes of fake time.
	eventually(t, "two fake hours", func() bool { return calls.Load()-start > 120 })
	for i := 0; i < 7; i++ {
		if s := h.sess(fmt.Sprintf("claude:H%d", i)); s == nil || s.Slot != i {
			t.Fatalf("H%d = %+v", i, s)
		}
	}
	if got := h.status().Selected; got != 3 {
		t.Fatalf("selected = %d, want 3", got)
	}
}
