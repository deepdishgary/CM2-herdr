package device

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Errors returned by Client.
var (
	// ErrClosed: the client or its transport is closed.
	ErrClosed = errors.New("device: client closed")
	// ErrTimeout: no reply arrived in time. TimeoutError wraps it.
	ErrTimeout = errors.New("device: rpc timeout")
)

// TimeoutError is returned when a call gets no reply before its deadline.
// errors.Is matches both ErrTimeout and context.DeadlineExceeded.
type TimeoutError struct {
	Method string
	ID     int64
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("device: no reply to %s (id %d) before the deadline", e.Method, e.ID)
}

func (e *TimeoutError) Is(target error) bool {
	return target == ErrTimeout || target == context.DeadlineExceeded
}

// DeviceError is an error object in a reply from the pad.
type DeviceError struct {
	Method  string
	Code    int
	Message string
	// Raw is the error value as the pad sent it.
	Raw json.RawMessage
}

func (e *DeviceError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("device error from %s: %s", e.Method, e.Raw)
	}
	return fmt.Sprintf("device error from %s: %s (code %d)", e.Method, e.Message, e.Code)
}

func parseDeviceError(method string, raw json.RawMessage) *DeviceError {
	e := &DeviceError{Method: method, Raw: raw}
	var obj struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
	}
	var s string
	switch {
	case json.Unmarshal(raw, &obj) == nil:
		if n, err := obj.Code.Int64(); err == nil {
			e.Code = int(n)
		}
		e.Message = obj.Message
	case json.Unmarshal(raw, &s) == nil:
		e.Message = s
	}
	return e
}

// Notification is a message from the pad that is not a reply. On the wire
// the pad abbreviates the names: {"m":"v.oai.hid","p":{"k":"AG00","act":1}}.
// The long names {"method":..., "params":...} are accepted too.
type Notification struct {
	Method string
	Params json.RawMessage
	// Raw is the whole message.
	Raw json.RawMessage
}

// Reply is a matched reply.
type Reply struct {
	ID     int64
	Result json.RawMessage
	// Raw is the whole reply message as the pad sent it.
	Raw json.RawMessage
}

// ClientOptions configure a Client. The zero value is usable.
type ClientOptions struct {
	// Timeout is the default time a call waits for its reply. Default 3 s.
	Timeout time.Duration
	// FirstID is the first request id. Zero means a random start in
	// [1000, 30000). A start well above 1 lowers the chance that a reply
	// meant for another host (which starts at 1) matches one of our calls.
	// The range stays under 32767 so any parser on the pad accepts it.
	// Only id 1 is verified on the pad (prototype/device.js).
	FirstID int64
	// IDFirst puts "id" first in the request. The default order is
	// method, params, id, as prototype/device.js sends it (verified on the
	// pad). The task text lists id first. JSON objects are unordered, so
	// both should work; a pad parser that scans in order is the reason to
	// keep the verified order by default.
	IDFirst bool
	// Terminator is appended to every request. Default none, as in
	// prototype/device.js. design.md says messages are newline-terminated.
	// Set it to []byte("\n") if the pad does not answer.
	Terminator []byte
	// NotifyBuffer is the capacity of the notification channel. Default 256.
	NotifyBuffer int
	// OnForeign is called, from the reader goroutine, for each reply whose
	// id this client is not waiting for and did not abandon.
	OnForeign func(id int64, raw []byte)
}

// Stats counts what a Client saw.
type Stats struct {
	Calls   uint64 // requests sent
	Replies uint64 // replies matched to a call
	// Foreign counts replies whose id we never sent or already got an
	// answer for. A second host on the pad is the usual cause.
	Foreign uint64
	// ForeignIDs holds the last few foreign ids.
	ForeignIDs []int64
	// Late counts replies that came after their call timed out.
	Late                 uint64
	Notifications        uint64
	NotificationsDropped uint64 // consumer too slow
	DebugLines           uint64
	DebugDropped         uint64
	Unclassified         uint64 // JSON objects that are neither reply nor notification
	// TransportDropped counts input frames the transport dropped (slow reader).
	TransportDropped uint64
	Reassembler      ReassemblerStats
}

