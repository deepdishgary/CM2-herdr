package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cm2-claude/internal/device"
)

const padKeymap = `{"version":1,"activeProfileId":0,"profiles":[{"id":0,"layers":[
{"id":0,"layout":{"keymap":[["KC_A","KC_B"],["KC_C","KC_D","KC_E","KC_F"],["KC_G","KC_H","KC_I","KC_J"],["KC_NONE","KC_L","KC_M"]],"encoders":[["KC_VOLU","KC_VOLD","KC_MPLY"]]}},
{"id":1,"layout":{"keymap":[["KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE"]],"encoders":[["KC_NONE","KC_NONE","KC_NONE"]]}}]}]}`

// fakePad answers fs.read and fs.write from an in-memory keymap.json.
type fakePad struct {
	mu     sync.Mutex
	file   string
	writes int
}

func (p *fakePad) serve(h *harness) {
	h.reply(func(m string, params json.RawMessage) string {
		p.mu.Lock()
		defer p.mu.Unlock()
		switch m {
		case "fs.read":
			q, _ := json.Marshal(p.file)
			return `{"data":` + string(q) + `}`
		case "fs.write":
			var w struct{ File, Data string }
			if err := json.Unmarshal(params, &w); err != nil || w.File != "keymap.json" {
				h.t.Errorf("bad fs.write params %s", params)
			}
			p.file = w.Data
			p.writes++
			return `null`
		}
		h.t.Errorf("unexpected method %s", m)
		return `null`
	})
}

// run runs one cm2probe command against the pad with a fresh harness, since
// each command closes its connection when it ends.
func (p *fakePad) run(t *testing.T, args ...string) (int, *harness) {
	t.Helper()
	h := newHarness(t)
	p.serve(h)
	return h.run(args...), h
}

func writeLayout(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "layout.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const testLayout = `{"layers":[
 {"layer":0,"encoders":[[null,null,"KI_LS2"]]},
 {"layer":1,"keymap":[["KV_OAI_AG00","KV_OAI_AG01"],["KV_OAI_AG02","KV_OAI_AG03","KV_OAI_AG04","KV_OAI_AG05"],["KV_OAI_AG06","KV_OAI_AG07","KV_OAI_AG08","KV_OAI_AG09"],["KV_OAI_AG10","KV_OAI_AG11","KV_OAI_AG12"]],
  "encoders":[["KV_OAI_ENC_CW","KV_OAI_ENC_CC","KI_LS1"]]}]}`

func TestKeymapDryRunWritesNothing(t *testing.T) {
	h := newHarness(t)
	pad := &fakePad{file: padKeymap}
	pad.serve(h)
	dir := t.TempDir()
	if code := h.run("keymap", "apply", writeLayout(t, dir, testLayout), "--dry-run", "--backup-dir", filepath.Join(dir, "b")); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if pad.writes != 0 || !strings.Contains(h.out.String(), "17 key(s) change") {
		t.Fatalf("writes %d, out %s", pad.writes, h.out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "b")); err == nil {
		t.Fatal("dry run created a backup")
	}
}

func TestKeymapApplyRestoreRoundTrip(t *testing.T) {
	pad := &fakePad{file: padKeymap}
	dir := t.TempDir()
	bdir := filepath.Join(dir, "b")
	layout := writeLayout(t, dir, testLayout)

	code, h := pad.run(t, "keymap", "apply", layout, "--backup-dir", bdir)
	if code != exitOK {
		t.Fatalf("apply exit %d: %s", code, h.errb.String())
	}
	if !strings.Contains(h.out.String(), "verified") || !strings.Contains(pad.file, `"KI_LS2"`) || !strings.Contains(pad.file, `"KV_OAI_AG12"`) {
		t.Fatalf("apply did not land: %s\n%s", h.out.String(), pad.file)
	}
	// Re-applying changes nothing and writes nothing.
	if code, h = pad.run(t, "keymap", "apply", layout, "--backup-dir", bdir); code != exitOK || pad.writes != 1 {
		t.Fatalf("second apply: exit %d, writes %d\nstdout %s\nstderr %s", code, pad.writes, h.out.String(), h.errb.String())
	}

	orig := filepath.Join(dir, "orig.json")
	if err := os.WriteFile(orig, []byte(padKeymap), 0o600); err != nil {
		t.Fatal(err)
	}
	// Restore and apply again straight away: the backups taken before each
	// write must not collide, even within the same second.
	for i := 0; i < 3; i++ {
		if code, h := pad.run(t, "keymap", "restore", orig, "--backup-dir", bdir); code != exitOK {
			t.Fatalf("restore %d exit %d: %s", i, code, h.errb.String())
		}
		if !device.SameKeymap([]byte(pad.file), []byte(padKeymap)) {
			t.Fatalf("restore %d: pad differs from the backup", i)
		}
		if code, h := pad.run(t, "keymap", "apply", layout, "--backup-dir", bdir); code != exitOK {
			t.Fatalf("apply %d exit %d: %s", i, code, h.errb.String())
		}
	}
	entries, err := os.ReadDir(bdir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != pad.writes {
		t.Fatalf("%d backups for %d writes", len(entries), pad.writes)
	}
}

func TestKeymapApplyRefusesShapeMismatch(t *testing.T) {
	h := newHarness(t)
	pad := &fakePad{file: padKeymap}
	pad.serve(h)
	dir := t.TempDir()
	bad := `{"layers":[{"layer":1,"keymap":[["KV_OAI_AG00"]]}]}`
	if code := h.run("keymap", "apply", writeLayout(t, dir, bad), "--backup-dir", filepath.Join(dir, "b")); code == exitOK {
		t.Fatal("expected failure")
	}
	if pad.writes != 0 {
		t.Fatal("wrote despite a bad layout")
	}
}

func TestKeymapRejectsUnknownLayoutFields(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	if code := h.run("keymap", "apply", writeLayout(t, dir, `{"layerz":[]}`)); code == exitOK {
		t.Fatal("expected failure for a misspelled field")
	}
	if h.opens != 0 {
		t.Fatal("opened the pad for an invalid layout file")
	}
}
