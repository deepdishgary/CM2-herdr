package device

import (
	"encoding/json"
	"strings"
	"testing"
)

const sampleKeymap = `{"version":1,"activeProfileId":0,"profiles":[{"id":0,"name":"Default","layers":[
{"id":0,"name":"Layer 1","layout":{"keymap":[["KC_A","KC_B"],["KC_C","KC_D","KC_E","KC_F"],["KC_G","KC_H","KC_I","KC_J"],["KC_NONE","KC_L","KC_M"]],
"encoders":[["KC_VOLU","KC_VOLD","KC_MPLY"]],"joystick":{"type":"RADIAL","sectors":[{"k":"KI_X","a1":0.1875,"a2":0.3125}]}},
"lights":{"backlight":{"effect":"rainbow","brightness":1,"speed":0.5}}},
{"id":1,"name":"Layer 2","layout":{"keymap":[["KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE"]],
"encoders":[["KC_NONE","KC_NONE","KC_NONE"]],"joystick":{"type":"JOYSTICK","sectors":[]}}}]}]}`

// grid turns rows of codes into layout cells; "-" means null (keep).
func grid(rows ...[]string) [][]*string {
	out := make([][]*string, len(rows))
	for i, r := range rows {
		out[i] = make([]*string, len(r))
		for j, c := range r {
			if c != "-" {
				c := c
				out[i][j] = &c
			}
		}
	}
	return out
}

func agentLayout() Layout {
	return Layout{
		Layer: 1,
		Keymap: grid(
			[]string{"KV_OAI_AG00", "KV_OAI_AG01"},
			[]string{"KV_OAI_AG02", "KV_OAI_AG03", "KV_OAI_AG04", "KV_OAI_AG05"},
			[]string{"KV_OAI_AG06", "KV_OAI_AG07", "KV_OAI_AG08", "KV_OAI_AG09"},
			[]string{"KC_NONE", "KV_OAI_AG10", "KV_OAI_AG11"},
		),
		Encoders: grid([]string{"KV_OAI_AG12", "KV_OAI_AG13", "KV_OAI_AG14"}),
	}
}

func TestApplyLayoutChangesOnlyTargetLayer(t *testing.T) {
	out, changes, err := ApplyLayout([]byte(sampleKeymap), agentLayout())
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 15 { // 12 keys (the small circle stays KC_NONE) + 3 encoder codes
		t.Fatalf("got %d changes, want 15: %+v", len(changes), changes)
	}
	// Layer 1 and everything outside layer 2's keymap/encoders are unchanged.
	want := strings.Replace(sampleKeymap, `"keymap":[["KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE","KC_NONE"],["KC_NONE","KC_NONE","KC_NONE"]],
"encoders":[["KC_NONE","KC_NONE","KC_NONE"]]`,
		`"keymap":[["KV_OAI_AG00","KV_OAI_AG01"],["KV_OAI_AG02","KV_OAI_AG03","KV_OAI_AG04","KV_OAI_AG05"],["KV_OAI_AG06","KV_OAI_AG07","KV_OAI_AG08","KV_OAI_AG09"],["KC_NONE","KV_OAI_AG10","KV_OAI_AG11"]],
"encoders":[["KV_OAI_AG12","KV_OAI_AG13","KV_OAI_AG14"]]`, 1)
	if want == sampleKeymap {
		t.Fatal("test setup: replacement did not apply")
	}
	if !SameKeymap(out, []byte(want)) {
		t.Fatalf("result differs from expected:\n%s", out)
	}
	if !json.Valid(out) {
		t.Fatal("output is not valid JSON")
	}
}

func TestApplyLayoutKeepsNumbersExact(t *testing.T) {
	out, _, err := ApplyLayout([]byte(sampleKeymap), agentLayout())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"a1":0.1875`) || !strings.Contains(string(out), `"brightness":1`) {
		t.Fatalf("numbers changed: %s", out)
	}
}

func TestApplyLayoutIsIdempotent(t *testing.T) {
	once, _, err := ApplyLayout([]byte(sampleKeymap), agentLayout())
	if err != nil {
		t.Fatal(err)
	}
	twice, changes, err := ApplyLayout(once, agentLayout())
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 || !SameKeymap(once, twice) {
		t.Fatalf("second apply changed %d keys", len(changes))
	}
}

func TestApplyLayoutRefusesWrongShape(t *testing.T) {
	cases := map[string]func(*Layout){
		"missing row":   func(l *Layout) { l.Keymap = l.Keymap[:3] },
		"short row":     func(l *Layout) { l.Keymap[3] = l.Keymap[3][:2] },
		"long row":      func(l *Layout) { l.Keymap[0] = append(l.Keymap[0], grid([]string{"KC_NONE"})[0][0]) },
		"empty code":    func(l *Layout) { l.Keymap[1][1] = grid([]string{""})[0][0] },
		"bad encoders":  func(l *Layout) { l.Encoders = grid([]string{"KC_NONE"}) },
		"unknown layer": func(l *Layout) { l.Layer = 7 },
		"nothing":       func(l *Layout) { l.Keymap, l.Encoders = nil, nil },
	}
	for name, mutate := range cases {
		l := agentLayout()
		mutate(&l)
		if _, _, err := ApplyLayout([]byte(sampleKeymap), l); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestApplyLayoutRejectsBadDocument(t *testing.T) {
	for _, doc := range []string{`not json`, `[]`, `{"profiles":[]}`, `{"activeProfileId":3,"profiles":[{"id":0,"layers":[]}]}`} {
		if _, _, err := ApplyLayout([]byte(doc), agentLayout()); err == nil {
			t.Errorf("%q: expected an error", doc)
		}
	}
}

func TestSameKeymapIgnoresOrderAndSpace(t *testing.T) {
	a := `{"a":1,"b":[1,2,{"c":"x"}]}`
	b := "{ \"b\": [1, 2, {\"c\": \"x\"}],\n \"a\": 1 }"
	if !SameKeymap([]byte(a), []byte(b)) {
		t.Fatal("expected equal")
	}
	if SameKeymap([]byte(a), []byte(`{"a":2,"b":[1,2,{"c":"x"}]}`)) {
		t.Fatal("expected different")
	}
}

func TestNullCellKeepsPadCode(t *testing.T) {
	// Layer 0: only the encoder press changes; the turn codes stay as on the pad.
	f := LayoutFile{Layers: []Layout{
		{Layer: 0, Encoders: grid([]string{"-", "-", "KI_LS2"})},
		agentLayout(),
	}}
	out, changes, err := ApplyLayouts([]byte(sampleKeymap), f)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 16 {
		t.Fatalf("got %d changes, want 16: %+v", len(changes), changes)
	}
	if changes[0].Where != "layer 0 encoders[0][2]" || changes[0].Old != "KC_MPLY" || changes[0].New != "KI_LS2" {
		t.Fatalf("first change = %+v", changes[0])
	}
	if !strings.Contains(string(out), `["KC_VOLU","KC_VOLD","KI_LS2"]`) {
		t.Fatalf("layer 0 encoder turn codes not kept: %s", out)
	}
	if !strings.Contains(string(out), `["KC_A","KC_B"]`) {
		t.Fatalf("layer 0 keys changed: %s", out)
	}
}

func TestApplyLayoutsNeedsLayers(t *testing.T) {
	if _, _, err := ApplyLayouts([]byte(sampleKeymap), LayoutFile{}); err == nil {
		t.Fatal("expected an error")
	}
}
