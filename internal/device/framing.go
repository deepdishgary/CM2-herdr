// Package device talks to the Work Louder Creator Micro 2 over its vendor HID
// channel.
//
// The package has three layers:
//
//   - framing.go: pure Go. Cuts a JSON message into 64-byte HID reports and
//     puts reports from the pad back together into whole messages.
//   - rpc.go: pure Go. A request/reply client with id matching, timeouts and
//     notification delivery, over a Transport.
//   - iokit_darwin.go: cgo. The Transport for macOS (IOHIDManager/IOHIDDevice).
//
// Wire format (the same in design.md "Device facts" and prototype/device.js):
//
//	byte 0     report id, 0x06
//	byte 1     channel: 1 debug text, 2 RPC JSON
//	byte 2     payload length, 0..61
//	bytes 3..  payload (UTF-8), zero padded to 64 bytes in total
//
// A 64-byte report is one id byte plus 63 bytes of report data. Some APIs
// count the report as 63 bytes (without the id) and some as 64 (with it).
// This package always uses the 64-byte form inside frames. The IOKit layer
// converts at the OS boundary (see IDMode in iokit_types.go).
package device

import (
	"bytes"
	"encoding/json"
	"sync/atomic"
)

const (
	// ReportID is the HID report id of the vendor collection.
	ReportID byte = 0x06
	// ChannelDebug carries newline-terminated text log lines.
	ChannelDebug byte = 1
	// ChannelRPC carries JSON messages.
	ChannelRPC byte = 2

	// FrameSize is the size of one report including the report id byte.
	FrameSize = 64
	// HeaderSize is report id + channel + length.
	HeaderSize = 3
	// MaxPayload is the number of message bytes in one frame.
	MaxPayload = FrameSize - HeaderSize // 61

	// MaxMessageSize bounds the buffer for one incomplete message. A message
	// that grows past it is dropped and counted, so a lost closing brace
	// cannot grow memory without limit.
	MaxMessageSize = 1 << 20
	// maxDebugLine bounds one debug text line.
	maxDebugLine = 4096
)

// Message is one whole message from the pad.
//
// For ChannelRPC, Data is one complete, valid JSON text that starts with '{'.
// For ChannelDebug, Data is one text line without its line ending.
type Message struct {
	Channel byte
	Data    []byte
}

// Encode cuts msg into RPC-channel frames. Each frame is exactly FrameSize
// bytes. An empty msg gives no frames.
//
// Encode does not append a line terminator. prototype/device.js sends none
// and that is the behavior verified on the pad. design.md says messages are
// "newline-terminated". ClientOptions.Terminator makes this configurable.
func Encode(msg []byte) [][]byte { return EncodeChannel(ChannelRPC, msg) }

// EncodeChannel is Encode for an explicit channel.
func EncodeChannel(channel byte, msg []byte) [][]byte {
	if len(msg) == 0 {
		return nil
	}
	n := (len(msg) + MaxPayload - 1) / MaxPayload
	frames := make([][]byte, 0, n)
	for off := 0; off < len(msg); off += MaxPayload {
		end := off + MaxPayload
		if end > len(msg) {
			end = len(msg)
		}
		f := make([]byte, FrameSize)
		f[0] = ReportID
		f[1] = channel
		f[2] = byte(end - off)
		copy(f[HeaderSize:], msg[off:end])
		frames = append(frames, f)
	}
	return frames
}

// EscapeNonASCII rewrites every non-ASCII rune in a JSON text as \uXXXX
// (surrogate pairs for runes above U+FFFF), so each byte on the wire is
// 7-bit. The input must be valid JSON: non-ASCII bytes can then occur only
// inside strings, where the escape is valid. prototype/device.js does the
// same for each request.
func EscapeNonASCII(js []byte) []byte {
	ascii := true
	for _, b := range js {
		if b >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return js
	}
	const hex = "0123456789abcdef"
	var out bytes.Buffer
	out.Grow(len(js) + 16)
	put := func(u uint16) {
		out.WriteString(`\u`)
		out.WriteByte(hex[u>>12&0xf])
		out.WriteByte(hex[u>>8&0xf])
		out.WriteByte(hex[u>>4&0xf])
		out.WriteByte(hex[u&0xf])
	}
	for _, r := range string(js) {
		switch {
		case r < 0x80:
			out.WriteByte(byte(r))
		case r >= 0x10000:
			r -= 0x10000
			put(uint16(0xd800 + (r >> 10)))
			put(uint16(0xdc00 + (r & 0x3ff)))
		default:
			put(uint16(r))
		}
	}
	return out.Bytes()
}

// ReassemblerStats counts what the Reassembler saw.
type ReassemblerStats struct {
	Frames    uint64 // reports accepted as vendor frames
	Ignored   uint64 // reports with another report id (other collections)
	BadFrames uint64 // right id but unusable (short, bad channel, bad length)
	JunkBytes uint64 // payload bytes outside any JSON message
	Malformed uint64 // message-shaped text that is not valid JSON, dropped
	Overflow  uint64 // messages dropped for exceeding MaxMessageSize
}

