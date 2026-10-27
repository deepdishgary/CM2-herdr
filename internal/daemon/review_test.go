package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cm2-claude/internal/daemon"
	"cm2-claude/internal/device"
	"cm2-claude/internal/device/devicetest"
)

// fullRepaints counts the thstatus calls that carry all 13 threads.
func fullRepaints(f *devicetest.Fake) int {
	n := 0
	for _, c := range thstatusCalls(f) {
		if len(c) == 13 {
			n++
		}
	}
	return n
}

// hangPad makes every pad RPC block until release is closed. It models an
// IOHIDDeviceSetReport that never returns.
type hangPad struct {
	hung    atomic.Bool
	release chan struct{}
	once    sync.Once
}

func newHangPad(t *testing.T) *hangPad {
	p := &hangPad{release: make(chan struct{})}
	t.Cleanup(p.unblock) // runs before the harness stops the daemon
	return p
}

func (p *hangPad) unblock() { p.once.Do(func() { close(p.release) }) }

func (p *hangPad) hook(n int, f *devicetest.Fake) {
	inner := f.OnMessage
	f.OnMessage = func(req []byte) [][]byte {
		if p.hung.Load() {
			<-p.release
			return nil
		}
		return inner(req)
	}
}

// A pad that never answers must not hold up hook handling: the run loop
// does not wait for pad RPCs.
func TestHungPadDoesNotStallTheLoop(t *testing.T) {
	hp := newHangPad(t)
	h := newHarness(t, opts{padHook: hp.hook, cfg: func(c *daemon.Config) { c.CallTimeout = 600 * time.Millisecond }})
	h.work("A", "/w/alpha")
	h.waitLight("A blue", 0, solid(blue))
	hp.hung.Store(true)
	h.event("Stop", "A", "/w/alpha") // a paint is now stuck in the pad

	// A headless request gets its empty 200 at once.
	r := recv(t, h.permission(context.Background(), "X", "/w/x", "Bash", "sdk-cli"))
	if r.code != 200 || r.body != "" || r.took > 300*time.Millisecond {
		t.Fatalf("headless reply = %d %q after %v", r.code, r.body, r.took)
	}
	// A held request is released at once by the terminal answer.
	h.work("B", "/w/beta")
	res := h.permission(context.Background(), "B", "/w/beta", "Bash", "cli")
	h.waitHeld(1)
	h.event("PostToolUse", "B", "/w/beta")
	if r := recv(t, res); r.body != "" || r.took > 500*time.Millisecond {
		t.Fatalf("release reply = %q after %v", r.body, r.took)
	}
	// /status answers with a document, not an error.
	for i := 0; i < 3; i++ {
		start := time.Now()
		if st := h.status(); st.Version != "test" || time.Since(start) > 300*time.Millisecond {
			t.Fatalf("status = %+v after %v", st, time.Since(start))
		}
	}
	// A burst of events is not dropped.
	for i := 0; i < 300; i++ {
		h.event("PostToolUse", "A", "/w/alpha")
	}
	if got := h.status().EventsDropped; got != 0 {
		t.Fatalf("%d events dropped while the pad was hung", got)
	}
	hp.unblock()
}

// A pad that fails every write is closed and opened again.
func TestWriteErrorReopensThePad(t *testing.T) {
	h := newHarness(t, opts{padHook: func(n int, f *devicetest.Fake) {
		if n == 1 {
			f.WriteErr = errors.New("write failed")
		}
	}})
	h.work("A", "/w/alpha")
	eventually(t, "second open", func() bool { return h.padCount() >= 2 && h.d.Connected() })
	h.waitLight("A blue on the new pad", 0, solid(blue))
}

// When the pad goes away nothing can answer a held request, so it is
// released, and the keys do not light up again on reconnect.
func TestPadUnplugReleasesHeldRequests(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.press("AG00")
	res := h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli")
	h.waitHeld(1)
	h.waitLight("approve lit", 10, breathing(green))

	h.setOpenFail(errors.New("pad not connected"))
	h.pad().Close() // unplug
	r := recv(t, res)
	if r.code != 200 || r.body != "" {
		t.Fatalf("reply = %d %q, want an empty 200", r.code, r.body)
	}
	eventually(t, "nothing held or pending", func() bool {
		st := h.status()
		return st.Held == 0 && !st.Pad.Connected && h.sess("claude:A").Pending == 0
	})

	h.setOpenFail(nil)
	h.waitConnected()
	h.press("AG00")
	h.waitLight("A selected", 0, func(l light) bool { return l.B == 1.0 })
	time.Sleep(100 * time.Millisecond)
	if l := lights(h.pad())[10]; !l.off() {
		t.Fatalf("approve lit with nothing to answer: %+v", l)
	}
}

