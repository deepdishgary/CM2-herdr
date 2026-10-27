# Research: Herdr support next to Claude desktop sessions (2026-10-23)

**Status:** research only. No design decision yet. Beads epic `ai-6ky`.
**Method:** five read-only research agents plus a critic, then spot checks in the main session.
Nothing on the Mac changed. No Herdr focus, send-keys, prompt, or update command ran.
One agent ran `herdr agent focus --help`; in 0.9.3 that prints only the top-level help, and the focused pane stayed the same.
Tiers: **C** confirmed, **S** strongly supported, **T** tentative.

## 1. What changed since the August design

| Frontend | Oct 2026 hook events | Oct sessions | Last seen |
|---|---|---|---|
| Claude desktop app | 1064 | 43 | today |
| Herdr (inside Ghostty) | 706 | 16 | 2026-10-22 |
| Plain Ghostty | 28 | 7 | 2026-10-11 |
| cmux | 0 | 0 | 2026-08-19 |

Source: a local hook log (not published). **C**

- cmux dropped out. Herdr replaced it. The desktop app is still the main frontend.
- Claude Code is 2.1.304 (design: 2.1.227). Desktop app is 3.6556.2.
- Herdr 0.9.3 is installed. 0.9.5 is out with the same socket protocol (22).
- The pad runs firmware **0.6.3** since 2026-10-23 10:44. The Input app flashed it (log: `~/Library/Logs/input/main.log`). It was on 0.6.2 before that. **C**

## 2. Herdr facts that matter to cm2d

**Socket API.** `~/.config/herdr/herdr.sock`, newline JSON `{id, method, params}`. One request per connection, except `events.subscribe` and `events.wait`. A process outside Herdr (no `HERDR_ENV`) can use it with no flags. `herdr api schema --json` prints the full schema (102 methods). **C**

**Agent list.** `agent.list` returns per agent: `pane_id`, `workspace_id`, `tab_id`, `agent` (kind), `agent_status` (`idle|working|blocked|done|unknown`), `focused`, `completion_seq`, `state_change_seq`, `cwd`, and `agent_session {source, agent, kind, value}`. **C**

**Session mapping.** For Claude, `agent_session.value` equals the Claude Code `session_id` that our hooks receive. Herdr's own SessionStart hook (`~/.claude/hooks/herdr-agent-state.sh`) reports it. Verified for the 6 live Herdr sessions. **C**
- Claude processes in Herdr panes carry `HERDR_ENV=1`, `HERDR_PANE_ID`, `HERDR_SOCKET_PATH`. Desktop sessions do not. So our hooks can tell Herdr sessions apart directly. **C** (process env); hook inheritance **S**.
- `HERDR_PANE_ID` is fixed at process start. A cross-workspace `pane move` assigns a new pane id. Treat the env value as a hint and confirm through `agent.list`. **S**

**Events.** `events.subscribe` streams events on one long-lived connection. `pane.agent_status_changed` needs a `pane_id`, so there is no global status stream. Global events exist for `pane.created/closed/exited/agent_detected/focused`. No replay. A slow reader gets `events_lost` and is disconnected. A missing pane id rejects the whole subscribe request. Recovery: resubscribe, then `session.snapshot`. **C**
- Event delivery itself was not observed (all agents idle during the test). **T**
- Open bug #5194: about 12x idle server CPU while status subscriptions are open.

**Focus.** `herdr agent focus <target>` (`agent.focus`). Since 0.9.1 it moves attached TUI clients to that pane (changelog only, **not tested on 0.9.3**). It marks the target seen (`done` becomes `idle`). It does not raise the terminal window. Every Herdr hardware project raises Ghostty separately. **S**

