package session_test

import (
	"fmt"
	"testing"
	"time"

	"cm2-claude/internal/herdr"
	"cm2-claude/internal/session"
)

// desktopEv builds a hook event for an attended desktop session.
func desktopEv(name, id string, at time.Time) session.HookEvent {
	e := ev(name, id, at)
	e.Entrypoint = "claude-desktop"
	return e
}

// idleDesktop makes an idle, already seen desktop session with a key.
func idleDesktop(tb *session.Table, id string, at time.Time) {
	tb.ApplyHook(desktopEv("UserPromptSubmit", id, at))
	tb.ApplyHook(desktopEv("Stop", id, at))
	tb.Focused(key(id), at)
}

// idleHerdr makes a hook-known idle Claude session in a Herdr pane.
func idleHerdr(tb *session.Table, id, pane string, at time.Time) {
	e := ev("UserPromptSubmit", id, at)
	e.HerdrPane = pane
	tb.ApplyHook(e)
	tb.ApplyHook(ev("Stop", id, at))
	tb.Focused(key(id), at)
}

func TestIdleKeysStayForever(t *testing.T) {
	tb := newTable(session.Config{})
	idleDesktop(tb, "d", t0)
	idleHerdr(tb, "h", "p1", t0)
	tb.ApplyHerdr(snap(
		agent("p1", "claude", herdr.StatusIdle, "h"),
		agent("p2", "claude", herdr.StatusIdle, "seed"),
		agent("p3", "codex", herdr.StatusDone, ""),
	), t0)
	want := map[string]int{key("d"): 0, key("h"): 1, key("seed"): 2, "herdr:p3": 3}
	for k, slot := range want {
		if slotOf(tb, k) != slot {
			t.Fatalf("%s slot = %d, want %d", k, slotOf(tb, k), slot)
		}
	}
	tb.Select(1)
	for _, d := range []time.Duration{time.Hour, 24 * time.Hour, 72 * time.Hour} {
		tb.Tick(t0.Add(d))
		tb.ApplyHerdr(snap(
			agent("p1", "claude", herdr.StatusIdle, "h"),
			agent("p2", "claude", herdr.StatusIdle, "seed"),
			agent("p3", "codex", herdr.StatusDone, ""),
		), t0.Add(d))
		for k, slot := range want {
			if slotOf(tb, k) != slot {
				t.Fatalf("at +%v: %s slot = %d, want %d", d, k, slotOf(tb, k), slot)
			}
		}
		if tb.Selected() != 1 {
			t.Fatalf("at +%v: selection = %d", d, tb.Selected())
		}
	}
}

func TestStickyEvictionLRUIdle(t *testing.T) {
	tb := newTable(session.Config{})
	for i := 0; i < 10; i++ {
		idleDesktop(tb, fmt.Sprintf("s%d", i), sec(i))
	}
	tb.Tick(t0.Add(48 * time.Hour)) // idle keys stay, even after two days
	tb.ApplyHook(desktopEv("UserPromptSubmit", "n", t0.Add(49*time.Hour)))
	if slotOf(tb, key("n")) != 0 || slotOf(tb, key("s0")) != -1 {
		t.Fatalf("n=%d s0=%d", slotOf(tb, key("n")), slotOf(tb, key("s0")))
	}
	for i := 1; i < 10; i++ {
		if slotOf(tb, key(fmt.Sprintf("s%d", i))) != i {
			t.Fatalf("s%d lost its key", i)
		}
	}
}

func TestStickyNeverEvictsDoneWorkingNeedsInputError(t *testing.T) {
	prep := map[string]func(tb *session.Table, id string){
		"done": func(tb *session.Table, id string) {
			tb.ApplyHook(ev("UserPromptSubmit", id, t0))
			tb.ApplyHook(ev("Stop", id, t0))
		},
		"working": func(tb *session.Table, id string) { tb.ApplyHook(ev("UserPromptSubmit", id, t0)) },
		"needs-input": func(tb *session.Table, id string) {
			tb.ApplyHook(perm(id, "r-"+id, t0))
		},
		"error": func(tb *session.Table, id string) {
			tb.ApplyHook(ev("UserPromptSubmit", id, t0))
			tb.ApplyHook(ev("StopFailure", id, t0))
		},
	}
	for name, p := range prep {
		t.Run(name, func(t *testing.T) {
			tb := newTable(session.Config{})
			for i := 0; i < 10; i++ {
				p(tb, fmt.Sprintf("s%d", i))
			}
			tb.Tick(t0.Add(5 * time.Hour)) // before the 6 h stuck cap
			tb.ApplyHook(ev("UserPromptSubmit", "n", t0.Add(5*time.Hour+time.Minute)))
			if slotOf(tb, key("n")) != -1 {
				t.Fatalf("n took slot %d", slotOf(tb, key("n")))
			}
			for i := 0; i < 10; i++ {
				if slotOf(tb, key(fmt.Sprintf("s%d", i))) != i {
					t.Fatalf("s%d lost its key", i)
				}
			}
		})
	}
}

