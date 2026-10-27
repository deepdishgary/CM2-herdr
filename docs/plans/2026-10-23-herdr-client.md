# Spec: Herdr client (bead ai-6ky.14)

**Date:** 2026-10-23
**Branch:** `feature/herdr-client`
**Goal:** a Go package that gives cm2d a live, correct list of every agent in Herdr
(any kind), with status, and a way to focus one. No device code. No Claude hook code.

Background and evidence: `docs/research/2026-10-23-herdr-and-desktop.md`.
Herdr reference copies (read-only) in the session scratchpad:
`herdr-schema-0.9.3.json` (`herdr api schema --json`), `herdr-docs-095/socket-api.mdx`,
`herdr-src-095/` (Herdr source), `mm/` (schacon/micro-manager, no license: read for
ideas only, copy no code), `hoh-src/` (house-of-herdr codex-micro).

## Constraints

- Go standard library only. No third-party modules. `go.mod` module is `cm2-claude`.
- Package path: `internal/herdr`. One small CLI hook in `cmd/cm2d`.
- macOS first. Must also compile on Linux (no cgo in this package).
- Herdr has no API stability promise. Decode leniently: ignore unknown fields,
  tolerate missing optional fields, never fail a whole list on one odd record.
- Never send a mutating request to the live Herdr server from tests or tools,
  except `agent.focus` from the explicit CLI command a human runs.

## Wire protocol (Herdr 0.9.3, protocol 22; same on 0.9.5)

- Unix socket. Newline-delimited JSON. Request: `{"id":"<string>","method":"<m>","params":{...}}`.
- Success: `{"id":"<same>","result":{"type":"<t>", ...}}`.
- Error: `{"id":"<same or empty>","error":{"code":"<c>","message":"<m>"}}`.
- **One request per connection**, then the server closes it. Exception: `events.subscribe`
  keeps the connection open.
- Methods used:
  - `ping` (params `{}`) -> `{"type":"pong","version":"0.9.3","protocol":22,"capabilities":{...}}`
  - `session.snapshot` (params `{}`) -> `{"type":"session_snapshot","snapshot":{"version","protocol","workspaces","tabs","panes","layouts","agents":[AgentInfo...],"focused_pane_id","focused_tab_id","focused_workspace_id"}}`
  - `agent.list` (params `{}`) -> `{"type":"agent_list","agents":[AgentInfo...]}`
  - `agent.focus` (params `{"target":"<pane_id or agent name>"}`) -> some ok result. It marks the agent seen and, since 0.9.1, moves attached TUI clients. It does NOT raise the terminal window.
  - `events.subscribe` (params `{"subscriptions":[{"type":"pane.created"}, ..., {"type":"pane.agent_status_changed","pane_id":"w1:p2"}]}`;
    the global types we use are listed under "Watcher algorithm")
    -> first line `{"id":..,"result":{"type":"subscription_started"}}`, then pushed lines
    `{"event":"<kind>","data":{...}}`. Possible later line: an error with the request id and
    `code:"events_lost"`, after which the server closes the connection.
- AgentInfo fields (required: `terminal_id, agent_status, workspace_id, tab_id, pane_id, focused, revision`;
  optional: `agent` (kind, e.g. "claude"), `name`, `display_agent`, `title`, `cwd`, `foreground_cwd`,
  `completion_seq` (uint64 or null), `state_change_seq` (uint64, default 0), `terminal_title`,
  `terminal_title_stripped`, `agent_session` {`source`,`agent`,`kind` ("id"|"path"),`value`} or null,
  `state_labels`, `tokens`, `interactive_ready`, `launch_pending`, `screen_detection_skipped`).
- `agent_status` is one of `idle, working, blocked, done, unknown`. Map anything else to `unknown`.
- Subscription rules:
  - `pane.agent_status_changed` REQUIRES a `pane_id`. There is no global status stream.
  - If any referenced pane does not exist, Herdr rejects the whole request with an error and
    closes the connection.
  - No replay of past events. Ordering holds only within one subscription entry.
  - Unknown whether a second `events.subscribe` can be sent on an open connection: do not rely
    on it. To change the pane set, open a new subscription connection, then close the old one.
- Socket path resolution, in order (`<base>` is `$XDG_CONFIG_HOME` if it is set, else `~/.config`):
  1. an explicit path (`--socket`, or the argument to `NewClient`);
  2. an explicit session name (`--session NAME`, or the argument to `NewClientSession`):
     `<base>/herdr/sessions/<name>/herdr.sock`;
  3. `$HERDR_SOCKET_PATH`;
  4. `$HERDR_SESSION`, if it is not empty: `<base>/herdr/sessions/<name>/herdr.sock`;
  5. `<base>/herdr/herdr.sock`.