// A request that came while the pad was away was not held. It must not
// light APPROVE after the pad returns.
func TestRequestWhilePadAwayDoesNotLightApprove(t *testing.T) {
	h := newHarness(t, opts{openFail: errors.New("pad not connected")})
	h.work("A", "/w/alpha")
	r := recv(t, h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli"))
	if r.body != "" {
		t.Fatalf("reply = %q", r.body)
	}
	eventually(t, "pending dropped", func() bool {
		s := h.sess("claude:A")
		return s != nil && s.Pending == 0
	})

	h.setOpenFail(nil)
	h.waitConnected()
	h.press("AG00")
	h.waitLight("A selected", 0, func(l light) bool { return l.B == 1.0 })
	time.Sleep(100 * time.Millisecond)
	for _, id := range []int{10, 11, 12} {
		if l := lights(h.pad())[id]; !l.off() {
			t.Fatalf("thread %d lit with nothing held: %+v", id, l)
		}
	}
	// APPROVE has nothing to answer: it flashes red.
	h.press("AG10")
	eventually(t, "red flash", func() bool { return everLit(h.pad(), 10, red) })
}

// Spec 3.7 / section 2: each change burst is followed by one full repaint
// about 500 ms later, because the pad's replies are always {"ok":1}.
func TestReconcileFullRepaintAfterBurst(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.waitLight("A blue", 0, solid(blue))
	time.Sleep(900 * time.Millisecond) // the first burst reconciles
	pad := h.pad()
	before := fullRepaints(pad)

	h.work("B", "/w/beta")
	h.waitLight("B blue", 1, solid(blue))
	time.Sleep(250 * time.Millisecond)
	if got := fullRepaints(pad); got != before {
		t.Fatalf("full repaint after 250 ms, want about 500 ms")
	}
	eventually(t, "full repaint after the burst", func() bool { return fullRepaints(pad) == before+1 })
	time.Sleep(700 * time.Millisecond)
	if got := fullRepaints(pad); got != before+1 {
		t.Fatalf("%d full repaints for one burst", got-before)
	}
}

// A replug with no recent change still repaints: no reconcile timer may be
// needed to light the new pad.
func TestRepaintOnReconnectNotMasked(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.waitLight("A blue", 0, solid(blue))
	time.Sleep(1200 * time.Millisecond) // no reconcile timer is pending now

	h.pad().Close()
	eventually(t, "reopened", func() bool { return h.padCount() == 2 && h.d.Connected() })
	second := h.pad()
	start := time.Now()
	for fullRepaints(second) == 0 {
		if time.Since(start) > 300*time.Millisecond {
			t.Fatal("the new pad got no full repaint within 300 ms")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.waitLight("A blue on the new pad", 0, solid(blue))
}

// The 1 s tick frees the slot of an ended session after EndedGrace.
func TestTickFreesEndedSlot(t *testing.T) {
	h := newHarness(t, opts{cfg: func(c *daemon.Config) { c.Table.EndedGrace = 100 * time.Millisecond }})
	h.work("A", "/w/alpha")
	h.waitLight("A blue", 0, solid(blue))
	h.event("SessionEnd", "A", "/w/alpha")
	eventually(t, "slot freed after EndedGrace", func() bool { return h.sess("claude:A") == nil })
	h.waitLight("key dark", 0, isOff)
}

// One APPROVE press answers the oldest held request only.
func TestApproveAnswersOnlyTheOldest(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.press("AG00")
	first := h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli")
	h.waitHeld(1)
	second := h.permission(context.Background(), "A", "/w/alpha", "Edit", "cli")
	h.waitHeld(2)
	eventually(t, "both pending", func() bool { return h.sess("claude:A").Pending == 2 })

	h.press("AG10")
	if r := recv(t, first); r.body != allowJSON {
		t.Fatalf("first reply = %q", r.body)
	}
	time.Sleep(100 * time.Millisecond)
	if got := h.status().Held; got != 1 {
		t.Fatalf("held = %d after one press, want 1", got)
	}
	select {
	case r := <-second:
		t.Fatalf("second request was answered: %+v", r)
	default:
	}
	h.press("AG12")
	if r := recv(t, second); !strings.Contains(r.body, `"behavior":"deny"`) {
		t.Fatalf("second reply = %q", r.body)
	}
}

// DENY with nothing to deny does nothing. It does not blink APPROVE.
func TestDenyWithNothingPendingDoesNotFlash(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.press("AG00")
	h.waitLight("A selected", 0, func(l light) bool { return l.B == 1.0 })
	h.press("AG12")
	time.Sleep(150 * time.Millisecond)
	if everLit(h.pad(), 10, red) || everLit(h.pad(), 11, red) {
		t.Fatal("DENY with nothing pending flashed the APPROVE key")
	}
}

// A status call sees every event that was answered before it, even when the
// run loop has a backlog.
func TestStatusSeesEarlierEvents(t *testing.T) {
	h := newHarness(t, opts{})
	const n = 60
	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("S%d", i)
			body := fmt.Sprintf(`{"session_id":%q,"cwd":"/w/%s","hook_event_name":"UserPromptSubmit"}`, id, id)
			req, _ := http.NewRequest(http.MethodPost, h.base+"/event", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Cm2-Entrypoint", "cli")
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp.StatusCode != 200 {
				failed.Add(1)
			}
			if resp != nil {
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait() // every POST was answered, so every event is queued or applied
	if failed.Load() != 0 {
		t.Fatalf("%d posts failed", failed.Load())
	}
	if got := len(h.status().Sessions); got != n {
		t.Fatalf("status shows %d of %d sessions", got, n)
	}
}

// --- logs and defaults -------------------------------------------------

// syncBuf is a log sink that is safe to read while the daemon writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) count(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Count(s.b.String(), sub)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newLogger(buf *syncBuf) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

// One distinct open error is logged once, however often the daemon retries,
// and an Input Monitoring failure carries the hint.
func TestOpenErrorLoggedOncePerDistinctError(t *testing.T) {
	var buf syncBuf
	imErr := fmt.Errorf("open: %w", device.ErrInputMonitoring)
	h := newHarness(t, opts{openFail: imErr, cfg: func(c *daemon.Config) { c.Logger = newLogger(&buf) }})
	eventually(t, "several retries", func() bool { return h.opensCount() >= 6 })
	if n := buf.count("cannot open the pad"); n != 1 {
		t.Fatalf("%d open-error lines for one error, want 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "hint=") {
		t.Fatalf("no Input Monitoring hint:\n%s", buf.String())
	}
	// A different error is logged again.
	h.setOpenFail(errors.New("pad not connected"))
	eventually(t, "second error logged", func() bool { return buf.count("cannot open the pad") == 2 })
}

// A foreign reply logs one warning for the whole pause, not one per reply.
func TestForeignReplyLoggedOncePerPause(t *testing.T) {
	var buf syncBuf
	h := newHarness(t, opts{cfg: func(c *daemon.Config) { c.Logger = newLogger(&buf); c.ForeignPause = 2 * time.Second }})
	h.work("A", "/w/a")
	h.waitLight("A blue", 0, solid(blue))
	h.pad().Inject([]byte(`{"id":7,"result":{"ok":1}}`))
	eventually(t, "paused", func() bool { return h.status().Pad.PaintPausedUntil != nil })
	h.pad().Inject([]byte(`{"id":8,"result":{"ok":1}}`))
	h.pad().Inject([]byte(`{"id":9,"result":{"ok":1}}`))
	time.Sleep(100 * time.Millisecond)
	if n := buf.count("another program is talking to the pad"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, buf.String())
	}
}

// Held requests are limited by MaxHold, 590 s unless set. The hooks package
// tests the default; here it is only passed through.
func TestMaxHoldIsPassedThrough(t *testing.T) {
	h := newHarness(t, opts{cfg: func(c *daemon.Config) { c.MaxHold = 200 * time.Millisecond }})
	h.work("A", "/w/alpha")
	h.press("AG00")
	start := time.Now()
	r := recv(t, h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli"))
	if r.body != "" || time.Since(start) < 150*time.Millisecond || time.Since(start) > 2*time.Second {
		t.Fatalf("reply %q after %v", r.body, time.Since(start))
	}
}

// A pad that answers with an error is alive: the daemon does not reopen it.
func TestDeviceErrorDoesNotReopenThePad(t *testing.T) {
	h := newHarness(t, opts{padHook: func(n int, f *devicetest.Fake) {
		f.OnMessage = func(req []byte) [][]byte {
			var r struct {
				ID int64 `json:"id"`
			}
			json.Unmarshal(req, &r)
			return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"error":{"code":5,"message":"no"}}`, r.ID))}
		}
	}})
	h.work("A", "/w/alpha")
	time.Sleep(600 * time.Millisecond) // many ticks, each one a failed paint
	if got := h.padCount(); got != 1 {
		t.Fatalf("the pad was opened %d times", got)
	}
	if !h.d.Connected() {
		t.Fatal("pad dropped")
	}
}

// Run returns only after its focus goroutines have ended.
func TestRunWaitsForFocusGoroutines(t *testing.T) {
	var finished atomic.Bool
	h := newHarness(t, opts{cfg: func(c *daemon.Config) {
		c.Focuser.Run = func(ctx context.Context, name string, args ...string) error {
			<-ctx.Done()
			time.Sleep(150 * time.Millisecond)
			finished.Store(true)
			return ctx.Err()
		}
	}})
	h.work("A", "/w/alpha")
	h.waitLight("A blue", 0, solid(blue))
	h.press("AG00") // starts a focus command that waits for shutdown
	eventually(t, "selected", func() bool { return h.status().Selected == 0 })
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
		h.done <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if !finished.Load() {
		t.Fatal("Run returned while a focus goroutine still ran")
	}
}
