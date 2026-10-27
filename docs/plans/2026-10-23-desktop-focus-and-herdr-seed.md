# Spec: exact desktop chat focus + Herdr sessions at startup (beads ai-6ky.8, ai-6ky.11)

**Date:** 2026-10-23. **Branch:** `feature/desktop-focus`.
Background: `docs/design.md` decision 4; the 16:10 fork incident (bead ai-6ky.8 notes).

## Facts (verified 2026-10-23)

- `claude://resume?session=<cliSessionId>` imports/adopts/unarchives a desktop record and can start a second claude
  process on a running session (it forked this session at 16:10). **Never use it.**
- `claude://code/continue?session=local_<id>` only navigates the main window to that exact desktop record
  (`/epitaxy/local_<id>`). It never imports, adopts or unarchives. An archived or unknown id falls back to the Code
  home page. It starts a process only through normal warm-on-show, and only when that record has no live process
  (same as a sidebar click). Live test 17:37: target `local_1d9854de…` (one record, live process) came forward;
  process list identical, no new or changed records except the target's mtime, no import/unarchive/warm log lines.
- Each desktop-spawned claude process has `CLAUDE_CODE_HOST_SESSION_ID=local_<id>` (its desktop record) and
  `CLAUDE_CODE_ENTRYPOINT=claude-desktop` in its environment. Nested headless runs inherit it but are attended=0
  and already ignored.
- Desktop records: `~/Library/Application Support/Claude/claude-code-sessions/<org>/<account>/local_*.json` with
  fields `sessionId`, `cliSessionId`, `isArchived`, `cwd`, `title` (private: never log titles).
  Several records may share one `cliSessionId` (this session has two: an August import and the original).

## A. Plumb the host id

1. `internal/session/types.go`: add `HostSession string` to `HookEvent` (the desktop record id) and to `Session`;
   add `HostConflict bool` to `Session` (two different host ids seen for one Claude session: an existing fork).
2. `internal/hookcfg`: every http hook (event + permission) gets header
   `"X-Cm2-Host-Session": "$CLAUDE_CODE_HOST_SESSION_ID"` and `allowedEnvVars` gains
   `"CLAUDE_CODE_HOST_SESSION_ID"`. Re-running install updates existing cm2d entries in place (idempotent; backup;
   other hooks untouched). Uninstall still removes exactly what install adds.
3. `cmd/cm2d/hookcmd.go` (SessionStart forwarder): send `host_session_id` from the env.
4. `internal/hooks`: read header `X-Cm2-Host-Session` on `/event` and `/permission`, and body field
   `host_session_id` on `/session`. Keep the value only if it matches `^local_[0-9A-Za-z-]{1,64}$`; otherwise "".
5. `internal/session`: on any hook event with a non-empty HostSession for a Claude session: if the session has none,
   store it; if it differs from the stored one, set `HostConflict = true` (sticky for the session's life) and keep
   the first id. Status (`/cm2d/v1/status` and `cm2d status`) shows `host_session` and `host_conflict`.

## B. Exact desktop focus

1. `internal/focus`: new `DesktopChats` interface `Check(hostID string) (ok bool, reason string)` and a production
   implementation reading the records folder (root configurable for tests; default the path above):
   - reject unless hostID matches the regex above (no path built from an unchecked id);
   - find exactly one file `<root>/*/*/<hostID>.json` (none or more than one: reject);
   - reject if unreadable, not JSON, `isArchived` true, or `cliSessionId` empty;
   - reject if any OTHER record under `<root>/*/*/` has the same `cliSessionId` and `isArchived` false;
   - fail closed (review fix): reject if any OTHER `local_*.json` is unreadable, too large, not JSON, or has a wrong
     field type; reject if the scan meets a symlinked org or account folder, a folder it cannot read, or a
     `local_*.json` that is not a regular file. Open records without blocking and without following symlinks;
   - never write, never log titles. Read fresh on every call (about 20 small files).
2. `Focuser` gets fields `Desktop DesktopChats` and `Log *slog.Logger`. Desktop branch:
   - if `s.HostSession != ""` and `!s.HostConflict` and `!s.Ended` and `Desktop != nil` and `Check` is ok:
     run `open claude://code/continue?session=<hostID>` (exec, no shell, exactly this route);
   - otherwise run `open -a Claude` and log at Info why (no host id / conflict / ended / the Check reason).
3. A test must fail if any focus path can ever produce a URL containing `resume`, `continue?session=last`,
   `needs-input`, `epitaxy`, or any scheme other than the exact continue route with a validated id.
4. `cmd/cm2d/run.go` wires the production DesktopChats and the logger.

## C. Herdr sessions get keys when they appear

1. `session.ApplyHerdr` / `joinClaude`: a Claude agent with a session id that the table does not know is ADMITTED
   (Herdr panes are interactive), unless the id is in the ignored set or its cwd is ignored. New session: key
   `claude:<sid>`, Kind claude, Frontend herdr, HerdrPane, CWD from Herdr, State from Herdr status (`stateOf`),
   LastActive now. Later hook events take over as the state source, exactly as today.
2. Any Herdr agent seen for the first time (newly admitted Claude agent, or a new non-Claude agent) takes the lowest
   FREE slot if one exists. It never evicts anyone for this. (Existing rule unchanged: working/blocked activation
   may evict as before.)
3. Review fixes. A seeded entry (no hook event seen yet) is removed when its Herdr pane goes away or holds another
   session, because no hook would ever end it. Once a hook event arrives, the entry is a hook session. An id
   forgotten after SessionEnd is not seeded again for an hour, unless a hook event revives it.
4. Result: after a cm2d restart, or when Herdr starts, all Herdr sessions show on the pad at once (dim when idle),
   up to the free keys.

## Tests (go test -race, all packages)

Unit tests for each item above, including: header and body parsing with valid, missing, malformed and literal
`$CLAUDE_CODE_HOST_SESSION_ID` values; HostConflict set on a second different id and not on the same id; Check with
temp record trees (ok; archived; missing; duplicate active record on the same cliSessionId; duplicate but archived
is fine; two files with the same name in different account folders; traversal attempts like `local_../x`;
unreadable, oversized, wrong-typed or symlinked records fail closed); Focus routing table for every desktop case; the banned-route test; install idempotence when
upgrading an old install (old entries without the new header get updated in place, not duplicated); Herdr seeding
(restart with 7 idle Herdr Claude agents fills 7 free slots; ignored id not admitted; ignored cwd not admitted;
no eviction when keys are full; hooks for a seeded session join the same entry). Daemon integration: a seeded Herdr
session and a desktop session with a host id; pressing the desktop key runs the continue URL through a fake Runner
with a fake DesktopChats; with Check failing it runs `open -a Claude`.

## Rules for builders

Never run `open` with any URL or app, never touch the real records folder (tests use temp dirs), never edit
~/.claude/settings.json, never open the pad, never send anything to the live Herdr except read-only calls.
