package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cm2-claude/internal/device"
	"cm2-claude/internal/device/devicetest"
)

type harness struct {
	t      *testing.T
	out    bytes.Buffer
	errb   bytes.Buffer
	fake   *devicetest.Fake
	opens  int
	access device.AccessState
	opts   device.OpenOptions
	env    *Env
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, fake: devicetest.New(), access: device.AccessGranted}
	h.env = &Env{
		Stdout: &h.out,
		Stderr: &h.errb,
		Open: func(o device.OpenOptions) (device.Transport, error) {
			h.opens++
			h.opts = o
			return h.fake, nil
		},
		Access:        func() device.AccessState { return h.access },
		RequestAccess: func() bool { t.Fatal("RequestAccess must not be called without --request"); return false },
		Devices:       func() ([]device.DeviceInfo, error) { return nil, errors.New("not used") },
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.out.Reset()
	h.errb.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Run(ctx, args, h.env)
}

// reply makes the fake answer every request with result(method, params).
func (h *harness) reply(result func(method string, params json.RawMessage) string) {
	h.fake.OnMessage = func(req []byte) [][]byte {
		var r struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(req, &r); err != nil {
			h.t.Errorf("bad request %q", req)
			return nil
		}
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":%s}`, r.ID, result(r.Method, r.Params)))}
	}
}

func TestUsageAndUnknown(t *testing.T) {
	h := newHarness(t)
	if c := h.run(); c != exitUsage {
		t.Fatalf("no args: %d", c)
	}
	if c := h.run("frobnicate"); c != exitUsage {
		t.Fatalf("unknown: %d", c)
	}
	if c := h.run("version"); c != exitOK || !strings.Contains(h.out.String(), "cm2probe 0.1.0") {
		t.Fatalf("version: %d %q", c, h.out.String())
	}
	if h.opens != 0 {
		t.Fatal("opened the device")
	}
}

func TestCallGuardRefusesBeforeOpening(t *testing.T) {
	for _, m := range []string{"fs.write", "sys.bootloader", "sys.reset", "sys.reboot", "sys.factory_reset", "fs.delete", "fs.rm", "fs.writebin", "x.erase", "x.flash", "x.boot", "x.reset", "x.write"} {
		h := newHarness(t)
		if c := h.run("call", m, `{"a":1}`); c != exitRefused {
			t.Errorf("%s: exit %d", m, c)
		}
		if h.opens != 0 || len(h.fake.Frames()) != 0 {
			t.Errorf("%s: touched the device", m)
		}
		e := h.errb.String()
		if !strings.Contains(e, "refused") || !strings.Contains(e, "fs.write") || !strings.Contains(e, "--allow-write") {
			t.Errorf("%s: refusal text: %q", m, e)
		}
	}
}

func TestCallAllowWrite(t *testing.T) {
	h := newHarness(t)
	h.reply(func(m string, p json.RawMessage) string { return `"ok"` })
	if c := h.run("call", "fs.write", `{"file":"x"}`, "--allow-write"); c != exitOK {
		t.Fatalf("exit %d: %s", c, h.errb.String())
	}
	if !strings.Contains(h.errb.String(), "WARNING") || h.opens != 1 {
		t.Fatalf("stderr %q opens %d", h.errb.String(), h.opens)
	}
}

func TestCallVolatileAndReadAllowed(t *testing.T) {
	echo := func(m string, p json.RawMessage) string { return `{"m":"` + m + `","p":` + string(p) + `}` }
	for _, m := range []string{"v.oai.thstatus", "v.oai.rgbcfg", "lights.preview", "device.status", "sys.version"} {
		h := newHarness(t) // the fake is closed after each run
		h.reply(echo)
		if c := h.run("call", m, "--timeout", "1s"); c != exitOK {
			t.Fatalf("%s: exit %d: %s", m, c, h.errb.String())
		}
		want := `{"m":"` + m + `","p":null}`
		if got := strings.TrimSpace(h.out.String()); got != want {
			t.Fatalf("%s: stdout %q", m, got)
		}
	}
	h := newHarness(t)
	if c := h.run("--timeout=1s", "call"); c != exitUsage { // flags before the command are not supported
		t.Fatalf("exit %d", c)
	}
}

