package device

import (
	"fmt"
	"regexp"
	"strings"
)

// Volatile methods change LEDs only. A reboot or unplug undoes them.
// GuardMethod always allows these.
var volatileMethods = []string{
	"v.oai.thstatus",
	"v.oai.rgbcfg",
	"lights.preview",
}

// Methods refused by name. These are the names from the probe's safety rule.
var refusedExact = []string{
	"fs.write",
	"fs.writebin",
	"fs.delete",
	"fs.rm",
	"sys.bootloader",
	"sys.reset",
	"sys.reboot",
}

// A method that starts with one of these is refused.
var refusedPrefixes = []string{
	"sys.factory",
}

// A method whose name contains one of these is refused.
var refusedSubstrings = []string{
	"write",
	"erase",
	"flash",
	"boot",
	"reset",
}

// Extra rules, beyond the probe's minimum. They match a whole word of the
// name (words are split at '.', '_' and '-'). They exist because the firmware
// method list is not documented, and a name like "fs.mkdir" or "fs.rename"
// would change flash without containing a word from refusedSubstrings.
var refusedWords = []string{
	"delete", "remove", "rm", "rename", "mv", "mkdir", "rmdir", "format",
	"wipe", "truncate", "append", "save", "store", "persist", "commit",
	"dfu", "ota", "upgrade", "update", "factory",
	// Setters, state clearing, and bonding or power changes. Not on the
	// probe's minimum list; a wrong refusal only costs an --allow-write.
	"set", "put", "create", "clear", "restore", "import", "install", "apply",
	"restart", "shutdown", "poweroff", "halt", "nvs", "calibrate", "pair",
	"unpair", "bond", "unbond",
}

// Methods under "fs." that only read. Any other "fs." method is refused,
// because an unknown file-system method may change flash.
var fsReadOnly = []string{"fs.read", "fs.list", "fs.stat", "fs.exists", "fs.info", "fs.size", "fs.ls", "fs.cat"}

var methodNameRE = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)

// GuardMethod reports whether a method may change persistent device state.
// It returns refused == true with a reason when the method must not be sent
// without an explicit opt-in. A malformed name is also refused.
func GuardMethod(method string) (refused bool, reason string) {
	if !methodNameRE.MatchString(method) {
		return true, "method name is empty or has characters outside [A-Za-z0-9_.-]"
	}
	m := strings.ToLower(method)
	for _, v := range volatileMethods {
		if m == v {
			return false, ""
		}
	}
	for _, x := range refusedExact {
		if m == x {
			return true, fmt.Sprintf("%q is on the persistent-state list", x)
		}
	}
	for _, p := range refusedPrefixes {
		if strings.HasPrefix(m, p) {
			return true, fmt.Sprintf("name starts with %q", p)
		}
	}
	for _, s := range refusedSubstrings {
		if strings.Contains(m, s) {
			return true, fmt.Sprintf("name contains %q", s)
		}
	}
	words := strings.FieldsFunc(m, func(r rune) bool { return r == '.' || r == '_' || r == '-' })
	for _, w := range words {
		for _, rw := range refusedWords {
			if w == rw {
				return true, fmt.Sprintf("name has the word %q", rw)
			}
		}
	}
	if strings.HasPrefix(m, "fs.") {
		for _, ok := range fsReadOnly {
			if m == ok {
				return false, ""
			}
		}
		return true, "unknown fs.* method (only known read-only fs methods pass)"
	}
	return false, ""
}

// GuardRules returns the guard rules as printable lines, for a refusal
// message.
func GuardRules() []string {
	return []string{
		"exact: " + strings.Join(refusedExact, ", "),
		"prefix: " + strings.Join(refusedPrefixes, ", "),
		"name contains: " + strings.Join(refusedSubstrings, ", "),
		"name has the word: " + strings.Join(refusedWords, ", "),
		"any fs.* method except: " + strings.Join(fsReadOnly, ", "),
		"always allowed (volatile LED methods): " + strings.Join(volatileMethods, ", "),
	}
}
