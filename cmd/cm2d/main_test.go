package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cm2-claude/internal/daemon"
	"cm2-claude/internal/device"
	"cm2-claude/internal/device/devicetest"
	"cm2-claude/internal/hookcfg"
)

type testEnv struct {
	*env
	out, errOut *lockedBuf
}

func newTestEnv(t *testing.T, stdin string, vars map[string]string) *testEnv {
	t.Helper()
	te := &testEnv{out: &lockedBuf{}, errOut: &lockedBuf{}}
	home := t.TempDir()
	te.env = &env{
		Stdin:  strings.NewReader(stdin),
		Stdout: te.out, Stderr: te.errOut,
		Getenv: func(k string) string { return vars[k] },
		Exe:    func() (string, error) { return "/opt/cm2d/cm2d", nil },
		Home:   func() (string, error) { return home, nil },
	}
	return te
}

func (te *testEnv) run(args ...string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return run(ctx, args, te.env)
}

func TestUsageAndVersion(t *testing.T) {
	te := newTestEnv(t, "", nil)
	if code := te.run(); code != exitUsage || !strings.Contains(te.errOut.String(), "usage: cm2d") {
		t.Fatalf("no args: %d %q", code, te.errOut.String())
	}
	te = newTestEnv(t, "", nil)
	if code := te.run("bogus"); code != exitUsage || !strings.Contains(te.errOut.String(), `unknown command "bogus"`) {
		t.Fatalf("unknown: %d %q", code, te.errOut.String())
	}
	te = newTestEnv(t, "", nil)
	if code := te.run("version"); code != exitOK || te.out.String() != "cm2d "+version+"\n" {
		t.Fatalf("version: %d %q", code, te.out.String())
	}
	te = newTestEnv(t, "", nil)
	if code := te.run("help"); code != exitOK {
		t.Fatalf("help: %d", code)
	}
	for _, want := range []string{"run", "status", "hook session-start", "install-hooks", "uninstall-hooks", "setup", "herdr", "version"} {
		if !strings.Contains(te.out.String(), want) {
			t.Errorf("usage lacks %q", want)
		}
	}
}

func TestHerdrSubcommandStillWorks(t *testing.T) {
	srv := fakeWithAgents(t)
	te := newTestEnv(t, "", nil)
	if code := te.run("herdr", "list", "--socket", srv.Path()); code != exitOK || !strings.Contains(te.out.String(), "w1:p1") {
		t.Fatalf("herdr list: %d %q %q", code, te.out.String(), te.errOut.String())
	}
}

// hookServer records the POSTs it gets.
type hookServer struct {
	srv  *httptest.Server
	port int
	mu   sync.Mutex
	got  []string
}

func newHookServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *hookServer {
	t.Helper()
	hs := &hookServer{}
	hs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hs.mu.Lock()
		hs.got = append(hs.got, r.URL.Path+" "+string(b))
		hs.mu.Unlock()
		if handler != nil {
			handler(w, r)
		}
	}))
	t.Cleanup(hs.srv.Close)
	_, p, _ := net.SplitHostPort(hs.srv.Listener.Addr().String())
	hs.port, _ = strconv.Atoi(p)
	return hs
}

func (hs *hookServer) posts() []string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]string(nil), hs.got...)
}

const startJSON = `{"session_id":"abc","cwd":"/w/p","hook_event_name":"SessionStart","source":"startup"}`

