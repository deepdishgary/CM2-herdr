package session_test

import (
	"fmt"
	"testing"
	"time"

	"cm2-claude/internal/herdr"
	"cm2-claude/internal/session"
)

func TestSlotsLenAndDefaults(t *testing.T) {
	if n := len(newTable(session.Config{}).Slots()); n != 10 {
		t.Fatalf("default slots = %d", n)
	}
	if n := len(newTable(session.Config{Slots: 3}).Slots()); n != 3 {
		t.Fatalf("slots = %d", n)
	}
}

func TestSlotAssignment(t *testing.T) {
	tests := []struct {
		name string
		ev   session.HookEvent
		slot int
	}{
		{"session start gets none", start("a", t0), -1},
		{"prompt gets one", ev("UserPromptSubmit", "a", t0), 0},
		{"permission request gets one", perm("a", "r1", t0), 0},
		{"question gets one", func() session.HookEvent {
			e := ev("PermissionRequest", "a", t0)
			e.IsQuestion = true
			return e
		}(), 0},
		{"notification for an unknown session gets none", func() session.HookEvent {
			e := ev("Notification", "a", t0)
			e.NotificationType = "permission_prompt"
			return e
		}(), -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTable(session.Config{})
			tb.ApplyHook(tc.ev)
			if got := slotOf(tb, key("a")); got != tc.slot && !(tc.slot == -1 && got == -2) {
				t.Fatalf("slot = %d, want %d", got, tc.slot)
			}
		})
	}
}

func TestSlotAfterStartedSession(t *testing.T) {
	tests := []struct {
		name string
		then session.HookEvent
		slot int
	}{
		{"stop gives none", ev("Stop", "a", sec(1)), -1},
		{"post tool use gives none", ev("PostToolUse", "a", sec(1)), -1},
		{"idle_prompt gives none", func() session.HookEvent {
			e := ev("Notification", "a", sec(1))
			e.NotificationType = "idle_prompt"
			return e
		}(), -1},
		{"permission_prompt cue gives one", func() session.HookEvent {
			e := ev("Notification", "a", sec(1))
			e.NotificationType = "permission_prompt"
			return e
		}(), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTable(session.Config{})
			tb.ApplyHook(start("a", t0))
			tb.ApplyHook(tc.then)
			if got := slotOf(tb, key("a")); got != tc.slot {
				t.Fatalf("slot = %d, want %d", got, tc.slot)
			}
		})
	}
}

func TestSlotsLowestFreeAndSticky(t *testing.T) {
	tb := newTable(session.Config{})
	fill(tb, 3)
	for i := 0; i < 3; i++ {
		if got := slotOf(tb, key(fmt.Sprintf("s%d", i))); got != i {
			t.Fatalf("s%d slot = %d", i, got)
		}
	}
	// A repeat prompt keeps the slot.
	tb.ApplyHook(ev("UserPromptSubmit", "s1", sec(10)))
	if slotOf(tb, key("s1")) != 1 {
		t.Fatal("slot moved")
	}
	// Free slot 1 (ended + grace), then the next session takes the lowest free slot.
	tb.ApplyHook(ev("SessionEnd", "s1", sec(11)))
	tb.Tick(min(12))
	if tb.Slots()[1] != nil {
		t.Fatal("slot 1 not freed")
	}
	tb.ApplyHook(ev("UserPromptSubmit", "new", min(13)))
	if got := slotOf(tb, key("new")); got != 1 {
		t.Fatalf("new slot = %d", got)
	}
	// Others did not move.
	if slotOf(tb, key("s0")) != 0 || slotOf(tb, key("s2")) != 2 {
		t.Fatal("sticky slots moved")
	}
	if tb.Slots()[1].Key != key("new") {
		t.Fatal("Slots() does not match Slot field")
	}
}

