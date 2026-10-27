package painter_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"cm2-claude/internal/device"
	"cm2-claude/internal/device/devicetest"
	"cm2-claude/internal/painter"
)

func newPad(t *testing.T, reply string) (*devicetest.Fake, *painter.DeviceLights) {
	t.Helper()
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		var r struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(req, &r); err != nil {
			t.Errorf("bad request %q", req)
		}
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":%s}`, r.ID, reply))}
	}
	c := device.NewClient(f, device.ClientOptions{Timeout: 2 * time.Second})
	t.Cleanup(c.Close)
	return f, painter.NewDeviceLights(c)
}

func TestDeviceLightsWire(t *testing.T) {
	f, l := newPad(t, `{"ok":1}`)
	err := l.SetThreads(context.Background(), []painter.Thread{
		{ID: 3, Color: 0xFFA000, Brightness: 0.45, Effect: 4, Speed: 1},
		{ID: 4, Color: 0x0050FF, Brightness: 1, Effect: 1},
		{ID: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	var r struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(reqs[0], &r); err != nil {
		t.Fatal(err)
	}
	if r.Method != "v.oai.thstatus" {
		t.Errorf("method %q", r.Method)
	}
	want := []string{
		`{"id":3,"c":16752640,"b":0.45,"e":4,"s":1}`,
		`{"id":4,"c":20735,"b":1,"e":1}`,
		`{"id":5,"c":0,"b":0,"e":0}`,
	}
	if len(r.Params) != len(want) {
		t.Fatalf("params %s", reqs[0])
	}
	for i, w := range want {
		if string(r.Params[i]) != w {
			t.Errorf("entry %d = %s, want %s", i, r.Params[i], w)
		}
	}
}

func TestDeviceLightsEmptySendsNothing(t *testing.T) {
	f, l := newPad(t, `{"ok":1}`)
	if err := l.SetThreads(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.Requests()) != 0 {
		t.Fatal("sent a request")
	}
}

func TestDeviceLightsBadReply(t *testing.T) {
	_, l := newPad(t, `{"ok":0}`)
	if err := l.SetThreads(context.Background(), []painter.Thread{{ID: 1}}); err == nil {
		t.Fatal("want error for ok=0")
	}
}

func TestDeviceLightsCallError(t *testing.T) {
	f := devicetest.New()
	f.WriteErr = errors.New("pad write failed")
	c := device.NewClient(f, device.ClientOptions{Timeout: time.Second})
	t.Cleanup(c.Close)
	l := painter.NewDeviceLights(c)
	if err := l.SetThreads(context.Background(), []painter.Thread{{ID: 1}}); err == nil {
		t.Fatal("want error")
	}
}

// Painter on top of the device-backed lights: one full paint on the wire.
func TestPainterOverDevice(t *testing.T) {
	f, l := newPad(t, `{"ok":1}`)
	p := painter.New(l, nil)
	defer p.Stop()
	if err := p.Repaint(context.Background()); err != nil {
		t.Fatal(err)
	}
	var r struct {
		Params []json.RawMessage `json:"params"`
	}
	reqs := f.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	if err := json.Unmarshal(reqs[0], &r); err != nil || len(r.Params) != 13 {
		t.Fatalf("params %v err %v", len(r.Params), err)
	}
}
