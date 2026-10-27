package session_test

import (
	"testing"
	"time"

	"cm2-claude/internal/session"
)

func TestClaudeStateMachine(t *testing.T) {
	notif := func(typ string, at time.Time) session.HookEvent {
		e := ev("Notification", "a", at)
		e.NotificationType = typ
		return e
	}
	question := func(at time.Time) session.HookEvent {
		e := ev("PermissionRequest", "a", at)
		e.ToolName, e.IsQuestion = "AskUserQuestion", true
		return e
	}
	rel := func(req string, at time.Time) session.HookEvent {
		e := ev("PermissionReleased", "a", at)
		e.RequestID = req
		return e
	}
	tests := []struct {
		name     string
		events   []session.HookEvent
		now      time.Time
		state    session.State // stored
		visible  session.State
		pending  int
		question bool
	}{
		{"start only is idle", []session.HookEvent{start("a", t0)}, sec(0), session.Idle, session.Idle, 0, false},
		{"prompt works", []session.HookEvent{ev("UserPromptSubmit", "a", t0)}, sec(0), session.Working, session.Working, 0, false},
		{"stop is done", []session.HookEvent{ev("UserPromptSubmit", "a", t0), ev("Stop", "a", sec(1))}, sec(2), session.Done, session.Done, 0, false},
		{"stop failure is error", []session.HookEvent{ev("UserPromptSubmit", "a", t0), ev("StopFailure", "a", sec(1))}, sec(2), session.Error, session.Error, 0, false},
		{"prompt clears error", []session.HookEvent{ev("UserPromptSubmit", "a", t0), ev("StopFailure", "a", sec(1)), ev("UserPromptSubmit", "a", sec(2))}, sec(3), session.Working, session.Working, 0, false},
		{"prompt clears done", []session.HookEvent{ev("UserPromptSubmit", "a", t0), ev("Stop", "a", sec(1)), ev("UserPromptSubmit", "a", sec(2))}, sec(3), session.Working, session.Working, 0, false},
		{"permission pending shows after delay only", []session.HookEvent{ev("UserPromptSubmit", "a", t0), perm("a", "r1", sec(1))}, sec(1), session.Working, session.Working, 1, false},
		{"permission pending older than delay", []session.HookEvent{ev("UserPromptSubmit", "a", t0), perm("a", "r1", sec(1))}, sec(2), session.Working, session.NeedsInput, 1, false},
		{"two pending", []session.HookEvent{perm("a", "r1", t0), perm("a", "r2", sec(1))}, sec(5), session.Working, session.NeedsInput, 2, false},
		{"duplicate request id kept once", []session.HookEvent{perm("a", "r1", t0), perm("a", "r1", sec(1))}, sec(5), session.Working, session.NeedsInput, 1, false},
		{"released one of two", []session.HookEvent{perm("a", "r1", t0), perm("a", "r2", sec(1)), rel("r1", sec(2))}, sec(5), session.Working, session.NeedsInput, 1, false},
		{"released last pending", []session.HookEvent{perm("a", "r1", t0), rel("r1", sec(2))}, sec(5), session.Working, session.Working, 0, false},
		{"released unknown id is a no-op", []session.HookEvent{perm("a", "r1", t0), rel("zz", sec(2))}, sec(5), session.Working, session.NeedsInput, 1, false},
		{"post tool use clears pending", []session.HookEvent{perm("a", "r1", t0), ev("PostToolUse", "a", sec(1))}, sec(5), session.Working, session.Working, 0, false},
		{"stop clears pending", []session.HookEvent{perm("a", "r1", t0), ev("Stop", "a", sec(1))}, sec(5), session.Done, session.Done, 0, false},
		{"stop failure clears pending", []session.HookEvent{perm("a", "r1", t0), ev("StopFailure", "a", sec(1))}, sec(5), session.Error, session.Error, 0, false},
		{"prompt clears pending", []session.HookEvent{perm("a", "r1", t0), ev("UserPromptSubmit", "a", sec(1))}, sec(5), session.Working, session.Working, 0, false},
		{"question shows at once", []session.HookEvent{question(t0)}, sec(0), session.Working, session.NeedsInput, 0, true},
		{"post tool use clears question", []session.HookEvent{question(t0), ev("PostToolUse", "a", sec(1))}, sec(1), session.Working, session.Working, 0, false},
		{"stop clears question", []session.HookEvent{question(t0), ev("Stop", "a", sec(1))}, sec(1), session.Done, session.Done, 0, false},
		{"prompt clears question", []session.HookEvent{question(t0), ev("UserPromptSubmit", "a", sec(1))}, sec(1), session.Working, session.Working, 0, false},
		{"permission_prompt cue with no pending", []session.HookEvent{ev("UserPromptSubmit", "a", t0), notif("permission_prompt", sec(6))}, sec(6), session.NeedsInput, session.NeedsInput, 0, false},
		{"permission_prompt cue ignored with pending", []session.HookEvent{perm("a", "r1", t0), notif("permission_prompt", sec(6))}, sec(6), session.Working, session.NeedsInput, 1, false},
		{"idle_prompt does not change state", []session.HookEvent{ev("UserPromptSubmit", "a", t0), ev("Stop", "a", sec(1)), notif("idle_prompt", sec(2))}, sec(3), session.Done, session.Done, 0, false},
		{"cue cleared by post tool use", []session.HookEvent{ev("UserPromptSubmit", "a", t0), notif("permission_prompt", sec(6)), ev("PostToolUse", "a", sec(7))}, sec(8), session.Working, session.Working, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTable(session.Config{})
			for _, e := range tc.events {
				tb.ApplyHook(e)
			}
			s := mustGet(t, tb, key("a"))
			if s.State != tc.state {
				t.Errorf("state = %v, want %v", s.State, tc.state)
			}
			if got := tb.Visible(s, tc.now); got != tc.visible {
				t.Errorf("visible = %v, want %v", got, tc.visible)
			}
			if len(s.Pending) != tc.pending {
				t.Errorf("pending = %d, want %d", len(s.Pending), tc.pending)
			}
			if s.Question != tc.question {
				t.Errorf("question = %v, want %v", s.Question, tc.question)
			}
		})
	}
}

