package hookcfg

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A settings file like the real one: other hooks, permissions, unrelated
// keys, odd escapes, in 2-space layout.
const realistic = `{
  "model": "opus",
  "env": {
    "BASH_DEFAULT_TIMEOUT_MS": "120000",
    "NOTE": "a <b> & c é \"quoted\" \/ slash"
  },
  "permissions": {
    "allow": [
      "Bash(git status:*)",
      "Bash(go test:*)"
    ],
    "deny": [
      "Read(./.env)"
    ],
    "defaultMode": "default"
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/Users/me/bin/guard.sh",
            "timeout": 10
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "~/bin/greet.sh"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "~/bin/other-hook.sh Stop"
          }
        ]
      }
    ],
    "Empty": []
  },
  "statusLine": {
    "type": "command",
    "command": "~/.claude/statusline.sh"
  },
  "ratio": 1.50e+3,
  "flags": [
    true,
    false,
    null,
    {}
  ],
  "alwaysThinkingEnabled": true
}`

const cm2dPath = "/Users/me/cm2-claude/bin/cm2d"

func setup(t *testing.T, content string) (path string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func backups(t *testing.T, path string) []string {
	t.Helper()
	m, _ := filepath.Glob(path + ".cm2d-bak-*")
	return m
}

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, s)
	}
	return v
}

// ours lists every cm2d hook object under hooks[event].
func ours(t *testing.T, v map[string]any, event string) []map[string]any {
	t.Helper()
	var out []map[string]any
	hooks, _ := v["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	for _, g := range groups {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			hm := h.(map[string]any)
			u, _ := hm["url"].(string)
			c, _ := hm["command"].(string)
			if strings.Contains(u, "/cm2d/v1/") || strings.Contains(c, " hook session-start") {
				out = append(out, hm)
			}
		}
	}
	return out
}

func TestInstallAddsEntries(t *testing.T) {
	path := setup(t, realistic+"\n")
	changed, backup, err := Install(path, cm2dPath, 47731)
	if err != nil || !changed {
		t.Fatalf("Install = %v %q %v", changed, backup, err)
	}
	got := parse(t, read(t, path))

	ss := ours(t, got, "SessionStart")
	if len(ss) != 1 || ss[0]["type"] != "command" || ss[0]["command"] != cm2dPath+" hook session-start" || ss[0]["timeout"] != 5.0 {
		t.Fatalf("SessionStart = %v", ss)
	}
	for _, ev := range []string{"UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "Notification", "PostToolUse"} {
		hs := ours(t, got, ev)
		if len(hs) != 1 {
			t.Fatalf("%s: %d entries", ev, len(hs))
		}
		h := hs[0]
		if h["type"] != "http" || h["url"] != "http://127.0.0.1:47731/cm2d/v1/event" || h["timeout"] != 5.0 {
			t.Errorf("%s = %v", ev, h)
		}
		if hd, _ := h["headers"].(map[string]any); len(hd) != 2 || hd["X-Cm2-Entrypoint"] != "$CLAUDE_CODE_ENTRYPOINT" ||
			hd["X-Cm2-Host-Session"] != "$CLAUDE_CODE_HOST_SESSION_ID" {
			t.Errorf("%s headers = %v", ev, h["headers"])
		}
		if a, _ := h["allowedEnvVars"].([]any); !reflect.DeepEqual(a, []any{"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_HOST_SESSION_ID"}) {
			t.Errorf("%s allowedEnvVars = %v", ev, h["allowedEnvVars"])
		}
		if _, has := h["onFailure"]; has {
			t.Errorf("%s has onFailure", ev)
		}
	}
	pr := ours(t, got, "PermissionRequest")
	if len(pr) != 1 || pr[0]["url"] != "http://127.0.0.1:47731/cm2d/v1/permission" || pr[0]["timeout"] != 600.0 {
		t.Fatalf("PermissionRequest = %v", pr)
	}
	if _, has := pr[0]["onFailure"]; has {
		t.Fatal("PermissionRequest has onFailure")
	}
	if hd, _ := pr[0]["headers"].(map[string]any); len(hd) != 2 || hd["X-Cm2-Entrypoint"] != "$CLAUDE_CODE_ENTRYPOINT" ||
		hd["X-Cm2-Host-Session"] != "$CLAUDE_CODE_HOST_SESSION_ID" {
		t.Fatalf("PermissionRequest headers = %v", pr[0]["headers"])
	}
	if a, _ := pr[0]["allowedEnvVars"].([]any); !reflect.DeepEqual(a, []any{"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_HOST_SESSION_ID"}) {
		t.Fatalf("PermissionRequest allowedEnvVars = %v", pr[0]["allowedEnvVars"])
	}
}

