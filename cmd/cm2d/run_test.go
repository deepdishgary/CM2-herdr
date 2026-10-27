package main

import (
	"bytes"
	"strings"
	"testing"

	"cm2-claude/internal/focus"
)

func TestRunConfigWiresDesktopChatsAndLogger(t *testing.T) {
	te := newTestEnv(t, "", nil)
	cfg := runConfig(te.env, 47731, true, true, "")
	f := cfg.Focuser
	if f == nil {
		t.Fatal("no focuser")
	}
	d, ok := f.Desktop.(*focus.DesktopRecords)
	if !ok || d == nil {
		t.Fatalf("Desktop = %T, want *focus.DesktopRecords", f.Desktop)
	}
	if d.Root != focus.DefaultDesktopRoot() || d.Root == "" {
		t.Fatalf("root = %q", d.Root)
	}
	if f.Log == nil || f.Log != cfg.Logger {
		t.Fatal("focuser does not share the daemon logger")
	}
}

func TestVerboseLoggerReachesTheFocuser(t *testing.T) {
	te := newTestEnv(t, "", nil)
	cfg := runConfig(te.env, 47731, true, true, "")
	var buf bytes.Buffer
	setVerbose(&cfg, &buf)
	if cfg.Focuser.Log != cfg.Logger {
		t.Fatal("focuser kept the old logger")
	}
	cfg.Focuser.Log.Debug("probe")
	if !strings.Contains(buf.String(), "probe") {
		t.Fatalf("debug line missing: %q", buf.String())
	}
}
