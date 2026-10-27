// Package daemon is the cm2d run loop. It joins the hooks server, the Herdr
// watcher, the pad and the session table. See
// docs/plans/2026-10-23-cm2d-daemon.md section 3.7.
package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"cm2-claude/internal/device"
	"cm2-claude/internal/focus"
	"cm2-claude/internal/herdr"
	"cm2-claude/internal/hooks"
	"cm2-claude/internal/keys"
	"cm2-claude/internal/painter"
	"cm2-claude/internal/session"
)

// DefaultAddr is where the hooks server listens.
const DefaultAddr = "127.0.0.1:47731"

// Config sets up a Daemon. Zero values take the defaults.
type Config struct {
	Addr      string                           // hooks server address; default DefaultAddr
	NoDevice  bool                             // never open the pad
	OpenPad   func() (device.Transport, error) // default device.Open
	Herdr     *herdr.Client                    // nil: do not watch Herdr
	HerdrTune func(*herdr.Watcher)             // tests only
	Focuser   *focus.Focuser                   // nil: a key press selects but focuses nothing
	Table     session.Config
	Logger    *slog.Logger
	Version   string

	MaxHold      time.Duration    // held request limit; default 590 s
	RetryEvery   time.Duration    // pad reopen delay; default 3 s
	ForeignPause time.Duration    // painting pause after a foreign reply; default 30 s
	TickEvery    time.Duration    // table tick; default 1 s
	CallTimeout  time.Duration    // one painter call; default 2 s
	Now          func() time.Time // the clock; default time.Now (tests only)
}

func (c *Config) defaults() {
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if c.OpenPad == nil {
		c.OpenPad = device.Open
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.RetryEvery <= 0 {
		c.RetryEvery = 3 * time.Second
	}
	if c.ForeignPause <= 0 {
		c.ForeignPause = 30 * time.Second
	}
	if c.TickEvery <= 0 {
		c.TickEvery = time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 2 * time.Second
	}
	c.Table.Slots = 10
}

type focusResult struct {
	key string
	err error
}

// Daemon is one cm2d. The run loop goroutine owns the table and the painter.
type Daemon struct {
	cfg    Config
	log    *slog.Logger
	table  *session.Table
	paint  *painter.Painter
	lights *padLights
	pad    *pad
	server *hooks.Server

	events    chan session.HookEvent
	herdrCh   chan herdr.Snapshot
	padKeys   chan padKey
	repaint   chan struct{}
	padGone   chan struct{}
	focusRes  chan focusResult
	statusQ   chan chan StatusDoc
	ready     chan struct{}
	loopDone  chan struct{}
	srvDone   chan struct{}
	srvErr    error
	bg        sync.WaitGroup // herdr watcher and focus goroutines
	padWG     sync.WaitGroup // the pad supervisor
	paintDone chan struct{}  // closed when the paint goroutine ends

	// The run loop posts views; the paint goroutine takes the latest one.
	// The loop never waits for the pad.
	box  viewBox
	wake chan struct{}

	// Touched only by the run loop.
	dec     keys.Decoder
	flash   painter.FlashKind
	herdr   HerdrStatus
	started time.Time

	// Touched only by the paint goroutine.
	needRepaint bool
	wasPaused   bool
	failStreak  int

	paintErr onceLog
	herdrErr onceLog
}

// viewBox holds the newest view for the paint goroutine. A flash is a
// one-shot, so it is kept until a paint takes it, even if newer views
// replace the one that carried it.
type viewBox struct {
	mu    sync.Mutex
	view  painter.View
	flash painter.FlashKind
}

func (b *viewBox) post(v painter.View) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v.Flash != painter.FlashNone {
		b.flash = v.Flash
	}
	v.Flash = painter.FlashNone
	b.view = v
}

func (b *viewBox) take() painter.View {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.view
	v.Flash, b.flash = b.flash, painter.FlashNone
	return v
}

// failLimit is how many painter calls in a row may fail before the pad is
// closed and opened again.
const failLimit = 3

