package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cm2-claude/internal/herdr"
)

// forgetAfter drops a slotless session with a quiet hooks-only history.
const forgetAfter = 24 * time.Hour

const (
	// stuckAfter: a Claude session that shows Working or NeedsInput but sent
	// no event for this long is dead (kill -9, closed terminal). It becomes
	// Idle, so its key can be reused.
	stuckAfter = 6 * time.Hour
	// herdrDownGrace: after Herdr has been unreachable this long, its
	// non-Claude agents stop showing Working or NeedsInput.
	herdrDownGrace = 30 * time.Second
	// maxIgnored bounds the set of ignored session ids. The oldest is
	// forgotten first.
	maxIgnored = 4096
	// endedMemory: how long a forgotten ended session id stays barred from
	// Herdr seeding. It is longer than any lag in what Herdr reports.
	endedMemory = time.Hour
)

// entry is a Session plus the bookkeeping the table needs.
type entry struct {
	s           *Session
	seq         int          // creation order
	entrypoint  string       // last claude-desktop or cli seen
	activated   bool         // had activity and wants a slot
	herdrStatus herdr.Status // last status seen in a snapshot; "" if none
	seeded      bool         // made from a Herdr snapshot; no hook event seen yet
}

// Table is the set of sessions and the 10 slots. It does no I/O and is not
// safe for concurrent use. Sessions returned by its methods are live
// pointers: callers must treat them as read-only.
type Table struct {
	cfg       Config
	byKey     map[string]*entry
	slots     []*Session
	selected  int
	ignored   map[string]bool // headless or ignored Claude session ids
	ignOrder  []string        // ignored ids, oldest first
	herdrDown time.Time       // when Herdr became unreachable; zero if it is reachable
	seq       int
	clock     time.Time            // latest time seen, for events with a zero At
	endedIDs  map[string]time.Time // ids forgotten after SessionEnd, with when; Herdr must not seed them again
}

// New returns an empty table. Zero Config values take the defaults.
func New(cfg Config) *Table {
	if cfg.Slots <= 0 {
		cfg.Slots = 10
	}
	if cfg.EndedGrace <= 0 {
		cfg.EndedGrace = 10 * time.Minute
	}
	if cfg.NeedsInputDelay <= 0 {
		cfg.NeedsInputDelay = time.Second
	}
	home, _ := os.UserHomeDir()
	if cfg.IgnoreCWDPrefixes == nil {
		if home != "" {
			cfg.IgnoreCWDPrefixes = []string{filepath.Join(home, "elara")}
		}
	} else {
		ps := make([]string, 0, len(cfg.IgnoreCWDPrefixes))
		for _, p := range cfg.IgnoreCWDPrefixes {
			if home != "" && strings.HasPrefix(p, "~/") {
				p = filepath.Join(home, p[2:])
			}
			ps = append(ps, p)
		}
		cfg.IgnoreCWDPrefixes = ps
	}
	return &Table{
		cfg:      cfg,
		byKey:    map[string]*entry{},
		slots:    make([]*Session, cfg.Slots),
		selected: -1,
		ignored:  map[string]bool{},
		endedIDs: map[string]time.Time{},
	}
}

func claudeKey(id string) string { return "claude:" + id }

// stamp returns t if set, else the latest time seen, and records it.
func (t *Table) stamp(at time.Time) time.Time {
	if at.IsZero() {
		return t.clock
	}
	if at.After(t.clock) {
		t.clock = at
	}
	return at
}

// cwdIgnored reports whether cwd is at or under an ignored prefix.
func (t *Table) cwdIgnored(cwd string) bool {
	if cwd == "" {
		return false
	}
	for _, p := range t.cfg.IgnoreCWDPrefixes {
		p = strings.TrimRight(p, "/")
		if p == "" {
			continue
		}
		if cwd == p || strings.HasPrefix(cwd, p+"/") {
			return true
		}
	}
	return false
}

// ApplyHook applies one Claude hook event.
func (t *Table) ApplyHook(ev HookEvent) {
	if ev.SessionID == "" {
		return
	}
	at := t.stamp(ev.At)
	defer t.finish()
	if t.ignored[ev.SessionID] {
		return
	}
	if ev.Attended == "0" || t.cwdIgnored(ev.CWD) {
		t.ignore(ev.SessionID)
		t.remove(claudeKey(ev.SessionID))
		return
	}
	e := t.byKey[claudeKey(ev.SessionID)]
	if e == nil {
		if e = t.admit(ev, at); e == nil {
			return
		}
	}
	t.apply(e, ev, at)
}

