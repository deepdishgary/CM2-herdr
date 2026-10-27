// Package setup shows, applies and restores the pad's keymap.json.
// Both cm2probe and cm2d use it. All functions take an open device client.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"cm2-claude/internal/device"
)

// DefaultTimeout is the time limit per pad call when Options.Timeout is zero.
const DefaultTimeout = 15 * time.Second

// Options tunes Apply and Restore.
type Options struct {
	Timeout    time.Duration    // per pad call; default DefaultTimeout
	BackupDir  string           // where the pre-write backup goes; default "backups"
	DryRun     bool             // show the changes, write nothing
	RestoreCmd string           // text for "restore with: <RestoreCmd> <backup>"; default "cm2probe keymap restore"
	Now        func() time.Time // for backup names; default time.Now
}

func (o Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return DefaultTimeout
	}
	return o.Timeout
}

func (o Options) backupDir() string {
	if o.BackupDir == "" {
		return "backups"
	}
	return o.BackupDir
}

func (o Options) restoreCmd() string {
	if o.RestoreCmd == "" {
		return "cm2probe keymap restore"
	}
	return o.RestoreCmd
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}

// LoadLayout reads a layout file. Unknown fields are an error, so a
// misspelled field cannot pass silently.
func LoadLayout(path string) (device.LayoutFile, error) {
	var layout device.LayoutFile
	b, err := os.ReadFile(path)
	if err != nil {
		return layout, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&layout); err != nil {
		return layout, fmt.Errorf("layout file %s: %v", path, err)
	}
	return layout, nil
}

// LoadBackup reads a keymap.json backup and checks that it looks like one.
func LoadBackup(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) || !device.SameKeymap(b, b) {
		return nil, fmt.Errorf("%s is not a keymap.json document", path)
	}
	return b, nil
}

// ReadKeymap reads keymap.json from the pad.
func ReadKeymap(ctx context.Context, c *device.Client, timeout time.Duration) ([]byte, error) {
	rep, err := c.CallTimeout(ctx, timeout, "fs.read", map[string]string{"file": "keymap.json"})
	if err != nil {
		return nil, fmt.Errorf("fs.read: %w", err)
	}
	b, err := DecodeFSRead(rep.Result)
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, errors.New("the pad's keymap.json is not valid JSON")
	}
	return b, nil
}

// Show prints each layer of the pad's active profile.
func Show(ctx context.Context, c *device.Client, timeout time.Duration, out io.Writer) error {
	cur, err := ReadKeymap(ctx, c, timeout)
	if err != nil {
		return fmt.Errorf("keymap: %w", err)
	}
	return ShowDoc(cur, out)
}