func TestEvictedHerdrSessionRegainsFreeKey(t *testing.T) {
	tb := newTable(session.Config{})
	var as []herdr.Agent
	for i := 0; i < 10; i++ {
		as = append(as, agent(fmt.Sprintf("p%d", i), "claude", herdr.StatusIdle, fmt.Sprintf("h%d", i)))
	}
	tb.ApplyHerdr(snap(as...), t0)
	tb.ApplyHook(desktopEv("UserPromptSubmit", "n", min(1)))
	if slotOf(tb, key("n")) != 0 || slotOf(tb, key("h0")) != -1 {
		t.Fatalf("n=%d h0=%d", slotOf(tb, key("n")), slotOf(tb, key("h0")))
	}
	tb.Tick(min(2))
	if slotOf(tb, key("h0")) != -1 {
		t.Fatal("h0 took a key with none free")
	}
	// n ends. Its key frees after the grace, and h0 takes it with no new activity.
	tb.ApplyHook(desktopEv("SessionEnd", "n", min(3)))
	tb.Tick(min(14))
	if tb.Get(key("n")) != nil {
		t.Fatal("n not removed")
	}
	if slotOf(tb, key("h0")) != 0 {
		t.Fatalf("h0 = %d, want 0", slotOf(tb, key("h0")))
	}
}

func TestEvictedNonClaudeHerdrAgentRegainsFreeKey(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(agent("p0", "codex", herdr.StatusIdle, "")), t0)
	for i := 1; i < 10; i++ {
		idleDesktop(tb, fmt.Sprintf("s%d", i), sec(i))
	}
	tb.ApplyHook(desktopEv("UserPromptSubmit", "n", min(1)))
	if slotOf(tb, "herdr:p0") != -1 {
		t.Fatal("codex agent not evicted")
	}
	tb.ApplyHook(desktopEv("SessionEnd", "n", min(2)))
	tb.Tick(min(13))
	if slotOf(tb, "herdr:p0") != 0 {
		t.Fatalf("codex agent = %d, want 0", slotOf(tb, "herdr:p0"))
	}
}

func TestEvictedDesktopSessionStaysKeyless(t *testing.T) {
	tb := newTable(session.Config{})
	for i := 0; i < 10; i++ {
		idleDesktop(tb, fmt.Sprintf("s%d", i), sec(i))
	}
	tb.ApplyHook(desktopEv("UserPromptSubmit", "n", min(1)))
	if slotOf(tb, key("s0")) != -1 {
		t.Fatal("s0 not evicted")
	}
	tb.ApplyHook(desktopEv("SessionEnd", "n", min(2)))
	tb.Tick(min(13))
	tb.Tick(min(14))
	if slotOf(tb, key("s0")) != -1 {
		t.Fatal("desktop session took a key with no activity")
	}
	tb.ApplyHook(desktopEv("UserPromptSubmit", "s0", min(15)))
	if slotOf(tb, key("s0")) != 0 {
		t.Fatalf("s0 = %d after activity, want 0", slotOf(tb, key("s0")))
	}
}

func TestHerdrReclaimTakesFreeKey(t *testing.T) {
	tb := newTable(session.Config{})
	fill(tb, 10) // ten working sessions
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "h")), min(1))
	if slotOf(tb, key("h")) != -1 {
		t.Fatalf("h = %d, want -1", slotOf(tb, key("h")))
	}
	tb.ApplyHook(ev("SessionEnd", "s0", min(3)))
	tb.ApplyHook(ev("SessionEnd", "s1", min(3)))
	tb.Tick(min(14))
	if slotOf(tb, key("h")) != 0 {
		t.Fatalf("h = %d, want 0", slotOf(tb, key("h")))
	}
}

