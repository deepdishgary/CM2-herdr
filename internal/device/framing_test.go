package device

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func feedAll(r *Reassembler, frames [][]byte) []Message {
	var out []Message
	for _, f := range frames {
		out = append(out, r.Feed(f)...)
	}
	return out
}

func obj(n int) []byte { // a valid JSON object of exactly n bytes (n >= 10)
	pad := n - len(`{"k":""}`)
	return []byte(`{"k":"` + strings.Repeat("a", pad) + `"}`)
}

func TestEncodeShape(t *testing.T) {
	msg := []byte(`{"id":1}`)
	frames := Encode(msg)
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	f := frames[0]
	if len(f) != FrameSize || f[0] != 0x06 || f[1] != 2 || f[2] != byte(len(msg)) {
		t.Fatalf("bad header: % x", f[:8])
	}
	if !bytes.Equal(f[3:3+len(msg)], msg) {
		t.Fatal("payload mismatch")
	}
	for _, b := range f[3+len(msg):] {
		if b != 0 {
			t.Fatal("padding is not zero")
		}
	}
	if got := Encode(nil); len(got) != 0 {
		t.Fatalf("Encode(nil) = %d frames", len(got))
	}
	if got := EncodeChannel(ChannelDebug, []byte("hi"))[0][1]; got != 1 {
		t.Fatalf("debug channel byte = %d", got)
	}
}

func TestFrameCounts(t *testing.T) {
	for _, tc := range []struct{ size, frames int }{
		{10, 1}, {60, 1}, {61, 1}, {62, 2}, {122, 2}, {123, 3}, {1000, 17},
	} {
		frames := Encode(obj(tc.size))
		if len(frames) != tc.frames {
			t.Errorf("size %d: %d frames, want %d", tc.size, len(frames), tc.frames)
		}
		// every frame but the last is full
		for i, f := range frames[:len(frames)-1] {
			if f[2] != MaxPayload {
				t.Errorf("size %d frame %d length %d", tc.size, i, f[2])
			}
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, size := range []int{10, 30, 60, 61, 62, 63, 121, 122, 123, 500, 5000} {
		msg := obj(size)
		for _, term := range []string{"", "\n", "\r\n"} {
			r := NewReassembler()
			got := feedAll(r, Encode(append(append([]byte(nil), msg...), term...)))
			if len(got) != 1 || !bytes.Equal(got[0].Data, msg) || got[0].Channel != ChannelRPC {
				t.Fatalf("size %d term %q: got %d messages", size, term, len(got))
			}
		}
	}
}

func TestManyMessagesInOneStream(t *testing.T) {
	var stream []byte
	var want []string
	for i := 0; i < 50; i++ {
		m := fmt.Sprintf(`{"id":%d,"result":{"text":"%s"}}`, i, strings.Repeat("x", i*3))
		want = append(want, m)
		stream = append(stream, m...)
		if i%2 == 0 {
			stream = append(stream, '\n')
		}
	}
	r := NewReassembler()
	got := feedAll(r, Encode(stream))
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i].Data) != want[i] {
			t.Fatalf("message %d differs", i)
		}
	}
}

func TestTwoMessagesInOneFrame(t *testing.T) {
	r := NewReassembler()
	got := r.Feed(Encode([]byte(`{"a":1}{"b":2}`))[0])
	if len(got) != 2 || string(got[0].Data) != `{"a":1}` || string(got[1].Data) != `{"b":2}` {
		t.Fatalf("got %v", got)
	}
}

func TestSplitAtEveryOffset(t *testing.T) {
	msg := []byte(`{"id":7,"result":{"s":"a\"}\\{b","u":"é","n":[1,{"x":null}]}}`)
	for cut := 1; cut < len(msg); cut++ {
		var frames [][]byte
		frames = append(frames, EncodeChannel(ChannelRPC, msg[:cut])...)
		frames = append(frames, EncodeChannel(ChannelRPC, msg[cut:])...)
		r := NewReassembler()
		got := feedAll(r, frames)
		if len(got) != 1 || !bytes.Equal(got[0].Data, msg) {
			t.Fatalf("cut %d: got %d messages", cut, len(got))
		}
	}
}

