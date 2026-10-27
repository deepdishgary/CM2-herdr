package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cm2-claude/internal/session"
)

// rig is a server behind httptest with a buffered event channel.
type rig struct {
	t   *testing.T
	s   *Server
	ts  *httptest.Server
	evs chan session.HookEvent
}

func newRig(t *testing.T, mutate func(*Server)) *rig {
	t.Helper()
	evs := make(chan session.HookEvent, 32)
	s := &Server{Events: evs, MaxHold: 5 * time.Second}
	if mutate != nil {
		mutate(s)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &rig{t: t, s: s, ts: ts, evs: evs}
}

func (r *rig) next() session.HookEvent {
	r.t.Helper()
	select {
	case ev := <-r.evs:
		return ev
	case <-time.After(3 * time.Second):
		r.t.Fatal("no event within 3s")
		return session.HookEvent{}
	}
}

func (r *rig) noEvent() {
	r.t.Helper()
	select {
	case ev := <-r.evs:
		r.t.Fatalf("unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func (r *rig) post(path, body string, hdr map[string]string) (*http.Response, string) {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, r.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// permAsync posts a permission request and returns a channel with the reply.
type reply struct {
	code int
	body string
	err  error
}

func (r *rig) permAsync(ctx context.Context, body string) <-chan reply {
	return r.permAsyncHdr(ctx, body, nil)
}

func (r *rig) permAsyncHdr(ctx context.Context, body string, hdr map[string]string) <-chan reply {
	ch := make(chan reply, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.ts.URL+"/cm2d/v1/permission", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- reply{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		ch <- reply{code: resp.StatusCode, body: string(b)}
	}()
	return ch
}

func (r *rig) waitReply(ch <-chan reply) reply {
	r.t.Helper()
	select {
	case rp := <-ch:
		return rp
	case <-time.After(3 * time.Second):
		r.t.Fatal("no reply within 3s")
		return reply{}
	}
}

func (r *rig) noReply(ch <-chan reply) {
	r.t.Helper()
	select {
	case rp := <-ch:
		r.t.Fatalf("reply came too early: %+v", rp)
	case <-time.After(50 * time.Millisecond):
	}
}

const permBody = `{"session_id":"s1","cwd":"/work/proj","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`

const (
	wantAllow = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
	wantDeny  = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"Denied from the macropad"}}}`
)

func TestSessionEndpoint(t *testing.T) {
	r := newRig(t, nil)
	body := `{"payload":{"session_id":"s1","cwd":"/work/p","hook_event_name":"SessionStart","source":"resume"},"attended":"1","entrypoint":"cli","herdr_pane":"p_7"}`
	resp, out := r.post("/cm2d/v1/session", body, nil)
	if resp.StatusCode != 200 || out != "" {
		t.Fatalf("got %d %q", resp.StatusCode, out)
	}
	ev := r.next()
	want := session.HookEvent{Name: "SessionStart", SessionID: "s1", CWD: "/work/p", Attended: "1",
		Entrypoint: "cli", HerdrPane: "p_7", Source: "resume"}
	ev.At = time.Time{}
	if ev != want {
		t.Fatalf("event = %+v, want %+v", ev, want)
	}
}

func TestSessionEndpointBad(t *testing.T) {
	r := newRig(t, nil)
	for name, body := range map[string]string{
		"not json":        `{`,
		"no payload":      `{"attended":"1"}`,
		"payload not obj": `{"payload":"x"}`,
		"no session id":   `{"payload":{"cwd":"/x"}}`,
	} {
		resp, _ := r.post("/cm2d/v1/session", body, nil)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	r.noEvent()
}

func TestEventEndpoint(t *testing.T) {
	r := newRig(t, nil)
	cases := []struct {
		body string
		want session.HookEvent
	}{
		{`{"session_id":"s1","cwd":"/a","hook_event_name":"UserPromptSubmit","prompt":"hi"}`,
			session.HookEvent{Name: "UserPromptSubmit", SessionID: "s1", CWD: "/a"}},
		{`{"session_id":"s1","cwd":"/a","hook_event_name":"Stop"}`,
			session.HookEvent{Name: "Stop", SessionID: "s1", CWD: "/a"}},
		{`{"session_id":"s1","cwd":"/a","hook_event_name":"StopFailure"}`,
			session.HookEvent{Name: "StopFailure", SessionID: "s1", CWD: "/a"}},
		{`{"session_id":"s1","cwd":"/a","hook_event_name":"SessionEnd","reason":"clear"}`,
			session.HookEvent{Name: "SessionEnd", SessionID: "s1", CWD: "/a"}},
		{`{"session_id":"s1","cwd":"/a","hook_event_name":"Notification","notification_type":"permission_prompt"}`,
			session.HookEvent{Name: "Notification", SessionID: "s1", CWD: "/a", NotificationType: "permission_prompt"}},
		{`{"session_id":"s1","cwd":"/a","hook_event_name":"PostToolUse","tool_name":"Bash"}`,
			session.HookEvent{Name: "PostToolUse", SessionID: "s1", CWD: "/a", ToolName: "Bash"}},
	}
	for _, c := range cases {
		c.want.Entrypoint = "claude-desktop"
		resp, out := r.post("/cm2d/v1/event", c.body, map[string]string{EntrypointHeader: "claude-desktop"})
		if resp.StatusCode != 200 || out != "" {
			t.Fatalf("%s: got %d %q", c.want.Name, resp.StatusCode, out)
		}
		ev := r.next()
		if ev.At.IsZero() {
			t.Errorf("%s: At not set", c.want.Name)
		}
		ev.At = time.Time{}
		if ev != c.want {
			t.Errorf("event = %+v, want %+v", ev, c.want)
		}
	}
}

func TestEventEndpointNoHeader(t *testing.T) {
	r := newRig(t, nil)
	r.post("/cm2d/v1/event", `{"session_id":"s1","hook_event_name":"Stop"}`, nil)
	if ev := r.next(); ev.Entrypoint != "" {
		t.Fatalf("entrypoint = %q, want empty", ev.Entrypoint)
	}
}

func TestEventEndpointBad(t *testing.T) {
	r := newRig(t, nil)
	for name, body := range map[string]string{
		"malformed":      `{"session_id":`,
		"empty":          ``,
		"array":          `[1,2]`,
		"no session id":  `{"hook_event_name":"Stop"}`,
		"no event name":  `{"session_id":"s1"}`,
		"wrong event":    `{"session_id":"s1","hook_event_name":"PermissionRequest"}`,
		"wrong type":     `{"session_id":5,"hook_event_name":"Stop"}`,
		"trailing junk":  `{"session_id":"s1","hook_event_name":"Stop"} x`,
		"null":           `null`,
		"bad nested key": `{"session_id":"s1","hook_event_name":"Stop","cwd":{}}`,
	} {
		resp, _ := r.post("/cm2d/v1/event", body, nil)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	r.noEvent()
}

func TestPermissionBadJSON(t *testing.T) {
	r := newRig(t, nil)
	resp, _ := r.post("/cm2d/v1/permission", `{nope`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	r.noEvent()
	if r.s.HeldCount() != 0 {
		t.Fatal("something was held")
	}
}

func TestBodyCap(t *testing.T) {
	r := newRig(t, nil)
	big := `{"session_id":"s1","hook_event_name":"Stop","pad":"` + strings.Repeat("a", MaxBody) + `"}`
	for _, path := range []string{"/cm2d/v1/event", "/cm2d/v1/permission", "/cm2d/v1/session"} {
		resp, _ := r.post(path, big, nil)
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: status %d, want 413", path, resp.StatusCode)
		}
	}
	r.noEvent()
	// A body just under the cap still works.
	pad := MaxBody - len(`{"session_id":"s1","hook_event_name":"Stop","pad":""}`) - 10
	ok := `{"session_id":"s1","hook_event_name":"Stop","pad":"` + strings.Repeat("a", pad) + `"}`
	if resp, _ := r.post("/cm2d/v1/event", ok, nil); resp.StatusCode != 200 {
		t.Fatalf("under cap: status %d", resp.StatusCode)
	}
	r.next()
}

func TestRoutes(t *testing.T) {
	r := newRig(t, func(s *Server) { s.Status = func() any { return map[string]int{"n": 3} } })
	if resp, _ := r.post("/nope", `{}`, nil); resp.StatusCode != 404 {
		t.Errorf("unknown path: %d, want 404", resp.StatusCode)
	}
	if resp, _ := r.post("/cm2d/v1/other", `{}`, nil); resp.StatusCode != 404 {
		t.Errorf("unknown v1 path: %d, want 404", resp.StatusCode)
	}
	if resp, err := http.Get(r.ts.URL + "/cm2d/v1/event"); err != nil || resp.StatusCode != 405 {
		t.Errorf("GET /event: %v %v, want 405", err, resp)
	}
	if resp, _ := r.post("/cm2d/v1/status", `{}`, nil); resp.StatusCode != 405 {
		t.Errorf("POST /status: %d, want 405", resp.StatusCode)
	}
	resp, err := http.Get(r.ts.URL + "/cm2d/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != `{"n":3}` {
		t.Fatalf("status = %d %q", resp.StatusCode, b)
	}
}

func TestStatusNilAndUnmarshalable(t *testing.T) {
	r := newRig(t, nil)
	resp, _ := http.Get(r.ts.URL + "/cm2d/v1/status")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "{}" {
		t.Fatalf("nil Status: %d %q", resp.StatusCode, b)
	}
	r2 := newRig(t, func(s *Server) { s.Status = func() any { return make(chan int) } })
	resp, _ = http.Get(r2.ts.URL + "/cm2d/v1/status")
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("bad Status: %d, want 500", resp.StatusCode)
	}
}

func TestPermissionResolveAllow(t *testing.T) {
	r := newRig(t, nil)
	ch := r.permAsync(context.Background(), permBody)
	ev := r.next()
	if ev.Name != "PermissionRequest" || ev.SessionID != "s1" || ev.ToolName != "Bash" || ev.IsQuestion || ev.RequestID == "" {
		t.Fatalf("event = %+v", ev)
	}
	r.noReply(ch)
	if r.s.HeldCount() != 1 {
		t.Fatalf("held = %d, want 1", r.s.HeldCount())
	}
	if !r.s.Resolve(ev.RequestID, Allow) {
		t.Fatal("Resolve returned false")
	}
	rp := r.waitReply(ch)
	if rp.code != 200 || rp.body != wantAllow {
		t.Fatalf("reply = %+v", rp)
	}
	r.noEvent() // an answered request emits no PermissionReleased
	if r.s.HeldCount() != 0 {
		t.Fatal("still held")
	}
	if r.s.Resolve(ev.RequestID, Allow) {
		t.Fatal("second Resolve returned true")
	}
}

func TestPermissionResolveDeny(t *testing.T) {
	r := newRig(t, nil)
	ch := r.permAsync(context.Background(), permBody)
	ev := r.next()
	r.s.Resolve(ev.RequestID, Deny)
	rp := r.waitReply(ch)
	if rp.code != 200 || rp.body != wantDeny {
		t.Fatalf("reply = %+v", rp)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(rp.body), &v); err != nil {
		t.Fatalf("deny body is not JSON: %v", err)
	}
	r.noEvent()
}

func TestReplyContentType(t *testing.T) {
	r := newRig(t, nil)
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(r.ts.URL+"/cm2d/v1/permission", "application/json", strings.NewReader(permBody))
		if err == nil {
			done <- resp
		}
	}()
	ev := r.next()
	r.s.Resolve(ev.RequestID, Allow)
	resp := <-done
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
}

func TestPermissionResolvePass(t *testing.T) {
	r := newRig(t, nil)
	ch := r.permAsync(context.Background(), permBody)
	ev := r.next()
	if !r.s.Resolve(ev.RequestID, Pass) {
		t.Fatal("Resolve returned false")
	}
	rp := r.waitReply(ch)
	if rp.code != 200 || rp.body != "" {
		t.Fatalf("reply = %+v", rp)
	}
	rel := r.next()
	if rel.Name != "PermissionReleased" || rel.RequestID != ev.RequestID || rel.SessionID != "s1" {
		t.Fatalf("release event = %+v", rel)
	}
}

func TestResolveUnknown(t *testing.T) {
	r := newRig(t, nil)
	if r.s.Resolve("nope", Allow) {
		t.Fatal("Resolve of unknown id returned true")
	}
}

func TestReleaseSession(t *testing.T) {
	r := newRig(t, nil)
	a := r.permAsync(context.Background(), permBody)
	ea := r.next()
	b := r.permAsync(context.Background(), permBody) // same session, second request
	eb := r.next()
	other := r.permAsync(context.Background(), strings.Replace(permBody, `"s1"`, `"s2"`, 1))
	eo := r.next()
	if r.s.HeldCount() != 3 {
		t.Fatalf("held = %d", r.s.HeldCount())
	}
	r.s.ReleaseSession("s1")
	for _, ch := range []<-chan reply{a, b} {
		if rp := r.waitReply(ch); rp.code != 200 || rp.body != "" {
			t.Fatalf("reply = %+v", rp)
		}
	}
	ids := map[string]bool{}
	for i := 0; i < 2; i++ {
		rel := r.next()
		if rel.Name != "PermissionReleased" || rel.SessionID != "s1" {
			t.Fatalf("release event = %+v", rel)
		}
		ids[rel.RequestID] = true
	}
	if !ids[ea.RequestID] || !ids[eb.RequestID] {
		t.Fatalf("released ids %v, want %s and %s", ids, ea.RequestID, eb.RequestID)
	}
	r.noReply(other)
	if r.s.HeldCount() != 1 {
		t.Fatalf("held = %d, want 1", r.s.HeldCount())
	}
	r.s.Resolve(eo.RequestID, Allow)
	r.waitReply(other)
	r.s.ReleaseSession("nobody") // no-op
}

func TestClientDisconnectReleases(t *testing.T) {
	r := newRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch := r.permAsync(ctx, permBody)
	ev := r.next()
	cancel()
	if rp := r.waitReply(ch); rp.err == nil {
		t.Fatalf("client got a reply: %+v", rp)
	}
	rel := r.next()
	if rel.Name != "PermissionReleased" || rel.RequestID != ev.RequestID {
		t.Fatalf("release event = %+v", rel)
	}
	waitFor(t, func() bool { return r.s.HeldCount() == 0 })
	if r.s.Resolve(ev.RequestID, Allow) {
		t.Fatal("Resolve after disconnect returned true")
	}
}

func TestMaxHold(t *testing.T) {
	r := newRig(t, func(s *Server) { s.MaxHold = 60 * time.Millisecond })
	start := time.Now()
	ch := r.permAsync(context.Background(), permBody)
	ev := r.next()
	rp := r.waitReply(ch)
	if rp.code != 200 || rp.body != "" {
		t.Fatalf("reply = %+v", rp)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("released before MaxHold")
	}
	rel := r.next()
	if rel.Name != "PermissionReleased" || rel.RequestID != ev.RequestID {
		t.Fatalf("release event = %+v", rel)
	}
	if r.s.HeldCount() != 0 {
		t.Fatal("still held")
	}
}

func TestMaxHoldDefault(t *testing.T) {
	if (&Server{}).maxHold() != 590*time.Second {
		t.Fatal("default MaxHold is not 590s")
	}
}

func TestShutdownReleases(t *testing.T) {
	evs := make(chan session.HookEvent, 8)
	s := &Server{Addr: "127.0.0.1:0", Events: evs, MaxHold: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()
	select {
	case <-s.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("server not ready")
	}
	url := "http://" + s.BoundAddr() + "/cm2d/v1/permission"
	got := make(chan reply, 2)
	for i := 0; i < 2; i++ {
		go func() {
			resp, err := http.Post(url, "application/json", strings.NewReader(permBody))
			if err != nil {
				got <- reply{err: err}
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			got <- reply{code: resp.StatusCode, body: string(b)}
		}()
	}
	waitFor(t, func() bool { return s.HeldCount() == 2 })
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case rp := <-got:
			if rp.err != nil || rp.code != 200 || rp.body != "" {
				t.Fatalf("reply = %+v", rp)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("held request not released at shutdown")
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return")
	}
	// 2 requests + 2 releases
	released := 0
	for len(evs) > 0 {
		if (<-evs).Name == "PermissionReleased" {
			released++
		}
	}
	if released != 2 {
		t.Fatalf("released events = %d, want 2", released)
	}
	if s.HeldCount() != 0 {
		t.Fatal("still held")
	}
}

func TestListenAddrInUse(t *testing.T) {
	s1 := &Server{Addr: "127.0.0.1:0", Events: make(chan session.HookEvent, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s1.ListenAndServe(ctx)
	<-s1.Ready()
	s2 := &Server{Addr: s1.BoundAddr(), Events: make(chan session.HookEvent, 1)}
	if err := s2.ListenAndServe(context.Background()); err == nil {
		t.Fatal("second bind on the same port succeeded")
	}
}

func TestEventsFullDropsAndCounts(t *testing.T) {
	evs := make(chan session.HookEvent, 1)
	s := &Server{Events: evs}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	body := `{"session_id":"s1","hook_event_name":"Stop"}`
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			resp, err := http.Post(ts.URL+"/cm2d/v1/event", "application/json", strings.NewReader(body))
			if err != nil || resp.StatusCode != 200 {
				t.Errorf("post %d: %v %v", i, err, resp)
				return
			}
			resp.Body.Close()
		}
	}()
	select {
	case <-done: // nobody reads evs; a blocking server would hang here
	case <-time.After(3 * time.Second):
		t.Fatal("request blocked on a full channel")
	}
	if got := s.Dropped(); got != 4 {
		t.Fatalf("dropped = %d, want 4", got)
	}
	if len(evs) != 1 {
		t.Fatalf("queued = %d, want 1", len(evs))
	}
}

func TestEventsFullPermissionNotHeld(t *testing.T) {
	evs := make(chan session.HookEvent, 1)
	evs <- session.HookEvent{Name: "filler"}
	s := &Server{Events: evs, MaxHold: time.Minute}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	done := make(chan reply, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/cm2d/v1/permission", "application/json", strings.NewReader(permBody))
		if err != nil {
			done <- reply{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		done <- reply{code: resp.StatusCode, body: string(b)}
	}()
	select {
	case rp := <-done:
		if rp.err != nil || rp.code != 200 || rp.body != "" {
			t.Fatalf("reply = %+v", rp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("permission request was held although the run loop never saw it")
	}
	if s.HeldCount() != 0 || s.Dropped() != 1 {
		t.Fatalf("held=%d dropped=%d", s.HeldCount(), s.Dropped())
	}
}

func TestNilEventsChannelDoesNotBlock(t *testing.T) {
	s := &Server{}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/cm2d/v1/event", "application/json", strings.NewReader(`{"session_id":"s","hook_event_name":"Stop"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", err, resp)
	}
	resp.Body.Close()
	if s.Dropped() != 1 {
		t.Fatalf("dropped = %d", s.Dropped())
	}
}

func TestCanHoldFalse(t *testing.T) {
	r := newRig(t, func(s *Server) { s.CanHold = func() bool { return false } })
	resp, out := r.post("/cm2d/v1/permission", permBody, map[string]string{EntrypointHeader: "cli"})
	if resp.StatusCode != 200 || out != "" {
		t.Fatalf("got %d %q", resp.StatusCode, out)
	}
	ev := r.next()
	if ev.Name != "PermissionRequest" || ev.IsQuestion || ev.Entrypoint != "cli" {
		t.Fatalf("event = %+v", ev)
	}
	if r.s.HeldCount() != 0 {
		t.Fatal("held although CanHold is false")
	}
	if r.s.Resolve(ev.RequestID, Allow) {
		t.Fatal("Resolve found a request that was never held")
	}
	r.noEvent()
}

func TestCanHoldChangesPerRequest(t *testing.T) {
	var mu sync.Mutex
	can := true
	r := newRig(t, func(s *Server) {
		s.CanHold = func() bool { mu.Lock(); defer mu.Unlock(); return can }
	})
	ch := r.permAsync(context.Background(), permBody)
	ev := r.next()
	mu.Lock()
	can = false // the pad drops while a request is held: it stays held
	mu.Unlock()
	r.noReply(ch)
	r.s.Resolve(ev.RequestID, Allow)
	r.waitReply(ch)
}

func TestAskUserQuestionFastPath(t *testing.T) {
	r := newRig(t, nil)
	body := `{"session_id":"s1","cwd":"/w","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion","tool_input":{"questions":[]}}`
	resp, out := r.post("/cm2d/v1/permission", body, nil)
	if resp.StatusCode != 200 || out != "" {
		t.Fatalf("got %d %q", resp.StatusCode, out)
	}
	ev := r.next()
	if ev.Name != "PermissionRequest" || !ev.IsQuestion || ev.ToolName != "AskUserQuestion" {
		t.Fatalf("event = %+v", ev)
	}
	if r.s.HeldCount() != 0 {
		t.Fatal("question was held")
	}
	r.noEvent() // no PermissionReleased for a request that was never held
}

// ExitPlanMode needs the user to read the plan, so it is a question too: it is
// never held, and the pad can only take him to the session.
func TestExitPlanModeIsAQuestion(t *testing.T) {
	r := newRig(t, nil)
	body := `{"session_id":"s1","cwd":"/w","hook_event_name":"PermissionRequest","tool_name":"ExitPlanMode","tool_input":{"plan":"x"}}`
	resp, out := r.post("/cm2d/v1/permission", body, nil)
	if resp.StatusCode != 200 || out != "" {
		t.Fatalf("got %d %q", resp.StatusCode, out)
	}
	ev := r.next()
	if !ev.IsQuestion || ev.ToolName != "ExitPlanMode" {
		t.Fatalf("event = %+v", ev)
	}
	if r.s.HeldCount() != 0 {
		t.Fatal("ExitPlanMode was held")
	}
}

func TestPermissionEntrypoint(t *testing.T) {
	r := newRig(t, nil)
	ch := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, r.ts.URL+"/cm2d/v1/permission", strings.NewReader(permBody))
		req.Header.Set(EntrypointHeader, "claude-desktop")
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			ch <- resp
		}
	}()
	ev := r.next()
	if ev.Entrypoint != "claude-desktop" {
		t.Fatalf("entrypoint = %q", ev.Entrypoint)
	}
	r.s.Resolve(ev.RequestID, Pass)
	(<-ch).Body.Close()
}

func TestRequestIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newRequestID()
		if id == "" || seen[id] {
			t.Fatalf("bad or repeated id %q", id)
		}
		seen[id] = true
	}
}

