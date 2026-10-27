# Spec: keys stay until replaced (bead ai-6ky.11)

**Date:** 2026-10-23. **Branch:** `feature/sticky-keys`. Approved by the user.

## Why

`session.Table.Tick` frees the key of any session that is idle and quiet for `IdleExpiry` (30 min). In real use all
7 seeded Herdr sessions lost their keys after an hour idle, and desktop chats do the same. The user wants sessions to
keep their keys while they exist.

## Rules (all in `internal/session`)

1. **No time-based freeing of idle keys.** Remove the `IdleExpiry` branch in `Tick`. Keep `Config.IdleExpiry` in
   `types.go` only as a documented no-op field (comment: "unused since 2026-10-23: keys stay until replaced") so
   callers still compile; remove its default. Keep `forgetAfter` (slotless, paneless sessions dropped after 24 h),
   the 6 h `stuckAfter` cap, and the Herdr-down handling unchanged.
2. **A key frees only when:**
   a. the session ended and `EndedGrace` (10 min) passed (unchanged);
   b. its Herdr agent is gone: for a Claude session, no agent in an available snapshot carries its session id
      (a pane that MOVED keeps the same session id under a new pane id: re-bind the pane and keep the key);
      for a non-Claude agent, its pane is gone (unchanged). Seeded sessions (no hook events yet) are removed as
      today; hook-known Claude sessions lose HerdrPane and their key but stay in the table (they come back on
      activity). An unavailable snapshot changes nothing (unchanged);
   c. eviction: a session that needs a key (activation with eviction allowed, as today) takes the key of the least
      recently active evictable session when all keys are full (unchanged rule: evictable = ended, or Idle with no
      Pending and no Question; Done-unseen, Working, NeedsInput and Error are never evicted).
3. **Herdr sessions reclaim free keys.** A non-ended session with a current Herdr pane (Claude or non-Claude) and no
   key takes the lowest free key whenever one exists (in `finish`), most recently active first, without evicting.
   Sessions without a Herdr pane (desktop, terminal) come back only on activity, as today.

## Tests (test-first; go test -race)

- Idle Herdr and idle desktop sessions keep their keys after Tick at +1 h, +24 h (no freeing).
- Eviction when all 10 keys are full: a new Working session takes the LRU idle key; Done-unseen and NeedsInput are
  never evicted; with no evictable key the new session waits keyless.
- An evicted Herdr session regains a free key as soon as one frees, without new activity; an evicted desktop session
  does not.
- Claude session in Herdr: snapshot without its session id frees its key (hook-known: stays in table keyless;
  seeded: removed); snapshot with the same session id under a new pane id keeps the key and updates HerdrPane.
- Unavailable snapshot changes nothing.
- EndedGrace still frees; stuck cap unchanged.
- Daemon integration: seeded Herdr sessions still hold their keys after many 1 s ticks spanning > 30 min of fake time
  (use the injectable clock).
- Update existing tests that asserted the old IdleExpiry behavior; say which in the report.
