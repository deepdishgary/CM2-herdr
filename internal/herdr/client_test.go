package herdr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cm2-claude/internal/herdr/herdrtest"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newClient(t *testing.T) (*Client, *herdrtest.Server) {
	t.Helper()
	srv := herdrtest.New(t)
	c := NewClient(srv.Path())
	c.IOTimeout = 2 * time.Second
	return c, srv
}

func TestPing(t *testing.T) {
	c, srv := newClient(t)
	srv.SetVersion("0.9.5", 23)
	v, p, err := c.Ping(ctxT(t))
	if err != nil || v != "0.9.5" || p != 23 {
		t.Fatalf("Ping = %q %d %v", v, p, err)
	}
}

func TestSnapshotDecodesAgentsInServerOrder(t *testing.T) {
	c, srv := newClient(t)
	a := herdrtest.NewAgent("w2:p1", "claude", "working")
	a["agent_session"] = map[string]any{"source": "hook", "agent": "claude", "kind": "id", "value": "sess-123"}
	a["completion_seq"] = 7
	a["state_change_seq"] = 9
	a["revision"] = 11
	a["name"] = "main"
	a["display_agent"] = "Claude Code"
	a["foreground_cwd"] = "/fg"
	b := herdrtest.NewAgent("w1:p1", "codex", "blocked")
	srv.SetAgents(a, b)
	srv.SetFocused("w1:p1")

	snap, err := c.Snapshot(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Available || snap.Version != "0.9.3" || snap.Protocol != 22 || snap.FocusedPaneID != "w1:p1" {
		t.Fatalf("snapshot header: %+v", snap)
	}
	if len(snap.Agents) != 2 || snap.Agents[0].PaneID != "w2:p1" || snap.Agents[1].PaneID != "w1:p1" {
		t.Fatalf("order not kept: %+v", snap.Agents)
	}
	g := snap.Agents[0]
	if g.Kind != "claude" || g.Status != StatusWorking || g.Name != "main" || g.DisplayName != "Claude Code" ||
		g.CWD != "/fg" || g.WorkspaceID != "w2" || g.TabID != "w2:t1" || g.TerminalID != "term-w2:p1" ||
		g.Revision != 11 || g.StateChangeSeq != 9 || g.Focused {
		t.Fatalf("agent 0: %+v", g)
	}
	if g.CompletionSeq == nil || *g.CompletionSeq != 7 {
		t.Fatalf("completion_seq: %v", g.CompletionSeq)
	}
	if g.Session == nil || g.Session.Value != "sess-123" || g.Session.Kind != "id" || g.Session.Source != "hook" {
		t.Fatalf("session: %+v", g.Session)
	}
	if g.ClaudeSessionID() != "sess-123" {
		t.Fatalf("ClaudeSessionID = %q", g.ClaudeSessionID())
	}
	if !snap.Agents[1].Focused {
		t.Fatal("agent 1 should be focused")
	}
}

func TestDecodeIsLenient(t *testing.T) {
	c, srv := newClient(t)
	odd := herdrtest.NewAgent("w1:p2", "", "melting") // unknown status, no kind
	odd["title"] = nil
	odd["cwd"] = nil
	odd["agent_session"] = nil
	odd["completion_seq"] = nil
	odd["brand_new_field"] = []any{1, 2, 3}
	del := herdrtest.NewAgent("w1:p3", "claude", "idle")
	delete(del, "revision")
	delete(del, "agent_status")
	badTypes := herdrtest.NewAgent("w1:p4", "claude", "idle")
	badTypes["focused"] = "yes"       // wrong type: field falls back to zero
	badTypes["completion_seq"] = "no" // wrong type: nil
	noPane := herdrtest.Agent{"agent": "claude", "agent_status": "idle"}
	paneNotString := herdrtest.NewAgent("x", "claude", "idle")
	paneNotString["pane_id"] = 5
	srv.SetAgentsRaw(herdrtest.NewAgent("w1:p1", "claude", "done"), odd, "junk", 42, nil, noPane, paneNotString, del, badTypes)

	snap, err := c.Snapshot(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range snap.Agents {
		ids = append(ids, a.PaneID)
	}
	if got := strings.Join(ids, ","); got != "w1:p1,w1:p2,w1:p3,w1:p4" {
		t.Fatalf("kept records = %s", got)
	}
	a := snap.Agents[1]
	if a.Status != StatusUnknown || a.Kind != "" || a.CWD != "" || a.Session != nil || a.CompletionSeq != nil || a.DisplayName != "" {
		t.Fatalf("odd record: %+v", a)
	}
	if snap.Agents[2].Status != StatusUnknown {
		t.Fatalf("missing status should be unknown: %+v", snap.Agents[2])
	}
	if b := snap.Agents[3]; b.Focused || b.CompletionSeq != nil || b.Kind != "claude" {
		t.Fatalf("bad types: %+v", b)
	}
}

func TestDisplayNameAndCWDFallbacks(t *testing.T) {
	c, srv := newClient(t)
	a := herdrtest.NewAgent("w1:p1", "codex", "idle")
	a["title"] = "My Title"
	b := herdrtest.NewAgent("w1:p2", "codex", "idle")
	srv.SetAgents(a, b)
	snap, err := c.Snapshot(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Agents[0].DisplayName != "My Title" || snap.Agents[1].DisplayName != "codex" {
		t.Fatalf("display names: %q %q", snap.Agents[0].DisplayName, snap.Agents[1].DisplayName)
	}
	if snap.Agents[0].CWD != "/work/w1:p1" {
		t.Fatalf("cwd = %q", snap.Agents[0].CWD)
	}
}

func TestClaudeSessionID(t *testing.T) {
	id := &AgentSession{Kind: "id", Value: "abc"}
	path := &AgentSession{Kind: "path", Value: "/x/y.jsonl"}
	cases := []struct {
		a    Agent
		want string
	}{
		{Agent{Kind: "claude", Session: id}, "abc"},
		{Agent{Kind: "claude", Session: path}, ""},
		{Agent{Kind: "codex", Session: id}, ""},
		{Agent{Kind: "claude"}, ""},
	}
	for i, tc := range cases {
		if got := tc.a.ClaudeSessionID(); got != tc.want {
			t.Errorf("case %d: %q, want %q", i, got, tc.want)
		}
	}
}

func TestAPIErrorMapping(t *testing.T) {
	c, srv := newClient(t)
	srv.FailMethod("session.snapshot", "internal_error", "boom")
	_, err := c.Snapshot(ctxT(t))
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "internal_error" || ae.Message != "boom" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ae.Error(), "internal_error") || !strings.Contains(ae.Error(), "boom") {
		t.Fatalf("Error() = %q", ae.Error())
	}
	srv.FailMethod("ping", "nope", "no ping")
	if _, _, err := c.Ping(ctxT(t)); !errors.As(err, &ae) || ae.Code != "nope" {
		t.Fatalf("ping err = %v", err)
	}
}

func TestFocus(t *testing.T) {
	c, srv := newClient(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "done"), herdrtest.NewAgent("w1:p2", "codex", "idle"))
	if err := c.Focus(ctxT(t), "w1:p2"); err != nil {
		t.Fatal(err)
	}
	if got := srv.State().FocusCalls; len(got) != 1 || got[0] != "w1:p2" {
		t.Fatalf("focus calls = %v", got)
	}
	snap, _ := c.Snapshot(ctxT(t))
	if snap.FocusedPaneID != "w1:p2" {
		t.Fatalf("focused = %q", snap.FocusedPaneID)
	}
	var ae *APIError
	if err := c.Focus(ctxT(t), "w9:p9"); !errors.As(err, &ae) || ae.Code != "agent_not_found" {
		t.Fatalf("missing pane err = %v", err)
	}
	before := len(srv.State().FocusCalls)
	for _, blank := range []string{"", " ", " \t\n"} {
		if err := c.Focus(ctxT(t), blank); err == nil {
			t.Fatalf("blank target %q should fail before any request", blank)
		}
	}
	if got := len(srv.State().FocusCalls); got != before {
		t.Fatalf("a blank target reached the server (%d calls, was %d)", got, before)
	}
}

func TestResolveSocketPath(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("HERDR_SESSION", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	t.Setenv("HOME", home)
	if got, want := ResolveSocketPath(""), filepath.Join(home, ".config", "herdr", "herdr.sock"); got != want {
		t.Errorf("default = %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := ResolveSocketPath(""); got != "/xdg/herdr/herdr.sock" {
		t.Errorf("xdg = %q", got)
	}
	t.Setenv("HERDR_SOCKET_PATH", "/env/h.sock")
	if got := ResolveSocketPath(""); got != "/env/h.sock" {
		t.Errorf("env = %q", got)
	}
	if got := ResolveSocketPath("/explicit.sock"); got != "/explicit.sock" {
		t.Errorf("explicit = %q", got)
	}
	if got := NewClient("").SocketPath; got != "/env/h.sock" {
		t.Errorf("NewClient(\"\") path = %q", got)
	}
}

func TestUnavailableMissingSocket(t *testing.T) {
	c := NewClient(filepath.Join(herdrtest.ShortDir(t), "none.sock"))
	checkUnavailable(t, c)
}

func TestUnavailableRefused(t *testing.T) {
	path := filepath.Join(herdrtest.ShortDir(t), "stale.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false) // leave a stale socket file
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket file missing: %v", err)
	}
	checkUnavailable(t, NewClient(path))
}

func checkUnavailable(t *testing.T, c *Client) {
	t.Helper()
	snap, err := c.Snapshot(ctxT(t))
	if err != nil || snap.Available || len(snap.Agents) != 0 {
		t.Fatalf("Snapshot = %+v, %v; want Available=false and nil error", snap, err)
	}
	if _, _, err := c.Ping(ctxT(t)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Ping err = %v, want ErrNotRunning", err)
	}
	if err := c.Focus(ctxT(t), "w1:p1"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Focus err = %v, want ErrNotRunning", err)
	}
}

func TestSocketPathTooLong(t *testing.T) {
	c := NewClient("/tmp/" + strings.Repeat("x", 120) + "/herdr.sock")
	_, _, err := c.Ping(ctxT(t))
	if err == nil || !strings.Contains(err.Error(), "too long") || errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v", err)
	}
}

func TestEmptySocketPath(t *testing.T) {
	c := &Client{}
	if _, _, err := c.Ping(ctxT(t)); err == nil {
		t.Fatal("want error")
	}
}

func TestIOTimeout(t *testing.T) {
	c, srv := newClient(t)
	c.IOTimeout = 100 * time.Millisecond
	srv.Stall(true)
	start := time.Now()
	_, _, err := c.Ping(ctxT(t))
	if err == nil {
		t.Fatal("want timeout error")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("took too long")
	}
}

func TestContextCancelStopsCall(t *testing.T) {
	c, srv := newClient(t)
	c.IOTimeout = time.Minute
	srv.Stall(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := c.Ping(ctx); done <- err }()
	srv.Await(t, "request arrives", func(s herdrtest.State) bool { return s.Calls["ping"] == 1 })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not stop on cancel")
	}
}

func TestMalformedAndOversizedReplies(t *testing.T) {
	c, srv := newClient(t)
	srv.SetRaw("ping", "this is not json\n")
	if _, _, err := c.Ping(ctxT(t)); err == nil {
		t.Fatal("malformed reply should be an error")
	}
	srv.SetRaw("ping", `{"id":"x","result":{"type":"pong"}`) // no newline, then close
	if _, _, err := c.Ping(ctxT(t)); err == nil {
		t.Fatal("truncated reply should be an error")
	}
	c.MaxLine = 1024
	srv.SetRaw("session.snapshot", strings.Repeat("a", 5000)+"\n")
	if _, err := c.Snapshot(ctxT(t)); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("oversized reply err = %v", err)
	}
	srv.SetRaw("session.snapshot", `{"id":"x","result":{"type":"session_snapshot"}}`+"\n")
	if _, err := c.Snapshot(ctxT(t)); err == nil {
		t.Fatal("reply without a snapshot should be an error")
	}
}

