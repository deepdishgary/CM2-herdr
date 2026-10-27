package session_test

import (
	"fmt"
	"testing"

	"cm2-claude/internal/herdr"
	"cm2-claude/internal/session"
)

// herdrClaude builds a Herdr snapshot agent for a Claude session in pane pN.
func herdrClaude(n int, st herdr.Status) herdr.Agent {
	return agent(fmt.Sprintf("p%d", n), "claude", st, fmt.Sprintf("c%d", n))
}

func TestHerdrSeedFillsFreeSlotsAfterRestart(t *testing.T) {
	tb := newTable(session.Config{})
	var as []herdr.Agent
	for i := 1; i <= 7; i++ {
		as = append(as, herdrClaude(i, herdr.StatusIdle))
	}
	tb.ApplyHerdr(snap(as...), t0)
	if n := len(tb.All()); n != 7 {
		t.Fatalf("sessions = %d, want 7", n)
	}
	for i := 1; i <= 7; i++ {
		s := mustGet(t, tb, key(fmt.Sprintf("c%d", i)))
		if s.Slot < 0 || s.Slot > 6 {
			t.Errorf("c%d slot = %d", i, s.Slot)
		}
		if s.SessionID != fmt.Sprintf("c%d", i) || s.Kind != "claude" || s.Frontend != session.FrontendHerdr ||
			s.HerdrPane != fmt.Sprintf("p%d", i) || s.CWD != fmt.Sprintf("/work/p%d", i) ||
			s.State != session.Idle || !s.LastActive.Equal(t0) || s.Ended {
			t.Errorf("c%d = %+v", i, s)
		}
		if got := tb.Visible(s, t0); got != session.Idle {
			t.Errorf("c%d visible = %v", i, got)
		}
	}
	// Seven different slots, none beyond the seventh.
	seen := map[int]bool{}
	for _, s := range tb.Slots()[:7] {
		if s == nil {
			t.Fatal("a free slot among the first seven")
		}
		seen[s.Slot] = true
	}
	if len(seen) != 7 {
		t.Fatalf("slots = %v", seen)
	}
	for _, s := range tb.Slots()[7:] {
		if s != nil {
			t.Fatal("slot beyond seven used")
		}
	}
}

func TestHerdrSeedStateFromHerdrStatus(t *testing.T) {
	tests := []struct {
		status herdr.Status
		state  session.State
	}{
		{herdr.StatusIdle, session.Idle},
		{herdr.StatusWorking, session.Working},
		{herdr.StatusBlocked, session.NeedsInput},
		{herdr.StatusDone, session.Done},
		{herdr.StatusUnknown, session.Idle},
	}
	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			tb := newTable(session.Config{})
			tb.ApplyHerdr(snap(herdrClaude(1, tc.status)), t0)
			s := mustGet(t, tb, key("c1"))
			if s.State != tc.state || s.Slot != 0 {
				t.Fatalf("got state %v slot %d, want %v slot 0", s.State, s.Slot, tc.state)
			}
		})
	}
}

func TestHerdrSeedTakesLowestFreeSlotAndKeepsItOnLaterSnapshots(t *testing.T) {
	tb := newTable(session.Config{})
	// Slots 0 and 2 are in use; 1 is free.
	tb.ApplyHook(ev("UserPromptSubmit", "h0", sec(0)))
	tb.ApplyHook(ev("UserPromptSubmit", "h1", sec(1)))
	tb.ApplyHook(ev("UserPromptSubmit", "h2", sec(2)))
	tb.Tick(sec(3))
	tb.ApplyHook(ev("SessionEnd", "h1", sec(4)))
	tb.Tick(min(11)) // the ended grace is 10 minutes, so h1 is gone
	if slotOf(tb, key("h1")) != -2 {
		t.Fatal("h1 should be gone")
	}
	tb.ApplyHook(ev("UserPromptSubmit", "h0", min(11)))
	tb.ApplyHook(ev("UserPromptSubmit", "h2", min(11)))
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusIdle), agent("px", "codex", herdr.StatusIdle, "")), min(12))
	if got := slotOf(tb, key("c1")); got != 1 {
		t.Fatalf("claude slot = %d, want 1", got)
	}
	if got := slotOf(tb, "herdr:px"); got != 3 {
		t.Fatalf("codex slot = %d, want 3", got)
	}
	// The same snapshot again changes nothing.
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusIdle), agent("px", "codex", herdr.StatusIdle, "")), min(13))
	if slotOf(tb, key("c1")) != 1 || slotOf(tb, "herdr:px") != 3 || len(tb.All()) != 4 {
		t.Fatalf("slots changed: %d %d, sessions %d", slotOf(tb, key("c1")), slotOf(tb, "herdr:px"), len(tb.All()))
	}
}