func TestHerdrReclaimMostRecentFirst(t *testing.T) {
	tb := newTable(session.Config{})
	fill(tb, 10)
	a0 := agent("p0", "claude", herdr.StatusIdle, "h0")
	a1 := agent("p1", "claude", herdr.StatusIdle, "h1")
	a2 := agent("p2", "codex", herdr.StatusIdle, "")
	tb.ApplyHerdr(snap(a0), min(1))
	tb.ApplyHerdr(snap(a0, a1), min(2))
	tb.ApplyHerdr(snap(a0, a1, a2), min(3))
	for _, k := range []string{key("h0"), key("h1"), "herdr:p2"} {
		if slotOf(tb, k) != -1 {
			t.Fatalf("%s took a key with none free", k)
		}
	}
	tb.ApplyHook(ev("SessionEnd", "s4", min(4)))
	tb.ApplyHook(ev("SessionEnd", "s7", min(4)))
	tb.Tick(min(15))
	if slotOf(tb, "herdr:p2") != 4 || slotOf(tb, key("h1")) != 7 || slotOf(tb, key("h0")) != -1 {
		t.Fatalf("p2=%d h1=%d h0=%d", slotOf(tb, "herdr:p2"), slotOf(tb, key("h1")), slotOf(tb, key("h0")))
	}
}

func TestReclaimNeverEvicts(t *testing.T) {
	tb := newTable(session.Config{})
	for i := 0; i < 10; i++ {
		idleDesktop(tb, fmt.Sprintf("s%d", i), sec(i))
	}
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "h")), min(1))
	tb.Tick(min(60))
	if slotOf(tb, key("h")) != -1 {
		t.Fatalf("h = %d: a reclaim must not evict", slotOf(tb, key("h")))
	}
	for i := 0; i < 10; i++ {
		if slotOf(tb, key(fmt.Sprintf("s%d", i))) != i {
			t.Fatalf("s%d lost its key", i)
		}
	}
}

func TestClaudeAgentGoneFreesKey(t *testing.T) {
	t.Run("hook-known stays keyless", func(t *testing.T) {
		tb := newTable(session.Config{})
		idleHerdr(tb, "h", "p1", t0)
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "h")), sec(1))
		tb.Select(0)
		tb.ApplyHerdr(snap(), sec(2))
		s := mustGet(t, tb, key("h"))
		if s.Slot != -1 || s.HerdrPane != "" || tb.Selected() != -1 {
			t.Fatalf("slot=%d pane=%q selected=%d", s.Slot, s.HerdrPane, tb.Selected())
		}
		// It must not take a key back with no pane.
		tb.Tick(min(5))
		if slotOf(tb, key("h")) != -1 {
			t.Fatal("took a key back with no pane")
		}
		// It comes back on activity.
		tb.ApplyHook(ev("UserPromptSubmit", "h", min(6)))
		if slotOf(tb, key("h")) != 0 {
			t.Fatalf("slot = %d after activity", slotOf(tb, key("h")))
		}
	})
	t.Run("seeded is removed", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "h")), t0)
		if slotOf(tb, key("h")) != 0 {
			t.Fatal("not seeded")
		}
		tb.ApplyHerdr(snap(), sec(1))
		if tb.Get(key("h")) != nil || tb.Slots()[0] != nil {
			t.Fatal("seeded session or its key stayed")
		}
	})
	t.Run("another session in the pane", func(t *testing.T) {
		tb := newTable(session.Config{})
		idleHerdr(tb, "h", "p1", t0)
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "other")), sec(1))
		if s := mustGet(t, tb, key("h")); s.Slot != -1 || s.HerdrPane != "" {
			t.Fatalf("slot=%d pane=%q", s.Slot, s.HerdrPane)
		}
	})
	t.Run("pane known, id not yet", func(t *testing.T) {
		tb := newTable(session.Config{})
		idleHerdr(tb, "h", "p1", t0)
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "")), sec(1))
		if s := mustGet(t, tb, key("h")); s.Slot != 0 || s.HerdrPane != "p1" {
			t.Fatalf("slot=%d pane=%q", s.Slot, s.HerdrPane)
		}
	})
}