// ignore adds a session id to the ignored set, within maxIgnored.
func (t *Table) ignore(id string) {
	if t.ignored[id] {
		return
	}
	t.ignored[id] = true
	t.ignOrder = append(t.ignOrder, id)
	if len(t.ignOrder) > maxIgnored {
		old := t.ignOrder[0]
		t.ignOrder = t.ignOrder[1:]
		delete(t.ignored, old)
	}
}

// admit creates the session for an unknown id, or returns nil to drop the event.
func (t *Table) admit(ev HookEvent, at time.Time) *entry {
	switch ev.Name {
	case "SessionStart":
		if ev.Attended != "1" {
			return nil
		}
	case "UserPromptSubmit", "PermissionRequest":
		// Never registered (cm2d restarted): best effort on the entrypoint.
		if ev.Attended != "1" && ev.Entrypoint != "claude-desktop" && ev.Entrypoint != "cli" {
			return nil
		}
	default:
		return nil
	}
	t.seq++
	s := &Session{
		Key:        claudeKey(ev.SessionID),
		SessionID:  ev.SessionID,
		Kind:       "claude",
		Frontend:   FrontendUnknown,
		State:      Idle,
		LastActive: at,
		Slot:       -1,
	}
	e := &entry{s: s, seq: t.seq}
	t.byKey[s.Key] = e
	delete(t.endedIDs, ev.SessionID) // a hook event revives an id
	return e
}

func (t *Table) apply(e *entry, ev HookEvent, at time.Time) {
	s := e.s
	e.seeded = false // hooks own the session from now on
	if ev.Entrypoint != "" {
		e.entrypoint = ev.Entrypoint
	}
	if ev.CWD != "" {
		s.CWD = ev.CWD
	}
	if ev.HerdrPane != "" {
		s.HerdrPane = ev.HerdrPane
	}
	if ev.HostSession != "" {
		switch {
		case s.HostSession == "":
			s.HostSession = ev.HostSession
		case s.HostSession != ev.HostSession:
			s.HostConflict = true
		}
	}
	setFrontend(e)
	s.LastActive = at

	revive := ev.Name == "UserPromptSubmit" || ev.Name == "PermissionRequest" ||
		(ev.Name == "SessionStart" && ev.Source == "resume")
	if s.Ended {
		if !revive {
			return
		}
		s.Ended, s.EndedAt = false, time.Time{}
	}

	switch ev.Name {
	case "UserPromptSubmit":
		s.State = Working
		s.Pending, s.Question = nil, false
		t.activate(e, true)
	case "PermissionRequest":
		s.State = Working
		if ev.IsQuestion {
			s.Question = true
		} else if !hasPending(s, ev.RequestID) {
			s.Pending = append(s.Pending, Pending{RequestID: ev.RequestID, Tool: ev.ToolName, Since: at})
		}
		t.activate(e, true)
	case "PermissionReleased":
		for i, p := range s.Pending {
			if p.RequestID == ev.RequestID {
				s.Pending = append(s.Pending[:i:i], s.Pending[i+1:]...)
				break
			}
		}
		if len(s.Pending) == 0 {
			s.Pending = nil
		}
	case "PostToolUse":
		s.Pending, s.Question = nil, false
		s.State = Working
	case "Notification":
		if ev.NotificationType == "permission_prompt" && len(s.Pending) == 0 && !s.Question {
			s.State = NeedsInput
			t.activate(e, true)
		}
	case "Stop":
		s.Pending, s.Question = nil, false
		s.State = Done
	case "StopFailure":
		s.Pending, s.Question = nil, false
		s.State = Error
	case "SessionEnd":
		s.Pending, s.Question = nil, false
		s.Ended, s.EndedAt = true, at
	}
}

func hasPending(s *Session, id string) bool {
	if id == "" {
		return false
	}
	for _, p := range s.Pending {
		if p.RequestID == id {
			return true
		}
	}
	return false
}

// setFrontend derives the frontend of a session from its pane and entrypoint.
func setFrontend(e *entry) {
	s := e.s
	switch {
	case s.HerdrPane != "":
		s.Frontend = FrontendHerdr
	case e.entrypoint == "claude-desktop":
		s.Frontend = FrontendDesktop
	case e.entrypoint == "cli":
		s.Frontend = FrontendTerminal
	default:
		s.Frontend = FrontendUnknown
	}
}

// Focused marks a session as seen: Done becomes Idle.
func (t *Table) Focused(key string, now time.Time) {
	now = t.stamp(now)
	defer t.finish()
	e := t.byKey[key]
	if e == nil {
		return
	}
	if e.s.State == Done {
		e.s.State = Idle
	}
	e.s.LastActive = now
}