func TestHerdrSeedNonClaudeIdleGetsAFreeSlot(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(
		agent("p1", "codex", herdr.StatusIdle, ""),
		agent("p2", "codex", herdr.StatusDone, ""),
		agent("p3", "codex", herdr.StatusUnknown, ""),
	), t0)
	for _, k := range []string{"herdr:p1", "herdr:p2", "herdr:p3"} {
		if s := mustGet(t, tb, k); s.Slot < 0 {
			t.Errorf("%s has no slot", k)
		}
	}
}

func TestHerdrSeedIgnoredIdIsNotAdmitted(t *testing.T) {
	tb := newTable(session.Config{})
	h := start("c1", t0)
	h.Attended = "0" // headless: c1 joins the ignored set
	tb.ApplyHook(h)
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusWorking), herdrClaude(2, herdr.StatusIdle)), sec(1))
	if tb.Get(key("c1")) != nil {
		t.Fatal("ignored id admitted")
	}
	if tb.Get(key("c2")) == nil {
		t.Fatal("other agent not admitted")
	}
	if n := len(tb.All()); n != 1 {
		t.Fatalf("sessions = %d", n)
	}
}

func TestHerdrSeedIgnoredCWDIsNotAdmitted(t *testing.T) {
	tb := newTable(session.Config{})
	a := herdrClaude(1, herdr.StatusIdle)
	a.CWD = "/ignored/deep/dir"
	b := herdrClaude(2, herdr.StatusWorking)
	b.CWD = "/ignored"
	tb.ApplyHerdr(snap(a, b), t0)
	if len(tb.All()) != 0 {
		t.Fatalf("ignored cwd admitted: %+v", tb.All())
	}
}

func TestHerdrSeedNeedsASessionId(t *testing.T) {
	tb := newTable(session.Config{})
	path := herdrClaude(2, herdr.StatusIdle)
	path.Session = &herdr.AgentSession{Kind: "path", Value: "/x"}
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, ""), path), t0)
	if len(tb.All()) != 0 {
		t.Fatalf("admitted without an id: %+v", tb.All())
	}
}

func TestHerdrSeedNeverEvictsAnIdleAgent(t *testing.T) {
	tb := newTable(session.Config{})
	var as []herdr.Agent
	for i := 1; i <= 10; i++ {
		as = append(as, herdrClaude(i, herdr.StatusIdle))
	}
	tb.ApplyHerdr(snap(as...), t0)
	before := map[string]int{}
	for _, s := range tb.Slots() {
		if s == nil {
			t.Fatal("slot empty")
		}
		before[s.Key] = s.Slot
	}
	// Three more agents, all idle or done: no free key, no eviction.
	more := append([]herdr.Agent(nil), as...)
	more = append(more, herdrClaude(11, herdr.StatusIdle), herdrClaude(12, herdr.StatusDone),
		agent("px", "codex", herdr.StatusIdle, ""))
	tb.ApplyHerdr(snap(more...), sec(1))
	for k, slot := range before {
		if got := slotOf(tb, k); got != slot {
			t.Errorf("%s moved from %d to %d", k, slot, got)
		}
	}
	for _, k := range []string{key("c11"), key("c12"), "herdr:px"} {
		s := mustGet(t, tb, k) // known, just without a key
		if s.Slot != -1 {
			t.Errorf("%s slot = %d, want -1", k, s.Slot)
		}
	}
	// Nothing changes on a repeat snapshot either.
	tb.ApplyHerdr(snap(more...), sec(2))
	for k, slot := range before {
		if got := slotOf(tb, k); got != slot {
			t.Errorf("%s moved from %d to %d", k, slot, got)
		}
	}
}

