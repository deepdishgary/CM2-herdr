package session_test

import (
	"testing"

	"cm2-claude/internal/herdr"
	"cm2-claude/internal/session"
)

func TestHerdrNonClaudeStatus(t *testing.T) {
	tests := []struct {
		status herdr.Status
		state  session.State
		slot   bool // gets a slot (every first-seen agent takes a free one)
	}{
		{herdr.StatusWorking, session.Working, true},
		{herdr.StatusBlocked, session.NeedsInput, true},
		{herdr.StatusDone, session.Done, true},
		{herdr.StatusIdle, session.Idle, true},
		{herdr.StatusUnknown, session.Idle, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			tb := newTable(session.Config{})
			tb.ApplyHerdr(snap(agent("p1", "codex", tc.status, "")), t0)
			s := mustGet(t, tb, "herdr:p1")
			if s.State != tc.state {
				t.Errorf("state = %v, want %v", s.State, tc.state)
			}
			if (s.Slot >= 0) != tc.slot {
				t.Errorf("slot = %d, want slot=%v", s.Slot, tc.slot)
			}
			if s.Kind != "codex" || s.Frontend != session.FrontendHerdr || s.HerdrPane != "p1" || s.SessionID != "" || s.CWD != "/work/p1" {
				t.Errorf("fields = %+v", s)
			}
			if got := tb.Visible(s, t0); got != tc.state {
				t.Errorf("visible = %v", got)
			}
		})
	}
}

func TestHerdrStatusTransitions(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusWorking, "")), t0)
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusDone, "")), sec(5))
	s := mustGet(t, tb, "herdr:p1")
	if s.State != session.Done || s.Slot != 0 || !s.LastActive.Equal(sec(5)) {
		t.Fatalf("after done: %+v", s)
	}
	// Seen locally; an unchanged Herdr status must not flip it back to Done.
	tb.Focused("herdr:p1", sec(6))
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusDone, "")), sec(7))
	if s.State != session.Idle {
		t.Fatalf("state = %v, want idle after focus", s.State)
	}
	// A new run starts again.
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusWorking, "")), sec(8))
	if s.State != session.Working || s.Slot != 0 {
		t.Fatalf("after rerun: %+v", s)
	}
}

func TestHerdrUnchangedSnapshotKeepsLastActive(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusIdle, "")), t0)
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusIdle, "")), min(20))
	if got := mustGet(t, tb, "herdr:p1").LastActive; !got.Equal(t0) {
		t.Fatalf("LastActive = %v", got)
	}
}

func TestHerdrKindAndCWDUpdate(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusIdle, "")), t0)
	a := agent("p1", "gemini", herdr.StatusIdle, "")
	a.CWD = "/other/dir"
	tb.ApplyHerdr(snap(a), sec(1))
	s := mustGet(t, tb, "herdr:p1")
	if s.Kind != "gemini" || s.CWD != "/other/dir" || s.Label != "dir" {
		t.Fatalf("got %+v", s)
	}
}

func TestHerdrPaneGone(t *testing.T) {
	t.Run("non-claude removed and slot freed", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusWorking, "")), t0)
		tb.Select(0)
		tb.ApplyHerdr(snap(), sec(1))
		if tb.Get("herdr:p1") != nil || tb.Slots()[0] != nil {
			t.Fatal("session or slot stayed")
		}
		if tb.Selected() != -1 {
			t.Fatalf("selected = %d", tb.Selected())
		}
	})
	t.Run("claude loses pane and key but stays", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusWorking, "c1")), sec(1))
		if s := mustGet(t, tb, key("c1")); s.HerdrPane != "p1" || s.Frontend != session.FrontendHerdr {
			t.Fatalf("join: %+v", s)
		}
		tb.ApplyHerdr(snap(), sec(2))
		s := mustGet(t, tb, key("c1"))
		if s.HerdrPane != "" || s.Slot != -1 || s.Frontend != session.FrontendTerminal {
			t.Fatalf("after loss: %+v", s)
		}
	})
	t.Run("claude pane taken by another session", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "c1")), sec(1))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "c2")), sec(2))
		if s := mustGet(t, tb, key("c1")); s.HerdrPane != "" {
			t.Fatalf("pane = %q", s.HerdrPane)
		}
	})
	t.Run("claude pane without session id yet is kept", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "c1")), sec(1))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "")), sec(2))
		if s := mustGet(t, tb, key("c1")); s.HerdrPane != "p1" {
			t.Fatalf("pane = %q", s.HerdrPane)
		}
	})
	t.Run("pane now holds a claude agent: stale non-claude entry removed", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHerdr(snap(agent("p1", "codex", herdr.StatusWorking, "")), t0)
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusWorking, "")), sec(1))
		if tb.Get("herdr:p1") != nil {
			t.Fatal("stale entry stayed")
		}
	})
}

func TestHerdrUnavailableKeepsEverything(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
	tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusWorking, "c1"), agent("p2", "codex", herdr.StatusWorking, "")), sec(1))
	tb.ApplyHerdr(herdr.Snapshot{Available: false}, sec(2))
	if s := mustGet(t, tb, key("c1")); s.HerdrPane != "p1" {
		t.Errorf("claude pane = %q", s.HerdrPane)
	}
	if tb.Get("herdr:p2") == nil {
		t.Error("codex session dropped")
	}
}

