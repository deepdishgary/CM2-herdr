package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"cm2-claude/internal/device"
	"cm2-claude/internal/setup"
)

// openPad opens the pad. Tests replace it.
var openPad = func() (device.Transport, error) { return device.Open() }

// padAccess reports the Input Monitoring state. Tests replace it.
var padAccess = device.Access

const setupTimeout = 15 * time.Second

// defaultLayout is the layout file `cm2d setup` reads without --layout.
const defaultLayout = "config/layout.json"

const grantHelp = `Input Monitoring is not granted to this process.
Open System Settings > Privacy & Security > Input Monitoring, turn on the app
that runs cm2d (your terminal app), quit that app completely, and try again.`

// connectPad refuses to touch the pad while a daemon runs, then opens it.
func connectPad(ctx context.Context, e *env, port int) (*device.Client, bool) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if _, err := fetchStatus(ctx, addr, 500*time.Millisecond); err == nil {
		fmt.Fprintf(e.Stderr, "cm2d: a daemon runs on %s and owns the pad. Stop it first.\n", addr)
		return nil, false
	}
	if padAccess() == device.AccessDenied {
		fmt.Fprintln(e.Stderr, grantHelp)
		return nil, false
	}
	tr, err := openPad()
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: open the pad: %v\n", err)
		if errors.Is(err, device.ErrInputMonitoring) {
			fmt.Fprintln(e.Stderr, grantHelp)
		}
		return nil, false
	}
	return device.NewClient(tr, device.ClientOptions{Timeout: setupTimeout}), true
}

func cmdSetup(ctx context.Context, args []string, e *env) int {
	fs := newFlagSet(e, "setup")
	layoutPath := fs.String("layout", defaultLayout, "layout file")
	dry := fs.Bool("dry-run", false, "show the changes, write nothing")
	backupDir := fs.String("backup-dir", "backups", "where the backup of the old keymap goes")
	port := fs.Int("port", defaultPort(e), "hooks port, to check that no daemon runs")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(e.Stderr, "usage: cm2d setup [--layout FILE] [--dry-run] [--backup-dir DIR]")
		return exitUsage
	}
	layout, err := setup.LoadLayout(*layoutPath)
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	c, ok := connectPad(ctx, e, *port)
	if !ok {
		return exitError
	}
	defer c.Close()
	defer warnForeign(e, c)
	err = setup.Apply(ctx, c, layout, setup.Options{
		Timeout: setupTimeout, BackupDir: *backupDir, DryRun: *dry, RestoreCmd: "cm2d restore",
	}, e.Stdout)
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	return exitOK
}

func cmdRestore(ctx context.Context, args []string, e *env) int {
	fs := newFlagSet(e, "restore")
	backupDir := fs.String("backup-dir", "backups", "where the backup of the current keymap goes")
	port := fs.Int("port", defaultPort(e), "hooks port, to check that no daemon runs")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(e.Stderr, "usage: cm2d restore <backup.json> [--backup-dir DIR]")
		return exitUsage
	}
	backup, err := setup.LoadBackup(pos[0])
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	c, ok := connectPad(ctx, e, *port)
	if !ok {
		return exitError
	}
	defer c.Close()
	defer warnForeign(e, c)
	err = setup.Restore(ctx, c, backup, setup.Options{
		Timeout: setupTimeout, BackupDir: *backupDir, RestoreCmd: "cm2d restore",
	}, e.Stdout)
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	return exitOK
}

// warnForeign says so when another program was on the pad during the command.
func warnForeign(e *env, c *device.Client) {
	if n := c.Stats().Foreign; n > 0 {
		fmt.Fprintf(e.Stderr, "cm2d: warning: %d reply(ies) were not ours. Another program (the Input app?) is on the pad. Check the result.\n", n)
	}
}