func TestCallParamsAndFlagsAnywhere(t *testing.T) {
	h := newHarness(t)
	h.reply(func(m string, p json.RawMessage) string { return string(p) })
	if c := h.run("call", "--timeout", "1s", "v.oai.thstatus", `[{"id":0,"c":255}]`); c != exitOK {
		t.Fatalf("exit %d: %s", c, h.errb.String())
	}
	if got := strings.TrimSpace(h.out.String()); got != `[{"id":0,"c":255}]` {
		t.Fatalf("stdout %q", got)
	}
	if c := h.run("call", "device.status", `{bad`); c != exitUsage {
		t.Fatalf("bad params exit %d", c)
	}
}

func TestCallOpenFlags(t *testing.T) {
	h := newHarness(t)
	h.reply(func(m string, p json.RawMessage) string { return `1` })
	if c := h.run("call", "device.status", "--in-id", "absent", "--out-id", "absent", "--newline", "--id-first"); c != exitOK {
		t.Fatalf("exit %d: %s", c, h.errb.String())
	}
	if h.opts.InputID != device.IDAbsent || h.opts.OutputID != device.IDAbsent {
		t.Fatalf("opts %+v", h.opts)
	}
	req := string(h.fake.Requests()[0])
	if !strings.HasPrefix(req, `{"id":`) {
		t.Fatalf("request %q", req)
	}
	if c := h.run("call", "device.status", "--out-id", "auto"); c != exitUsage {
		t.Fatalf("out-id auto should be a usage error, got %d", c)
	}
}

