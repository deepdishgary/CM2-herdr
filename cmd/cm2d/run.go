package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"

	"cm2-claude/internal/daemon"
	"cm2-claude/internal/focus"
	"cm2-claude/internal/herdr"
)

func newFlagSet(e *env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	return fs
}

// cmdRun runs the daemon until ctx ends.
func cmdRun(ctx context.Context, args []string, e *env) int {
	fs := newFlagSet(e, "run")
	port := fs.Int("port", defaultPort(e), "hooks port on 127.0.0.1")
	noDevice := fs.Bool("no-device", false, "do not open the pad (no approvals, no lights)")
	noHerdr := fs.Bool("no-herdr", false, "do not watch Herdr")
	verbose := fs.Bool("verbose", false, "log every hook event and its session")
	herdrSocket := fs.String("herdr-socket", "", "Herdr socket path (default: the usual Herdr socket)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(e.Stderr, "cm2d run: unexpected argument", fs.Arg(0))
		return exitUsage
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintln(e.Stderr, "cm2d run: bad --port")
		return exitUsage
	}

	cfg := runConfig(e, *port, *noDevice, *noHerdr, *herdrSocket)
	if *verbose {
		setVerbose(&cfg, e.Stderr)
	}

	if err := daemon.New(cfg).Run(ctx); err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: %v\n", err)
		return exitError
	}
	return exitOK
}

// setVerbose logs at debug level to w, in the daemon and in the focuser.
func setVerbose(cfg *daemon.Config, w io.Writer) {
	cfg.Logger = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if cfg.Focuser != nil {
		cfg.Focuser.Log = cfg.Logger
	}
}

// terminalApp is the terminal that comes forward on a session key press.
const terminalApp = "Ghostty"

// runConfig builds the daemon config for `cm2d run`.
func runConfig(e *env, port int, noDevice, noHerdr bool, herdrSocket string) daemon.Config {
	cfg := daemon.Config{
		Addr:     net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		NoDevice: noDevice,
		Logger:   slog.New(slog.NewTextHandler(e.Stderr, nil)),
		Version:  version,
	}
	f := &focus.Focuser{
		Run:         focus.ExecRunner,
		TerminalApp: terminalApp,
		Desktop:     focus.NewDesktopRecords(""), // the app's records folder
		Log:         cfg.Logger,
	}
	if !noHerdr {
		cfg.Herdr = herdr.NewClient(herdrSocket)
		f.Herdr = cfg.Herdr
	}
	cfg.Focuser = f
	return cfg
}
