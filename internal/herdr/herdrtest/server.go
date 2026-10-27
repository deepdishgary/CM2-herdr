// Package herdrtest is a fake Herdr server for tests. It speaks the same
// newline-delimited JSON protocol on a Unix socket: one request per
// connection, except events.subscribe, which keeps its connection open.
//
// It never touches the real Herdr socket.
package herdrtest

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Agent is one raw AgentInfo record. Tests may edit any key.
type Agent map[string]any

// NewAgent returns a complete record. An empty kind leaves "agent" out.
func NewAgent(pane, kind, status string) Agent {
	ws := pane
	if i := strings.IndexByte(pane, ':'); i > 0 {
		ws = pane[:i]
	}
	a := Agent{
		"terminal_id":      "term-" + pane,
		"agent_status":     status,
		"workspace_id":     ws,
		"tab_id":           ws + ":t1",
		"pane_id":          pane,
		"focused":          false,
		"revision":         1,
		"state_change_seq": 0,
		"cwd":              "/work/" + pane,
	}
	if kind != "" {
		a["agent"] = kind
	}
	return a
}

// State is a copy of the server's counters.
type State struct {
	Calls       map[string]int
	Accepted    int
	Subs        int            // open subscription connections
	PeakSubs    int            // most subscription connections open at the same time
	Served      map[string]int // session.snapshot replies built (Calls counts arrivals)
	SubRequests [][]map[string]any
	FocusCalls  []string
}

type apiErr struct {
	code, msg string
	left      int // replies left; -1 means until cleared
}

type sub struct {
	c      net.Conn
	id     string
	wmu    sync.Mutex
	global map[string]bool
	status map[string]bool
}

// Server is the fake. Create it with New.
type Server struct {
	tb   testing.TB
	dir  string
	path string

	mu         sync.Mutex
	ln         net.Listener
	conns      map[net.Conn]struct{}
	agents     []any
	focused    string
	version    string
	protocol   int
	calls      map[string]int
	accepted   int
	focusCalls []string
	subReqs    [][]map[string]any
	subs       map[*sub]struct{}
	peakSubs   int
	served     map[string]int
	gate       chan struct{}
	rejectN    int
	stall      bool
	fail       map[string]*apiErr
	raw        map[string]string
	notify     chan struct{}
	wg         sync.WaitGroup
}

var knownSubs = map[string]bool{
	"pane.created": true, "pane.closed": true, "pane.exited": true,
	"pane.moved": true, "pane.agent_detected": true, "pane.focused": true,
	"pane.updated": true, "pane.agent_status_changed": true,
	"tab.focused": true, "workspace.focused": true,
}

// ShortDir makes a short temp dir. macOS limits socket paths to about 104 bytes.
func ShortDir(tb testing.TB) string {
	tb.Helper()
	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// New starts a server. It stops at test cleanup.
func New(tb testing.TB) *Server {
	tb.Helper()
	dir := ShortDir(tb)
	s := &Server{
		tb: tb, dir: dir, path: filepath.Join(dir, "s.sock"),
		conns: map[net.Conn]struct{}{}, calls: map[string]int{}, served: map[string]int{},
		subs: map[*sub]struct{}{}, fail: map[string]*apiErr{}, raw: map[string]string{},
		version: "0.9.3", protocol: 22, notify: make(chan struct{}),
	}
	s.Start()
	tb.Cleanup(s.Stop)
	return s
}

// Path is the socket path.
func (s *Server) Path() string { return s.path }

// Start listens again on the same path (after Stop). State is kept.
func (s *Server) Start() {
	s.tb.Helper()
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		s.tb.Fatal(err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.wg.Add(1)
	go s.acceptLoop(ln)
}

// Stop closes the listener and every connection, and removes the socket file.
func (s *Server) Stop() {
	s.mu.Lock()
	ln := s.ln
	s.ln = nil
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for _, c := range conns {
		c.Close()
	}
	s.wg.Wait()
}

func (s *Server) acceptLoop(ln net.Listener) {
	defer s.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.accepted++
		s.bumpLocked()
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handle(c)
	}
}

func (s *Server) bumpLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}