// Resolve and a release at the same moment: exactly one wins, nothing hangs.
func TestResolveReleaseRace(t *testing.T) {
	r := newRig(t, func(s *Server) { s.MaxHold = 5 * time.Millisecond })
	for i := 0; i < 20; i++ {
		ch := r.permAsync(context.Background(), permBody)
		ev := r.next()
		go r.s.Resolve(ev.RequestID, Allow)
		rp := r.waitReply(ch)
		if rp.code != 200 || (rp.body != "" && rp.body != wantAllow) {
			t.Fatalf("reply = %+v", rp)
		}
		// Drain a possible PermissionReleased.
		for len(r.evs) > 0 {
			<-r.evs
		}
	}
}

func TestSessionBodyEcho(t *testing.T) {
	// Unknown extra fields are fine; the body type must not matter.
	r := newRig(t, nil)
	var buf bytes.Buffer
	buf.WriteString(`{"payload":{"session_id":"s9","extra":[1,2,3]},"attended":"0","unknown":true}`)
	resp, _ := r.post("/cm2d/v1/session", buf.String(), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ev := r.next(); ev.SessionID != "s9" || ev.Attended != "0" {
		t.Fatalf("event = %+v", ev)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}

// MaxBody is the 1 MiB of the spec, not whatever the constant says.
func TestBodyCapIsOneMiB(t *testing.T) {
	if MaxBody != 1<<20 {
		t.Fatalf("MaxBody = %d, want %d", MaxBody, 1<<20)
	}
	r := newRig(t, nil)
	const head = `{"session_id":"s1","hook_event_name":"Stop","pad":"`
	const tail = `"}`
	fill := func(total int) string {
		return head + strings.Repeat("a", total-len(head)-len(tail)) + tail
	}
	if resp, _ := r.post("/cm2d/v1/event", fill(1<<20), nil); resp.StatusCode != 200 {
		t.Fatalf("exactly 1 MiB: status %d", resp.StatusCode)
	}
	r.next()
	if resp, _ := r.post("/cm2d/v1/event", fill(1<<20+1), nil); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("1 MiB + 1: status %d, want 413", resp.StatusCode)
	}
	r.noEvent()
}

// A page in the user's browser can send a cross-origin POST (text/plain) or
// reach the port through DNS rebinding. Neither may inject an event.
func TestRejectsForeignHost(t *testing.T) {
	r := newRig(t, func(s *Server) { s.Status = func() any { return map[string]int{"n": 1} } })
	body := `{"session_id":"s1","hook_event_name":"Stop"}`
	for _, host := range []string{"evil.example", "evil.example:80", "127.0.0.1.evil.example:1234", "10.0.0.5:1234"} {
		req, _ := http.NewRequest(http.MethodPost, r.ts.URL+"/cm2d/v1/event", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Host %q: status %d, want 403", host, resp.StatusCode)
		}
		// The status document must not leak either.
		greq, _ := http.NewRequest(http.MethodGet, r.ts.URL+"/cm2d/v1/status", nil)
		greq.Host = host
		gresp, err := http.DefaultClient.Do(greq)
		if err != nil {
			t.Fatal(err)
		}
		gresp.Body.Close()
		if gresp.StatusCode != http.StatusForbidden {
			t.Errorf("GET status with Host %q: status %d, want 403", host, gresp.StatusCode)
		}
	}
	r.noEvent()
	for _, host := range []string{"127.0.0.1:47731", "localhost:47731", "[::1]:47731", "LOCALHOST:1", "127.0.0.1"} {
		req, _ := http.NewRequest(http.MethodPost, r.ts.URL+"/cm2d/v1/event", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("Host %q: status %d, want 200", host, resp.StatusCode)
		}
		r.next()
	}
}

func TestPostNeedsJSONContentType(t *testing.T) {
	r := newRig(t, nil)
	body := `{"session_id":"s1","hook_event_name":"Stop"}`
	for _, ct := range []string{"text/plain", "text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data", ""} {
		for _, path := range []string{"/cm2d/v1/event", "/cm2d/v1/permission", "/cm2d/v1/session"} {
			req, _ := http.NewRequest(http.MethodPost, r.ts.URL+path, strings.NewReader(body))
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			} else {
				req.Header["Content-Type"] = nil
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnsupportedMediaType {
				t.Errorf("%s with %q: status %d, want 415", path, ct, resp.StatusCode)
			}
		}
	}
	r.noEvent()
	if r.s.HeldCount() != 0 {
		t.Fatal("something was held")
	}
	// JSON with parameters, in any case, is fine.
	if resp, _ := r.post("/cm2d/v1/event", body, map[string]string{"Content-Type": "Application/JSON; charset=utf-8"}); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	r.next()
}

// A client that sends headers and stalls must not park a handler for ever.
func TestBodyReadDeadline(t *testing.T) {
	r := newRig(t, func(s *Server) { s.BodyTimeout = 150 * time.Millisecond })
	conn, err := net.Dial("tcp", r.ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /cm2d/v1/event HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nContent-Length: 500\r\n\r\n{\"session_id\"")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	line, err := io.ReadAll(conn)
	if time.Since(start) > time.Second {
		t.Fatalf("handler waited %v for a stalled body (err %v)", time.Since(start), err)
	}
	if !strings.HasPrefix(string(line), "HTTP/1.1 400") && !strings.HasPrefix(string(line), "HTTP/1.1 408") {
		t.Fatalf("reply = %q", line)
	}
	r.noEvent()
}

// A held request is not killed by the body deadline.
func TestBodyDeadlineDoesNotCutHold(t *testing.T) {
	r := newRig(t, func(s *Server) { s.BodyTimeout = 100 * time.Millisecond })
	ch := r.permAsync(context.Background(), permBody)
	ev := r.next()
	r.noReply(ch)
	time.Sleep(300 * time.Millisecond)
	r.noReply(ch)
	if r.s.HeldCount() != 1 {
		t.Fatalf("held = %d", r.s.HeldCount())
	}
	r.s.Resolve(ev.RequestID, Allow)
	if rp := r.waitReply(ch); rp.body != wantAllow {
		t.Fatalf("reply = %+v", rp)
	}
}

func TestStatusErrorIs503(t *testing.T) {
	r := newRig(t, func(s *Server) { s.Status = func() any { return errors.New("daemon is busy") } })
	resp, err := http.Get(r.ts.URL + "/cm2d/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), "daemon is busy") {
		t.Fatalf("got %d %q, want 503 with the message", resp.StatusCode, b)
	}
}

func TestHolds(t *testing.T) {
	r := newRig(t, nil)
	ch := r.permAsync(context.Background(), permBody)
	ev := r.next()
	if !r.s.Holds(ev.RequestID) {
		t.Fatal("Holds = false for a held request")
	}
	if r.s.Holds("nope") {
		t.Fatal("Holds = true for an unknown id")
	}
	r.s.Resolve(ev.RequestID, Allow)
	r.waitReply(ch)
	if r.s.Holds(ev.RequestID) {
		t.Fatal("Holds = true after Resolve")
	}
}

func TestReleaseAll(t *testing.T) {
	r := newRig(t, nil)
	a := r.permAsync(context.Background(), permBody)
	ea := r.next()
	b := r.permAsync(context.Background(), strings.Replace(permBody, `"s1"`, `"s2"`, 1))
	eb := r.next()
	r.s.ReleaseAll()
	for _, ch := range []<-chan reply{a, b} {
		if rp := r.waitReply(ch); rp.code != 200 || rp.body != "" {
			t.Fatalf("reply = %+v", rp)
		}
	}
	ids := map[string]bool{}
	for i := 0; i < 2; i++ {
		rel := r.next()
		if rel.Name != "PermissionReleased" {
			t.Fatalf("event = %+v", rel)
		}
		ids[rel.RequestID] = true
	}
	if !ids[ea.RequestID] || !ids[eb.RequestID] || r.s.HeldCount() != 0 {
		t.Fatalf("released %v, held %d", ids, r.s.HeldCount())
	}
	r.s.ReleaseAll() // nothing left: no-op
}

// If the pad drops between the first CanHold check and the registration,
// the request must not stay held: ReleaseAll has already run.
func TestCanHoldRechecked(t *testing.T) {
	var calls int
	var mu sync.Mutex
	r := newRig(t, func(s *Server) {
		s.CanHold = func() bool { mu.Lock(); defer mu.Unlock(); calls++; return calls == 1 }
	})
	resp, out := r.post("/cm2d/v1/permission", permBody, nil)
	if resp.StatusCode != 200 || out != "" {
		t.Fatalf("got %d %q, want an empty 200 at once", resp.StatusCode, out)
	}
	ev := r.next()
	if ev.Name != "PermissionRequest" {
		t.Fatalf("event = %+v", ev)
	}
	if r.s.HeldCount() != 0 || r.s.Holds(ev.RequestID) {
		t.Fatal("request stayed held")
	}
	r.noEvent()
}
