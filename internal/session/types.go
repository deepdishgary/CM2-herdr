// Package session keeps the table of coding-agent sessions that the pad shows.
// See docs/plans/2026-10-23-cm2d-daemon.md section 3.1.
package session

import (
	"regexp"
	"time"
)

// Frontend is where a session runs, which decides how to focus it.
type Frontend string

const (
	FrontendDesktop  Frontend = "desktop"
	FrontendHerdr    Frontend = "herdr"
	FrontendTerminal Frontend = "terminal"
	FrontendUnknown  Frontend = "unknown"
)

// State is what a session key shows.
type State int

const (
	Idle       State = iota // waiting, already seen
	Working                 // the agent is running a turn
	NeedsInput              // a permission request or a question is waiting
	Done                    // finished and not yet seen
	Error                   // the turn ended on an API error
)

func (s State) String() string {
	switch s {
	case Idle:
		return "idle"
	case Working:
		return "working"
	case NeedsInput:
		return "needs-input"
	case Done:
		return "done"
	case Error:
		return "error"
	}
	return "unknown"
}

// HookEvent is one Claude Code hook event, already parsed by the hooks package.
type HookEvent struct {
	Name             string // SessionStart, UserPromptSubmit, Stop, StopFailure, SessionEnd, Notification, PostToolUse, PermissionRequest, PermissionReleased
	SessionID, CWD   string
	Attended         string // "1", "0", or "" (unknown); only the SessionStart command hook knows it
	Entrypoint       string // claude-desktop | cli | sdk-cli | ""
	HerdrPane        string
	HostSession      string // desktop record id (local_...), already validated; "" if unknown
	Source           string // SessionStart source: startup | resume | clear | compact
	NotificationType string
	ToolName         string
	RequestID        string // PermissionRequest and PermissionReleased
	IsQuestion       bool   // PermissionRequest for AskUserQuestion or ExitPlanMode
	At               time.Time
}

// Pending is one held permission request.
type Pending struct {
	RequestID, Tool string
	Since           time.Time
}

// Session is one row of the table.
type Session struct {
	Key        string // "claude:<session_id>" or "herdr:<pane_id>"
	SessionID  string // Claude session id; empty for non-Claude agents
	HerdrPane  string
	Kind       string // agent kind: claude, codex, ...
	Frontend   Frontend
	CWD, Label string
	State      State
	Pending    []Pending // oldest first
	Question   bool      // AskUserQuestion or ExitPlanMode is showing
	LastActive time.Time
	Ended      bool
	EndedAt    time.Time
	Slot       int // 0..Slots-1, or -1

	HostSession  string // desktop record id (local_...) of the first host id seen; "" if none
	HostConflict bool   // two different host ids were seen: the session is forked. Sticky.
}

// hostIDPattern is the shape of a desktop record id.
var hostIDPattern = regexp.MustCompile(`^local_[0-9A-Za-z-]{1,64}$`)

// ValidHostID tells whether id has the shape of a desktop record id.
func ValidHostID(id string) bool { return hostIDPattern.MatchString(id) }

// Config tunes the table. Zero values take the defaults below.
type Config struct {
	Slots             int           // default 10
	EndedGrace        time.Duration // default 10m
	IdleExpiry        time.Duration // unused since 2026-10-23: keys stay until replaced
	NeedsInputDelay   time.Duration // default 1s
	IgnoreCWDPrefixes []string      // default: ~/elara (expanded)
}