func (s *Server) drop(c net.Conn) {
	c.Close()
	s.mu.Lock()
	delete(s.conns, c)
	s.bumpLocked()
	s.mu.Unlock()
}

func writeLine(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	c.Write(append(b, '\n'))
}

func errBody(id, code, msg string) map[string]any {
	return map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}}
}

func (s *Server) handle(c net.Conn) {
	defer s.wg.Done()
	defer s.drop(c)
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		ID     string          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &req) != nil {
		writeLine(c, errBody("", "invalid_request", "bad json"))
		return
	}

	s.mu.Lock()
	s.calls[req.Method]++
	s.bumpLocked()
	stall := s.stall
	raw, hasRaw := s.raw[req.Method]
	fail, hasFail := s.fail[req.Method]
	if hasFail {
		fail = &apiErr{code: fail.code, msg: fail.msg}
		if f := s.fail[req.Method]; f.left > 0 {
			if f.left--; f.left == 0 {
				delete(s.fail, req.Method)
			}
		}
	}
	s.mu.Unlock()

	switch {
	case stall:
		io.Copy(io.Discard, c) // never answer; wait for the client to give up
		return
	case hasRaw:
		c.Write([]byte(raw))
		return
	case hasFail:
		writeLine(c, errBody(req.ID, fail.code, fail.msg))
		return
	}

	switch req.Method {
	case "ping":
		s.mu.Lock()
		v, p := s.version, s.protocol
		s.mu.Unlock()
		writeLine(c, map[string]any{"id": req.ID, "result": map[string]any{
			"type": "pong", "version": v, "protocol": p, "capabilities": map[string]any{}}})
	case "session.snapshot":
		s.mu.Lock()
		s.served[req.Method]++
		gate := s.gate
		snap := map[string]any{
			"version": s.version, "protocol": s.protocol,
			"workspaces": []any{}, "tabs": []any{}, "panes": []any{}, "layouts": []any{},
			"agents": s.agentsCopyLocked(), "focused_pane_id": nilIfEmpty(s.focused),
			"focused_tab_id": nil, "focused_workspace_id": nil,
			"future_field": map[string]any{"x": 1},
		}
		s.bumpLocked()
		s.mu.Unlock()
		if gate != nil {
			<-gate // the content above is already fixed: a held read is a stale read
		}
		writeLine(c, map[string]any{"id": req.ID, "result": map[string]any{"type": "session_snapshot", "snapshot": snap}})
	case "agent.list":
		s.mu.Lock()
		list := s.agentsCopyLocked()
		s.mu.Unlock()
		writeLine(c, map[string]any{"id": req.ID, "result": map[string]any{"type": "agent_list", "agents": list}})
	case "agent.focus":
		var p struct {
			Target string `json:"target"`
		}
		json.Unmarshal(req.Params, &p)
		if !s.focus(p.Target) {
			writeLine(c, errBody(req.ID, "agent_not_found", "no agent "+p.Target))
			return
		}
		writeLine(c, map[string]any{"id": req.ID, "result": map[string]any{"type": "ok"}})
	case "events.subscribe":
		s.subscribe(c, req.ID, req.Params)
	default:
		writeLine(c, errBody(req.ID, "unknown_method", req.Method))
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Server) agentsCopyLocked() []any {
	out := make([]any, len(s.agents))
	for i, a := range s.agents {
		if m, ok := a.(Agent); ok {
			cp := Agent{}
			for k, v := range m {
				cp[k] = v
			}
			out[i] = cp
		} else {
			out[i] = a
		}
	}
	return out
}

func (s *Server) focus(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.focusCalls = append(s.focusCalls, target)
	s.bumpLocked()
	found := ""
	for _, a := range s.agents {
		if m, ok := a.(Agent); ok && (m["pane_id"] == target || m["name"] == target) {
			found, _ = m["pane_id"].(string)
		}
	}
	if found == "" {
		return false
	}
	s.setFocusedLocked(found)
	return true
}

