// Package devicetest has a fake device.Transport for tests. It never touches
// hardware.
package devicetest

import (
	"errors"
	"sync"

	"cm2-claude/internal/device"
)

// Fake is an in-memory pad. Frames written to it are reassembled into
// messages. Each whole RPC message goes to OnMessage, and the messages that
// OnMessage returns are cut into frames and queued as input reports.
type Fake struct {
	// OnMessage handles one request. It may return reply messages (raw
	// JSON). It runs on the writer's goroutine. It may be nil.
	OnMessage func(req []byte) [][]byte
	// WriteErr, if set, makes every Write fail with it.
	WriteErr error

	reports chan []byte

	mu      sync.Mutex
	frames  [][]byte
	msgs    [][]byte
	reasm   *device.Reassembler
	closed  bool
	dropped uint64
}

// New returns a Fake with room for 4096 queued input frames.
func New() *Fake {
	return &Fake{reports: make(chan []byte, 4096), reasm: device.NewReassembler()}
}

// Write implements device.Transport.
func (f *Fake) Write(frame []byte) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return errors.New("fake: closed")
	}
	if f.WriteErr != nil {
		err := f.WriteErr
		f.mu.Unlock()
		return err
	}
	f.frames = append(f.frames, append([]byte(nil), frame...))
	var whole [][]byte
	for _, m := range f.reasm.Feed(frame) {
		if m.Channel == device.ChannelRPC {
			f.msgs = append(f.msgs, m.Data)
			whole = append(whole, m.Data)
		}
	}
	on := f.OnMessage
	f.mu.Unlock()
	if on != nil {
		for _, req := range whole {
			for _, rep := range on(req) {
				f.Inject(rep)
			}
		}
	}
	return nil
}

// Reports implements device.Transport.
func (f *Fake) Reports() <-chan []byte { return f.reports }

// Close implements device.Transport.
func (f *Fake) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.reports)
	}
}

// DroppedReports implements device.DropCounter.
func (f *Fake) DroppedReports() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dropped
}

// Inject queues msg (raw JSON) as input frames on the RPC channel, as if the
// pad sent it. A newline is added, as the pad does.
func (f *Fake) Inject(msg []byte) {
	f.InjectChannel(device.ChannelRPC, append(append([]byte(nil), msg...), '\n'))
}

// InjectChannel queues bytes as input frames on a channel, unchanged.
func (f *Fake) InjectChannel(ch byte, data []byte) {
	for _, fr := range device.EncodeChannel(ch, data) {
		f.InjectFrame(fr)
	}
}

// InjectFrame queues one raw input frame.
func (f *Fake) InjectFrame(frame []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	select {
	case f.reports <- append([]byte(nil), frame...):
	default:
		f.dropped++
	}
}

// Frames returns every frame written so far.
func (f *Fake) Frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.frames...)
}

// Requests returns every whole RPC message written so far.
func (f *Fake) Requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.msgs...)
}
