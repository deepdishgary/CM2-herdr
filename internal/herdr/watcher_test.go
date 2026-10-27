package herdr

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"cm2-claude/internal/herdr/herdrtest"
)

// harness runs a Watcher against a fake server and collects what it publishes.
type harness struct {
	t      *testing.T
	srv    *herdrtest.Server
	w      *Watcher
	out    chan Snapshot
	cancel context.CancelFunc
	done   chan error

	mu     sync.Mutex
	errs   []error // everything OnError received
	pubErr []int   // len(errs) at each publish, in publish order
}

// errors returns a copy of the errors OnError has received.
func (h *harness) errors() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.errs...)
}

// awaitErrors waits until pred holds for the errors received so far.
func (h *harness) awaitErrors(what string, pred func([]error) bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !pred(h.errors()) {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for errors: %s; got %v", what, h.errors())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// apiCodes counts the *APIError values by code.
func apiCodes(errs []error) map[string]int {
	out := map[string]int{}
	for _, e := range errs {
		var ae *APIError
		if errors.As(e, &ae) {
			out[ae.Code]++
		}
	}
	return out
}

func startHarness(t *testing.T, srv *herdrtest.Server, tune func(*Watcher)) *harness {
	t.Helper()
	return startHarnessClient(t, srv, NewClient(srv.Path()), tune)
}

// startHarnessClient is startHarness with a caller-made client. srv may be nil.
func startHarnessClient(t *testing.T, srv *herdrtest.Server, c *Client, tune func(*Watcher)) *harness {
	t.Helper()
	c.IOTimeout = 2 * time.Second
	w := &Watcher{
		Client:       c,
		PollInterval: time.Hour, // off unless a test turns it on
		Debounce:     20 * time.Millisecond,
		BackoffMin:   5 * time.Millisecond,
		BackoffMax:   40 * time.Millisecond,
	}
	if tune != nil {
		tune(w)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, srv: srv, w: w, out: make(chan Snapshot, 256), cancel: cancel, done: make(chan error, 1)}
	if w.OnError == nil {
		w.OnError = func(err error) {
			h.mu.Lock()
			h.errs = append(h.errs, err)
			h.mu.Unlock()
		}
	}
	go func() {
		h.done <- w.Run(ctx, func(s Snapshot) {
			h.mu.Lock()
			h.pubErr = append(h.pubErr, len(h.errs))
			h.mu.Unlock()
			h.out <- s
		})
	}()
	t.Cleanup(func() { h.stop() })
	return h
}

func (h *harness) stop() error {
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err // let a second stop see it too
		return err
	case <-time.After(5 * time.Second):
		h.t.Fatal("Run did not return after cancel")
		return nil
	}
}

// next waits for the next published snapshot.
func (h *harness) next() Snapshot {
	h.t.Helper()
	select {
	case s := <-h.out:
		return s
	case <-time.After(10 * time.Second):
		h.t.Fatal("timed out waiting for a publish")
		return Snapshot{}
	}
}

// nextWhere skips publishes until pred is true.
func (h *harness) nextWhere(what string, pred func(Snapshot) bool) Snapshot {
	h.t.Helper()
	for {
		s := h.next()
		if pred(s) {
			return s
		}
	}
}

// quiet fails if anything is published while fn runs and settles.
func (h *harness) expectNoPublish(settle func()) {
	h.t.Helper()
	settle()
	select {
	case s := <-h.out:
		h.t.Fatalf("unexpected publish: %+v", s)
	default:
	}
}

// reads waits until the server has built n snapshot replies. The reply content
// is fixed at that point, so a later Mutate cannot leak into those reads.
func reads(n int) func(herdrtest.State) bool {
	return func(s herdrtest.State) bool { return s.Served["session.snapshot"] >= n }
}

func subscribed(n int) func(herdrtest.State) bool {
	return func(s herdrtest.State) bool { return s.Subs == n }
}

func statusPanes(reqs [][]map[string]any, idx int) map[string]bool {
	out := map[string]bool{}
	for _, e := range reqs[idx] {
		if e["type"] == "pane.agent_status_changed" {
			out[e["pane_id"].(string)] = true
		}
	}
	return out
}

func TestWatcherPublishesInitialSnapshotAndSubscribes(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"), herdrtest.NewAgent("w1:p2", "codex", "working"))
	h := startHarness(t, srv, nil)

	first := h.next()
	if !first.Available || len(first.Agents) != 2 {
		t.Fatalf("first publish: %+v", first)
	}
	srv.Await(t, "one subscription", subscribed(1))
	st := srv.State()
	if len(st.SubRequests) != 1 {
		t.Fatalf("subscribe requests = %d", len(st.SubRequests))
	}
	types := map[string]bool{}
	for _, e := range st.SubRequests[0] {
		types[e["type"].(string)] = true
	}
	for _, want := range []string{"pane.created", "pane.closed", "pane.exited", "pane.moved", "pane.agent_detected"} {
		if !types[want] {
			t.Errorf("missing global subscription %s", want)
		}
	}
	if sp := statusPanes(st.SubRequests, 0); len(sp) != 2 || !sp["w1:p1"] || !sp["w1:p2"] {
		t.Fatalf("status subscriptions = %v", sp)
	}
	// Step 3: a fresh snapshot after the subscription starts (2 reads so far).
	srv.Await(t, "reconcile read", func(s herdrtest.State) bool { return s.Served["session.snapshot"] >= 2 })
	h.expectNoPublish(func() {}) // identical state is not republished
}

func TestWatcherPublishesUnavailableOnceThenRecovers(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	srv.Stop() // socket file gone: ENOENT
	h := startHarness(t, srv, nil)

	first := h.next()
	if first.Available {
		t.Fatalf("first publish should be Available=false: %+v", first)
	}
	// Several retries must not publish again.
	time.Sleep(150 * time.Millisecond) // 3+ backoff cycles at 5..40ms
	select {
	case s := <-h.out:
		t.Fatalf("repeated unavailable publish: %+v", s)
	default:
	}
	srv.Start()
	up := h.next()
	if !up.Available || len(up.Agents) != 1 {
		t.Fatalf("after start: %+v", up)
	}
	srv.Await(t, "subscribed", subscribed(1))
}

func TestWatcherGoesUnavailableWhenServerStops(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Stop()
	if s := h.nextWhere("unavailable", func(s Snapshot) bool { return !s.Available }); len(s.Agents) != 0 {
		t.Fatalf("unavailable snapshot has agents: %+v", s)
	}
	srv.Start()
	h.nextWhere("available again", func(s Snapshot) bool { return s.Available })
	srv.Await(t, "resubscribed", subscribed(1))
}

func TestWatcherEventTriggersRefresh(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read built", reads(2)) // from here only an event can show the change

	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "blocked" })
	srv.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "blocked"})
	s := h.next()
	if s.Agents[0].Status != StatusBlocked {
		t.Fatalf("status = %s", s.Agents[0].Status)
	}
}

