package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cm2-claude/internal/daemon"
	"cm2-claude/internal/device"
	"cm2-claude/internal/device/devicetest"
	"cm2-claude/internal/focus"
	"cm2-claude/internal/herdr"
	"cm2-claude/internal/herdr/herdrtest"
	"cm2-claude/internal/session"
)

// Colors of the palette.
const (
	blue  = 0x0050FF
	amber = 0xFFA000
	green = 0x00C853
	red   = 0xFF2020
	white = 0xFFFFFF
)

const (
	allowJSON = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
)

// light is one thread as the pad was told.
type light struct {
	C int
	B float64
	E int
	S float64
}

func (l light) off() bool { return l.E == 0 }

type wireThread struct {
	ID int     `json:"id"`
	C  int     `json:"c"`
	B  float64 `json:"b"`
	E  int     `json:"e"`
	S  float64 `json:"s"`
}

// thstatusCalls decodes every v.oai.thstatus request the fake saw.
func thstatusCalls(f *devicetest.Fake) [][]wireThread {
	var out [][]wireThread
	for _, req := range f.Requests() {
		var r struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(req, &r) != nil || r.Method != "v.oai.thstatus" {
			continue
		}
		var ts []wireThread
		if json.Unmarshal(r.Params, &ts) == nil {
			out = append(out, ts)
		}
	}
	return out
}

// lights replays the calls into the current state of each thread.
func lights(f *devicetest.Fake) map[int]light {
	cur := map[int]light{}
	for _, call := range thstatusCalls(f) {
		for _, t := range call {
			cur[t.ID] = light{t.C, t.B, t.E, t.S}
		}
	}
	return cur
}

// everLit reports whether thread id was ever sent with color c and effect on.
func everLit(f *devicetest.Fake, id, c int) bool {
	for _, call := range thstatusCalls(f) {
		for _, t := range call {
			if t.ID == id && t.C == c && t.E != 0 {
				return true
			}
		}
	}
	return false
}

// harness is a daemon on a random port with fake pads and a fake focuser.
type harness struct {
	t      *testing.T
	d      *daemon.Daemon
	base   string
	cancel context.CancelFunc
	done   chan error

	mu       sync.Mutex
	pads     []*devicetest.Fake
	openFail error
	opens    int
	focused  []string
	padHook  func(n int, f *devicetest.Fake)
}

type opts struct {
	noPad bool
	herdr *herdrtest.Server
	cfg   func(*daemon.Config)
	// openFail makes OpenPad fail until the harness clears it.
	openFail error
	// padHook may change the n-th fake pad (1 based) before it is used.
	padHook func(n int, f *devicetest.Fake)
}

func newHarness(t *testing.T, o opts) *harness {
	t.Helper()
	h := &harness{t: t, done: make(chan error, 1), openFail: o.openFail, padHook: o.padHook}
	cfg := daemon.Config{
		Addr:         "127.0.0.1:0",
		NoDevice:     o.noPad,
		OpenPad:      h.openPad,
		RetryEvery:   30 * time.Millisecond,
		TickEvery:    20 * time.Millisecond,
		ForeignPause: 300 * time.Millisecond,
		Version:      "test",
		Table: session.Config{
			NeedsInputDelay:   30 * time.Millisecond,
			IgnoreCWDPrefixes: []string{"/ignored"},
		},
		Focuser: &focus.Focuser{Run: h.runCmd},
	}
	if o.herdr != nil {
		cfg.Herdr = herdr.NewClient(o.herdr.Path())
		cfg.Focuser.Herdr = cfg.Herdr
		cfg.HerdrTune = func(w *herdr.Watcher) {
			w.PollInterval = 50 * time.Millisecond
			w.Debounce = 10 * time.Millisecond
		}
	}
	if o.cfg != nil {
		o.cfg(&cfg)
	}
	h.d = daemon.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- h.d.Run(ctx) }()
	select {
	case <-h.d.Ready():
	case err := <-h.done:
		t.Fatalf("daemon stopped at start: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("daemon not ready")
	}
	h.base = "http://" + h.d.Addr() + "/cm2d/v1"
	t.Cleanup(h.stop)
	if !o.noPad && o.openFail == nil {
		h.waitConnected()
	}
	return h
}

// stop ends the daemon and checks that Run returned nil.
func (h *harness) stop() {
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Run returned %v", err)
		}
		h.done <- nil
	case <-time.After(5 * time.Second):
		h.t.Error("daemon did not stop")
	}
}

func (h *harness) openPad() (device.Transport, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opens++
	if h.openFail != nil {
		return nil, h.openFail
	}
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		var r struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(req, &r) != nil {
			return nil
		}
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":{"ok":1}}`, r.ID))}
	}
	h.pads = append(h.pads, f)
	if h.padHook != nil {
		h.padHook(len(h.pads), f)
	}
	return f, nil
}

