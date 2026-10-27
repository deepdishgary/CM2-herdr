package device_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"cm2-claude/internal/device"
	"cm2-claude/internal/device/devicetest"
)

// echoID parses a request and returns its id and method.
func parseReq(t *testing.T, req []byte) (id int64, method string, params json.RawMessage) {
	t.Helper()
	var r struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(req, &r); err != nil {
		t.Fatalf("bad request %q: %v", req, err)
	}
	return r.ID, r.Method, r.Params
}

func newClient(t *testing.T, f *devicetest.Fake, o device.ClientOptions) *device.Client {
	t.Helper()
	if o.Timeout == 0 {
		o.Timeout = 2 * time.Second
	}
	c := device.NewClient(f, o)
	t.Cleanup(c.Close)
	return c
}

func TestCallMatchesReplyByID(t *testing.T) {
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		id, method, _ := parseReq(t, req)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":{"echo":%q}}`, id, method))}
	}
	c := newClient(t, f, device.ClientOptions{FirstID: 1})
	res, err := c.Call(context.Background(), "device.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"echo":"device.status"}` {
		t.Fatalf("result %s", res)
	}
	reqs := f.Requests()
	if len(reqs) != 1 || string(reqs[0]) != `{"method":"device.status","params":null,"id":1}` {
		t.Fatalf("request on the wire: %q", reqs)
	}
	if s := c.Stats(); s.Calls != 1 || s.Replies != 1 || s.Foreign != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestRequestShapeOptions(t *testing.T) {
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		id, _, _ := parseReq(t, req)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":1}`, id))}
	}
	c := newClient(t, f, device.ClientOptions{FirstID: 5, IDFirst: true, Terminator: []byte("\n")})
	if _, err := c.Call(context.Background(), "fs.read", map[string]string{"file": "keymap.json"}); err != nil {
		t.Fatal(err)
	}
	frames := f.Frames()
	var got []byte
	for _, fr := range frames {
		got = append(got, fr[3:3+int(fr[2])]...)
	}
	if string(got) != "{\"id\":5,\"method\":\"fs.read\",\"params\":{\"file\":\"keymap.json\"}}\n" {
		t.Fatalf("wire: %q", got)
	}
}

func TestNonASCIIParamsAreEscaped(t *testing.T) {
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		id, _, _ := parseReq(t, req)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":1}`, id))}
	}
	c := newClient(t, f, device.ClientOptions{})
	if _, err := c.Call(context.Background(), "x", map[string]string{"s": "café"}); err != nil {
		t.Fatal(err)
	}
	for _, fr := range f.Frames() {
		for _, b := range fr {
			if b >= 0x80 {
				t.Fatal("non-ASCII byte on the wire")
			}
		}
	}
}

func TestLongRequestSpansFrames(t *testing.T) {
	f := devicetest.New()
	var gotParams string
	f.OnMessage = func(req []byte) [][]byte {
		id, _, p := parseReq(t, req)
		gotParams = string(p)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":"%s"}`, id, strings.Repeat("r", 400)))}
	}
	c := newClient(t, f, device.ClientOptions{})
	big := strings.Repeat("q", 300)
	res, err := c.Call(context.Background(), "fs.read", map[string]string{"file": big})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotParams, big) || len(res) != 402 {
		t.Fatalf("params ok=%v result len %d", strings.Contains(gotParams, big), len(res))
	}
	if n := len(f.Frames()); n < 6 {
		t.Fatalf("only %d frames", n)
	}
}

func TestDeviceErrorIsTyped(t *testing.T) {
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		id, _, _ := parseReq(t, req)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32601,"message":"no such method"}}`, id))}
	}
	c := newClient(t, f, device.ClientOptions{})
	_, err := c.Call(context.Background(), "nope", nil)
	var de *device.DeviceError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if de.Code != -32601 || de.Message != "no such method" || de.Method != "nope" {
		t.Fatalf("%+v", de)
	}
	// string-valued error
	f.OnMessage = func(req []byte) [][]byte {
		id, _, _ := parseReq(t, req)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"error":"bad file"}`, id))}
	}
	_, err = c.Call(context.Background(), "fs.read", nil)
	if !errors.As(err, &de) || de.Message != "bad file" {
		t.Fatalf("err = %v", err)
	}
}