// New returns a Daemon. Call Run to start it.
func New(cfg Config) *Daemon {
	cfg.defaults()
	d := &Daemon{
		cfg:      cfg,
		log:      cfg.Logger,
		table:    session.New(cfg.Table),
		lights:   &padLights{},
		events:   make(chan session.HookEvent, 256),
		herdrCh:  make(chan herdr.Snapshot, 16),
		padKeys:  make(chan padKey, 256),
		repaint:  make(chan struct{}, 1),
		padGone:  make(chan struct{}, 1),
		focusRes: make(chan focusResult, 16),
		statusQ:  make(chan chan StatusDoc),
		ready:    make(chan struct{}),
		loopDone: make(chan struct{}),
		srvDone:  make(chan struct{}),

		paintDone: make(chan struct{}),
		wake:      make(chan struct{}, 1),
	}
	d.box.view = painter.View{Selected: -1}
	d.paint = painter.New(d.lights, nil)
	d.pad = &pad{
		open: cfg.OpenPad, log: cfg.Logger, lights: d.lights,
		retry: cfg.RetryEvery, pause: cfg.ForeignPause,
		keys: d.padKeys, repaint: d.repaint,
		onGone: d.onPadGone,
	}
	d.server = &hooks.Server{
		Addr:    cfg.Addr,
		Events:  d.events,
		CanHold: d.Connected,
		MaxHold: cfg.MaxHold,
		Status:  d.statusAny,
		Logger:  cfg.Logger,
	}
	return d
}

// Connected reports whether the pad is open. The hooks server holds
// permission requests only while it is.
func (d *Daemon) Connected() bool { return d.pad.connected.Load() }

// Ready is closed once the hooks port is bound.
func (d *Daemon) Ready() <-chan struct{} { return d.ready }

// Addr is the bound hooks address, or "" before Ready.
func (d *Daemon) Addr() string { return d.server.BoundAddr() }

func (d *Daemon) now() time.Time { return d.cfg.Now() }

// Run runs the daemon until ctx ends. Binding the hooks port is the
// single-instance lock: if it fails, Run returns the error at once and has
// not touched the pad. On shutdown Run releases every held request, turns
// the lights off and closes the pad.
func (d *Daemon) Run(ctx context.Context) error {
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		d.srvErr = d.server.ListenAndServe(runCtx)
		close(d.srvDone)
	}()
	select {
	case <-d.server.Ready():
	case <-d.srvDone:
		return d.srvErr
	}
	d.started = d.now()
	close(d.ready)
	d.log.Info("listening for hooks", "addr", d.server.BoundAddr())

	// The paint goroutine talks to the pad, so a slow pad never holds up
	// the run loop.
	paintCtx, paintCancel := context.WithCancel(context.Background())
	defer paintCancel()
	go d.paintLoop(paintCtx)

	padCtx, padCancel := context.WithCancel(context.Background())
	defer padCancel()
	if !d.cfg.NoDevice {
		d.padWG.Add(1)
		go func() { defer d.padWG.Done(); d.pad.run(padCtx) }()
	}
	if d.cfg.Herdr != nil {
		d.herdr.Watching = true
		d.bg.Add(1)
		go func() { defer d.bg.Done(); d.watchHerdr(runCtx) }()
	}

	err := d.loop(runCtx)

	// Stop. The server releases every held request when its context ends.
	runCancel()
	<-d.srvDone
	if err == nil && d.srvErr != nil && ctx.Err() == nil {
		err = d.srvErr
	}
	d.bg.Wait() // herdr watcher and focus goroutines; the pad is still open
	paintCancel()
	<-d.paintDone
	d.lightsOff()
	padCancel()
	d.padWG.Wait()
	d.log.Info("stopped")
	return err
}

// lightsOff turns every thread off. It uses its own deadline because the
// run context has ended.
func (d *Daemon) lightsOff() {
	if !d.Connected() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.CallTimeout)
	defer cancel()
	if err := d.paint.Off(ctx); err != nil {
		d.log.Warn("cannot turn the lights off", "err", err)
	}
	d.paint.Stop()
}

// watchHerdr feeds Herdr snapshots to the run loop.
func (d *Daemon) watchHerdr(ctx context.Context) {
	w := &herdr.Watcher{
		Client:  d.cfg.Herdr,
		OnError: func(err error) { d.herdrErr.warn(d.log, "herdr error", err) },
	}
	if d.cfg.HerdrTune != nil {
		d.cfg.HerdrTune(w)
	}
	w.Run(ctx, func(s herdr.Snapshot) {
		select {
		case d.herdrCh <- s:
		case <-ctx.Done():
		}
	})
}

// loop is the run loop. It returns when ctx ends or the hooks server stops.
func (d *Daemon) loop(ctx context.Context) error {
	defer close(d.loopDone)
	tick := time.NewTicker(d.cfg.TickEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.srvDone:
			return d.srvErr
		case ev := <-d.events:
			d.onHook(ev)
		case snap := <-d.herdrCh:
			d.onHerdr(snap)
		case k := <-d.padKeys:
			d.onKey(ctx, k)
		case <-d.padGone:
			d.onPadGoneLoop()
		case r := <-d.focusRes:
			d.onFocusResult(r)
		case <-tick.C:
			d.table.Tick(d.now())
		case q := <-d.statusQ:
			d.drainEvents() // so a status call sees every event answered before it
			q <- d.status()
		}
		d.post()
	}
}

