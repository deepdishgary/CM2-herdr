# CM2-Claude: Creator Micro 2 ↔ Claude Code Integration

**Date:** 2026-08-19
**Status:** Design validated with the user (brainstorming session 2026-08-18/19)
**Beads epic:** `ai-6ky`
**Updated:** 2026-10-23 — scope adds Herdr. See "Decisions 2026-10-23" below; it overrides older text where they conflict.

## Decisions 2026-10-23

Research digest with evidence: `docs/research/2026-10-23-herdr-and-desktop.md`.

1. **Scope adds every agent in Herdr**, of any kind (Claude, Codex, and others).
   Claude Code sessions outside Herdr (desktop app, plain terminal) stay in scope.
2. **Two session sources.**
   - Claude Code hooks cover Claude sessions in every frontend, Herdr included.
   - The Herdr socket API (`agent.list`, `events.subscribe`) covers all Herdr agents.
   - Join key: the Claude `session_id` equals Herdr's `agent_session.value`, so one session gets one key.
   - For Claude sessions, hook state wins. Herdr's screen-derived state is the only source for non-Claude agents.
3. **cmux is out.** It was unused after 2026-08-19.
4. **Focus.** Herdr: `herdr agent focus <pane>`, then raise Ghostty (proven live). Desktop: **raise the
   Claude app only** (`open -a Claude`). The `claude://resume?session=` deep link is banned from cm2d: on
   2026-10-23 at 16:10 a pad key press on a live session made the desktop app spawn a SECOND claude process
   on the same session id (a stale duplicate session record took the adopt path). The fork inherited the
   conversation and acted on its plan — merged a branch, committed and pushed — before going idle. Harmless
   this time (the commits were correct), but a deep link that can fork a running agent must not sit on a
   hardware key. Revisit only with a mechanism proven to focus-without-forking.
   **Update 2026-10-23 (ai-6ky.8):** `claude://code/continue?session=local_<id>` only navigates to an existing
   desktop record (live-tested; no import, adopt or unarchive). cm2d uses it only when the hooks reported the
   record id (`CLAUDE_CODE_HOST_SESSION_ID`, header `X-Cm2-Host-Session`), no second id was seen for the session,
   and the records folder has exactly one unarchived record with that id and no other unarchived record with the
   same `cliSessionId`. The scan fails closed: an unreadable, oversized or malformed record, or a symlink in the
   records folder, also raises the app only. Every other case raises the app. Spec: `docs/plans/2026-10-23-desktop-focus-and-herdr-seed.md`.
5. **Pad ownership.** ChatGPT's Input Monitoring permission is off (2026-10-23).
   The Input app must be closed while cm2d runs. Any other connected client corrupts RPC replies.
6. **Keys (chosen by the user).** All 12 key caps are clear. Layout, on an empty layer
   (Layer 1 macros stay untouched):

   ```
    (dial)  [ S1 ] [ S2 ]  (joy)
    [ S3 ] [ S4 ] [ S5 ] [ S6 ]
    [ S7 ] [ S8 ] [ S9 ] [S10 ]
      o    [  APPROVE   ] [DENY]
   ```

   - S1–S10: session keys. Color shows state. Press focuses that session and selects it.
   - Wide key: approve. Bottom-right key: deny. Both act only on the selected session.
   - Dial turn moves the selection. Dial press focuses the selected session.
   - Joystick: unassigned in v1. The small circle (`o`) has no binding slot in Input.
   - Keycodes: AG00–AG09 for S1–S10 (lighting follows the AG number, per okko/micro2-agent-keys).
     The exact key ids and LED thread ids come from the hardware walk in bead `ai-6ky.2`.
   - This replaces the original "six Agent Keys" in the Goal section.
7. **Approvals: Design A (decided by spike ai-6ky.9, `docs/research/2026-10-23-approvals-spike.md`).**
   An HTTP PermissionRequest hook POSTs to cm2d, which holds it (timeout 600, no `onFailure`). The normal
   dialog stays live meanwhile; the first answer wins; cm2d down or slow falls back to the normal dialog.
   Only Claude sessions get approvals. Herdr has no approve/deny API, so non-Claude Herdr agents get focus only.
   Design B (channel relay) is rejected for v1: launch flag + warning dialog every launch, no documented desktop
   support, dead relay not restarted.
