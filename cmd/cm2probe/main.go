// Command cm2probe is a one-shot command line tool to check the Creator
// Micro 2 and back up its keymap. It is for a human at a terminal.
//
//	cm2probe devices [--descriptor] [--json]
//	cm2probe access [--request] [--json]
//	cm2probe call <method> [params-json] [--timeout D] [--allow-write]
//	cm2probe listen <seconds>
//	cm2probe backup <file> [--force]
//	cm2probe version
//
// Exit codes: 0 ok, 1 error, 2 usage, 3 Input Monitoring denied,
// 4 method refused by the safety guard.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"cm2-claude/internal/device"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	env := &Env{
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		Open:          device.OpenWith,
		Access:        device.Access,
		RequestAccess: device.RequestAccess,
		Devices:       device.Devices,
	}
	code := Run(ctx, os.Args[1:], env)
	stop()
	os.Exit(code)
}