// drainEvents applies the hook events that are already queued.
func (d *Daemon) drainEvents() {
	for {
		select {
		case ev := <-d.events:
			d.onHook(ev)
		default:
			return
		}
	}
}

// onHook applies one hook event.
func (d *Daemon) onHook(ev session.HookEvent) {
	if ev.At.IsZero() {
		ev.At = d.now()
	}
	d.table.ApplyHook(ev)
	switch ev.Name {
	case "PostToolUse", "Stop", "StopFailure", "UserPromptSubmit", "SessionEnd":
		// The turn moved on, so a held request was answered elsewhere.
		d.server.ReleaseSession(ev.SessionID)
	case "PermissionRequest":
		d.checkRequest(ev)
	}
}

// checkRequest keeps a Pending only for a request that the hooks server
// holds and that a key could answer. A request the server did not hold (the
// pad was away, or it was already released) is dropped from the table, so
// the keys do not light for something nobody waits on. A held request of a
// session with no slot is released: holding it would only stall a headless
// run.
func (d *Daemon) checkRequest(ev session.HookEvent) {
	if ev.IsQuestion || ev.RequestID == "" {
		return
	}
	if !d.server.Holds(ev.RequestID) {
		d.dropPending(ev.SessionID, ev.RequestID)
		return
	}
	if s := d.table.Get("claude:" + ev.SessionID); s != nil && s.Slot >= 0 && hasPending(s, ev.RequestID) {
		return
	}
	if !d.server.Resolve(ev.RequestID, hooks.Pass) {
		d.dropPending(ev.SessionID, ev.RequestID)
	}
}

func hasPending(s *session.Session, id string) bool {
	for _, p := range s.Pending {
		if p.RequestID == id {
			return true
		}
	}
	return false
}

// dropPending removes a request from the table.
func (d *Daemon) dropPending(sessionID, requestID string) {
	d.table.ApplyHook(session.HookEvent{
		Name: "PermissionReleased", SessionID: sessionID, RequestID: requestID, At: d.now(),
	})
}

func (d *Daemon) onHerdr(s herdr.Snapshot) {
	if s.Available != d.herdr.Available {
		d.log.Info("herdr link changed", "available", s.Available, "version", s.Version)
	}
	d.herdr.Available = s.Available
	d.herdr.Version = s.Version
	d.herdr.Agents = len(s.Agents)
	d.table.ApplyHerdr(s, d.now())
}

// onKey runs the action for one pad notification.
func (d *Daemon) onKey(ctx context.Context, k padKey) {
	a, ok := d.dec.Decode(k.n, k.at)
	if !ok {
		return
	}
	switch a.Kind {
	case keys.SelectFocus:
		d.selectFocus(ctx, a.Slot)
	case keys.Move:
		d.table.MoveSelection(a.Delta)
	case keys.Approve:
		d.answer(hooks.Allow)
	case keys.Deny:
		d.answer(hooks.Deny)
	}
}

// selectFocus selects the session on a key and brings its window forward.
func (d *Daemon) selectFocus(ctx context.Context, slot int) {
	slots := d.table.Slots()
	if slot < 0 || slot >= len(slots) || slots[slot] == nil {
		return
	}
	d.table.Select(slot)
	s := slots[slot]
	if d.cfg.Focuser == nil {
		d.table.Focused(s.Key, d.now())
		return
	}
	cp := *s // the focus goroutine must not share the live row
	cp.Pending = nil
	d.bg.Add(1)
	go func() {
		defer d.bg.Done()
		err := d.cfg.Focuser.Focus(ctx, &cp)
		select {
		case d.focusRes <- focusResult{key: cp.Key, err: err}:
		case <-ctx.Done():
		}
	}()
}

func (d *Daemon) onFocusResult(r focusResult) {
	if r.err != nil {
		d.log.Warn("focus failed", "session", r.key, "err", r.err)
		return
	}
	d.table.Focused(r.key, d.now())
}