func TestHookSessionStartForwards(t *testing.T) {
	hs := newHookServer(t, nil)
	te := newTestEnv(t, startJSON+"\n", map[string]string{
		"CLAUDE_CODE_SESSION_ATTENDED": "1", "CLAUDE_CODE_ENTRYPOINT": "cli", "HERDR_PANE_ID": "w1:p2",
	})
	if code := te.run("hook", "session-start", "--port", strconv.Itoa(hs.port)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if te.out.String() != "" || te.errOut.String() != "" {
		t.Fatalf("printed %q / %q", te.out.String(), te.errOut.String())
	}
	posts := hs.posts()
	if len(posts) != 1 || !strings.HasPrefix(posts[0], "/cm2d/v1/session ") {
		t.Fatalf("posts = %q", posts)
	}
	var env struct {
		Payload    map[string]string `json:"payload"`
		Attended   string            `json:"attended"`
		Entrypoint string            `json:"entrypoint"`
		HerdrPane  string            `json:"herdr_pane"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(posts[0], "/cm2d/v1/session ")), &env); err != nil {
		t.Fatal(err)
	}
	if env.Payload["session_id"] != "abc" || env.Attended != "1" || env.Entrypoint != "cli" || env.HerdrPane != "w1:p2" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestHookSessionStartForwardsHostSession(t *testing.T) {
	for _, tc := range []struct{ name, env, want string }{
		{"set", "local_abc-123", "local_abc-123"},
		{"unset", "", ""},
		{"raw value is sent, the daemon checks it", "$CLAUDE_CODE_HOST_SESSION_ID", "$CLAUDE_CODE_HOST_SESSION_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHookServer(t, nil)
			te := newTestEnv(t, startJSON, map[string]string{
				"CLAUDE_CODE_ENTRYPOINT": "claude-desktop", "CLAUDE_CODE_HOST_SESSION_ID": tc.env,
			})
			if code := te.run("hook", "session-start", "--port", strconv.Itoa(hs.port)); code != 0 {
				t.Fatalf("exit %d", code)
			}
			posts := hs.posts()
			if len(posts) != 1 {
				t.Fatalf("posts = %q", posts)
			}
			var env struct {
				HostSessionID string `json:"host_session_id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(posts[0], "/cm2d/v1/session ")), &env); err != nil {
				t.Fatal(err)
			}
			if env.HostSessionID != tc.want {
				t.Fatalf("host_session_id = %q, want %q", env.HostSessionID, tc.want)
			}
		})
	}
}

func TestHookSessionStartPortFromEnv(t *testing.T) {
	hs := newHookServer(t, nil)
	te := newTestEnv(t, startJSON, map[string]string{"CM2D_PORT": strconv.Itoa(hs.port)})
	if code := te.run("hook", "session-start"); code != 0 || len(hs.posts()) != 1 {
		t.Fatalf("exit %d, posts %q", code, hs.posts())
	}
}