type countConn struct {
	net.Conn
	closed *atomic.Int64
	once   atomic.Bool
}

func (c *countConn) Close() error {
	if c.once.CompareAndSwap(false, true) {
		c.closed.Add(1)
	}
	return c.Conn.Close()
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("no /dev/fd")
	}
	return len(ents)
}

func TestOneConnectionPerCallAndNoFDLeak(t *testing.T) {
	c, srv := newClient(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	var opened, closed atomic.Int64
	c.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		opened.Add(1)
		return &countConn{Conn: conn, closed: &closed}, nil
	}
	ctx := ctxT(t)
	run := func(n int) {
		for i := 0; i < n; i++ {
			c.Ping(ctx)
			c.Snapshot(ctx)
			c.Focus(ctx, "w1:p1")
			c.Focus(ctx, "nope") // error path
		}
	}
	run(5) // warm up runtime pollers
	before := openFDs(t)
	opened.Store(0)
	closed.Store(0)
	startAccepted := srv.State().Accepted
	run(150)
	// error and timeout paths too
	srv.FailMethod("ping", "x", "y")
	for i := 0; i < 50; i++ {
		c.Ping(ctx)
	}
	srv.SetRaw("ping", "garbage\n")
	for i := 0; i < 50; i++ {
		c.Ping(ctx)
	}
	srv.Stall(true)
	c.IOTimeout = 5 * time.Millisecond
	for i := 0; i < 20; i++ {
		c.Ping(ctx)
	}
	srv.Stall(false)

	if opened.Load() != 150*4+120 {
		t.Fatalf("opened %d connections, want %d (one per call)", opened.Load(), 150*4+120)
	}
	if opened.Load() != closed.Load() {
		t.Fatalf("opened %d, closed %d", opened.Load(), closed.Load())
	}
	if got := srv.State().Accepted - startAccepted; got != int(opened.Load()) {
		t.Fatalf("server accepted %d, client opened %d", got, opened.Load())
	}
	// Server-side handlers close their sockets a moment after the client does.
	deadline := time.Now().Add(5 * time.Second)
	for openFDs(t) > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := openFDs(t); after > before+2 {
		t.Fatalf("fd leak: %d open before, %d after", before, after)
	}
}

