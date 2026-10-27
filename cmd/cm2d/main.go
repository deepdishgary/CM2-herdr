// Command cm2d is the daemon that bridges the Work Louder Creator Micro 2
// macropad to Claude Code.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

const version = "0.1.0"

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// env is everything the commands take from the outside. Tests replace it.
type env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Exe            func() (string, error) // path of this binary
	Home           func() (string, error)
}

func osEnv() *env {
	return &env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, Exe: os.Executable, Home: os.UserHomeDir,
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], osEnv())
	stop()
	os.Exit(code)
}

// run runs one command and returns the exit code.
func run(ctx context.Context, args []string, e *env) int {
	if len(args) < 1 {
		usage(e.Stderr)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return cmdRun(ctx, rest, e)
	case "status":
		return cmdStatus(ctx, rest, e)
	case "hook":
		return cmdHook(ctx, rest, e)
	case "install-hooks":
		return cmdInstallHooks(rest, e)
	case "uninstall-hooks":
		return cmdUninstallHooks(rest, e)
	case "setup":
		return cmdSetup(ctx, rest, e)
	case "restore":
		return cmdRestore(ctx, rest, e)
	case "herdr":
		return runHerdr(ctx, rest, e.Stdout, e.Stderr)
	case "version":
		fmt.Fprintf(e.Stdout, "cm2d %s\n", version)
		return exitOK
	case "-h", "-help", "--help", "help":
		usage(e.Stdout)
		return exitOK
	}
	fmt.Fprintf(e.Stderr, "cm2d: unknown command %q\n", cmd)
	usage(e.Stderr)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: cm2d <command> [flags]
commands:
  run [--port N] [--no-device]        run the daemon (Ctrl-C stops it)
  status [--port N] [--json]          show what the running daemon sees
  hook session-start                  forwarder for the SessionStart command hook (reads stdin)
  install-hooks [--port N] [--settings PATH] [--dry-run]
                                      add the cm2d hooks to Claude Code's settings.json
  uninstall-hooks [--settings PATH] [--dry-run]
                                      remove them again
  setup [--layout FILE] [--dry-run]   write the agent-key layout to the pad (stop the daemon first)
  restore <backup.json>               write a keymap backup back to the pad
  herdr list|watch|focus              look at Herdr (see "cm2d herdr --help")
  version                             print the version
The hooks port is 47731. Set CM2D_PORT or pass --port to change it.
`)
}

// defaultPort is the hooks port: CM2D_PORT, else 47731.
func defaultPort(e *env) int {
	if v := e.Getenv("CM2D_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return defaultHooksPort
}

const defaultHooksPort = 47731