func TestWatcherDebouncesBursts(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	h := startHarness(t, srv, func(w *Watcher) { w.Debounce = 300 * time.Millisecond })
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read", func(s herdrtest.State) bool { return s.Served["session.snapshot"] >= 2 })
	base := srv.State().Calls["session.snapshot"]

	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "done" })
	srv.PushN("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "done"}, 50)
	if s := h.next(); s.Agents[0].Status != StatusDone {
		t.Fatalf("status = %s", s.Agents[0].Status)
	}
	if got := srv.State().Calls["session.snapshot"] - base; got != 1 {
		t.Fatalf("50 events caused %d snapshot reads, want 1", got)
	}
}

func TestWatcherIgnoresNoiseFields(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read", func(s herdrtest.State) bool { return s.Served["session.snapshot"] >= 2 })

	base := srv.State().Calls["session.snapshot"]
	srv.Mutate("w1:p1", func(a herdrtest.Agent) {
		a["revision"] = 99
		a["state_change_seq"] = 42
		a["terminal_title"] = "⠋ spinner"
		a["terminal_title_stripped"] = "spinner"
	})
	srv.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "working"})
	srv.Await(t, "refresh happened", func(s herdrtest.State) bool { return s.Calls["session.snapshot"] > base })
	h.expectNoPublish(func() {})

	// A real change afterwards still arrives, and is the very next publish.
	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["completion_seq"] = 3 })
	srv.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "working"})
	s := h.next()
	if s.Agents[0].CompletionSeq == nil || *s.Agents[0].CompletionSeq != 3 {
		t.Fatalf("publish: %+v", s.Agents[0])
	}
}

