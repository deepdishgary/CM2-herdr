// Package hookcfg adds and removes the cm2d hook entries in a Claude Code
// settings.json. See docs/plans/2026-10-23-cm2d-daemon.md section 3.3.
package hookcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var now = time.Now // tests replace this

const (
	urlMarker = "/cm2d/v1/"
	// DefaultPort is the hooks port that needs no flag in the SessionStart command.
	DefaultPort = 47731
	// binaryName is the name Install requires, so isOurs can find the command.
	binaryName = "cm2d"
)

// The hook events that get an http hook, in file order.
var eventHooks = []string{"UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "Notification", "PostToolUse"}

type commandHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type httpHook struct {
	Type           string            `json:"type"`
	URL            string            `json:"url"`
	Timeout        int               `json:"timeout"`
	Headers        map[string]string `json:"headers"`
	AllowedEnvVars []string          `json:"allowedEnvVars"`
}

// entry is one hook object cm2d wants under one event.
type entry struct {
	event string
	hook  *node
}

func desired(cm2dPath string, port int) []entry {
	base := fmt.Sprintf("http://127.0.0.1:%d%s", port, urlMarker)
	h := func(path string, timeout int) *node {
		return fromGo(httpHook{
			Type: "http", URL: base + path, Timeout: timeout,
			Headers: map[string]string{
				"X-Cm2-Entrypoint":   "$CLAUDE_CODE_ENTRYPOINT",
				"X-Cm2-Host-Session": "$CLAUDE_CODE_HOST_SESSION_ID",
			},
			AllowedEnvVars: []string{"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_HOST_SESSION_ID"},
		})
	}
	es := []entry{{"SessionStart", fromGo(commandHook{
		Type: "command", Command: sessionStartCommand(cm2dPath, port), Timeout: 5,
	})}}
	for _, ev := range eventHooks {
		es = append(es, entry{ev, h("event", 5)})
	}
	return append(es, entry{"PermissionRequest", h("permission", 600)})
}

// sessionStartCommand is the SessionStart command line. A port other than
// the default goes in as a flag, so the forwarder posts to the right daemon.
func sessionStartCommand(cm2dPath string, port int) string {
	c := shellQuote(cm2dPath) + " hook session-start"
	if port != DefaultPort {
		c += " --port " + strconv.Itoa(port)
	}
	return c
}

// shellWords splits a command line made of bare words, single-quoted
// strings and backslash escapes (all that shellQuote makes). It reports false
// for anything else, such as an open quote.
func shellWords(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	in := false // a word is open
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			in = true
		case '\\':
			if i+1 >= len(s) {
				return nil, false
			}
			i++
			cur.WriteByte(s[i])
			in = true
		case ' ', '\t':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words, true
}

// isSessionStartCommand tells whether c is "<path>/cm2d hook session-start"
// with an optional "--port N". A foreign command that only contains these
// words does not match.
func isSessionStartCommand(c string) bool {
	w, ok := shellWords(c)
	if !ok || len(w) < 3 || filepath.Base(w[0]) != binaryName || w[1] != "hook" || w[2] != "session-start" {
		return false
	}
	switch len(w) {
	case 3:
		return true
	case 5:
		n, err := strconv.Atoi(w[4])
		return w[3] == "--port" && err == nil && n >= 1 && n <= 65535
	}
	return false
}

// shellQuote quotes p only when the shell would split or expand it.
func shellQuote(p string) string {
	safe := p != ""
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./-+@:,=~", r)) {
			safe = false
			break
		}
	}
	if safe {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// isOurs tells whether a hook object is a cm2d entry.
func isOurs(h *node) bool {
	if u, ok := h.get("url").str(); ok && strings.Contains(u, urlMarker) {
		return true
	}
	if c, ok := h.get("command").str(); ok && isSessionStartCommand(c) {
		return true
	}
	return false
}

// Install adds the cm2d hooks to the settings file at settingsPath.
// It returns changed=false and writes nothing when they are all present.
// Otherwise it first copies the old file to a timestamped backup (none if
// the file did not exist).
func Install(settingsPath, cm2dPath string, port int) (changed bool, backup string, err error) {
	if cm2dPath == "" {
		return false, "", errors.New("hookcfg: cm2d path is empty")
	}
	if port < 1 || port > 65535 {
		return false, "", fmt.Errorf("hookcfg: bad port %d", port)
	}
	if b := filepath.Base(cm2dPath); b != binaryName {
		return false, "", fmt.Errorf("hookcfg: the binary must be named %s, not %s (Uninstall could not find the hook)", binaryName, b)
	}
	return apply(settingsPath, true, func(data []byte) ([]byte, bool, error) {
		return InstallData(data, cm2dPath, port)
	})
}

// Uninstall removes every cm2d hook entry from the settings file.
func Uninstall(settingsPath string) (changed bool, backup string, err error) {
	return apply(settingsPath, false, UninstallData)
}

// InstallData is Install on bytes, for dry runs. Empty input counts as {}.
func InstallData(data []byte, cm2dPath string, port int) ([]byte, bool, error) {
	root, nl, err := parseSettings(data)
	if err != nil {
		return nil, false, err
	}
	changed, err := install(root, desired(cm2dPath, port))
	if err != nil || !changed {
		return data, false, err
	}
	return render(root, nl), true, nil
}

// UninstallData is Uninstall on bytes, for dry runs.
func UninstallData(data []byte) ([]byte, bool, error) {
	root, nl, err := parseSettings(data)
	if err != nil {
		return nil, false, err
	}
	changed, err := uninstall(root)
	if err != nil || !changed {
		return data, false, err
	}
	return render(root, nl), true, nil
}

func parseSettings(data []byte) (root *node, trailingNL bool, err error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &node{kind: object}, true, nil
	}
	root, err = parseTree(data)
	if err != nil {
		return nil, false, fmt.Errorf("hookcfg: settings file: %w", err)
	}
	if root.kind != object {
		return nil, false, errors.New("hookcfg: settings file is not a JSON object")
	}
	return root, bytes.HasSuffix(bytes.TrimRight(data, " \t"), []byte("\n")), nil
}