func TestClaudePaneMovedKeepsKey(t *testing.T) {
	t.Run("hook-known", func(t *testing.T) {
		tb := newTable(session.Config{})
		idleHerdr(tb, "x", "p0", t0)
		idleHerdr(tb, "h", "p1", t0)
		tb.ApplyHerdr(snap(
			agent("p0", "claude", herdr.StatusIdle, "x"),
			agent("p1", "claude", herdr.StatusIdle, "h")), sec(1))
		tb.ApplyHerdr(snap(
			agent("p0", "claude", herdr.StatusIdle, "x"),
			agent("p9", "claude", herdr.StatusIdle, "h")), sec(2))
		if s := mustGet(t, tb, key("h")); s.Slot != 1 || s.HerdrPane != "p9" {
			t.Fatalf("slot=%d pane=%q", s.Slot, s.HerdrPane)
		}
	})
	t.Run("seeded", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHerdr(snap(
			agent("p0", "claude", herdr.StatusIdle, "x"),
			agent("p1", "claude", herdr.StatusIdle, "h")), t0)
		tb.ApplyHerdr(snap(
			agent("p0", "claude", herdr.StatusIdle, "x"),
			agent("p9", "claude", herdr.StatusIdle, "h")), min(5))
		s := mustGet(t, tb, key("h"))
		if s.Slot != 1 || s.HerdrPane != "p9" || !s.LastActive.Equal(t0) {
			t.Fatalf("slot=%d pane=%q last=%v: the session must be kept, not seeded again", s.Slot, s.HerdrPane, s.LastActive)
		}
	})
}

func TestUnavailableSnapshotChangesNothing(t *testing.T) {
	tb := newTable(session.Config{})
	idleHerdr(tb, "h", "p1", t0)
	idleDesktop(tb, "d", t0)
	tb.ApplyHerdr(snap(
		agent("p1", "claude", herdr.StatusIdle, "h"),
		agent("p2", "claude", herdr.StatusIdle, "seed"),
		agent("p3", "codex", herdr.StatusIdle, "")), t0)
	before := map[string]int{}
	for _, s := range tb.All() {
		before[s.Key] = s.Slot
	}
	for i := 1; i <= 5; i++ {
		tb.ApplyHerdr(herdr.Snapshot{Available: false}, min(i*60))
		tb.Tick(min(i*60 + 1))
	}
	if len(tb.All()) != len(before) {
		t.Fatalf("sessions = %d, want %d", len(tb.All()), len(before))
	}
	for k, slot := range before {
		if slotOf(tb, k) != slot {
			t.Fatalf("%s slot = %d, want %d", k, slotOf(tb, k), slot)
		}
	}
	if s := mustGet(t, tb, key("h")); s.HerdrPane != "p1" {
		t.Fatalf("pane = %q", s.HerdrPane)
	}
}

func TestEndedGraceStillFreesAStickyKey(t *testing.T) {
	tb := newTable(session.Config{})
	idleHerdr(tb, "h", "p1", t0)
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "h")), t0)
	tb.ApplyHook(ev("SessionEnd", "h", min(1)))
	tb.Tick(min(10))
	if slotOf(tb, key("h")) != 0 {
		t.Fatal("freed before the grace")
	}
	tb.Tick(min(11))
	if tb.Get(key("h")) != nil || tb.Slots()[0] != nil {
		t.Fatal("ended session kept its key")
	}
}

// A pane move for a hook-known session keeps the key and the selection, even
// when a lower key is free (a free and retake would move it to that key).
func TestMoveKeepsKeyAndSelectionWithLowerSlotFree(t *testing.T) {
	tb := newTable(session.Config{})
	idleHerdr(tb, "a", "p1", t0)
	idleHerdr(tb, "b", "p2", t0)
	idleHerdr(tb, "c", "p3", t0)
	tb.ApplyHerdr(snap(
		agent("p1", "claude", herdr.StatusIdle, "a"),
		agent("p2", "claude", herdr.StatusIdle, "b"),
		agent("p3", "claude", herdr.StatusIdle, "c")), sec(1))
	tb.ApplyHook(ev("SessionEnd", "a", min(1)))
	tb.Tick(min(12)) // a is removed; key 0 is free
	if tb.Get(key("a")) != nil || tb.Slots()[0] != nil {
		t.Fatal("a still holds key 0")
	}
	tb.Select(2)
	tb.ApplyHerdr(snap(
		agent("p2", "claude", herdr.StatusIdle, "b"),
		agent("p9", "claude", herdr.StatusIdle, "c")), min(13))
	s := mustGet(t, tb, key("c"))
	if s.Slot != 2 || s.HerdrPane != "p9" || tb.Selected() != 2 {
		t.Fatalf("slot=%d pane=%q selected=%d", s.Slot, s.HerdrPane, tb.Selected())
	}
}