8. **Needs-input signal.** The earlier PermissionRequest "noise" came from an `ask` rule on the exa fetch tool
   (removed 2026-10-18). Amber = a PermissionRequest from an attended session still pending after ~1 s, plus
   AskUserQuestion (a "question" state: APPROVE must not send a plain allow), plus Herdr `blocked` for non-Claude
   agents. Clear on PostToolUse, Stop, UserPromptSubmit, connection close, or timeout. `permission_prompt`
   Notification (~6 s late) is a backup cue only.
9. **Spike logger removed** from `~/.claude/settings.json` on 2026-10-23.
   The logged hook data stays local (mode 600, not published).
10. **Firmware** is 0.6.3. The Input app flashed it on 2026-10-23.
11. **Hardware verified (ai-6ky.2, `docs/research/2026-10-23-hardware-walk.md`).** Agent layer = Layer 2 (id 1):
    S1–S10 = AG00–AG09 in reading order, APPROVE = wide key (AG10 + AG11, both report and light), DENY = AG12.
    Dial: ENC_CW / ENC_CC per detent; **dial press toggles Layer 1 <-> Layer 2** (`KI_LS2` / `KI_LS1`), so the
    dial press does not focus (this replaces the earlier plan). Lights: brightness 0..1, effect 1 solid, 4 breathing
    (needs a speed `s`). Layout file: `config/layout.json`, applied with `cm2probe keymap apply`.

## Goal

Give the Work Louder Creator Micro 2 a Claude Code integration equivalent to its
official ChatGPT/Codex one:

- Six Agent Keys mirror the six most recent interactive Claude Code sessions
  with status LEDs (working / needs input / done / error).
- Pressing an Agent Key focuses that session's window.
- Action keys approve or deny a pending permission request.
- The encoder cycles the selected session.

Out of scope for v1: headless launchd jobs on the keys, canned prompts,
joystick actions, voice.

## Constraints

- Must port to an MDM-managed Mac. Shape: clone the repo,
  `make build` from source, grant Input Monitoring once, run manually or via
  launchd. No installer. No notarization.
- Frontends in use (2026-10-23): Claude Code desktop app, Herdr (inside
  Ghostty), plain Ghostty. cmux was dropped. No iTerm2, no tmux.
- The pad's normal macro layers must keep working untouched.

## Device facts (verified 2026-08-18)

- USB `0x303A:0x8298` ("Creator Micro 2", ESP32-S3). One composite
  IOHIDDevice: keyboard + consumer + mouse + vendor collection
  (usage page `0xFF00`, report ID `0x06`, 63-byte reports).
- Vendor channel protocol (reverse-engineered, two independent specs):
  byte0 = `0x06`, byte1 = channel (1 debug, 2 RPC), byte2 = length,
  bytes 3–63 = UTF-8 JSON chunk. Newline-terminated JSON-RPC-style messages,
  integer ids, no `jsonrpc` field.
- Key methods: `device.status`, `sys.version`, `fs.read`/`fs.write`
  (on-device `keymap.json`, applies live), `v.oai.thstatus` (per-key LED
  status), `v.oai.hid` (key events `AG00–AG05`, `ACT06–ACT12`, encoder),
  `v.oai.rad` (joystick). Firmware ≥ 0.6.0 required for `v.oai.*`.
- Agent Keys are opt-in keycodes (`KV_OAI_AG00`…) assigned in `keymap.json`.
  A key with one stops emitting keystrokes and emits vendor events instead.
- macOS gives no exclusive lock on this device. All hosts open it
  non-exclusively. Concurrent writers corrupt RPC (single receive buffer per
  channel) and fight over LEDs. Reads are broadcast and safe.
- hidapi's synchronous open fails on this device. Working paths:
  node-hid `HIDAsync` with `nonExclusive: true`, or direct `IOHIDDeviceOpen`
  with `kIOHIDOptionsTypeNone`.
- Opening the device requires the Input Monitoring TCC grant for the hosting
  binary.

## Architecture

One Go daemon, `cm2d`, three planes:

**Device plane.** Opens the vendor HID channel non-exclusively via IOKit
(cgo — not hidapi). Implements the JSON-RPC framing. All writes go through
one serialized queue. Reads `v.oai.hid` events; writes `v.oai.thstatus` LED
states. A setup subcommand writes `keymap.json` via `fs.write` to assign
Agent Key codes to a dedicated layer (with backup and restore).