func TestEviction(t *testing.T) {
	// Two-slot table: s0 and s1 are placed, then n arrives.
	type setup func(tb *session.Table)
	idle := func(id string, at int) setup {
		return func(tb *session.Table) {
			tb.ApplyHook(ev("UserPromptSubmit", id, sec(at)))
			tb.ApplyHook(ev("Stop", id, sec(at)))
			tb.Focused(key(id), sec(at)) // seen: idle
		}
	}
	working := func(id string, at int) setup {
		return func(tb *session.Table) { tb.ApplyHook(ev("UserPromptSubmit", id, sec(at))) }
	}
	needs := func(id string, at int) setup {
		return func(tb *session.Table) { tb.ApplyHook(perm(id, "r-"+id, sec(at))) }
	}
	done := func(id string, at int) setup {
		return func(tb *session.Table) {
			tb.ApplyHook(ev("UserPromptSubmit", id, sec(at)))
			tb.ApplyHook(ev("Stop", id, sec(at)))
		}
	}
	errored := func(id string, at int) setup {
		return func(tb *session.Table) {
			tb.ApplyHook(ev("UserPromptSubmit", id, sec(at)))
			tb.ApplyHook(ev("StopFailure", id, sec(at)))
		}
	}
	ended := func(id string, at int) setup {
		return func(tb *session.Table) {
			tb.ApplyHook(ev("UserPromptSubmit", id, sec(at)))
			tb.ApplyHook(ev("SessionEnd", id, sec(at)))
		}
	}
	tests := []struct {
		name    string
		a, b    setup  // s-a (slot 0) and s-b (slot 1)
		evicted string // "", "a" or "b"
	}{
		{"idle evicted over working", idle("a", 1), working("b", 2), "a"},
		{"oldest idle evicted", idle("a", 5), idle("b", 2), "b"},
		{"oldest idle evicted (other order)", idle("a", 2), idle("b", 5), "a"},
		{"ended evicted", working("a", 1), ended("b", 2), "b"},
		{"ended and idle: oldest goes", idle("a", 9), ended("b", 3), "b"},
		{"working and needs-input are safe", working("a", 1), needs("b", 2), ""},
		{"done unseen is safe", done("a", 1), working("b", 2), ""},
		{"error is safe", errored("a", 1), working("b", 2), ""},
		{"needs input is safe", needs("a", 1), needs("b", 2), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTable(session.Config{Slots: 2})
			tc.a(tb) // helper uses ids "a" and "b"
			tc.b(tb)
			if slotOf(tb, key("a")) != 0 || slotOf(tb, key("b")) != 1 {
				t.Fatalf("setup slots a=%d b=%d", slotOf(tb, key("a")), slotOf(tb, key("b")))
			}
			tb.ApplyHook(ev("UserPromptSubmit", "n", sec(100)))
			n := slotOf(tb, key("n"))
			switch tc.evicted {
			case "":
				if n != -1 {
					t.Fatalf("n got slot %d, nothing should be evicted", n)
				}
				if slotOf(tb, key("a")) != 0 || slotOf(tb, key("b")) != 1 {
					t.Fatal("something was evicted")
				}
			case "a":
				if n != 0 || slotOf(tb, key("a")) != -1 || slotOf(tb, key("b")) != 1 {
					t.Fatalf("n=%d a=%d b=%d", n, slotOf(tb, key("a")), slotOf(tb, key("b")))
				}
			case "b":
				if n != 1 || slotOf(tb, key("b")) != -1 || slotOf(tb, key("a")) != 0 {
					t.Fatalf("n=%d a=%d b=%d", n, slotOf(tb, key("a")), slotOf(tb, key("b")))
				}
			}
		})
	}
}

func TestEvictedSessionStaysInTableAndCanReturn(t *testing.T) {
	tb := newTable(session.Config{Slots: 1})
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(1)))
	tb.ApplyHook(ev("Stop", "a", sec(2)))
	tb.Focused(key("a"), sec(3))
	tb.ApplyHook(ev("UserPromptSubmit", "b", sec(4)))
	if slotOf(tb, key("a")) != -1 || tb.Get(key("a")) == nil {
		t.Fatal("a should be slotless but present")
	}
	// An evicted idle session does not steal the slot back.
	tb.Tick(sec(5))
	if slotOf(tb, key("a")) != -1 {
		t.Fatal("evicted session retook a slot")
	}
	// b finishes and is seen; a prompts again and evicts it.
	tb.ApplyHook(ev("Stop", "b", sec(6)))
	tb.Focused(key("b"), sec(7))
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(8)))
	if slotOf(tb, key("a")) != 0 || slotOf(tb, key("b")) != -1 {
		t.Fatalf("a=%d b=%d", slotOf(tb, key("a")), slotOf(tb, key("b")))
	}
}