// oldInstall turns a fresh install into the shape the first release wrote:
// no host session header, one allowed env var.
func oldInstall(t *testing.T, path string) {
	t.Helper()
	v := parse(t, read(t, path))
	n := 0
	for _, groups := range v["hooks"].(map[string]any) {
		for _, g := range groups.([]any) {
			for _, h := range g.(map[string]any)["hooks"].([]any) {
				hm := h.(map[string]any)
				if u, _ := hm["url"].(string); strings.Contains(u, "/cm2d/v1/") {
					delete(hm["headers"].(map[string]any), "X-Cm2-Host-Session")
					hm["allowedEnvVars"] = []any{"CLAUDE_CODE_ENTRYPOINT"}
					n++
				}
			}
		}
	}
	if n != 7 {
		t.Fatalf("rewrote %d http hooks, want 7", n)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeOldInstallUpdatesInPlace(t *testing.T) {
	path := setup(t, realistic+"\n")
	if _, _, err := Install(path, cm2dPath, 47731); err != nil {
		t.Fatal(err)
	}
	oldInstall(t, path)
	before := parse(t, read(t, path))

	changed, backup, err := Install(path, cm2dPath, 47731)
	if err != nil || !changed || backup == "" {
		t.Fatalf("upgrade = %v %q %v", changed, backup, err)
	}
	after := parse(t, read(t, path))

	// Same shape as a fresh install: one entry per event, new header, new env var.
	fresh := setup(t, realistic+"\n")
	Install(fresh, cm2dPath, 47731)
	freshV := parse(t, read(t, fresh))
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "Notification", "PostToolUse", "PermissionRequest"} {
		if n := len(ours(t, after, ev)); n != 1 {
			t.Errorf("%s: %d entries after upgrade", ev, n)
		}
		if !reflect.DeepEqual(ours(t, after, ev), ours(t, freshV, ev)) {
			t.Errorf("%s differs from a fresh install:\n%v\n%v", ev, ours(t, after, ev), ours(t, freshV, ev))
		}
	}
	// Other hooks, and the position of our entries in the arrays, are untouched.
	if !reflect.DeepEqual(after, freshV) {
		t.Errorf("upgraded file differs from a fresh install")
	}
	for ev, groups := range before["hooks"].(map[string]any) {
		if len(after["hooks"].(map[string]any)[ev].([]any)) != len(groups.([]any)) {
			t.Errorf("%s: group count changed", ev)
		}
	}
	if !reflect.DeepEqual(after["hooks"].(map[string]any)["PreToolUse"], before["hooks"].(map[string]any)["PreToolUse"]) {
		t.Error("PreToolUse changed")
	}

	// A second run changes nothing, and there is no new backup.
	changed, backup, err = Install(path, cm2dPath, 47731)
	if err != nil || changed || backup != "" {
		t.Fatalf("second install = %v %q %v", changed, backup, err)
	}
	if n := len(backups(t, path)); n != 2 { // first install + upgrade
		t.Fatalf("backups = %d, want 2", n)
	}

	// Uninstall removes exactly what the new install adds.
	if _, _, err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	gone := read(t, path)
	if strings.Contains(gone, "cm2d") || strings.Contains(gone, "Host-Session") {
		t.Fatalf("uninstall left cm2d entries:\n%s", gone)
	}
	want := parse(t, realistic)
	if !reflect.DeepEqual(parse(t, gone), want) {
		t.Fatal("uninstall did not restore the other hooks")
	}
}

func TestInstallKeepsEverythingElse(t *testing.T) {
	path := setup(t, realistic+"\n")
	if _, _, err := Install(path, cm2dPath, 47731); err != nil {
		t.Fatal(err)
	}
	before, after := parse(t, realistic), parse(t, read(t, path))
	// Everything except hooks is equal.
	for k, v := range before {
		if k == "hooks" {
			continue
		}
		a, _ := json.Marshal(v)
		b, _ := json.Marshal(after[k])
		if !bytes.Equal(a, b) {
			t.Errorf("key %q changed:\n%s\n%s", k, a, b)
		}
	}
	// The old hooks are all still there, in order, first in their arrays.
	oldHooks := before["hooks"].(map[string]any)
	newHooks := after["hooks"].(map[string]any)
	for ev, groups := range oldHooks {
		ng := newHooks[ev].([]any)
		for i, g := range groups.([]any) {
			a, _ := json.Marshal(g)
			b, _ := json.Marshal(ng[i])
			if !bytes.Equal(a, b) {
				t.Errorf("hooks.%s[%d] changed", ev, i)
			}
		}
	}
	// Existing SessionStart and Stop arrays gained a group, not a rewrite.
	if n := len(newHooks["SessionStart"].([]any)); n != 2 {
		t.Errorf("SessionStart groups = %d, want 2", n)
	}
	if n := len(newHooks["Stop"].([]any)); n != 2 {
		t.Errorf("Stop groups = %d, want 2", n)
	}
}

func TestKeyOrderAndTextPreserved(t *testing.T) {
	path := setup(t, realistic+"\n")
	if _, _, err := Install(path, cm2dPath, 47731); err != nil {
		t.Fatal(err)
	}
	out := read(t, path)
	// Top-level key order is unchanged.
	last := -1
	for _, k := range []string{`"model"`, `"env"`, `"permissions"`, `"hooks"`, `"statusLine"`, `"ratio"`, `"flags"`, `"alwaysThinkingEnabled"`} {
		i := strings.Index(out, "\n  "+k+":")
		if i <= last {
			t.Fatalf("key %s out of order or missing (at %d, previous %d)", k, i, last)
		}
		last = i
	}
	// Raw leaves survive byte for byte.
	for _, s := range []string{`1.50e+3`, `"a <b> & c é \"quoted\" \/ slash"`, `"flags": [`} {
		if !strings.Contains(out, s) {
			t.Errorf("output lost %q", s)
		}
	}
	// 2-space indent, no tabs, no 4-space steps.
	if strings.Contains(out, "\t") {
		t.Error("output has tabs")
	}
	if !strings.HasSuffix(out, "}\n") {
		t.Error("trailing newline lost")
	}
	// Original hook order inside "hooks" is kept; new events come after.
	i := func(s string) int { return strings.Index(out, "\n    "+s+":") }
	if !(i(`"PreToolUse"`) < i(`"SessionStart"`) && i(`"SessionStart"`) < i(`"Stop"`) && i(`"Stop"`) < i(`"Empty"`) && i(`"Empty"`) < i(`"UserPromptSubmit"`)) {
		t.Errorf("hooks order wrong:\n%s", out)
	}
}

func TestRoundTripIsExact(t *testing.T) {
	for _, nl := range []string{"", "\n"} {
		path := setup(t, realistic+nl)
		if _, _, err := Install(path, cm2dPath, 47731); err != nil {
			t.Fatal(err)
		}
		changed, _, err := Uninstall(path)
		if err != nil || !changed {
			t.Fatalf("Uninstall = %v %v", changed, err)
		}
		if got := read(t, path); got != realistic+nl {
			t.Fatalf("round trip differs (newline %q):\n%s", nl, got)
		}
	}
}

func TestRoundTripWithoutHooksKey(t *testing.T) {
	const src = "{\n  \"model\": \"opus\"\n}\n"
	path := setup(t, src)
	Install(path, cm2dPath, 47731)
	if strings.Count(read(t, path), "cm2d") == 0 {
		t.Fatal("nothing installed")
	}
	Uninstall(path)
	if got := read(t, path); got != src {
		t.Fatalf("got %q", got)
	}
}

func TestInstallIdempotent(t *testing.T) {
	path := setup(t, realistic+"\n")
	if _, _, err := Install(path, cm2dPath, 47731); err != nil {
		t.Fatal(err)
	}
	first := read(t, path)
	fi1, _ := os.Stat(path)
	time.Sleep(20 * time.Millisecond)
	changed, backup, err := Install(path, cm2dPath, 47731)
	if err != nil || changed || backup != "" {
		t.Fatalf("second Install = %v %q %v", changed, backup, err)
	}
	if read(t, path) != first {
		t.Fatal("file changed on second Install")
	}
	if fi2, _ := os.Stat(path); !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatal("file was rewritten")
	}
	if n := len(backups(t, path)); n != 1 {
		t.Fatalf("backups = %d, want 1", n)
	}
	// No duplicates anywhere.
	got := parse(t, first)
	for _, ev := range []string{"SessionStart", "Stop", "PermissionRequest", "PostToolUse"} {
		if n := len(ours(t, got, ev)); n != 1 {
			t.Errorf("%s: %d entries", ev, n)
		}
	}
}

