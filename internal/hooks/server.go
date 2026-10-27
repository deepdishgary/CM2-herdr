// Package hooks is the HTTP server that receives Claude Code hook events
// and holds PermissionRequest calls until the pad answers.
// See docs/plans/2026-10-23-cm2d-daemon.md section 3.2.
package hooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cm2-claude/internal/session"
)

// Decision is the answer to a held permission request.
type Decision int

const (
	Allow Decision = iota
	Deny
	Pass // empty 200: no decision, the normal dialog decides
)

const (
	// DefaultMaxHold stays under the 600 s hook timeout.
	DefaultMaxHold = 590 * time.Second
	// MaxBody caps every request body.
	MaxBody = 1 << 20
	// DefaultBodyTimeout is how long a client may take to send a body.
	DefaultBodyTimeout = 10 * time.Second

	// Header carrying CLAUDE_CODE_ENTRYPOINT from the http hooks.
	EntrypointHeader = "X-Cm2-Entrypoint"
	// Header carrying CLAUDE_CODE_HOST_SESSION_ID (the desktop record id).
	// The API trusts every process of this user on loopback. A forged id can
	// only steer focus to another checked record; cm2d runs no other command.
	HostSessionHeader = "X-Cm2-Host-Session"

	denyMessage = "Denied from the macropad"
)

