package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"cm2-claude/internal/hookcfg"
)

// settingsPath is the --settings value, or ~/.claude/settings.json.
func settingsPath(e *env, flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	home, err := e.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func cmdInstallHooks(args []string, e *env) int {
	fl := newFlagSet(e, "install-hooks")
	port := fl.Int("port", defaultPort(e), "hooks port")
	settings := fl.String("settings", "", "settings file (default ~/.claude/settings.json)")
	dry := fl.Bool("dry-run", false, "show the change, write nothing")
	if err := fl.Parse(args); err != nil {
		return exitUsage
	}
	if fl.NArg() != 0 || *port < 1 || *port > 65535 {
		fmt.Fprintln(e.Stderr, "usage: cm2d install-hooks [--port N] [--settings PATH] [--dry-run]")
		return exitUsage
	}
	path, err := settingsPath(e, *settings)
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	exe, err := e.Exe()
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: cannot find this binary: %v\n", err)
		return exitError
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}

	if *dry {
		return dryRun(e, path, func(data []byte) ([]byte, bool, error) {
			return hookcfg.InstallData(data, exe, *port)
		})
	}
	changed, backup, err := hookcfg.Install(path, exe, *port)
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	if !changed {
		fmt.Fprintf(e.Stdout, "%s already has the cm2d hooks; nothing to change\n", path)
	} else {
		fmt.Fprintf(e.Stdout, "added the cm2d hooks to %s\n", path)
		if backup != "" {
			fmt.Fprintf(e.Stdout, "backup of the old file: %s\n", backup)
		}
	}
	return exitOK
}

func cmdUninstallHooks(args []string, e *env) int {
	fl := newFlagSet(e, "uninstall-hooks")
	settings := fl.String("settings", "", "settings file (default ~/.claude/settings.json)")
	dry := fl.Bool("dry-run", false, "show the change, write nothing")
	if err := fl.Parse(args); err != nil {
		return exitUsage
	}
	if fl.NArg() != 0 {
		fmt.Fprintln(e.Stderr, "usage: cm2d uninstall-hooks [--settings PATH] [--dry-run]")
		return exitUsage
	}
	path, err := settingsPath(e, *settings)
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	if *dry {
		return dryRun(e, path, hookcfg.UninstallData)
	}
	changed, backup, err := hookcfg.Uninstall(path)
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	if !changed {
		fmt.Fprintf(e.Stdout, "%s has no cm2d hooks; nothing to change\n", path)
		return exitOK
	}
	fmt.Fprintf(e.Stdout, "removed the cm2d hooks from %s\nbackup of the old file: %s\n", path, backup)
	return exitOK
}

// dryRun prints the change fn would make to the settings file.
func dryRun(e *env, path string, fn func([]byte) ([]byte, bool, error)) int {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	next, changed, err := fn(old)
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	if !changed {
		fmt.Fprintf(e.Stdout, "dry run: %s would not change\n", path)
		return exitOK
	}
	fmt.Fprintf(e.Stdout, "dry run: %s would change as follows; nothing written\n", path)
	for _, l := range lineDiff(string(old), string(next)) {
		fmt.Fprintln(e.Stdout, l)
	}
	return exitOK
}

// lineDiff lists the lines that differ, from a longest-common-subsequence
// match. Lines only in b start with "+ ", lines only in a with "- ".
func lineDiff(a, b string) []string {
	x, y := splitLines(a), splitLines(b)
	// lcs[i][j] is the common length of x[i:] and y[j:].
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			i++
			j++
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			out = append(out, "+ "+y[j])
			j++
		default:
			out = append(out, "- "+x[i])
			i++
		}
	}
	return out
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
