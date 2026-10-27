package session_test

import (
	"fmt"
	"testing"
	"time"

	"cm2-claude/internal/herdr"
	"cm2-claude/internal/session"
)

var t0 = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// sec returns t0 plus n seconds.
func sec(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }

// min returns t0 plus n minutes.
func min(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

// newTable makes a table with a fixed ignore list, so tests do not depend on HOME.
func newTable(cfg session.Config) *session.Table {
	if cfg.IgnoreCWDPrefixes == nil {
		cfg.IgnoreCWDPrefixes = []string{"/ignored"}
	}
	return session.New(cfg)
}

// ev builds a hook event for an attended CLI session.
func ev(name, id string, at time.Time) session.HookEvent {
	return session.HookEvent{Name: name, SessionID: id, CWD: "/work/" + id, Entrypoint: "cli", At: at}
}

func start(id string, at time.Time) session.HookEvent {
	e := ev("SessionStart", id, at)
	e.Attended = "1"
	e.Source = "startup"
	return e
}

func perm(id, req string, at time.Time) session.HookEvent {
	e := ev("PermissionRequest", id, at)
	e.RequestID, e.ToolName = req, "Bash"
	return e
}

func key(id string) string { return "claude:" + id }

func agent(pane, kind string, st herdr.Status, sid string) herdr.Agent {
	a := herdr.Agent{PaneID: pane, Kind: kind, Status: st, CWD: "/work/" + pane}
	if sid != "" {
		a.Session = &herdr.AgentSession{Kind: "id", Value: sid}
	}
	return a
}

func snap(agents ...herdr.Agent) herdr.Snapshot {
	return herdr.Snapshot{Available: true, Agents: agents}
}

// mustGet fails the test when the session is missing.
func mustGet(t *testing.T, tb *session.Table, k string) *session.Session {
	t.Helper()
	s := tb.Get(k)
	if s == nil {
		t.Fatalf("session %q not in table", k)
	}
	return s
}

// slotOf returns the slot of a key, or -2 when the session is missing.
func slotOf(tb *session.Table, k string) int {
	if s := tb.Get(k); s != nil {
		return s.Slot
	}
	return -2
}

// fill makes n working CLI sessions s0..s(n-1), one second apart.
func fill(tb *session.Table, n int) {
	for i := 0; i < n; i++ {
		tb.ApplyHook(ev("UserPromptSubmit", fmt.Sprintf("s%d", i), sec(i)))
	}
}
