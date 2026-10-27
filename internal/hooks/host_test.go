package hooks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// hostCases are header or body values and what the server keeps.
var hostCases = []struct {
	name, in, want string
}{
	{"valid", "local_1d9854de-0000-4000-8000-123456789abc", "local_1d9854de-0000-4000-8000-123456789abc"},
	{"valid short", "local_a", "local_a"},
	{"missing", "", ""},
	{"literal variable", "$CLAUDE_CODE_HOST_SESSION_ID", ""},
	{"literal braces", "${CLAUDE_CODE_HOST_SESSION_ID}", ""},
	{"wrong prefix", "abc123", ""},
	{"prefix only", "local_", ""},
	{"traversal", "local_../x", ""},
	{"slash", "local_a/b", ""},
	{"space", "local_a b", ""},
	{"too long", "local_" + strings.Repeat("a", 65), ""},
	{"query chars", "local_a?x=1", ""},
	{"url", "claude://resume?session=local_a", ""},
}

func TestEventHostHeader(t *testing.T) {
	r := newRig(t, nil)
	for _, c := range hostCases {
		t.Run(c.name, func(t *testing.T) {
			hdr := map[string]string{}
			if c.in != "" {
				hdr[HostSessionHeader] = c.in
			}
			r.post("/cm2d/v1/event", `{"session_id":"s1","hook_event_name":"Stop"}`, hdr)
			if ev := r.next(); ev.HostSession != c.want {
				t.Fatalf("host = %q, want %q", ev.HostSession, c.want)
			}
		})
	}
}

func TestPermissionHostHeader(t *testing.T) {
	r := newRig(t, nil)
	for _, c := range hostCases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hdr := map[string]string{}
			if c.in != "" {
				hdr[HostSessionHeader] = c.in
			}
			rep := r.permAsyncHdr(ctx, permBody, hdr)
			ev := r.next()
			if ev.HostSession != c.want {
				t.Fatalf("host = %q, want %q", ev.HostSession, c.want)
			}
			r.s.Resolve(ev.RequestID, Pass)
			r.next() // PermissionReleased
			r.waitReply(rep)
		})
	}
}

func TestQuestionPermissionHostHeader(t *testing.T) {
	r := newRig(t, nil)
	body := `{"session_id":"s1","cwd":"/w","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion"}`
	r.post("/cm2d/v1/permission", body, map[string]string{HostSessionHeader: "local_q1"})
	if ev := r.next(); ev.HostSession != "local_q1" || !ev.IsQuestion {
		t.Fatalf("event = %+v", ev)
	}
}

func TestSessionHostBody(t *testing.T) {
	r := newRig(t, nil)
	for _, c := range hostCases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]any{
				"payload":         json.RawMessage(`{"session_id":"s1","cwd":"/w","hook_event_name":"SessionStart","source":"startup"}`),
				"attended":        "1",
				"entrypoint":      "claude-desktop",
				"host_session_id": c.in,
			}
			b, _ := json.Marshal(env)
			r.post("/cm2d/v1/session", string(b), nil)
			if ev := r.next(); ev.HostSession != c.want {
				t.Fatalf("host = %q, want %q", ev.HostSession, c.want)
			}
		})
	}
}

func TestSessionHostBodyAbsent(t *testing.T) {
	r := newRig(t, nil)
	r.post("/cm2d/v1/session", `{"payload":{"session_id":"s1","hook_event_name":"SessionStart"},"attended":"1"}`, nil)
	if ev := r.next(); ev.HostSession != "" {
		t.Fatalf("host = %q", ev.HostSession)
	}
}

// The host id in a body of /event or /permission is not trusted: only the header counts.
func TestEventHostBodyFieldIgnored(t *testing.T) {
	r := newRig(t, nil)
	r.post("/cm2d/v1/event", `{"session_id":"s1","hook_event_name":"Stop","host_session_id":"local_body"}`, nil)
	if ev := r.next(); ev.HostSession != "" {
		t.Fatalf("host = %q", ev.HostSession)
	}
}
