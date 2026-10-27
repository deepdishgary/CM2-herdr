package focus

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	hostA       = "local_1d9854de-0000-4000-8000-000000000001"
	hostB       = "local_1d9854de-0000-4000-8000-000000000002"
	titleSecret = "SECRET-TITLE-do-not-leak"
)

// record writes a desktop record file and returns its path.
func record(t *testing.T, root, org, acct, id, body string) string {
	t.Helper()
	dir := filepath.Join(root, org, acct)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, id+".json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// rec is a record body with a cli session id and archive flag.
func rec(cli string, archived bool) string {
	return fmt.Sprintf(`{"sessionId":"x","cliSessionId":%q,"isArchived":%v,"cwd":"/w","title":%q}`, cli, archived, titleSecret)
}

func check(t *testing.T, root, id string) (bool, string) {
	t.Helper()
	return NewDesktopRecords(root).Check(id)
}

func TestCheckOK(t *testing.T) {
	root := t.TempDir()
	record(t, root, "org", "acct", hostA, rec("cli-1", false))
	if ok, why := check(t, root, hostA); !ok {
		t.Fatalf("rejected: %s", why)
	}
}

func TestCheckRejects(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{"missing record", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostB, rec("cli-1", false))
		}},
		{"empty root", func(t *testing.T, root string) {}},
		{"archived", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostA, rec("cli-1", true))
		}},
		{"empty cli session id", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostA, rec("", false))
		}},
		{"no cli session id", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostA, `{"isArchived":false}`)
		}},
		{"not json", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostA, `{"cliSessionId": "c1", `)
		}},
		{"empty file", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostA, ``)
		}},
		{"json array", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostA, `["cliSessionId"]`)
		}},
		{"isArchived is not a bool", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostA, `{"cliSessionId":"c1","isArchived":"no"}`)
		}},
		{"cliSessionId is not a string", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostA, `{"cliSessionId":5,"isArchived":false}`)
		}},
		{"another active record shares the cli session", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostA, rec("cli-1", false))
			record(t, root, "org", "acct", hostB, rec("cli-1", false))
		}},
		{"another active record, other account folder", func(t *testing.T, root string) {
			record(t, root, "org", "acct1", hostA, rec("cli-1", false))
			record(t, root, "org", "acct2", hostB, rec("cli-1", false))
		}},
		{"another active record without isArchived", func(t *testing.T, root string) {
			record(t, root, "org", "acct", hostA, rec("cli-1", false))
			record(t, root, "org", "acct", hostB, `{"cliSessionId":"cli-1"}`)
		}},
		{"same name in two account folders", func(t *testing.T, root string) {
			record(t, root, "org", "acct1", hostA, rec("cli-1", false))
			record(t, root, "org", "acct2", hostA, rec("cli-1", false))
		}},
		{"same name in two org folders, only one active", func(t *testing.T, root string) {
			record(t, root, "org1", "acct", hostA, rec("cli-1", false))
			record(t, root, "org2", "acct", hostA, rec("cli-9", true))
		}},
		{"record is a directory", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "org", "acct", hostA+".json"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"record is a symlink", func(t *testing.T, root string) {
			real := record(t, root, "org", "other", "local_real", rec("cli-1", false))
			dir := filepath.Join(root, "org", "acct")
			os.MkdirAll(dir, 0o755)
			if err := os.Symlink(real, filepath.Join(dir, hostA+".json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"record one level too deep", func(t *testing.T, root string) {
			record(t, filepath.Join(root, "org"), "acct", "deeper", hostA, rec("cli-1", false))
		}},
		{"record one level too shallow", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "org", hostA+".json"), []byte(rec("cli-1", false)), 0o600); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			os.MkdirAll(filepath.Join(root, "org"), 0o755)
			c.setup(t, root)
			ok, why := check(t, root, hostA)
			if ok {
				t.Fatal("accepted")
			}
			if why == "" {
				t.Fatal("no reason")
			}
			if strings.Contains(why, titleSecret) {
				t.Fatalf("reason leaks a title: %q", why)
			}
		})
	}
}

func TestCheckRootMissing(t *testing.T) {
	if ok, why := check(t, filepath.Join(t.TempDir(), "nope"), hostA); ok || why == "" {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
}

func TestCheckUnreadableRecord(t *testing.T) {
	root := t.TempDir()
	p := record(t, root, "org", "acct", hostA, rec("cli-1", false))
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o600)
	if f, err := os.Open(p); err == nil {
		f.Close()
		t.Skip("file is readable despite mode 0 (running as root?)")
	}
	if ok, _ := check(t, root, hostA); ok {
		t.Fatal("accepted an unreadable record")
	}
}

