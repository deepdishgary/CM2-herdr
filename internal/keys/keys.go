// Package keys turns pad notifications into actions.
// See docs/plans/2026-10-23-cm2d-daemon.md section 3.5.
package keys

import (
	"encoding/json"
	"time"

	"cm2-claude/internal/device"
)

// ActionKind says what a key press means.
type ActionKind int

const (
	SelectFocus ActionKind = iota + 1 // session key: select and focus Slot
	Move                              // dial turn: move the selection by Delta
	Approve                           // APPROVE (wide key)
	Deny                              // DENY
)

// Action is one decoded key event.
type Action struct {
	Kind  ActionKind
	Slot  int // SelectFocus
	Delta int // Move: +1 or -1
}

// Pad events.
const (
	hidMethod = "v.oai.hid"
	actPress  = 1
	actTurn   = 2

	// The wide APPROVE key is two switches; one press reports both.
	approveDebounce = 80 * time.Millisecond
)

// Decoder decodes notifications. It remembers the last APPROVE press to
// merge the double report of the wide key. Not safe for concurrent use.
type Decoder struct {
	lastApprove time.Time
	haveApprove bool
}

type hidParams struct {
	K   string `json:"k"`
	Act *int   `json:"act"`
}

// Decode returns the action for n, or false when n means nothing: another
// method, a key release, an unknown key, or bad JSON. A press of AG10 or
// AG11 within 80 ms of the last accepted APPROVE press is the same press.
func (d *Decoder) Decode(n device.Notification, now time.Time) (Action, bool) {
	if n.Method != hidMethod {
		return Action{}, false
	}
	var p hidParams
	if err := json.Unmarshal(n.Params, &p); err != nil || p.Act == nil {
		return Action{}, false
	}
	switch p.K {
	case "ENC_CW", "ENC_CC":
		if *p.Act != actTurn {
			return Action{}, false
		}
		if p.K == "ENC_CW" {
			return Action{Kind: Move, Delta: 1}, true
		}
		return Action{Kind: Move, Delta: -1}, true
	case "AG10", "AG11":
		if *p.Act != actPress {
			return Action{}, false
		}
		if d.haveApprove {
			if gap := now.Sub(d.lastApprove); gap >= 0 && gap < approveDebounce {
				return Action{}, false
			}
		}
		d.lastApprove, d.haveApprove = now, true
		return Action{Kind: Approve}, true
	case "AG12":
		if *p.Act != actPress {
			return Action{}, false
		}
		return Action{Kind: Deny}, true
	}
	if slot, ok := sessionSlot(p.K); ok {
		if *p.Act != actPress {
			return Action{}, false
		}
		return Action{Kind: SelectFocus, Slot: slot}, true
	}
	return Action{}, false
}

// sessionSlot parses "AG00".."AG09".
func sessionSlot(k string) (int, bool) {
	if len(k) != 4 || k[0] != 'A' || k[1] != 'G' || k[2] != '0' || k[3] < '0' || k[3] > '9' {
		return 0, false
	}
	return int(k[3] - '0'), true
}