func TestSameStateFields(t *testing.T) {
	seq := func(n uint64) *uint64 { return &n }
	base := Snapshot{Available: true, FocusedPaneID: "w1:p1", Agents: []Agent{{
		PaneID: "w1:p1", Kind: "claude", Status: StatusIdle, Focused: true, DisplayName: "C", CWD: "/a",
		CompletionSeq: seq(1), Session: &AgentSession{Value: "s"},
	}}}
	clone := func() Snapshot {
		c := base
		c.Agents = append([]Agent(nil), base.Agents...)
		return c
	}
	noise := clone()
	noise.Agents[0].Revision = 5
	noise.Agents[0].StateChangeSeq = 5
	noise.Agents[0].Name = "n"
	noise.Version = "x"
	if !sameState(base, noise) {
		t.Fatal("noise fields must not count as a change")
	}
	mutations := map[string]func(*Snapshot){
		"available": func(s *Snapshot) { s.Available = false },
		"focusedid": func(s *Snapshot) { s.FocusedPaneID = "w9" },
		"pane":      func(s *Snapshot) { s.Agents[0].PaneID = "w2" },
		"kind":      func(s *Snapshot) { s.Agents[0].Kind = "codex" },
		"status":    func(s *Snapshot) { s.Agents[0].Status = StatusDone },
		"focused":   func(s *Snapshot) { s.Agents[0].Focused = false },
		"display":   func(s *Snapshot) { s.Agents[0].DisplayName = "D" },
		"cwd":       func(s *Snapshot) { s.Agents[0].CWD = "/b" },
		"session":   func(s *Snapshot) { s.Agents[0].Session = &AgentSession{Value: "t"} },
		"nosession": func(s *Snapshot) { s.Agents[0].Session = nil },
		"seqvalue":  func(s *Snapshot) { s.Agents[0].CompletionSeq = seq(2) },
		"seqnil":    func(s *Snapshot) { s.Agents[0].CompletionSeq = nil },
		"count":     func(s *Snapshot) { s.Agents = nil },
	}
	for name, mut := range mutations {
		m := clone()
		mut(&m)
		if sameState(base, m) {
			t.Errorf("%s: change not detected", name)
		}
	}
}

func TestWatcherResubscribesWhenPaneAppears(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read built", reads(2))

	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"), herdrtest.NewAgent("w1:p2", "codex", "idle"))
	srv.Push("pane.created", map[string]any{"pane_id": "w1:p2"})
	if s := h.next(); len(s.Agents) != 2 {
		t.Fatalf("publish: %+v", s)
	}
	srv.Await(t, "second subscription request", func(s herdrtest.State) bool { return len(s.SubRequests) == 2 })
	srv.Await(t, "old connection closed", subscribed(1))
	st := srv.State()
	if sp := statusPanes(st.SubRequests, 1); len(sp) != 2 || !sp["w1:p2"] {
		t.Fatalf("new subscription = %v", sp)
	}

	// The new pane's status events now reach the watcher. Reads so far: initial,
	// reconcile, event refresh, and the re-read after the resubscribe.
	srv.Await(t, "re-read after resubscribe built", reads(4))
	srv.Mutate("w1:p2", func(a herdrtest.Agent) { a["agent_status"] = "blocked" })
	srv.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p2", "workspace_id": "w1", "agent_status": "blocked"})
	s := h.nextWhere("p2 blocked", func(s Snapshot) bool { return len(s.Agents) == 2 && s.Agents[1].Status == StatusBlocked })
	_ = s

	// A pane going away shrinks the set again.
	srv.SetAgents(herdrtest.NewAgent("w1:p2", "codex", "blocked"))
	srv.Push("pane.closed", map[string]any{"pane_id": "w1:p1"})
	h.nextWhere("p1 gone", func(s Snapshot) bool { return len(s.Agents) == 1 })
	srv.Await(t, "third subscription request", func(s herdrtest.State) bool { return len(s.SubRequests) == 3 })
	srv.Await(t, "one connection", subscribed(1))
	if sp := statusPanes(srv.State().SubRequests, 2); len(sp) != 1 || !sp["w1:p2"] {
		t.Fatalf("shrunk subscription = %v", sp)
	}
}

