package focus

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"cm2-claude/internal/session"
)

type fakeDesktop struct {
	ok     bool
	reason string
	asked  []string
}

func (d *fakeDesktop) Check(id string) (bool, string) {
	d.asked = append(d.asked, id)
	return d.ok, d.reason
}

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

var openClaude = []call{{"open", []string{"-a", "Claude"}}}

func continueCall(id string) []call {
	return []call{{"open", []string{"claude://code/continue?session=" + id}}}
}

func desktopSession(host string) *session.Session {
	return &session.Session{Key: "claude:c1", SessionID: "c1", Frontend: session.FrontendDesktop, HostSession: host}
}

func TestDesktopFocusRouting(t *testing.T) {
	tests := []struct {
		name       string
		s          *session.Session
		desktop    *fakeDesktop // nil: Desktop field stays nil
		want       []call
		wantAsked  []string
		wantReason string // logged when the fallback runs
	}{
		{
			name: "ok: exact continue route", s: desktopSession(hostA),
			desktop: &fakeDesktop{ok: true}, want: continueCall(hostA), wantAsked: []string{hostA},
		},
		{
			name: "no host id", s: desktopSession(""),
			desktop: &fakeDesktop{ok: true}, want: openClaude, wantReason: "no host id",
		},
		{
			name: "host conflict", s: func() *session.Session { s := desktopSession(hostA); s.HostConflict = true; return s }(),
			desktop: &fakeDesktop{ok: true}, want: openClaude, wantReason: "host conflict",
		},
		{
			name: "ended", s: func() *session.Session { s := desktopSession(hostA); s.Ended = true; return s }(),
			desktop: &fakeDesktop{ok: true}, want: openClaude, wantReason: "session ended",
		},
		{
			name: "no desktop checker", s: desktopSession(hostA),
			desktop: nil, want: openClaude, wantReason: "no desktop checker",
		},
		{
			name: "check fails", s: desktopSession(hostA),
			desktop: &fakeDesktop{ok: false, reason: "record archived"}, want: openClaude,
			wantAsked: []string{hostA}, wantReason: "record archived",
		},
		{
			name: "check fails without a reason", s: desktopSession(hostA),
			desktop: &fakeDesktop{ok: false}, want: openClaude, wantAsked: []string{hostA}, wantReason: "check failed",
		},
		{
			name: "malformed host id never reaches the checker", s: desktopSession("local_../x"),
			desktop: &fakeDesktop{ok: true}, want: openClaude, wantReason: "invalid host id",
		},
		{
			name: "literal variable never reaches the checker", s: desktopSession("$CLAUDE_CODE_HOST_SESSION_ID"),
			desktop: &fakeDesktop{ok: true}, want: openClaude, wantReason: "invalid host id",
		},
		{
			name: "terminal session with a host id ignores it", s: &session.Session{SessionID: "c1", Frontend: session.FrontendTerminal, HostSession: hostA},
			desktop: &fakeDesktop{ok: true}, want: []call{{"open", []string{"-a", "Ghostty"}}},
		},
		{
			name: "unknown frontend with a host id ignores it", s: &session.Session{SessionID: "c1", Frontend: session.FrontendUnknown, HostSession: hostA},
			desktop: &fakeDesktop{ok: true}, want: []call{{"open", []string{"-a", "Ghostty"}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{}
			lg, buf := testLogger()
			f := &Focuser{Run: r.run, Log: lg}
			if tc.desktop != nil {
				f.Desktop = tc.desktop
			}
			if err := f.Focus(context.Background(), tc.s); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.calls, tc.want) {
				t.Fatalf("calls %v, want %v", r.calls, tc.want)
			}
			if tc.desktop != nil && !reflect.DeepEqual(tc.desktop.asked, tc.wantAsked) {
				t.Fatalf("checked %v, want %v", tc.desktop.asked, tc.wantAsked)
			}
			logged := buf.String()
			if tc.wantReason != "" {
				if !strings.Contains(logged, "level=INFO") || !strings.Contains(logged, tc.wantReason) {
					t.Fatalf("log lacks an Info line with %q:\n%s", tc.wantReason, logged)
				}
			} else if logged != "" {
				t.Fatalf("unexpected log:\n%s", logged)
			}
		})
	}
}

func TestDesktopFocusWithNilLogger(t *testing.T) {
	r := &fakeRunner{}
	f := &Focuser{Run: r.run}
	if err := f.Focus(context.Background(), desktopSession(hostA)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, openClaude) {
		t.Fatalf("calls %v", r.calls)
	}
}

func TestDesktopContinueRunnerErrorIsReturned(t *testing.T) {
	r := &fakeRunner{err: errors.New("boom")}
	f := &Focuser{Run: r.run, Desktop: &fakeDesktop{ok: true}}
	if err := f.Focus(context.Background(), desktopSession(hostA)); err == nil {
		t.Fatal("expected an error")
	}
	// One attempt only: a failed continue does not retry with another route.
	if len(r.calls) != 1 {
		t.Fatalf("calls %v", r.calls)
	}
}

