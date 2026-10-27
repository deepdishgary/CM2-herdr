package focus

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"cm2-claude/internal/session"
)

type call struct {
	name string
	args []string
}

// fakeRunner records commands and never starts a process.
type fakeRunner struct {
	calls    []call
	err      error
	deadline time.Time
	block    bool
}

func (r *fakeRunner) run(ctx context.Context, name string, args ...string) error {
	r.calls = append(r.calls, call{name, args})
	r.deadline, _ = ctx.Deadline()
	if r.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return r.err
}

type fakeHerdr struct {
	panes []string
	err   error
	order *[]string
}

func (h *fakeHerdr) Focus(ctx context.Context, pane string) error {
	h.panes = append(h.panes, pane)
	if h.order != nil {
		*h.order = append(*h.order, "herdr")
	}
	return h.err
}

func TestHerdrPaneFocusesHerdrThenRaisesGhostty(t *testing.T) {
	var order []string
	r := &fakeRunner{}
	h := &fakeHerdr{order: &order}
	run := func(ctx context.Context, name string, args ...string) error {
		order = append(order, "open")
		return r.run(ctx, name, args...)
	}
	f := &Focuser{Run: run, Herdr: h}
	s := &session.Session{Key: "claude:abc", SessionID: "abc", HerdrPane: "p_7", Frontend: session.FrontendHerdr}
	if err := f.Focus(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.panes, []string{"p_7"}) {
		t.Fatalf("herdr panes %v", h.panes)
	}
	want := []call{{"open", []string{"-a", "Ghostty"}}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls %v", r.calls)
	}
	if !reflect.DeepEqual(order, []string{"herdr", "open"}) {
		t.Fatalf("order %v", order)
	}
}

func TestNonClaudeHerdrAgentUsesHerdrFocus(t *testing.T) {
	r := &fakeRunner{}
	h := &fakeHerdr{}
	f := &Focuser{Run: r.run, Herdr: h}
	s := &session.Session{Key: "herdr:p_9", HerdrPane: "p_9", Kind: "codex", Frontend: session.FrontendHerdr}
	if err := f.Focus(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.panes, []string{"p_9"}) || len(r.calls) != 1 {
		t.Fatalf("panes %v calls %v", h.panes, r.calls)
	}
}

func TestHerdrFailureFallsBackToOpenGhostty(t *testing.T) {
	r := &fakeRunner{}
	h := &fakeHerdr{err: errors.New("socket gone")}
	f := &Focuser{Run: r.run, Herdr: h}
	s := &session.Session{HerdrPane: "p_1", Frontend: session.FrontendHerdr}
	if err := f.Focus(context.Background(), s); err != nil {
		t.Fatalf("fallback should succeed: %v", err)
	}
	want := []call{{"open", []string{"-a", "Ghostty"}}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls %v", r.calls)
	}
}

func TestNilHerdrFallsBackToOpenGhostty(t *testing.T) {
	r := &fakeRunner{}
	f := &Focuser{Run: r.run}
	s := &session.Session{HerdrPane: "p_1", Frontend: session.FrontendHerdr}
	if err := f.Focus(context.Background(), s); err != nil || len(r.calls) != 1 {
		t.Fatalf("err %v calls %v", err, r.calls)
	}
}

func TestHerdrAndFallbackBothFail(t *testing.T) {
	r := &fakeRunner{err: errors.New("open failed")}
	h := &fakeHerdr{err: errors.New("socket gone")}
	f := &Focuser{Run: r.run, Herdr: h}
	s := &session.Session{HerdrPane: "p_1", Frontend: session.FrontendHerdr}
	if err := f.Focus(context.Background(), s); err == nil || !strings.Contains(err.Error(), "open failed") {
		t.Fatalf("err %v", err)
	}
}