// F5: the whole precedence table. Higher rows win over lower rows.
func TestResolveSocketPathPrecedence(t *testing.T) {
	tests := []struct {
		name                       string
		explicit, session          string
		envSocket, envSession, xdg string
		want                       string
	}{
		{"explicit beats everything", "/e.sock", "arg", "/env.sock", "envs", "/x", "/e.sock"},
		{"session arg beats env", "", "arg", "/env.sock", "envs", "/x", "/x/herdr/sessions/arg/herdr.sock"},
		{"session arg without xdg", "", "arg", "/env.sock", "envs", "", "/home/u/.config/herdr/sessions/arg/herdr.sock"},
		{"HERDR_SOCKET_PATH beats HERDR_SESSION", "", "", "/env.sock", "envs", "/x", "/env.sock"},
		{"HERDR_SESSION with xdg", "", "", "", "work", "/x", "/x/herdr/sessions/work/herdr.sock"},
		{"HERDR_SESSION with home", "", "", "", "work", "", "/home/u/.config/herdr/sessions/work/herdr.sock"},
		{"xdg", "", "", "", "", "/x", "/x/herdr/herdr.sock"},
		{"home", "", "", "", "", "", "/home/u/.config/herdr/herdr.sock"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", "/home/u")
			t.Setenv("HERDR_SOCKET_PATH", tc.envSocket)
			t.Setenv("HERDR_SESSION", tc.envSession)
			t.Setenv("XDG_CONFIG_HOME", tc.xdg)
			if got := ResolveSocketPathSession(tc.explicit, tc.session); got != tc.want {
				t.Errorf("ResolveSocketPathSession(%q, %q) = %q, want %q", tc.explicit, tc.session, got, tc.want)
			}
			if got := NewClientSession(tc.explicit, tc.session).SocketPath; got != tc.want {
				t.Errorf("NewClientSession(%q, %q).SocketPath = %q, want %q", tc.explicit, tc.session, got, tc.want)
			}
			if tc.session == "" {
				if got := ResolveSocketPath(tc.explicit); got != tc.want {
					t.Errorf("ResolveSocketPath(%q) = %q, want %q", tc.explicit, got, tc.want)
				}
			}
		})
	}
}