**Session plane.** Claude Code hooks in `~/.claude/settings.json` POST
lifecycle events to `cm2d` on a localhost HTTP socket. Hooks are
frontend-agnostic: desktop app, cmux-spawned, and Ghostty sessions all
report identically. The daemon keeps a table of the six most recent
interactive sessions and paints LEDs from it.

**Control plane.** Key press → daemon action. Agent Key = focus that
session's window. Approve/deny keys = resolve a pending permission request
on the selected session. Encoder = cycle selection.

## Session tracking

- **Identity:** keyed on `session_id` from hook payloads; labeled by `cwd`
  basename.
- **Interactive discriminator (RESOLVED by spike ai-6ky.3, 2026-08-19):**
  headless `claude -p` DOES emit `UserPromptSubmit`, so that signal cannot
  discriminate. Use `CLAUDE_CODE_ENTRYPOINT` from the hook process
  environment instead: interactive = `cli` or `claude-desktop`; headless =
  `sdk-cli`. Headless jobs are `sdk-cli` and drop out automatically. Keep a
  config ignore-list (default `~/elara`) as belt and suspenders.
- **Label collisions:** cwd basename is not unique (three concurrent
  sessions shared one folder during the spike). Labels need a
  disambiguator (frontend + short session id, or session title when
  available).
- **Recency:** six most recently active sessions hold the keys. Assignments
  stay sticky while a session lives. Keys do not expire when idle (since
  2026-10-23). A session frees its key when it ends, when its Herdr agent
  is gone, or when a new session replaces it.

## LED semantics

| State | Color | Trigger |
|---|---|---|
| Working | Blue | Between `UserPromptSubmit` and `Stop` |
| Needs input | Amber (blink) | Unresolved `PermissionRequest`/`Notification` |
| Done, unread | Green | `Stop` fired, not focused since |
| Idle | Dim white | Focused/read, waiting |
| Error | Red | Abnormal end or reported error |

Colors match the Codex convention so the pad feels the same.

## Resilience rules

- One serialized device-write queue. No concurrent RPC.
- On unplug/replug: reopen, repaint full state.
- On daemon restart: rebuild the table from incoming hook events. No
  persisted state. LEDs are stale for seconds, not minutes.
- If the Work Louder Input app is running: back off all device writes and
  log a warning. Never fight another writer.

## Inbound control

**Focus (spike first).** Candidates, in preference order:
1. cmux's control surface (muxboard proves cmux state is externally
   observable; find the API).
2. Claude Code desktop app local session tooling.
3. Generic fallback: System Events window-title matching (works for Ghostty).

The daemon maps `session_id → frontend` at whatever fidelity the spike
finds. Worst case: raise the right app, best-effort window match.

**Approvals (spike, two designs).**
- *Design A — hook-hold:* a `PermissionRequest` HTTP hook holds the request
  while the key blinks amber. A button answers it. Timeout (default ~15 s)
  returns "no decision" so the normal terminal prompt appears.
- *Design B — channel relay:* a custom channel (`claude --channels`,
  verified working in CLI 2.1.227) relays permission prompts to the daemon —
  the official remote-approval path.

The spike tests both against desktop and cmux sessions. Ship the better one;
document the other.

**Safety rail:** approve/deny acts only on the explicitly selected session
(last Agent Key pressed), never on "whatever is amber." Deny is always safe.

## Portability (a managed Mac)

The repo carries everything:
- `make build` — Go + cgo/IOKit, no Homebrew deps beyond Go.
- `make sign` — stable self-signed cert so the Input Monitoring grant
  survives rebuilds (a common launchd pattern).
- `make install-hooks` — idempotently merges hook config into
  `~/.claude/settings.json`.
- launchd plist template + README runbook, including the one-time Input
  Monitoring grant and the Input-app coexistence rule.

Firmware flashes once at home and travels with the pad. The managed Mac never
needs the Input app.

## Phases

1. **Dashboard** — device layer + hooks + LED painter. Usable on its own.
2. **Focus** — spike, then implement.
3. **Approvals** — spike, then implement with safety rail.
4. **Polish + port** — encoder cycling, hardening, signing/launchd,
   work-Mac validation.