func TestEvictionResetsSelection(t *testing.T) {
	tb := newTable(session.Config{Slots: 1})
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(1)))
	tb.ApplyHook(ev("Stop", "a", sec(2)))
	tb.Focused(key("a"), sec(3))
	tb.Select(0)
	tb.ApplyHook(ev("UserPromptSubmit", "b", sec(4)))
	if tb.Selected() != -1 {
		t.Fatalf("selected = %d", tb.Selected())
	}
}

func TestWaitingSessionGetsSlotWhenFreed(t *testing.T) {
	tb := newTable(session.Config{Slots: 2})
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(1)))
	tb.ApplyHook(ev("UserPromptSubmit", "b", sec(2)))
	tb.ApplyHook(ev("UserPromptSubmit", "w", sec(3)))
	if slotOf(tb, key("w")) != -1 {
		t.Fatal("w should wait")
	}
	if tb.Get(key("w")).State != session.Working {
		t.Fatal("waiting session lost its state")
	}
	// a ends and passes its grace: w takes the slot on the same Tick.
	tb.ApplyHook(ev("SessionEnd", "a", sec(4)))
	tb.Tick(min(20))
	if got := slotOf(tb, key("w")); got != 0 {
		t.Fatalf("w slot = %d", got)
	}
}

func TestWaitingSessionTakesEvictableSlotWhenItBecomesSafeToEvict(t *testing.T) {
	tb := newTable(session.Config{Slots: 1})
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(1)))
	tb.ApplyHook(ev("UserPromptSubmit", "w", sec(2)))
	if slotOf(tb, key("w")) != -1 {
		t.Fatal("w should wait")
	}
	// a finishes and the user sees it: it is now evictable.
	tb.ApplyHook(ev("Stop", "a", sec(3)))
	tb.Focused(key("a"), sec(4))
	if slotOf(tb, key("w")) != 0 || slotOf(tb, key("a")) != -1 {
		t.Fatalf("w=%d a=%d", slotOf(tb, key("w")), slotOf(tb, key("a")))
	}
}

func TestFreeEndedAfterGrace(t *testing.T) {
	tests := []struct {
		name  string
		grace time.Duration
		at    time.Time
		gone  bool
	}{
		{"default grace not reached", 0, t0.Add(10*time.Minute - time.Nanosecond), false},
		{"default grace reached", 0, t0.Add(10 * time.Minute), true},
		{"custom grace not reached", time.Minute, t0.Add(59 * time.Second), false},
		{"custom grace reached", time.Minute, t0.Add(time.Minute), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTable(session.Config{EndedGrace: tc.grace})
			tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
			tb.ApplyHook(ev("SessionEnd", "a", t0))
			tb.Select(0)
			tb.Tick(tc.at)
			if gone := tb.Get(key("a")) == nil; gone != tc.gone {
				t.Fatalf("gone = %v, want %v", gone, tc.gone)
			}
			if tc.gone && (tb.Slots()[0] != nil || tb.Selected() != -1) {
				t.Fatal("slot or selection stayed")
			}
			if !tc.gone && (tb.Slots()[0] == nil || tb.Selected() != 0) {
				t.Fatal("slot or selection lost early")
			}
		})
	}
}