func TestNoWriteWhenNothingChangesEvenIfFormatDiffers(t *testing.T) {
	path := setup(t, realistic+"\n")
	Install(path, cm2dPath, 47731)
	// Re-indent with 4 spaces by hand: still installed, so nothing is touched.
	var v any
	json.Unmarshal([]byte(read(t, path)), &v)
	b, _ := json.MarshalIndent(v, "", "    ")
	os.WriteFile(path, b, 0o600)
	changed, backup, err := Install(path, cm2dPath, 47731)
	if err != nil || changed || backup != "" {
		t.Fatalf("Install = %v %q %v", changed, backup, err)
	}
	if read(t, path) != string(b) {
		t.Fatal("file was rewritten")
	}
}

func TestBackup(t *testing.T) {
	path := setup(t, realistic+"\n")
	fixed := time.Date(2026, 10, 23, 14, 5, 9, 0, time.Local)
	now = func() time.Time { return fixed }
	defer func() { now = time.Now }()

	_, backup, err := Install(path, cm2dPath, 47731)
	if err != nil {
		t.Fatal(err)
	}
	if want := path + ".cm2d-bak-20261023-140509"; backup != want {
		t.Fatalf("backup = %q, want %q", backup, want)
	}
	if read(t, backup) != realistic+"\n" {
		t.Fatal("backup is not the original")
	}
	if fi, _ := os.Stat(backup); fi.Mode().Perm() != 0o640 {
		t.Errorf("backup mode %v", fi.Mode().Perm())
	}
	// Same second again: a new name, the first backup stays.
	installed := read(t, path)
	_, backup2, err := Uninstall(path)
	if err != nil {
		t.Fatal(err)
	}
	if backup2 == backup || backup2 == "" {
		t.Fatalf("second backup = %q", backup2)
	}
	if read(t, backup) != realistic+"\n" || read(t, backup2) != installed {
		t.Fatal("backup contents wrong")
	}
	if filepath.Dir(backup) != filepath.Dir(path) {
		t.Fatal("backup is not next to the file")
	}
}