type outcome struct {
	reply *Reply
	err   error
}

// Client is a request/reply client over a Transport. It is safe for
// concurrent use. Requests are written one at a time, each message as a
// contiguous run of frames: the pad has one receive buffer per channel, so
// interleaved frames would corrupt both messages.
type Client struct {
	t   Transport
	opt ClientOptions

	nextID atomic.Int64
	reasm  *Reassembler

	mu         sync.Mutex
	pending    map[int64]chan outcome
	abandoned  map[int64]struct{}
	abandonFIF []int64
	closed     bool
	foreignIDs []int64

	writeSem chan struct{}
	notif    chan Notification
	debug    chan string
	quit     chan struct{}
	done     chan struct{}
	once     sync.Once

	calls, replies, foreign, late             atomic.Uint64
	notifs, notifDrop, dbgLines, dbgDrop, unc atomic.Uint64
}

const maxAbandoned = 256

// NewClient starts a Client on t. The Client owns t: Close closes it.
func NewClient(t Transport, o ClientOptions) *Client {
	if o.Timeout <= 0 {
		o.Timeout = 3 * time.Second
	}
	if o.NotifyBuffer <= 0 {
		o.NotifyBuffer = 256
	}
	c := &Client{
		t:         t,
		opt:       o,
		reasm:     NewReassembler(),
		pending:   map[int64]chan outcome{},
		abandoned: map[int64]struct{}{},
		writeSem:  make(chan struct{}, 1),
		notif:     make(chan Notification, o.NotifyBuffer),
		debug:     make(chan string, 256),
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	first := o.FirstID
	if first <= 0 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err == nil {
			first = 1000 + int64(binary.BigEndian.Uint32(b[:])%29000)
		} else {
			first = 1000
		}
	}
	c.nextID.Store(first)
	go c.readLoop()
	return c
}

// Notifications yields messages from the pad that are not replies, such as
// v.oai.hid key events. It is closed when the client ends. A slow consumer
// loses the newest messages (counted in Stats), never blocks the reader.
func (c *Client) Notifications() <-chan Notification { return c.notif }

// Debug yields text lines from the debug channel. Closed when the client ends.
func (c *Client) Debug() <-chan string { return c.debug }

// Done is closed when the client has stopped reading.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close stops the client, fails pending calls with ErrClosed and closes the
// transport.
func (c *Client) Close() {
	c.once.Do(func() {
		close(c.quit)
		<-c.done
		c.t.Close()
	})
}

// Stats returns a snapshot of the counters.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	ids := append([]int64(nil), c.foreignIDs...)
	c.mu.Unlock()
	s := Stats{
		Calls:                c.calls.Load(),
		Replies:              c.replies.Load(),
		Foreign:              c.foreign.Load(),
		ForeignIDs:           ids,
		Late:                 c.late.Load(),
		Notifications:        c.notifs.Load(),
		NotificationsDropped: c.notifDrop.Load(),
		DebugLines:           c.dbgLines.Load(),
		DebugDropped:         c.dbgDrop.Load(),
		Unclassified:         c.unc.Load(),
		Reassembler:          c.reasm.Stats(),
	}
	if d, ok := c.t.(DropCounter); ok {
		s.TransportDropped = d.DroppedReports()
	}
	return s
}

// Call sends a request and waits for its reply, for at most ClientOptions.Timeout
// (or until ctx ends, whichever is first). params may be nil (sent as
// null), a json.RawMessage, or any value json.Marshal accepts. The result
// is the "result" member of the reply. A reply with an error member gives a
// *DeviceError.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	r, err := c.CallFull(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return r.Result, nil
}

