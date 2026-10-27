package session_test

import (
	"testing"

	"cm2-claude/internal/session"
)

func withHost(e session.HookEvent, host string) session.HookEvent {
	e.HostSession = host
	return e
}

func TestHostSessionStoredOnFirstEvent(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(withHost(ev("UserPromptSubmit", "c1", t0), "local_aaa"))
	s := mustGet(t, tb, key("c1"))
	if s.HostSession != "local_aaa" || s.HostConflict {
		t.Fatalf("host = %q conflict = %v", s.HostSession, s.HostConflict)
	}
}

func TestHostSessionFilledByLaterEvent(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(ev("UserPromptSubmit", "c1", t0))
	if s := mustGet(t, tb, key("c1")); s.HostSession != "" || s.HostConflict {
		t.Fatalf("host = %q conflict = %v", s.HostSession, s.HostConflict)
	}
	tb.ApplyHook(withHost(ev("PostToolUse", "c1", sec(1)), "local_aaa"))
	if s := mustGet(t, tb, key("c1")); s.HostSession != "local_aaa" || s.HostConflict {
		t.Fatalf("host = %q conflict = %v", s.HostSession, s.HostConflict)
	}
}

func TestHostSessionSameIdNoConflict(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(withHost(ev("UserPromptSubmit", "c1", t0), "local_aaa"))
	tb.ApplyHook(withHost(ev("PostToolUse", "c1", sec(1)), "local_aaa"))
	tb.ApplyHook(ev("Stop", "c1", sec(2))) // no host id: no change
	if s := mustGet(t, tb, key("c1")); s.HostSession != "local_aaa" || s.HostConflict {
		t.Fatalf("host = %q conflict = %v", s.HostSession, s.HostConflict)
	}
}

func TestHostSessionConflictIsStickyAndKeepsFirst(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(withHost(ev("UserPromptSubmit", "c1", t0), "local_aaa"))
	tb.ApplyHook(withHost(ev("PostToolUse", "c1", sec(1)), "local_bbb"))
	s := mustGet(t, tb, key("c1"))
	if s.HostSession != "local_aaa" || !s.HostConflict {
		t.Fatalf("host = %q conflict = %v", s.HostSession, s.HostConflict)
	}
	// The conflict stays after more events, same or no id.
	tb.ApplyHook(withHost(ev("PostToolUse", "c1", sec(2)), "local_aaa"))
	tb.ApplyHook(ev("Stop", "c1", sec(3)))
	s = mustGet(t, tb, key("c1"))
	if s.HostSession != "local_aaa" || !s.HostConflict {
		t.Fatalf("host = %q conflict = %v", s.HostSession, s.HostConflict)
	}
}

func TestHostSessionOnSessionStartAndPermission(t *testing.T) {
	tb := newTable(session.Config{})
	e := withHost(start("c1", t0), "local_aaa")
	tb.ApplyHook(e)
	p := withHost(perm("c1", "r1", sec(1)), "local_bbb")
	tb.ApplyHook(p)
	s := mustGet(t, tb, key("c1"))
	if s.HostSession != "local_aaa" || !s.HostConflict {
		t.Fatalf("host = %q conflict = %v", s.HostSession, s.HostConflict)
	}
}

func TestHostSessionIsPerSession(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(withHost(ev("UserPromptSubmit", "c1", t0), "local_aaa"))
	tb.ApplyHook(withHost(ev("UserPromptSubmit", "c2", t0), "local_bbb"))
	for id, want := range map[string]string{"c1": "local_aaa", "c2": "local_bbb"} {
		if s := mustGet(t, tb, key(id)); s.HostSession != want || s.HostConflict {
			t.Fatalf("%s: host = %q conflict = %v", id, s.HostSession, s.HostConflict)
		}
	}
}

func TestValidHostID(t *testing.T) {
	good := []string{"local_a", "local_1d9854de-0000-4000-8000-123456789abc", "local_ABCxyz-09"}
	for _, id := range good {
		if !session.ValidHostID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	long := "local_"
	for i := 0; i < 64; i++ {
		long += "a"
	}
	if !session.ValidHostID(long) {
		t.Error("64 characters should be valid")
	}
	bad := []string{
		"", "local_", long + "a", "$CLAUDE_CODE_HOST_SESSION_ID", "local_../x", "local_a/b", "local_a b",
		"local_a\n", "\nlocal_a", "local_a%2f", "Local_a", "local-a", "xlocal_a", "local_a_b", "local_é", "local_a.json",
		"local_a?x=1", "local_a\x00",
	}
	for _, id := range bad {
		if session.ValidHostID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}