**State source.** Herdr gets Claude state only from screen scraping (a TOML manifest it updates from herdr.dev). Open bugs show wrong Claude states: stuck `working` (#4819), false `blocked` (#4064), dialogs shown as `idle` (#2868, #5221). An external tool cannot push state into a Claude pane (#5007). **C**
- Herdr has no `error` state.
- `blocked` covers permission prompts, question forms, MCP elicitation, and workflow prompts.

**Approvals.** No approve or deny method exists. The only route is keystroke injection (`agent.send_keys`, `pane.send_keys`), with no request identity or acknowledgment. **C**

**Server lifecycle.** The server is not a launchd job. The first TUI client spawns it, and it is reparented to PID 1 when that client exits. After a reboot, no server exists until Herdr is opened. cm2d must handle a missing socket and reconnect. **C**

**Plugins.** Manifest-declared commands (startup, actions, event hooks, panes). Not supervised daemons. A poor home for a long-running HID daemon. A plugin could still add keybound helpers. **S**

**Other agents.** Herdr knows 24 agent kinds. Integrations installed here: claude (v10), codex (v8). Codex or other agents in Herdr are visible to cm2d only through the Herdr API, not through Claude hooks. **C**

## 3. Claude Code changes that matter (2.1.227 to 2.1.304)

- HTTP hooks (`type: "http"`) POST the event JSON. A 2xx JSON body is parsed like command output. **S**
- **Default hook timeout is 600 s.** The design assumed about 15 s, so cm2d must set `timeout` explicitly. **S**
- `onFailure` (2.1.295): `continue` (default) or `block`. Keep `continue`. With `block`, a daemon outage would deny every PermissionRequest. **S**
- `StopFailure` fires **instead of** `Stop` after an API error. Without it the Working LED could stick. Use it for the Error LED. **S**
- `PermissionDenied` fires only for auto-mode denials. Not a waiting signal. **S**
- Notification `permission_prompt` fires about 6 s after a prompt waits. `idle_prompt` fires about 60 s after a finished turn. **S**
- `CLAUDE_CODE_MESSAGING_SOCKET` (inbox socket) is documented. Docs say a message can never answer a permission prompt. **S**
- Channels permission relay ("Design B") works but is still a research preview. It needs a per-session launch flag, and desktop support is undocumented. **S**/**T**
- `claude agents --json` is the documented way to read session state from outside. Not run (it may start the background daemon). **S**
- `claude --resume <id> --desktop` (2.1.285) opens a session in the desktop app. Whether it focuses an open session is undocumented. **S**
- Managed-Mac risk: managed settings can restrict HTTP hook URLs (`allowedHttpHookUrls`). **T**

## 4. Hook-log findings that change the design

- **PermissionRequest is noisy.** 412 of 467 PermissionRequests fired in `bypassPermissions` mode. The largest group is `mcp__exa__web_fetch_exa` (282), which is in the allow list, in bursts about 1 s apart, mostly with no `permission_prompt` Notification after them. The August design assumed PermissionRequest would be rare. It cannot drive amber or a held decision on its own. **C** (counts); cause **T**.
- **AskUserQuestion fires PermissionRequest** (94 in bypass mode). It is a real wait (lines up with the 6 s `permission_prompt` delay), but it needs an answer choice, not allow/deny. **C**
- **Headless discriminator.** All headless launchd job records are `CLAUDE_CODE_ENTRYPOINT=sdk-cli` (this closes the open item of `ai-6ky.3`). But `sdk-cli` also appears for child `claude` runs inside interactive sessions, and 103 desktop child runs inherit `claude-desktop`. `CLAUDE_CODE_SESSION_ATTENDED` separates them cleanly (1 = interactive) but is undocumented. **C** (data); choice open.
- Desktop sessions emit only `permission_prompt` Notifications, no `idle_prompt`. Herdr sessions emit both. **C**
- Frontend ancestry: Herdr sessions trace `claude > -zsh > herdr > (PID 1)`. The ancestry walk needs a herdr case, or use `HERDR_ENV`. **C**

## 5. Desktop focus status

Desktop 3.6556.2 still handles `claude://resume?session=<uuid>`. It now reads only `session` (plain UUID), ignores `cwd`, and calls `importCliSession` / `adoptCliSession` before it navigates. A separate `claude://code/continue?session=local_<id>` route needs the desktop's own `local_` id. The CLI 2.1.304 no longer contains the resume URL. Whether resume now focuses an open desktop session or imports a duplicate is **unknown; needs a live test**. **C** (static read); behavior **T**.

## 6. Pad ownership (blocks every architecture)

- **Input app** (`it.focusense.input-app` 1.0.2) is running. It sends `host.focused_app` to the pad on every app switch. It starts after each reboot (probably a login item; not confirmed). **C**
- **ChatGPT.app** is the Codex desktop app (`com.openai.codex` 26.1016.62342). Its Codex Micro integration is set up (onboarding complete, pad detected). It has no off switch and drives the pad whenever the app runs. It claims layer 1 and listens on AG00 to AG05 (from public analyses of an older build). **C** (setup); key range **S**.
- On 2026-10-23 10:38 to 10:41, Input and ChatGPT were both connected. Each received the other's RPC replies ("No resolver found for id"), and calls timed out. Input's first firmware flash attempt timed out in that window. cm2d would collide the same way. **C** (log); cm2d collision **S**.
- Options: quit both apps (or revoke their Input Monitoring) while cm2d runs, and/or put our agent keys on AG06+ so ChatGPT ignores them.

## 7. Prior art

| Project | Lang / license | Covers | Notes |
|---|---|---|---|
| schacon/micro-manager | Swift, **no license** | Herdr only | Menu-bar app. agent.list + per-pane subscriptions + 2.5 s poll; key press = `agent.focus` + raise Ghostty. No approvals. Last commit 2026-08-05. Reference only. |
| thefotes/Dispatch | Swift, MIT | Herdr (+ SSH remotes) | Clean-room successor; backs up keymap before writing. |
| alasano/house-of-herdr codex-micro | TS, Herdr plugin | Herdr | Spawns a detached Node daemon from a plugin. |
| jkirkpatrick24/herdr-micro | TS, MIT | Herdr | Measured API notes. |
| Endless-Winning-AI/lets-build-clauder | **Go, MIT, purego IOKit (no cgo)** | Claude desktop + Claude Code hooks | No Herdr. Possible code reuse for our HID layer and desktop half. |
| n4ru/cm2-claude | Node (Windows) | Claude Code | Holds PermissionRequest open for pad approve/deny. |
| okko/micro2-agent-keys | — | VS Code | 20 slots (AG00 to AG19); lighting follows AG number, not position. |

No project covers both Herdr and the Claude desktop app. Projects disagree on hardware facts (top-row order, joystick numbering, brightness scale 0..1 vs 0..255, thread 12, ambient zone, `v.oai.rad` vs `kb.radial`). Check them on this pad.

Work Louder has no official Claude support (feedback post in Backlog, 68 votes). Staff call the `v.oai.*` RPC private to OpenAI and subject to change. Newer firmware: 0.6.3 stable, 0.7.0-rc.11 prerelease; neither mentions the agent API.

## 8. Design changes this implies

1. Add Herdr as a frontend; drop cmux.
2. Session table keyed on Claude `session_id`. A Herdr pane with a Claude agent joins on `agent_session.value`, so one session gets one key.
3. For Claude sessions, hooks stay the state source (Herdr's screen scraping has known errors). Herdr status is a cross-check, and the only source for non-Claude Herdr agents (if in scope).
4. Focus: Herdr = `agent.focus` + raise Ghostty. Desktop = retest resume vs `claude --resume <id> --desktop`.
5. Needs-input: drive amber from `permission_prompt` Notifications, Herdr `blocked`, and AskUserQuestion, not from every PermissionRequest.
6. Error LED from `StopFailure`.
7. Explicit short hook timeouts; `onFailure: continue`.
8. Pad ownership rule (quit Input/ChatGPT or use AG06+), decided before any device code.
9. Herdr reconnect policy (server may be absent).

## 9. Open questions that need a live test

- Does `agent.focus` from outside switch the Ghostty TUI to the pane on 0.9.3?
- Does `claude://resume?session=` focus an open desktop session or import a duplicate?
- Do `v.oai.thstatus`, `v.oai.rgbcfg`, `v.oai.hid` still work on firmware 0.6.3 from our own code?
- Does holding a PermissionRequest HTTP hook keep the terminal dialog visible?
- Why does PermissionRequest fire for an allowed tool in bypass mode?
- Is `done` emitted as its own `pane.agent_status_changed` transition?
