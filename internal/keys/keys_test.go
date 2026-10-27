package keys

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"cm2-claude/internal/device"
)

func note(method, k string, act int) device.Notification {
	return device.Notification{
		Method: method,
		Params: json.RawMessage(fmt.Sprintf(`{"k":%q,"act":%d}`, k, act)),
	}
}

func hid(k string, act int) device.Notification { return note("v.oai.hid", k, act) }

var t0 = time.Unix(2000, 0)

func TestSessionKeys(t *testing.T) {
	var d Decoder
	for i := 0; i < 10; i++ {
		a, ok := d.Decode(hid(fmt.Sprintf("AG%02d", i), 1), t0)
		if !ok || a.Kind != SelectFocus || a.Slot != i {
			t.Errorf("AG%02d -> %+v %v", i, a, ok)
		}
	}
}

func TestReleaseIgnored(t *testing.T) {
	var d Decoder
	for _, k := range []string{"AG00", "AG09", "AG10", "AG11", "AG12"} {
		if a, ok := d.Decode(hid(k, 0), t0); ok {
			t.Errorf("release of %s gave %+v", k, a)
		}
	}
}

func TestDeny(t *testing.T) {
	var d Decoder
	a, ok := d.Decode(hid("AG12", 1), t0)
	if !ok || a.Kind != Deny {
		t.Fatalf("%+v %v", a, ok)
	}
	// No debounce on deny.
	if _, ok := d.Decode(hid("AG12", 1), t0.Add(time.Millisecond)); !ok {
		t.Error("second deny dropped")
	}
}

func TestApproveDoubleReportCountsOnce(t *testing.T) {
	for _, order := range [][2]string{{"AG10", "AG11"}, {"AG11", "AG10"}} {
		var d Decoder
		a, ok := d.Decode(hid(order[0], 1), t0)
		if !ok || a.Kind != Approve {
			t.Fatalf("first: %+v %v", a, ok)
		}
		if _, ok := d.Decode(hid(order[1], 1), t0.Add(5*time.Millisecond)); ok {
			t.Errorf("%v: second half counted", order)
		}
		// Releases in between change nothing.
		if _, ok := d.Decode(hid(order[0], 0), t0.Add(60*time.Millisecond)); ok {
			t.Error("release counted")
		}
	}
}

func TestApproveDebounceWindow(t *testing.T) {
	var d Decoder
	d.Decode(hid("AG10", 1), t0)
	if _, ok := d.Decode(hid("AG10", 1), t0.Add(79*time.Millisecond)); ok {
		t.Error("79 ms later counted")
	}
	if a, ok := d.Decode(hid("AG10", 1), t0.Add(80*time.Millisecond)); !ok || a.Kind != Approve {
		t.Error("80 ms later should count")
	}
	// The window runs from the last counted press, not from the dropped half.
	d = Decoder{}
	d.Decode(hid("AG10", 1), t0)
	d.Decode(hid("AG11", 1), t0.Add(70*time.Millisecond))
	if _, ok := d.Decode(hid("AG10", 1), t0.Add(85*time.Millisecond)); !ok {
		t.Error("window extended by a dropped report")
	}
}

func TestSecondPressAfterGapCounts(t *testing.T) {
	var d Decoder
	d.Decode(hid("AG10", 1), t0)
	d.Decode(hid("AG11", 1), t0.Add(5*time.Millisecond))
	a, ok := d.Decode(hid("AG11", 1), t0.Add(500*time.Millisecond))
	if !ok || a.Kind != Approve {
		t.Fatalf("%+v %v", a, ok)
	}
}

func TestDial(t *testing.T) {
	var d Decoder
	a, ok := d.Decode(hid("ENC_CW", 2), t0)
	if !ok || a.Kind != Move || a.Delta != 1 {
		t.Errorf("CW: %+v %v", a, ok)
	}
	a, ok = d.Decode(hid("ENC_CC", 2), t0)
	if !ok || a.Kind != Move || a.Delta != -1 {
		t.Errorf("CC: %+v %v", a, ok)
	}
	// Dial events are not debounced.
	if _, ok := d.Decode(hid("ENC_CW", 2), t0); !ok {
		t.Error("repeat turn dropped")
	}
}

func TestDialWrongAct(t *testing.T) {
	var d Decoder
	for _, act := range []int{0, 1, 3} {
		if a, ok := d.Decode(hid("ENC_CW", act), t0); ok {
			t.Errorf("ENC_CW act %d gave %+v", act, a)
		}
	}
}

func TestSessionKeyWrongAct(t *testing.T) {
	var d Decoder
	if a, ok := d.Decode(hid("AG03", 2), t0); ok {
		t.Errorf("act 2 on a key gave %+v", a)
	}
}

func TestUnknownKeysIgnored(t *testing.T) {
	var d Decoder
	for _, k := range []string{"", "AG13", "AG99", "AG0", "AG001", "ag00", "AGxx", "AG-1", "ENC_PRESS", "KC_A", "AG1A"} {
		if a, ok := d.Decode(hid(k, 1), t0); ok {
			t.Errorf("key %q gave %+v", k, a)
		}
	}
}

func TestOtherMethodsIgnored(t *testing.T) {
	var d Decoder
	n := device.Notification{
		Method: "kb.radial",
		Params: json.RawMessage(`{"a":10,"d":0.5,"s":2,"l":1}`),
	}
	if _, ok := d.Decode(n, t0); ok {
		t.Error("kb.radial gave an action")
	}
	if _, ok := d.Decode(note("v.oai.other", "AG00", 1), t0); ok {
		t.Error("other method gave an action")
	}
}

func TestBadParams(t *testing.T) {
	var d Decoder
	for _, p := range []string{``, `null`, `[]`, `"AG00"`, `{`, `{"k":"AG00"}`, `{"act":1}`, `{"k":5,"act":1}`, `{"k":"AG00","act":"1"}`} {
		n := device.Notification{Method: "v.oai.hid", Params: json.RawMessage(p)}
		if a, ok := d.Decode(n, t0); ok {
			t.Errorf("params %q gave %+v", p, a)
		}
	}
}

func TestLongNameWireFormatFromClient(t *testing.T) {
	// Params come from the device client as "p" or "params"; Decode reads Params only.
	n := device.Notification{
		Method: "v.oai.hid",
		Params: json.RawMessage(`{"k":"AG03","act":1}`),
		Raw:    json.RawMessage(`{"m":"v.oai.hid","p":{"k":"AG03","act":1}}`),
	}
	var d Decoder
	a, ok := d.Decode(n, t0)
	if !ok || a.Kind != SelectFocus || a.Slot != 3 {
		t.Fatalf("%+v %v", a, ok)
	}
}
