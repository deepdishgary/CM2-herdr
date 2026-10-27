package session_test

import (
	"testing"

	"cm2-claude/internal/session"
)

func TestHeadlessAndAdmission(t *testing.T) {
	tests := []struct {
		name   string
		events []session.HookEvent
		want   bool // session "a" is in the table at the end
	}{
		{"attended start registers", []session.HookEvent{start("a", t0)}, true},
		{"headless start ignored", []session.HookEvent{func() session.HookEvent {
			e := start("a", t0)
			e.Attended = "0"
			return e
		}()}, false},
		{"headless start then prompt stays ignored", []session.HookEvent{func() session.HookEvent {
			e := start("a", t0)
			e.Attended = "0"
			return e
		}(), ev("UserPromptSubmit", "a", sec(1)), perm("a", "r1", sec(2))}, false},
		{"start with unknown attended is not registered", []session.HookEvent{func() session.HookEvent {
			e := start("a", t0)
			e.Attended = ""
			return e
		}()}, false},
		{"unregistered prompt from cli is admitted", []session.HookEvent{ev("UserPromptSubmit", "a", t0)}, true},
		{"unregistered prompt from desktop is admitted", []session.HookEvent{func() session.HookEvent {
			e := ev("UserPromptSubmit", "a", t0)
			e.Entrypoint = "claude-desktop"
			return e
		}()}, true},
		{"unregistered permission from cli is admitted", []session.HookEvent{perm("a", "r1", t0)}, true},
		{"unregistered prompt from sdk-cli is dropped", []session.HookEvent{func() session.HookEvent {
			e := ev("UserPromptSubmit", "a", t0)
			e.Entrypoint = "sdk-cli"
			return e
		}()}, false},
		{"unregistered prompt with no entrypoint is dropped", []session.HookEvent{func() session.HookEvent {
			e := ev("UserPromptSubmit", "a", t0)
			e.Entrypoint = ""
			return e
		}()}, false},
		{"unregistered permission from sdk-cli is dropped", []session.HookEvent{func() session.HookEvent {
			e := perm("a", "r1", t0)
			e.Entrypoint = "sdk-cli"
			return e
		}()}, false},
		{"unregistered stop is dropped", []session.HookEvent{ev("Stop", "a", t0)}, false},
		{"unregistered post tool use is dropped", []session.HookEvent{ev("PostToolUse", "a", t0)}, false},
		{"unregistered notification is dropped", []session.HookEvent{func() session.HookEvent {
			e := ev("Notification", "a", t0)
			e.NotificationType = "permission_prompt"
			return e
		}()}, false},
		{"attended 0 on a plain event ignores the session", []session.HookEvent{func() session.HookEvent {
			e := ev("UserPromptSubmit", "a", t0)
			e.Attended = "0"
			return e
		}()}, false},
		{"cwd under ignored prefix", []session.HookEvent{func() session.HookEvent {
			e := start("a", t0)
			e.CWD = "/ignored/proj"
			return e
		}()}, false},
		{"cwd equal to ignored prefix", []session.HookEvent{func() session.HookEvent {
			e := start("a", t0)
			e.CWD = "/ignored"
			return e
		}()}, false},
		{"cwd that only shares the text prefix is kept", []session.HookEvent{func() session.HookEvent {
			e := start("a", t0)
			e.CWD = "/ignored-not"
			return e
		}()}, true},
		{"event without session id is dropped", []session.HookEvent{{Name: "UserPromptSubmit", Entrypoint: "cli", At: t0}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTable(session.Config{})
			for _, e := range tc.events {
				tb.ApplyHook(e)
			}
			if got := tb.Get(key("a")) != nil; got != tc.want {
				t.Fatalf("in table = %v, want %v", got, tc.want)
			}
			if !tc.want {
				for _, s := range tb.Slots() {
					if s != nil {
						t.Fatalf("ignored session holds a slot: %+v", s)
					}
				}
			}
		})
	}
}

func TestHeadlessFilterDropsLaterEventsForLife(t *testing.T) {
	tb := newTable(session.Config{})
	h := start("a", t0)
	h.Attended = "0"
	tb.ApplyHook(h)
	// A later "attended" start with the same id must not revive it.
	tb.ApplyHook(start("a", sec(1)))
	if tb.Get(key("a")) != nil {
		t.Fatal("headless session came back")
	}
}

func TestIgnoreRemovesKnownSession(t *testing.T) {
	tb := newTable(session.Config{})
	tb.ApplyHook(ev("UserPromptSubmit", "a", t0))
	e := ev("Stop", "a", sec(1))
	e.CWD = "/ignored/x"
	tb.ApplyHook(e)
	if tb.Get(key("a")) != nil || tb.Slots()[0] != nil {
		t.Fatal("session under an ignored cwd stayed")
	}
}

func TestDefaultIgnoreIsHomeElara(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tb := session.New(session.Config{})
	in := ev("UserPromptSubmit", "a", t0)
	in.CWD = home + "/elara/jobs"
	tb.ApplyHook(in)
	out := ev("UserPromptSubmit", "b", t0)
	out.CWD = home + "/code"
	tb.ApplyHook(out)
	if tb.Get(key("a")) != nil {
		t.Error("~/elara session was kept")
	}
	if tb.Get(key("b")) == nil {
		t.Error("other session was dropped")
	}
}

func TestConfiguredIgnoreExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tb := session.New(session.Config{IgnoreCWDPrefixes: []string{"~/skip"}})
	e := ev("UserPromptSubmit", "a", t0)
	e.CWD = home + "/skip/x"
	tb.ApplyHook(e)
	if tb.Get(key("a")) != nil {
		t.Fatal("tilde prefix not expanded")
	}
}

func TestEmptyIgnoreListIgnoresNothing(t *testing.T) {
	tb := session.New(session.Config{IgnoreCWDPrefixes: []string{}})
	e := ev("UserPromptSubmit", "a", t0)
	e.CWD = "/anything/elara"
	tb.ApplyHook(e)
	if tb.Get(key("a")) == nil {
		t.Fatal("dropped with an empty ignore list")
	}
}

func TestFrontendRules(t *testing.T) {
	tests := []struct {
		name       string
		entrypoint string
		pane       string
		want       session.Frontend
	}{
		{"desktop", "claude-desktop", "", session.FrontendDesktop},
		{"terminal", "cli", "", session.FrontendTerminal},
		{"herdr pane wins over cli", "cli", "p_1", session.FrontendHerdr},
		{"herdr pane wins over desktop", "claude-desktop", "p_1", session.FrontendHerdr},
		{"no entrypoint", "", "", session.FrontendUnknown},
		{"sdk-cli is unknown", "sdk-cli", "", session.FrontendUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTable(session.Config{})
			e := start("a", t0)
			e.Entrypoint, e.HerdrPane = tc.entrypoint, tc.pane
			tb.ApplyHook(e)
			if got := mustGet(t, tb, key("a")).Frontend; got != tc.want {
				t.Fatalf("frontend = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFrontendFromLaterEvent(t *testing.T) {
	tb := newTable(session.Config{})
	e := start("a", t0)
	e.Entrypoint = ""
	tb.ApplyHook(e)
	tb.ApplyHook(ev("UserPromptSubmit", "a", sec(1))) // header says cli
	if got := mustGet(t, tb, key("a")).Frontend; got != session.FrontendTerminal {
		t.Fatalf("frontend = %q", got)
	}
}
