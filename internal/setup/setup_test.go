package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cm2-claude/internal/device"
	"cm2-claude/internal/device/devicetest"
)

const padKeymap = `{"version":1,"activeProfileId":0,"profiles":[{"id":0,"layers":[
{"id":0,"name":"Base","layout":{"keymap":[["KC_A","KC_B"],["KC_C","KC_D","KC_E","KC_F"],["KC_G","KC_H","KC_I","KC_J"],["KC_NONE","KC_L","KC_M"]],"encoders":[["KC_VOLU","KC_VOLD","KC_MPLY"]]}},
{"id":1,"layout":{"keymap":[["KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE"]],"encoders":[["KC_NONE","KC_NONE","KC_NONE"]]}}]}]}`

const testLayout = `{"layers":[
 {"layer":0,"encoders":[[null,null,"KI_LS2"]]},
 {"layer":1,"keymap":[["KV_OAI_AG00","KV_OAI_AG01"],["KV_OAI_AG02","KV_OAI_AG03","KV_OAI_AG04","KV_OAI_AG05"],["KV_OAI_AG06","KV_OAI_AG07","KV_OAI_AG08","KV_OAI_AG09"],["KV_OAI_AG10","KV_OAI_AG11","KV_OAI_AG12"]],
  "encoders":[["KV_OAI_ENC_CW","KV_OAI_ENC_CC","KI_LS1"]]}]}`

// fakePad answers fs.read and fs.write from an in-memory keymap.json.
type fakePad struct {
	mu        sync.Mutex
	file      string
	writes    int
	dropWrite bool   // accept a write but keep the old file
	failWrite bool   // answer fs.write with an error
	rawRead   string // if set, the fs.read result
}

func (p *fakePad) client(t *testing.T) *device.Client {
	t.Helper()
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		var r struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(req, &r); err != nil {
			t.Errorf("bad request %q", req)
			return nil
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		result := `null`
		switch r.Method {
		case "fs.read":
			if p.rawRead != "" {
				result = p.rawRead
			} else {
				q, _ := json.Marshal(p.file)
				result = `{"data":` + string(q) + `}`
			}
		case "fs.write":
			if p.failWrite {
				return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"error":{"code":5,"message":"flash busy"}}`, r.ID))}
			}
			var w struct{ File, Data string }
			if err := json.Unmarshal(r.Params, &w); err != nil || w.File != "keymap.json" {
				t.Errorf("bad fs.write params %s", r.Params)
			}
			if !p.dropWrite {
				p.file = w.Data
			}
			p.writes++
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":%s}`, r.ID, result))}
	}
	c := device.NewClient(f, device.ClientOptions{Timeout: 2 * time.Second})
	t.Cleanup(c.Close)
	return c
}