func (h *harness) setOpenFail(err error) {
	h.mu.Lock()
	h.openFail = err
	h.mu.Unlock()
}

// pad is the newest fake pad.
func (h *harness) pad() *devicetest.Fake {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pads) == 0 {
		h.t.Fatal("no pad opened")
	}
	return h.pads[len(h.pads)-1]
}

// opensCount is how often the daemon tried to open the pad.
func (h *harness) opensCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.opens
}

func (h *harness) padCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pads)
}

func (h *harness) runCmd(ctx context.Context, name string, args ...string) error {
	h.mu.Lock()
	h.focused = append(h.focused, name+" "+strings.Join(args, " "))
	h.mu.Unlock()
	return nil
}

func (h *harness) focusCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.focused...)
}

func (h *harness) waitConnected() {
	h.t.Helper()
	eventually(h.t, "pad connected", func() bool { return h.d.Connected() })
}

// press sends a key press as the pad would.
func (h *harness) press(key string) {
	h.t.Helper()
	h.pad().Inject([]byte(fmt.Sprintf(`{"m":"v.oai.hid","p":{"k":%q,"act":1}}`, key)))
}

func (h *harness) turn(key string) {
	h.t.Helper()
	h.pad().Inject([]byte(fmt.Sprintf(`{"m":"v.oai.hid","p":{"k":%q,"act":2}}`, key)))
}

func (h *harness) post(path, entrypoint string, body any) (int, string) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if entrypoint != "" {
		req.Header.Set("X-Cm2-Entrypoint", entrypoint)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// start sends a SessionStart through the command-hook endpoint.
func (h *harness) start(id, cwd, attended, pane string) {
	h.t.Helper()
	payload := map[string]string{"session_id": id, "cwd": cwd, "hook_event_name": "SessionStart", "source": "startup"}
	code, _ := h.post("/session", "", map[string]any{
		"payload": payload, "attended": attended, "entrypoint": "cli", "herdr_pane": pane,
	})
	if code != 200 {
		h.t.Fatalf("session start: HTTP %d", code)
	}
}

// event posts a non-permission hook event.
func (h *harness) event(name, id, cwd string) {
	h.t.Helper()
	code, _ := h.post("/event", "cli", map[string]string{"session_id": id, "cwd": cwd, "hook_event_name": name})
	if code != 200 {
		h.t.Fatalf("%s: HTTP %d", name, code)
	}
}

// work starts a session and a turn.
func (h *harness) work(id, cwd string) {
	h.t.Helper()
	h.start(id, cwd, "1", "")
	h.event("UserPromptSubmit", id, cwd)
}

type permResult struct {
	code int
	body string
	err  error
	took time.Duration
}

// permission posts a PermissionRequest in the background.
func (h *harness) permission(ctx context.Context, id, cwd, tool, entrypoint string) <-chan permResult {
	out := make(chan permResult, 1)
	body, _ := json.Marshal(map[string]any{
		"session_id": id, "cwd": cwd, "hook_event_name": "PermissionRequest", "tool_name": tool,
		"tool_input": map[string]string{"command": "ls"},
	})
	go func() {
		start := time.Now()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/permission", bytes.NewReader(body))
		req.Header.Set("X-Cm2-Entrypoint", entrypoint)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- permResult{err: err, took: time.Since(start)}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		out <- permResult{code: resp.StatusCode, body: string(b), took: time.Since(start)}
	}()
	return out
}

func (h *harness) status() daemon.StatusDoc {
	h.t.Helper()
	resp, err := http.Get(h.base + "/status")
	if err != nil {
		h.t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	var doc daemon.StatusDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		h.t.Fatalf("status JSON: %v", err)
	}
	return doc
}

// sess returns the session row with the key, or nil.
func (h *harness) sess(key string) *daemon.SessionStatus {
	h.t.Helper()
	for _, s := range h.status().Sessions {
		if s.Key == key {
			s := s
			return &s
		}
	}
	return nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// waitLight waits until thread id satisfies pred on the newest pad.
func (h *harness) waitLight(what string, id int, pred func(light) bool) {
	h.t.Helper()
	eventually(h.t, what, func() bool {
		l, ok := lights(h.pad())[id]
		return ok && pred(l)
	})
}

func solid(c int) func(light) bool { return func(l light) bool { return l.C == c && l.E == 1 } }
func breathing(c int) func(light) bool {
	return func(l light) bool { return l.C == c && l.E == 4 && l.S > 0 }
}
func isOff(l light) bool { return l.off() }

// waitHeld waits until the hooks server holds n requests.
func (h *harness) waitHeld(n int) {
	h.t.Helper()
	eventually(h.t, fmt.Sprintf("%d held requests", n), func() bool { return h.status().Held == n })
}

func recv(t *testing.T, ch <-chan permResult) permResult {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("permission request failed: %v", r.err)
		}
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("no reply to the permission request")
	}
	return permResult{}
}