// Reassembler turns input reports into whole messages. It keeps one stream
// per channel, so a message may be split over any number of frames and one
// frame may hold the end of one message and the start of the next.
//
// Message end on the RPC channel is found by JSON structure (matching
// braces, aware of strings and escapes), not by a newline. This handles
// both newline-terminated and unterminated devices. A raw newline inside an
// unfinished message means the message is broken: it is dropped and the
// stream resynchronizes at the next '{'. prototype/device.js splits on
// newlines, so it makes the same single-line assumption.
//
// A Reassembler is not safe for concurrent Feed calls. Stats may be read
// from any goroutine.
type Reassembler struct {
	rpc jsonScanner
	dbg lineScanner

	frames, ignored, bad      atomic.Uint64
	junk, malformed, overflow atomic.Uint64
}

// NewReassembler returns an empty Reassembler.
func NewReassembler() *Reassembler { return &Reassembler{} }

// Reset drops partial messages. Call it after a reconnect.
func (r *Reassembler) Reset() {
	r.rpc.reset()
	r.dbg.buf = r.dbg.buf[:0]
}

// Stats returns a snapshot of the counters.
func (r *Reassembler) Stats() ReassemblerStats {
	return ReassemblerStats{
		Frames:    r.frames.Load(),
		Ignored:   r.ignored.Load(),
		BadFrames: r.bad.Load(),
		JunkBytes: r.junk.Load(),
		Malformed: r.malformed.Load(),
		Overflow:  r.overflow.Load(),
	}
}

// Feed adds one input report (a whole frame, report id first) and returns the
// messages that this report completed. The returned Data slices are copies.
func (r *Reassembler) Feed(report []byte) []Message {
	if len(report) == 0 || report[0] != ReportID {
		r.ignored.Add(1)
		return nil
	}
	if len(report) < HeaderSize {
		r.bad.Add(1)
		return nil
	}
	channel, n := report[1], int(report[2])
	if (channel != ChannelDebug && channel != ChannelRPC) || n > MaxPayload || HeaderSize+n > len(report) {
		r.bad.Add(1)
		return nil
	}
	r.frames.Add(1)
	payload := report[HeaderSize : HeaderSize+n]
	var out []Message
	switch channel {
	case ChannelRPC:
		r.rpc.feed(payload, func(m []byte) {
			out = append(out, Message{Channel: ChannelRPC, Data: m})
		}, r)
	case ChannelDebug:
		r.dbg.feed(payload, func(l []byte) {
			out = append(out, Message{Channel: ChannelDebug, Data: l})
		})
	}
	return out
}

// jsonScanner finds complete top-level JSON objects in a byte stream.
type jsonScanner struct {
	buf    []byte
	active bool
	depth  int
	inStr  bool
	esc    bool
}

func (s *jsonScanner) reset() {
	s.buf = s.buf[:0]
	s.active, s.inStr, s.esc = false, false, false
	s.depth = 0
}

func (s *jsonScanner) feed(data []byte, emit func([]byte), st *Reassembler) {
	for _, b := range data {
		if !s.active {
			switch {
			case b == '{':
				s.buf = append(s.buf[:0], b)
				s.active, s.depth, s.inStr, s.esc = true, 1, false, false
			case b == 0 || b == '\n' || b == '\r' || b == ' ' || b == '\t':
				// padding and separators between messages
			default:
				st.junk.Add(1)
			}
			continue
		}
		if len(s.buf) >= MaxMessageSize {
			st.overflow.Add(1)
			s.reset()
			continue
		}
		s.buf = append(s.buf, b)
		if b == '\n' {
			// A raw newline is never inside a valid single-line message.
			st.malformed.Add(1)
			s.reset()
			continue
		}
		if s.inStr {
			switch {
			case s.esc:
				s.esc = false
			case b == '\\':
				s.esc = true
			case b == '"':
				s.inStr = false
			}
			continue
		}
		switch b {
		case '"':
			s.inStr = true
		case '{', '[':
			s.depth++
		case '}', ']':
			s.depth--
			if s.depth == 0 {
				if json.Valid(s.buf) {
					emit(append([]byte(nil), s.buf...))
				} else {
					st.malformed.Add(1)
				}
				s.reset()
			}
		}
	}
}

// lineScanner cuts a text stream into lines.
type lineScanner struct{ buf []byte }

func (s *lineScanner) feed(data []byte, emit func([]byte)) {
	for _, b := range data {
		if b == '\n' {
			s.flush(emit)
			continue
		}
		s.buf = append(s.buf, b)
		if len(s.buf) >= maxDebugLine {
			s.flush(emit)
		}
	}
}

func (s *lineScanner) flush(emit func([]byte)) {
	line := bytes.Trim(s.buf, "\x00\r \t")
	if len(line) > 0 {
		emit(append([]byte(nil), line...))
	}
	s.buf = s.buf[:0]
}