func TestCheckDuplicateButArchivedIsFine(t *testing.T) {
	root := t.TempDir()
	// The August import: same cli session id, archived.
	record(t, root, "org", "acct", hostA, rec("cli-1", false))
	record(t, root, "org", "acct", hostB, rec("cli-1", true))
	if ok, why := check(t, root, hostA); !ok {
		t.Fatalf("rejected: %s", why)
	}
	// The archived one itself is rejected.
	if ok, _ := check(t, root, hostB); ok {
		t.Fatal("accepted the archived record")
	}
}

func TestCheckOtherCliSessionsDoNotMatter(t *testing.T) {
	root := t.TempDir()
	record(t, root, "org", "acct", hostA, rec("cli-1", false))
	record(t, root, "org", "acct", hostB, rec("cli-2", false))
	if ok, why := check(t, root, hostA); !ok {
		t.Fatalf("rejected: %s", why)
	}
}

func TestCheckIgnoresFilesThatAreNotRecords(t *testing.T) {
	root := t.TempDir()
	record(t, root, "org", "acct", hostA, rec("cli-1", false))
	// Files that are not local_*.json records, with the same cli session id inside.
	record(t, root, "org", "acct", "notarecord", rec("cli-1", false))
	os.WriteFile(filepath.Join(root, "org", "acct", "local_x.txt"), []byte(rec("cli-1", false)), 0o600)
	os.WriteFile(filepath.Join(root, "org", "stray.json"), []byte(rec("cli-1", false)), 0o600)
	os.WriteFile(filepath.Join(root, "top.json"), []byte(rec("cli-1", false)), 0o600)
	if ok, why := check(t, root, hostA); !ok {
		t.Fatalf("rejected: %s", why)
	}
}

// A record we cannot read or parse might be a live twin, so Check fails closed.
func TestCheckRejectsWhenAnotherRecordIsUnreadable(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"truncated", `{"cliSessionId":"cli-1",`},
		{"empty", ``},
		{"not json", `garbage`},
		{"archived as string", `{"cliSessionId":"cli-1","isArchived":"false"}`},
		{"archived as number", `{"cliSessionId":"cli-1","isArchived":0}`},
		{"cli id as number", `{"cliSessionId":7,"isArchived":false}`},
		{"oversized", `{"cliSessionId":"cli-1","pad":"` + strings.Repeat("x", maxRecord) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			record(t, root, "org", "acct", hostA, rec("cli-1", false))
			record(t, root, "org", "acct", "local_other", tc.body)
			if ok, _ := check(t, root, hostA); ok {
				t.Fatal("accepted while another record could not be read")
			}
		})
	}
}

func TestCheckRejectsWhenAnotherRecordHasNoReadPermission(t *testing.T) {
	root := t.TempDir()
	record(t, root, "org", "acct", hostA, rec("cli-1", false))
	p := record(t, root, "org", "acct", "local_other", rec("cli-1", false))
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o600)
	if f, err := os.Open(p); err == nil {
		f.Close()
		t.Skip("file is readable despite mode 0 (running as root?)")
	}
	if ok, _ := check(t, root, hostA); ok {
		t.Fatal("accepted while another record was unreadable")
	}
}

// A symlink in the scan could hide a twin, so Check fails closed on it.
func TestCheckRejectsSymlinksInTheScan(t *testing.T) {
	t.Run("symlinked record file", func(t *testing.T) {
		root := t.TempDir()
		record(t, root, "org", "acct", hostA, rec("cli-1", false))
		elsewhere := filepath.Join(t.TempDir(), "twin.json")
		os.WriteFile(elsewhere, []byte(rec("cli-1", false)), 0o600)
		if err := os.Symlink(elsewhere, filepath.Join(root, "org", "acct", "local_twin.json")); err != nil {
			t.Fatal(err)
		}
		if ok, _ := check(t, root, hostA); ok {
			t.Fatal("accepted with a symlinked twin")
		}
	})
	t.Run("symlinked account folder", func(t *testing.T) {
		root := t.TempDir()
		record(t, root, "org", "acct", hostA, rec("cli-1", false))
		other := t.TempDir()
		record(t, other, "o", "a", "local_twin", rec("cli-1", false))
		if err := os.Symlink(filepath.Join(other, "o", "a"), filepath.Join(root, "org", "acct2")); err != nil {
			t.Fatal(err)
		}
		if ok, _ := check(t, root, hostA); ok {
			t.Fatal("accepted with a symlinked account folder")
		}
	})
	t.Run("symlinked org folder", func(t *testing.T) {
		root := t.TempDir()
		record(t, root, "org", "acct", hostA, rec("cli-1", false))
		other := t.TempDir()
		record(t, other, "o", "a", "local_twin", rec("cli-1", false))
		if err := os.Symlink(filepath.Join(other, "o"), filepath.Join(root, "org2")); err != nil {
			t.Fatal(err)
		}
		if ok, _ := check(t, root, hostA); ok {
			t.Fatal("accepted with a symlinked org folder")
		}
	})
}

