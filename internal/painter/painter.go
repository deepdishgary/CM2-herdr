// Package painter turns the session view into pad lights.
// See docs/plans/2026-10-23-cm2d-daemon.md section 3.4.
package painter

import (
	"context"
	"sync"
	"time"

	"cm2-claude/internal/session"
)

// Thread ids on the pad. Thread N lights the key bound to AG<N>.
const (
	threadSlots    = 10 // threads 0..9: session keys
	threadApprove  = 10 // threads 10 and 11: the two halves of APPROVE
	threadApprove2 = 11
	threadDeny     = 12
	threadCount    = 13
)

// Effects of v.oai.thstatus.
const (
	effectOff       = 0
	effectSolid     = 1
	effectBreathing = 4
)

// Palette.
const (
	colorWorking = 0x0050FF
	colorNeeds   = 0xFFA000
	colorDone    = 0x00C853
	colorError   = 0xFF2020
	colorIdle    = 0xFFFFFF
)

const (
	brightSelected = 1.0
	brightOther    = 0.45
	brightIdleSel  = 0.25
	brightIdleOth  = 0.08
	brightDeny     = 0.6
	breathSpeed    = 1.0 // breathing stays dark without a speed
)

const (
	defaultFlash     = 600 * time.Millisecond
	defaultReconcile = 500 * time.Millisecond
)

// Lights sends thread states to the pad. It wraps v.oai.thstatus.
type Lights interface {
	SetThreads(ctx context.Context, threads []Thread) error
}

// Thread is the state of one light thread.
type Thread struct {
	ID         int
	Color      int     // 0xRRGGBB
	Brightness float64 // 0..1
	Effect     int     // 0 off, 1 solid, 4 breathing
	Speed      float64 // needed for breathing
}

// FlashKind is a short one-shot light effect.
type FlashKind int

const (
	FlashNone FlashKind = iota
	// FlashApproveRefused blinks the APPROVE key red for about 600 ms.
	FlashApproveRefused
)

// View is what the painter shows.
type View struct {
	Slots     [10]SlotView
	Selected  int  // slot index, or -1
	ApproveOn bool // the selected session has a Pending (not a question)
	// Question: the selected session shows an AskUserQuestion. It lights
	// DENY, as ApproveOn does (the spec lists it for thread 12 but the
	// View in the spec has no field for it).
	Question bool
	// Flash starts a flash when it changes from FlashNone. The painter
	// times the flash itself.
	Flash FlashKind
}

// SlotView is one session key.
type SlotView struct {
	Present bool
	State   session.State
}

// Painter keeps the last thing sent and sends only differences.
// It is safe for concurrent use, but one run loop should own it.
type Painter struct {
	l   Lights
	now func() time.Time

	mu         sync.Mutex
	view       View
	prevFlash  FlashKind
	flashUntil time.Time
	sent       [threadCount]Thread
	known      [threadCount]bool

	flashDur       time.Duration
	reconcileDelay time.Duration
	recTimer       *time.Timer
	flashTimer     *time.Timer
	rec            chan time.Time
}

// New returns a Painter. now may be nil (time.Now).
func New(l Lights, now func() time.Time) *Painter {
	if now == nil {
		now = time.Now
	}
	p := &Painter{
		l:              l,
		now:            now,
		view:           View{Selected: -1},
		flashDur:       defaultFlash,
		reconcileDelay: defaultReconcile,
		rec:            make(chan time.Time, 1),
	}
	p.recTimer = time.AfterFunc(time.Hour, p.fire)
	p.recTimer.Stop()
	p.flashTimer = time.AfterFunc(time.Hour, p.fire)
	p.flashTimer.Stop()
	return p
}

// fire signals the reconcile channel. It never blocks.
func (p *Painter) fire() {
	select {
	case p.rec <- time.Now():
	default:
	}
}

// Reconcile fires about 500 ms after the last change burst, and when a flash
// ends. The caller answers with Repaint.
func (p *Painter) Reconcile() <-chan time.Time { return p.rec }