// A session in NeedsInput state is never evicted. The state comes from a
// permission_prompt notification or from a blocked Herdr agent.
func TestNeedsInputStateNeverEvicted(t *testing.T) {
	prep := map[string]func(tb *session.Table){
		"notification": func(tb *session.Table) {
			tb.ApplyHook(ev("UserPromptSubmit", "w", t0))
			n := ev("Notification", "w", t0)
			n.NotificationType = "permission_prompt"
			tb.ApplyHook(n)
		},
		"herdr blocked": func(tb *session.Table) {
			tb.ApplyHerdr(snap(agent("p0", "claude", herdr.StatusBlocked, "w")), t0)
		},
	}
	for name, p := range prep {
		t.Run(name, func(t *testing.T) {
			tb := newTable(session.Config{})
			p(tb)
			if s := mustGet(t, tb, key("w")); s.State != session.NeedsInput || s.Slot != 0 {
				t.Fatalf("state=%v slot=%d", s.State, s.Slot)
			}
			for i := 1; i < 10; i++ {
				idleDesktop(tb, fmt.Sprintf("s%d", i), sec(i))
			}
			tb.ApplyHook(desktopEv("UserPromptSubmit", "n", min(1)))
			// w is the least recently active, but only an idle session goes.
			if slotOf(tb, key("w")) != 0 || slotOf(tb, key("n")) != 1 || slotOf(tb, key("s1")) != -1 {
				t.Fatalf("w=%d n=%d s1=%d", slotOf(tb, key("w")), slotOf(tb, key("n")), slotOf(tb, key("s1")))
			}
		})
	}
}

// An ended session with a Herdr pane does not take a free key in its grace.
func TestEndedSlotlessSessionNeverReclaims(t *testing.T) {
	tb := newTable(session.Config{})
	idleHerdr(tb, "s0", "p0", t0)
	s0 := snap(agent("p0", "claude", herdr.StatusIdle, "s0"))
	tb.ApplyHerdr(s0, t0)
	for i := 1; i < 10; i++ {
		tb.ApplyHook(ev("UserPromptSubmit", fmt.Sprintf("s%d", i), sec(i)))
	}
	tb.ApplyHook(ev("UserPromptSubmit", "n", min(1))) // evicts s0
	if slotOf(tb, key("s0")) != -1 {
		t.Fatalf("s0 = %d, want -1", slotOf(tb, key("s0")))
	}
	tb.ApplyHook(ev("SessionEnd", "s1", min(7)))
	tb.ApplyHook(ev("SessionEnd", "s0", min(9)))
	tb.Tick(min(17).Add(time.Second)) // s1 is removed; s0 is still in its grace
	if tb.Get(key("s1")) != nil {
		t.Fatal("s1 not removed")
	}
	if s := mustGet(t, tb, key("s0")); !s.Ended || s.Slot != -1 {
		t.Fatalf("ended=%v slot=%d: an ended session must not reclaim", s.Ended, s.Slot)
	}
}

// A keyless session with a Herdr pane is not forgotten after a day.
func TestKeylessHerdrSessionSurvivesForgetAfter(t *testing.T) {
	tb := newTable(session.Config{})
	fill(tb, 10)
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "h")), min(1))
	if slotOf(tb, key("h")) != -1 {
		t.Fatalf("h = %d, want -1", slotOf(tb, key("h")))
	}
	tb.Tick(t0.Add(25 * time.Hour))
	if tb.Get(key("h")) == nil {
		t.Fatal("keyless Herdr session was forgotten")
	}
}

