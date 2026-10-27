package focus

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"cm2-claude/internal/session"
)

// maxRecord caps the size of one desktop record we read.
const maxRecord = 1 << 20

// DesktopRecords checks desktop chats against the app's record files:
// <Root>/<org>/<account>/local_<id>.json. It only reads.
type DesktopRecords struct {
	Root string
}

// DefaultDesktopRoot is the folder where the Claude desktop app keeps its records.
func DefaultDesktopRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
}

// NewDesktopRecords returns a checker for root. An empty root means the default.
func NewDesktopRecords(root string) *DesktopRecords {
	if root == "" {
		root = DefaultDesktopRoot()
	}
	return &DesktopRecords{Root: root}
}

// deskRecord is the part of a desktop record we read. The title is not read.
type deskRecord struct {
	CLISessionID string `json:"cliSessionId"`
	IsArchived   bool   `json:"isArchived"`
}

// Check implements DesktopChats. It reads the folder fresh on every call.
func (d *DesktopRecords) Check(hostID string) (bool, string) {
	// No path is built from an id that has not passed this check.
	if !session.ValidHostID(hostID) {
		return false, "invalid host id"
	}
	if d == nil || d.Root == "" {
		return false, "no records folder"
	}
	files, why := d.recordFiles()
	if why != "" {
		return false, why
	}
	name := hostID + ".json"
	var target string
	for _, p := range files {
		if filepath.Base(p) == name {
			if target != "" {
				return false, "more than one record"
			}
			target = p
		}
	}
	if target == "" {
		return false, "no record"
	}
	rec, ok := readRecord(target)
	if !ok {
		return false, "record unreadable"
	}
	if rec.IsArchived {
		return false, "record archived"
	}
	if rec.CLISessionID == "" {
		return false, "record has no cli session id"
	}
	for _, p := range files {
		if p == target {
			continue
		}
		// A record we cannot read might be a live twin (a half-written file,
		// a wrong type, too large), so it blocks focus too.
		other, ok := readRecord(p)
		if !ok {
			return false, "another record is unreadable"
		}
		if !other.IsArchived && other.CLISessionID == rec.CLISessionID {
			return false, "another active record has the same cli session"
		}
	}
	return true, ""
}

// recordFiles lists <Root>/<org>/<account>/local_*.json. It fails closed: a
// folder it cannot read, a symlinked org or account folder, or a local_*.json
// that is not a regular file could hide a live twin, so it returns a reason.
func (d *DesktopRecords) recordFiles() (files []string, why string) {
	orgs, err := os.ReadDir(d.Root)
	if err != nil {
		return nil, "records folder unreadable"
	}
	for _, org := range orgs {
		if org.Type()&os.ModeSymlink != 0 {
			return nil, "symlink in records folder"
		}
		if !org.IsDir() {
			continue
		}
		accts, err := os.ReadDir(filepath.Join(d.Root, org.Name()))
		if err != nil {
			return nil, "records folder unreadable"
		}
		for _, acct := range accts {
			if acct.Type()&os.ModeSymlink != 0 {
				return nil, "symlink in records folder"
			}
			if !acct.IsDir() {
				continue
			}
			dir := filepath.Join(d.Root, org.Name(), acct.Name())
			ents, err := os.ReadDir(dir)
			if err != nil {
				return nil, "records folder unreadable"
			}
			for _, e := range ents {
				n := e.Name()
				if !strings.HasPrefix(n, "local_") || !strings.HasSuffix(n, ".json") {
					continue
				}
				if !e.Type().IsRegular() {
					return nil, "record is not a regular file"
				}
				files = append(files, filepath.Join(dir, n))
			}
		}
	}
	return files, ""
}

// readRecord reads and parses one record. It reports false for any failure,
// including a wrong field type.
func readRecord(path string) (deskRecord, bool) {
	var rec deskRecord
	// Do not block on a FIFO and do not follow a symlink.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return rec, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return rec, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxRecord+1))
	if err != nil || len(b) > maxRecord {
		return rec, false
	}
	if json.Unmarshal(b, &rec) != nil {
		return rec, false
	}
	return rec, true
}