## Prototype assets

Node scripts implementing the full vendor RPC protocol (from the 2026-08-18
research session) are preserved at `~/Development/cm2-claude/prototype/`:
`device.js` (protocol client), `listener.js`, `requester.js`,
`hidlisten.c` (IOKit dual-client test). Use them to validate the device
before and during the Go port. They need `npm install` (node-hid) and the
Input Monitoring grant.

## Key references

- Protocol specs: `mpociot/codex-micro-stream-deck-emulator` (DEVELOPMENT.md),
  `buddyh/worklouder-macropad` (references/input-protocol.md)
- Claude Code prior art (this device): `honest-andy/cm2-agent-keys`
  (examples/claude-code/, agent keycodes, coexistence warnings)
- 20 agent keycodes exist (AG00–AG19): `okko/micro2-agent-keys`
- Firmware releases: `github.com/worklouder/cm-v2-fw-releases` (need ≥ 0.6.0;
  latest 0.6.2)
- Input app: `brew install --cask work-louder-input` (setup only, quit during
  daemon use)
- Replacement firmware (fallback only): `mrbillpapas/libremicro`
- cmux Stream Deck prior art: `mrshu/muxboard`

## Risks

| Risk | Mitigation |
|---|---|
| cgo IOKit open path has device quirks | Prototype scripts prove the protocol first; `honest-andy/cm2-agent-keys` documents the open flags that work |
| Hook payloads differ across frontends | Spike before building the session table |
| No usable focus API for cmux/desktop | Fallback: raise app + title match; dashboard still fully useful |
| MDM blocks Input Monitoring grant | Discover early: test the runbook on a managed Mac in phase 4; dashboard-only degrades gracefully to nothing (pad stays a normal macropad) |
| Input app overwrites agent keycodes | Keymap backup/restore in setup command; document "quit Input" rule |

## Spike findings: hooks and session identity (ai-6ky.3, 2026-08-19)

Captured with a temporary user-level logger hook (not published) across all four frontend types on the user's Mac
(claude 2.1.227/2.1.234).

### Frontend × identity table

| Frontend | `CLAUDE_CODE_ENTRYPOINT` | Process ancestry (hook → …) | Notes |
|---|---|---|---|
| Claude Code desktop app | `claude-desktop` | `claude > disclaimer > Claude > launchd` | Hot-reloads hook config mid-session (settings watcher) |
| cmux | `cli` | `claude > zsh > (zsh >) login > cmux > launchd` | Spawns plain interactive CLI per workspace |
| Ghostty terminal | `cli` | `claude > zsh > login > ghostty > launchd` | |
| Headless `claude -p` | `sdk-cli` | parent is the spawning process | Headless launchd jobs land here |

`cli` sessions are told apart (cmux vs Ghostty) by walking the ancestry from
the hook's ppid until a known frontend comm appears. Hook env also carries
`CLAUDE_PID` (the claude process), `CLAUDE_CODE_SESSION_ID`, and
`CLAUDE_PROJECT_DIR`.

### Event coverage (all frontends, verified)

- `SessionStart` fires with `source: startup | resume`.
- `UserPromptSubmit` carries `prompt` and `permission_mode` — and fires for
  headless `-p` too.
- `Stop` carries `permission_mode`. `SessionEnd` carries `reason`
  (`other`, `prompt_input_exit` observed).
- Payloads always include `session_id`, `cwd`, `transcript_path`.

### Design consequences

1. `SessionEnd` is not terminal: the desktop app ended and later resumed the
   same `session_id` (`source: resume`). The session table must treat
   resume as the same session and not free its key eagerly.
2. cmux resumed SIX sessions simultaneously on launch (`SessionStart
   source=resume` burst). Resume storms must not reshuffle or steal keys;
   recency ordering should key off user activity (`UserPromptSubmit`), not
   `SessionStart`.
3. The user's interactive sessions run `permission_mode=bypassPermissions`, so
   `PermissionRequest` will rarely fire in daily use. "Needs input" amber
   must primarily key off `Notification` (`idle_prompt`, questions), with
   `PermissionRequest` handled when it does occur (ai-6ky.9).