func TestPendingOrderAndFields(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(perm("a", "r1", sec(1)))
	tb.ApplyHook(perm("a", "r2", sec(3)))
	s := mustGet(t, tb, key("a"))
	if s.Pending[0].RequestID != "r1" || s.Pending[1].RequestID != "r2" {
		t.Fatalf("order = %+v", s.Pending)
	}
	if s.Pending[0].Tool != "Bash" || !s.Pending[0].Since.Equal(sec(1)) {
		t.Fatalf("fields = %+v", s.Pending[0])
	}
}

func TestNeedsInputDelayConfig(t *testing.T) {
	tests := []struct {
		name  string
		delay time.Duration
		now   time.Time
		want  session.State
	}{
		{"default 1s, just under", 0, t0.Add(999 * time.Millisecond), session.Working},
		{"default 1s, reached", 0, sec(1), session.NeedsInput},
		{"custom 5s, under", 5 * time.Second, sec(4), session.Working},
		{"custom 5s, reached", 5 * time.Second, sec(5), session.NeedsInput},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTable(session.Config{NeedsInputDelay: tc.delay})
			tb.ApplyHook(perm("a", "r1", t0))
			if got := tb.Visible(mustGet(t, tb, key("a")), tc.now); got != tc.want {
				t.Fatalf("visible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOldestPendingDecidesDelay(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(perm("a", "r1", t0))
	tb.ApplyHook(perm("a", "r2", sec(10)))
	if got := tb.Visible(mustGet(t, tb, key("a")), sec(10)); got != session.NeedsInput {
		t.Fatalf("visible = %v", got)
	}
}

func TestEveryEventUpdatesLastActive(t *testing.T) {
	for _, name := range []string{"UserPromptSubmit", "Stop", "StopFailure", "PostToolUse", "Notification", "PermissionRequest", "PermissionReleased"} {
		t.Run(name, func(t *testing.T) {
			tb := newTable(session.Config{})
			tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
			tb.ApplyHook(ev(name, "a", sec(30)))
			if got := mustGet(t, tb, key("a")).LastActive; !got.Equal(sec(30)) {
				t.Fatalf("LastActive = %v", got)
			}
		})
	}
}

func TestZeroEventTimeUsesLatestClock(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(40)))
	tb.ApplyHook(ev("Stop", "a", time.Time{}))
	if got := mustGet(t, tb, key("a")).LastActive; !got.Equal(sec(40)) {
		t.Fatalf("LastActive = %v", got)
	}
}

func TestSessionEndAndResume(t *testing.T) {
	t.Run("end keeps state and slot", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
		tb.ApplyHook(ev("Stop", "a", sec(1)))
		tb.ApplyHook(ev("SessionEnd", "a", sec(2)))
		s := mustGet(t, tb, key("a"))
		if !s.Ended || !s.EndedAt.Equal(sec(2)) || s.State != session.Done || s.Slot != 0 {
			t.Fatalf("got %+v", s)
		}
		if tb.Visible(s, sec(3)) != session.Done {
			t.Fatal("ended done session should still show done")
		}
	})
	t.Run("end while working shows idle but keeps state", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(perm("a", "r1", t0))
		tb.ApplyHook(ev("SessionEnd", "a", sec(5)))
		s := mustGet(t, tb, key("a"))
		if s.State != session.Working || len(s.Pending) != 0 {
			t.Fatalf("state=%v pending=%v", s.State, s.Pending)
		}
		if got := tb.Visible(s, sec(9)); got != session.Idle {
			t.Fatalf("visible = %v", got)
		}
	})
	t.Run("resume start un-ends", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
		tb.ApplyHook(ev("SessionEnd", "a", sec(1)))
		r := start("a", sec(2))
		r.Source = "resume"
		tb.ApplyHook(r)
		s := mustGet(t, tb, key("a"))
		if s.Ended || !s.EndedAt.IsZero() {
			t.Fatalf("still ended: %+v", s)
		}
	})
	t.Run("non-resume start does not un-end", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
		tb.ApplyHook(ev("SessionEnd", "a", sec(1)))
		tb.ApplyHook(start("a", sec(2)))
		if !mustGet(t, tb, key("a")).Ended {
			t.Fatal("startup start un-ended the session")
		}
	})
	t.Run("new prompt un-ends", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
		tb.ApplyHook(ev("SessionEnd", "a", sec(1)))
		tb.ApplyHook(ev("UserPromptSubmit", "a", sec(2)))
		if s := mustGet(t, tb, key("a")); s.Ended || s.State != session.Working {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("stray event after end changes nothing", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
		tb.ApplyHook(ev("Stop", "a", sec(1)))
		tb.ApplyHook(ev("SessionEnd", "a", sec(2)))
		tb.ApplyHook(ev("StopFailure", "a", sec(3)))
		if s := mustGet(t, tb, key("a")); !s.Ended || s.State != session.Done {
			t.Fatalf("got %+v", s)
		}
	})
}