// Update stores v and sends the threads that differ from what was sent.
// A thread never sent counts as different.
func (p *Painter) Update(ctx context.Context, v View) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if v.Flash == FlashApproveRefused && p.prevFlash != FlashApproveRefused {
		p.flashUntil = now.Add(p.flashDur)
		p.flashTimer.Reset(p.flashDur)
	}
	p.prevFlash = v.Flash
	p.view = v
	want := p.desired(now)
	var diff []Thread
	for i, t := range want {
		if !p.known[i] || p.sent[i] != t {
			diff = append(diff, t)
		}
	}
	if len(diff) == 0 {
		return nil
	}
	p.recTimer.Reset(p.reconcileDelay) // also after a failure, so it is retried
	if err := p.l.SetThreads(ctx, diff); err != nil {
		return err
	}
	for _, t := range diff {
		p.sent[t.ID] = t
		p.known[t.ID] = true
	}
	return nil
}

// Repaint sends all 13 threads for the last view. It does not start the
// reconcile timer, so a Reconcile -> Repaint loop does not repeat.
func (p *Painter) Repaint(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sendAll(ctx, p.desired(p.now()))
}

// Off switches every thread off and forgets the view and any flash. Later
// Update calls paint again.
func (p *Painter) Off(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recTimer.Stop()
	p.flashTimer.Stop()
	p.view = View{Selected: -1}
	p.prevFlash = FlashNone
	p.flashUntil = time.Time{}
	return p.sendAll(ctx, p.desired(p.now()))
}

// Stop stops the timers. Call it when the painter is no longer used.
func (p *Painter) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recTimer.Stop()
	p.flashTimer.Stop()
}

func (p *Painter) sendAll(ctx context.Context, want [threadCount]Thread) error {
	if err := p.l.SetThreads(ctx, want[:]); err != nil {
		return err
	}
	for _, t := range want {
		p.sent[t.ID] = t
		p.known[t.ID] = true
	}
	return nil
}

func off(id int) Thread { return Thread{ID: id} }

// desired computes all 13 threads for the stored view at time now.
func (p *Painter) desired(now time.Time) [threadCount]Thread {
	v := p.view
	var out [threadCount]Thread
	for i := 0; i < threadSlots; i++ {
		out[i] = slotThread(i, v.Slots[i], i == v.Selected)
	}
	out[threadApprove] = off(threadApprove)
	out[threadApprove2] = off(threadApprove2)
	switch {
	case now.Before(p.flashUntil):
		for _, id := range []int{threadApprove, threadApprove2} {
			out[id] = Thread{ID: id, Color: colorError, Brightness: brightSelected, Effect: effectSolid}
		}
	case v.ApproveOn:
		for _, id := range []int{threadApprove, threadApprove2} {
			out[id] = Thread{ID: id, Color: colorDone, Brightness: brightSelected, Effect: effectBreathing, Speed: breathSpeed}
		}
	}
	out[threadDeny] = off(threadDeny)
	if v.ApproveOn || v.Question {
		out[threadDeny] = Thread{ID: threadDeny, Color: colorError, Brightness: brightDeny, Effect: effectSolid}
	}
	return out
}

func slotThread(id int, s SlotView, selected bool) Thread {
	if !s.Present {
		return off(id)
	}
	t := Thread{ID: id, Effect: effectSolid, Brightness: brightOther}
	if selected {
		t.Brightness = brightSelected
	}
	switch s.State {
	case session.Working:
		t.Color = colorWorking
	case session.NeedsInput:
		t.Color = colorNeeds
		t.Effect = effectBreathing
		t.Speed = breathSpeed
	case session.Done:
		t.Color = colorDone
	case session.Error:
		t.Color = colorError
	default: // Idle
		t.Color = colorIdle
		t.Brightness = brightIdleOth
		if selected {
			t.Brightness = brightIdleSel
		}
	}
	return t
}