func TestFileModeKept(t *testing.T) {
	path := setup(t, realistic)
	os.Chmod(path, 0o600)
	Install(path, cm2dPath, 47731)
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	// No temp files left behind.
	m, _ := filepath.Glob(path + ".tmp-*")
	if len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}

func TestPortChangeUpdatesInPlace(t *testing.T) {
	path := setup(t, realistic+"\n")
	Install(path, cm2dPath, 47731)
	changed, backup, err := Install(path, cm2dPath, 50000)
	if err != nil || !changed || backup == "" {
		t.Fatalf("Install = %v %q %v", changed, backup, err)
	}
	out := read(t, path)
	if strings.Contains(out, "47731") || strings.Count(out, ":50000/cm2d/v1/event") != 6 {
		t.Fatalf("port not replaced:\n%s", out)
	}
	got := parse(t, out)
	for _, ev := range []string{"SessionStart", "Stop", "PermissionRequest"} {
		if n := len(ours(t, got, ev)); n != 1 {
			t.Errorf("%s: %d entries", ev, n)
		}
	}
	// A new binary path updates the command the same way.
	Install(path, "/opt/cm2d", 50000)
	if !strings.Contains(read(t, path), `"/opt/cm2d hook session-start --port 50000"`) || strings.Contains(read(t, path), cm2dPath) {
		t.Fatal("path not updated")
	}
}

