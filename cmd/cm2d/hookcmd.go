package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// forwardTimeout is the longest the SessionStart forwarder may take, stdin
// read and POST together. Claude Code waits for the hook.
const forwardTimeout = time.Second

// maxStdin caps what the forwarder reads.
const maxStdin = 1 << 20

func cmdHook(ctx context.Context, args []string, e *env) int {
	if len(args) == 0 || args[0] != "session-start" {
		fmt.Fprintln(e.Stderr, "usage: cm2d hook session-start [--port N]")
		return exitUsage
	}
	// From here on the command prints nothing and exits 0, whatever happens:
	// a failing hook must never disturb a Claude Code session.
	fs := newFlagSet(e, "hook session-start")
	fs.SetOutput(io.Discard)
	port := fs.Int("port", defaultPort(e), "")
	if err := fs.Parse(args[1:]); err != nil || *port < 1 || *port > 65535 {
		return exitOK
	}
	forwardSessionStart(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)), e)
	return exitOK
}

// forwardSessionStart reads the hook JSON from stdin and posts it, with the
// facts only a command hook can see, to the daemon.
func forwardSessionStart(ctx context.Context, addr string, e *env) {
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()

	got := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(e.Stdin, maxStdin))
		got <- b
	}()
	var payload []byte
	select {
	case b := <-got:
		payload = bytes.TrimSpace(b)
	case <-ctx.Done():
		return
	}
	if !json.Valid(payload) {
		return
	}
	body, err := json.Marshal(struct {
		Payload    json.RawMessage `json:"payload"`
		Attended   string          `json:"attended"`
		Entrypoint string          `json:"entrypoint"`
		HerdrPane  string          `json:"herdr_pane"`
		HostID     string          `json:"host_session_id"`
	}{
		Payload:    payload,
		Attended:   e.Getenv("CLAUDE_CODE_SESSION_ATTENDED"),
		Entrypoint: e.Getenv("CLAUDE_CODE_ENTRYPOINT"),
		HerdrPane:  e.Getenv("HERDR_PANE_ID"),
		HostID:     e.Getenv("CLAUDE_CODE_HOST_SESSION_ID"),
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/cm2d/v1/session", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}