func TestHookSessionStartAlwaysExitsZeroAndSilent(t *testing.T) {
	// A closed port.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	te := newTestEnv(t, startJSON, nil)
	if code := te.run("hook", "session-start", "--port", p); code != 0 || te.out.String()+te.errOut.String() != "" {
		t.Fatalf("closed port: exit %d, output %q %q", code, te.out.String(), te.errOut.String())
	}

	// Bad JSON on stdin: nothing is posted.
	hs := newHookServer(t, nil)
	te = newTestEnv(t, "not json", nil)
	if code := te.run("hook", "session-start", "--port", strconv.Itoa(hs.port)); code != 0 || len(hs.posts()) != 0 {
		t.Fatalf("bad stdin: exit %d, posts %q", code, hs.posts())
	}

	// A bad flag: still silent and zero.
	te = newTestEnv(t, startJSON, nil)
	if code := te.run("hook", "session-start", "--nonsense"); code != 0 || te.out.String()+te.errOut.String() != "" {
		t.Fatalf("bad flag: exit %d, output %q %q", code, te.out.String(), te.errOut.String())
	}

	// A daemon that never answers: the forwarder gives up after about a second.
	release := make(chan struct{})
	slow := newHookServer(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	te = newTestEnv(t, startJSON, nil)
	start := time.Now()
	if code := te.run("hook", "session-start", "--port", strconv.Itoa(slow.port)); code != 0 {
		t.Fatalf("slow daemon: exit %d", code)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("forwarder took %v", took)
	}

	// Stdin that never ends.
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	te = newTestEnv(t, "", nil)
	te.Stdin = pr
	start = time.Now()
	if code := te.run("hook", "session-start", "--port", strconv.Itoa(hs.port)); code != 0 {
		t.Fatalf("open stdin: exit %d", code)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("forwarder waited %v on stdin", took)
	}

	// Other hook names are a usage error.
	te = newTestEnv(t, "", nil)
	if code := te.run("hook", "other"); code != exitUsage {
		t.Fatalf("hook other: %d", code)
	}
}

const sampleSettings = `{
  "model": "opus",
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "keep.sh"
          }
        ]
      }
    ]
  }
}
`

func TestInstallAndUninstallHooks(t *testing.T) {
	te := newTestEnv(t, "", nil)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(sampleSettings), 0o600); err != nil {
		t.Fatal(err)
	}

	// A dry run shows the change and writes nothing.
	if code := te.run("install-hooks", "--settings", path, "--dry-run"); code != exitOK {
		t.Fatalf("dry run: %d %q", code, te.errOut.String())
	}
	if !strings.Contains(te.out.String(), "+ ") || !strings.Contains(te.out.String(), "/cm2d/v1/permission") {
		t.Fatalf("dry-run output:\n%s", te.out.String())
	}
	if got, _ := os.ReadFile(path); string(got) != sampleSettings {
		t.Fatal("dry run wrote the file")
	}
	if m, _ := filepath.Glob(path + ".cm2d-bak-*"); len(m) != 0 {
		t.Fatalf("dry run made a backup: %v", m)
	}

	te = newTestEnv(t, "", nil)
	if code := te.run("install-hooks", "--settings", path); code != exitOK {
		t.Fatalf("install: %d %q", code, te.errOut.String())
	}
	got, _ := os.ReadFile(path)
	for _, want := range []string{"http://127.0.0.1:47731/cm2d/v1/event", "http://127.0.0.1:47731/cm2d/v1/permission",
		"/opt/cm2d/cm2d hook session-start", "keep.sh", `"model": "opus"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("settings lack %q", want)
		}
	}
	if m, _ := filepath.Glob(path + ".cm2d-bak-*"); len(m) != 1 {
		t.Fatalf("backups = %v", m)
	}
	if !strings.Contains(te.out.String(), "backup of the old file") || strings.Contains(te.out.String(), "CM2D_PORT") {
		t.Fatalf("install output: %q", te.out.String())
	}

	// A second run changes nothing.
	te = newTestEnv(t, "", nil)
	if code := te.run("install-hooks", "--settings", path); code != exitOK || !strings.Contains(te.out.String(), "nothing to change") {
		t.Fatalf("second install: %d %q", code, te.out.String())
	}
	again, _ := os.ReadFile(path)
	if !bytes.Equal(got, again) {
		t.Fatal("second install changed the file")
	}

	// Uninstall dry run, then for real: the original comes back.
	te = newTestEnv(t, "", nil)
	if code := te.run("uninstall-hooks", "--settings", path, "--dry-run"); code != exitOK || !strings.Contains(te.out.String(), "- ") {
		t.Fatalf("uninstall dry run: %d %q", code, te.out.String())
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, got) {
		t.Fatal("uninstall dry run wrote the file")
	}
	te = newTestEnv(t, "", nil)
	if code := te.run("uninstall-hooks", "--settings", path); code != exitOK {
		t.Fatalf("uninstall: %d %q", code, te.errOut.String())
	}
	if now, _ := os.ReadFile(path); string(now) != sampleSettings {
		t.Fatalf("uninstall did not restore the file:\n%s", now)
	}
	te = newTestEnv(t, "", nil)
	if code := te.run("uninstall-hooks", "--settings", path); code != exitOK || !strings.Contains(te.out.String(), "nothing to change") {
		t.Fatalf("second uninstall: %d %q", code, te.out.String())
	}
}

func TestInstallHooksCustomPortAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	te := newTestEnv(t, "", nil)
	if code := te.run("install-hooks", "--settings", path, "--port", "50123"); code != exitOK {
		t.Fatalf("install: %d %q", code, te.errOut.String())
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), ":50123/cm2d/v1/event") {
		t.Fatalf("settings:\n%s", got)
	}
	// The SessionStart command carries the port, so no CM2D_PORT note is needed.
	if !strings.Contains(string(got), "/opt/cm2d/cm2d hook session-start --port 50123") || strings.Contains(te.out.String(), "CM2D_PORT") {
		t.Fatalf("SessionStart has no port: out %q\nsettings:\n%s", te.out.String(), got)
	}

	te = newTestEnv(t, "", nil)
	if code := te.run("install-hooks", "--settings", path, "--port", "0"); code != exitUsage {
		t.Fatalf("port 0: %d", code)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o600)
	te = newTestEnv(t, "", nil)
	if code := te.run("install-hooks", "--settings", bad); code != exitError {
		t.Fatalf("bad settings: %d", code)
	}
	if now, _ := os.ReadFile(bad); string(now) != "{not json" {
		t.Fatal("a bad settings file was changed")
	}
}

func TestInstallHooksDefaultPathUsesHome(t *testing.T) {
	te := newTestEnv(t, "", nil)
	home, _ := te.Home()
	if code := te.run("install-hooks", "--dry-run"); code != exitOK {
		t.Fatalf("exit %d %q", code, te.errOut.String())
	}
	if !strings.Contains(te.out.String(), filepath.Join(home, ".claude", "settings.json")) {
		t.Fatalf("output: %q", te.out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Fatal("a dry run created ~/.claude")
	}
}

var addrRE = regexp.MustCompile(`addr=(127\.0\.0\.1:\d+)`)

// startRun runs "cm2d run" on a random port and returns its port.
func startRun(t *testing.T, extra ...string) (port string, done <-chan int, cancel func()) {
	t.Helper()
	te := newTestEnv(t, "", nil)
	ctx, cancelCtx := context.WithCancel(context.Background())
	args := append([]string{"run", "--port", "0", "--no-device", "--no-herdr"}, extra...)
	d := make(chan int, 1)
	go func() { d <- run(ctx, args, te.env) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m := addrRE.FindStringSubmatch(te.errOut.String()); m != nil {
			_, port, _ = net.SplitHostPort(m[1])
			t.Cleanup(cancelCtx)
			return port, d, cancelCtx
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancelCtx()
	t.Fatalf("run did not report its address: %q", te.errOut.String())
	return
}

func postJSON(t *testing.T, url string, header map[string]string, body any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST %s: %d", url, resp.StatusCode)
	}
}

func TestRunAndStatus(t *testing.T) {
	port, done, cancel := startRun(t)
	base := "http://127.0.0.1:" + port + "/cm2d/v1"
	postJSON(t, base+"/session", nil, map[string]any{
		"payload":  map[string]string{"session_id": "s1", "cwd": "/w/alpha", "hook_event_name": "SessionStart", "source": "startup"},
		"attended": "1", "entrypoint": "cli",
	})
	postJSON(t, base+"/event", map[string]string{"X-Cm2-Entrypoint": "cli"},
		map[string]string{"session_id": "s1", "cwd": "/w/alpha", "hook_event_name": "UserPromptSubmit"})

	te := newTestEnv(t, "", nil)
	if code := te.run("status", "--port", port); code != exitOK {
		t.Fatalf("status: %d %q", code, te.errOut.String())
	}
	out := te.out.String()
	for _, want := range []string{"cm2d " + version, "pad: off (--no-device)", "herdr: not watched", "alpha", "working", "claude:s1", "terminal"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	te = newTestEnv(t, "", nil)
	if code := te.run("status", "--port", port, "--json"); code != exitOK {
		t.Fatalf("status --json: %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(te.out.String()), &doc); err != nil || doc["version"] != version {
		t.Fatalf("json status: %v %q", err, te.out.String())
	}

	// SIGINT is a context cancel here: a clean exit is code 0.
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("run exited %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	te = newTestEnv(t, "", nil)
	if code := te.run("status", "--port", port); code != exitError || !strings.Contains(te.errOut.String(), "no daemon answered") {
		t.Fatalf("status after stop: %d %q", code, te.errOut.String())
	}
}

func TestRunFailsWhenThePortIsTaken(t *testing.T) {
	port, _, cancel := startRun(t)
	defer cancel()
	te := newTestEnv(t, "", nil)
	if code := te.run("run", "--port", port, "--no-device", "--no-herdr"); code != exitError || !strings.Contains(te.errOut.String(), "listen") {
		t.Fatalf("second run: %d %q", code, te.errOut.String())
	}
}

func TestRunWatchesHerdr(t *testing.T) {
	srv := fakeWithAgents(t)
	te := newTestEnv(t, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"run", "--port", "0", "--no-device", "--herdr-socket", srv.Path()}, te.env)
	}()
	var port string
	eventually(t, func() bool {
		m := addrRE.FindStringSubmatch(te.errOut.String())
		if m == nil {
			return false
		}
		_, port, _ = net.SplitHostPort(m[1])
		return true
	})
	eventually(t, func() bool {
		st := newTestEnv(t, "", nil)
		return st.run("status", "--port", port) == exitOK && strings.Contains(st.out.String(), "herdr: 0.9.3, 2 agents")
	})
	st := newTestEnv(t, "", nil)
	st.run("status", "--port", port)
	if !strings.Contains(st.out.String(), "herdr:w1:p2") {
		t.Fatalf("the Herdr agent is not listed:\n%s", st.out.String())
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("run exited %d", code)
	}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestRunUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--bogus"},
		{"run", "extra"},
		{"run", "--port", "70000"},
		{"status", "extra"},
		{"install-hooks", "extra"},
		{"uninstall-hooks", "extra"},
		{"setup", "extra"},
		{"restore"},
	} {
		te := newTestEnv(t, "", nil)
		if code := te.run(args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

// ---- setup -----------------------------------------------------------------

const padKeymap = `{"version":1,"activeProfileId":0,"profiles":[{"id":0,"layers":[
{"id":0,"name":"Base","layout":{"keymap":[["KC_A","KC_B"],["KC_C","KC_D","KC_E","KC_F"],["KC_G","KC_H","KC_I","KC_J"],["KC_NONE","KC_L","KC_M"]],"encoders":[["KC_VOLU","KC_VOLD","KC_MPLY"]]}},
{"id":1,"layout":{"keymap":[["KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE"]],"encoders":[["KC_NONE","KC_NONE","KC_NONE"]]}}]}]}`

const testLayout = `{"layers":[
 {"layer":0,"encoders":[[null,null,"KI_LS2"]]},
 {"layer":1,"keymap":[["KV_OAI_AG00","KV_OAI_AG01"],["KV_OAI_AG02","KV_OAI_AG03","KV_OAI_AG04","KV_OAI_AG05"],["KV_OAI_AG06","KV_OAI_AG07","KV_OAI_AG08","KV_OAI_AG09"],["KV_OAI_AG10","KV_OAI_AG11","KV_OAI_AG12"]],
  "encoders":[["KV_OAI_ENC_CW","KV_OAI_ENC_CC","KI_LS1"]]}]}`

// withFakePad makes the setup commands use an in-memory pad. It returns a
// func that gives the current keymap file and the write count.
func withFakePad(t *testing.T) func() (string, int) {
	t.Helper()
	var mu sync.Mutex
	file, writes := padKeymap, 0
	onMessage := func(req []byte) [][]byte {
		var r struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(req, &r)
		mu.Lock()
		defer mu.Unlock()
		result := `null`
		switch r.Method {
		case "fs.read":
			q, _ := json.Marshal(file)
			result = `{"data":` + string(q) + `}`
		case "fs.write":
			var w struct{ File, Data string }
			json.Unmarshal(r.Params, &w)
			file = w.Data
			writes++
		}
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":%s}`, r.ID, result))}
	}
	oldOpen, oldAccess := openPad, padAccess
	// Each open gets a new fake: closing the client closes its transport.
	openPad = func() (device.Transport, error) {
		f := devicetest.New()
		f.OnMessage = onMessage
		return f, nil
	}
	padAccess = func() device.AccessState { return device.AccessGranted }
	t.Cleanup(func() { openPad, padAccess = oldOpen, oldAccess })
	return func() (string, int) {
		mu.Lock()
		defer mu.Unlock()
		return file, writes
	}
}

