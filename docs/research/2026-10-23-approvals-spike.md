# Spike: approvals — hook-hold vs channel relay (bead ai-6ky.9, 2026-10-23)

**Decision: ship Design A (HTTP PermissionRequest hook that holds the request). Do not build Design B for v1.**

Method: one log analysis (a local hook log plus session transcripts), one docs review, live experiments for each
design, and a critic who cross-checked the reports and ran three follow-up experiments. All live tests used throwaway
Haiku sessions in a scratch folder with `--setting-sources project,local` (the user's own settings, hooks and Remote
Control not loaded). CLI under test: 2.1.309 (`~/.local/bin/claude`). The desktop app bundles 2.1.308.
The harness and result files are not published.

Tiers: **C** observed (most runs are n=1), **S** strongly supported, **T** tentative.

## 1. Design A — HTTP PermissionRequest hook, held by cm2d

| Run | What happened | Tier |
|---|---|---|
| A1 hold 3 s, allow | The normal dialog appears ~0.2 s after the POST and stays visible during the hold. Allow runs the command ~0.25 s after the reply. UI: "Allowed by PermissionRequest hook". | C |
| A2 hold 2 s, deny + message | Command blocked. The model sees the message as a tool error and does not retry. | C |
| A3 hook timeout 3 s | Claude Code closes the HTTP connection at the timeout. The dialog stays and a terminal answer works. No error text. | C |
| A4 empty 200 / `{}` | Normal dialog at once. | C |
| A5 cm2d down (refused) | Normal dialog at once, no error text. | C |
| A6 terminal answers first | The terminal answer wins; a late hook answer is ignored. Terminal **No** drops the HTTP connection (TUI only). Terminal **Yes** does not; the TUI shows "running PermissionRequest hook" until cm2d replies, but Stop and the next turn are not blocked. | C |
| A7 two sessions held | Answers route by connection and `session_id`. Each dialog is independent. | C |
| A8 AskUserQuestion | Fires PermissionRequest. A plain allow does NOT skip the question. Allow + `updatedInput` (original questions + `answers: {question text: option label}`) answers it. | C |
| c2 desktop-style stdio protocol (`--permission-prompt-tool stdio`) | Host prompt and hook POST arrive together; first answer wins; a hook answer cancels the host prompt (`control_cancel_request`). A host answer does NOT drop the hook connection, and a host deny leaves only PreToolUse then Stop. | C (protocol level, not the desktop UI) |
| c1 second silent PermissionRequest hook | An http allow still won with a second command hook that gave no decision (like the dead Claude Island hook). | C (one combination) |

Fails open in every tested failure (down, timeout, empty, `{}`). `onFailure: block` was not tested; per the docs it would
deny every request when cm2d is down, so cm2d must not use it.

## 2. Design B — development channel permission relay

- Works in default mode: request reaches the server within 50 ms of the dialog; verdict to command ~30–270 ms;
  terminal dialog stays live; first answer wins. **C**
- Costs: `--dangerously-load-development-channels server:<name>` on every launch, a full-screen warning dialog every
  launch, and no documented desktop-app support. **C** / docs
- A killed channel server is not restarted (30 s idle + one prompt); the relay silently stops. **S**
- AskUserQuestion did not reach the channel (one default-mode trial). **T**
- Bypass mode: no relay for Bash/Write (no dialog opens). Untested for ask-rule tools and AskUserQuestion, where
  dialogs do open in bypass. **C** for what was tested.

## 3. Why PermissionRequest looked noisy (log analysis + critic run c1)

- The 282 `mcp__exa__web_fetch_exa` requests came from an **`ask` rule** for that tool in `~/.claude/settings.json`,
  removed on 2026-10-18 17:58:13. An ask rule prompts (and fires the hook) even in bypassPermissions mode, and beats an
  allow rule on the same tool. After the change: zero exa requests; 16 PermissionRequests in 5 days in total. **S**
  (reproduced with a local MCP tool in c1).
- Subagents only carried the load (281 of 282 came from subagents because subagents made the fetches). A subagent's
  ask-rule prompt shows as a normal dialog on the main screen ("Tool use from the general-purpose agent"). **C**
- Things that still prompt in bypass mode (docs + tests): ask rules, AskUserQuestion, MCP tools with
  `_meta["anthropic/requiresUserInteraction"]`, critical-path rm. A relative `rm -rf` did not prompt. The cause of 35
  bypass-mode Bash requests in the log is still unknown. **T**
- `Notification permission_prompt` arrives ~6 s after the dialog (92 of 95 AskUserQuestion cases; 6.02 s in A3). Too
  late to be the main amber trigger. **C**
- 46 of 54 default-mode requests came from headless `sdk-cli` runs with `CLAUDE_CODE_SESSION_ATTENDED=0`. **C**

## 4. What cm2d will do (input to ai-6ky.10 and ai-6ky.6)

Hook config (installed by `make install-hooks`; the user's settings are not changed by this spike):

```json
{ "hooks": { "PermissionRequest": [ { "hooks": [
  { "type": "http", "url": "http://127.0.0.1:<port>/hooks/permission-request", "timeout": 600 } ] } ] } }
```

- No `onFailure` (default `continue`).
- Replies: allow = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`;
  deny = same with `"behavior":"deny","message":"..."`; pass through = HTTP 200 with an empty body.
- A terminal or desktop answer gives no reliable HTTP signal. cm2d also needs short fire-and-forget http hooks on
  PostToolUse, Stop, UserPromptSubmit and Notification. On PostToolUse or Stop for a session with a held request, cm2d
  replies an empty 200 at once (ends the "running PermissionRequest hook" spinner) and clears amber.
- Amber ("needs input"): a PermissionRequest from an attended session that is still pending after ~1 s. Clears on
  PostToolUse, Stop, UserPromptSubmit, connection close, or timeout. `permission_prompt` Notification is a backup cue only.
- AskUserQuestion: a "question" state. APPROVE must not send a plain allow. v1: reply empty 200 at once (question shows
  on screen), keep amber until the session moves on; DENY may send deny ("declined to answer"); the session key focuses
  the session so the user answers there. v2 idea: pick an option with the dial (allow + `updatedInput.answers`).
- Headless filter (T, untested): add `"headers": {"X-Cm2-Attended": "$CLAUDE_CODE_SESSION_ATTENDED"}` and
  `"allowedEnvVars": ["CLAUDE_CODE_SESSION_ATTENDED"]`; verify the value arrives before relying on it.

## 5. Not covered

- No test used the real desktop app UI or a Herdr pane (rules forbade it). Run one smoke test in each before relying on it.
- Not tested: non-2xx replies, a server reset mid-hold, `onFailure: block`, many held connections at once, what clears
  queued identical ask-rule dialogs, and channel relay in bypass mode for ask-rule tools.
- Test sessions launched with `--setting-sources project,local` started in auto mode on this Mac (cause unknown);
  tests passed `--permission-mode default`.
- Side effect: Claude Code added project entries for the scratch test folders to `~/.claude.json` (harmless).