// Idle keys never expire. Config.IdleExpiry is a no-op field.
func TestIdleKeysNeverExpire(t *testing.T) {
	prep := map[string]func(tb *session.Table){
		"done": func(tb *session.Table) {
			tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
			tb.ApplyHook(ev("Stop", "a", t0))
		},
		"idle": func(tb *session.Table) {
			tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
			tb.ApplyHook(ev("Stop", "a", t0))
			tb.Focused(key("a"), t0)
		},
		"error": func(tb *session.Table) {
			tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
			tb.ApplyHook(ev("StopFailure", "a", t0))
		},
		"working": func(tb *session.Table) { tb.ApplyHook(ev("UserPromptSubmit", "a", t0)) },
		"pending": func(tb *session.Table) { tb.ApplyHook(perm("a", "r1", t0)) },
		"question": func(tb *session.Table) {
			e := ev("PermissionRequest", "a", t0)
			e.IsQuestion = true
			tb.ApplyHook(e)
		},
		"needs-input cue": func(tb *session.Table) {
			tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
			e := ev("Notification", "a", t0)
			e.NotificationType = "permission_prompt"
			tb.ApplyHook(e)
		},
	}
	tests := []struct {
		kind   string
		expiry time.Duration
		at     time.Duration
	}{
		{"idle", 0, 30 * time.Minute},
		{"idle", 0, 24 * time.Hour},
		{"done", 0, 30 * time.Minute},
		{"done", 0, 24 * time.Hour},
		{"error", 0, 30 * time.Minute},
		{"error", 0, 24 * time.Hour},
		{"idle", time.Minute, time.Minute},
		{"idle", time.Minute, time.Hour},
		{"working", 0, 5 * time.Hour},
		{"pending", 0, 5 * time.Hour},
		{"question", 0, 5 * time.Hour},
		{"needs-input cue", 0, 5 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s/%v/%v", tc.kind, tc.expiry, tc.at), func(t *testing.T) {
			tb := newTable(session.Config{IdleExpiry: tc.expiry})
			prep[tc.kind](tb)
			tb.Select(0)
			tb.Tick(t0.Add(tc.at))
			if s := mustGet(t, tb, key("a")); s.Slot != 0 {
				t.Fatalf("slot = %d, want 0", s.Slot)
			}
			if tb.Selected() != 0 {
				t.Fatal("selection moved")
			}
		})
	}
}

func TestForgetOldSlotlessSession(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(start("a", t0))
	tb.Tick(t0.Add(23 * time.Hour))
	if tb.Get(key("a")) == nil {
		t.Fatal("dropped early")
	}
	tb.Tick(t0.Add(24 * time.Hour))
	if tb.Get(key("a")) != nil {
		t.Fatal("kept after 24h")
	}
	// A Herdr-attached slotless session is kept.
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusIdle, "")), t0.Add(25*time.Hour))
	tb.Tick(t0.Add(60 * time.Hour))
	if tb.Get("herdr:p1") == nil {
		t.Fatal("herdr session forgotten while its pane exists")
	}
}

func TestIdleHerdrAgentKeepsItsKey(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusWorking, "")), t0)
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusDone, "")), min(1))
	tb.Focused("herdr:p1", min(1))
	tb.Tick(min(32))
	tb.Tick(min(24 * 60))
	if slotOf(tb, "herdr:p1") != 0 {
		t.Fatal("idle herdr session lost its key")
	}
}

func TestSelection(t *testing.T) {
	t.Run("select ignores empty and invalid slots", func(t *testing.T) {
		tb := newTable(session.Config{})
		fill(tb, 2)
		for _, slot := range []int{5, -1, 10, 99} {
			tb.Select(slot)
			if tb.Selected() != -1 {
				t.Fatalf("Select(%d) selected %d", slot, tb.Selected())
			}
		}
		tb.Select(1)
		if tb.Selected() != 1 {
			t.Fatal("did not select slot 1")
		}
		tb.Select(7) // empty: keeps the old selection
		if tb.Selected() != 1 {
			t.Fatalf("selected = %d", tb.Selected())
		}
	})
	moves := []struct {
		name     string
		occupied []int // slots to fill
		start    int   // -1 = none
		delta    int
		want     int
	}{
		{"none selected, forward", []int{2, 5}, -1, 1, 2},
		{"none selected, back", []int{2, 5}, -1, -1, 2},
		{"forward", []int{0, 3, 7}, 0, 1, 3},
		{"forward skips empty", []int{0, 3, 7}, 3, 1, 7},
		{"forward wraps", []int{0, 3, 7}, 7, 1, 0},
		{"back", []int{0, 3, 7}, 3, -1, 0},
		{"back wraps", []int{0, 3, 7}, 0, -1, 7},
		{"two steps", []int{0, 3, 7}, 0, 2, 7},
		{"big delta wraps", []int{0, 3, 7}, 0, 4, 3},
		{"big negative delta wraps", []int{0, 3, 7}, 0, -4, 7},
		{"zero delta stays", []int{0, 3, 7}, 3, 0, 3},
		{"single session stays", []int{4}, 4, 1, 4},
	}
	for _, tc := range moves {
		t.Run(tc.name, func(t *testing.T) {
			tb := sparseTable(t, tc.occupied)
			if tc.start >= 0 {
				tb.Select(tc.start)
			}
			tb.MoveSelection(tc.delta)
			if tb.Selected() != tc.want {
				t.Fatalf("selected = %d, want %d", tb.Selected(), tc.want)
			}
		})
	}
	t.Run("no sessions", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.MoveSelection(1)
		if tb.Selected() != -1 {
			t.Fatalf("selected = %d", tb.Selected())
		}
	})
}