func TestHerdrSeedNoEvictionOfHookSessionsEither(t *testing.T) {
	tb := newTable(session.Config{})
	fill(tb, 10) // ten working CLI sessions
	for i := 0; i < 10; i++ {
		tb.ApplyHook(ev("Stop", fmt.Sprintf("s%d", i), sec(20+i)))
		tb.Focused(key(fmt.Sprintf("s%d", i)), sec(30+i)) // idle and seen: evictable
	}
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusIdle)), sec(40))
	if s := mustGet(t, tb, key("c1")); s.Slot != -1 {
		t.Fatalf("idle seed took slot %d from an idle session", s.Slot)
	}
	for i := 0; i < 10; i++ {
		if slotOf(tb, key(fmt.Sprintf("s%d", i))) < 0 {
			t.Fatalf("s%d lost its key", i)
		}
	}
}

func TestHerdrSeedWorkingAgentMayStillEvict(t *testing.T) {
	tb := newTable(session.Config{})
	fill(tb, 10)
	for i := 0; i < 10; i++ {
		tb.ApplyHook(ev("Stop", fmt.Sprintf("s%d", i), sec(20+i)))
		tb.Focused(key(fmt.Sprintf("s%d", i)), sec(30+i))
	}
	// Existing rule: working activation evicts the least recently active idle session.
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusWorking)), sec(40))
	if s := mustGet(t, tb, key("c1")); s.Slot < 0 || s.State != session.Working {
		t.Fatalf("working agent got no key: %+v", s)
	}
}

func TestHerdrSeedHooksJoinTheSeededEntry(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusIdle)), t0)
	seeded := mustGet(t, tb, key("c1"))
	slot := seeded.Slot

	tb.ApplyHook(ev("UserPromptSubmit", "c1", sec(1)))
	if n := len(tb.All()); n != 1 {
		t.Fatalf("sessions = %d", n)
	}
	s := mustGet(t, tb, key("c1"))
	if s != seeded || s.State != session.Working || s.Slot != slot || s.HerdrPane != "p1" || s.Frontend != session.FrontendHerdr {
		t.Fatalf("after prompt: %+v", s)
	}
	tb.ApplyHook(perm("c1", "r1", sec(2)))
	if s := mustGet(t, tb, key("c1")); len(s.Pending) != 1 {
		t.Fatalf("pending = %+v", s.Pending)
	}
	tb.ApplyHook(ev("Stop", "c1", sec(3)))
	if s := mustGet(t, tb, key("c1")); s.State != session.Done || len(tb.All()) != 1 {
		t.Fatalf("after stop: %+v", s)
	}
	tb.ApplyHook(ev("SessionEnd", "c1", sec(4)))
	if s := mustGet(t, tb, key("c1")); !s.Ended {
		t.Fatalf("not ended: %+v", s)
	}
}

func TestHerdrSeedHostSessionFromLaterHooks(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusIdle)), t0)
	tb.ApplyHook(withHost(ev("PostToolUse", "c1", sec(1)), "local_aaa"))
	s := mustGet(t, tb, key("c1"))
	if s.HostSession != "local_aaa" || s.HostConflict {
		t.Fatalf("host = %q conflict %v", s.HostSession, s.HostConflict)
	}
}

func TestHerdrSeedHookFirstThenHerdrStillOneEntry(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusIdle)), sec(1))
	if n := len(tb.All()); n != 1 {
		t.Fatalf("sessions = %d", n)
	}
}

func TestHerdrSeedReplacesStaleNonClaudeEntryInThePane(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusIdle, "")), t0)
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "c1")), sec(1))
	if tb.Get("herdr:p1") != nil {
		t.Fatal("stale entry stayed")
	}
	if s := mustGet(t, tb, key("c1")); s.Slot < 0 {
		t.Fatalf("seeded session has no key: %+v", s)
	}
}

func TestHerdrSeedLabels(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusIdle)), t0)
	if got := mustGet(t, tb, key("c1")).Label; got != "p1" {
		t.Fatalf("label = %q", got)
	}
}

