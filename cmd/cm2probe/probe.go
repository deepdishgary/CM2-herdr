package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"time"

	"cm2-claude/internal/device"
	"cm2-claude/internal/setup"
)

const version = "0.1.0"

// Exit codes.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitDenied   = 3
	exitRefused  = 4
	defaultWait  = 3 * time.Second
	maxListenSec = 3600
)

// Env holds everything the commands use from the outside. Tests replace it.
type Env struct {
	Stdout, Stderr io.Writer
	Open           func(device.OpenOptions) (device.Transport, error)
	Access         func() device.AccessState
	RequestAccess  func() bool
	Devices        func() ([]device.DeviceInfo, error)
}

const usageText = `usage: cm2probe <command> [args]

commands:
  devices [--descriptor] [--json]   list matching HID devices (opens nothing)
  access [--request] [--json]       show the Input Monitoring state;
                                    --request raises the macOS prompt
  call <method> [params-json] [--timeout D] [--allow-write]
                                    send one request, print the result
  listen <seconds>                  print device notifications, send nothing
  backup <file> [--force]           read keymap.json from the pad into <file>
                                    and <file>.raw.json
  keymap show|apply|restore         read or change keymap.json (see "cm2probe keymap")
  version                           print the probe version

flags for call, listen and backup:
  --timeout D       time limit per call (default 3s)
  --in-id MODE      input report id: auto (default), present, absent
  --out-id MODE     output report id: present (default), absent
  --newline         end each request with a newline
  --id-first        put "id" first in each request
  --open-manager    also open the IOHIDManager before the device
`

// Run runs one command and returns the exit code.
func Run(ctx context.Context, args []string, env *Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usageText)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "devices":
		return cmdDevices(env, rest)
	case "access":
		return cmdAccess(env, rest)
	case "call":
		return cmdCall(ctx, env, rest)
	case "listen":
		return cmdListen(ctx, env, rest)
	case "backup":
		return cmdBackup(ctx, env, rest)
	case "keymap":
		return cmdKeymap(ctx, env, rest)
	case "version", "--version", "-version":
		fmt.Fprintf(env.Stdout, "cm2probe %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return exitOK
	case "help", "-h", "--help", "-help":
		fmt.Fprint(env.Stdout, usageText)
		return exitOK
	}
	fmt.Fprintf(env.Stderr, "cm2probe: unknown command %q\n\n%s", cmd, usageText)
	return exitUsage
}

// parseInterleaved parses flags that may come before, between or after the
// positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func newFlagSet(env *Env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("cm2probe "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

// ---- devices ---------------------------------------------------------------

func cmdDevices(env *Env, args []string) int {
	fs := newFlagSet(env, "devices")
	asJSON := fs.Bool("json", false, "print JSON")
	desc := fs.Bool("descriptor", false, "also print the HID report descriptor as hex")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 0 {
		return exitUsage
	}
	devs, err := env.Devices()
	if err != nil {
		fmt.Fprintf(env.Stderr, "cm2probe: devices: %v\n", err)
		return exitError
	}
	if *asJSON {
		type out struct {
			device.DeviceInfo
			Descriptor string `json:"descriptor_hex,omitempty"`
		}
		list := make([]out, 0, len(devs))
		for _, d := range devs {
			o := out{DeviceInfo: d}
			if *desc {
				o.Descriptor = hex.EncodeToString(d.Descriptor)
			}
			list = append(list, o)
		}
		writeJSON(env.Stdout, list)
	} else {
		if len(devs) == 0 {
			fmt.Fprintf(env.Stdout, "no device with vendor id 0x%04X and product id 0x%04X\n", device.VendorID, device.ProductID)
		}
		for i, d := range devs {
			fmt.Fprintf(env.Stdout, "device %d: %s / %s  serial %q  transport %s  location 0x%08X\n",
				i, d.Manufacturer, d.Product, d.Serial, d.Transport, d.LocationID)
			fmt.Fprintf(env.Stdout, "  id %04X:%04X  primary usage page 0x%04X usage 0x%X\n", d.VendorID, d.ProductID, d.PrimaryUsagePage, d.PrimaryUsage)
			for _, p := range d.UsagePairs {
				fmt.Fprintf(env.Stdout, "  collection: usage page 0x%04X usage 0x%X\n", p.Page, p.Usage)
			}
			fmt.Fprintf(env.Stdout, "  max report size: input %d, output %d, feature %d\n", d.MaxInputReportSize, d.MaxOutputReportSize, d.MaxFeatureReportSize)
			fmt.Fprintf(env.Stdout, "  vendor collection (0xFF00): %v\n", d.HasVendorCollection)
			if *desc {
				fmt.Fprintf(env.Stdout, "  descriptor (%d bytes): %s\n", len(d.Descriptor), hex.EncodeToString(d.Descriptor))
			}
		}
	}
	for _, d := range devs {
		if d.HasVendorCollection {
			return exitOK
		}
	}
	if len(devs) > 0 {
		fmt.Fprintln(env.Stderr, "cm2probe: the pad is present but no device has usage page 0xFF00")
		return exitError
	}
	return exitError
}

