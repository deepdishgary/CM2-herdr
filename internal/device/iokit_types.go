package device

import (
	"errors"
	"fmt"
	"time"
)

// Pad identity.
const (
	VendorID  = 0x303A
	ProductID = 0x8298
	// VendorUsagePage is the usage page of the vendor collection.
	VendorUsagePage = 0xFF00
)

// Transport moves frames to and from the pad. Frames are FrameSize bytes
// with the report id first, on both sides. Implementations convert at the OS
// boundary.
type Transport interface {
	// Write sends one frame. It blocks until the OS accepts it.
	Write(frame []byte) error
	// Reports yields input frames. It is closed when the transport ends
	// (Close, unplug or fatal error).
	Reports() <-chan []byte
	// Close stops the transport and closes Reports. It is safe to call
	// more than once.
	Close()
}

// DropCounter is implemented by transports that can drop input frames when
// the consumer is too slow.
type DropCounter interface{ DroppedReports() uint64 }

// IDMode says whether a buffer includes the report id byte.
type IDMode int

const (
	// IDAuto (input only) detects the id: if the first byte equals the
	// report id the buffer has it, otherwise it is added. Channels are 1 and
	// 2, never 6, so the check is unambiguous.
	IDAuto IDMode = iota
	// IDPresent: the buffer has the id as its first byte (64-byte frame).
	IDPresent
	// IDAbsent: the buffer has no id byte (63 bytes); the id is passed to the
	// OS separately.
	IDAbsent
)

func (m IDMode) String() string {
	switch m {
	case IDAuto:
		return "auto"
	case IDPresent:
		return "present"
	case IDAbsent:
		return "absent"
	}
	return fmt.Sprintf("IDMode(%d)", int(m))
}

// ParseIDMode parses "auto", "present" or "absent".
func ParseIDMode(s string) (IDMode, error) {
	switch s {
	case "auto":
		return IDAuto, nil
	case "present":
		return IDPresent, nil
	case "absent":
		return IDAbsent, nil
	}
	return 0, fmt.Errorf("bad id mode %q (want auto, present or absent)", s)
}

// OpenOptions configure Open. The zero value is the default.
//
// The two ID modes exist because the sources disagree or are silent on how
// macOS shows the report id. hidapi (from memory) includes the id in the
// buffer for numbered reports, both ways. design.md says "63-byte reports",
// which fits the report data without the id. If the pad does not answer,
// try the other mode: probe flags --in-id and --out-id.
type OpenOptions struct {
	// InputID: does the OS input callback buffer include the report id?
	// Default IDAuto.
	InputID IDMode
	// OutputID: does the IOHIDDeviceSetReport buffer include the report id?
	// Only IDPresent (default, 64 bytes) and IDAbsent (63 bytes) apply.
	OutputID IDMode
	// OpenManager also calls IOHIDManagerOpen (shared) before the device
	// open. Default false: the manager only matches, and IOHIDDeviceOpen is
	// the only open. prototype/hidlisten.c, which is verified on this Mac,
	// calls IOHIDManagerOpen first (with a match-all dictionary). If the
	// device open fails in a way that looks like a missing manager open, try
	// this: probe flag --open-manager.
	OpenManager bool
	// OpenTimeout bounds Open. Default 3 s.
	OpenTimeout time.Duration
	// InputBuffer is the capacity of the Reports channel. Default 1024.
	InputBuffer int
}

func (o OpenOptions) withDefaults() OpenOptions {
	if o.OutputID == IDAuto {
		o.OutputID = IDPresent
	}
	if o.OpenTimeout <= 0 {
		o.OpenTimeout = 3 * time.Second
	}
	if o.InputBuffer <= 0 {
		o.InputBuffer = 1024
	}
	return o
}

// NormalizeInput turns an OS input report into a FrameSize frame with the
// report id first. It returns nil when the report belongs to another
// collection (another report id), so keyboard and mouse reports of the
// composite device are ignored.
//
// osID is the report id the OS passed to the callback. data is the buffer.
func NormalizeInput(mode IDMode, osID uint32, data []byte) []byte {
	if osID != 0 && osID != uint32(ReportID) {
		return nil // another collection (keyboard, mouse, consumer)
	}
	has := false
	switch mode {
	case IDPresent:
		has = true
	case IDAbsent:
		has = false
	default: // IDAuto
		has = len(data) > 0 && data[0] == ReportID
	}
	body := data
	if has {
		if len(data) == 0 || data[0] != ReportID {
			return nil
		}
	} else {
		if osID != uint32(ReportID) {
			return nil // no id byte and the OS did not say 6: not ours
		}
		body = append([]byte{ReportID}, data...)
	}
	frame := make([]byte, FrameSize)
	copy(frame, body)
	return frame
}

// AccessState is the Input Monitoring state for this process.
type AccessState int

const (
	AccessUnknown AccessState = iota
	AccessGranted
	AccessDenied
)

func (a AccessState) String() string {
	switch a {
	case AccessGranted:
		return "granted"
	case AccessDenied:
		return "denied"
	}
	return "unknown"
}

// UsagePair is one HID top-level collection of a device.
type UsagePair struct {
	Page  int `json:"page"`
	Usage int `json:"usage"`
}

// DeviceInfo describes one matching IOHIDDevice. It is read without opening
// the device.
type DeviceInfo struct {
	VendorID         int         `json:"vendor_id"`
	ProductID        int         `json:"product_id"`
	Product          string      `json:"product"`
	Manufacturer     string      `json:"manufacturer"`
	Serial           string      `json:"serial"`
	Transport        string      `json:"transport"`
	LocationID       uint32      `json:"location_id"`
	PrimaryUsagePage int         `json:"primary_usage_page"`
	PrimaryUsage     int         `json:"primary_usage"`
	UsagePairs       []UsagePair `json:"usage_pairs"`
	// HasVendorCollection is true if the primary usage page or any usage
	// pair is VendorUsagePage.
	HasVendorCollection  bool `json:"has_vendor_collection"`
	MaxInputReportSize   int  `json:"max_input_report_size"`
	MaxOutputReportSize  int  `json:"max_output_report_size"`
	MaxFeatureReportSize int  `json:"max_feature_report_size"`
	// Descriptor is the HID report descriptor (possibly cut at 1024 bytes).
	Descriptor []byte `json:"-"`
}

// Errors from Open and the transport.
var (
	// ErrUnsupported: this build has no HID backend (not darwin, or no cgo).
	ErrUnsupported = errors.New("device: HID backend not available in this build (needs darwin and cgo)")
	// ErrNoDevice: no IOHIDDevice matches the pad's vendor and product id.
	ErrNoDevice = errors.New("device: no Creator Micro 2 found (USB 303a:8298)")
	// ErrNoVendorCollection: the pad is present but no device has usage page 0xFF00.
	ErrNoVendorCollection = errors.New("device: Creator Micro 2 found, but it has no vendor collection (usage page 0xFF00)")
	// ErrInputMonitoring: macOS refused the open with kIOReturnNotPermitted.
	ErrInputMonitoring = errors.New("device: Input Monitoring not granted to this process (IOHIDDeviceOpen returned kIOReturnNotPermitted)")
	// ErrDeviceRemoved: the pad was unplugged while open.
	ErrDeviceRemoved = errors.New("device: Creator Micro 2 was removed")
)