// CallFull is Call that also returns the raw reply.
func (c *Client) CallFull(ctx context.Context, method string, params any) (*Reply, error) {
	return c.CallTimeout(ctx, c.opt.Timeout, method, params)
}

// CallTimeout is CallFull with an explicit timeout.
func (c *Client) CallTimeout(ctx context.Context, d time.Duration, method string, params any) (*Reply, error) {
	if d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	id := c.nextID.Add(1) - 1
	req, err := buildRequest(id, method, params, c.opt.IDFirst)
	if err != nil {
		return nil, err
	}
	if len(c.opt.Terminator) > 0 {
		req = append(req, c.opt.Terminator...)
	}
	frames := Encode(req)

	ch := make(chan outcome, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()

	// One writer at a time. The wait for the turn can be cancelled.
	select {
	case c.writeSem <- struct{}{}:
	case <-ctx.Done():
		c.abandon(id)
		return nil, c.ctxErr(ctx, method, id)
	case <-c.done:
		c.abandon(id)
		return nil, ErrClosed
	}
	// Do not send a request whose caller already gave up.
	if ctx.Err() != nil {
		<-c.writeSem
		c.unregister(id)
		return nil, c.ctxErr(ctx, method, id)
	}
	// The OS write (IOHIDDeviceSetReport) can block when the pad stops
	// taking reports. A goroutine does the write, so the call still ends at
	// its deadline. The goroutine keeps the write turn until the write
	// returns, so no other request can interleave its frames with ours.
	werrc := make(chan error, 1)
	go func() {
		var err error
		for _, f := range frames {
			if err = c.t.Write(f); err != nil {
				break
			}
		}
		<-c.writeSem
		werrc <- err
	}()
	var werr error
	select {
	case werr = <-werrc:
	case <-ctx.Done():
		c.abandon(id) // the request may be partly or fully sent
		return nil, c.ctxErr(ctx, method, id)
	}
	if werr != nil {
		c.abandon(id) // part of the request may have reached the pad
		return nil, fmt.Errorf("device: write %s: %w", method, werr)
	}
	c.calls.Add(1)

	select {
	case o := <-ch:
		return o.unwrap(method)
	case <-ctx.Done():
		select { // the reply may have raced with the deadline
		case o := <-ch:
			return o.unwrap(method)
		default:
		}
		c.abandon(id)
		return nil, c.ctxErr(ctx, method, id)
	}
}

func (o outcome) unwrap(method string) (*Reply, error) {
	var de *DeviceError
	if errors.As(o.err, &de) {
		de.Method = method
	}
	return o.reply, o.err
}

func (c *Client) ctxErr(ctx context.Context, method string, id int64) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &TimeoutError{Method: method, ID: id}
	}
	return ctx.Err()
}

