package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"cm2-claude/internal/daemon"
)

func cmdStatus(ctx context.Context, args []string, e *env) int {
	fs := newFlagSet(e, "status")
	port := fs.Int("port", defaultPort(e), "hooks port")
	asJSON := fs.Bool("json", false, "print the raw JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 || *port < 1 || *port > 65535 {
		fmt.Fprintln(e.Stderr, "usage: cm2d status [--port N] [--json]")
		return exitUsage
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(*port))
	body, err := fetchStatus(ctx, addr, 3*time.Second)
	if err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: no daemon answered on %s: %v\n", addr, err)
		return exitError
	}
	if *asJSON {
		e.Stdout.Write(body)
		fmt.Fprintln(e.Stdout)
		return exitOK
	}
	var doc daemon.StatusDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		fmt.Fprintf(e.Stderr, "cm2d: bad status reply: %v\n", err)
		return exitError
	}
	printStatus(e.Stdout, doc)
	return exitOK
}

// fetchStatus gets the status JSON from a daemon at addr.
func fetchStatus(ctx context.Context, addr string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/cm2d/v1/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The daemon says why in the body, e.g. "daemon is stopping".
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if m := strings.TrimSpace(string(msg)); m != "" {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, m)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func printStatus(w io.Writer, d daemon.StatusDoc) {
	fmt.Fprintf(w, "cm2d %s, up since %s\n", d.Version, d.StartedAt.Local().Format("2006-01-02 15:04:05"))
	pad := "off (--no-device)"
	if d.Pad.Enabled {
		pad = "not connected"
		if d.Pad.Connected {
			pad = "connected"
		}
	}
	if d.Pad.PaintPausedUntil != nil && d.Pad.PaintPausedUntil.After(time.Now()) {
		pad += ", painting paused until " + d.Pad.PaintPausedUntil.Local().Format("15:04:05")
	}
	fmt.Fprintf(w, "pad: %s\n", pad)
	switch {
	case !d.Herdr.Watching:
		fmt.Fprintln(w, "herdr: not watched")
	case d.Herdr.Available:
		fmt.Fprintf(w, "herdr: %s, %d agents\n", d.Herdr.Version, d.Herdr.Agents)
	default:
		fmt.Fprintln(w, "herdr: not running")
	}
	fmt.Fprintf(w, "held permission requests: %d, dropped events: %d\n", d.Held, d.EventsDropped)
	if len(d.Sessions) == 0 {
		fmt.Fprintln(w, "no sessions")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tSTATE\tFRONTEND\tLABEL\tKIND\tPENDING\tSESSION\tHOST")
	for _, s := range d.Sessions {
		key := "-"
		if s.Slot >= 0 {
			key = strconv.Itoa(s.Slot + 1)
			if s.Slot == d.Selected {
				key += "*"
			}
		}
		state := s.State
		if s.Ended {
			state += " (ended)"
		}
		pend := strconv.Itoa(s.Pending)
		if s.Question {
			pend += " +question"
		}
		host := "-"
		if s.HostSession != "" {
			host = s.HostSession
		}
		if s.HostConflict {
			host += " (conflict)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", key, state, s.Frontend, s.Label, s.Kind, pend, s.Key, host)
	}
	tw.Flush()
	fmt.Fprintln(w, "(* = selected key; keys are numbered 1 to 10)")
}
