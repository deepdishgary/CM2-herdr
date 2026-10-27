package session

import (
	"time"

	"cm2-claude/internal/herdr"
)

// ApplyHerdr merges a Herdr snapshot into the table. An unavailable snapshot
// changes no session; it only starts the clock that Tick uses to stop
// trusting the last status of non-Claude agents.
func (t *Table) ApplyHerdr(snap herdr.Snapshot, now time.Time) {
	now = t.stamp(now)
	defer t.finish()
	if !snap.Available {
		if t.herdrDown.IsZero() {
			t.herdrDown = now
		}
		return
	}
	t.herdrDown = time.Time{}
	panes := make(map[string]herdr.Agent, len(snap.Agents))
	claudeIDs := map[string]bool{} // session ids of the Claude agents
	for _, a := range snap.Agents {
		panes[a.PaneID] = a
		if sid := a.ClaudeSessionID(); sid != "" {
			claudeIDs[sid] = true
		}
	}

	// Agents that are gone. A Claude session is tied to its session id, not to
	// its pane: a moved pane keeps the key (joinClaude below binds the new pane).
	for _, e := range t.sorted() {
		s := e.s
		if s.HerdrPane == "" {
			continue
		}
		a, ok := panes[s.HerdrPane]
		if s.SessionID == "" {
			if !ok || a.Kind == "claude" {
				t.remove(s.Key)
			}
			continue
		}
		if claudeIDs[s.SessionID] {
			continue
		}
		if ok && a.Kind == "claude" && a.ClaudeSessionID() == "" {
			continue // the pane is there, but Herdr does not know the id yet
		}
		if e.seeded {
			// No hook ever spoke for it, so nothing else would end it.
			t.remove(s.Key)
			continue
		}
		// It stays in the table and comes back on activity.
		s.HerdrPane = ""
		e.herdrStatus = ""
		setFrontend(e)
		t.freeSlot(e)
	}

	for _, a := range snap.Agents {
		if a.Kind == "claude" {
			if sid := a.ClaudeSessionID(); sid != "" {
				t.joinClaude(a, sid, now)
			}
			continue
		}
		t.upsertAgent(a, now)
	}
}

// joinClaude ties a Herdr pane to a hook session. Hooks stay the state
// source; Herdr only adds the pane, a slot cue, and a blocked cross-check.
// A Claude agent that the table does not know is admitted: Herdr panes are
// interactive, so this seeds the table after a cm2d restart.
func (t *Table) joinClaude(a herdr.Agent, sid string, now time.Time) {
	if t.ignored[sid] || t.cwdIgnored(a.CWD) {
		return
	}
	e := t.byKey[claudeKey(sid)]
	if e == nil {
		// An id that ended and was forgotten is not seeded again.
		if at, ok := t.endedIDs[sid]; ok && now.Sub(at) < endedMemory {
			return
		}
		t.seedClaude(a, sid, now)
		return
	}
	s := e.s
	s.HerdrPane = a.PaneID
	if s.CWD == "" {
		s.CWD = a.CWD
	}
	setFrontend(e)

	prev := e.herdrStatus
	e.herdrStatus = a.Status
	if prev == a.Status || s.Ended {
		return
	}
	noAsk := len(s.Pending) == 0 && !s.Question
	switch {
	case a.Status == herdr.StatusBlocked && noAsk && (s.State == Working || s.State == Idle):
		s.State = NeedsInput
		s.LastActive = now
	case prev == herdr.StatusBlocked && s.State == NeedsInput && noAsk:
		s.State = Working
		s.LastActive = now
	}
	if a.Status == herdr.StatusWorking || a.Status == herdr.StatusBlocked {
		t.activate(e, true)
	}
}

// seedClaude adds a Claude session that only Herdr knows. Its state comes from
// Herdr until hook events take over. It takes the lowest free slot if there is
// one; only a working or blocked agent may evict, as for any activity.
func (t *Table) seedClaude(a herdr.Agent, sid string, now time.Time) {
	t.seq++
	e := &entry{seq: t.seq, herdrStatus: a.Status, seeded: true, s: &Session{
		Key: claudeKey(sid), SessionID: sid, HerdrPane: a.PaneID, Kind: "claude",
		Frontend: FrontendHerdr, CWD: a.CWD, State: stateOf(a.Status),
		LastActive: now, Slot: -1,
	}}
	t.byKey[e.s.Key] = e
	t.assignSlot(e, false) // the lowest free slot; never evicts
	if a.Status == herdr.StatusWorking || a.Status == herdr.StatusBlocked {
		t.activate(e, true)
	}
}

// upsertAgent adds or updates a non-Claude Herdr agent.
func (t *Table) upsertAgent(a herdr.Agent, now time.Time) {
	key := "herdr:" + a.PaneID
	if t.cwdIgnored(a.CWD) {
		t.remove(key)
		return
	}
	e := t.byKey[key]
	if e == nil {
		t.seq++
		e = &entry{seq: t.seq, s: &Session{
			Key: key, HerdrPane: a.PaneID, Frontend: FrontendHerdr,
			LastActive: now, Slot: -1,
		}}
		t.byKey[key] = e
		t.assignSlot(e, false) // the lowest free slot; never evicts
	}
	s := e.s
	s.Kind = a.Kind
	if a.CWD != "" {
		s.CWD = a.CWD
	}
	if e.herdrStatus == a.Status {
		return
	}
	e.herdrStatus = a.Status
	s.State = stateOf(a.Status)
	s.LastActive = now
	if a.Status == herdr.StatusWorking || a.Status == herdr.StatusBlocked {
		t.activate(e, true)
	}
}

func stateOf(st herdr.Status) State {
	switch st {
	case herdr.StatusWorking:
		return Working
	case herdr.StatusBlocked:
		return NeedsInput
	case herdr.StatusDone:
		return Done
	}
	return Idle
}