func TestWatcherRetriesAfterSubscribeRejection(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	srv.RejectNextSubscribe(2)
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed after rejections", subscribed(1))
	st := srv.State()
	if len(st.SubRequests) != 3 {
		t.Fatalf("subscribe attempts = %d, want 3", len(st.SubRequests))
	}
	// Each retry re-read the snapshot first (no stale pane list).
	if st.Calls["session.snapshot"] < 3 {
		t.Fatalf("snapshot reads = %d", st.Calls["session.snapshot"])
	}
}

func TestWatcherRecoversFromEventsLost(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))

	// State changed while events were being lost.
	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "done" })
	srv.InjectEventsLost()
	s := h.next()
	if s.Agents[0].Status != StatusDone {
		t.Fatalf("not reconciled after events_lost: %+v", s.Agents[0])
	}
	srv.Await(t, "resubscribed", func(st herdrtest.State) bool { return len(st.SubRequests) == 2 && st.Subs == 1 })
	srv.Await(t, "reconcile after events_lost built", reads(4)) // initial, reconcile, restart, reconcile

	// And events flow again on the new connection.
	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "idle" })
	srv.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "idle"})
	if s := h.next(); s.Agents[0].Status != StatusIdle {
		t.Fatalf("status = %s", s.Agents[0].Status)
	}
}

func TestWatcherRecoversFromDroppedConnection(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "done" })
	srv.DropSubscribers()
	if s := h.next(); s.Agents[0].Status != StatusDone {
		t.Fatalf("status = %s", s.Agents[0].Status)
	}
	srv.Await(t, "resubscribed", func(st herdrtest.State) bool { return len(st.SubRequests) == 2 && st.Subs == 1 })
}

func TestWatcherPollIsASafetyNet(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	h := startHarness(t, srv, func(w *Watcher) { w.PollInterval = 30 * time.Millisecond })
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	// Change state and push no event at all.
	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "blocked" })
	if s := h.next(); s.Agents[0].Status != StatusBlocked {
		t.Fatalf("status = %s", s.Agents[0].Status)
	}
}

func TestWatcherPollNoticesNewPaneWithoutEvents(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	h := startHarness(t, srv, func(w *Watcher) { w.PollInterval = 30 * time.Millisecond })
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"), herdrtest.NewAgent("w1:p2", "claude", "idle"))
	h.next()
	srv.Await(t, "resubscribed with both panes", func(s herdrtest.State) bool {
		return len(s.SubRequests) >= 2 && len(statusPanes(s.SubRequests, len(s.SubRequests)-1)) == 2 && s.Subs == 1
	})
}

func TestWatcherCleanShutdown(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	before := runtime.NumGoroutine()
	h := startHarness(t, srv, func(w *Watcher) { w.PollInterval = 10 * time.Millisecond })
	h.next()
	srv.Await(t, "subscribed", subscribed(1))

	start := time.Now()
	err := h.stop()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("shutdown took %s", time.Since(start))
	}
	srv.Await(t, "subscription connection closed", subscribed(0))
	// Goroutines from Run, the reader and the connections are gone.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutines: %d before, %d after\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}
}

func TestWatcherShutdownWhileUnavailable(t *testing.T) {
	c := NewClient(filepath.Join(herdrtest.ShortDir(t), "none.sock"))
	w := &Watcher{Client: c, BackoffMin: time.Hour, BackoffMax: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan Snapshot, 4)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, func(s Snapshot) { got <- s }) }()
	if s := <-got; s.Available {
		t.Fatalf("want unavailable first: %+v", s)
	}
	cancel() // must interrupt the hour-long backoff
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run stuck in backoff")
	}
}

