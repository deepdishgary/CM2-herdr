package daemon

import (
	"errors"
	"time"
)

// StatusDoc is the body of GET /cm2d/v1/status.
type StatusDoc struct {
	Version       string          `json:"version"`
	StartedAt     time.Time       `json:"started_at"`
	Pad           PadStatus       `json:"pad"`
	Herdr         HerdrStatus     `json:"herdr"`
	Selected      int             `json:"selected"` // slot index, or -1
	Held          int             `json:"held"`     // permission requests held now
	EventsDropped uint64          `json:"events_dropped"`
	Ignored       int             `json:"ignored_sessions"` // headless or ignored-cwd sessions seen
	Sessions      []SessionStatus `json:"sessions"`
}

// PadStatus describes the pad connection.
type PadStatus struct {
	Enabled          bool       `json:"enabled"`
	Connected        bool       `json:"connected"`
	PaintPausedUntil *time.Time `json:"paint_paused_until,omitempty"`
}

// HerdrStatus describes the Herdr link.
type HerdrStatus struct {
	Watching  bool   `json:"watching"`
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Agents    int    `json:"agents"`
}

// SessionStatus is one session row.
type SessionStatus struct {
	Key       string `json:"key"`
	Kind      string `json:"kind"`
	Frontend  string `json:"frontend"`
	Label     string `json:"label"`
	CWD       string `json:"cwd,omitempty"`
	State     string `json:"state"` // what the key shows
	Slot      int    `json:"slot"`  // -1: no key
	Pending   int    `json:"pending"`
	Question  bool   `json:"question,omitempty"`
	Ended     bool   `json:"ended,omitempty"`
	HerdrPane string `json:"herdr_pane,omitempty"`
	// HostSession is the desktop record id; HostConflict means two ids were seen.
	HostSession  string    `json:"host_session,omitempty"`
	HostConflict bool      `json:"host_conflict,omitempty"`
	LastActive   time.Time `json:"last_active"`
}

// status builds the document. Only the run loop calls it.
func (d *Daemon) status() StatusDoc {
	now := d.now()
	doc := StatusDoc{
		Version:       d.cfg.Version,
		StartedAt:     d.started,
		Selected:      d.table.Selected(),
		Held:          d.server.HeldCount(),
		EventsDropped: d.server.Dropped(),
		Ignored:       d.table.IgnoredCount(),
		Pad:           PadStatus{Enabled: !d.cfg.NoDevice, Connected: d.pad.connected.Load()},
		Herdr:         d.herdr,
		Sessions:      []SessionStatus{},
	}
	if d.pad.paused(now) {
		t := time.Unix(0, d.pad.pausedUntil.Load())
		doc.Pad.PaintPausedUntil = &t
	}
	for _, s := range d.table.All() {
		doc.Sessions = append(doc.Sessions, SessionStatus{
			Key: s.Key, Kind: s.Kind, Frontend: string(s.Frontend), Label: s.Label, CWD: s.CWD,
			State: d.table.Visible(s, now).String(), Slot: s.Slot, Pending: len(s.Pending),
			Question: s.Question, Ended: s.Ended, HerdrPane: s.HerdrPane, LastActive: s.LastActive,
			HostSession: s.HostSession, HostConflict: s.HostConflict,
		})
	}
	return doc
}

// statusAny answers GET /status for the hooks server. It asks the run loop,
// which owns the table. An error value gives a 503.
func (d *Daemon) statusAny() any {
	q := make(chan StatusDoc, 1)
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case d.statusQ <- q:
	case <-d.loopDone:
		return errors.New("daemon is stopping")
	case <-t.C:
		return errors.New("daemon is busy")
	}
	select {
	case doc := <-q:
		return doc
	case <-d.loopDone:
		return errors.New("daemon is stopping")
	case <-t.C:
		return errors.New("daemon is busy")
	}
}