func TestDuplicatesCollapse(t *testing.T) {
	const src = `{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          { "type": "command", "command": "keep.sh" },
          { "type": "http", "url": "http://127.0.0.1:1/cm2d/v1/event" }
        ]
      },
      {
        "hooks": [
          { "type": "http", "url": "http://127.0.0.1:2/cm2d/v1/event" }
        ]
      }
    ]
  }
}`
	path := setup(t, src)
	if _, _, err := Install(path, cm2dPath, 47731); err != nil {
		t.Fatal(err)
	}
	got := parse(t, read(t, path))
	if n := len(ours(t, got, "Stop")); n != 1 {
		t.Fatalf("Stop entries = %d", n)
	}
	groups := got["hooks"].(map[string]any)["Stop"].([]any)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1 (the emptied one is dropped)", len(groups))
	}
	hs := groups[0].(map[string]any)["hooks"].([]any)
	if len(hs) != 2 || hs[0].(map[string]any)["command"] != "keep.sh" {
		t.Fatalf("group hooks = %v", hs)
	}
}

func TestUninstallRemovesOnlyOurs(t *testing.T) {
	// Our entry shares a group with another hook; a matcher group too.
	const src = `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume",
        "hooks": [
          {
            "type": "command",
            "command": "/x/cm2d hook session-start",
            "timeout": 5
          },
          {
            "type": "command",
            "command": "other.sh"
          }
        ]
      }
    ],
    "Notification": [
      {
        "hooks": [
          {
            "type": "http",
            "url": "http://127.0.0.1:47731/cm2d/v1/event"
          }
        ]
      }
    ],
    "Elsewhere": [
      {
        "hooks": [
          {
            "type": "http",
            "url": "http://example.com/other"
          }
        ]
      }
    ]
  },
  "model": "opus"
}`
	path := setup(t, src)
	changed, backup, err := Uninstall(path)
	if err != nil || !changed || backup == "" {
		t.Fatalf("Uninstall = %v %q %v", changed, backup, err)
	}
	out := read(t, path)
	if strings.Contains(out, "cm2d") || strings.Contains(out, "Notification") {
		t.Fatalf("not removed:\n%s", out)
	}
	for _, s := range []string{`"matcher": "startup|resume"`, `"other.sh"`, `"Elsewhere"`, `http://example.com/other`, `"model": "opus"`} {
		if !strings.Contains(out, s) {
			t.Errorf("lost %q", s)
		}
	}
	parse(t, out)
}

func TestUninstallDropsEmptyHooks(t *testing.T) {
	path := setup(t, "{\n  \"a\": 1\n}")
	Install(path, cm2dPath, 47731)
	Uninstall(path)
	if got := read(t, path); got != "{\n  \"a\": 1\n}" {
		t.Fatalf("got %q", got)
	}
}

func TestUninstallNothingToDo(t *testing.T) {
	path := setup(t, realistic+"\n")
	changed, backup, err := Uninstall(path)
	if err != nil || changed || backup != "" {
		t.Fatalf("Uninstall = %v %q %v", changed, backup, err)
	}
	if read(t, path) != realistic+"\n" || len(backups(t, path)) != 0 {
		t.Fatal("file touched")
	}
	// No hooks key at all.
	p2 := setup(t, `{"a":1}`)
	if changed, _, err := Uninstall(p2); err != nil || changed {
		t.Fatalf("Uninstall = %v %v", changed, err)
	}
	if read(t, p2) != `{"a":1}` {
		t.Fatal("file touched")
	}
}

func TestMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if changed, b, err := Uninstall(path); err != nil || changed || b != "" {
		t.Fatalf("Uninstall = %v %q %v", changed, b, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("Uninstall created the file")
	}
	changed, backup, err := Install(path, cm2dPath, 47731)
	if err != nil || !changed || backup != "" {
		t.Fatalf("Install = %v %q %v", changed, backup, err)
	}
	out := read(t, path)
	parse(t, out)
	if len(ours(t, parse(t, out), "PermissionRequest")) != 1 {
		t.Fatal("not installed")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if _, _, err := Install(filepath.Join(dir, "no", "such", "settings.json"), cm2dPath, 1); err == nil {
		t.Fatal("Install into a missing folder succeeded")
	}
}

func TestEmptyFile(t *testing.T) {
	for _, c := range []string{"", "  \n"} {
		path := setup(t, c)
		changed, _, err := Install(path, cm2dPath, 47731)
		if err != nil || !changed {
			t.Fatalf("Install = %v %v", changed, err)
		}
		parse(t, read(t, path))
	}
}

func TestBadInput(t *testing.T) {
	cases := map[string]string{
		"not json":         `{"a":`,
		"array":            `[1]`,
		"hooks not object": `{"hooks":[]}`,
		"event not array":  `{"hooks":{"Stop":{}}}`,
	}
	for name, src := range cases {
		path := setup(t, src)
		if _, _, err := Install(path, cm2dPath, 47731); err == nil {
			t.Errorf("%s: Install succeeded", name)
		}
		if read(t, path) != src || len(backups(t, path)) != 0 {
			t.Errorf("%s: file touched", name)
		}
	}
	for _, src := range []string{`{"a":`, `[1]`, `{"hooks":3}`} {
		path := setup(t, src)
		if _, _, err := Uninstall(path); err == nil {
			t.Errorf("Uninstall of %q succeeded", src)
		}
		if read(t, path) != src {
			t.Errorf("file touched: %q", src)
		}
	}
	path := setup(t, `{}`)
	if _, _, err := Install(path, "", 47731); err == nil {
		t.Error("empty cm2d path accepted")
	}
	for _, port := range []int{0, -1, 65536} {
		if _, _, err := Install(path, cm2dPath, port); err == nil {
			t.Errorf("port %d accepted", port)
		}
	}
	if read(t, path) != `{}` {
		t.Error("file touched")
	}
}

func TestSymlinkTargetReplaced(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles-settings.json")
	os.WriteFile(target, []byte(realistic), 0o600)
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlinks")
	}
	changed, backup, err := Install(link, cm2dPath, 47731)
	if err != nil || !changed {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a file")
	}
	if !strings.Contains(read(t, target), "/cm2d/v1/") {
		t.Fatal("target not updated")
	}
	if want, _ := filepath.EvalSymlinks(target); !strings.HasPrefix(backup, want) {
		t.Fatalf("backup %q not next to the target", backup)
	}
}

func TestCommandQuoting(t *testing.T) {
	for in, want := range map[string]string{
		"/a/b/cm2d":             "/a/b/cm2d hook session-start",
		"/Users/me/My Dir/cm2d": "'/Users/me/My Dir/cm2d' hook session-start",
		"/it's/cm2d":            `'/it'\''s/cm2d' hook session-start`,
		"/a$b/cm2d":             "'/a$b/cm2d' hook session-start",
	} {
		path := setup(t, `{}`)
		if _, _, err := Install(path, in, 47731); err != nil {
			t.Fatal(err)
		}
		ss := ours(t, parse(t, read(t, path)), "SessionStart")
		if len(ss) != 1 || ss[0]["command"] != want {
			t.Errorf("%q: command = %v, want %q", in, ss, want)
		}
		// Detected as ours on the next run, so no second entry.
		if changed, _, _ := Install(path, in, 47731); changed {
			t.Errorf("%q: second Install changed the file", in)
		}
	}
}

func TestDataFunctionsDoNotTouchDisk(t *testing.T) {
	out, changed, err := InstallData([]byte(realistic), cm2dPath, 47731)
	if err != nil || !changed || !bytes.Contains(out, []byte("/cm2d/v1/permission")) {
		t.Fatalf("InstallData = %v %v", changed, err)
	}
	out2, changed, err := InstallData(out, cm2dPath, 47731)
	if err != nil || changed || !bytes.Equal(out, out2) {
		t.Fatalf("second InstallData changed = %v err = %v", changed, err)
	}
	back, changed, err := UninstallData(out)
	if err != nil || !changed || string(back) != realistic {
		t.Fatalf("UninstallData = %v %v\n%s", changed, err, back)
	}
}