// sparseTable returns a table with sessions in exactly the given slots.
func sparseTable(t *testing.T, occupied []int) *session.Table {
	t.Helper()
	tb := newTable(session.Config{})
	fill(tb, 10)
	keep := map[int]bool{}
	for _, s := range occupied {
		keep[s] = true
	}
	for i := 0; i < 10; i++ {
		if !keep[i] {
			tb.ApplyHook(ev("SessionEnd", fmt.Sprintf("s%d", i), sec(50)))
		}
	}
	tb.Tick(min(20))
	for i := 0; i < 10; i++ {
		if (tb.Slots()[i] != nil) != keep[i] {
			t.Fatalf("slot %d occupied=%v, want %v", i, tb.Slots()[i] != nil, keep[i])
		}
	}
	return tb
}

func TestAllOrderIsStable(t *testing.T) {
	tb := newTable(session.Config{Slots: 2})
	tb.ApplyHook(start("z", t0))                      // slotless, first
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(1))) // slot 0
	tb.ApplyHook(ev("UserPromptSubmit", "b", sec(2))) // slot 1
	tb.ApplyHook(start("y", sec(3)))                  // slotless, later
	var got []string
	for _, s := range tb.All() {
		got = append(got, s.Key)
	}
	want := []string{key("a"), key("b"), key("z"), key("y")}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	again := fmt.Sprint(tb.All()[0].Key, tb.All()[3].Key)
	if again != fmt.Sprint(key("a"), key("y")) {
		t.Fatal("order changed between calls")
	}
}

func TestGetUnknown(t *testing.T) {
	if newTable(session.Config{}).Get("claude:nope") != nil {
		t.Fatal("expected nil")
	}
}

func TestSlotsReturnsCopy(t *testing.T) {
	tb := newTable(session.Config{})
	fill(tb, 1)
	got := tb.Slots()
	got[0] = nil
	if tb.Slots()[0] == nil {
		t.Fatal("Slots() leaked the internal slice")
	}
}

func TestWaitingIdleSessionNeverEvicts(t *testing.T) {
	tb := newTable(session.Config{Slots: 1})
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(1)))
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusWorking, "")), sec(2)) // waits
	tb.ApplyHook(ev("Stop", "a", sec(3)))
	tb.Focused(key("a"), sec(4)) // a is idle and evictable
	// p1 was waiting and working, so it took the slot from idle a.
	if slotOf(tb, "herdr:p1") != 0 {
		t.Fatalf("p1 slot = %d", slotOf(tb, "herdr:p1"))
	}
	// Now p1 goes idle; a (idle, activated=false) must not evict it back.
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusIdle, "")), sec(5))
	if slotOf(tb, key("a")) != -1 || slotOf(tb, "herdr:p1") != 0 {
		t.Fatal("idle sessions swapped slots")
	}
}

func TestWaitingThatTurnedIdleOnlyTakesFreeSlot(t *testing.T) {
	tb := newTable(session.Config{Slots: 1})
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(1)))                          // working, safe
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusWorking, "")), sec(2)) // waits
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusIdle, "")), sec(3))    // waiting, now idle
	tb.ApplyHook(ev("Stop", "a", sec(4)))
	tb.Focused(key("a"), sec(5)) // a becomes evictable, but p1 is idle: no eviction
	if slotOf(tb, key("a")) != 0 || slotOf(tb, "herdr:p1") != -1 {
		t.Fatalf("a=%d p1=%d", slotOf(tb, key("a")), slotOf(tb, "herdr:p1"))
	}
	// Once a slot is truly free, p1 takes it.
	tb.ApplyHook(ev("SessionEnd", "a", sec(6)))
	tb.Tick(min(20))
	if slotOf(tb, "herdr:p1") != 0 {
		t.Fatalf("p1 slot = %d", slotOf(tb, "herdr:p1"))
	}
}