func TestCWDAndLabel(t *testing.T) {
	tb := newTable(session.Config{})
	e := start("a", t0)
	e.CWD = "/Users/x/Development/proj"
	tb.ApplyHook(e)
	s := mustGet(t, tb, key("a"))
	if s.CWD != "/Users/x/Development/proj" || s.Label != "proj" || s.Kind != "claude" || s.SessionID != "a" || s.Key != key("a") {
		t.Fatalf("got %+v", s)
	}
	m := ev("Stop", "a", sec(1))
	m.CWD = "/Users/x/other"
	tb.ApplyHook(m)
	if s.Label != "other" {
		t.Fatalf("label after cwd change = %q", s.Label)
	}
}

func TestLabelCollisionSuffix(t *testing.T) {
	same := func(id string) session.HookEvent {
		e := ev("UserPromptSubmit", id, t0)
		e.CWD = "/Users/x/proj"
		return e
	}
	tb := newTable(session.Config{})
	tb.ApplyHook(same("abcd1111"))
	if got := mustGet(t, tb, key("abcd1111")).Label; got != "proj" {
		t.Fatalf("single label = %q", got)
	}
	tb.ApplyHook(same("wxyz2222"))
	if got := mustGet(t, tb, key("abcd1111")).Label; got != "proj ·abcd" {
		t.Errorf("first label = %q", got)
	}
	if got := mustGet(t, tb, key("wxyz2222")).Label; got != "proj ·wxyz" {
		t.Errorf("second label = %q", got)
	}
	other := ev("UserPromptSubmit", "qqqq3333", t0)
	other.CWD = "/Users/x/else"
	tb.ApplyHook(other)
	if got := mustGet(t, tb, key("qqqq3333")).Label; got != "else" {
		t.Errorf("unrelated label = %q", got)
	}
	// Removing one removes the suffix from the other.
	tb.ApplyHook(func() session.HookEvent { e := same("wxyz2222"); e.Name = "SessionEnd"; return e }())
	tb.Tick(min(11))
	if got := mustGet(t, tb, key("abcd1111")).Label; got != "proj" {
		t.Errorf("label after collision gone = %q", got)
	}
}