// ---- access ----------------------------------------------------------------

const grantHelp = `Input Monitoring is not granted to this process.
Grant it once:
  1. Open System Settings > Privacy & Security > Input Monitoring.
  2. Turn on the app that runs this command (your terminal app), or add it with +.
  3. Quit the terminal app completely and open it again.
  4. Run: cm2probe access
Or run "cm2probe access --request" yourself to raise the macOS prompt.
The grant belongs to the terminal app, not to the cm2probe file.`

func cmdAccess(env *Env, args []string) int {
	fs := newFlagSet(env, "access")
	req := fs.Bool("request", false, "raise the macOS Input Monitoring prompt (IOHIDRequestAccess)")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 0 {
		return exitUsage
	}
	if *req {
		ok := env.RequestAccess()
		fmt.Fprintf(env.Stderr, "access: IOHIDRequestAccess returned %v\n", ok)
	}
	st := env.Access()
	if *asJSON {
		writeJSON(env.Stdout, map[string]string{"input_monitoring": st.String()})
	} else {
		fmt.Fprintf(env.Stdout, "input monitoring: %s\n", st)
	}
	if st != device.AccessGranted {
		fmt.Fprintln(env.Stderr, grantHelp)
	}
	if st == device.AccessDenied {
		return exitDenied
	}
	return exitOK
}

// ---- shared flags for commands that open the pad ---------------------------

type openFlags struct {
	timeout time.Duration
	inID    string
	outID   string
	newline bool
	idFirst bool
	openMgr bool
}

func (o *openFlags) add(fs *flag.FlagSet) {
	fs.DurationVar(&o.timeout, "timeout", defaultWait, "time limit per call")
	fs.StringVar(&o.inID, "in-id", "auto", "input report id mode: auto, present, absent")
	fs.StringVar(&o.outID, "out-id", "present", "output report id mode: present, absent")
	fs.BoolVar(&o.newline, "newline", false, "end each request with a newline")
	fs.BoolVar(&o.idFirst, "id-first", false, "put id first in each request")
	fs.BoolVar(&o.openMgr, "open-manager", false, "also open the IOHIDManager before the device (as prototype/hidlisten.c does)")
}

// connect checks access, opens the pad and starts a client. On failure it
// has printed the reason; the int is the exit code.
func connect(env *Env, o *openFlags) (*device.Client, int) {
	if o.timeout <= 0 {
		fmt.Fprintln(env.Stderr, "cm2probe: --timeout must be positive")
		return nil, exitUsage
	}
	inMode, err1 := device.ParseIDMode(o.inID)
	outMode, err2 := device.ParseIDMode(o.outID)
	if err1 != nil || err2 != nil || outMode == device.IDAuto {
		fmt.Fprintln(env.Stderr, "cm2probe: bad --in-id or --out-id (in: auto|present|absent, out: present|absent)")
		return nil, exitUsage
	}
	st := env.Access()
	fmt.Fprintf(env.Stderr, "access: input monitoring %s\n", st)
	switch st {
	case device.AccessDenied:
		fmt.Fprintln(env.Stderr, grantHelp)
		return nil, exitDenied
	case device.AccessUnknown:
		fmt.Fprintln(env.Stderr, "access: not decided yet. The open below may raise the macOS prompt for your terminal app.")
		fmt.Fprintln(env.Stderr, grantHelp)
	}
	tr, err := env.Open(device.OpenOptions{InputID: inMode, OutputID: outMode, OpenManager: o.openMgr, OpenTimeout: o.timeout})
	if err != nil {
		fmt.Fprintf(env.Stderr, "cm2probe: open: %v\n", err)
		if errors.Is(err, device.ErrInputMonitoring) {
			fmt.Fprintln(env.Stderr, grantHelp)
			return nil, exitDenied
		}
		return nil, exitError
	}
	co := device.ClientOptions{Timeout: o.timeout, IDFirst: o.idFirst}
	if o.newline {
		co.Terminator = []byte("\n")
	}
	return device.NewClient(tr, co), exitOK
}