func TestSetupDryRunApplyAndRestore(t *testing.T) {
	state := withFakePad(t)
	dir := t.TempDir()
	layout := filepath.Join(dir, "layout.json")
	os.WriteFile(layout, []byte(testLayout), 0o600)
	// A port nobody listens on, so the "daemon running" check passes.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()

	te := newTestEnv(t, "", nil)
	if code := te.run("setup", "--layout", layout, "--dry-run", "--port", p, "--backup-dir", dir); code != exitOK {
		t.Fatalf("dry run: %d %q", code, te.errOut.String())
	}
	if _, w := state(); w != 0 || !strings.Contains(te.out.String(), "dry run: nothing written") {
		t.Fatalf("dry run wrote (%d) or said %q", w, te.out.String())
	}

	te = newTestEnv(t, "", nil)
	if code := te.run("setup", "--layout", layout, "--port", p, "--backup-dir", dir); code != exitOK {
		t.Fatalf("setup: %d %q", code, te.errOut.String())
	}
	file, w := state()
	if w != 1 || !strings.Contains(file, "KV_OAI_AG12") {
		t.Fatalf("writes %d, file %s", w, file)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "*"))
	var backup string
	for _, b := range backups {
		if b != layout {
			backup = b
		}
	}
	if backup == "" {
		t.Fatalf("no backup in %v", backups)
	}

	te = newTestEnv(t, "", nil)
	if code := te.run("restore", backup, "--port", p, "--backup-dir", dir); code != exitOK {
		t.Fatalf("restore: %d %q", code, te.errOut.String())
	}
	if file, _ := state(); strings.Contains(file, "KV_OAI_AG12") {
		t.Fatal("restore did not put the old keymap back")
	}
}