// A desktop session only raises the Claude app. The resume deep link is
// banned: on 2026-10-23 it FORKED a live session (the desktop app spawned a
// second claude process on the same session id, which then ran git pushes)
// instead of focusing it. See docs/design.md decision 4.
func TestDesktopOnlyRaisesTheClaudeApp(t *testing.T) {
	r := &fakeRunner{}
	h := &fakeHerdr{}
	f := &Focuser{Run: r.run, Herdr: h}
	s := &session.Session{Key: "claude:1b2c", SessionID: "1b2c", Frontend: session.FrontendDesktop}
	if err := f.Focus(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	want := []call{{"open", []string{"-a", "Claude"}}}
	if !reflect.DeepEqual(r.calls, want) || len(h.panes) != 0 {
		t.Fatalf("calls %v herdr %v", r.calls, h.panes)
	}
}

func TestNoCallEverUsesADeepLink(t *testing.T) {
	r := &fakeRunner{}
	f := &Focuser{Run: r.run, Herdr: &fakeHerdr{}}
	for _, s := range []*session.Session{
		{SessionID: "x", Frontend: session.FrontendDesktop},
		{SessionID: "x", Frontend: session.FrontendTerminal},
		{Key: "herdr:w1:p1", HerdrPane: "w1:p1", Frontend: session.FrontendHerdr},
	} {
		if err := f.Focus(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range r.calls {
		for _, a := range c.args {
			if strings.Contains(a, "://") {
				t.Fatalf("deep link in call %v", c)
			}
		}
	}
}

func TestDesktopWithoutSessionIDStillRaisesTheApp(t *testing.T) {
	r := &fakeRunner{}
	f := &Focuser{Run: r.run}
	if err := f.Focus(context.Background(), &session.Session{Frontend: session.FrontendDesktop}); err != nil {
		t.Fatal(err)
	}
	want := []call{{"open", []string{"-a", "Claude"}}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("ran %v", r.calls)
	}
}

func TestTerminalAndUnknownOpenGhostty(t *testing.T) {
	for _, fe := range []session.Frontend{session.FrontendTerminal, session.FrontendUnknown, session.FrontendHerdr} {
		r := &fakeRunner{}
		h := &fakeHerdr{}
		f := &Focuser{Run: r.run, Herdr: h}
		s := &session.Session{SessionID: "x", Frontend: fe} // no pane
		if err := f.Focus(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		want := []call{{"open", []string{"-a", "Ghostty"}}}
		if !reflect.DeepEqual(r.calls, want) || len(h.panes) != 0 {
			t.Fatalf("%s: calls %v herdr %v", fe, r.calls, h.panes)
		}
	}
}

func TestTerminalAppOverride(t *testing.T) {
	r := &fakeRunner{}
	f := &Focuser{Run: r.run, TerminalApp: "iTerm"}
	if err := f.Focus(context.Background(), &session.Session{Frontend: session.FrontendTerminal}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, []call{{"open", []string{"-a", "iTerm"}}}) {
		t.Fatalf("calls %v", r.calls)
	}
}

func TestRunnerErrorIsReturned(t *testing.T) {
	r := &fakeRunner{err: errors.New("boom")}
	f := &Focuser{Run: r.run}
	if err := f.Focus(context.Background(), &session.Session{Frontend: session.FrontendTerminal}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNilSessionAndNilRunner(t *testing.T) {
	f := &Focuser{Run: (&fakeRunner{}).run}
	if err := f.Focus(context.Background(), nil); err == nil {
		t.Fatal("nil session: expected an error")
	}
	if err := (&Focuser{}).Focus(context.Background(), &session.Session{}); err == nil {
		t.Fatal("nil runner: expected an error")
	}
}

func TestDefaultTimeoutIsThreeSeconds(t *testing.T) {
	r := &fakeRunner{}
	f := &Focuser{Run: r.run}
	start := time.Now()
	if err := f.Focus(context.Background(), &session.Session{Frontend: session.FrontendTerminal}); err != nil {
		t.Fatal(err)
	}
	left := r.deadline.Sub(start)
	if left < 2*time.Second || left > 3*time.Second+100*time.Millisecond {
		t.Fatalf("deadline %v away, want about 3s", left)
	}
}

func TestTimeoutStopsAHungCommand(t *testing.T) {
	r := &fakeRunner{block: true}
	f := &Focuser{Run: r.run, timeout: 50 * time.Millisecond}
	err := f.Focus(context.Background(), &session.Session{Frontend: session.FrontendTerminal})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}

func TestTimeoutAlsoCoversHerdr(t *testing.T) {
	// A Herdr call that hangs must not hold the key press forever.
	r := &fakeRunner{}
	h := &hangingHerdr{}
	f := &Focuser{Run: r.run, Herdr: h, timeout: 50 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		done <- f.Focus(context.Background(), &session.Session{HerdrPane: "p", Frontend: session.FrontendHerdr})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Focus did not return")
	}
	if !h.sawDeadline {
		t.Fatal("herdr call had no deadline")
	}
}

type hangingHerdr struct{ sawDeadline bool }

func (h *hangingHerdr) Focus(ctx context.Context, pane string) error {
	_, h.sawDeadline = ctx.Deadline()
	<-ctx.Done()
	return ctx.Err()
}

func TestExecRunner(t *testing.T) {
	ctx := context.Background()
	if err := ExecRunner(ctx, "true"); err != nil {
		t.Fatalf("true: %v", err)
	}
	if err := ExecRunner(ctx, "false"); err == nil {
		t.Fatal("false: expected an error")
	}
	if err := ExecRunner(ctx, "cm2-no-such-command"); err == nil {
		t.Fatal("missing command: expected an error")
	}
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := ExecRunner(cctx, "sleep", "5"); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("sleep: err %v after %v", err, time.Since(start))
	}
}