func (s *Server) setFocusedLocked(pane string) {
	s.focused = pane
	for _, a := range s.agents {
		if m, ok := a.(Agent); ok {
			m["focused"] = m["pane_id"] == pane
		}
	}
}

func (s *Server) subscribe(c net.Conn, id string, params json.RawMessage) {
	var p struct {
		Subscriptions []map[string]any `json:"subscriptions"`
	}
	if json.Unmarshal(params, &p) != nil {
		writeLine(c, errBody(id, "invalid_request", "bad params"))
		return
	}
	sb := &sub{c: c, id: id, global: map[string]bool{}, status: map[string]bool{}}

	s.mu.Lock()
	s.subReqs = append(s.subReqs, p.Subscriptions)
	s.bumpLocked()
	if s.rejectN > 0 {
		s.rejectN--
		s.mu.Unlock()
		writeLine(c, errBody(id, "pane_not_found", "forced rejection"))
		return
	}
	known := map[string]bool{}
	for _, a := range s.agents {
		if m, ok := a.(Agent); ok {
			if pid, ok := m["pane_id"].(string); ok {
				known[pid] = true
			}
		}
	}
	for _, e := range p.Subscriptions {
		t, _ := e["type"].(string)
		if !knownSubs[t] {
			s.mu.Unlock()
			writeLine(c, errBody(id, "invalid_request", "unknown subscription "+t))
			return
		}
		if t == "pane.agent_status_changed" {
			pid, _ := e["pane_id"].(string)
			if pid == "" || !known[pid] {
				s.mu.Unlock()
				writeLine(c, errBody(id, "pane_not_found", "pane "+pid))
				return
			}
			sb.status[pid] = true
		} else {
			sb.global[t] = true
		}
	}
	// Hold the writer lock from registration until the ack is written. Push
	// takes this lock before it writes, so no event can reach the client
	// before subscription_started.
	sb.wmu.Lock()
	s.subs[sb] = struct{}{}
	if len(s.subs) > s.peakSubs {
		s.peakSubs = len(s.subs)
	}
	s.bumpLocked()
	s.mu.Unlock()

	writeLine(c, map[string]any{"id": id, "result": map[string]any{"type": "subscription_started"}})
	sb.wmu.Unlock()

	io.Copy(io.Discard, c) // returns when the client closes
	s.mu.Lock()
	delete(s.subs, sb)
	s.bumpLocked()
	s.mu.Unlock()
}

// SetAgents replaces the agent list.
func (s *Server) SetAgents(agents ...Agent) {
	recs := make([]any, len(agents))
	for i, a := range agents {
		recs[i] = a
	}
	s.SetAgentsRaw(recs...)
}

// SetAgentsRaw replaces the agent list with arbitrary JSON values.
func (s *Server) SetAgentsRaw(records ...any) {
	s.mu.Lock()
	s.agents = records
	s.bumpLocked()
	s.mu.Unlock()
}

// SetFocused marks one pane focused (agent flags and focused_pane_id).
func (s *Server) SetFocused(pane string) {
	s.mu.Lock()
	s.setFocusedLocked(pane)
	s.bumpLocked()
	s.mu.Unlock()
}

// Mutate edits the agent with the given pane id in place.
func (s *Server) Mutate(pane string, fn func(Agent)) {
	s.mu.Lock()
	for _, a := range s.agents {
		if m, ok := a.(Agent); ok && m["pane_id"] == pane {
			fn(m)
		}
	}
	s.bumpLocked()
	s.mu.Unlock()
}

// SetVersion sets the version and protocol that ping and snapshot report.
func (s *Server) SetVersion(v string, protocol int) {
	s.mu.Lock()
	s.version, s.protocol = v, protocol
	s.mu.Unlock()
}

// FailMethod makes a method return an error response until ClearFail.
func (s *Server) FailMethod(method, code, msg string) {
	s.FailNext(method, code, msg, -1)
}

// FailNext makes the next n calls of a method return an error response.
func (s *Server) FailNext(method, code, msg string, n int) {
	s.mu.Lock()
	s.fail[method] = &apiErr{code: code, msg: msg, left: n}
	s.mu.Unlock()
}