func TestHerdrClaudeJoin(t *testing.T) {
	t.Run("joins hook session: one slot not two", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusWorking, "c1")), sec(1))
		if n := len(tb.All()); n != 1 {
			t.Fatalf("sessions = %d", n)
		}
		s := mustGet(t, tb, key("c1"))
		if s.HerdrPane != "p1" || s.Frontend != session.FrontendHerdr || s.Slot != 0 {
			t.Fatalf("got %+v", s)
		}
		if tb.Get("herdr:p1") != nil {
			t.Fatal("duplicate herdr: entry")
		}
	})
	t.Run("unknown claude session is admitted", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusWorking, "zzz")), t0)
		s := mustGet(t, tb, key("zzz"))
		if s.SessionID != "zzz" || s.HerdrPane != "p1" || s.Frontend != session.FrontendHerdr || s.Slot != 0 {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("claude agent without session id skipped", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusWorking, "")), t0)
		if len(tb.All()) != 0 {
			t.Fatal("session created")
		}
	})
	t.Run("claude agent with path session skipped", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
		a := agent("p1", "claude", herdr.StatusWorking, "")
		a.Session = &herdr.AgentSession{Kind: "path", Value: "c1"}
		tb.ApplyHerdr(snap(a), sec(1))
		if mustGet(t, tb, key("c1")).HerdrPane != "" {
			t.Fatal("joined on a path")
		}
	})
	t.Run("headless session is not joined", func(t *testing.T) {
		tb := newTable(session.Config{})
		h := start("c1", t0)
		h.Attended = "0"
		tb.ApplyHook(h)
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusWorking, "c1")), sec(1))
		if len(tb.All()) != 0 {
			t.Fatal("headless session joined")
		}
	})
	t.Run("hook pane from session start and herdr agree", func(t *testing.T) {
		tb := newTable(session.Config{})
		e := start("c1", t0)
		e.HerdrPane = "p1"
		tb.ApplyHook(e)
		if mustGet(t, tb, key("c1")).Frontend != session.FrontendHerdr {
			t.Fatal("frontend")
		}
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "c1")), sec(1))
		if n := len(tb.All()); n != 1 {
			t.Fatalf("sessions = %d", n)
		}
	})
	t.Run("herdr cwd fills a missing cwd", func(t *testing.T) {
		tb := newTable(session.Config{})
		e := start("c1", t0)
		e.CWD = ""
		tb.ApplyHook(e)
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusIdle, "c1")), sec(1))
		if got := mustGet(t, tb, key("c1")).CWD; got != "/work/p1" {
			t.Fatalf("cwd = %q", got)
		}
	})
}

func TestHerdrDoesNotTakeClaudeStatus(t *testing.T) {
	tests := []struct {
		name   string
		hooks  []session.HookEvent
		status herdr.Status
		want   session.State
	}{
		{"hooks done beats herdr working", []session.HookEvent{ev("UserPromptSubmit", "c1", t0), ev("Stop", "c1", sec(1))}, herdr.StatusWorking, session.Done},
		{"hooks working beats herdr idle", []session.HookEvent{ev("UserPromptSubmit", "c1", t0)}, herdr.StatusIdle, session.Working},
		{"hooks working beats herdr done", []session.HookEvent{ev("UserPromptSubmit", "c1", t0)}, herdr.StatusDone, session.Working},
		{"hooks done beats herdr blocked", []session.HookEvent{ev("UserPromptSubmit", "c1", t0), ev("Stop", "c1", sec(1))}, herdr.StatusBlocked, session.Done},
		{"hooks idle ignores herdr working", []session.HookEvent{start("c1", t0)}, herdr.StatusWorking, session.Idle},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTable(session.Config{})
			for _, e := range tc.hooks {
				tb.ApplyHook(e)
			}
			tb.ApplyHerdr(snap(agent("p1", "claude", tc.status, "c1")), sec(5))
			if got := mustGet(t, tb, key("c1")).State; got != tc.want {
				t.Fatalf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHerdrBlockedCrossCheck(t *testing.T) {
	t.Run("blocked with no pending is needs-input", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusBlocked, "c1")), sec(1))
		s := mustGet(t, tb, key("c1"))
		if got := tb.Visible(s, sec(1)); got != session.NeedsInput {
			t.Fatalf("visible = %v", got)
		}
	})
	t.Run("blocked with pending keeps hook state", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(perm("c1", "r1", t0))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusBlocked, "c1")), t0)
		s := mustGet(t, tb, key("c1"))
		if s.State != session.Working {
			t.Fatalf("state = %v", s.State)
		}
		if got := tb.Visible(s, t0); got != session.Working { // still inside the delay
			t.Fatalf("visible = %v", got)
		}
	})
	t.Run("unblocked clears the cross-check cue", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusBlocked, "c1")), sec(1))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusWorking, "c1")), sec(2))
		if got := mustGet(t, tb, key("c1")).State; got != session.Working {
			t.Fatalf("state = %v", got)
		}
	})
	t.Run("hook event clears the cue", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusBlocked, "c1")), sec(1))
		tb.ApplyHook(ev("PostToolUse", "c1", sec(2)))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusBlocked, "c1")), sec(3)) // unchanged
		if got := mustGet(t, tb, key("c1")).State; got != session.Working {
			t.Fatalf("state = %v", got)
		}
	})
	t.Run("blocked gives a slot to a known idle session", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(start("c1", t0))
		tb.ApplyHerdr(snap(agent("p1", "claude", herdr.StatusBlocked, "c1")), sec(1))
		if s := mustGet(t, tb, key("c1")); s.Slot != 0 || s.State != session.NeedsInput {
			t.Fatalf("got %+v", s)
		}
	})
}

func TestHerdrIgnoredCWD(t *testing.T) {
	tb := newTable(session.Config{})
	a := agent("p1", "codex", herdr.StatusWorking, "")
	a.CWD = "/ignored/x"
	tb.ApplyHerdr(snap(a), t0)
	if len(tb.All()) != 0 {
		t.Fatal("ignored cwd agent admitted")
	}
}