// ShowDoc prints each layer of the active profile of a keymap document.
func ShowDoc(doc []byte, out io.Writer) error {
	var km struct {
		ActiveProfileID int `json:"activeProfileId"`
		Profiles        []struct {
			ID     int `json:"id"`
			Layers []struct {
				ID     int    `json:"id"`
				Name   string `json:"name"`
				Layout struct {
					Keymap   [][]string `json:"keymap"`
					Encoders [][]string `json:"encoders"`
				} `json:"layout"`
			} `json:"layers"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(doc, &km); err != nil {
		return err
	}
	for _, p := range km.Profiles {
		if p.ID != km.ActiveProfileID {
			continue
		}
		for _, l := range p.Layers {
			fmt.Fprintf(out, "layer %d %q\n", l.ID, l.Name)
			for i, row := range l.Layout.Keymap {
				fmt.Fprintf(out, "  row %d: %v\n", i, row)
			}
			for i, e := range l.Layout.Encoders {
				fmt.Fprintf(out, "  encoder %d [cw ccw press]: %v\n", i, e)
			}
		}
	}
	return nil
}

// Apply merges the layout into the pad's keymap, backs up the current file,
// writes, reads back and verifies. With DryRun it prints the changes only.
func Apply(ctx context.Context, c *device.Client, layout device.LayoutFile, o Options, out io.Writer) error {
	cur, err := ReadKeymap(ctx, c, o.timeout())
	if err != nil {
		return fmt.Errorf("keymap: %w", err)
	}
	next, changes, err := device.ApplyLayouts(cur, layout)
	if err != nil {
		return fmt.Errorf("keymap apply: %w", err)
	}
	fmt.Fprintf(out, "%d key(s) change\n", len(changes))
	for _, ch := range changes {
		fmt.Fprintf(out, "  %-26s %s -> %s\n", ch.Where, ch.Old, ch.New)
	}
	if len(changes) == 0 {
		fmt.Fprintln(out, "nothing to write")
		return nil
	}
	return write(ctx, c, cur, next, o, out)
}

// Restore writes a backup keymap back to the pad, after backing up the
// current file.
func Restore(ctx context.Context, c *device.Client, backup []byte, o Options, out io.Writer) error {
	cur, err := ReadKeymap(ctx, c, o.timeout())
	if err != nil {
		return fmt.Errorf("keymap: %w", err)
	}
	if device.SameKeymap(cur, backup) {
		fmt.Fprintln(out, "the pad already holds this keymap; nothing to write")
		return nil
	}
	return write(ctx, c, cur, backup, o, out)
}

// write backs up cur, writes want, then reads back and compares.
func write(ctx context.Context, c *device.Client, cur, want []byte, o Options, out io.Writer) error {
	if o.DryRun {
		fmt.Fprintln(out, "dry run: nothing written")
		return nil
	}
	bpath, err := backup(o, cur)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "backup of the current keymap: %s\n", bpath)

	res, err := c.CallTimeout(ctx, o.timeout(), "fs.write", map[string]string{"file": "keymap.json", "data": string(want)})
	if err != nil {
		return fmt.Errorf("fs.write: %v\nThe pad may hold a partial write. Restore with: %s %s", err, o.restoreCmd(), bpath)
	}
	fmt.Fprintf(out, "fs.write reply: %s\n", compact(res.Raw))

	back, err := ReadKeymap(ctx, c, o.timeout())
	if err != nil {
		return fmt.Errorf("read back: %w", err)
	}
	if !device.SameKeymap(back, want) {
		return fmt.Errorf("VERIFY FAILED: the pad's keymap differs from what was written. Restore with: %s %s", o.restoreCmd(), bpath)
	}
	fmt.Fprintln(out, "verified: the pad holds the new keymap")
	return nil
}

// backup saves cur to a new timestamped file in the backup folder.
func backup(o Options, cur []byte) (string, error) {
	dir := o.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	stamp := o.now().Format("20060102-150405")
	bpath := filepath.Join(dir, "keymap-"+stamp+".json")
	for n := 2; ; n++ { // two writes in the same second must not share a backup name
		if _, err := os.Lstat(bpath); err != nil {
			break
		}
		bpath = filepath.Join(dir, fmt.Sprintf("keymap-%s-%d.json", stamp, n))
	}
	if err := WriteNew(bpath, cur, false); err != nil {
		return "", fmt.Errorf("backup: %v (nothing written to the pad)", err)
	}
	return bpath, nil
}

// DecodeFSRead gets the file bytes out of an fs.read result. prototype/
// device.js accepts a JSON string, or an object with a "data" member. Both
// are handled. If "data" is not a string (the pad sent JSON, not text), the
// JSON value is the file.
func DecodeFSRead(result json.RawMessage) ([]byte, error) {
	var s string
	if json.Unmarshal(result, &s) == nil {
		return []byte(s), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		return nil, fmt.Errorf("fs.read result is neither a string nor an object: %.80s", result)
	}
	data, ok := obj["data"]
	if !ok {
		return nil, errors.New(`fs.read result is an object without a "data" member`)
	}
	if json.Unmarshal(data, &s) == nil {
		return []byte(s), nil
	}
	return data, nil
}

// WriteNew writes a file with mode 0600. It fails if the file exists, unless
// force is set.
func WriteNew(path string, data []byte, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func compact(raw []byte) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}
