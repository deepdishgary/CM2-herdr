package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cm2-claude/internal/herdr"
	"cm2-claude/internal/herdr/herdrtest"
)

func runCLI(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code = runHerdr(ctx, args, &o, &e)
	return code, o.String(), e.String()
}

func fakeWithAgents(t *testing.T) *herdrtest.Server {
	srv := herdrtest.New(t)
	a := herdrtest.NewAgent("w1:p1", "claude", "working")
	a["agent_session"] = map[string]any{"kind": "id", "value": "0123456789abcdef"}
	a["cwd"] = "/Users/me/proj"
	b := herdrtest.NewAgent("w1:p2", "codex", "blocked")
	b["cwd"] = "/Users/me/other"
	srv.SetAgents(a, b)
	srv.SetFocused("w1:p2")
	return srv
}

func TestHerdrListTable(t *testing.T) {
	srv := fakeWithAgents(t)
	code, out, errOut := runCLI(t, "list", "--socket", srv.Path())
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("output:\n%s", out)
	}
	for _, want := range []string{"PANE", "KIND", "STATUS", "FOCUSED", "SESSION", "CWD"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header missing %s: %q", want, lines[0])
		}
	}
	if f := strings.Fields(lines[1]); len(f) != 6 || f[0] != "w1:p1" || f[1] != "claude" || f[2] != "working" ||
		f[3] != "-" || f[4] != "01234567" || f[5] != "proj" {
		t.Errorf("row 1: %q", lines[1])
	}
	if f := strings.Fields(lines[2]); len(f) != 6 || f[0] != "w1:p2" || f[3] != "yes" || f[4] != "-" || f[5] != "other" {
		t.Errorf("row 2: %q", lines[2])
	}
}

func TestHerdrListJSON(t *testing.T) {
	srv := fakeWithAgents(t)
	code, out, _ := runCLI(t, "list", "--json", "--socket", srv.Path())
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var snap herdr.Snapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if !snap.Available || len(snap.Agents) != 2 || snap.FocusedPaneID != "w1:p2" || snap.Agents[0].Session.Value != "0123456789abcdef" {
		t.Fatalf("snapshot: %+v", snap)
	}
}

