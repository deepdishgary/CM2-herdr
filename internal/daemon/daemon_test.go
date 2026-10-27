package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cm2-claude/internal/daemon"
	"cm2-claude/internal/device"
	"cm2-claude/internal/herdr/herdrtest"
)

func TestTwoSessionsWorkingThenDone(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.work("B", "/w/beta")
	h.waitLight("slot 0 blue", 0, solid(blue))
	h.waitLight("slot 1 blue", 1, solid(blue))
	if l := lights(h.pad())[2]; !l.off() {
		t.Fatalf("slot 2 should be off: %+v", l)
	}

	h.event("Stop", "A", "/w/alpha")
	h.event("Stop", "B", "/w/beta")
	h.waitLight("slot 0 green", 0, solid(green))
	h.waitLight("slot 1 green", 1, solid(green))

	// A key press focuses the session and marks it seen: green -> dim white.
	h.press("AG00")
	h.waitLight("slot 0 idle", 0, func(l light) bool { return l.C == white && l.E == 1 && l.B == 0.25 })
	h.waitLight("slot 1 still green", 1, solid(green))
	if calls := h.focusCalls(); len(calls) != 1 || calls[0] != "open -a Ghostty" {
		t.Fatalf("focus calls = %q", calls)
	}
	if s := h.sess("claude:A"); s == nil || s.State != "idle" {
		t.Fatalf("A = %+v", s)
	}
}

func TestApproveSelectedSessionAllows(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.press("AG00")
	h.waitLight("A selected", 0, func(l light) bool { return l.B == 1.0 })

	res := h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli")
	h.waitHeld(1)
	h.waitLight("approve breathing", 10, breathing(green))
	h.waitLight("approve breathing 2", 11, breathing(green))
	h.waitLight("deny lit", 12, solid(red))
	h.waitLight("A needs input", 0, breathing(amber))

	h.press("AG10")
	r := recv(t, res)
	if r.code != 200 || r.body != allowJSON {
		t.Fatalf("reply = %d %q", r.code, r.body)
	}
	h.waitHeld(0)
	h.waitLight("approve off", 10, isOff)
	h.waitLight("deny off", 12, isOff)
	h.waitLight("A working again", 0, solid(blue))
	if s := h.sess("claude:A"); s.Pending != 0 || s.State != "working" {
		t.Fatalf("A = %+v", s)
	}
}

func TestDenySelectedSession(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.press("AG00")
	res := h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli")
	h.waitHeld(1)
	h.press("AG12")
	r := recv(t, res)
	if r.code != 200 || !strings.Contains(r.body, `"behavior":"deny"`) || !strings.Contains(r.body, "Denied from the macropad") {
		t.Fatalf("reply = %d %q", r.code, r.body)
	}
	h.waitHeld(0)
}

