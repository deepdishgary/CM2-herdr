package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"sync"
	"time"
)

// globalEvents are the events that need no pane id. The last four are an
// addition to the spec list: they let the watcher see focus moves and pane
// updates at once. pane.updated is chatty (titles change often), but a refresh
// is debounced and publishes only on a meaningful change.
var globalEvents = []string{
	"pane.created", "pane.closed", "pane.exited", "pane.moved",
	"pane.agent_detected", "pane.focused", "tab.focused", "workspace.focused",
	"pane.updated",
}

// subscription is an open events.subscribe connection.
type subscription struct {
	conn    net.Conn
	r       *bufio.Reader
	maxLine int
	stop    func() bool
	once    sync.Once
}

// subscriptionSpecs builds the subscription list for a set of agent panes.
func subscriptionSpecs(panes []string) []map[string]string {
	specs := make([]map[string]string, 0, len(globalEvents)+len(panes))
	for _, t := range globalEvents {
		specs = append(specs, map[string]string{"type": t})
	}
	for _, p := range panes {
		specs = append(specs, map[string]string{"type": "pane.agent_status_changed", "pane_id": p})
	}
	return specs
}

// paneSet returns the sorted, unique pane ids of the agents.
func paneSet(s Snapshot) []string {
	seen := make(map[string]struct{}, len(s.Agents))
	out := make([]string, 0, len(s.Agents))
	for _, a := range s.Agents {
		if _, dup := seen[a.PaneID]; !dup {
			seen[a.PaneID] = struct{}{}
			out = append(out, a.PaneID)
		}
	}
	sort.Strings(out)
	return out
}

// subscribe opens a subscription connection and waits for subscription_started.
// The connection closes when ctx ends or when close is called. A rejection
// returns an *APIError.
func (c *Client) subscribe(ctx context.Context, panes []string) (*subscription, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	s := &subscription{conn: conn, r: bufio.NewReaderSize(conn, 64<<10), maxLine: c.maxLine()}
	s.stop = context.AfterFunc(ctx, func() { conn.Close() })

	id := c.nextID()
	conn.SetDeadline(time.Now().Add(c.ioTimeout()))
	b, err := json.Marshal(request{ID: id, Method: "events.subscribe",
		Params: map[string]any{"subscriptions": subscriptionSpecs(panes)}})
	if err == nil {
		_, err = conn.Write(append(b, '\n'))
	}
	var line []byte
	if err == nil {
		line, err = readLine(s.r, s.maxLine)
	}
	if err != nil {
		s.close()
		return nil, ioErr(ctx, err)
	}
	res, err := parseReply(id, line)
	if err != nil {
		s.close()
		return nil, err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(res, &m) != nil || rawString(m, "type") != "subscription_started" {
		s.close()
		return nil, errors.New("herdr: subscribe was not acknowledged")
	}
	conn.SetDeadline(time.Time{}) // the stream may stay quiet for a long time
	return s, nil
}

// next blocks until the next event and returns its kind. An error line (for
// example events_lost) returns an *APIError. Lines that are not events or
// errors are skipped.
func (s *subscription) next() (string, error) {
	for {
		line, err := readLine(s.r, s.maxLine)
		if err != nil {
			return "", err
		}
		var rep reply
		if json.Unmarshal(line, &rep) != nil {
			continue
		}
		if rep.Error != nil {
			return "", rep.Error
		}
		if rep.Event != "" {
			return rep.Event, nil
		}
	}
}

// close closes the connection. It is safe to call more than once.
func (s *subscription) close() {
	s.once.Do(func() {
		s.stop()
		s.conn.Close()
	})
}