func TestStateString(t *testing.T) {
	want := map[session.State]string{session.Idle: "idle", session.Working: "working", session.NeedsInput: "needs-input", session.Done: "done", session.Error: "error", session.State(99): "unknown"}
	for s, w := range want {
		if s.String() != w {
			t.Errorf("%d = %q, want %q", s, s.String(), w)
		}
	}
}

// Waiting sessions take a freed slot in this order: the most recently active
// first, and by arrival when they tie.
func TestWaitingSessionsTakeSlotsMostRecentFirst(t *testing.T) {
	tb := newTable(session.Config{Slots: 2})
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(1)))
	tb.ApplyHook(ev("UserPromptSubmit", "b", sec(2)))
	tb.ApplyHook(ev("UserPromptSubmit", "w1", sec(3)))
	tb.ApplyHook(ev("UserPromptSubmit", "w2", sec(5)))
	tb.ApplyHook(ev("UserPromptSubmit", "w3", sec(4)))
	for _, id := range []string{"w1", "w2", "w3"} {
		if slotOf(tb, key(id)) != -1 {
			t.Fatalf("%s should wait", id)
		}
	}
	tb.ApplyHook(ev("SessionEnd", "a", sec(6)))
	tb.ApplyHook(ev("SessionEnd", "b", sec(6)))
	tb.Tick(min(20))
	if slotOf(tb, key("w2")) < 0 || slotOf(tb, key("w3")) < 0 || slotOf(tb, key("w1")) != -1 {
		t.Fatalf("w1=%d w2=%d w3=%d, want w2 and w3 seated",
			slotOf(tb, key("w1")), slotOf(tb, key("w2")), slotOf(tb, key("w3")))
	}

	// With equal activity times, the earlier arrival wins.
	tb = newTable(session.Config{Slots: 1})
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(1)))
	tb.ApplyHook(ev("UserPromptSubmit", "x", sec(2)))
	tb.ApplyHook(ev("UserPromptSubmit", "y", sec(2)))
	tb.ApplyHook(ev("SessionEnd", "a", sec(3)))
	tb.Tick(min(20))
	if slotOf(tb, key("x")) != 0 || slotOf(tb, key("y")) != -1 {
		t.Fatalf("x=%d y=%d", slotOf(tb, key("x")), slotOf(tb, key("y")))
	}
}

// A Claude session that dies without a SessionEnd (kill -9, closed terminal)
// leaves Working or NeedsInput behind. After six hours without any event it
// counts as idle, so its key can be taken by a new session.
func TestStuckClaudeSessionExpires(t *testing.T) {
	prep := map[string]func(tb *session.Table){
		"working": func(tb *session.Table) { tb.ApplyHook(ev("UserPromptSubmit", "a", t0)) },
		"pending": func(tb *session.Table) { tb.ApplyHook(perm("a", "r1", t0)) },
		"question": func(tb *session.Table) {
			e := ev("PermissionRequest", "a", t0)
			e.IsQuestion = true
			tb.ApplyHook(e)
		},
		"needs-input cue": func(tb *session.Table) {
			tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
			e := ev("Notification", "a", t0)
			e.NotificationType = "permission_prompt"
			tb.ApplyHook(e)
		},
	}
	for kind, p := range prep {
		t.Run(kind, func(t *testing.T) {
			tb := newTable(session.Config{})
			p(tb)
			tb.Select(0)
			tb.Tick(t0.Add(6*time.Hour - time.Second))
			if s := mustGet(t, tb, key("a")); s.Slot != 0 || s.State == session.Idle {
				t.Fatalf("expired early: %+v", s)
			}
			tb.Tick(t0.Add(6 * time.Hour))
			s := mustGet(t, tb, key("a"))
			if s.State != session.Idle || len(s.Pending) != 0 || s.Question {
				t.Fatalf("still stuck after 6 h: %+v", s)
			}
			// It keeps its key, but a new session may take it now.
			if s.Slot != 0 {
				t.Fatalf("slot = %d, want 0", s.Slot)
			}
			at := t0.Add(6*time.Hour + time.Second)
			for i := 0; i < 9; i++ {
				tb.ApplyHook(ev("UserPromptSubmit", fmt.Sprintf("f%d", i), at))
			}
			tb.ApplyHook(ev("UserPromptSubmit", "n", at))
			if slotOf(tb, key("n")) != 0 || slotOf(tb, key("a")) != -1 {
				t.Fatalf("n=%d a=%d", slotOf(tb, key("n")), slotOf(tb, key("a")))
			}
		})
	}
	// An event any time before keeps it.
	tb := newTable(session.Config{})
	tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
	tb.ApplyHook(ev("PostToolUse", "a", t0.Add(5*time.Hour)))
	tb.Tick(t0.Add(7 * time.Hour))
	if s := mustGet(t, tb, key("a")); s.Slot != 0 || s.State != session.Working {
		t.Fatalf("got %+v", s)
	}
}

