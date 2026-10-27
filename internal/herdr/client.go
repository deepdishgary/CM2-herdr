package herdr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultDialTimeout = 2 * time.Second
	defaultIOTimeout   = 5 * time.Second
	defaultMaxLine     = 8 << 20 // a snapshot can be large
)

// socketPathLimit is the size of sun_path on goos, which includes the closing
// NUL byte: a path must be shorter than this. It is 104 on macOS and the BSDs
// and 108 on Linux. It is 0 on other systems, where the client does no check.
func socketPathLimit(goos string) int {
	switch goos {
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		return 104
	case "linux":
		return 108
	}
	return 0
}

// Client talks to the Herdr socket. Every call opens its own connection
// (Herdr serves one request per connection) and closes it before it returns.
type Client struct {
	SocketPath  string
	DialTimeout time.Duration // default 2s
	IOTimeout   time.Duration // read/write deadline per call, default 5s
	MaxLine     int           // longest reply line in bytes, default 8 MB

	dial func(ctx context.Context, network, addr string) (net.Conn, error) // test hook
	seq  atomic.Uint64
}

// NewClient returns a client for socketPath. An empty path is resolved with
// ResolveSocketPath.
func NewClient(socketPath string) *Client {
	return NewClientSession(socketPath, "")
}

// NewClientSession returns a client for socketPath or, if that is empty, for
// the named Herdr session. See ResolveSocketPathSession.
func NewClientSession(socketPath, session string) *Client {
	return &Client{SocketPath: ResolveSocketPathSession(socketPath, session)}
}

// ResolveSocketPath picks the socket path with no session name. See
// ResolveSocketPathSession.
func ResolveSocketPath(explicit string) string {
	return ResolveSocketPathSession(explicit, "")
}

// ResolveSocketPathSession picks the socket path. In order:
//
//  1. the explicit path
//  2. the session argument: <base>/herdr/sessions/<session>/herdr.sock
//  3. $HERDR_SOCKET_PATH
//  4. $HERDR_SESSION, resolved like the session argument
//  5. <base>/herdr/herdr.sock
//
// <base> is $XDG_CONFIG_HOME if it is set, else ~/.config. The result is ""
// if no base can be found.
func ResolveSocketPathSession(explicit, session string) string {
	if explicit != "" {
		return explicit
	}
	if session != "" {
		return sessionSocketPath(session)
	}
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	if name := os.Getenv("HERDR_SESSION"); name != "" {
		return sessionSocketPath(name)
	}
	if base := configBase(); base != "" {
		return filepath.Join(base, "herdr", "herdr.sock")
	}
	return ""
}

func sessionSocketPath(name string) string {
	if base := configBase(); base != "" {
		return filepath.Join(base, "herdr", "sessions", name, "herdr.sock")
	}
	return ""
}

// configBase is $XDG_CONFIG_HOME if set, else ~/.config, else "".
func configBase() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".config")
	}
	return ""
}

func (c *Client) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return defaultDialTimeout
}

func (c *Client) ioTimeout() time.Duration {
	if c.IOTimeout > 0 {
		return c.IOTimeout
	}
	return defaultIOTimeout
}

func (c *Client) maxLine() int {
	if c.MaxLine > 0 {
		return c.MaxLine
	}
	return defaultMaxLine
}

func (c *Client) nextID() string {
	return fmt.Sprintf("cm2d-%d", c.seq.Add(1))
}

// connect dials the socket. A missing or refusing socket maps to ErrNotRunning.
func (c *Client) connect(ctx context.Context) (net.Conn, error) {
	if c.SocketPath == "" {
		return nil, errors.New("herdr: cannot work out the socket path")
	}
	if limit := socketPathLimit(runtime.GOOS); limit > 0 && len(c.SocketPath) >= limit {
		return nil, fmt.Errorf("herdr: socket path is too long (%d bytes, limit %d): %s", len(c.SocketPath), limit-1, c.SocketPath)
	}
	dial := c.dial
	if dial == nil {
		d := &net.Dialer{Timeout: c.dialTimeout()}
		dial = d.DialContext
	}
	conn, err := dial(ctx, "unix", c.SocketPath)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w (socket %s)", ErrNotRunning, c.SocketPath)
		}
		return nil, fmt.Errorf("herdr: connect %s: %w", c.SocketPath, err)
	}
	return conn, nil
}

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type reply struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *APIError       `json:"error"`
	Event  string          `json:"event"`
}

// readLine reads one line of at most max bytes (without the newline).
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max+1 {
			return nil, errors.New("herdr: reply line too long")
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(buf) > 0:
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// parseReply turns one reply line into a result or an *APIError.
func parseReply(id string, line []byte) (json.RawMessage, error) {
	var rep reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return nil, fmt.Errorf("herdr: bad reply: %w", err)
	}
	if rep.Error != nil {
		return nil, rep.Error
	}
	if rep.ID != "" && rep.ID != id {
		return nil, fmt.Errorf("herdr: reply id %q does not match request %q", rep.ID, id)
	}
	if len(rep.Result) == 0 {
		return nil, errors.New("herdr: reply has no result")
	}
	return rep.Result, nil
}

// call sends one request on a fresh connection and returns the result.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(c.ioTimeout()))
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	id := c.nextID()
	b, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, ioErr(ctx, err)
	}
	line, err := readLine(bufio.NewReaderSize(conn, 64<<10), c.maxLine())
	if err != nil {
		return nil, ioErr(ctx, err)
	}
	return parseReply(id, line)
}

// ioErr reports the context error when the context caused the I/O error.
func ioErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Ping returns the Herdr version and protocol number.
func (c *Client) Ping(ctx context.Context) (version string, protocol int, err error) {
	res, err := c.call(ctx, "ping", struct{}{})
	if err != nil {
		return "", 0, err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(res, &m) != nil {
		return "", 0, errors.New("herdr: malformed ping reply")
	}
	return rawString(m, "version"), int(rawUint(m, "protocol")), nil
}

// Snapshot reads the whole session with session.snapshot. If Herdr is not
// running it returns Snapshot{Available: false} and a nil error.
func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	res, err := c.call(ctx, "session.snapshot", struct{}{})
	if err != nil {
		if errors.Is(err, ErrNotRunning) {
			return Snapshot{}, nil
		}
		return Snapshot{}, err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(res, &m) != nil || len(m["snapshot"]) == 0 {
		return Snapshot{}, errors.New("herdr: snapshot reply has no snapshot")
	}
	return decodeSnapshot(m["snapshot"])
}

// Focus calls agent.focus. The target is a pane id or an agent name. This
// marks the agent seen and moves attached TUI clients. It does not raise the
// terminal window.
func (c *Client) Focus(ctx context.Context, paneID string) error {
	if strings.TrimSpace(paneID) == "" {
		return errors.New("herdr: focus needs a pane id")
	}
	_, err := c.call(ctx, "agent.focus", map[string]string{"target": paneID})
	return err
}