// unregister forgets a call that never reached the pad.
func (c *Client) unregister(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// abandon forgets a call whose request was (or may have been) sent. A late
// reply for it is counted as late, not foreign.
func (c *Client) abandon(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pending[id]; !ok {
		return
	}
	delete(c.pending, id)
	c.abandoned[id] = struct{}{}
	c.abandonFIF = append(c.abandonFIF, id)
	if len(c.abandonFIF) > maxAbandoned {
		delete(c.abandoned, c.abandonFIF[0])
		c.abandonFIF = c.abandonFIF[1:]
	}
}

func buildRequest(id int64, method string, params any, idFirst bool) ([]byte, error) {
	var p []byte
	switch v := params.(type) {
	case nil:
		p = []byte("null")
	case json.RawMessage:
		if len(v) == 0 {
			p = []byte("null")
		} else {
			// Compact: a raw newline on the wire would end the message early
			// on a pad that splits on newlines (as prototype/device.js does).
			var cb bytes.Buffer
			if err := json.Compact(&cb, v); err != nil {
				return nil, errors.New("device: params are not valid JSON")
			}
			p = cb.Bytes()
		}
	case []byte:
		return nil, errors.New("device: params must be a value or json.RawMessage, not []byte")
	default:
		var err error
		if p, err = json.Marshal(v); err != nil {
			return nil, fmt.Errorf("device: marshal params: %w", err)
		}
	}
	if !json.Valid(p) {
		return nil, errors.New("device: params are not valid JSON")
	}
	m, err := json.Marshal(method)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	idStr := strconv.FormatInt(id, 10)
	if idFirst {
		fmt.Fprintf(&b, `{"id":%s,"method":%s,"params":%s}`, idStr, m, p)
	} else {
		fmt.Fprintf(&b, `{"method":%s,"params":%s,"id":%s}`, m, p, idStr)
	}
	return EscapeNonASCII(b.Bytes()), nil
}

// readLoop is the only reader of the transport.
func (c *Client) readLoop() {
	defer close(c.done)
	defer close(c.debug)
	defer close(c.notif)
	defer c.failAll()
	reports := c.t.Reports()
	for {
		select {
		case <-c.quit:
			return
		case r, ok := <-reports:
			if !ok {
				return
			}
			for _, m := range c.reasm.Feed(r) {
				c.dispatch(m)
			}
		}
	}
}

func (c *Client) failAll() {
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		ch <- outcome{err: ErrClosed}
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

type envelope struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
	Method string          `json:"method"`
	M      string          `json:"m"`
	Params json.RawMessage `json:"params"`
	P      json.RawMessage `json:"p"`
}

func (c *Client) dispatch(m Message) {
	if m.Channel == ChannelDebug {
		c.dbgLines.Add(1)
		select {
		case c.debug <- string(m.Data):
		default:
			c.dbgDrop.Add(1)
		}
		return
	}
	var env envelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		c.unc.Add(1)
		return
	}
	method := env.Method
	if method == "" {
		method = env.M
	}
	// A message with a method is a notification, unless it also carries a
	// result or an error: then it is a reply that echoes the method.
	// prototype/device.js checks the pending id first, so it does the same.
	hasOutcome := len(env.Result) > 0 || (len(env.Error) > 0 && string(bytes.TrimSpace(env.Error)) != "null")
	if method != "" && !hasOutcome {
		params := env.Params
		if len(params) == 0 {
			params = env.P
		}
		c.notifs.Add(1)
		select {
		case c.notif <- Notification{Method: method, Params: params, Raw: m.Data}:
		default:
			c.notifDrop.Add(1)
		}
		return
	}
	idText := string(bytes.TrimSpace(env.ID))
	if idText == "" || idText == "null" {
		c.unc.Add(1)
		return
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		c.countForeign(0, m.Data) // id is not an integer: not ours
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	} else if _, was := c.abandoned[id]; was {
		delete(c.abandoned, id)
		c.mu.Unlock()
		c.late.Add(1)
		return
	}
	c.mu.Unlock()
	if !ok {
		c.countForeign(id, m.Data)
		return
	}
	c.replies.Add(1)
	hasErr := len(env.Error) > 0 && string(bytes.TrimSpace(env.Error)) != "null"
	if hasErr {
		// the method name is not in the reply; the caller wraps it
		ch <- outcome{err: parseDeviceError("", env.Error)}
		return
	}
	res := env.Result
	if len(res) == 0 {
		res = json.RawMessage("null")
	}
	ch <- outcome{reply: &Reply{ID: id, Result: res, Raw: m.Data}}
}

func (c *Client) countForeign(id int64, raw []byte) {
	c.foreign.Add(1)
	c.mu.Lock()
	c.foreignIDs = append(c.foreignIDs, id)
	if len(c.foreignIDs) > 8 {
		c.foreignIDs = c.foreignIDs[1:]
	}
	c.mu.Unlock()
	if c.opt.OnForeign != nil {
		c.opt.OnForeign(id, raw)
	}
}