// F6: the limit depends on the OS.
func TestSocketPathLimitPerOS(t *testing.T) {
	want := map[string]int{
		"darwin": 104, "freebsd": 104, "openbsd": 104, "netbsd": 104, "dragonfly": 104,
		"linux":   108,
		"windows": 0, "plan9": 0, "solaris": 0, "js": 0,
	}
	for goos, w := range want {
		if got := socketPathLimit(goos); got != w {
			t.Errorf("socketPathLimit(%q) = %d, want %d", goos, got, w)
		}
	}
}

// F6: the client applies the limit of the OS it runs on. A path one byte short
// of the limit gets as far as the dial. A path at the limit does not.
func TestSocketPathLimitAppliesAtTheBoundary(t *testing.T) {
	limit := socketPathLimit(runtime.GOOS)
	if limit == 0 {
		t.Skipf("no pre-check on %s", runtime.GOOS)
	}
	pad := func(n int) string {
		p := "/nonexistent-herdr-test/"
		return p + strings.Repeat("x", n-len(p)-len("/s.sock")) + "/s.sock"
	}
	okPath, badPath := pad(limit-1), pad(limit)
	if len(okPath) != limit-1 || len(badPath) != limit {
		t.Fatalf("bad test paths: %d, %d", len(okPath), len(badPath))
	}
	if _, _, err := NewClient(okPath).Ping(ctxT(t)); !errors.Is(err, ErrNotRunning) {
		t.Errorf("path of %d bytes: err = %v, want ErrNotRunning (it reached the dial)", len(okPath), err)
	}
	_, _, err := NewClient(badPath).Ping(ctxT(t))
	if err == nil || errors.Is(err, ErrNotRunning) || !strings.Contains(err.Error(), "too long") {
		t.Errorf("path of %d bytes: err = %v, want a too-long error", len(badPath), err)
	}
}

