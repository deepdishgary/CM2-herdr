// Package focus brings a session's window to the front when its pad key is pressed.
// See docs/plans/2026-10-23-cm2d-daemon.md section 3.6.
package focus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"cm2-claude/internal/session"
)

// Timeout is the longest one Focus call may take.
const Timeout = 3 * time.Second

const defaultTerminalApp = "Ghostty"

// Runner runs an external command. ExecRunner is the production one.
type Runner func(ctx context.Context, name string, args ...string) error

// HerdrFocuser moves Herdr's focus to a pane. *herdr.Client satisfies it.
type HerdrFocuser interface {
	Focus(ctx context.Context, pane string) error
}

// DesktopChats tells whether a desktop chat can be focused by its record id.
type DesktopChats interface {
	// Check reports ok when hostID names exactly one live desktop record. When
	// not ok, reason says why in a few words. A reason never has a title.
	Check(hostID string) (ok bool, reason string)
}

// continueRoute is the one deep link cm2d may open. It only navigates the
// desktop window to an existing record. It never imports, adopts or unarchives.
const continueRoute = "claude://code/continue?session="

// bannedInRoute lists words no URL we open may contain (lower case).
var bannedInRoute = []string{"resume", "continue?session=last", "needs-input", "epitaxy"}

// Focuser focuses sessions.
type Focuser struct {
	Run         Runner
	Herdr       HerdrFocuser
	TerminalApp string       // default "Ghostty"
	Desktop     DesktopChats // nil: desktop sessions only raise the app
	Log         *slog.Logger // nil: no log

	// timeout is for tests only; zero means Timeout.
	timeout time.Duration
}

// ExecRunner runs the command with exec.CommandContext.
func ExecRunner(ctx context.Context, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Focus brings the session to the front.
//   - Herdr pane: focus the pane in Herdr, then raise the terminal app. If Herdr
//     fails, still raise the terminal app.
//   - Desktop session with a checked record id: open claude://code/continue
//     for that record. Without one: raise the Claude app. The resume deep
//     link is never used.
//   - Anything else: raise the terminal app.
func (f *Focuser) Focus(ctx context.Context, s *session.Session) error {
	if s == nil {
		return errors.New("focus: no session")
	}
	if f.Run == nil {
		return errors.New("focus: no runner")
	}
	d := f.timeout
	if d <= 0 {
		d = Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	switch {
	case s.HerdrPane != "":
		// A Herdr failure is not fatal: raising the terminal still helps.
		if f.Herdr != nil {
			_ = f.Herdr.Focus(ctx, s.HerdrPane)
		}
		return f.raiseTerminal(ctx)
	case s.Frontend == session.FrontendDesktop:
		// The resume deep link is banned: on 2026-10-23 it forked a live
		// session (a second claude process on the same session id, which
		// then acted on its inherited plan). See docs/design.md decision 4.
		// The continue route only navigates, and only for a checked record.
		// Known limits: the records can change between Check and open (a short
		// window), and Check does not prove a claude process is alive for the
		// record. Both are accepted: the route never starts a process by itself.
		url, why := f.desktopURL(s)
		if url != "" {
			return f.Run(ctx, "open", url)
		}
		f.logInfo("desktop focus: raise the app only", s, why)
		return f.Run(ctx, "open", "-a", "Claude")
	default:
		return f.raiseTerminal(ctx)
	}
}

func (f *Focuser) raiseTerminal(ctx context.Context) error {
	app := f.TerminalApp
	if app == "" {
		app = defaultTerminalApp
	}
	return f.Run(ctx, "open", "-a", app)
}

// desktopURL returns the continue URL for a desktop session, or "" and the
// reason it must fall back to raising the app.
func (f *Focuser) desktopURL(s *session.Session) (url, why string) {
	switch {
	case s.HostSession == "":
		return "", "no host id"
	case s.HostConflict:
		return "", "host conflict"
	case s.Ended:
		return "", "session ended"
	case f.Desktop == nil:
		return "", "no desktop checker"
	case !session.ValidHostID(s.HostSession):
		return "", "invalid host id"
	}
	if ok, reason := f.Desktop.Check(s.HostSession); !ok {
		if reason == "" {
			reason = "check failed"
		}
		return "", reason
	}
	url = continueRoute + s.HostSession
	low := strings.ToLower(url)
	for _, w := range bannedInRoute {
		if strings.Contains(low, w) {
			return "", "banned word in route"
		}
	}
	return url, ""
}

func (f *Focuser) logInfo(msg string, s *session.Session, why string) {
	lg := f.Log
	if lg == nil {
		lg = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	lg.Info(msg, "session", s.Key, "reason", why)
}