4. Prior art ON THIS MACHINE: `~/.claude/hooks/claude-island-state.py` +
   Claude Island.app already hook all events, track per-session tty/pid, and
   implement hook-held PermissionRequest approve/deny over a Unix socket
   (`/tmp/claude-island.sock`, 300 s wait, fails open when the app is not
   running). cm2d's hook receiver should copy this pattern; coexistence with
   Island's PermissionRequest hook is an open question for ai-6ky.9.

### Remaining for this spike

- Confirm a real headless scheduled run logs as `sdk-cli` (next triage fire).
- Remove the logger entries from `~/.claude/settings.json` (backup:
  `~/.claude/settings.json.bak-cm2spike`) when done.

## Spike findings: focus mechanisms (ai-6ky.7, 2026-08-19)

All three frontends have working programmatic focus. Verified live on this
Mac except where noted.

| Frontend | Mechanism | Session → target mapping | Status |
|---|---|---|---|
| cmux | AppleScript (cmux.sdef): `activate` + `focus (first terminal whose id is "<uuid>")` | `CMUX_SURFACE_ID` (and `CMUX_WORKSPACE_ID`) in the claude process env — capture in the SessionStart hook | PROVEN live (cmux raised + workspace switched) |
| Claude desktop | `open "claude://resume?session=<uuid>&cwd=<path>"` — the URL the CLI's `/desktop` command builds | session_id from hooks; cwd from hook payload | PROVEN live (app raised, session focused). **SUPERSEDED 2026-10-23: banned, it can fork a live session. See decision 4.** |
| Ghostty ≥ 1.3.0 | Native AppleScript (Ghostty.sdef, same model cmux derives from): `focus <terminal>` | Match `working directory` of terminals to session cwd; official docs list "jumping to terminals by working directory" as supported. No surface-id env var found yet — live mapping test in ai-6ky.8 | Mechanism confirmed (docs + local sdef); live test pending |

Demo commands:

```bash
# cmux
osascript -e 'tell application "cmux"
  activate
  focus (first terminal whose id is "17A7FD55-C912-4219-90E4-A09774D4972F")
end tell'

# Claude desktop (SUPERSEDED, banned: see decision 4. cm2d uses claude://code/continue instead.)
open "claude://resume?session=<id>&cwd=/Users/me/project"

# Ghostty (same dictionary shape as cmux)
osascript -e 'tell application "Ghostty"
  activate
  focus (first terminal whose working directory is "/path/to/project")
end tell'
```

Notes and caveats:

1. `focus` alone changes in-app state but may not raise the app over others;
   always pair with `activate`. LaunchServices `open` auto-activates, which
   sidesteps the desktop app's known open-url foregrounding bug (issue
   #65610).
2. (Superseded by decision 4: cm2d never opens this link.) `claude://resume?session=...` is undocumented internal plumbing for the
   CLI's `/desktop` command (min desktop 1.1.2396) — treat as fragile;
   corroborated by two independent sources and verified here. Desktop
   session records: `~/Library/Application Support/Claude/local-agent-mode-sessions/`.
3. `claude-cli://open` (the CLI's own scheme) only LAUNCHES new sessions
   (q/cwd/repo params) — useless for focus, possibly useful later for a
   "new session" key.
4. cm2d needs the macOS Automation TCC grant (one prompt per target app:
   cmux, Ghostty). No Accessibility grant needed — both terminals have real
   scripting dictionaries; System Events UI scripting is dead weight.
5. cmux's Unix-socket CLI is richer (`select-workspace`, `focus-pane`,
   `send`, `read-screen`, `events --reconnect`, `notify`) but gated:
   default `automation.socketControlMode: cmuxOnly` rejects external
   processes. Unlock by setting `automation.socketControlMode: "automation"`
   in `~/.config/cmux/cmux.json` + full cmux relaunch (muxboard-documented).
   Optional for v1; AppleScript suffices for focus.
6. Bonus for later phases: both cmux and Ghostty support `input text`
   (paste into a specific terminal) and cmux `send`/`send-key` — a
   canned-prompt key needs no new protocol. cmux `events` could even
   substitute for some hook plumbing on cmux sessions.
7. Prior art: mrshu/muxboard focuses via `cmux open-notification --id <uuid>`
   (requires an existing notification); gonzaloserrano/streamdeck-cmux
   selects workspaces by index. Our per-session mapping via CMUX_SURFACE_ID
   is more direct than both.