- Socket path length: Herdr's path must fit in `sun_path`. The client checks it before the dial
  and gives a clear error when the path is too long: 104 bytes on macOS and the BSDs (freebsd,
  openbsd, netbsd, dragonfly), 108 on Linux, no check on other systems. The limit counts the
  closing NUL, so the path must be shorter than the limit.
- The server may be absent (it starts only when the user opens Herdr). Treat
  `ENOENT`/`ECONNREFUSED` as "Herdr not running", not as a fatal error.

## Package API (shape; names may adjust if the code is clearer)

```go
package herdr

type Status string // "idle","working","blocked","done","unknown"

type AgentSession struct { Source, Agent, Kind, Value string }

type Agent struct {
    PaneID, WorkspaceID, TabID, TerminalID string
    Kind          string  // AgentInfo.agent, e.g. "claude", "codex"; "" if absent
    Name          string
    DisplayName   string  // display_agent, else title, else Kind
    Status        Status
    Focused       bool
    CompletionSeq *uint64
    StateChangeSeq uint64
    Revision      uint64
    CWD           string  // foreground_cwd if set, else cwd
    Session       *AgentSession
}

// ClaudeSessionID returns the Claude Code session id when Kind=="claude"
// and Session.Kind=="id"; otherwise "".
func (a Agent) ClaudeSessionID() string

type Snapshot struct {
    Available     bool     // false when the socket is missing or refuses
    Version       string
    Protocol      int
    Agents        []Agent  // server order (workspace, tab, pane); do not re-sort
    FocusedPaneID string
}

type APIError struct { Code, Message string } // implements error

type Client struct { /* SocketPath, timeouts */ }
func NewClient(socketPath string) *Client        // "" = resolve per rules above
func NewClientSession(socketPath, session string) *Client
func ResolveSocketPath(explicit string) string
func ResolveSocketPathSession(explicit, session string) string
func (c *Client) Ping(ctx) (version string, protocol int, err error)
func (c *Client) Snapshot(ctx) (Snapshot, error) // via session.snapshot
func (c *Client) Focus(ctx, paneID string) error // agent.focus

// Watcher keeps a live Snapshot and calls onChange when the meaningful
// state changes (and once at start, including Available=false).
type Watcher struct {
    Client *Client
    PollInterval, Debounce, BackoffMin, BackoffMax time.Duration // 3s, 100ms, 500ms, 10s
    OnError          func(error) // optional; see "Watcher error semantics"
    UnavailableAfter int         // default 3; see "Watcher error semantics"
}
func (w *Watcher) Run(ctx context.Context, onChange func(Snapshot)) error // returns ctx.Err() on cancel
```

## Watcher algorithm (follows Herdr's documented recovery pattern)

1. Read a snapshot. If Herdr is not running, publish `Available=false` once, then retry with
   capped exponential backoff (start 500 ms, cap 10 s, reset after a successful subscribe).
   Other read failures follow "Watcher error semantics" below.
2. Open a subscription connection with the global events `pane.created`, `pane.closed`,
   `pane.exited`, `pane.moved`, `pane.agent_detected`, `pane.focused`, `tab.focused`,
   `workspace.focused` and `pane.updated` (none of them takes a `pane_id`; all are valid in the
   0.9.3 schema), plus one `pane.agent_status_changed` per agent pane in the snapshot. The last
   four global events are additions to the first version of this spec. They let the watcher see
   focus moves and pane updates at once. `pane.updated` is chatty (titles change often), but
   the debounce and the "meaningful change" rule below absorb that noise. Wait for
   `subscription_started`. If the server rejects it (a pane vanished), go back to step 1 (no
   tight loop: small backoff).
3. Read a fresh snapshot (authoritative) and publish if changed.
4. Read events. Every event marks state dirty. After `Debounce`, re-read the snapshot and
   publish if changed. Serialize refreshes; if events arrive during a read, refresh again.
5. If the set of agent panes changed, open a new subscription connection for the new set
   (step 2), then close the old one.
6. On `events_lost`, EOF, or any read error: close, back off briefly, go to step 1.
7. Independently, re-read the snapshot every `PollInterval` as a safety net (events may be
   missed; detection can change without events).
8. On ctx cancel: close every connection and return promptly (no goroutine leaks). After a
   snapshot read returns with ctx done, `Run` returns `ctx.Err()` and publishes nothing.

### Watcher error semantics

- **Not running.** `Available=false` is published at once, and only, when Herdr is not running
  (the socket is missing or refuses: the snapshot read returns no error and `Available=false`).
- **Any other failure of a snapshot read** (an `APIError` such as `server_unavailable` or
  `internal_error`, an I/O timeout, a malformed or over-long reply, a socket path that is too
  long) keeps the last published snapshot and is reported to `Watcher.OnError`. After
  `Watcher.UnavailableAfter` such failures in a row (default 3) `Run` publishes
  `Available=false`. A success, or "not running", resets the count. If nothing has been
  published yet, the first `onChange` call waits for the third failure or for a success.