// When Herdr has been gone for more than 30 s, its non-Claude agents stop
// showing Working. They show it again when Herdr returns and still says so.
func TestHerdrDownMarksAgentsIdle(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusWorking, "c1"), agent("p2", "codex", herdr.StatusWorking, "")), sec(1))
	tb.ApplyHerdr(herdr.Snapshot{Available: false}, sec(2))

	tb.Tick(sec(2 + 29))
	if s := mustGet(t, tb, "herdr:p2"); s.State != session.Working {
		t.Fatalf("idle after 29 s: %+v", s)
	}
	tb.Tick(sec(2 + 31))
	s := mustGet(t, tb, "herdr:p2")
	if s.State != session.Idle {
		t.Fatalf("still %v after 31 s without Herdr", s.State)
	}
	// Claude sessions take their state from hooks, not from Herdr.
	if c := mustGet(t, tb, key("c1")); c.State != session.Working {
		t.Fatalf("claude session = %v", c.State)
	}
	// Herdr returns and the agent still works.
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusWorking, "c1"), agent("p2", "codex", herdr.StatusWorking, "")), sec(60))
	if s := mustGet(t, tb, "herdr:p2"); s.State != session.Working || s.Slot < 0 {
		t.Fatalf("after Herdr returned: %+v", s)
	}

	// A short outage changes nothing.
	tb.ApplyHerdr(herdr.Snapshot{Available: false}, sec(70))
	tb.ApplyHerdr(snap(agent("p2", "codex", herdr.StatusWorking, "")), sec(80))
	tb.Tick(sec(200))
	if s := mustGet(t, tb, "herdr:p2"); s.State != session.Working {
		t.Fatalf("after a short outage: %+v", s)
	}
}

// The set of ignored session ids is bounded. The newest ids stay ignored.
func TestIgnoredSetIsBounded(t *testing.T) {
	tb := newTable(session.Config{})
	const n = 20000
	for i := 0; i < n; i++ {
		e := ev("SessionStart", fmt.Sprintf("h%d", i), t0)
		e.Attended = "0"
		tb.ApplyHook(e)
	}
	// The newest headless session is still ignored.
	tb.ApplyHook(ev("UserPromptSubmit", fmt.Sprintf("h%d", n-1), sec(1)))
	if tb.Get(key(fmt.Sprintf("h%d", n-1))) != nil {
		t.Fatal("a recent headless session was admitted")
	}
	// The oldest one was forgotten, so the cli prompt admits it again.
	tb.ApplyHook(ev("UserPromptSubmit", "h0", sec(1)))
	if tb.Get(key("h0")) == nil {
		t.Fatal("the ignored set never forgets: unbounded")
	}
}

func TestIgnoredCount(t *testing.T) {
	tb := newTable(session.Config{})
	e := ev("SessionStart", "h1", t0)
	e.Attended = "0"
	tb.ApplyHook(e)
	tb.ApplyHook(ev("UserPromptSubmit", "h1", sec(1))) // the same session again
	if got := tb.IgnoredCount(); got != 1 {
		t.Fatalf("IgnoredCount = %d, want 1", got)
	}
}