// ClearFail ends FailMethod and FailNext for a method.
func (s *Server) ClearFail(method string) {
	s.mu.Lock()
	delete(s.fail, method)
	s.mu.Unlock()
}

// SetRaw makes a method reply with these exact bytes, then close.
func (s *Server) SetRaw(method, reply string) {
	s.mu.Lock()
	s.raw[method] = reply
	s.mu.Unlock()
}

// HoldSnapshots makes every session.snapshot reply wait. The reply content is
// fixed when the request arrives, so a held read returns stale data. The
// returned function releases all held replies and ends the hold. It is safe to
// call more than once, and runs at test cleanup.
func (s *Server) HoldSnapshots() (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.gate = gate
	s.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			s.mu.Lock()
			if s.gate == gate {
				s.gate = nil
			}
			s.mu.Unlock()
			close(gate)
		})
	}
	s.tb.Cleanup(release)
	return release
}

// Stall makes the server read requests but never answer.
func (s *Server) Stall(on bool) {
	s.mu.Lock()
	s.stall = on
	s.mu.Unlock()
}

// RejectNextSubscribe makes the next n subscribe requests fail with pane_not_found.
func (s *Server) RejectNextSubscribe(n int) {
	s.mu.Lock()
	s.rejectN = n
	s.mu.Unlock()
}

// Push sends an event to every subscription that matches it.
func (s *Server) Push(kind string, data map[string]any) {
	s.PushN(kind, data, 1)
}

// PushN sends the same event n times, in one write per subscription.
func (s *Server) PushN(kind string, data map[string]any, n int) {
	line, _ := json.Marshal(map[string]any{"event": kind, "data": data})
	line = append(line, '\n')
	var buf []byte
	for i := 0; i < n; i++ {
		buf = append(buf, line...)
	}
	pane, _ := data["pane_id"].(string)
	s.mu.Lock()
	var targets []*sub
	for sb := range s.subs {
		if sb.global[kind] || (kind == "pane.agent_status_changed" && sb.status[pane]) {
			targets = append(targets, sb)
		}
	}
	s.mu.Unlock()
	for _, sb := range targets {
		sb.wmu.Lock()
		sb.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		sb.c.Write(buf)
		sb.wmu.Unlock()
	}
}

// InjectEventsLost sends the events_lost error to every subscriber, then closes them.
func (s *Server) InjectEventsLost() {
	s.mu.Lock()
	var targets []*sub
	for sb := range s.subs {
		targets = append(targets, sb)
	}
	s.mu.Unlock()
	for _, sb := range targets {
		sb.wmu.Lock()
		writeLine(sb.c, errBody(sb.id, "events_lost", "subscriber fell behind"))
		sb.wmu.Unlock()
		sb.c.Close()
	}
}

// DropSubscribers closes every subscription connection with no message.
func (s *Server) DropSubscribers() {
	s.mu.Lock()
	var targets []*sub
	for sb := range s.subs {
		targets = append(targets, sb)
	}
	s.mu.Unlock()
	for _, sb := range targets {
		sb.c.Close()
	}
}

// State returns a copy of the counters.
func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *Server) stateLocked() State {
	st := State{Calls: map[string]int{}, Served: map[string]int{}, Accepted: s.accepted, Subs: len(s.subs), PeakSubs: s.peakSubs}
	for k, v := range s.calls {
		st.Calls[k] = v
	}
	for k, v := range s.served {
		st.Served[k] = v
	}
	st.FocusCalls = append(st.FocusCalls, s.focusCalls...)
	st.SubRequests = append(st.SubRequests, s.subReqs...)
	return st
}

// Await blocks until pred is true. It wakes on every server change, not on a timer.
func (s *Server) Await(tb testing.TB, what string, pred func(State) bool) {
	tb.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		ok := pred(s.stateLocked())
		ch := s.notify
		s.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			tb.Fatalf("timed out waiting for: %s", what)
		}
	}
}