func TestSetupRefusesWhileADaemonRuns(t *testing.T) {
	state := withFakePad(t)
	port, _, cancel := startRun(t)
	defer cancel()
	layout := filepath.Join(t.TempDir(), "layout.json")
	os.WriteFile(layout, []byte(testLayout), 0o600)
	te := newTestEnv(t, "", nil)
	if code := te.run("setup", "--layout", layout, "--port", port); code != exitError || !strings.Contains(te.errOut.String(), "Stop it first") {
		t.Fatalf("setup: %d %q", code, te.errOut.String())
	}
	if _, w := state(); w != 0 {
		t.Fatal("setup wrote to the pad while a daemon ran")
	}
}

func TestSetupErrors(t *testing.T) {
	withFakePad(t)
	te := newTestEnv(t, "", nil)
	if code := te.run("setup", "--layout", filepath.Join(t.TempDir(), "missing.json")); code != exitError {
		t.Fatalf("missing layout: %d", code)
	}
	padAccess = func() device.AccessState { return device.AccessDenied }
	layout := filepath.Join(t.TempDir(), "layout.json")
	os.WriteFile(layout, []byte(testLayout), 0o600)
	te = newTestEnv(t, "", nil)
	if code := te.run("setup", "--layout", layout, "--port", "1"); code != exitError || !strings.Contains(te.errOut.String(), "Input Monitoring") {
		t.Fatalf("denied: %d %q", code, te.errOut.String())
	}
}

