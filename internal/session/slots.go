package session

import (
	"path/filepath"
	"sort"
)

// activate marks a session as wanting a slot and tries to give it one.
func (t *Table) activate(e *entry, allowEvict bool) {
	if e.s.Ended {
		return
	}
	e.activated = true
	t.assignSlot(e, allowEvict)
}

// assignSlot gives e the lowest free slot. If none is free and allowEvict is
// set, it evicts the least recently active evictable session.
func (t *Table) assignSlot(e *entry, allowEvict bool) {
	s := e.s
	if s.Slot >= 0 {
		return
	}
	for i, occ := range t.slots {
		if occ == nil {
			t.place(e, i)
			return
		}
	}
	if !allowEvict {
		return
	}
	var victim *entry
	for _, occ := range t.slots {
		v := t.byKey[occ.Key]
		if !evictable(v.s) {
			continue
		}
		if victim == nil || v.s.LastActive.Before(victim.s.LastActive) {
			victim = v
		}
	}
	if victim == nil {
		return
	}
	slot := victim.s.Slot
	t.freeSlot(victim)
	t.place(e, slot)
}

func (t *Table) place(e *entry, slot int) {
	t.slots[slot] = e.s
	e.s.Slot = slot
}

// evictable: an ended session, or one that is idle and already seen.
func evictable(s *Session) bool {
	if s.Ended {
		return true
	}
	return s.State == Idle && !s.Question && len(s.Pending) == 0
}

// freeSlot empties the slot of e. The session stays in the table, and it must
// have new activity to get a slot again.
func (t *Table) freeSlot(e *entry) {
	s := e.s
	e.activated = false
	if s.Slot < 0 {
		return
	}
	t.slots[s.Slot] = nil
	if t.selected == s.Slot {
		t.selected = -1
	}
	s.Slot = -1
}

// remove deletes a session and frees its slot.
func (t *Table) remove(key string) {
	e := t.byKey[key]
	if e == nil {
		return
	}
	t.freeSlot(e)
	delete(t.byKey, key)
}

// finish runs after every change: slotless sessions that want a slot get one
// when it is free, then labels are rebuilt. A session wants a slot when it
// had activity, or when Herdr lists its agent (it reclaims a free key). A
// pane that only a hook event named does not count: herdrStatus is set by a
// snapshot and cleared when the agent is gone.
func (t *Table) finish() {
	var waiting []*entry
	for _, e := range t.byKey {
		if e.s.Slot < 0 && !e.s.Ended && (e.activated || (e.s.HerdrPane != "" && e.herdrStatus != "")) {
			waiting = append(waiting, e)
		}
	}
	sort.Slice(waiting, func(i, j int) bool {
		a, b := waiting[i], waiting[j]
		if !a.s.LastActive.Equal(b.s.LastActive) {
			return a.s.LastActive.After(b.s.LastActive)
		}
		return a.seq < b.seq
	})
	for _, e := range waiting {
		// An idle session only takes a free slot; so does a session that
		// has a pane but no activity. Others with activity may evict.
		t.assignSlot(e, e.activated && !evictable(e.s))
	}
	t.relabel()
}

// relabel sets each Label to the cwd basename, plus " ·" and 4 characters
// of the id when two sessions share a basename.
func (t *Table) relabel() {
	count := map[string]int{}
	base := map[string]string{}
	for k, e := range t.byKey {
		b := labelBase(e.s)
		base[k] = b
		count[b]++
	}
	for k, e := range t.byKey {
		l := base[k]
		if count[l] > 1 {
			l += " ·" + idSuffix(e.s)
		}
		e.s.Label = l
	}
}

func labelBase(s *Session) string {
	if s.CWD != "" {
		return filepath.Base(s.CWD)
	}
	if s.Kind != "" {
		return s.Kind
	}
	return "agent"
}

// idSuffix is 4 characters of the session id, or of the pane id.
func idSuffix(s *Session) string {
	if s.SessionID != "" {
		return truncate(s.SessionID, 4)
	}
	p := s.HerdrPane
	if len(p) > 4 {
		p = p[len(p)-4:]
	}
	return p
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}