func TestWatcherShutdownDuringStalledRead(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	h := startHarness(t, srv, func(w *Watcher) { w.Client.IOTimeout = time.Minute })
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read built", reads(2))
	// Barrier: one full event round trip, so the watcher is in its main loop.
	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "done" })
	srv.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "done"})
	h.next()
	before := srv.State().Calls["session.snapshot"]
	srv.Stall(true)
	srv.Push("pane.created", map[string]any{"pane_id": "w1:p9"})
	srv.Await(t, "refresh is stuck", func(s herdrtest.State) bool { return s.Calls["session.snapshot"] > before })
	if err := h.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestWatcherPublishesFocusChange(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"), herdrtest.NewAgent("w1:p2", "claude", "idle"))
	srv.SetFocused("w1:p1")
	h := startHarness(t, srv, nil)
	if s := h.next(); s.FocusedPaneID != "w1:p1" {
		t.Fatalf("focused = %q", s.FocusedPaneID)
	}
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read built", reads(2))
	srv.SetFocused("w1:p2")
	srv.Push("pane.focused", map[string]any{"pane_id": "w1:p2"})
	if s := h.next(); s.FocusedPaneID != "w1:p2" || !s.Agents[1].Focused {
		t.Fatalf("focus change not published: %+v", s)
	}
}

// T2: the first subscribe request names every global event type. The list is
// typed out here on purpose, so dropping a type from the code fails the test.
func TestWatcherSubscribesToEveryGlobalEvent(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	req := srv.State().SubRequests[0]
	got := map[string]bool{}
	for _, e := range req {
		typ, _ := e["type"].(string)
		got[typ] = true
		if typ != "pane.agent_status_changed" {
			if _, has := e["pane_id"]; has {
				t.Errorf("global subscription %s carries a pane_id", typ)
			}
		}
	}
	for _, want := range []string{
		"pane.created", "pane.closed", "pane.exited", "pane.moved", "pane.agent_detected",
		"pane.focused", "tab.focused", "workspace.focused", "pane.updated",
	} {
		if !got[want] {
			t.Errorf("first subscribe request lacks %s: %v", want, req)
		}
	}
}

// T5: a resubscribe opens the new connection before it closes the old one,
// so there is never a gap with no stream.
func TestWatcherResubscribeOpensNewBeforeClosingOld(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"))
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read built", reads(2))
	if got := srv.State().PeakSubs; got != 1 {
		t.Fatalf("PeakSubs before any change = %d, want 1", got)
	}

	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "idle"), herdrtest.NewAgent("w1:p2", "codex", "idle"))
	srv.Push("pane.created", map[string]any{"pane_id": "w1:p2"})
	h.next()
	srv.Await(t, "second subscription open, first closed", func(s herdrtest.State) bool {
		return len(s.SubRequests) == 2 && s.Subs == 1
	})
	if got := srv.State().PeakSubs; got != 2 {
		t.Fatalf("PeakSubs = %d, want 2 (new stream open before the old one closes)", got)
	}
}

// T7: an event that arrives while a snapshot read is in flight makes a second
// read. The first read is stale, so only that second read can show the change.
// The poll is off, so it cannot be the poll that finds it.
func TestWatcherEventDuringInFlightReadTriggersSecondRead(t *testing.T) {
	srv := herdrtest.New(t)
	srv.SetAgents(herdrtest.NewAgent("w1:p1", "claude", "working"))
	h := startHarness(t, srv, nil)
	h.next()
	srv.Await(t, "subscribed", subscribed(1))
	srv.Await(t, "reconcile read built", reads(2))

	release := srv.HoldSnapshots()
	srv.Push("pane.created", map[string]any{"pane_id": "w1:p9"}) // event 1: starts a read
	srv.Await(t, "held read built", reads(3))                    // its content is now fixed (stale)
	srv.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "blocked" })
	srv.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "blocked"}) // event 2: during the read
	// Event 2 must reach the watcher while the read is held.
	h.expectNoPublish(func() { time.Sleep(100 * time.Millisecond) })
	release()

	if s := h.next(); s.Agents[0].Status != StatusBlocked {
		t.Fatalf("status = %s", s.Agents[0].Status)
	}
	if got := srv.State().Served["session.snapshot"]; got != 4 {
		t.Fatalf("snapshot reads = %d, want 4 (initial, reconcile, held, one more)", got)
	}
}