// The port with no flag in the SessionStart command must be the daemon default.
func TestDefaultPortsAgree(t *testing.T) {
	if defaultHooksPort != hookcfg.DefaultPort {
		t.Fatalf("defaultHooksPort %d != hookcfg.DefaultPort %d", defaultHooksPort, hookcfg.DefaultPort)
	}
}

// The shipped binary must wire the focuser: a key press brings Ghostty
// forward, and a Herdr pane gets the Herdr focus first.
func TestRunConfigWiring(t *testing.T) {
	te := newTestEnv(t, "", nil)
	cfg := runConfig(te.env, 47731, false, false, "/tmp/herdr-test.sock")
	if cfg.Addr != "127.0.0.1:47731" || cfg.NoDevice || cfg.Version != version {
		t.Fatalf("cfg = %+v", cfg)
	}
	f := cfg.Focuser
	if f == nil || f.Run == nil || f.TerminalApp != "Ghostty" {
		t.Fatalf("Focuser = %+v", f)
	}
	if cfg.Herdr == nil || f.Herdr == nil {
		t.Fatalf("Herdr client %v, focuser Herdr %v", cfg.Herdr, f.Herdr)
	}

	cfg = runConfig(te.env, 5000, true, true, "")
	if cfg.Herdr != nil || cfg.Focuser == nil || cfg.Focuser.Herdr != nil || !cfg.NoDevice {
		t.Fatalf("--no-device --no-herdr: %+v focuser %+v", cfg, cfg.Focuser)
	}
	if cfg.Focuser.Run == nil || cfg.Focuser.TerminalApp != "Ghostty" {
		t.Fatalf("Focuser = %+v", cfg.Focuser)
	}
}