// A record that is a FIFO must not hang Check.
func TestCheckDoesNotHangOnAFifoRecord(t *testing.T) {
	root := t.TempDir()
	record(t, root, "org", "acct", hostA, rec("cli-1", false))
	fifo := filepath.Join(root, "org", "acct", "local_fifo.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no fifo:", err)
	}
	done := make(chan bool, 1)
	go func() { ok, _ := check(t, root, hostA); done <- ok }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("accepted with a FIFO record")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Check hung on a FIFO record")
	}
}

// The target is opened without blocking: a FIFO swapped in for it cannot hang.
func TestReadRecordRefusesAFifo(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "local_f.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no fifo:", err)
	}
	done := make(chan bool, 1)
	go func() { _, ok := readRecord(fifo); done <- ok }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("read a FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readRecord hung on a FIFO")
	}
}

func TestCheckRejectsBadIDsEvenWhenTheTargetExists(t *testing.T) {
	root := t.TempDir()
	// A real record, and a decoy a traversal could reach.
	record(t, root, "org", "acct", hostA, rec("cli-1", false))
	record(t, root, "org", "acct", "local_x", rec("cli-2", false))
	os.WriteFile(filepath.Join(filepath.Dir(root), "x.json"), []byte(rec("cli-3", false)), 0o600)
	bad := []string{
		"", "local_", "local_../x", "local_../../x", "../" + hostA, hostA + "/../" + hostA, "org/acct/" + hostA,
		"local_x/../local_x", hostA + ".json", hostA + "\n", hostA + "\x00", "local_*", "local_?", "local_[a]",
		"/etc/passwd", "local_" + strings.Repeat("a", 65), "LOCAL_x", "local_é", "local x", "$CLAUDE_CODE_HOST_SESSION_ID",
	}
	for _, id := range bad {
		if ok, why := check(t, root, id); ok || why == "" {
			t.Errorf("%q: ok=%v why=%q", id, ok, why)
		}
	}
}

func TestCheckReadsFreshEachCall(t *testing.T) {
	root := t.TempDir()
	p := record(t, root, "org", "acct", hostA, rec("cli-1", false))
	d := NewDesktopRecords(root)
	if ok, _ := d.Check(hostA); !ok {
		t.Fatal("first check")
	}
	// The user archives it.
	os.WriteFile(p, []byte(rec("cli-1", true)), 0o600)
	if ok, _ := d.Check(hostA); ok {
		t.Fatal("second check used a cached record")
	}
	// A fork appears.
	os.WriteFile(p, []byte(rec("cli-1", false)), 0o600)
	record(t, root, "org", "acct", hostB, rec("cli-1", false))
	if ok, _ := d.Check(hostA); ok {
		t.Fatal("third check missed the new record")
	}
	// And is removed.
	os.Remove(filepath.Join(root, "org", "acct", hostB+".json"))
	if ok, why := d.Check(hostA); !ok {
		t.Fatalf("fourth check: %s", why)
	}
}

// snapshot lists every path under root with its mode, size, mtime and content.
func snapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		fi, _ := d.Info()
		line := fmt.Sprintf("%s %v %d %d", p, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			line += " " + string(b)
		}
		out = append(out, line)
		return nil
	})
	sort.Strings(out)
	return out
}

func TestCheckNeverWrites(t *testing.T) {
	root := t.TempDir()
	record(t, root, "org", "acct", hostA, rec("cli-1", false))
	record(t, root, "org", "acct", hostB, rec("cli-1", true))
	record(t, root, "org", "acct2", "local_c", `not json`)
	before := snapshot(t, root)
	for _, id := range []string{hostA, hostB, "local_c", "local_missing", "local_../x"} {
		check(t, root, id)
	}
	after := snapshot(t, root)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("records folder changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestDefaultRootIsTheDesktopRecordsFolder(t *testing.T) {
	// Only the path is checked here. The real folder is never read.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	want := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
	if got := NewDesktopRecords("").Root; got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
	if got := NewDesktopRecords("/x/y").Root; got != "/x/y" {
		t.Fatalf("root = %q", got)
	}
}
