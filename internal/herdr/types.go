// Package herdr is a small client for the Herdr socket API. It gives cm2d a
// live list of every agent in Herdr, with status, and a way to focus one.
//
// Herdr has no API stability promise, so decoding is lenient: unknown fields
// are ignored, optional fields may be missing, and one odd record never fails
// a whole list.
package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Status is an agent's state as Herdr reports it.
type Status string

const (
	StatusIdle    Status = "idle"
	StatusWorking Status = "working"
	StatusBlocked Status = "blocked"
	StatusDone    Status = "done"
	StatusUnknown Status = "unknown"
)

func parseStatus(s string) Status {
	switch st := Status(s); st {
	case StatusIdle, StatusWorking, StatusBlocked, StatusDone:
		return st
	}
	return StatusUnknown
}

// AgentSession links a pane to the agent's own session.
type AgentSession struct {
	Source string `json:"source,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Kind   string `json:"kind,omitempty"` // "id" or "path"
	Value  string `json:"value,omitempty"`
}

// Agent is one agent pane.
type Agent struct {
	PaneID         string        `json:"pane_id"`
	WorkspaceID    string        `json:"workspace_id"`
	TabID          string        `json:"tab_id"`
	TerminalID     string        `json:"terminal_id"`
	Kind           string        `json:"kind,omitempty"` // "claude", "codex", ...; "" if absent
	Name           string        `json:"name,omitempty"`
	DisplayName    string        `json:"display_name,omitempty"` // display_agent, else title, else Kind
	Status         Status        `json:"status"`
	Focused        bool          `json:"focused"`
	CompletionSeq  *uint64       `json:"completion_seq,omitempty"`
	StateChangeSeq uint64        `json:"state_change_seq"`
	Revision       uint64        `json:"revision"`
	CWD            string        `json:"cwd,omitempty"` // foreground_cwd if set, else cwd
	Session        *AgentSession `json:"session,omitempty"`
}

// ClaudeSessionID returns the Claude Code session id when Kind is "claude"
// and the session kind is "id". Otherwise it returns "".
func (a Agent) ClaudeSessionID() string {
	if a.Kind == "claude" && a.Session != nil && a.Session.Kind == "id" {
		return a.Session.Value
	}
	return ""
}

// Snapshot is the state of Herdr at one moment.
type Snapshot struct {
	Available     bool    `json:"available"` // false when the socket is missing or refuses
	Version       string  `json:"version,omitempty"`
	Protocol      int     `json:"protocol,omitempty"`
	Agents        []Agent `json:"agents"` // server order (workspace, tab, pane)
	FocusedPaneID string  `json:"focused_pane_id,omitempty"`
}

// APIError is an error response from Herdr.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("herdr: %s: %s", e.Code, e.Message)
}

// ErrNotRunning means the socket is missing or refuses connections.
var ErrNotRunning = errors.New("herdr is not running")

// Field helpers. A field of the wrong type reads as its zero value.

func rawString(m map[string]json.RawMessage, key string) string {
	var s string
	if v, ok := m[key]; ok && json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

func rawBool(m map[string]json.RawMessage, key string) bool {
	var b bool
	if v, ok := m[key]; ok && json.Unmarshal(v, &b) != nil {
		return false
	}
	return b
}

func rawUint(m map[string]json.RawMessage, key string) uint64 {
	var u uint64
	if v, ok := m[key]; ok && json.Unmarshal(v, &u) != nil {
		return 0
	}
	return u
}

func rawUintPtr(m map[string]json.RawMessage, key string) *uint64 {
	v, ok := m[key]
	if !ok {
		return nil
	}
	var u *uint64
	if json.Unmarshal(v, &u) != nil {
		return nil
	}
	return u
}

// decodeAgent reads one AgentInfo record. It reports false for a record that
// is not an object or has no pane id.
func decodeAgent(raw json.RawMessage) (Agent, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return Agent{}, false
	}
	a := Agent{
		PaneID:         rawString(m, "pane_id"),
		WorkspaceID:    rawString(m, "workspace_id"),
		TabID:          rawString(m, "tab_id"),
		TerminalID:     rawString(m, "terminal_id"),
		Kind:           rawString(m, "agent"),
		Name:           rawString(m, "name"),
		Status:         parseStatus(rawString(m, "agent_status")),
		Focused:        rawBool(m, "focused"),
		CompletionSeq:  rawUintPtr(m, "completion_seq"),
		StateChangeSeq: rawUint(m, "state_change_seq"),
		Revision:       rawUint(m, "revision"),
	}
	if a.PaneID == "" {
		return Agent{}, false
	}
	a.DisplayName = rawString(m, "display_agent")
	if a.DisplayName == "" {
		a.DisplayName = rawString(m, "title")
	}
	if a.DisplayName == "" {
		a.DisplayName = a.Kind
	}
	a.CWD = rawString(m, "foreground_cwd")
	if a.CWD == "" {
		a.CWD = rawString(m, "cwd")
	}
	if sm := m["agent_session"]; len(sm) > 0 {
		var s map[string]json.RawMessage
		if json.Unmarshal(sm, &s) == nil && s != nil {
			a.Session = &AgentSession{
				Source: rawString(s, "source"), Agent: rawString(s, "agent"),
				Kind: rawString(s, "kind"), Value: rawString(s, "value"),
			}
		}
	}
	return a, true
}

// decodeSnapshot reads the "snapshot" object of a session.snapshot reply.
func decodeSnapshot(raw json.RawMessage) (Snapshot, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return Snapshot{}, errors.New("herdr: malformed snapshot")
	}
	snap := Snapshot{
		Available:     true,
		Version:       rawString(m, "version"),
		Protocol:      int(rawUint(m, "protocol")),
		FocusedPaneID: rawString(m, "focused_pane_id"),
	}
	var recs []json.RawMessage
	if v, ok := m["agents"]; ok {
		if err := json.Unmarshal(v, &recs); err != nil {
			return Snapshot{}, fmt.Errorf("herdr: malformed agent list: %w", err)
		}
	}
	snap.Agents = make([]Agent, 0, len(recs))
	for _, r := range recs {
		if a, ok := decodeAgent(r); ok {
			snap.Agents = append(snap.Agents, a)
		}
	}
	return snap, nil
}

// sameState reports whether two snapshots match in the fields cm2d cares
// about. It ignores Revision, StateChangeSeq and terminal titles, which only
// add noise.
func sameState(a, b Snapshot) bool {
	if a.Available != b.Available || a.FocusedPaneID != b.FocusedPaneID || len(a.Agents) != len(b.Agents) {
		return false
	}
	for i := range a.Agents {
		x, y := a.Agents[i], b.Agents[i]
		if x.PaneID != y.PaneID || x.Kind != y.Kind || x.Status != y.Status || x.Focused != y.Focused ||
			x.DisplayName != y.DisplayName || x.CWD != y.CWD || sessionValue(x) != sessionValue(y) ||
			!sameSeq(x.CompletionSeq, y.CompletionSeq) {
			return false
		}
	}
	return true
}

func sessionValue(a Agent) string {
	if a.Session == nil {
		return ""
	}
	return a.Session.Value
}

func sameSeq(a, b *uint64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