// An agent that left Herdr while Working and comes back Working counts as
// new activity: it may evict an idle session.
func TestReturningWorkingAgentActivates(t *testing.T) {
	tb := newTable(session.Config{})
	w := ev("UserPromptSubmit", "h", t0)
	w.HerdrPane = "p1"
	tb.ApplyHook(w)
	back := snap(agent("p1", "claude", herdr.StatusWorking, "h"))
	tb.ApplyHerdr(back, t0)
	for i := 1; i < 10; i++ {
		idleDesktop(tb, fmt.Sprintf("s%d", i), sec(i))
	}
	tb.ApplyHerdr(snap(), min(1)) // the agent is gone; h loses key 0
	if slotOf(tb, key("h")) != -1 {
		t.Fatalf("h = %d, want -1", slotOf(tb, key("h")))
	}
	tb.ApplyHook(desktopEv("UserPromptSubmit", "n", min(2))) // takes key 0
	if slotOf(tb, key("n")) != 0 {
		t.Fatalf("n = %d, want 0", slotOf(tb, key("n")))
	}
	tb.ApplyHerdr(back, min(3))
	if slotOf(tb, key("h")) != 1 || slotOf(tb, key("s1")) != -1 {
		t.Fatalf("h=%d s1=%d, want 1 and -1", slotOf(tb, key("h")), slotOf(tb, key("s1")))
	}
}

// A pane that only a hook event named is not a confirmed Herdr pane. It does
// not retake a key that Herdr's missing agent freed (no flapping).
func TestHookPaneDoesNotRetakeKeyUntilHerdrConfirms(t *testing.T) {
	tb := newTable(session.Config{})
	idleHerdr(tb, "h", "p1", t0)
	listed := snap(agent("p1", "claude", herdr.StatusIdle, "h"))
	tb.ApplyHerdr(listed, sec(1))
	tb.ApplyHerdr(snap(), sec(2)) // Herdr does not list the agent
	if slotOf(tb, key("h")) != -1 {
		t.Fatalf("h = %d, want -1", slotOf(tb, key("h")))
	}
	n := ev("Notification", "h", sec(3))
	n.NotificationType = "idle_prompt"
	n.HerdrPane = "p1"
	tb.ApplyHook(n)
	tb.Tick(sec(4))
	if slotOf(tb, key("h")) != -1 {
		t.Fatalf("h = %d: a hook pane retook the key", slotOf(tb, key("h")))
	}
	tb.ApplyHerdr(listed, sec(5)) // Herdr lists it again
	if slotOf(tb, key("h")) != 0 {
		t.Fatalf("h = %d after Herdr listed it, want 0", slotOf(tb, key("h")))
	}
}

// A session with a question or a held permission request is waiting for
// the user: it is never evicted. Today every such request also sets Working,
// which is not evictable, so the Question and Pending guards are a second
// line. The test forces State back to Idle through the live pointer so the
// guards are pinned for any future change that leaves Idle with a request.
func TestQuestionOrPendingNeverEvicted(t *testing.T) {
	prep := map[string]func(tb *session.Table){
		"question": func(tb *session.Table) {
			q := ev("PermissionRequest", "w", t0)
			q.ToolName, q.IsQuestion, q.RequestID = "AskUserQuestion", true, "r1"
			tb.ApplyHook(q)
		},
		"pending": func(tb *session.Table) {
			p := ev("PermissionRequest", "w", t0)
			p.ToolName, p.RequestID = "Bash", "r1"
			tb.ApplyHook(p)
		},
	}
	for name, p := range prep {
		t.Run(name, func(t *testing.T) {
			tb := newTable(session.Config{})
			tb.ApplyHerdr(snap(agent("p0", "claude", herdr.StatusIdle, "w")), t0) // seeded idle
			p(tb)
			s := mustGet(t, tb, key("w"))
			s.State = session.Idle // see the comment above
			if s.State != session.Idle || s.Slot != 0 || (!s.Question && len(s.Pending) == 0) {
				t.Fatalf("setup: state=%v slot=%d question=%v pending=%d", s.State, s.Slot, s.Question, len(s.Pending))
			}
			for i := 1; i < 10; i++ {
				idleDesktop(tb, fmt.Sprintf("s%d", i), sec(i))
			}
			tb.ApplyHook(desktopEv("UserPromptSubmit", "n", min(1)))
			// w is the least recently active, but it is waiting for the user.
			if slotOf(tb, key("w")) != 0 || slotOf(tb, key("n")) != 1 || slotOf(tb, key("s1")) != -1 {
				t.Fatalf("w=%d n=%d s1=%d", slotOf(tb, key("w")), slotOf(tb, key("n")), slotOf(tb, key("s1")))
			}
		})
	}
}