func TestApproveOtherSessionSelectedIsRefused(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.work("B", "/w/beta")
	h.press("AG01") // B is selected
	h.waitLight("B selected", 1, func(l light) bool { return l.B == 1.0 })

	res := h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli")
	h.waitHeld(1)
	// A's request does not light APPROVE: the selected session is B.
	if l := lights(h.pad())[10]; !l.off() {
		t.Fatalf("approve lit for a session that is not selected: %+v", l)
	}

	h.press("AG10")
	eventually(t, "red flash on approve", func() bool { return everLit(h.pad(), 10, red) })
	if got := h.status().Held; got != 1 {
		t.Fatalf("request not held any more: held = %d", got)
	}
	select {
	case r := <-res:
		t.Fatalf("request was answered: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	// The flash ends by itself.
	h.waitLight("flash over", 10, isOff)

	// Deny does nothing to a session that is not selected either.
	h.press("AG12")
	time.Sleep(60 * time.Millisecond)
	if got := h.status().Held; got != 1 {
		t.Fatalf("deny acted on a session that is not selected: held = %d", got)
	}

	// Release it the normal way.
	h.event("PostToolUse", "A", "/w/alpha")
	if r := recv(t, res); r.code != 200 || r.body != "" {
		t.Fatalf("reply = %d %q", r.code, r.body)
	}
}

func TestTerminalAnswerReleasesHeldRequest(t *testing.T) {
	for _, ev := range []string{"PostToolUse", "Stop", "StopFailure", "UserPromptSubmit", "SessionEnd"} {
		t.Run(ev, func(t *testing.T) {
			h := newHarness(t, opts{})
			h.work("A", "/w/alpha")
			h.press("AG00")
			res := h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli")
			h.waitHeld(1)
			h.event(ev, "A", "/w/alpha")
			r := recv(t, res)
			if r.code != 200 || r.body != "" {
				t.Fatalf("reply = %d %q, want an empty 200", r.code, r.body)
			}
			h.waitHeld(0)
			eventually(t, "pending cleared", func() bool {
				s := h.sess("claude:A")
				return s != nil && s.Pending == 0
			})
		})
	}
}

func TestAskUserQuestionAnsweredAtOnce(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.press("AG00")
	h.waitLight("selected", 0, func(l light) bool { return l.B == 1.0 })

	r := recv(t, h.permission(context.Background(), "A", "/w/alpha", "AskUserQuestion", "cli"))
	if r.code != 200 || r.body != "" || r.took > time.Second {
		t.Fatalf("reply = %d %q after %v", r.code, r.body, r.took)
	}
	if got := h.status().Held; got != 0 {
		t.Fatalf("held = %d", got)
	}
	h.waitLight("amber at once", 0, breathing(amber))
	h.waitLight("deny lit for a question", 12, solid(red))
	if l := lights(h.pad())[10]; !l.off() {
		t.Fatalf("approve lit for a question: %+v", l)
	}
	if s := h.sess("claude:A"); !s.Question || s.State != "needs-input" {
		t.Fatalf("A = %+v", s)
	}

	// APPROVE must not send a plain allow: it is refused.
	h.press("AG10")
	eventually(t, "red flash on approve", func() bool { return everLit(h.pad(), 10, red) })
	// DENY does nothing in v1.
	h.press("AG12")
	time.Sleep(60 * time.Millisecond)
	if s := h.sess("claude:A"); !s.Question {
		t.Fatalf("deny changed the question: %+v", s)
	}
}

func TestClientDisconnectReleases(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	h.press("AG00")
	ctx, cancel := context.WithCancel(context.Background())
	h.permission(ctx, "A", "/w/alpha", "Bash", "cli")
	h.waitHeld(1)
	h.waitLight("approve lit", 10, breathing(green))
	cancel()
	h.waitHeld(0)
	eventually(t, "pending cleared", func() bool { return h.sess("claude:A").Pending == 0 })
	h.waitLight("approve off", 10, isOff)
}

func TestPadDisconnectedHoldsNothing(t *testing.T) {
	h := newHarness(t, opts{openFail: errors.New("pad not connected")})
	h.work("A", "/w/alpha")
	if h.d.Connected() {
		t.Fatal("connected without a pad")
	}
	r := recv(t, h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli"))
	if r.code != 200 || r.body != "" || r.took > time.Second {
		t.Fatalf("reply = %d %q after %v", r.code, r.body, r.took)
	}
	if h.status().Held != 0 {
		t.Fatal("a request was held without a pad")
	}

	// The pad appears: the next request is held. The earlier request was
	// never held, so APPROVE skips it and answers the held one.
	h.setOpenFail(nil)
	h.waitConnected()
	res := h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli")
	h.waitHeld(1)
	h.press("AG00")
	h.waitLight("approve lit", 10, breathing(green))
	h.press("AG10")
	if r := recv(t, res); r.body != allowJSON {
		t.Fatalf("reply = %q", r.body)
	}
	eventually(t, "no pending left", func() bool { return h.sess("claude:A").Pending == 0 })
}

func TestNoDeviceNeverHolds(t *testing.T) {
	h := newHarness(t, opts{noPad: true})
	h.work("A", "/w/alpha")
	r := recv(t, h.permission(context.Background(), "A", "/w/alpha", "Bash", "cli"))
	if r.body != "" || r.took > time.Second {
		t.Fatalf("reply = %q after %v", r.body, r.took)
	}
	if st := h.status(); st.Pad.Enabled || st.Pad.Connected {
		t.Fatalf("pad status = %+v", st.Pad)
	}
}

func TestHeadlessSessionNeverGetsASlot(t *testing.T) {
	h := newHarness(t, opts{})
	h.start("H", "/w/headless", "0", "")
	h.event("UserPromptSubmit", "H", "/w/headless")
	if s := h.sess("claude:H"); s != nil {
		t.Fatalf("headless session in the table: %+v", s)
	}
	// Its permission request is not held: nobody could answer it.
	r := recv(t, h.permission(context.Background(), "H", "/w/headless", "Bash", "cli"))
	if r.body != "" || r.took > time.Second {
		t.Fatalf("reply = %q after %v", r.body, r.took)
	}
	// An unknown session from a headless entrypoint is not held either.
	r = recv(t, h.permission(context.Background(), "X", "/w/x", "Bash", "sdk-cli"))
	if r.body != "" || r.took > time.Second {
		t.Fatalf("reply = %q after %v", r.body, r.took)
	}
	// A session under an ignored folder is dropped.
	h.start("I", "/ignored/job", "1", "")
	h.event("UserPromptSubmit", "I", "/ignored/job")
	if got := len(h.status().Sessions); got != 0 {
		t.Fatalf("sessions = %+v", h.status().Sessions)
	}
	// Both are counted, so `cm2d status` shows that hooks arrive and were filtered.
	if got := h.status().Ignored; got != 2 {
		t.Fatalf("ignored_sessions = %d, want 2 (H and I)", got)
	}
	for id, l := range lights(h.pad()) {
		if id < 10 && !l.off() {
			t.Fatalf("thread %d lit: %+v", id, l)
		}
	}
}

func TestHerdrNonClaudeAgentPaintsFromHerdr(t *testing.T) {
	hs := herdrtest.New(t)
	hs.SetAgents(herdrtest.NewAgent("w1:p1", "codex", "working"))
	h := newHarness(t, opts{herdr: hs})

	h.waitLight("codex working", 0, solid(blue))
	s := h.sess("herdr:w1:p1")
	if s == nil || s.Kind != "codex" || s.Frontend != "herdr" || s.Slot != 0 {
		t.Fatalf("session = %+v", s)
	}
	if st := h.status(); !st.Herdr.Available || st.Herdr.Agents != 1 {
		t.Fatalf("herdr status = %+v", st.Herdr)
	}

	hs.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "blocked" })
	h.waitLight("codex blocked", 0, breathing(amber))
	hs.Mutate("w1:p1", func(a herdrtest.Agent) { a["agent_status"] = "done" })
	h.waitLight("codex done", 0, solid(green))

	// The key focuses the Herdr pane, then raises Ghostty.
	h.press("AG00")
	eventually(t, "herdr focus", func() bool {
		for _, p := range hs.State().FocusCalls {
			if p == "w1:p1" {
				return true
			}
		}
		return false
	})
	eventually(t, "ghostty raised", func() bool { return len(h.focusCalls()) == 1 })

	// The pane closes: the key goes dark.
	hs.SetAgents()
	h.waitLight("pane gone", 0, isOff)
}

func TestHerdrClaudeAgentJoinsHookSession(t *testing.T) {
	hs := herdrtest.New(t)
	h := newHarness(t, opts{herdr: hs})
	h.work("S1", "/w/proj")

	a := herdrtest.NewAgent("w1:p2", "claude", "working")
	a["cwd"] = "/w/proj"
	a["agent_session"] = map[string]any{"source": "hook", "agent": "claude", "kind": "id", "value": "S1"}
	// A Claude agent with no session id yet is skipped.
	b := herdrtest.NewAgent("w1:p3", "claude", "working")
	hs.SetAgents(a, b)

	eventually(t, "join", func() bool {
		s := h.sess("claude:S1")
		return s != nil && s.HerdrPane == "w1:p2" && s.Frontend == "herdr"
	})
	st := h.status()
	if len(st.Sessions) != 1 {
		t.Fatalf("want one session, got %+v", st.Sessions)
	}
	if st.Sessions[0].Slot != 0 {
		t.Fatalf("slot = %d", st.Sessions[0].Slot)
	}
	// Hooks stay the state source for a Claude session.
	h.waitLight("hook state", 0, solid(blue))
	h.event("Stop", "S1", "/w/proj")
	h.waitLight("done from hooks", 0, solid(green))
}

func TestSlotEvictionRules(t *testing.T) {
	h := newHarness(t, opts{})
	for i := 0; i < 10; i++ {
		h.work(fmt.Sprintf("S%d", i), fmt.Sprintf("/w/p%d", i))
	}
	for i := 0; i < 10; i++ {
		if s := h.sess(fmt.Sprintf("claude:S%d", i)); s == nil || s.Slot != i {
			t.Fatalf("S%d = %+v", i, s)
		}
	}
	// All ten are working, so an eleventh session waits without a slot.
	h.work("S10", "/w/p10")
	if s := h.sess("claude:S10"); s == nil || s.Slot != -1 {
		t.Fatalf("S10 = %+v", s)
	}
	// Its permission request has no key to answer it: the dialog decides at once.
	r := recv(t, h.permission(context.Background(), "S10", "/w/p10", "Bash", "cli"))
	if r.body != "" || r.took > time.Second {
		t.Fatalf("reply = %q after %v", r.body, r.took)
	}
	if h.status().Held != 0 {
		t.Fatal("held a request for a session without a slot")
	}

	// S0 finishes. Done and unseen is not evictable.
	h.event("Stop", "S0", "/w/p0")
	h.waitLight("S0 green", 0, solid(green))
	if s := h.sess("claude:S10"); s.Slot != -1 {
		t.Fatalf("S10 took a slot from an unseen Done session: %+v", s)
	}
	// S0 is seen: it is idle now, so S10 may take its key.
	h.press("AG00")
	eventually(t, "S10 takes the slot of S0", func() bool {
		a, b := h.sess("claude:S10"), h.sess("claude:S0")
		return a != nil && b != nil && a.Slot == 0 && b.Slot == -1
	})
	h.waitLight("slot 0 blue again", 0, solid(blue))
}

func TestDialMovesSelection(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/a")
	h.work("B", "/w/b")
	h.work("C", "/w/c")
	h.waitLight("C blue", 2, solid(blue))
	sel := func(want int) {
		t.Helper()
		eventually(t, fmt.Sprintf("selected %d", want), func() bool { return h.status().Selected == want })
		h.waitLight("selected is bright", want, func(l light) bool { return l.B == 1.0 })
	}
	h.turn("ENC_CW") // no selection: the first session
	sel(0)
	h.turn("ENC_CW")
	sel(1)
	h.turn("ENC_CC")
	sel(0)
	h.turn("ENC_CC") // wraps
	sel(2)
	h.waitLight("old selection dim", 0, func(l light) bool { return l.B == 0.45 })
	// A turn does not focus anything.
	if got := h.focusCalls(); len(got) != 0 {
		t.Fatalf("focus calls = %q", got)
	}
	// A release (act 0) does nothing.
	h.pad().Inject([]byte(`{"m":"v.oai.hid","p":{"k":"AG00","act":0}}`))
	time.Sleep(50 * time.Millisecond)
	if h.status().Selected != 2 {
		t.Fatal("a key release changed the selection")
	}
}

func TestWideKeyDoubleReportCountsOnce(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/a")
	h.press("AG00")
	res := h.permission(context.Background(), "A", "/w/a", "Bash", "cli")
	h.waitHeld(1)
	h.waitLight("approve lit", 10, breathing(green))

	h.press("AG10")
	h.press("AG11")
	if r := recv(t, res); r.body != allowJSON {
		t.Fatalf("reply = %q", r.body)
	}
	// A second count would find nothing to approve and flash red.
	time.Sleep(150 * time.Millisecond)
	if everLit(h.pad(), 10, red) || everLit(h.pad(), 11, red) {
		t.Fatal("the second half of the wide key counted as another press")
	}
}

func TestShutdownReleasesHeldRequestsAndTurnsLightsOff(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/a")
	h.press("AG00")
	res := h.permission(context.Background(), "A", "/w/a", "Bash", "cli")
	h.waitHeld(1)
	h.waitLight("approve lit", 10, breathing(green))
	pad := h.pad()

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
	if r := recv(t, res); r.code != 200 || r.body != "" {
		t.Fatalf("held request reply = %d %q", r.code, r.body)
	}
	calls := thstatusCalls(pad)
	last := calls[len(calls)-1]
	if len(last) != 13 {
		t.Fatalf("last thstatus has %d threads", len(last))
	}
	for _, th := range last {
		if th.E != 0 {
			t.Fatalf("thread %d is still lit at exit: %+v", th.ID, th)
		}
	}
	if h.d.Connected() {
		t.Fatal("pad still connected after Run")
	}
	if err := pad.Write(make([]byte, 64)); err == nil {
		t.Fatal("the pad was not closed")
	}
}

func TestPadReconnectRepaints(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/a")
	h.waitLight("A blue", 0, solid(blue))
	first := h.pad()

	first.Close() // unplug
	eventually(t, "disconnect seen", func() bool { return !h.d.Connected() || h.padCount() > 1 })
	eventually(t, "reopened", func() bool { return h.padCount() == 2 && h.d.Connected() })

	second := h.pad()
	eventually(t, "full repaint", func() bool {
		calls := thstatusCalls(second)
		return len(calls) > 0 && len(calls[0]) == 13
	})
	h.waitLight("A blue on the new pad", 0, solid(blue))
	// Keys work on the new pad.
	h.press("AG00")
	eventually(t, "selected", func() bool { return h.status().Selected == 0 })
}

func TestForeignReplyPausesPainting(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/a")
	h.waitLight("A blue", 0, solid(blue))
	pad := h.pad()

	pad.Inject([]byte(`{"id":7,"result":{"ok":1}}`)) // a reply to somebody else's call
	eventually(t, "paused", func() bool { return h.status().Pad.PaintPausedUntil != nil })
	n := len(pad.Requests())
	h.work("B", "/w/b")
	time.Sleep(100 * time.Millisecond)
	if got := len(pad.Requests()); got != n {
		t.Fatalf("painted %d request(s) during the pause", got-n)
	}
	// The pause ends and the lights catch up with a full repaint.
	h.waitLight("B blue after the pause", 1, solid(blue))
	calls := thstatusCalls(pad)
	if len(calls[len(calls)-1]) != 13 && len(calls[len(calls)-2]) != 13 {
		t.Fatal("no full repaint after the pause")
	}
}

func TestSecondDaemonOnTheSamePortFailsWithoutTouchingThePad(t *testing.T) {
	h := newHarness(t, opts{})
	opened := 0
	d2 := daemon.New(daemon.Config{
		Addr:    h.d.Addr(),
		OpenPad: func() (device.Transport, error) { opened++; return nil, errors.New("must not open") },
	})
	err := d2.Run(context.Background())
	if err == nil {
		t.Fatal("the second daemon started")
	}
	if opened != 0 {
		t.Fatal("the second daemon opened the pad")
	}
}

func TestStatusEndpoint(t *testing.T) {
	h := newHarness(t, opts{})
	h.work("A", "/w/alpha")
	st := h.status()
	if st.Version != "test" || !st.Pad.Connected || st.Selected != -1 || len(st.Sessions) != 1 {
		t.Fatalf("status = %+v", st)
	}
	s := st.Sessions[0]
	if s.Key != "claude:A" || s.Label != "alpha" || s.State != "working" || s.Slot != 0 || s.Frontend != "terminal" {
		t.Fatalf("session = %+v", s)
	}
	if code, _ := h.post("/nope", "", map[string]string{}); code != 404 {
		t.Fatalf("unknown path = %d", code)
	}
}