func TestLabelFallbacksAndHerdrCollision(t *testing.T) {
	tb := newTable(session.Config{})
	a := agent("w1-p1", "codex", "working", "")
	b := agent("w1-p2", "codex", "working", "")
	a.CWD, b.CWD = "/r/app", "/r/app"
	tb.ApplyHerdr(snap(a, b), t0)
	if got := mustGet(t, tb, "herdr:w1-p1").Label; got != "app ·1-p1" {
		t.Errorf("label = %q", got)
	}
	nc := agent("p9", "codex", "idle", "")
	nc.CWD = ""
	tb.ApplyHerdr(snap(nc), sec(1))
	if got := mustGet(t, tb, "herdr:p9").Label; got != "codex" {
		t.Errorf("no-cwd label = %q", got)
	}
}

func TestFocusedMarksSeen(t *testing.T) {
	tests := []struct {
		name  string
		setup []session.HookEvent
		want  session.State
	}{
		{"done becomes idle", []session.HookEvent{ev("UserPromptSubmit", "a", t0), ev("Stop", "a", sec(1))}, session.Idle},
		{"working stays", []session.HookEvent{ev("UserPromptSubmit", "a", t0)}, session.Working},
		{"error stays", []session.HookEvent{ev("UserPromptSubmit", "a", t0), ev("StopFailure", "a", sec(1))}, session.Error},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTable(session.Config{})
			for _, e := range tc.setup {
				tb.ApplyHook(e)
			}
			tb.Focused(key("a"), sec(10))
			s := mustGet(t, tb, key("a"))
			if s.State != tc.want {
				t.Fatalf("state = %v, want %v", s.State, tc.want)
			}
			if !s.LastActive.Equal(sec(10)) {
				t.Fatalf("LastActive = %v", s.LastActive)
			}
		})
	}
	tb := newTable(session.Config{})
	tb.Focused("claude:none", t0) // unknown key must not panic
}

// A PermissionRequest is activity: it un-ends an ended session, makes it
// Working, and adds the Pending. (The spec section 3.1 names this.)
func TestPermissionRequestRevivesAndWorks(t *testing.T) {
	t.Run("revives an ended session", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
		tb.ApplyHook(ev("SessionEnd", "a", sec(1)))
		tb.ApplyHook(perm("a", "r1", sec(2)))
		s := mustGet(t, tb, key("a"))
		if s.Ended || !s.EndedAt.IsZero() || len(s.Pending) != 1 || s.State != session.Working {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("a done session works again", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
		tb.ApplyHook(ev("Stop", "a", sec(1)))
		tb.ApplyHook(perm("a", "r1", sec(2)))
		if s := mustGet(t, tb, key("a")); s.State != session.Working || len(s.Pending) != 1 {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("another event does not revive", func(t *testing.T) {
		tb := newTable(session.Config{})
		tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
		tb.ApplyHook(ev("SessionEnd", "a", sec(1)))
		tb.ApplyHook(ev("PostToolUse", "a", sec(2)))
		if !mustGet(t, tb, key("a")).Ended {
			t.Fatal("PostToolUse un-ended the session")
		}
	})
}