// report prints warnings about what the client saw. A nonzero foreign count
// means another program is talking to the pad.
func report(env *Env, c *device.Client) {
	s := c.Stats()
	fmt.Fprintf(env.Stderr, "stats: calls %d, replies %d, notifications %d, debug lines %d, input frames %d\n",
		s.Calls, s.Replies, s.Notifications, s.DebugLines, s.Reassembler.Frames)
	if s.Foreign > 0 {
		fmt.Fprintf(env.Stderr, "warning: %d replies had ids this tool did not send (last ids %v). "+
			"Another program is probably talking to the pad. Quit the Work Louder Input app and any other client.\n", s.Foreign, s.ForeignIDs)
	}
	if s.Late > 0 {
		fmt.Fprintf(env.Stderr, "note: %d replies came after their call timed out\n", s.Late)
	}
	if bad := s.Reassembler.Malformed + s.Reassembler.BadFrames + s.Reassembler.Overflow; bad > 0 {
		fmt.Fprintf(env.Stderr, "warning: %d damaged messages or frames (bad frames %d, malformed %d, overflow %d)\n",
			bad, s.Reassembler.BadFrames, s.Reassembler.Malformed, s.Reassembler.Overflow)
	}
	if s.TransportDropped+s.NotificationsDropped > 0 {
		fmt.Fprintf(env.Stderr, "warning: dropped %d input frames, %d notifications\n", s.TransportDropped, s.NotificationsDropped)
	}
}

func writeJSON(w io.Writer, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(w, string(b))
}

func compactJSON(raw []byte) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

// ---- call ------------------------------------------------------------------

func cmdCall(ctx context.Context, env *Env, args []string) int {
	fs := newFlagSet(env, "call")
	var of openFlags
	of.add(fs)
	allow := fs.Bool("allow-write", false, "allow methods that change persistent device state")
	full := fs.Bool("full", false, "print the whole reply message, not only the result")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) < 1 || len(pos) > 2 {
		fmt.Fprintln(env.Stderr, "usage: cm2probe call <method> [params-json] [flags]")
		return exitUsage
	}
	method := pos[0]
	var params any
	if len(pos) == 2 {
		if !json.Valid([]byte(pos[1])) {
			fmt.Fprintf(env.Stderr, "cm2probe: params are not valid JSON: %s\n", pos[1])
			return exitUsage
		}
		params = json.RawMessage(pos[1])
	}

	// The guard runs before anything touches the pad.
	if refused, why := device.GuardMethod(method); refused {
		if !*allow {
			fmt.Fprintf(env.Stderr, "cm2probe: refused %q: %s.\n", method, why)
			fmt.Fprintln(env.Stderr, "This method may change persistent device state. Refusal rules:")
			for _, r := range device.GuardRules() {
				fmt.Fprintf(env.Stderr, "  %s\n", r)
			}
			fmt.Fprintln(env.Stderr, "Add --allow-write to send it anyway.")
			return exitRefused
		}
		fmt.Fprintf(env.Stderr, "WARNING: --allow-write: sending %q (%s)\n", method, why)
	}

	c, code := connect(env, &of)
	if c == nil {
		return code
	}
	defer c.Close()
	rep, err := c.CallFull(ctx, method, params)
	defer report(env, c)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cm2probe: call %s: %v\n", method, err)
		var de *device.DeviceError
		if errors.As(err, &de) {
			fmt.Fprintf(env.Stdout, "%s\n", compactJSON(de.Raw))
		}
		if errors.Is(err, device.ErrTimeout) {
			fmt.Fprintln(env.Stderr, "hint: no reply. Quit the Work Louder Input app, check --newline, --in-id absent, --out-id absent and --open-manager, or raise --timeout.")
		}
		return exitError
	}
	if *full {
		fmt.Fprintln(env.Stdout, compactJSON(rep.Raw))
	} else {
		fmt.Fprintln(env.Stdout, compactJSON(rep.Result))
	}
	return exitOK
}

// ---- listen ----------------------------------------------------------------