func TestTimeout(t *testing.T) {
	f := devicetest.New() // never answers
	c := newClient(t, f, device.ClientOptions{Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := c.Call(context.Background(), "device.status", nil)
	if !errors.Is(err, device.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	var te *device.TimeoutError
	if !errors.As(err, &te) || te.Method != "device.status" {
		t.Fatalf("not a TimeoutError: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestPerCallTimeoutAndContext(t *testing.T) {
	f := devicetest.New()
	c := newClient(t, f, device.ClientOptions{Timeout: 5 * time.Second})
	start := time.Now()
	_, err := c.CallTimeout(context.Background(), 30*time.Millisecond, "x", nil)
	if !errors.Is(err, device.ErrTimeout) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err = c.Call(ctx, "x", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestLateReplyIsNotForeign(t *testing.T) {
	f := devicetest.New()
	c := newClient(t, f, device.ClientOptions{FirstID: 10, Timeout: 30 * time.Millisecond})
	if _, err := c.Call(context.Background(), "slow", nil); !errors.Is(err, device.ErrTimeout) {
		t.Fatal(err)
	}
	f.Inject([]byte(`{"id":10,"result":"late"}`))
	// a call after the late reply still works and flushes the reader
	f.OnMessage = func(req []byte) [][]byte {
		id, _, _ := parseReq(t, req)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":2}`, id))}
	}
	if _, err := c.Call(context.Background(), "next", nil); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.Late != 1 || s.Foreign != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestForeignRepliesAreCounted(t *testing.T) {
	f := devicetest.New()
	var mu sync.Mutex
	var seen []int64
	f.OnMessage = func(req []byte) [][]byte {
		id, _, _ := parseReq(t, req)
		return [][]byte{
			[]byte(`{"id":3,"result":"for another host"}`),
			[]byte(`{"id":4,"error":{"message":"x"}}`),
			[]byte(`{"id":"abc","result":1}`),
			[]byte(fmt.Sprintf(`{"id":%d,"result":"mine"}`, id)),
		}
	}
	c := newClient(t, f, device.ClientOptions{FirstID: 100, OnForeign: func(id int64, raw []byte) {
		mu.Lock()
		seen = append(seen, id)
		mu.Unlock()
	}})
	res, err := c.Call(context.Background(), "device.status", nil)
	if err != nil || string(res) != `"mine"` {
		t.Fatalf("res %s err %v", res, err)
	}
	s := c.Stats()
	if s.Foreign != 3 || len(s.ForeignIDs) != 3 {
		t.Fatalf("stats %+v", s)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != 3 || seen[1] != 4 {
		t.Fatalf("OnForeign ids %v", seen)
	}
}

func TestReplyIDsNeverCrossBetweenConcurrentCalls(t *testing.T) {
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		id, method, _ := parseReq(t, req)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":%q}`, id, method))}
	}
	c := newClient(t, f, device.ClientOptions{})
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := fmt.Sprintf("m%d.%s", i, strings.Repeat("x", i*7)) // many sizes, multi-frame
			res, err := c.Call(context.Background(), m, nil)
			if err != nil || string(res) != fmt.Sprintf("%q", m) {
				t.Errorf("call %d: res %s err %v", i, res, err)
			}
		}(i)
	}
	wg.Wait()
	// every request reached the pad as one intact message
	if n := len(f.Requests()); n != 40 {
		t.Fatalf("pad saw %d whole requests, want 40 (frames interleaved?)", n)
	}
}

func TestNotificationRouting(t *testing.T) {
	f := devicetest.New()
	c := newClient(t, f, device.ClientOptions{})
	f.Inject([]byte(`{"m":"v.oai.hid","p":{"k":"AG00","act":1}}`))
	f.Inject([]byte(`{"method":"v.oai.rad","params":{"x":1}}`))
	for i, want := range []struct{ m, p string }{{"v.oai.hid", `{"k":"AG00","act":1}`}, {"v.oai.rad", `{"x":1}`}} {
		select {
		case n := <-c.Notifications():
			if n.Method != want.m || string(n.Params) != want.p {
				t.Fatalf("%d: %+v", i, n)
			}
		case <-time.After(time.Second):
			t.Fatalf("%d: no notification", i)
		}
	}
	if s := c.Stats(); s.Notifications != 2 || s.Foreign != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestNotificationsDoNotBlockReplies(t *testing.T) {
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		id, _, _ := parseReq(t, req)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":1}`, id))}
	}
	c := newClient(t, f, device.ClientOptions{NotifyBuffer: 2})
	for i := 0; i < 20; i++ { // nobody reads Notifications
		f.Inject([]byte(`{"m":"v.oai.hid","p":{"k":"AG01","act":1}}`))
	}
	if _, err := c.Call(context.Background(), "device.status", nil); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.NotificationsDropped != 18 {
		t.Fatalf("dropped %d", s.NotificationsDropped)
	}
}

func TestDebugLines(t *testing.T) {
	f := devicetest.New()
	c := newClient(t, f, device.ClientOptions{})
	f.InjectChannel(device.ChannelDebug, []byte("hello\nworld\n"))
	for _, want := range []string{"hello", "world"} {
		select {
		case l := <-c.Debug():
			if l != want {
				t.Fatalf("got %q", l)
			}
		case <-time.After(time.Second):
			t.Fatal("no debug line")
		}
	}
}

func TestWriteErrorAndClose(t *testing.T) {
	f := devicetest.New()
	f.WriteErr = errors.New("boom")
	c := newClient(t, f, device.ClientOptions{})
	if _, err := c.Call(context.Background(), "x", nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	c.Close()
	c.Close() // twice is fine
	if _, err := c.Call(context.Background(), "x", nil); !errors.Is(err, device.ErrClosed) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := <-c.Notifications(); ok {
		t.Fatal("Notifications not closed")
	}
}

func TestTransportEndFailsPendingCalls(t *testing.T) {
	f := devicetest.New()
	c := newClient(t, f, device.ClientOptions{Timeout: 5 * time.Second})
	errc := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "x", nil); errc <- err }()
	time.Sleep(30 * time.Millisecond)
	f.Close() // unplug
	select {
	case err := <-errc:
		if !errors.Is(err, device.ErrClosed) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("call still waiting after transport closed")
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed")
	}
}

func TestBadParams(t *testing.T) {
	f := devicetest.New()
	c := newClient(t, f, device.ClientOptions{})
	if _, err := c.Call(context.Background(), "x", json.RawMessage(`{bad`)); err == nil {
		t.Fatal("accepted invalid JSON params")
	}
	if _, err := c.Call(context.Background(), "x", make(chan int)); err == nil {
		t.Fatal("accepted unmarshalable params")
	}
	if n := len(f.Frames()); n != 0 {
		t.Fatalf("%d frames sent for bad params", n)
	}
}

func TestDefaultFirstIDRange(t *testing.T) {
	for i := 0; i < 20; i++ {
		f := devicetest.New()
		c := device.NewClient(f, device.ClientOptions{Timeout: 10 * time.Millisecond})
		_, _ = c.Call(context.Background(), "x", nil)
		c.Close()
		id, _, _ := parseReq(t, f.Requests()[0])
		if id < 1000 || id >= 30000 {
			t.Fatalf("first id %d out of [1000,30000)", id)
		}
	}
}

func TestKeymapSizedReply(t *testing.T) {
	// a 20 KB reply split into many frames and mixed with a notification
	big := strings.Repeat(`{"key":"A","layer":1},`, 900)
	reply := `{"id":1,"result":"[` + strings.Replace(big, `"`, `\"`, -1) + `0]"}`
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		return [][]byte{[]byte(`{"m":"v.oai.hid","p":{"k":"AG00","act":0}}`), []byte(reply)}
	}
	c := newClient(t, f, device.ClientOptions{FirstID: 1})
	r, err := c.CallFull(context.Background(), "fs.read", map[string]string{"file": "keymap.json"})
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Raw) != reply {
		t.Fatal("raw reply differs")
	}
	var s string
	if err := json.Unmarshal(r.Result, &s); err != nil || !strings.HasSuffix(s, "0]") {
		t.Fatalf("result decode: %v", err)
	}
}

// A pretty-printed params argument must not put a raw newline on the wire:
// a pad that splits on newlines would see a cut-off message.
func TestRawParamsAreCompacted(t *testing.T) {
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		id, _, _ := parseReq(t, req)
		return [][]byte{[]byte(fmt.Sprintf(`{"id":%d,"result":1}`, id))}
	}
	c := newClient(t, f, device.ClientOptions{FirstID: 1})
	params := json.RawMessage("{\n  \"file\": \"keymap.json\",\n\t\"a\": [1, 2]\n}")
	if _, err := c.Call(context.Background(), "fs.read", params); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for _, fr := range f.Frames() {
		got = append(got, fr[3:3+int(fr[2])]...)
	}
	if strings.ContainsAny(string(got), "\n\r\t") || string(got) != `{"method":"fs.read","params":{"file":"keymap.json","a":[1,2]},"id":1}` {
		t.Fatalf("wire: %q", got)
	}
}

// A reply that echoes the method is still a reply (prototype/device.js
// matches the pending id first). A message with a method and no result or
// error stays a notification.
func TestReplyWithMethodMember(t *testing.T) {
	f := devicetest.New()
	f.OnMessage = func(req []byte) [][]byte {
		id, method, _ := parseReq(t, req)
		return [][]byte{
			[]byte(`{"m":"v.oai.hid","p":{"k":"AG00","act":1}}`),
			[]byte(fmt.Sprintf(`{"id":%d,"method":%q,"result":"ok"}`, id, method)),
		}
	}
	c := newClient(t, f, device.ClientOptions{})
	res, err := c.Call(context.Background(), "device.status", nil)
	if err != nil || string(res) != `"ok"` {
		t.Fatalf("res %s err %v", res, err)
	}
	select {
	case n := <-c.Notifications():
		if n.Method != "v.oai.hid" {
			t.Fatalf("notification %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("notification missing")
	}
	if s := c.Stats(); s.Foreign != 0 || s.Notifications != 1 {
		t.Fatalf("stats %+v", s)
	}
}

// blockingTransport blocks in Write until released, like IOHIDDeviceSetReport
// can when the pad stops taking reports.
type blockingTransport struct {
	release chan struct{}
	started chan struct{}
	reports chan []byte
	once    sync.Once
}

func (b *blockingTransport) Write([]byte) error {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return nil
}
func (b *blockingTransport) Reports() <-chan []byte { return b.reports }
func (b *blockingTransport) Close()                 {}

func TestBlockedWriteStillTimesOut(t *testing.T) {
	bt := &blockingTransport{release: make(chan struct{}), started: make(chan struct{}), reports: make(chan []byte)}
	c := device.NewClient(bt, device.ClientOptions{Timeout: 100 * time.Millisecond})
	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := c.Call(context.Background(), "device.status", nil); done <- err }()
	select {
	case err := <-done:
		var te *device.TimeoutError
		if !errors.As(err, &te) {
			t.Fatalf("err = %v, want TimeoutError", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("took %s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call hung on a blocked write")
	}
	// A second call must not write while the first write is still stuck.
	second := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "next", nil); second <- err }()
	select {
	case err := <-second:
		var te *device.TimeoutError
		if !errors.As(err, &te) {
			t.Fatalf("second err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Call hung")
	}
	close(bt.release)
	c.Close()
}
