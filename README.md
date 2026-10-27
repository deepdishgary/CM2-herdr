# cm2-claude

`cm2d` turns a Work Louder Creator Micro 2 into a dashboard for coding agents. Ten keys show the most recent
Claude Code sessions (desktop app, terminal, Herdr) and every agent in Herdr. Pressing a key jumps to that session.
The wide key approves a pending Claude Code permission request on the selected session; the bottom-right key denies it.

Design: `docs/design.md`. Research and spikes: `docs/research/`. Build specs: `docs/plans/`.

## The pad layout (Layer 2, the agent layer)

```
 (dial)   [ S1 ] [ S2 ]   (joy)
 [ S3 ] [ S4 ] [ S5 ] [ S6 ]
 [ S7 ] [ S8 ] [ S9 ] [S10 ]
   logo  [   APPROVE   ] [DENY]
```

- **Dial press** switches between Layer 1 (your normal keys) and Layer 2 (agents).
- **Dial turn** moves the selection between sessions.
- **Key colors:** blue working, amber breathing needs you (a permission prompt or a question), green finished and
  not yet seen, dim white idle, red error. The selected key is brighter.
- **APPROVE** breathes green only when the selected session has a permission request the pad can answer.
  Questions (AskUserQuestion) and plans (ExitPlanMode) are never answered from the pad: press the session key and
  answer on screen.
- Pressing APPROVE with nothing to approve flashes it red. APPROVE and DENY never act on an unselected session.

## Run it (this Mac)

```bash
make build                 # bin/cm2d and bin/cm2probe
bin/cm2d setup --dry-run   # show the Layer 2 changes (config/layout.json)
bin/cm2d setup             # back up the pad's keymap, write the layout, verify
bin/cm2d install-hooks     # add the cm2d hooks to ~/.claude/settings.json (backup first)
bin/cm2d run --verbose     # the daemon; logs to stderr
bin/cm2d status            # what the daemon sees
```

- Quit the Work Louder **Input** app first (it starts at login). Two programs on the pad garble each other.
- `bin/cm2d run` needs Input Monitoring for the app that starts it (the Claude desktop app already has it).
- After you update cm2d, run `bin/cm2d install-hooks` again. It updates the existing cm2d entries in place (the
  desktop focus needs the `X-Cm2-Host-Session` header). A chat that reports no id just raises the app.
- A desktop key opens exactly that chat (`claude://code/continue?session=local_<id>`) only when the chat has one live,
  unarchived record. Otherwise it raises the Claude app. cm2d never uses the `resume` link.
- When cm2d is not running, the hooks fail at once and Claude Code behaves as if they were not there.
- Undo: `bin/cm2d uninstall-hooks`; `bin/cm2d restore backups/<keymap backup>.json` for the pad.
- If you save a layout in the Input app, it may overwrite Layer 2: run `bin/cm2d setup` again.

Debug: `bin/cm2d herdr list|watch` (what Herdr reports), `bin/cm2probe listen 30` (raw pad events),
`bin/cm2probe call device.status`.

## Still to do

- Run under launchd with a stable signing identity so the Input Monitoring grant survives rebuilds (bead ai-6ky.12).
- Port to a managed Mac (bead ai-6ky.13).
- Authenticate the PermissionRequest hook (Unix socket or shared secret) so no other program can answer it.

`prototype/` holds the original Node.js protocol scripts (need `npm install`).

## Security note

The PermissionRequest hook posts to `http://127.0.0.1:47731` with no shared secret. When cm2d is not running, another
local program that binds that port can answer permission requests. Only run cm2d on a Mac you trust.

## License

MIT. See `LICENSE`.