func cmdListen(ctx context.Context, env *Env, args []string) int {
	fs := newFlagSet(env, "listen")
	var of openFlags
	of.add(fs)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(env.Stderr, "usage: cm2probe listen <seconds>")
		return exitUsage
	}
	secs, err := strconv.ParseFloat(pos[0], 64)
	if err != nil || secs <= 0 || secs > maxListenSec {
		fmt.Fprintf(env.Stderr, "cm2probe: seconds must be a number from 0 to %d\n", maxListenSec)
		return exitUsage
	}
	c, code := connect(env, &of)
	if c == nil {
		return code
	}
	defer c.Close()
	defer report(env, c)

	start := time.Now()
	timer := time.NewTimer(time.Duration(secs * float64(time.Second)))
	defer timer.Stop()
	fmt.Fprintf(env.Stderr, "listening for %gs (this sends nothing to the pad)\n", secs)
	n := c.Notifications()
	d := c.Debug()
	for {
		select {
		case m, ok := <-n:
			if !ok {
				fmt.Fprintln(env.Stderr, "cm2probe: the pad connection ended")
				return exitError
			}
			fmt.Fprintf(env.Stdout, "+%.3fs notify %s %s\n", time.Since(start).Seconds(), m.Method, compactJSON(m.Params))
		case l, ok := <-d:
			if !ok {
				d = nil
				continue
			}
			fmt.Fprintf(env.Stdout, "+%.3fs debug %s\n", time.Since(start).Seconds(), l)
		case <-timer.C:
			return exitOK
		case <-ctx.Done():
			fmt.Fprintln(env.Stderr, "interrupted")
			return exitOK
		}
	}
}

// ---- backup ----------------------------------------------------------------

func cmdBackup(ctx context.Context, env *Env, args []string) int {
	fs := newFlagSet(env, "backup")
	var of openFlags
	of.add(fs)
	force := fs.Bool("force", false, "overwrite existing backup files")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(env.Stderr, "usage: cm2probe backup <file> [--force]")
		return exitUsage
	}
	path := pos[0]
	rawPath := path + ".raw.json"
	if !*force {
		for _, p := range []string{path, rawPath, path + ".invalid"} {
			if _, err := os.Lstat(p); err == nil {
				fmt.Fprintf(env.Stderr, "cm2probe: %s exists. Use --force to overwrite, or choose another name.\n", p)
				return exitError
			}
		}
	}

	c, code := connect(env, &of)
	if c == nil {
		return code
	}
	defer c.Close()
	defer report(env, c)

	// Same request as prototype/device.js readFile: fs.read {"file":"keymap.json"}.
	// device.js reads the whole file in one call and does not chunk.
	rep, err := c.CallFull(ctx, "fs.read", map[string]string{"file": "keymap.json"})
	if err != nil {
		fmt.Fprintf(env.Stderr, "cm2probe: backup: fs.read: %v\n", err)
		if errors.Is(err, device.ErrTimeout) {
			fmt.Fprintln(env.Stderr, "hint: a large keymap takes many frames. Try --timeout 10s.")
		}
		return exitError
	}
	if err := setup.WriteNew(rawPath, append(append([]byte(nil), rep.Raw...), '\n'), *force); err != nil {
		fmt.Fprintf(env.Stderr, "cm2probe: backup: write %s: %v\n", rawPath, err)
		return exitError
	}
	decoded, err := setup.DecodeFSRead(rep.Result)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cm2probe: backup: %v (raw reply saved to %s)\n", err, rawPath)
		return exitError
	}
	if len(bytes.TrimSpace(decoded)) == 0 {
		fmt.Fprintf(env.Stderr, "cm2probe: backup: the pad returned an empty file (raw reply saved to %s)\n", rawPath)
		return exitError
	}
	target := path
	if !json.Valid(decoded) {
		target = path + ".invalid"
		fmt.Fprintf(env.Stderr, "warning: the decoded keymap is not valid JSON. It is saved as %s, not %s.\n", target, path)
	}
	if err := setup.WriteNew(target, decoded, *force); err != nil {
		fmt.Fprintf(env.Stderr, "cm2probe: backup: write %s: %v\n", target, err)
		return exitError
	}
	sum := sha256.Sum256(decoded)
	fmt.Fprintf(env.Stdout, "backup: %s (%d bytes, sha256 %s)\n", target, len(decoded), hex.EncodeToString(sum[:]))
	fmt.Fprintf(env.Stdout, "raw reply: %s\n", rawPath)
	if target != path {
		return exitError
	}
	return exitOK
}
