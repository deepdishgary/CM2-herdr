package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"cm2-claude/internal/herdr"
)

// watchTune lets tests change the watcher before it runs.
var watchTune func(*herdr.Watcher)

const herdrUsage = `usage: cm2d herdr <command> [--socket PATH | --session NAME]
commands:
  list [--json]     print every agent Herdr knows about
  watch             print a line for each change until Ctrl-C; keeps waiting while Herdr is down
  focus <pane_id>   call agent.focus (changes the live Herdr view)
options:
  --socket PATH     Herdr socket path
  --session NAME    Herdr session name: <config>/herdr/sessions/NAME/herdr.sock
  -h, --help        print this help
The socket is chosen in this order: --socket, --session, $HERDR_SOCKET_PATH,
$HERDR_SESSION, then <config>/herdr/herdr.sock. <config> is $XDG_CONFIG_HOME
if it is set, else ~/.config.
watch prints "herdr is not running; waiting" on stdout while Herdr is down,
and "herdr error: ..." on stderr when a read or the event stream fails.
`

// runHerdr runs "cm2d herdr ...". It returns the exit code: 0 on success or
// after help, 1 on a runtime error or when Herdr is not running, 2 on a usage
// error.
func runHerdr(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, herdrUsage)
		return 2
	}
	cmd := args[0]
	switch cmd {
	case "-h", "-help", "--help":
		fmt.Fprint(stdout, herdrUsage)
		return 0
	case "list", "watch", "focus":
	default:
		fmt.Fprintf(stderr, "cm2d: unknown herdr command %q\n%s", cmd, herdrUsage)
		return 2
	}
	fs := flag.NewFlagSet("herdr "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {} // runHerdr prints the usage itself
	socket := fs.String("socket", "", "Herdr socket path")
	session := fs.String("session", "", "Herdr session name")
	var asJSON *bool
	if cmd == "list" {
		asJSON = fs.Bool("json", false, "print the snapshot as JSON")
	}
	pos, err := parseInterspersed(fs, args[1:])
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, herdrUsage)
		return 0
	}
	if err != nil {
		fmt.Fprint(stderr, herdrUsage)
		return 2
	}
	want := 0
	if cmd == "focus" {
		want = 1
	}
	if len(pos) != want {
		fmt.Fprintf(stderr, "cm2d: herdr %s takes %d argument(s)\n%s", cmd, want, herdrUsage)
		return 2
	}
	if cmd == "focus" && strings.TrimSpace(pos[0]) == "" {
		fmt.Fprintf(stderr, "cm2d: herdr focus needs a pane id that is not blank\n%s", herdrUsage)
		return 2
	}

	client := herdr.NewClientSession(*socket, *session)
	var runErr error
	switch cmd {
	case "list":
		runErr = herdrList(ctx, client, *asJSON, stdout)
	case "watch":
		runErr = herdrWatch(ctx, client, stdout, stderr)
	case "focus":
		if runErr = client.Focus(ctx, pos[0]); runErr == nil {
			fmt.Fprintf(stdout, "focused %s\n", pos[0])
		}
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "cm2d: %v\n", runErr)
		return 1
	}
	return 0
}

// parseInterspersed parses flags that may come before or after positional
// arguments, which the flag package alone does not allow.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func herdrList(ctx context.Context, c *herdr.Client, asJSON bool, out io.Writer) error {
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	if !snap.Available {
		return fmt.Errorf("%w (socket %s)", herdr.ErrNotRunning, c.SocketPath)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(snap)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PANE\tKIND\tSTATUS\tFOCUSED\tSESSION\tCWD")
	for _, a := range snap.Agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", a.PaneID, dash(a.Kind), a.Status, yesNo(a.Focused), shortSession(a), cwdBase(a))
	}
	return tw.Flush()
}

// herdrWatch prints a line per change to out. It prints "herdr error: ..." to
// errOut for each error the watcher reports. A Herdr that is not running at
// the time of a read is no error: the watcher publishes it as a change, and
// the "herdr is not running; waiting" line shows it on out.
func herdrWatch(ctx context.Context, c *herdr.Client, out, errOut io.Writer) error {
	w := &herdr.Watcher{Client: c, OnError: func(err error) {
		fmt.Fprintf(errOut, "herdr error: %v\n", err)
	}}
	if watchTune != nil {
		watchTune(w)
	}
	var prev *herdr.Snapshot
	err := w.Run(ctx, func(s herdr.Snapshot) {
		stamp := time.Now().Format("15:04:05")
		for _, l := range changeLines(prev, s) {
			fmt.Fprintf(out, "%s %s\n", stamp, l)
		}
		prev = &s
	})
	if errors.Is(err, context.Canceled) {
		return nil // Ctrl-C is the normal way to stop
	}
	return err
}

// changeLines describes what changed from prev to cur, one line per change.
func changeLines(prev *herdr.Snapshot, cur herdr.Snapshot) []string {
	if !cur.Available {
		if prev == nil || prev.Available {
			return []string{"herdr is not running; waiting"}
		}
		return nil
	}
	if prev == nil || !prev.Available {
		lines := []string{fmt.Sprintf("herdr %s up, %d agents", cur.Version, len(cur.Agents))}
		for _, a := range cur.Agents {
			lines = append(lines, "+ "+agentLine(a))
		}
		return lines
	}
	old := make(map[string]herdr.Agent, len(prev.Agents))
	for _, a := range prev.Agents {
		old[a.PaneID] = a
	}
	var lines []string
	seen := make(map[string]bool, len(cur.Agents))
	for _, a := range cur.Agents {
		seen[a.PaneID] = true
		o, existed := old[a.PaneID]
		switch {
		case !existed:
			lines = append(lines, "+ "+agentLine(a))
		case !sameAgent(o, a):
			l := "~ " + agentLine(a)
			if o.Status != a.Status {
				l += fmt.Sprintf(" (was %s)", o.Status)
			}
			lines = append(lines, l)
		}
	}
	for _, a := range prev.Agents {
		if !seen[a.PaneID] {
			lines = append(lines, "- "+a.PaneID+" gone")
		}
	}
	if cur.FocusedPaneID != prev.FocusedPaneID {
		lines = append(lines, "focus: "+dash(cur.FocusedPaneID))
	}
	return lines
}

func sameAgent(a, b herdr.Agent) bool {
	return agentLine(a) == agentLine(b) && a.DisplayName == b.DisplayName &&
		((a.CompletionSeq == nil) == (b.CompletionSeq == nil)) &&
		(a.CompletionSeq == nil || *a.CompletionSeq == *b.CompletionSeq)
}

func agentLine(a herdr.Agent) string {
	return fmt.Sprintf("%s %s %s focused=%s session=%s cwd=%s",
		a.PaneID, dash(a.Kind), a.Status, yesNo(a.Focused), shortSession(a), cwdBase(a))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

// shortSession is the first 8 characters of the session id, or "-".
func shortSession(a herdr.Agent) string {
	if a.Session == nil || a.Session.Value == "" {
		return "-"
	}
	v := a.Session.Value
	if a.Session.Kind == "path" {
		v = filepath.Base(v)
	}
	if len(v) > 8 {
		v = v[:8]
	}
	return v
}

func cwdBase(a herdr.Agent) string {
	if a.CWD == "" {
		return "-"
	}
	return filepath.Base(a.CWD)
}