- **Errors from the watch loop** (a rejected subscribe, `events_lost`, a stream that ends, a
  failed refresh) are reported to `OnError`, then `Run` starts over at step 1. A refresh that
  finds Herdr not running is not reported; the next read publishes `Available=false`.
- `OnError` is optional. It never gets a context cancellation, and nothing is published after
  the context is cancelled, even when a read finishes at that moment. `Run` calls it from its own
  goroutine, like `onChange`, so it must return quickly.
- **Retry delay.** While Herdr is not reachable, the delay doubles from `BackoffMin` to
  `BackoffMax`. While Herdr is reachable (the last snapshot read succeeded) the delay never
  exceeds `PollInterval`, so a subscription that keeps failing still leaves state refreshed at
  the poll rate. A successful subscribe resets the delay to `BackoffMin`.
- **First callback delay.** The first `onChange` call comes after the first snapshot read ends.
  If Herdr is hung, that read can take up to `IOTimeout` (default 5 s). `Run` publishes no
  placeholder in the meantime.

"Meaningful change" compares, per agent in order: PaneID, Kind, Status, Session.Value,
CompletionSeq, Focused, DisplayName, CWD; plus Available, FocusedPaneID. Ignore Revision,
StateChangeSeq and terminal titles (spinner noise).

## Resource rules

- Every non-subscription call: dial with timeout (default 2 s), set a read/write deadline
  (default 5 s), close the connection on every path. No leaked file descriptors
  (micro-manager's leak cost ~90 MB/day).
- Limit line length when reading (default 8 MB, `Client.MaxLine`); a snapshot can be large. A line
  longer than the 64 KB read buffer must still decode in full, and a line over the limit is an error.
- Ignore SIGPIPE concerns: Go returns EPIPE as an error; handle it as a read/write error.

## CLI (cmd/cm2d)

Add a `herdr` subcommand:
- `cm2d herdr list` — one snapshot, print a table: pane, kind, status, focused, session id (short), cwd basename. `--json` prints the Snapshot as JSON.
- `cm2d herdr watch` — run the Watcher and print a line per change until Ctrl-C. It keeps waiting while
  Herdr is down and prints `herdr is not running; waiting` on stdout. It prints `herdr error: <err>` on
  stderr for each `OnError` report.
- `cm2d herdr focus <pane_id>` — call `agent.focus`. For humans only; tests never call it against the live server.
  A blank or whitespace-only pane id is a usage error.
- Every command takes `--socket PATH` or `--session NAME` (see socket resolution above; `--socket` wins over
  `--session`, and `--session` wins over the environment).
- Exit codes: 0 on success, 1 on a runtime error (including "Herdr is not running" for `list` and `focus`),
  2 on a usage error. `-h` and `--help` (also after a command) print the usage on stdout and exit 0.

## Tests

- Unit tests with a **fake Herdr server** on a Unix socket (keep the path short: macOS limits
  socket paths to ~104 bytes; use `os.MkdirTemp("", "hd")`). The fake must enforce one request
  per connection, answer ping/snapshot/agent.list/agent.focus, accept `events.subscribe`
  (reject unknown pane ids by error + close), push scripted events, and inject `events_lost`.
- Cover: decoding (lenient, unknown fields, null optionals, odd status -> unknown), APIError
  mapping, socket path resolution, unavailable server (ENOENT and refused), Watcher start/
  publish/debounce/resubscribe-on-new-pane/rejection-retry/events_lost-recovery/poll-safety-net/
  clean shutdown, no publish when only noise fields change, no fd leaks across many calls
  (count open fds or wrap the dialer), `go test -race` clean.
- The fake can hold snapshot replies (a held read is stale), fail a method N times, and reports the
  most subscription connections open at once (`PeakSubs`). It writes `subscription_started` before
  any event can reach the new subscription.
- Event-driven tests wait until the reconcile read (the second snapshot reply) is built, and keep the
  poll off, so a publish can only come from the event path.
- Also covered: the first subscribe request names every global event; connection and fd accounting on
  every subscribe path; backoff growth, reset and the poll cap (the retry sleep is a test hook); the
  new subscription opens before the old one closes; a reply line over 64 KB; an event during an
  in-flight read; the error semantics above; socket resolution precedence and the per-OS path limit;
  CLI exit codes, help and `herdr error:` lines.
- One **live, read-only** test behind build tag `herdrlive`: ping, snapshot, and a 3 s
  subscription against the real socket. Skips if Herdr is not running. Never focuses.

## Done when

- `go vet ./...`, `gofmt -l .` (empty), `go test -race ./...` pass.
- `go test -tags herdrlive ./internal/herdr/` passes against the live Herdr on this Mac.
- `cm2d herdr list` shows the live agents; `cm2d herdr watch` runs and exits cleanly on Ctrl-C.