// answer handles APPROVE (Allow) and DENY (Deny). It acts only on the
// selected session, and only on a request the hooks server still holds.
func (d *Daemon) answer(dec hooks.Decision) {
	var s *session.Session
	if sel := d.table.Selected(); sel >= 0 {
		s = d.table.Slots()[sel]
	}
	if s == nil || len(s.Pending) == 0 {
		switch {
		case dec == hooks.Allow:
			d.flash = painter.FlashApproveRefused
		case s != nil && s.Question:
			d.log.Info("deny on a question does nothing in v1", "session", s.Key)
		}
		return
	}
	// Answer the oldest request the server still holds. One it does not
	// hold (it came while the pad was away, or was released) is dropped.
	answered := false
	for _, p := range append([]session.Pending(nil), s.Pending...) {
		if d.server.Resolve(p.RequestID, dec) {
			d.log.Info("answered from the pad", "session", s.Key, "tool", p.Tool, "allow", dec == hooks.Allow)
			d.dropPending(s.SessionID, p.RequestID)
			answered = true
			break
		}
		d.log.Info("request was not held", "session", s.Key, "request", p.RequestID)
		d.dropPending(s.SessionID, p.RequestID)
	}
	if !answered && dec == hooks.Allow {
		d.flash = painter.FlashApproveRefused
	}
}

// post builds the current view and hands it to the paint goroutine. It
// never waits for the pad.
func (d *Daemon) post() {
	v := d.buildView(d.now())
	v.Flash = d.flash
	d.flash = painter.FlashNone
	d.box.post(v)
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// onPadGone runs on the pad supervisor when the pad goes away. Nothing can
// answer a held request now, so release them all.
func (d *Daemon) onPadGone() {
	d.server.ReleaseAll()
	select {
	case d.padGone <- struct{}{}:
	default:
	}
}

// onPadGoneLoop clears every Pending in the table. ReleaseAll already sent
// PermissionReleased for each held request, but a full event queue can drop
// those.
func (d *Daemon) onPadGoneLoop() {
	for _, s := range d.table.All() {
		for _, p := range append([]session.Pending(nil), s.Pending...) {
			d.dropPending(s.SessionID, p.RequestID)
		}
	}
}

// paintLoop is the only goroutine that calls the painter, apart from the
// shutdown that follows it.
func (d *Daemon) paintLoop(ctx context.Context) {
	defer close(d.paintDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
			d.refresh(ctx)
		case <-d.repaint:
			d.needRepaint = true
			d.refresh(ctx)
		case <-d.paint.Reconcile():
			d.reconcile(ctx)
		}
	}
}

// reconcile repaints everything, the follow-up to a burst of changes.
func (d *Daemon) reconcile(ctx context.Context) {
	if !d.Connected() || d.pad.paused(d.now()) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CallTimeout)
	defer cancel()
	d.noteResult(ctx, d.paint.Repaint(cctx))
}

// refresh paints the newest view, if the pad is there and free.
func (d *Daemon) refresh(ctx context.Context) {
	now := d.now()
	v := d.box.take()
	if !d.Connected() {
		return
	}
	if d.pad.paused(now) {
		d.wasPaused = true
		return
	}
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CallTimeout)
	defer cancel()
	err := d.paint.Update(cctx, v)
	if err == nil && (d.needRepaint || d.wasPaused) {
		// A new connection or a foreign writer may have left other colors.
		if err = d.paint.Repaint(cctx); err == nil {
			d.needRepaint, d.wasPaused = false, false
		}
	}
	d.noteResult(ctx, err)
}

// noteResult logs a painter error and counts failures in a row. A pad that
// takes writes but does not answer them never ends its connection by itself,
// so after failLimit failures the daemon closes it and the supervisor
// reopens it.
func (d *Daemon) noteResult(ctx context.Context, err error) {
	d.paintErr.warn(d.log, "cannot paint", err)
	var devErr *device.DeviceError
	switch {
	case err == nil:
		d.failStreak = 0
	case ctx.Err() != nil, errors.Is(err, errNoPad):
		// Shutting down, or the pad is already gone.
	case errors.As(err, &devErr):
		// The pad answered with an error, so the link works. A new
		// connection would not change the answer.
	default:
		d.failStreak++
		if d.failStreak >= failLimit {
			d.log.Warn("the pad does not answer; opening it again", "failures", d.failStreak)
			d.failStreak = 0
			d.pad.kick()
		}
	}
}

// buildView turns the table into what the painter shows.
func (d *Daemon) buildView(now time.Time) painter.View {
	v := painter.View{Selected: d.table.Selected()}
	for i, s := range d.table.Slots() {
		if i >= len(v.Slots) || s == nil {
			continue
		}
		v.Slots[i] = painter.SlotView{Present: true, State: d.table.Visible(s, now)}
	}
	if v.Selected >= 0 {
		if s := d.table.Slots()[v.Selected]; s != nil {
			v.ApproveOn = len(s.Pending) > 0
			v.Question = s.Question
		}
	}
	return v
}