// Without --layout, setup reads config/layout.json.
func TestSetupDefaultLayoutPath(t *testing.T) {
	withFakePad(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "layout.json"), []byte(testLayout), 0o600); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	te := newTestEnv(t, "", nil)
	if code := te.run("setup", "--dry-run", "--port", p, "--backup-dir", dir); code != exitOK {
		t.Fatalf("setup with the default layout: %d %q", code, te.errOut.String())
	}
}

// A daemon that cannot answer gives a 503 with a reason, and the command says so.
func TestStatusShowsTheDaemonsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon is busy", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	te := newTestEnv(t, "", nil)
	if code := te.run("status", "--port", port); code != exitError || !strings.Contains(te.errOut.String(), "daemon is busy") || te.out.String() != "" {
		t.Fatalf("status: %d out %q err %q", code, te.out.String(), te.errOut.String())
	}
}

func TestPrintStatusShowsHostSession(t *testing.T) {
	doc := daemon.StatusDoc{Version: "t", Selected: -1, Sessions: []daemon.SessionStatus{
		{Key: "claude:a", Kind: "claude", Frontend: "desktop", Label: "alpha", State: "idle", Slot: 0, HostSession: "local_abc-1"},
		{Key: "claude:b", Kind: "claude", Frontend: "desktop", Label: "beta", State: "idle", Slot: 1, HostSession: "local_def-2", HostConflict: true},
		{Key: "claude:c", Kind: "claude", Frontend: "terminal", Label: "gamma", State: "idle", Slot: 2},
	}}
	var buf bytes.Buffer
	printStatus(&buf, doc)
	lines := strings.Split(buf.String(), "\n")
	find := func(label string) string {
		for _, l := range lines {
			if strings.Contains(l, label) {
				return l
			}
		}
		t.Fatalf("no line for %s:\n%s", label, buf.String())
		return ""
	}
	if !strings.Contains(buf.String(), "HOST") {
		t.Fatalf("no HOST column:\n%s", buf.String())
	}
	if l := find("alpha"); !strings.Contains(l, "local_abc-1") || strings.Contains(l, "conflict") {
		t.Errorf("alpha: %q", l)
	}
	if l := find("beta"); !strings.Contains(l, "local_def-2") || !strings.Contains(l, "conflict") {
		t.Errorf("beta: %q", l)
	}
	if l := find("gamma"); strings.Contains(l, "local_") || strings.Contains(l, "conflict") {
		t.Errorf("gamma: %q", l)
	}
}
