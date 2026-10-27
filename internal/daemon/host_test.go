package daemon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"cm2-claude/internal/daemon"
	"cm2-claude/internal/focus"
	"cm2-claude/internal/herdr/herdrtest"
)

const (
	hostID1 = "local_1d9854de-0000-4000-8000-000000000001"
	hostID2 = "local_1d9854de-0000-4000-8000-000000000002"
)

// fakeDesktop answers Check from a fixed table and records the asks.
type fakeDesktop struct {
	mu     sync.Mutex
	ok     bool
	reason string
	asked  []string
}

func (f *fakeDesktop) Check(id string) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, id)
	return f.ok, f.reason
}

func (f *fakeDesktop) set(ok bool, reason string) {
	f.mu.Lock()
	f.ok, f.reason = ok, reason
	f.mu.Unlock()
}

func (f *fakeDesktop) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// postHost posts with the entrypoint and host session headers.
func (h *harness) postHost(path, host string, body any) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cm2-Entrypoint", "claude-desktop")
	if host != "" {
		req.Header.Set("X-Cm2-Host-Session", host)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		h.t.Fatalf("POST %s: HTTP %d", path, resp.StatusCode)
	}
}

// desktopStart registers an attended desktop session, as the forwarder does.
func (h *harness) desktopStart(id, cwd, host string) {
	h.t.Helper()
	code, _ := h.post("/session", "", map[string]any{
		"payload":  map[string]string{"session_id": id, "cwd": cwd, "hook_event_name": "SessionStart", "source": "startup"},
		"attended": "1", "entrypoint": "claude-desktop", "host_session_id": host,
	})
	if code != 200 {
		h.t.Fatalf("session start: HTTP %d", code)
	}
}

func (h *harness) desktopEvent(name, id, cwd, host string) {
	h.t.Helper()
	h.postHost("/event", host, map[string]string{"session_id": id, "cwd": cwd, "hook_event_name": name})
}

func withDesktop(d focus.DesktopChats) func(*daemon.Config) {
	return func(c *daemon.Config) { c.Focuser.Desktop = d }
}

func TestDesktopKeyOpensTheContinueRoute(t *testing.T) {
	d := &fakeDesktop{ok: true}
	h := newHarness(t, opts{cfg: withDesktop(d)})
	h.desktopStart("D1", "/w/desk", hostID1)
	h.desktopEvent("UserPromptSubmit", "D1", "/w/desk", hostID1)
	h.waitLight("desktop session lit", 0, solid(blue))

	s := h.sess("claude:D1")
	if s == nil || s.HostSession != hostID1 || s.HostConflict || s.Frontend != "desktop" {
		t.Fatalf("session = %+v", s)
	}

	h.press("AG00")
	eventually(t, "continue route opened", func() bool { return len(h.focusCalls()) == 1 })
	want := "open claude://code/continue?session=" + hostID1
	if got := h.focusCalls()[0]; got != want {
		t.Fatalf("ran %q, want %q", got, want)
	}
	if got := d.calls(); len(got) != 1 || got[0] != hostID1 {
		t.Fatalf("checked %v", got)
	}
}

func TestDesktopKeyFallsBackWhenTheCheckFails(t *testing.T) {
	d := &fakeDesktop{ok: false, reason: "another active record has the same cli session"}
	h := newHarness(t, opts{cfg: withDesktop(d)})
	h.desktopStart("D1", "/w/desk", hostID1)
	h.desktopEvent("UserPromptSubmit", "D1", "/w/desk", hostID1)
	h.waitLight("desktop session lit", 0, solid(blue))

	h.press("AG00")
	eventually(t, "app raised", func() bool { return len(h.focusCalls()) == 1 })
	if got := h.focusCalls()[0]; got != "open -a Claude" {
		t.Fatalf("ran %q", got)
	}

	// The check is made on every press: once the records are fine, the route is used.
	d.set(true, "")
	h.press("AG00")
	eventually(t, "continue route opened", func() bool { return len(h.focusCalls()) == 2 })
	if got := h.focusCalls()[1]; got != "open claude://code/continue?session="+hostID1 {
		t.Fatalf("ran %q", got)
	}
}

func TestDesktopKeyWithoutAHostIDRaisesTheApp(t *testing.T) {
	d := &fakeDesktop{ok: true}
	h := newHarness(t, opts{cfg: withDesktop(d)})
	h.desktopStart("D1", "/w/desk", "") // an older hook config: no host id
	h.desktopEvent("UserPromptSubmit", "D1", "/w/desk", "")
	h.waitLight("desktop session lit", 0, solid(blue))
	h.press("AG00")
	eventually(t, "app raised", func() bool { return len(h.focusCalls()) == 1 })
	if got := h.focusCalls()[0]; got != "open -a Claude" {
		t.Fatalf("ran %q", got)
	}
	if len(d.calls()) != 0 {
		t.Fatalf("checked %v", d.calls())
	}
}