// Exact reply bodies.
const (
	allowBody = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
	denyBody  = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"` + denyMessage + `"}}}`
)

// Server receives hook events and holds permission requests.
type Server struct {
	Addr    string                   // "127.0.0.1:47731"
	Events  chan<- session.HookEvent // to the run loop; a full channel drops the event
	CanHold func() bool              // false when the pad is not connected; nil means true
	MaxHold time.Duration            // default 590 s
	Status  func() any               // GET /status body; an error value gives a 503
	Logger  *slog.Logger
	// BodyTimeout limits the read of one request body; default 10 s. It does
	// not limit a held PermissionRequest.
	BodyTimeout time.Duration

	initOnce sync.Once
	mu       sync.Mutex
	held     map[string]*heldReq
	shutdown chan struct{} // closed when ListenAndServe starts to stop
	stopOnce sync.Once
	ready    chan struct{} // closed once the port is bound
	bound    atomic.Value  // string: the bound address
	dropped  atomic.Uint64
}

// heldReq is one parked PermissionRequest.
type heldReq struct {
	id, sessionID string
	ch            chan Decision // buffered 1; one send at most
}

func (s *Server) init() {
	s.initOnce.Do(func() {
		s.held = make(map[string]*heldReq)
		s.shutdown = make(chan struct{})
		s.ready = make(chan struct{})
	})
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (s *Server) maxHold() time.Duration {
	if s.MaxHold > 0 {
		return s.MaxHold
	}
	return DefaultMaxHold
}

func (s *Server) bodyTimeout() time.Duration {
	if s.BodyTimeout > 0 {
		return s.BodyTimeout
	}
	return DefaultBodyTimeout
}

// Dropped is the number of events dropped because Events was full.
func (s *Server) Dropped() uint64 { return s.dropped.Load() }

// HeldCount is the number of requests held right now.
func (s *Server) HeldCount() int {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held)
}

// Ready is closed once ListenAndServe has bound the port.
func (s *Server) Ready() <-chan struct{} { s.init(); return s.ready }

// BoundAddr is the address ListenAndServe bound, or "" before that.
func (s *Server) BoundAddr() string {
	v, _ := s.bound.Load().(string)
	return v
}

// Handler is the HTTP handler; ListenAndServe serves it.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	mux.HandleFunc("/cm2d/v1/session", s.method(http.MethodPost, s.handleSession))
	mux.HandleFunc("/cm2d/v1/event", s.method(http.MethodPost, s.handleEvent))
	mux.HandleFunc("/cm2d/v1/permission", s.method(http.MethodPost, s.handlePermission))
	mux.HandleFunc("/cm2d/v1/status", s.method(http.MethodGet, s.handleStatus))
	return mux
}

func (s *Server) method(m string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A panic must never take the daemon down.
		defer func() {
			if v := recover(); v != nil {
				s.log().Error("hook handler panic", "panic", v, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		if r.Method != m {
			w.Header().Set("Allow", m)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Only loopback names: a rebinding page uses its own host name.
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		// Claude Code and the forwarder send JSON. A browser form or
		// text/plain POST from a web page is not a hook.
		if m == http.MethodPost && !isJSON(r.Header.Get("Content-Type")) {
			http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		h(w, r)
	}
}

// loopbackHost tells whether a Host header names this machine's loopback.
func loopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return h == "127.0.0.1" || h == "::1" || strings.EqualFold(h, "localhost")
}

// isJSON tells whether a Content-Type header is application/json.
func isJSON(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// ListenAndServe binds Addr and serves until ctx ends. Binding the port is
// the single-instance lock, so a bind error is returned at once. When ctx
// ends it releases every held request with an empty 200 and returns nil.
func (s *Server) ListenAndServe(ctx context.Context) error {
	s.init()
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("hooks: listen %s: %w", s.Addr, err)
	}
	s.bound.Store(ln.Addr().String())
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	close(s.ready)

	select {
	case err := <-errc:
		s.stopOnce.Do(func() { close(s.shutdown) })
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	// Wake every held handler first, so Shutdown does not wait on them.
	s.stopOnce.Do(func() { close(s.shutdown) })
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
	}
	return nil
}

// emit sends an event without blocking. It reports whether the event was queued.
func (s *Server) emit(ev session.HookEvent) bool {
	select {
	case s.Events <- ev: // a nil channel is never ready, so it drops
		s.log().Debug("hook event", "event", ev.Name, "session", shortID(ev.SessionID),
			"attended", ev.Attended, "entrypoint", ev.Entrypoint, "tool", ev.ToolName)
		return true
	default:
		n := s.dropped.Add(1)
		s.log().Warn("hook event dropped: channel full", "event", ev.Name, "dropped_total", n)
		return false
	}
}

// Resolve answers a held request. It reports whether the request was held.
// Allow and Deny send the decision JSON. Pass sends an empty 200 and emits
// PermissionReleased, like any other release.
func (s *Server) Resolve(requestID string, d Decision) bool {
	s.init()
	s.mu.Lock()
	h, ok := s.held[requestID]
	if ok {
		delete(s.held, requestID)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	h.ch <- d
	return true
}

// Holds reports whether the request is held now.
func (s *Server) Holds(requestID string) bool {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.held[requestID]
	return ok
}

// ReleaseAll releases every held request with an empty 200. The daemon calls
// it when the pad goes away, since nothing can answer a request then.
func (s *Server) ReleaseAll() {
	s.init()
	s.mu.Lock()
	hs := make([]*heldReq, 0, len(s.held))
	for id, h := range s.held {
		delete(s.held, id)
		hs = append(hs, h)
	}
	s.mu.Unlock()
	for _, h := range hs {
		h.ch <- Pass
	}
}

// ReleaseSession releases every held request of the session with an empty 200.
func (s *Server) ReleaseSession(sessionID string) {
	s.init()
	s.mu.Lock()
	var hs []*heldReq
	for id, h := range s.held {
		if h.sessionID == sessionID {
			delete(s.held, id)
			hs = append(hs, h)
		}
	}
	s.mu.Unlock()
	for _, h := range hs {
		h.ch <- Pass
	}
}

// payload is the part of a hook body that cm2d reads.
type payload struct {
	SessionID        string `json:"session_id"`
	CWD              string `json:"cwd"`
	HookEventName    string `json:"hook_event_name"`
	ToolName         string `json:"tool_name"`
	NotificationType string `json:"notification_type"`
	Source           string `json:"source"`
}

// readBody reads the capped body. On failure it writes the error reply.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	// A deadline for the body only. A held request must outlive it, so it
	// is cleared once the body is in.
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(s.bodyTimeout()))
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err == nil {
		rc.SetReadDeadline(time.Time{})
	}
	// On an error the deadline stays: the server drains the rest of the
	// body after the reply, and a stalled client must not block that.
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "cannot read body", http.StatusBadRequest)
		}
		return nil, false
	}
	return b, true
}

func parsePayload(b []byte) (payload, error) {
	var p payload
	if err := json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	if p.SessionID == "" {
		return p, errors.New("missing session_id")
	}
	return p, nil
}

// ok sends an empty 200.
func ok(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

func bad(w http.ResponseWriter, msg string, err error) {
	http.Error(w, msg+": "+err.Error(), http.StatusBadRequest)
}

// handleSession takes the SessionStart forwarder's envelope.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	b, good := s.readBody(w, r)
	if !good {
		return
	}
	var env struct {
		Payload    json.RawMessage `json:"payload"`
		Attended   string          `json:"attended"`
		Entrypoint string          `json:"entrypoint"`
		HerdrPane  string          `json:"herdr_pane"`
		HostID     string          `json:"host_session_id"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		bad(w, "bad envelope", err)
		return
	}
	if len(env.Payload) == 0 {
		bad(w, "bad envelope", errors.New("missing payload"))
		return
	}
	p, err := parsePayload(env.Payload)
	if err != nil {
		bad(w, "bad payload", err)
		return
	}
	s.emit(session.HookEvent{
		Name:        "SessionStart",
		SessionID:   p.SessionID,
		CWD:         p.CWD,
		Attended:    env.Attended,
		Entrypoint:  env.Entrypoint,
		HerdrPane:   env.HerdrPane,
		HostSession: hostID(env.HostID),
		Source:      p.Source,
		At:          time.Now(),
	})
	ok(w)
}