func layoutOf(t *testing.T, body string) device.LayoutFile {
	t.Helper()
	p := filepath.Join(t.TempDir(), "layout.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLayout(p)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func opts(dir string) Options {
	return Options{Timeout: 2 * time.Second, BackupDir: filepath.Join(dir, "b")}
}

func TestShowPrintsActiveProfileLayers(t *testing.T) {
	pad := &fakePad{file: padKeymap}
	var out bytes.Buffer
	if err := Show(context.Background(), pad.client(t), 2*time.Second, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{`layer 0 "Base"`, "row 0: [KC_A KC_B]", "encoder 0 [cw ccw press]: [KC_VOLU KC_VOLD KC_MPLY]", `layer 1 ""`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if pad.writes != 0 {
		t.Fatal("show wrote to the pad")
	}
}

func TestShowSkipsInactiveProfiles(t *testing.T) {
	doc := `{"activeProfileId":1,"profiles":[{"id":0,"layers":[{"id":7,"name":"Old","layout":{}}]},{"id":1,"layers":[{"id":3,"name":"New","layout":{}}]}]}`
	var out bytes.Buffer
	if err := ShowDoc([]byte(doc), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Old") || !strings.Contains(out.String(), `layer 3 "New"`) {
		t.Fatalf("out %s", out.String())
	}
}

func TestShowReadErrors(t *testing.T) {
	pad := &fakePad{rawRead: `"not json"`}
	err := Show(context.Background(), pad.client(t), 2*time.Second, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("err %v", err)
	}
	pad = &fakePad{rawRead: `{"nodata":1}`}
	if err := Show(context.Background(), pad.client(t), 2*time.Second, &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error for a reply without data")
	}
}

func TestApplyDryRunWritesNothing(t *testing.T) {
	pad := &fakePad{file: padKeymap}
	dir := t.TempDir()
	o := opts(dir)
	o.DryRun = true
	var out bytes.Buffer
	if err := Apply(context.Background(), pad.client(t), layoutOf(t, testLayout), o, &out); err != nil {
		t.Fatal(err)
	}
	if pad.writes != 0 || !strings.Contains(out.String(), "17 key(s) change") || !strings.Contains(out.String(), "dry run: nothing written") {
		t.Fatalf("writes %d, out %s", pad.writes, out.String())
	}
	if _, err := os.Stat(o.BackupDir); err == nil {
		t.Fatal("dry run created a backup folder")
	}
}

func TestApplyBacksUpWritesAndVerifies(t *testing.T) {
	pad := &fakePad{file: padKeymap}
	o := opts(t.TempDir())
	var out bytes.Buffer
	if err := Apply(context.Background(), pad.client(t), layoutOf(t, testLayout), o, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "verified") || !strings.Contains(pad.file, `"KI_LS2"`) || !strings.Contains(pad.file, `"KV_OAI_AG12"`) {
		t.Fatalf("apply did not land: %s\n%s", out.String(), pad.file)
	}
	entries, err := os.ReadDir(o.BackupDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("backups %v err %v", entries, err)
	}
	b, _ := os.ReadFile(filepath.Join(o.BackupDir, entries[0].Name()))
	if string(b) != padKeymap {
		t.Fatal("backup is not the keymap that was on the pad")
	}
	if fi, _ := os.Stat(filepath.Join(o.BackupDir, entries[0].Name())); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", fi.Mode())
	}
}

func TestApplyTwiceWritesOnce(t *testing.T) {
	pad := &fakePad{file: padKeymap}
	o := opts(t.TempDir())
	l := layoutOf(t, testLayout)
	if err := Apply(context.Background(), pad.client(t), l, o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Apply(context.Background(), pad.client(t), l, o, &out); err != nil {
		t.Fatal(err)
	}
	if pad.writes != 1 || !strings.Contains(out.String(), "nothing to write") {
		t.Fatalf("writes %d out %s", pad.writes, out.String())
	}
}

func TestBackupNamesNeverCollide(t *testing.T) {
	pad := &fakePad{file: padKeymap}
	o := opts(t.TempDir())
	o.Now = func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
	l := layoutOf(t, testLayout)
	orig := []byte(padKeymap)
	for i := 0; i < 3; i++ {
		if err := Apply(context.Background(), pad.client(t), l, o, &bytes.Buffer{}); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		if err := Restore(context.Background(), pad.client(t), orig, o, &bytes.Buffer{}); err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
		if !device.SameKeymap([]byte(pad.file), orig) {
			t.Fatalf("restore %d: pad differs from the backup", i)
		}
	}
	entries, err := os.ReadDir(o.BackupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != pad.writes || pad.writes != 6 {
		t.Fatalf("%d backups for %d writes", len(entries), pad.writes)
	}
	if _, err := os.Stat(filepath.Join(o.BackupDir, "keymap-20261023-120000.json")); err != nil {
		t.Fatalf("first backup name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.BackupDir, "keymap-20261023-120000-2.json")); err != nil {
		t.Fatalf("second backup name: %v", err)
	}
}

func TestApplyRefusesShapeMismatch(t *testing.T) {
	pad := &fakePad{file: padKeymap}
	bad := layoutOf(t, `{"layers":[{"layer":1,"keymap":[["KV_OAI_AG00"]]}]}`)
	err := Apply(context.Background(), pad.client(t), bad, opts(t.TempDir()), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "keymap apply") {
		t.Fatalf("err %v", err)
	}
	if pad.writes != 0 {
		t.Fatal("wrote despite a bad layout")
	}
}

func TestRestoreSameKeymapWritesNothing(t *testing.T) {
	pad := &fakePad{file: padKeymap}
	var out bytes.Buffer
	if err := Restore(context.Background(), pad.client(t), []byte(padKeymap), opts(t.TempDir()), &out); err != nil {
		t.Fatal(err)
	}
	if pad.writes != 0 || !strings.Contains(out.String(), "already holds this keymap") {
		t.Fatalf("writes %d out %s", pad.writes, out.String())
	}
}

func TestRestoreDryRun(t *testing.T) {
	pad := &fakePad{file: padKeymap}
	other := strings.Replace(padKeymap, "KC_A", "KC_Z", 1)
	o := opts(t.TempDir())
	o.DryRun = true
	var out bytes.Buffer
	if err := Restore(context.Background(), pad.client(t), []byte(other), o, &out); err != nil {
		t.Fatal(err)
	}
	if pad.writes != 0 || !strings.Contains(out.String(), "dry run") {
		t.Fatalf("writes %d out %s", pad.writes, out.String())
	}
}

func TestWriteErrorNamesBackupAndRestoreCommand(t *testing.T) {
	pad := &fakePad{file: padKeymap, failWrite: true}
	o := opts(t.TempDir())
	o.RestoreCmd = "cm2d setup restore"
	err := Apply(context.Background(), pad.client(t), layoutOf(t, testLayout), o, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected an error")
	}
	m := err.Error()
	if !strings.Contains(m, "fs.write") || !strings.Contains(m, "flash busy") || !strings.Contains(m, "cm2d setup restore "+o.BackupDir) {
		t.Fatalf("message %q", m)
	}
}

func TestDefaultRestoreCommand(t *testing.T) {
	pad := &fakePad{file: padKeymap, failWrite: true}
	err := Apply(context.Background(), pad.client(t), layoutOf(t, testLayout), opts(t.TempDir()), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "cm2probe keymap restore ") {
		t.Fatalf("err %v", err)
	}
}

func TestVerifyFailure(t *testing.T) {
	pad := &fakePad{file: padKeymap, dropWrite: true}
	err := Apply(context.Background(), pad.client(t), layoutOf(t, testLayout), opts(t.TempDir()), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "VERIFY FAILED") {
		t.Fatalf("err %v", err)
	}
}

func TestBackupFailureStopsBeforeWrite(t *testing.T) {
	pad := &fakePad{file: padKeymap}
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	o := opts(dir)
	o.BackupDir = filepath.Join(blocker, "sub") // a file where a folder must go
	if err := Apply(context.Background(), pad.client(t), layoutOf(t, testLayout), o, &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error")
	}
	if pad.writes != 0 {
		t.Fatal("wrote without a backup")
	}
}

func TestLoadLayoutRejectsUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l.json")
	if err := os.WriteFile(p, []byte(`{"layerz":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLayout(p); err == nil || !strings.Contains(err.Error(), "layout file") {
		t.Fatalf("err %v", err)
	}
	if _, err := LoadLayout(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestLoadBackup(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(good, []byte(padKeymap), 0o600)
	os.WriteFile(bad, []byte("not json"), 0o600)
	if b, err := LoadBackup(good); err != nil || string(b) != padKeymap {
		t.Fatalf("good: %v", err)
	}
	if _, err := LoadBackup(bad); err == nil || !strings.Contains(err.Error(), "not a keymap.json") {
		t.Fatalf("bad: %v", err)
	}
	if _, err := LoadBackup(filepath.Join(dir, "none.json")); err == nil {
		t.Fatal("missing: expected an error")
	}
}

func TestDecodeFSRead(t *testing.T) {
	cases := []struct {
		in, want string
		fail     bool
	}{
		{`"abc"`, "abc", false},
		{`{"data":"abc"}`, "abc", false},
		{`{"data":{"a":1}}`, `{"a":1}`, false},
		{`{"other":1}`, "", true},
		{`42`, "", true},
	}
	for _, c := range cases {
		got, err := DecodeFSRead(json.RawMessage(c.in))
		if (err != nil) != c.fail || string(got) != c.want {
			t.Errorf("%s: got %q err %v", c.in, got, err)
		}
	}
}

func TestWriteNew(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := WriteNew(p, []byte("one"), false); err != nil {
		t.Fatal(err)
	}
	if err := WriteNew(p, []byte("two"), false); err == nil {
		t.Fatal("overwrote without force")
	}
	if err := WriteNew(p, []byte("two"), true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Fatalf("content %q", b)
	}
}