func render(root *node, trailingNL bool) []byte {
	var b bytes.Buffer
	root.write(&b, true, 0)
	if trailingNL {
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// hooksOf returns the "hooks" object, creating it when create is set.
func hooksOf(root *node, create bool) (*node, bool, error) {
	h := root.get("hooks")
	if h == nil {
		if !create {
			return nil, false, nil
		}
		h = &node{kind: object}
		root.set("hooks", h)
		return h, true, nil
	}
	if h.kind != object {
		return nil, false, errors.New(`hookcfg: "hooks" is not an object`)
	}
	return h, false, nil
}

func install(root *node, want []entry) (bool, error) {
	hooks, changed, err := hooksOf(root, true)
	if err != nil {
		return false, err
	}
	for _, e := range want {
		arr := hooks.get(e.event)
		if arr == nil {
			arr = &node{kind: array}
			hooks.set(e.event, arr)
			changed = true
		} else if arr.kind != array {
			return false, fmt.Errorf("hookcfg: hooks.%s is not an array", e.event)
		}
		if installOne(arr, e.hook) {
			changed = true
		}
	}
	return changed, nil
}

// installOne makes arr hold exactly one cm2d entry equal to hook.
func installOne(arr, hook *node) bool {
	type pos struct{ group, hook int }
	var found []pos
	for gi, g := range arr.items {
		hs := g.get("hooks")
		if hs == nil || hs.kind != array {
			continue
		}
		for hi, h := range hs.items {
			if h.kind == object && isOurs(h) {
				found = append(found, pos{gi, hi})
			}
		}
	}
	if len(found) == 1 && sameJSON(arr.items[found[0].group].get("hooks").items[found[0].hook], hook) {
		return false
	}
	if len(found) == 0 {
		arr.items = append(arr.items, fromGoGroup(hook))
		return true
	}
	// Replace the first in place, drop the others.
	arr.items[found[0].group].get("hooks").items[found[0].hook] = hook
	for _, p := range found[1:] {
		hs := arr.items[p.group].get("hooks")
		hs.items[p.hook] = nil
	}
	pruneNil(arr)
	return true
}

// sameJSON compares two nodes by value, ignoring key order and layout.
func sameJSON(a, b *node) bool {
	var x, y any
	if json.Unmarshal([]byte(a.compact()), &x) != nil || json.Unmarshal([]byte(b.compact()), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func fromGoGroup(hook *node) *node {
	return &node{kind: object, members: []member{
		{raw: `"hooks"`, name: "hooks", val: &node{kind: array, items: []*node{hook}}},
	}}
}

// pruneNil removes nil hook items, then groups that were left empty by that.
func pruneNil(arr *node) {
	var groups []*node
	for _, g := range arr.items {
		hs := g.get("hooks")
		if hs == nil || hs.kind != array {
			groups = append(groups, g)
			continue
		}
		had := false
		var keep []*node
		for _, h := range hs.items {
			if h == nil {
				had = true
				continue
			}
			keep = append(keep, h)
		}
		hs.items = keep
		if had && len(keep) == 0 {
			continue
		}
		groups = append(groups, g)
	}
	arr.items = groups
}

func uninstall(root *node) (bool, error) {
	hooks, _, err := hooksOf(root, false)
	if err != nil || hooks == nil {
		return false, err
	}
	changed := false
	var emptied []string
	for _, m := range hooks.members {
		arr := m.val
		if arr.kind != array {
			continue
		}
		removed := false
		for _, g := range arr.items {
			hs := g.get("hooks")
			if hs == nil || hs.kind != array {
				continue
			}
			for i, h := range hs.items {
				if h.kind == object && isOurs(h) {
					hs.items[i] = nil
					removed = true
				}
			}
		}
		if !removed {
			continue
		}
		changed = true
		pruneNil(arr)
		if len(arr.items) == 0 {
			emptied = append(emptied, m.name)
		}
	}
	for _, name := range emptied {
		hooks.remove(name)
	}
	if changed && len(hooks.members) == 0 {
		root.remove("hooks")
	}
	return changed, nil
}

// apply reads the file, runs fn, and writes the result with a backup.
func apply(settingsPath string, createIfMissing bool, fn func([]byte) ([]byte, bool, error)) (bool, string, error) {
	path, err := resolve(settingsPath)
	if err != nil {
		return false, "", err
	}
	old, err := os.ReadFile(path)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return false, "", fmt.Errorf("hookcfg: read settings: %w", err)
	}
	if missing && !createIfMissing {
		return false, "", nil
	}
	out, changed, err := fn(old)
	if err != nil || !changed {
		return false, "", err
	}
	mode := fs.FileMode(0o600)
	backup := ""
	if !missing {
		st, err := os.Stat(path)
		if err != nil {
			return false, "", fmt.Errorf("hookcfg: stat settings: %w", err)
		}
		mode = st.Mode().Perm()
		if backup, err = writeBackup(path, old, mode); err != nil {
			return false, "", err
		}
	}
	if err := writeAtomic(path, out, mode); err != nil {
		return false, backup, err
	}
	return true, backup, nil
}

// resolve follows a symlink, so the target is replaced and the link stays.
func resolve(p string) (string, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return "", fmt.Errorf("hookcfg: stat %s: %w", p, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return p, nil
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("hookcfg: resolve %s: %w", p, err)
	}
	return r, nil
}

// writeBackup copies the old bytes to <path>.cm2d-bak-<timestamp>.
func writeBackup(path string, old []byte, mode fs.FileMode) (string, error) {
	stamp := now().Format("20060102-150405")
	for i := 0; i < 100; i++ {
		name := path + ".cm2d-bak-" + stamp
		if i > 0 {
			name = fmt.Sprintf("%s-%d", name, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("hookcfg: write backup: %w", err)
		}
		_, werr := f.Write(old)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(name)
			return "", fmt.Errorf("hookcfg: write backup: %w", werr)
		}
		return name, nil
	}
	return "", errors.New("hookcfg: no free backup name")
}

// writeAtomic writes through a temp file and a rename.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("hookcfg: write settings: %w", err)
	}
	name := tmp.Name()
	_, err = io.Copy(tmp, bytes.NewReader(data))
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
		return fmt.Errorf("hookcfg: write settings: %w", err)
	}
	return nil
}