// Names accepted on /event.
var eventNames = map[string]bool{
	"UserPromptSubmit": true, "Stop": true, "StopFailure": true,
	"SessionEnd": true, "Notification": true, "PostToolUse": true,
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	b, good := s.readBody(w, r)
	if !good {
		return
	}
	p, err := parsePayload(b)
	if err != nil {
		bad(w, "bad payload", err)
		return
	}
	if !eventNames[p.HookEventName] {
		bad(w, "bad payload", fmt.Errorf("unexpected hook_event_name %q", p.HookEventName))
		return
	}
	s.emit(session.HookEvent{
		Name:             p.HookEventName,
		SessionID:        p.SessionID,
		CWD:              p.CWD,
		Entrypoint:       r.Header.Get(EntrypointHeader),
		HostSession:      hostID(r.Header.Get(HostSessionHeader)),
		NotificationType: p.NotificationType,
		ToolName:         p.ToolName,
		At:               time.Now(),
	})
	ok(w)
}

// hostID keeps a desktop record id only if it has the right shape. An unset
// variable arrives empty or as the literal "$CLAUDE_CODE_HOST_SESSION_ID".
func hostID(v string) string {
	if session.ValidHostID(v) {
		return v
	}
	return ""
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("r%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	b, good := s.readBody(w, r)
	if !good {
		return
	}
	p, err := parsePayload(b)
	if err != nil {
		bad(w, "bad payload", err)
		return
	}
	// A question needs the user's own answer or reading, so it is never held:
	// a plain allow would not answer AskUserQuestion, and would approve a plan
	// he has not read for ExitPlanMode.
	question := p.ToolName == "AskUserQuestion" || p.ToolName == "ExitPlanMode"
	ev := session.HookEvent{
		Name:        "PermissionRequest",
		SessionID:   p.SessionID,
		CWD:         p.CWD,
		Entrypoint:  r.Header.Get(EntrypointHeader),
		HostSession: hostID(r.Header.Get(HostSessionHeader)),
		ToolName:    p.ToolName,
		RequestID:   newRequestID(),
		IsQuestion:  question,
		At:          time.Now(),
	}

	// Questions and an absent pad get an empty 200 at once. The event
	// still goes out: the dialog is live and the key should show it.
	if question || (s.CanHold != nil && !s.CanHold()) {
		s.emit(ev)
		ok(w)
		return
	}

	// Register before the event goes out, so Resolve can find it at once.
	h := &heldReq{id: ev.RequestID, sessionID: p.SessionID, ch: make(chan Decision, 1)}
	s.mu.Lock()
	s.held[h.id] = h
	s.mu.Unlock()

	// The pad may have dropped since the first check, after ReleaseAll ran.
	// The check comes after the registration, so one of the two sees it.
	if s.CanHold != nil && !s.CanHold() {
		s.unhold(h)
		s.emit(ev)
		ok(w)
		return
	}

	if !s.emit(ev) {
		// The run loop never heard of it, so nothing could answer. Do not hold.
		s.unhold(h)
		ok(w)
		return
	}

	timer := time.NewTimer(s.maxHold())
	defer timer.Stop()
	var d Decision
	select {
	case d = <-h.ch:
	case <-r.Context().Done():
		d = s.leave(h)
	case <-timer.C:
		d = s.leave(h)
	case <-s.shutdown:
		d = s.leave(h)
	}

	switch d {
	case Allow:
		writeJSON(w, allowBody)
	case Deny:
		writeJSON(w, denyBody)
	default:
		ok(w) // a gone client just ignores this
		s.emit(session.HookEvent{
			Name: "PermissionReleased", SessionID: p.SessionID, RequestID: h.id, At: time.Now(),
		})
	}
}

// unhold removes h from the table. It reports whether h was still there.
func (s *Server) unhold(h *heldReq) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[h.id] == h {
		delete(s.held, h.id)
		return true
	}
	return false
}

// leave handles a wait that ended without an answer. If a Resolve or
// ReleaseSession got in first, its decision wins.
func (s *Server) leave(h *heldReq) Decision {
	if s.unhold(h) {
		return Pass
	}
	return <-h.ch // the other side removed h, so its send is certain
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var v any = struct{}{}
	if s.Status != nil {
		v = s.Status()
	}
	if err, isErr := v.(error); isErr {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

// shortID shortens a session id for logs.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