// T6: a reply line far past the 64 KB read buffer decodes in full.
func TestLargeReplyLineDecodes(t *testing.T) {
	c, srv := newClient(t)
	title := strings.Repeat("t", 4096)
	var agents []herdrtest.Agent
	for i := 0; i < 40; i++ {
		a := herdrtest.NewAgent("w1:p"+strings.Repeat("1", i+1), "claude", "idle")
		a["title"] = title
		agents = append(agents, a)
	}
	srv.SetAgents(agents...)
	snap, err := c.Snapshot(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Agents) != 40 {
		t.Fatalf("decoded %d agents, want 40", len(snap.Agents))
	}
	for i, a := range snap.Agents {
		if a.DisplayName != title {
			t.Fatalf("agent %d: display name has %d bytes, want 4096", i, len(a.DisplayName))
		}
	}
}

// T6: a line longer than MaxLine is an error, whether it passes the limit by a
// few bytes or by several read chunks. The limit itself is allowed.
func TestReplyLineLimit(t *testing.T) {
	c, srv := newClient(t)
	c.MaxLine = 100 << 10
	line := func(n int) string { // a JSON line of exactly n bytes, newline not counted
		const pre, post = `{"id":"x","result":{"pad":"`, `"}}`
		return pre + strings.Repeat("a", n-len(pre)-len(post)) + post + "\n"
	}
	// The pad is not a snapshot, so a line that fits is rejected for another reason.
	srv.SetRaw("session.snapshot", line(c.MaxLine))
	if _, err := c.Snapshot(ctxT(t)); err == nil || strings.Contains(err.Error(), "too long") {
		t.Fatalf("line at the limit: err = %v, want a non-length error", err)
	}
	for _, n := range []int{c.MaxLine + 1, c.MaxLine + 10, c.MaxLine + 64<<10, c.MaxLine + 300<<10} {
		srv.SetRaw("session.snapshot", line(n))
		if _, err := c.Snapshot(ctxT(t)); err == nil || !strings.Contains(err.Error(), "too long") {
			t.Errorf("line of %d bytes (limit %d): err = %v, want a too-long error", n, c.MaxLine, err)
		}
	}
}

// F9: the fake never lets an event reach a new subscription before its
// subscription_started. A pusher sends events all the time; if one beat the ack,
// subscribe would see an event line as its first reply and fail.
func TestFakeAcksSubscriptionBeforeAnyEvent(t *testing.T) {
	c, srv := newClient(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	stop := make(chan struct{})
	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		for {
			select {
			case <-stop:
				return
			default:
			}
			srv.Push("pane.created", map[string]any{"pane_id": "w1:p2"})
			time.Sleep(20 * time.Microsecond)
		}
	}()
	defer func() { close(stop); <-pushed }()
	ctx := ctxT(t)
	for i := 0; i < 300; i++ {
		sub, err := c.subscribe(ctx, []string{"w1:p1"})
		if err != nil {
			t.Fatalf("subscribe %d: %v (an event came before the ack?)", i, err)
		}
		sub.close()
	}
}