func TestDesktopKeyWithAMalformedHostIDRaisesTheApp(t *testing.T) {
	d := &fakeDesktop{ok: true}
	h := newHarness(t, opts{cfg: withDesktop(d)})
	h.desktopStart("D1", "/w/desk", "$CLAUDE_CODE_HOST_SESSION_ID")
	h.desktopEvent("UserPromptSubmit", "D1", "/w/desk", "local_../x")
	h.waitLight("desktop session lit", 0, solid(blue))
	if s := h.sess("claude:D1"); s == nil || s.HostSession != "" {
		t.Fatalf("session = %+v", s)
	}
	h.press("AG00")
	eventually(t, "app raised", func() bool { return len(h.focusCalls()) == 1 })
	if got := h.focusCalls()[0]; got != "open -a Claude" {
		t.Fatalf("ran %q", got)
	}
}

func TestHostConflictShowsInStatusAndBlocksTheRoute(t *testing.T) {
	d := &fakeDesktop{ok: true}
	h := newHarness(t, opts{cfg: withDesktop(d)})
	h.desktopStart("D1", "/w/desk", hostID1)
	h.desktopEvent("UserPromptSubmit", "D1", "/w/desk", hostID1)
	h.desktopEvent("PostToolUse", "D1", "/w/desk", hostID2) // a fork reports in
	eventually(t, "conflict", func() bool {
		s := h.sess("claude:D1")
		return s != nil && s.HostConflict
	})
	if s := h.sess("claude:D1"); s.HostSession != hostID1 {
		t.Fatalf("first id not kept: %+v", s)
	}
	h.press("AG00")
	eventually(t, "app raised", func() bool { return len(h.focusCalls()) == 1 })
	if got := h.focusCalls()[0]; got != "open -a Claude" {
		t.Fatalf("ran %q", got)
	}
	if len(d.calls()) != 0 {
		t.Fatalf("checked %v", d.calls())
	}
}

func TestStatusJSONHasHostFields(t *testing.T) {
	h := newHarness(t, opts{noPad: true})
	h.desktopStart("D1", "/w/desk", hostID1)
	h.desktopEvent("UserPromptSubmit", "D1", "/w/desk", hostID1)
	resp, err := http.Get(h.base + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Sessions) != 1 {
		t.Fatalf("sessions = %v", doc.Sessions)
	}
	row := doc.Sessions[0]
	if row["host_session"] != hostID1 {
		t.Fatalf("row = %v", row)
	}
	if v, ok := row["host_conflict"]; ok && v != false {
		t.Fatalf("host_conflict = %v", v)
	}
	if strings.Contains(fmt.Sprint(row), "title") {
		t.Fatalf("row mentions a title: %v", row)
	}
}

func TestSeededHerdrSessionsShowAtOnce(t *testing.T) {
	hs := herdrtest.New(t)
	var as []herdrtest.Agent
	for i := 0; i < 7; i++ {
		pane := fmt.Sprintf("w1:p%d", i+1)
		a := herdrtest.NewAgent(pane, "claude", "idle")
		a["agent_session"] = map[string]any{"source": "hook", "agent": "claude", "kind": "id", "value": fmt.Sprintf("H%d", i)}
		as = append(as, a)
	}
	hs.SetAgents(as...)
	h := newHarness(t, opts{herdr: hs})

	eventually(t, "seven sessions", func() bool { return len(h.status().Sessions) == 7 })
	for i := 0; i < 7; i++ {
		s := h.sess(fmt.Sprintf("claude:H%d", i))
		if s == nil || s.Slot != i || s.Frontend != "herdr" || s.State != "idle" || s.Kind != "claude" {
			t.Fatalf("H%d = %+v", i, s)
		}
		h.waitLight(fmt.Sprintf("key %d dim", i), i, func(l light) bool { return !l.off() && l.B < 0.2 })
	}
	for id := 7; id < 10; id++ {
		if l, ok := lights(h.pad())[id]; ok && !l.off() {
			t.Fatalf("thread %d lit: %+v", id, l)
		}
	}

	// A hook event for a seeded session joins its entry.
	h.event("UserPromptSubmit", "H3", "/work/w1:p4")
	h.waitLight("H3 working", 3, solid(blue))
	if n := len(h.status().Sessions); n != 7 {
		t.Fatalf("sessions = %d", n)
	}
}

func TestSeededHerdrSessionsNeverEvict(t *testing.T) {
	hs := herdrtest.New(t)
	h := newHarness(t, opts{herdr: hs})
	for i := 0; i < 10; i++ {
		h.work(fmt.Sprintf("S%d", i), fmt.Sprintf("/w/p%d", i))
	}
	// Ten working sessions fill the pad. Two idle Herdr agents appear.
	var as []herdrtest.Agent
	for i := 0; i < 2; i++ {
		a := herdrtest.NewAgent(fmt.Sprintf("w2:p%d", i), "claude", "idle")
		a["agent_session"] = map[string]any{"source": "hook", "agent": "claude", "kind": "id", "value": fmt.Sprintf("H%d", i)}
		as = append(as, a)
	}
	hs.SetAgents(as...)
	eventually(t, "twelve sessions", func() bool { return len(h.status().Sessions) == 12 })
	for i := 0; i < 2; i++ {
		if s := h.sess(fmt.Sprintf("claude:H%d", i)); s == nil || s.Slot != -1 {
			t.Fatalf("H%d = %+v", i, s)
		}
	}
	for i := 0; i < 10; i++ {
		if s := h.sess(fmt.Sprintf("claude:S%d", i)); s == nil || s.Slot != i {
			t.Fatalf("S%d = %+v", i, s)
		}
	}
}
