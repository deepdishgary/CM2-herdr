package painter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"cm2-claude/internal/session"
)

type fakeLights struct {
	mu    sync.Mutex
	calls [][]Thread
	err   error
}

func (f *fakeLights) SetThreads(_ context.Context, th []Thread) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, append([]Thread(nil), th...))
	return nil
}

func (f *fakeLights) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLights) last() []Thread {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTest() (*Painter, *fakeLights, *clock) {
	l := &fakeLights{}
	c := &clock{t: time.Unix(1000, 0)}
	p := New(l, c.now)
	return p, l, c
}

func byID(th []Thread) map[int]Thread {
	m := map[int]Thread{}
	for _, t := range th {
		m[t.ID] = t
	}
	return m
}

func view(sel int, states ...session.State) View {
	v := View{Selected: sel}
	for i, s := range states {
		v.Slots[i] = SlotView{Present: true, State: s}
	}
	return v
}

func TestPalette(t *testing.T) {
	p, l, _ := newTest()
	v := view(-1, session.Working, session.NeedsInput, session.Done, session.Error, session.Idle)
	if err := p.Update(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	m := byID(l.last())
	want := []Thread{
		{ID: 0, Color: 0x0050FF, Brightness: 0.45, Effect: 1},
		{ID: 1, Color: 0xFFA000, Brightness: 0.45, Effect: 4, Speed: 1},
		{ID: 2, Color: 0x00C853, Brightness: 0.45, Effect: 1},
		{ID: 3, Color: 0xFF2020, Brightness: 0.45, Effect: 1},
		{ID: 4, Color: 0xFFFFFF, Brightness: 0.08, Effect: 1},
	}
	for _, w := range want {
		if m[w.ID] != w {
			t.Errorf("thread %d = %+v, want %+v", w.ID, m[w.ID], w)
		}
	}
}

func TestEmptySlotOff(t *testing.T) {
	p, l, _ := newTest()
	if err := p.Update(context.Background(), view(0, session.Working)); err != nil {
		t.Fatal(err)
	}
	m := byID(l.last())
	for i := 1; i < 10; i++ {
		if m[i] != (Thread{ID: i}) {
			t.Errorf("empty slot %d = %+v", i, m[i])
		}
	}
}

func TestSelectedBrightness(t *testing.T) {
	p, l, _ := newTest()
	v := view(1, session.Working, session.Working, session.Idle, session.Idle)
	if err := p.Update(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	m := byID(l.last())
	if m[1].Brightness != 1.0 || m[0].Brightness != 0.45 {
		t.Errorf("working: selected %v other %v", m[1].Brightness, m[0].Brightness)
	}
	if m[2].Brightness != 0.08 {
		t.Errorf("idle other = %v", m[2].Brightness)
	}
	// Select the idle one.
	v.Selected = 2
	if err := p.Update(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	m = byID(l.last())
	if m[2].Brightness != 0.25 {
		t.Errorf("idle selected = %v", m[2].Brightness)
	}
}

func TestApproveAndDeny(t *testing.T) {
	p, l, _ := newTest()
	v := view(0, session.NeedsInput)
	v.ApproveOn = true
	if err := p.Update(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	m := byID(l.last())
	for _, id := range []int{10, 11} {
		w := Thread{ID: id, Color: 0x00C853, Brightness: 1, Effect: 4, Speed: 1}
		if m[id] != w {
			t.Errorf("approve %d = %+v", id, m[id])
		}
	}
	if m[12] != (Thread{ID: 12, Color: 0xFF2020, Brightness: 0.6, Effect: 1}) {
		t.Errorf("deny = %+v", m[12])
	}
	// ApproveOn off: approve and deny go dark.
	v.ApproveOn = false
	if err := p.Update(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	m = byID(l.last())
	for _, id := range []int{10, 11, 12} {
		if m[id] != (Thread{ID: id}) {
			t.Errorf("thread %d should be off: %+v", id, m[id])
		}
	}
}

func TestDenyOnForQuestion(t *testing.T) {
	p, l, _ := newTest()
	v := view(0, session.NeedsInput)
	v.Question = true
	if err := p.Update(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	m := byID(l.last())
	if m[12].Effect != 1 || m[12].Color != 0xFF2020 {
		t.Errorf("deny = %+v", m[12])
	}
	if m[10] != (Thread{ID: 10}) || m[11] != (Thread{ID: 11}) {
		t.Errorf("approve should stay off for a question: %+v %+v", m[10], m[11])
	}
}

func TestUpdateSendsOnlyChanges(t *testing.T) {
	p, l, _ := newTest()
	ctx := context.Background()
	v := view(0, session.Working, session.Working)
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	if got := len(l.last()); got != 13 {
		t.Fatalf("first update sent %d threads, want 13", got)
	}
	n := l.n()
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	if l.n() != n {
		t.Fatal("identical view sent something")
	}
	v.Slots[1].State = session.Done
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	got := l.last()
	if len(got) != 1 || got[0].ID != 1 || got[0].Color != 0x00C853 {
		t.Fatalf("diff = %+v", got)
	}
	// A move changes two threads.
	v.Selected = 1
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	got = l.last()
	if len(got) != 2 || got[0].ID != 0 || got[1].ID != 1 {
		t.Fatalf("move diff = %+v", got)
	}
}

func TestFailedSendIsRetried(t *testing.T) {
	p, l, _ := newTest()
	ctx := context.Background()
	v := view(0, session.Working)
	l.err = errors.New("pad gone")
	if err := p.Update(ctx, v); err == nil {
		t.Fatal("want error")
	}
	l.err = nil
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	if l.n() != 1 || len(l.last()) != 13 {
		t.Fatalf("retry did not resend all: %d calls", l.n())
	}
}

func TestRepaintSendsAllAndResetsCache(t *testing.T) {
	p, l, _ := newTest()
	ctx := context.Background()
	v := view(0, session.Working)
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := p.Repaint(ctx); err != nil {
		t.Fatal(err)
	}
	got := l.last()
	if len(got) != 13 {
		t.Fatalf("repaint sent %d", len(got))
	}
	for i, th := range got {
		if th.ID != i {
			t.Fatalf("order: %d at %d", th.ID, i)
		}
	}
	if byID(got)[0].Color != 0x0050FF {
		t.Error("repaint lost the view")
	}
	n := l.n()
	if err := p.Update(ctx, v); err != nil || l.n() != n {
		t.Fatal("update after repaint should be a no-op")
	}
}

func TestRepaintBeforeAnyViewIsAllOff(t *testing.T) {
	p, l, _ := newTest()
	if err := p.Repaint(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, th := range l.last() {
		if th != (Thread{ID: th.ID}) {
			t.Errorf("thread %d not off: %+v", th.ID, th)
		}
	}
}

func TestFlash(t *testing.T) {
	p, l, c := newTest()
	ctx := context.Background()
	v := view(0, session.Working)
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	v.Flash = FlashApproveRefused
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	got := l.last()
	if len(got) != 2 {
		t.Fatalf("flash diff = %+v", got)
	}
	for _, th := range got {
		if (th.ID != 10 && th.ID != 11) || th.Color != 0xFF2020 || th.Effect != 1 {
			t.Errorf("flash thread %+v", th)
		}
	}
	// The flash ends on its own, even if the loop clears Flash at once.
	v.Flash = FlashNone
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(500 * time.Millisecond)
	n := l.n()
	if err := p.Repaint(ctx); err != nil {
		t.Fatal(err)
	}
	if byID(l.last())[10].Color != 0xFF2020 {
		t.Error("flash ended early")
	}
	c.t = c.t.Add(101 * time.Millisecond)
	if err := p.Repaint(ctx); err != nil {
		t.Fatal(err)
	}
	if l.n() != n+2 {
		t.Fatalf("calls %d", l.n())
	}
	if byID(l.last())[10] != (Thread{ID: 10}) {
		t.Errorf("approve after flash = %+v", byID(l.last())[10])
	}
}

func TestFlashDoesNotRetriggerWhileSet(t *testing.T) {
	p, l, c := newTest()
	ctx := context.Background()
	v := view(0, session.Working)
	v.Flash = FlashApproveRefused
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Second)
	// The same flag again is not a new flash; the flash is over.
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	if byID(l.last())[10].Color == 0xFF2020 {
		t.Error("flash restarted")
	}
	// A new edge starts one.
	v.Flash = FlashNone
	_ = p.Update(ctx, v)
	v.Flash = FlashApproveRefused
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	if byID(l.last())[10].Color != 0xFF2020 {
		t.Error("second flash did not start")
	}
}

func TestReconcileFiresAfterBurst(t *testing.T) {
	p, _, _ := newTest()
	defer p.Stop()
	p.reconcileDelay = 60 * time.Millisecond
	ctx := context.Background()
	v := view(0, session.Working)
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	v.Slots[0].State = session.Done // second change restarts the delay
	if err := p.Update(ctx, v); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	select {
	case <-p.Reconcile():
	case <-time.After(2 * time.Second):
		t.Fatal("no reconcile")
	}
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Errorf("fired %v after the last change, too early", d)
	}
	select {
	case <-p.Reconcile():
		t.Fatal("fired twice")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestNoReconcileForNoChange(t *testing.T) {
	p, _, _ := newTest()
	defer p.Stop()
	p.reconcileDelay = 30 * time.Millisecond
	ctx := context.Background()
	v := view(0, session.Working)
	_ = p.Update(ctx, v)
	<-p.Reconcile()
	_ = p.Update(ctx, v) // no diff
	select {
	case <-p.Reconcile():
		t.Fatal("reconcile after a no-op update")
	case <-time.After(120 * time.Millisecond):
	}
}

func TestRepaintDoesNotArmReconcile(t *testing.T) {
	p, _, _ := newTest()
	defer p.Stop()
	p.reconcileDelay = 30 * time.Millisecond
	if err := p.Repaint(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Reconcile():
		t.Fatal("repaint armed the reconcile timer")
	case <-time.After(120 * time.Millisecond):
	}
}

func TestReconcileFiresWhenFlashEnds(t *testing.T) {
	l := &fakeLights{}
	p := New(l, nil) // real clock
	defer p.Stop()
	p.flashDur = 40 * time.Millisecond
	p.reconcileDelay = time.Hour
	v := view(0, session.Working)
	v.Flash = FlashApproveRefused
	if err := p.Update(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Reconcile():
	case <-time.After(2 * time.Second):
		t.Fatal("no signal at flash end")
	}
	if err := p.Repaint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if byID(l.last())[10] != (Thread{ID: 10}) {
		t.Errorf("approve still lit after flash: %+v", byID(l.last())[10])
	}
}

func TestOff(t *testing.T) {
	p, l, _ := newTest()
	defer p.Stop()
	ctx := context.Background()
	v := view(0, session.Working, session.Done)
	v.ApproveOn = true
	_ = p.Update(ctx, v)
	if err := p.Off(ctx); err != nil {
		t.Fatal(err)
	}
	got := l.last()
	if len(got) != 13 {
		t.Fatalf("off sent %d", len(got))
	}
	for _, th := range got {
		if th != (Thread{ID: th.ID}) {
			t.Errorf("thread %d not off: %+v", th.ID, th)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	p, _, _ := newTest()
	defer p.Stop()
	var wg sync.WaitGroup
	ctx := context.Background()
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				v := view(i%2, session.Working, session.Done)
				_ = p.Update(ctx, v)
				_ = p.Repaint(ctx)
			}
		}(g)
	}
	wg.Wait()
}

// The spec numbers: reconcile about 500 ms after a change, flash 600 ms.
func TestSpecTimings(t *testing.T) {
	if defaultReconcile != 500*time.Millisecond {
		t.Errorf("defaultReconcile = %v, want 500ms", defaultReconcile)
	}
	if defaultFlash != 600*time.Millisecond {
		t.Errorf("defaultFlash = %v, want 600ms", defaultFlash)
	}
	p := New(&fakeLights{}, nil)
	defer p.Stop()
	if p.reconcileDelay != defaultReconcile || p.flashDur != defaultFlash {
		t.Errorf("New uses %v and %v", p.reconcileDelay, p.flashDur)
	}
}