func TestParseTreeEdgeCases(t *testing.T) {
	src := `{"a":{"b":[1,[2,{"c":"x,}]\"y"}],"d"],"e\"k":null,"f":-0.5E-2,"g":[],"h":{}}}`
	root, err := parseTree([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := root.compact(); got != src {
		t.Fatalf("compact = %s", got)
	}
	var pretty bytes.Buffer
	root.write(&pretty, true, 0)
	var a, b any
	json.Unmarshal([]byte(src), &a)
	json.Unmarshal(pretty.Bytes(), &b)
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if !bytes.Equal(x, y) {
		t.Fatalf("pretty output differs:\n%s", pretty.String())
	}
	if !strings.Contains(pretty.String(), "\"g\": []") || !strings.Contains(pretty.String(), "\"h\": {}") {
		t.Fatalf("empty containers:\n%s", pretty.String())
	}
	if _, err := parseTree([]byte(`{"a":`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

// A foreign command that has " hook session-start" in it is not ours: Install
// must keep it and add its own entry, and Uninstall must keep it.
func TestForeignSessionStartHookIsNotOurs(t *testing.T) {
	const src = `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          { "type": "command", "command": "/usr/local/bin/othertool hook session-start --quiet", "timeout": 30 },
          { "type": "command", "command": "bd hook session-start" }
        ]
      }
    ]
  }
}
`
	path := setup(t, src)
	if _, _, err := Install(path, "/opt/cm2d", 47731); err != nil {
		t.Fatal(err)
	}
	out := read(t, path)
	for _, keep := range []string{"/usr/local/bin/othertool hook session-start --quiet", `"command": "bd hook session-start"`} {
		if !strings.Contains(out, keep) {
			t.Fatalf("foreign hook %q was overwritten:\n%s", keep, out)
		}
	}
	if !strings.Contains(out, `"/opt/cm2d hook session-start"`) {
		t.Fatalf("our hook is missing:\n%s", out)
	}
	if changed, _, _ := Install(path, "/opt/cm2d", 47731); changed {
		t.Fatal("second Install changed the file")
	}
	if _, _, err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); !reflect.DeepEqual(parse(t, got), parse(t, src)) {
		t.Fatalf("Uninstall did not restore the foreign hooks:\n%s", got)
	}
}

func TestIsOursCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/opt/cm2d hook session-start":                        true,
		"cm2d hook session-start":                             true,
		"'/Users/me/My Dir/cm2d' hook session-start":          true,
		`'/it'\''s/cm2d' hook session-start`:                  true,
		"/opt/cm2d hook session-start --port 50000":           true,
		"'/My Dir/cm2d' hook session-start --port 50000":      true,
		"/usr/local/bin/othertool hook session-start --quiet": false,
		"bd hook session-start":                               false,
		"/opt/cm2d hook session-start extra":                  false,
		"/opt/cm2d hook session-start --port":                 false,
		"/opt/cm2d hook session-start --port abc":             false,
		"/opt/cm2d-other hook session-start":                  false,
		"/opt/cm2d hook session-stop":                         false,
		"'/opt/cm2d hook session-start":                       false,
		"":                                                    false,
	} {
		h := fromGo(commandHook{Type: "command", Command: cmd})
		if got := isOurs(h); got != want {
			t.Errorf("isOurs(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// A non-default port goes into the SessionStart command too, so the
// forwarder posts to the right daemon.
func TestSessionStartCarriesNonDefaultPort(t *testing.T) {
	path := setup(t, `{}`)
	sessionStart := func() string {
		ss := ours(t, parse(t, read(t, path)), "SessionStart")
		if len(ss) != 1 {
			t.Fatalf("%d SessionStart entries", len(ss))
		}
		return ss[0]["command"].(string)
	}
	if _, _, err := Install(path, "/opt/cm2d", 50000); err != nil {
		t.Fatal(err)
	}
	if got := sessionStart(); got != "/opt/cm2d hook session-start --port 50000" {
		t.Fatalf("command = %q", got)
	}
	if changed, _, _ := Install(path, "/opt/cm2d", 50000); changed {
		t.Fatal("second Install changed the file")
	}
	// Back to the default port: the flag goes away, in place.
	if changed, _, _ := Install(path, "/opt/cm2d", 47731); !changed {
		t.Fatal("port change not written")
	}
	if got := sessionStart(); got != "/opt/cm2d hook session-start" {
		t.Fatalf("command = %q", got)
	}
	// Quoted path plus port; Uninstall finds it.
	if _, _, err := Install(path, "/My Dir/cm2d", 51000); err != nil {
		t.Fatal(err)
	}
	if got := sessionStart(); got != "'/My Dir/cm2d' hook session-start --port 51000" {
		t.Fatalf("command = %q", got)
	}
	if _, _, err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(read(t, path)); got != "{}" {
		t.Fatalf("Uninstall left %s", got)
	}
}

// Uninstall can only find a hook it can recognise, so Install refuses a
// binary that is not named cm2d.
func TestInstallRefusesOtherBinaryName(t *testing.T) {
	path := setup(t, `{}`)
	if _, _, err := Install(path, "/opt/other", 47731); err == nil {
		t.Fatal("Install accepted a binary not named cm2d")
	}
	if read(t, path) != `{}` {
		t.Fatal("file changed")
	}
}