func TestHerdrNotRunning(t *testing.T) {
	sock := filepath.Join(herdrtest.ShortDir(t), "none.sock")
	for _, args := range [][]string{{"list"}, {"list", "--json"}, {"focus", "w1:p1"}} {
		code, out, errOut := runCLI(t, append(args, "--socket", sock)...)
		if code != 1 || out != "" || !strings.Contains(errOut, "not running") {
			t.Errorf("%v: code %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

func TestHerdrFocusUsesFakeOnly(t *testing.T) {
	srv := fakeWithAgents(t)
	code, out, errOut := runCLI(t, "focus", "w1:p1", "--socket", srv.Path())
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "w1:p1") {
		t.Errorf("stdout %q", out)
	}
	if got := srv.State().FocusCalls; len(got) != 1 || got[0] != "w1:p1" {
		t.Fatalf("focus calls = %v", got)
	}
	code, _, errOut = runCLI(t, "focus", "w9:p9", "--socket", srv.Path())
	if code != 1 || !strings.Contains(errOut, "agent_not_found") {
		t.Errorf("missing pane: code %d, stderr %q", code, errOut)
	}
}

func TestHerdrUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"bogus"}, {"focus"}, {"focus", "a", "b"}, {"list", "extra"}, {"list", "--nope"}, {"list", "--session"}} {
		code, _, errOut := runCLI(t, args...)
		if code != 2 || errOut == "" {
			t.Errorf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// tuneWatch makes the watcher fast to start and keeps its poll off, so a change
// can only reach the output through an event.
func tuneWatch(t *testing.T) {
	t.Helper()
	watchTune = func(w *herdr.Watcher) {
		w.PollInterval = time.Hour
		w.Debounce = 20 * time.Millisecond
		w.BackoffMin = 5 * time.Millisecond
		w.BackoffMax = 40 * time.Millisecond
	}
	t.Cleanup(func() { watchTune = nil })
}

func TestHerdrWatchPrintsChangesAndStopsOnCancel(t *testing.T) {
	tuneWatch(t)
	srv := fakeWithAgents(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out lockedBuf
	done := make(chan int, 1)
	go func() { done <- runHerdr(ctx, []string{"watch", "--socket", srv.Path()}, &out, &out) }()

	srv.Await(t, "watch subscribed", func(s herdrtest.State) bool { return s.Subs == 1 })
	srv.Await(t, "reconcile read built", func(s herdrtest.State) bool { return s.Served["session.snapshot"] >= 2 })
	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "done" })
	srv.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "done"})
	deadline := time.After(10 * time.Second)
	for !strings.Contains(out.String(), "was working") {
		select {
		case <-deadline:
			t.Fatalf("no change line; output:\n%s", out.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop")
	}
	if !strings.Contains(out.String(), "w1:p1") || !strings.Contains(out.String(), "2 agents") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestChangeLines(t *testing.T) {
	mk := func(pane, status string, focused bool) herdr.Agent {
		return herdr.Agent{PaneID: pane, Kind: "claude", Status: herdr.Status(status), Focused: focused, CWD: "/x/" + pane}
	}
	up := herdr.Snapshot{Available: true, Version: "0.9.3", FocusedPaneID: "p1",
		Agents: []herdr.Agent{mk("p1", "idle", true), mk("p2", "idle", false)}}

	first := changeLines(nil, up)
	if len(first) != 3 || !strings.Contains(first[0], "2 agents") || !strings.HasPrefix(first[1], "+ p1") || !strings.HasPrefix(first[2], "+ p2") {
		t.Fatalf("first: %q", first)
	}
	if got := changeLines(nil, herdr.Snapshot{}); len(got) != 1 || !strings.Contains(got[0], "not running") {
		t.Fatalf("unavailable first: %q", got)
	}
	if got := changeLines(&herdr.Snapshot{}, up); len(got) != 3 {
		t.Fatalf("back up: %q", got)
	}
	if got := changeLines(&up, herdr.Snapshot{}); len(got) != 1 || !strings.Contains(got[0], "not running") {
		t.Fatalf("went down: %q", got)
	}

	next := herdr.Snapshot{Available: true, FocusedPaneID: "p2",
		Agents: []herdr.Agent{mk("p1", "working", false), mk("p3", "idle", false)}}
	got := strings.Join(changeLines(&up, next), "\n")
	for _, want := range []string{"~ p1", "was idle", "- p2", "+ p3", "focus: p2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if same := changeLines(&up, up); len(same) != 0 {
		t.Fatalf("no change should print nothing: %q", same)
	}
}

// F8: -h and --help print the usage on stdout and exit 0, for the command and
// for each subcommand.
func TestHerdrHelp(t *testing.T) {
	for _, args := range [][]string{
		{"-h"}, {"--help"}, {"list", "-h"}, {"list", "--help"}, {"watch", "-h"}, {"watch", "--help"},
		{"focus", "-h"}, {"focus", "--help"}, {"list", "--json", "-h"},
	} {
		code, out, errOut := runCLI(t, args...)
		if code != 0 || errOut != "" || !strings.Contains(out, "usage: cm2d herdr") {
			t.Errorf("%v: code %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

func TestHerdrUsageText(t *testing.T) {
	_, out, _ := runCLI(t, "--help")
	for _, want := range []string{"keeps waiting while Herdr is down", "--session NAME", "--socket PATH", "HERDR_SESSION"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage lacks %q:\n%s", want, out)
		}
	}
}

// F8: a blank pane id is a usage error, and nothing reaches Herdr.
func TestHerdrFocusBlankIDIsUsageError(t *testing.T) {
	srv := fakeWithAgents(t)
	for _, id := range []string{"", " ", "   ", "\t", " \t \n"} {
		code, out, errOut := runCLI(t, "focus", id, "--socket", srv.Path())
		if code != 2 || out != "" || errOut == "" {
			t.Errorf("focus %q: code %d, stdout %q, stderr %q", id, code, out, errOut)
		}
	}
	if got := srv.State().FocusCalls; len(got) != 0 {
		t.Fatalf("a blank id reached the server: %v", got)
	}
}

// F5: --session picks <XDG_CONFIG_HOME>/herdr/sessions/<name>/herdr.sock. The
// fake listens at its own short path, so a link stands in for the session path.
func TestHerdrSessionFlag(t *testing.T) {
	srv := fakeWithAgents(t)
	cfg := herdrtest.ShortDir(t)
	dir := filepath.Join(cfg, "herdr", "sessions", "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(srv.Path(), filepath.Join(dir, "herdr.sock")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HERDR_SESSION", "")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(cfg, "wrong.sock")) // the flag must win over this

	code, out, errOut := runCLI(t, "list", "--session", "work")
	if code != 0 || !strings.Contains(out, "w1:p1") {
		t.Fatalf("--session work: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, errOut = runCLI(t, "focus", "w1:p1", "--session", "work")
	if code != 0 || len(srv.State().FocusCalls) != 1 {
		t.Fatalf("focus --session: code %d, stdout %q, stderr %q", code, out, errOut)
	}

	// A session that does not exist: not running, and the message names its path.
	code, _, errOut = runCLI(t, "list", "--session", "ghost")
	if code != 1 || !strings.Contains(errOut, filepath.Join(cfg, "herdr", "sessions", "ghost", "herdr.sock")) {
		t.Fatalf("--session ghost: code %d, stderr %q", code, errOut)
	}

	// An explicit --socket wins over --session.
	code, out, errOut = runCLI(t, "list", "--session", "ghost", "--socket", srv.Path())
	if code != 0 || !strings.Contains(out, "w1:p1") {
		t.Fatalf("--socket with --session: code %d, stdout %q, stderr %q", code, out, errOut)
	}

	// HERDR_SESSION from the environment works when no flag is given.
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("HERDR_SESSION", "work")
	code, out, errOut = runCLI(t, "list")
	if code != 0 || !strings.Contains(out, "w1:p1") {
		t.Fatalf("HERDR_SESSION: code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// F8: watch prints "herdr error: ..." on stderr for OnError reports. It is not
// the same line as "herdr is not running; waiting", which goes to stdout.
func TestHerdrWatchPrintsErrorsOnStderr(t *testing.T) {
	tuneWatch(t)
	srv := fakeWithAgents(t)
	srv.RejectNextSubscribe(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut lockedBuf
	done := make(chan int, 1)
	go func() { done <- runHerdr(ctx, []string{"watch", "--socket", srv.Path()}, &out, &errOut) }()

	srv.Await(t, "watch subscribed after the rejection", func(s herdrtest.State) bool { return len(s.SubRequests) == 2 && s.Subs == 1 })
	waitFor(t, "an error line", func() bool { return strings.Contains(errOut.String(), "herdr error: ") })
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(errOut.String(), "herdr error: herdr: pane_not_found") {
		t.Errorf("stderr:\n%s", errOut.String())
	}
	if strings.Contains(out.String(), "herdr error") || strings.Contains(out.String(), "not running") {
		t.Errorf("stdout has error or not-running text:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "2 agents") {
		t.Errorf("stdout:\n%s", out.String())
	}
}

func TestHerdrWatchWaitsWhileHerdrIsDown(t *testing.T) {
	tuneWatch(t)
	sock := filepath.Join(herdrtest.ShortDir(t), "none.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut lockedBuf
	done := make(chan int, 1)
	go func() { done <- runHerdr(ctx, []string{"watch", "--socket", sock}, &out, &errOut) }()
	waitFor(t, "the waiting line", func() bool { return strings.Contains(out.String(), "herdr is not running; waiting") })
	time.Sleep(100 * time.Millisecond) // several retries at 5..40 ms
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if errOut.String() != "" {
		t.Errorf("stderr for a missing Herdr: %q", errOut.String())
	}
	if n := strings.Count(out.String(), "not running"); n != 1 {
		t.Errorf("not-running line printed %d times:\n%s", n, out.String())
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