func TestCallTimeoutAndDeviceError(t *testing.T) {
	h := newHarness(t) // no answers
	if c := h.run("call", "device.status", "--timeout", "40ms"); c != exitError {
		t.Fatalf("exit %d", c)
	}
	if !strings.Contains(h.errb.String(), "no reply") {
		t.Fatalf("stderr %q", h.errb.String())
	}
	h = newHarness(t)
	h.fake.OnMessage = func(req []byte) [][]byte {
		var r struct{ ID int64 }
		_ = json.Unmarshal(req, &r)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"error":{"code":5,"message":"nope"}}`, r.ID))}
	}
	if c := h.run("call", "device.status", "--timeout", "1s"); c != exitError || !strings.Contains(h.errb.String(), "nope") {
		t.Fatalf("exit %d stderr %q", c, h.errb.String())
	}
}

func TestForeignRepliesWarning(t *testing.T) {
	h := newHarness(t)
	h.fake.OnMessage = func(req []byte) [][]byte {
		var r struct{ ID int64 }
		_ = json.Unmarshal(req, &r)
		return [][]byte{[]byte(`{"id":2,"result":"other host"}`), []byte(fmt.Sprintf(`{"id":%d,"result":true}`, r.ID))}
	}
	if c := h.run("call", "device.status"); c != exitOK {
		t.Fatalf("exit %d", c)
	}
	if !strings.Contains(h.errb.String(), "Another program is probably talking to the pad") {
		t.Fatalf("stderr %q", h.errb.String())
	}
}

func TestAccessDeniedStopsBeforeOpen(t *testing.T) {
	h := newHarness(t)
	h.access = device.AccessDenied
	for _, args := range [][]string{{"call", "device.status"}, {"listen", "1"}, {"backup", filepath.Join(t.TempDir(), "k.json")}} {
		if c := h.run(args...); c != exitDenied {
			t.Errorf("%v: exit %d", args, c)
		}
		if !strings.Contains(h.errb.String(), "access: input monitoring denied") || !strings.Contains(h.errb.String(), "System Settings") {
			t.Errorf("%v: stderr %q", args, h.errb.String())
		}
	}
	if h.opens != 0 {
		t.Fatal("opened despite denied")
	}
}

func TestAccessUnknownPrintsHelpAndProceeds(t *testing.T) {
	h := newHarness(t)
	h.access = device.AccessUnknown
	h.reply(func(m string, p json.RawMessage) string { return `1` })
	if c := h.run("call", "device.status"); c != exitOK {
		t.Fatalf("exit %d", c)
	}
	if !strings.Contains(h.errb.String(), "input monitoring unknown") || !strings.Contains(h.errb.String(), "System Settings") {
		t.Fatalf("stderr %q", h.errb.String())
	}
}

func TestOpenErrorInputMonitoring(t *testing.T) {
	h := newHarness(t)
	h.env.Open = func(device.OpenOptions) (device.Transport, error) {
		return nil, fmt.Errorf("%w (test)", device.ErrInputMonitoring)
	}
	if c := h.run("call", "device.status"); c != exitDenied || !strings.Contains(h.errb.String(), "Input Monitoring not granted") {
		t.Fatalf("exit %d stderr %q", c, h.errb.String())
	}
}

func TestAccessCommand(t *testing.T) {
	h := newHarness(t)
	if c := h.run("access"); c != exitOK || strings.TrimSpace(h.out.String()) != "input monitoring: granted" {
		t.Fatalf("exit %d out %q", c, h.out.String())
	}
	h.access = device.AccessDenied
	if c := h.run("access", "--json"); c != exitDenied || !strings.Contains(h.out.String(), `"denied"`) {
		t.Fatalf("exit %d out %q", c, h.out.String())
	}
	requested := false
	h.env.RequestAccess = func() bool { requested = true; return true }
	h.run("access", "--request")
	if !requested {
		t.Fatal("--request did not call RequestAccess")
	}
}

func TestDevicesCommand(t *testing.T) {
	h := newHarness(t)
	h.env.Devices = func() ([]device.DeviceInfo, error) {
		return []device.DeviceInfo{{VendorID: 0x303a, ProductID: 0x8298, Product: "Creator Micro 2", PrimaryUsagePage: 0xff00,
			UsagePairs: []device.UsagePair{{Page: 0xff00, Usage: 1}}, HasVendorCollection: true, MaxInputReportSize: 64, MaxOutputReportSize: 64,
			Descriptor: []byte{0x06, 0x00, 0xff}}}, nil
	}
	if c := h.run("devices", "--descriptor"); c != exitOK || !strings.Contains(h.out.String(), "0600ff") || !strings.Contains(h.out.String(), "0xFF00") {
		t.Fatalf("exit %d out %q", c, h.out.String())
	}
	if c := h.run("devices", "--json"); c != exitOK {
		t.Fatal(c)
	}
	var list []map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &list); err != nil || len(list) != 1 || list[0]["has_vendor_collection"] != true {
		t.Fatalf("json: %v %s", err, h.out.String())
	}
	h.env.Devices = func() ([]device.DeviceInfo, error) { return nil, nil }
	if c := h.run("devices"); c != exitError {
		t.Fatalf("no devices should exit 1, got %d", c)
	}
	if h.opens != 0 {
		t.Fatal("devices opened the pad")
	}
}

func TestListen(t *testing.T) {
	h := newHarness(t)
	h.fake.Inject([]byte(`{"m":"v.oai.hid","p":{"k":"AG00","act":1}}`))
	h.fake.InjectChannel(device.ChannelDebug, []byte("fw log\n"))
	if c := h.run("listen", "0.15"); c != exitOK {
		t.Fatalf("exit %d: %s", c, h.errb.String())
	}
	o := h.out.String()
	if !strings.Contains(o, `notify v.oai.hid {"k":"AG00","act":1}`) || !strings.Contains(o, "debug fw log") {
		t.Fatalf("stdout %q", o)
	}
	if n := len(h.fake.Frames()); n != 0 {
		t.Fatalf("listen wrote %d frames", n)
	}
	for _, bad := range []string{"0", "-1", "abc", "99999"} {
		if c := h.run("listen", bad); c != exitUsage {
			t.Errorf("listen %s: exit %d", bad, c)
		}
	}
}

func fsReadResponder(h *harness, shape string, file string) {
	h.reply(func(m string, p json.RawMessage) string {
		if m != "fs.read" || string(p) != `{"file":"keymap.json"}` {
			h.t.Errorf("unexpected request %s %s", m, p)
		}
		q, _ := json.Marshal(file)
		switch shape {
		case "string":
			return string(q)
		case "data":
			return `{"data":` + string(q) + `}`
		case "object":
			return `{"data":` + file + `}`
		}
		return `null`
	})
}

func TestBackupShapes(t *testing.T) {
	const keymap = `{"layers":[{"k":["A","B"]}],"name":"é"}`
	for _, shape := range []string{"string", "data", "object"} {
		h := newHarness(t)
		fsReadResponder(h, shape, keymap)
		dir := t.TempDir()
		path := filepath.Join(dir, "keymap.json")
		if c := h.run("backup", path); c != exitOK {
			t.Fatalf("%s: exit %d: %s", shape, c, h.errb.String())
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != keymap {
			t.Fatalf("%s: file %q err %v", shape, got, err)
		}
		raw, err := os.ReadFile(path + ".raw.json")
		if err != nil || !json.Valid(raw) || !strings.Contains(string(raw), `"id"`) {
			t.Fatalf("%s: raw %q err %v", shape, raw, err)
		}
		sum := sha256.Sum256([]byte(keymap))
		if !strings.Contains(h.out.String(), hex.EncodeToString(sum[:])) {
			t.Fatalf("%s: stdout %q", shape, h.out.String())
		}
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode())
		}
	}
}

func TestBackupRefusesToOverwrite(t *testing.T) {
	h := newHarness(t)
	fsReadResponder(h, "string", `{"a":1}`)
	path := filepath.Join(t.TempDir(), "keymap.json")
	if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := h.run("backup", path); c != exitError {
		t.Fatalf("exit %d", c)
	}
	if h.opens != 0 {
		t.Fatal("opened the pad before checking the target")
	}
	if b, _ := os.ReadFile(path); string(b) != "precious" {
		t.Fatal("existing file damaged")
	}
	if c := h.run("backup", path, "--force"); c != exitOK {
		t.Fatalf("--force exit %d: %s", c, h.errb.String())
	}
	if b, _ := os.ReadFile(path); string(b) != `{"a":1}` {
		t.Fatalf("file %q", b)
	}
}

func TestBackupInvalidJSONIsNotSavedAsBackup(t *testing.T) {
	h := newHarness(t)
	fsReadResponder(h, "string", `not json`)
	path := filepath.Join(t.TempDir(), "keymap.json")
	if c := h.run("backup", path); c != exitError {
		t.Fatalf("exit %d", c)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("invalid data saved under the backup name")
	}
	if b, err := os.ReadFile(path + ".invalid"); err != nil || string(b) != "not json" {
		t.Fatalf("invalid file: %q %v", b, err)
	}
	if _, err := os.Stat(path + ".raw.json"); err != nil {
		t.Fatal("raw reply not saved")
	}
}

func TestBackupBadReplies(t *testing.T) {
	for _, shape := range []string{"null", "empty"} {
		h := newHarness(t)
		if shape == "null" {
			fsReadResponder(h, "null", "")
		} else {
			fsReadResponder(h, "string", "")
		}
		path := filepath.Join(t.TempDir(), "keymap.json")
		if c := h.run("backup", path); c != exitError {
			t.Errorf("%s: exit %d", shape, c)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s: backup file written", shape)
		}
	}
}

func TestBackupOnlyReads(t *testing.T) {
	h := newHarness(t)
	fsReadResponder(h, "string", `{"a":1}`)
	if c := h.run("backup", filepath.Join(t.TempDir(), "k.json")); c != exitOK {
		t.Fatal(c)
	}
	for _, r := range h.fake.Requests() {
		var q struct{ Method string }
		_ = json.Unmarshal(r, &q)
		if q.Method != "fs.read" {
			t.Fatalf("backup sent %s", q.Method)
		}
		if refused, _ := device.GuardMethod(q.Method); refused {
			t.Fatalf("backup sent a method the guard refuses: %s", q.Method)
		}
	}
}