func TestDesktopHerdrPaneWinsOverHostId(t *testing.T) {
	r := &fakeRunner{}
	h := &fakeHerdr{}
	d := &fakeDesktop{ok: true}
	f := &Focuser{Run: r.run, Herdr: h, Desktop: d}
	s := desktopSession(hostA)
	s.HerdrPane = "w1:p1"
	if err := f.Focus(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, []call{{"open", []string{"-a", "Ghostty"}}}) || len(d.asked) != 0 {
		t.Fatalf("calls %v asked %v", r.calls, d.asked)
	}
}

var continueURL = regexp.MustCompile(`^claude://code/continue\?session=local_[0-9A-Za-z-]{1,64}$`)

// The banned-route test. Whatever the session, the host id, or the checker
// says, no focus path may build a URL that is not the exact continue route
// with a validated id, and none may carry a banned word.
func TestNoFocusPathProducesABannedRoute(t *testing.T) {
	banned := []string{"resume", "continue?session=last", "needs-input", "epitaxy"}
	hosts := []string{
		"", hostA, "local_a", "local_resume", "local_Resume", "local_epitaxy", "local_needs-input", "local_last",
		"local_x-resume-y", "last", "resume", "epitaxy", "local_../x", "local_a?session=b", "local_a&x=1",
		"local_a#frag", "local_a/b", "local_a%2e", "claude://resume?session=local_a", "$CLAUDE_CODE_HOST_SESSION_ID",
		"local_" + strings.Repeat("a", 64), "local_" + strings.Repeat("a", 65), "local_a\n", " local_a",
	}
	frontends := []session.Frontend{
		session.FrontendDesktop, session.FrontendHerdr, session.FrontendTerminal, session.FrontendUnknown, "",
	}
	checkers := []DesktopChats{nil, &fakeDesktop{ok: true}, &fakeDesktop{ok: false, reason: "x"}}
	n := 0
	for _, host := range hosts {
		for _, fe := range frontends {
			for _, conflict := range []bool{false, true} {
				for _, ended := range []bool{false, true} {
					for _, pane := range []string{"", "w1:p1"} {
						for _, ck := range checkers {
							r := &fakeRunner{}
							f := &Focuser{Run: r.run, Herdr: &fakeHerdr{}}
							if ck != nil {
								f.Desktop = ck
							}
							s := &session.Session{
								Key: "claude:c1", SessionID: "c1", Kind: "claude", Frontend: fe,
								HostSession: host, HostConflict: conflict, Ended: ended, HerdrPane: pane,
							}
							if err := f.Focus(context.Background(), s); err != nil {
								t.Fatal(err)
							}
							for _, c := range r.calls {
								if c.name != "open" {
									t.Fatalf("ran %q, only open is allowed: %+v", c.name, s)
								}
								for _, a := range c.args {
									low := strings.ToLower(a)
									for _, b := range banned {
										if strings.Contains(low, b) {
											t.Fatalf("call %v has %q (session %+v)", c, b, s)
										}
									}
									if strings.Contains(a, "://") && !continueURL.MatchString(a) {
										t.Fatalf("call %v is not the exact continue route (session %+v)", c, s)
									}
								}
								if len(c.args) == 1 && !continueURL.MatchString(c.args[0]) {
									t.Fatalf("one-argument call %v is not the continue route", c)
								}
								if len(c.args) == 2 && (c.args[0] != "-a" || (c.args[1] != "Claude" && c.args[1] != "Ghostty")) {
									t.Fatalf("call %v is not an app raise", c)
								}
								if len(c.args) != 1 && len(c.args) != 2 {
									t.Fatalf("call %v has odd arguments", c)
								}
							}
							n++
						}
					}
				}
			}
		}
	}
	if n < 1000 {
		t.Fatalf("only %d combinations ran", n)
	}
}

// A URL can only appear for a desktop session that passes every gate.
func TestContinueOnlyForACleanDesktopSession(t *testing.T) {
	ok := &fakeDesktop{ok: true}
	for _, s := range []*session.Session{
		{Frontend: session.FrontendDesktop, HostSession: hostA, HostConflict: true},
		{Frontend: session.FrontendDesktop, HostSession: hostA, Ended: true},
		{Frontend: session.FrontendDesktop},
		{Frontend: session.FrontendTerminal, HostSession: hostA},
		{Frontend: session.FrontendUnknown, HostSession: hostA},
		{Frontend: session.FrontendHerdr, HostSession: hostA, HerdrPane: "p"},
	} {
		r := &fakeRunner{}
		f := &Focuser{Run: r.run, Desktop: ok, Herdr: &fakeHerdr{}}
		f.Focus(context.Background(), s)
		for _, c := range r.calls {
			for _, a := range c.args {
				if strings.Contains(a, "://") {
					t.Fatalf("session %+v got %v", s, c)
				}
			}
		}
	}
}