// Tick expires ended sessions, ends stuck states, and drops old sessions that
// have no key and no pane. An idle session keeps its key until it is replaced.
func (t *Table) Tick(now time.Time) {
	now = t.stamp(now)
	defer t.finish()
	t.pruneEnded(now)
	for _, e := range t.sorted() {
		s := e.s
		if s.Ended {
			if now.Sub(s.EndedAt) >= t.cfg.EndedGrace {
				t.remove(s.Key)
				if s.SessionID != "" {
					t.endedIDs[s.SessionID] = now
				}
			}
			continue
		}
		busy := s.State == Working || s.State == NeedsInput || s.Question || len(s.Pending) > 0
		if busy && s.SessionID != "" && now.Sub(s.LastActive) >= stuckAfter {
			// No event for hours: the session died without a SessionEnd.
			s.State, s.Pending, s.Question = Idle, nil, false
			busy = false
		}
		if busy && s.SessionID == "" && s.HerdrPane != "" && t.herdrGone(now) {
			// Herdr is gone, so nothing tells whether this agent still works.
			// Forget its status, so the next snapshot sets it again.
			s.State = Idle
			e.herdrStatus = ""
			busy = false
		}
		if busy {
			continue
		}
		if s.Slot < 0 && s.HerdrPane == "" && now.Sub(s.LastActive) >= forgetAfter {
			t.remove(s.Key)
		}
	}
}

// pruneEnded forgets ended ids after endedMemory, and keeps the set within maxIgnored.
func (t *Table) pruneEnded(now time.Time) {
	for id, at := range t.endedIDs {
		if now.Sub(at) >= endedMemory {
			delete(t.endedIDs, id)
		}
	}
	for len(t.endedIDs) > maxIgnored {
		var oldID string
		var oldAt time.Time
		for id, at := range t.endedIDs {
			if oldID == "" || at.Before(oldAt) {
				oldID, oldAt = id, at
			}
		}
		delete(t.endedIDs, oldID)
	}
}

// herdrGone tells whether Herdr has been unreachable for herdrDownGrace.
func (t *Table) herdrGone(now time.Time) bool {
	return !t.herdrDown.IsZero() && now.Sub(t.herdrDown) >= herdrDownGrace
}

// Visible returns the state to paint for s at now.
func (t *Table) Visible(s *Session, now time.Time) State {
	if s.Ended && (s.State == Working || s.State == NeedsInput) {
		return Idle
	}
	if s.Question {
		return NeedsInput
	}
	for _, p := range s.Pending {
		if now.Sub(p.Since) >= t.cfg.NeedsInputDelay {
			return NeedsInput
		}
	}
	return s.State
}

// Slots returns one entry per slot; nil marks an empty slot.
func (t *Table) Slots() []*Session {
	return append([]*Session(nil), t.slots...)
}

// Get returns the session with the key, or nil.
func (t *Table) Get(key string) *Session {
	if e := t.byKey[key]; e != nil {
		return e.s
	}
	return nil
}

// All returns every session: slotted ones by slot, then the rest by arrival.
func (t *Table) All() []*Session {
	var out []*Session
	for _, s := range t.slots {
		if s != nil {
			out = append(out, s)
		}
	}
	for _, e := range t.sorted() {
		if e.s.Slot < 0 {
			out = append(out, e.s)
		}
	}
	return out
}

// sorted returns the entries in creation order.
func (t *Table) sorted() []*entry {
	es := make([]*entry, 0, len(t.byKey))
	for _, e := range t.byKey {
		es = append(es, e)
	}
	sort.Slice(es, func(i, j int) bool { return es[i].seq < es[j].seq })
	return es
}

// Selected returns the selected slot, or -1.
func (t *Table) Selected() int { return t.selected }

// Select selects a slot. An empty or invalid slot is ignored.
func (t *Table) Select(slot int) {
	if slot >= 0 && slot < len(t.slots) && t.slots[slot] != nil {
		t.selected = slot
	}
}

// MoveSelection moves to the next (delta > 0) or previous (delta < 0)
// occupied slot, wrapping. With no selection it selects the first one.
func (t *Table) MoveSelection(delta int) {
	var occ []int
	for i, s := range t.slots {
		if s != nil {
			occ = append(occ, i)
		}
	}
	if len(occ) == 0 {
		return
	}
	pos := -1
	for i, slot := range occ {
		if slot == t.selected {
			pos = i
		}
	}
	if pos < 0 {
		t.selected = occ[0]
		return
	}
	n := len(occ)
	pos = ((pos+delta)%n + n) % n
	t.selected = occ[pos]
}

// IgnoredCount is the number of session ids ignored as headless or by cwd.
func (t *Table) IgnoredCount() int { return len(t.ignored) }