func TestBracesInStringsAreNotStructure(t *testing.T) {
	msg := []byte(`{"s":"}}}{{{ \" } \\"}`)
	got := NewReassembler().Feed(Encode(msg)[0])
	if len(got) != 1 || !bytes.Equal(got[0].Data, msg) {
		t.Fatalf("got %v", got)
	}
}

func TestMultibyteSplitAcrossFrames(t *testing.T) {
	// raw UTF-8 split in the middle of a rune must survive (byte stream)
	msg := []byte(`{"s":"` + strings.Repeat("é", 60) + `"}`)
	frames := Encode(msg)
	if len(frames) < 2 {
		t.Fatal("test needs a split")
	}
	got := feedAll(NewReassembler(), frames)
	if len(got) != 1 || !bytes.Equal(got[0].Data, msg) {
		t.Fatal("multibyte message lost")
	}
}

func TestNoiseAndPadding(t *testing.T) {
	r := NewReassembler()
	var frames [][]byte
	// a frame of only padding
	frames = append(frames, EncodeChannel(ChannelRPC, make([]byte, 20))...)
	frames = append(frames, EncodeChannel(ChannelRPC, []byte("garbage ok? "))...)
	frames = append(frames, EncodeChannel(ChannelRPC, []byte(`{"id":1,"result":true}`))...)
	// reports of other collections (keyboard etc.)
	kb := make([]byte, 64)
	kb[0], kb[1], kb[2] = 0x01, 0x02, 0x04
	frames = append(frames, kb)
	frames = append(frames, EncodeChannel(ChannelRPC, []byte("\n\n\x00\x00"+`{"id":2,"result":false}`))...)
	// bad frames: length too big, unknown channel, too short
	bad1 := make([]byte, 64)
	bad1[0], bad1[1], bad1[2] = 0x06, 2, 200
	bad2 := make([]byte, 64)
	bad2[0], bad2[1], bad2[2] = 0x06, 9, 5
	frames = append(frames, bad1, bad2, []byte{0x06, 2}, nil)
	got := feedAll(r, frames)
	if len(got) != 2 || string(got[0].Data) != `{"id":1,"result":true}` || string(got[1].Data) != `{"id":2,"result":false}` {
		t.Fatalf("got %v", got)
	}
	st := r.Stats()
	if st.Ignored != 2 || st.BadFrames != 3 || st.JunkBytes == 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestBrokenMessageResyncs(t *testing.T) {
	r := NewReassembler()
	// a message cut off by a newline, then a good one
	got := feedAll(r, Encode([]byte("{\"id\":1,\"res\n"+`{"id":2,"result":1}`+"\n")))
	if len(got) != 1 || string(got[0].Data) != `{"id":2,"result":1}` {
		t.Fatalf("got %v", got)
	}
	if r.Stats().Malformed != 1 {
		t.Fatalf("malformed = %d", r.Stats().Malformed)
	}
	// balanced braces but not JSON
	r = NewReassembler()
	got = feedAll(r, Encode([]byte(`{nope}{"ok":1}`)))
	if len(got) != 1 || string(got[0].Data) != `{"ok":1}` || r.Stats().Malformed != 1 {
		t.Fatalf("got %v stats %+v", got, r.Stats())
	}
}

func TestOverflowIsBounded(t *testing.T) {
	r := NewReassembler()
	// an opening brace and then endless text: the buffer must stay bounded
	feedAll(r, Encode([]byte(`{"a":"`)))
	chunk := bytes.Repeat([]byte("x"), MaxPayload)
	for i := 0; i < MaxMessageSize/MaxPayload+10; i++ {
		r.Feed(EncodeChannel(ChannelRPC, chunk)[0])
	}
	if r.Stats().Overflow == 0 {
		t.Fatal("overflow not counted")
	}
	if cap(r.rpc.buf) > 2*MaxMessageSize {
		t.Fatalf("buffer grew to %d", cap(r.rpc.buf))
	}
}

func TestDebugChannelLines(t *testing.T) {
	r := NewReassembler()
	got := feedAll(r, EncodeChannel(ChannelDebug, []byte("boot ok\r\nsecond li")))
	got = append(got, feedAll(r, EncodeChannel(ChannelDebug, []byte("ne\n\n")))...)
	if len(got) != 2 || string(got[0].Data) != "boot ok" || string(got[1].Data) != "second line" || got[0].Channel != ChannelDebug {
		t.Fatalf("got %v", got)
	}
}

func TestChannelsAreIndependent(t *testing.T) {
	// a half message on RPC must not be disturbed by debug lines between frames
	rpc := Encode([]byte(`{"id":3,"result":"` + strings.Repeat("z", 100) + `"}`))
	r := NewReassembler()
	var out []Message
	out = append(out, r.Feed(rpc[0])...)
	out = append(out, r.Feed(EncodeChannel(ChannelDebug, []byte("log\n"))[0])...)
	for _, f := range rpc[1:] {
		out = append(out, r.Feed(f)...)
	}
	if len(out) != 2 || out[0].Channel != ChannelDebug || out[1].Channel != ChannelRPC {
		t.Fatalf("got %v", out)
	}
	if !json.Valid(out[1].Data) {
		t.Fatal("rpc message damaged")
	}
}

func TestEscapeNonASCII(t *testing.T) {
	in := []byte(`{"s":"é😀 ok"}`)
	out := EscapeNonASCII(in)
	if string(out) != "{\"s\":\"\\u00e9\\ud83d\\ude00 ok\"}" {
		t.Fatalf("got %s", out)
	}
	var v struct{ S string }
	if err := json.Unmarshal(out, &v); err != nil || v.S != "é😀 ok" {
		t.Fatalf("round trip: %v %q", err, v.S)
	}
	pure := []byte(`{"a":1}`)
	if &EscapeNonASCII(pure)[0] != &pure[0] {
		t.Fatal("ASCII input should be returned as is")
	}
}

func TestNormalizeInput(t *testing.T) {
	withID := make([]byte, 64)
	withID[0], withID[1], withID[2] = 6, 2, 1
	noID := withID[1:] // 63 bytes
	for _, tc := range []struct {
		name string
		mode IDMode
		id   uint32
		data []byte
		ok   bool
	}{
		{"auto with id", IDAuto, 6, withID, true},
		{"auto without id", IDAuto, 6, noID, true},
		{"auto id zero with id byte", IDAuto, 0, withID, true},
		{"auto other collection", IDAuto, 1, withID, false},
		{"auto no id and os id zero", IDAuto, 0, noID, false},
		{"present", IDPresent, 6, withID, true},
		{"present wrong first byte", IDPresent, 6, noID, false},
		{"absent", IDAbsent, 6, noID, true},
		{"absent os id zero", IDAbsent, 0, noID, false},
		{"empty", IDAuto, 6, nil, true}, // empty body: all-zero frame, rejected later by the reassembler
	} {
		got := NormalizeInput(tc.mode, tc.id, tc.data)
		if (got != nil) != tc.ok {
			t.Errorf("%s: got %v, want ok=%v", tc.name, got != nil, tc.ok)
			continue
		}
		if got != nil && tc.name != "empty" && !bytes.Equal(got, withID) {
			t.Errorf("%s: frame differs: % x", tc.name, got[:6])
		}
	}
	if m, err := ParseIDMode("absent"); err != nil || m != IDAbsent {
		t.Fatal("ParseIDMode")
	}
	if _, err := ParseIDMode("x"); err == nil {
		t.Fatal("ParseIDMode accepted junk")
	}
}