// A seeded session has no hook that could end it, so its pane closing ends it.
func TestHerdrSeedPaneLossFreesTheKey(t *testing.T) {
	for _, st := range []herdr.Status{herdr.StatusIdle, herdr.StatusWorking, herdr.StatusBlocked} {
		t.Run(string(st), func(t *testing.T) {
			tb := newTable(session.Config{})
			tb.ApplyHerdr(snap(herdrClaude(1, st)), t0)
			if slotOf(tb, key("c1")) != 0 {
				t.Fatalf("not seeded into slot 0")
			}
			tb.ApplyHerdr(snap(), min(25))
			tb.Tick(min(31))
			if got := slotOf(tb, key("c1")); got != -2 {
				t.Fatalf("seeded session stayed after its pane closed (slot %d)", got)
			}
			if tb.Slots()[0] != nil {
				t.Fatal("slot 0 still held")
			}
		})
	}
}

// Once a hook event arrives, hooks own the session: a closed pane only
// clears the pane, as for any hook session.
func TestHerdrSeedPaneLossAfterHookKeepsTheSession(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusIdle)), t0)
	tb.ApplyHook(ev("UserPromptSubmit", "c1", sec(1)))
	tb.ApplyHerdr(snap(), sec(2))
	s := mustGet(t, tb, key("c1"))
	if s.HerdrPane != "" || s.State != session.Working {
		t.Fatalf("after pane loss: %+v", s)
	}
}

// A pane that now holds another Claude session frees the old seeded key too.
func TestHerdrSeedPaneReusedByAnotherSessionFreesTheOldKey(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "c1")), t0)
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "c2")), sec(1))
	if tb.Get(key("c1")) != nil {
		t.Fatal("old seeded session stayed")
	}
	if s := mustGet(t, tb, key("c2")); s.HerdrPane != "p1" {
		t.Fatalf("c2 = %+v", s)
	}
}

// Seeding must not bring back a session that ended and was forgotten.
func TestHerdrSeedDoesNotResurrectAnEndedSession(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
	tb.ApplyHook(ev("SessionEnd", "c1", sec(1)))
	tb.Tick(min(11)) // the ended grace is 10 minutes: c1 is forgotten
	if tb.Get(key("c1")) != nil {
		t.Fatal("c1 should be gone")
	}
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusIdle)), min(12))
	if tb.Get(key("c1")) != nil {
		t.Fatal("ended session was seeded again")
	}
	// A new hook event for the same id revives it as usual.
	tb.ApplyHook(ev("UserPromptSubmit", "c1", min(13)))
	if s := mustGet(t, tb, key("c1")); s.Ended {
		t.Fatalf("revived session = %+v", s)
	}
	// The memory expires.
	tb.ApplyHook(ev("SessionEnd", "c1", min(14)))
	tb.Tick(min(25))
	tb.Tick(min(25 + 24*60))
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusIdle)), min(25+24*60+1))
	if tb.Get(key("c1")) == nil {
		t.Fatal("a day later the id should seed again")
	}
}

// A blocked cross-check works for a seeded entry: Herdr moves it off blocked.
func TestHerdrSeedBlockedThenWorkingWithoutHooks(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusBlocked)), t0)
	if s := mustGet(t, tb, key("c1")); s.State != session.NeedsInput {
		t.Fatalf("seeded state = %v", s.State)
	}
	tb.ApplyHerdr(snap(herdrClaude(1, herdr.StatusWorking)), sec(1))
	if s := mustGet(t, tb, key("c1")); s.State != session.Working {
		t.Fatalf("state = %v, want working", s.State)
	}
}

// Two panes with one session id make one entry, and the pane is stable.
func TestHerdrSeedTwoPanesOneSessionId(t *testing.T) {
	tb := newTable(session.Config{})
	a := agent("p1", "claude", herdr.StatusIdle, "c1")
	b := agent("p2", "claude", herdr.StatusIdle, "c1")
	for i := 0; i < 3; i++ {
		tb.ApplyHerdr(snap(a, b), sec(i))
	}
	if n := len(tb.All()); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
	first := mustGet(t, tb, key("c1")).HerdrPane
	tb.ApplyHerdr(snap(a, b), sec(5))
	if got := mustGet(t, tb, key("c1")).HerdrPane; got != first {
		t.Fatalf("pane moved from %q to %q", first, got)
	}
}

func TestHerdrSeedUnavailableSnapshotAdmitsNothing(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(herdr.Snapshot{Available: false, Agents: []herdr.Agent{herdrClaude(1, herdr.StatusIdle)}}, t0)
	if len(tb.All()) != 0 {
		t.Fatal("admitted from an unavailable snapshot")
	}
}
