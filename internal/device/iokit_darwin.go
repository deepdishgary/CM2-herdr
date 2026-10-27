//go:build darwin && cgo

package device

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <stdlib.h>
#include "cm2hid.h"
*/
import "C"

import (
	"fmt"
	"runtime"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// closeWait bounds how long Close waits for the run loop thread.
const closeWait = 2 * time.Second

// IOReturn values this file maps. The numbers come from IOKit/IOReturn.h.
const (
	kIOReturnNotPrivileged   = 0xe00002c1
	kIOReturnExclusiveAccess = 0xe00002c5
	kIOReturnNotPermitted    = 0xe00002e2
	kIOReturnNoDevice        = 0xe00002c0
)

func ioError(op string, rc uint32) error {
	switch rc {
	case kIOReturnNotPermitted:
		return fmt.Errorf("%w (%s, 0x%08x)", ErrInputMonitoring, op, rc)
	case kIOReturnExclusiveAccess:
		return fmt.Errorf("device: %s: another process holds the device exclusively (kIOReturnExclusiveAccess 0x%08x)", op, rc)
	case kIOReturnNotPrivileged:
		return fmt.Errorf("device: %s: not privileged (kIOReturnNotPrivileged 0x%08x); the open must be shared, not seized", op, rc)
	case kIOReturnNoDevice:
		return fmt.Errorf("%w (%s, 0x%08x)", ErrDeviceRemoved, op, rc)
	case C.CM2_ERR_NO_DEVICE:
		return ErrNoDevice
	case C.CM2_ERR_NO_VENDOR:
		return ErrNoVendorCollection
	}
	return fmt.Errorf("device: %s failed: IOReturn 0x%08x", op, rc)
}

// Access reports the Input Monitoring state of this process, from
// IOHIDCheckAccess(kIOHIDRequestTypeListenEvent). It never shows a prompt.
//
// Input Monitoring is granted per app. A command line tool run from a
// terminal uses the grant of that terminal app (the "responsible process").
func Access() AccessState {
	switch C.cm2_access() {
	case 0:
		return AccessGranted
	case 1:
		return AccessDenied
	}
	return AccessUnknown
}

// RequestAccess calls IOHIDRequestAccess(kIOHIDRequestTypeListenEvent). On
// the first call this shows the macOS Input Monitoring prompt for the
// responsible app. Only the probe's `access --request` calls it.
func RequestAccess() bool { return C.cm2_request_access() != 0 }

// Devices lists the IOHIDDevices that match the pad's vendor and product id.
// It reads properties only and opens nothing.
func Devices() ([]DeviceInfo, error) {
	var arr [16]C.cm2_info
	var n C.int
	if rc := uint32(C.cm2_enumerate(VendorID, ProductID, &arr[0], C.int(len(arr)), &n)); rc != 0 {
		return nil, ioError("enumerate", rc)
	}
	out := make([]DeviceInfo, 0, int(n))
	for i := 0; i < int(n); i++ {
		out = append(out, goInfo(&arr[i]))
	}
	return out, nil
}

func goInfo(c *C.cm2_info) DeviceInfo {
	d := DeviceInfo{
		VendorID:             int(c.vendor_id),
		ProductID:            int(c.product_id),
		Product:              C.GoString(&c.product[0]),
		Manufacturer:         C.GoString(&c.manufacturer[0]),
		Serial:               C.GoString(&c.serial[0]),
		Transport:            C.GoString(&c.transport[0]),
		LocationID:           uint32(c.location_id),
		PrimaryUsagePage:     int(c.primary_page),
		PrimaryUsage:         int(c.primary_usage),
		MaxInputReportSize:   int(c.max_in),
		MaxOutputReportSize:  int(c.max_out),
		MaxFeatureReportSize: int(c.max_feature),
		Descriptor:           C.GoBytes(unsafe.Pointer(&c.desc[0]), c.desc_len),
	}
	if d.PrimaryUsagePage == VendorUsagePage {
		d.HasVendorCollection = true
	}
	for k := 0; k < int(c.pair_count); k++ {
		p := UsagePair{Page: int(c.pairs[k][0]), Usage: int(c.pairs[k][1])}
		d.UsagePairs = append(d.UsagePairs, p)
		if p.Page == VendorUsagePage {
			d.HasVendorCollection = true
		}
	}
	return d
}

// hidTransport is the macOS Transport. One goroutine, locked to its OS
// thread, owns the IOHIDDevice and its run loop. Callbacks run on that
// thread and only push into a channel.
type hidTransport struct {
	opts    OpenOptions
	handle  cgo.Handle
	reports chan []byte
	done    chan struct{}

	stopReq atomic.Bool
	dropped atomic.Uint64
	removed atomic.Bool

	mu   sync.RWMutex // guards d and dead; Write holds RLock
	d    *C.cm2_dev
	dead bool

	closeOnce sync.Once
}

// Open opens the pad with default options. See OpenWith.
func Open() (Transport, error) { return OpenWith(OpenOptions{}) }

// OpenWith opens the pad's vendor collection.
//
//   - IOHIDManager matches vendor 0x303A, product 0x8298.
//   - It chooses the device whose PrimaryUsagePage is 0xFF00, or else whose
//     DeviceUsagePairs contain 0xFF00 (the pad is one composite device).
//   - IOHIDDeviceOpen uses kIOHIDOptionsTypeNone: shared, not seized.
//   - kIOReturnNotPermitted becomes ErrInputMonitoring.
func OpenWith(o OpenOptions) (Transport, error) {
	o = o.withDefaults()
	t := &hidTransport{
		opts:    o,
		reports: make(chan []byte, o.InputBuffer),
		done:    make(chan struct{}),
	}
	t.handle = cgo.NewHandle(t)
	ready := make(chan error, 1)
	go t.loop(ready)
	select {
	case err := <-ready:
		if err != nil {
			return nil, err
		}
		return t, nil
	case <-time.After(o.OpenTimeout):
		t.stopReq.Store(true) // the loop closes the device when the open returns
		return nil, fmt.Errorf("device: opening the pad timed out after %s", o.OpenTimeout)
	}
}

// loop owns the thread. It is never unlocked: when the goroutine returns, Go
// ends the thread, so no run loop state leaks into a reused thread.
func (t *hidTransport) loop(ready chan<- error) {
	runtime.LockOSThread()
	var d *C.cm2_dev
	var info C.cm2_info
	openMgr := C.int(0)
	if t.opts.OpenManager {
		openMgr = 1
	}
	rc := uint32(C.cm2_open(VendorID, ProductID, VendorUsagePage, openMgr, C.uintptr_t(t.handle), &d, &info))
	if rc != 0 {
		ready <- ioError("IOHIDDeviceOpen", rc)
		close(t.reports)
		t.handle.Delete()
		close(t.done)
		return
	}
	t.mu.Lock()
	t.d = d
	t.mu.Unlock()
	if t.stopReq.Load() {
		C.cm2_stop(d)
	}
	ready <- nil
	C.cm2_run(d)
	t.mu.Lock()
	t.dead = true
	t.d = nil
	C.cm2_finish(d)
	t.mu.Unlock()
	close(t.reports)
	t.handle.Delete()
	close(t.done)
}

// Write sends one frame with IOHIDDeviceSetReport (output report, id 6).
// OutputID IDPresent passes all 64 bytes, id first (hidapi does the same).
// IDAbsent passes the 63 bytes after the id.
func (t *hidTransport) Write(frame []byte) error {
	if len(frame) != FrameSize {
		return fmt.Errorf("device: frame is %d bytes, want %d", len(frame), FrameSize)
	}
	data := frame
	if t.opts.OutputID == IDAbsent {
		data = frame[1:]
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.dead || t.d == nil {
		if t.removed.Load() {
			return ErrDeviceRemoved
		}
		return ErrClosed
	}
	rc := uint32(C.cm2_set_report(t.d, C.int(ReportID), (*C.uint8_t)(unsafe.Pointer(&data[0])), C.long(len(data))))
	if rc != 0 {
		return ioError("IOHIDDeviceSetReport", rc)
	}
	return nil
}

// Reports yields input frames of the vendor collection (64 bytes, id first).
func (t *hidTransport) Reports() <-chan []byte { return t.reports }

// DroppedReports counts input frames lost because the consumer was too slow.
func (t *hidTransport) DroppedReports() uint64 { return t.dropped.Load() }

// Close stops the run loop, closes the device and closes Reports. It waits
// for the thread to finish, which takes at most one run loop slice, and gives
// up after closeWait if a Write is stuck in the OS.
func (t *hidTransport) Close() {
	t.closeOnce.Do(func() {
		t.stopReq.Store(true)
		t.mu.RLock()
		if !t.dead && t.d != nil {
			C.cm2_stop(t.d)
		}
		t.mu.RUnlock()
		// A Write stuck inside IOHIDDeviceSetReport holds the read lock, which
		// keeps the loop from finishing. Do not hang the caller on it.
		select {
		case <-t.done:
		case <-time.After(closeWait):
		}
	})
}

//export cm2GoReport
func cm2GoReport(ctx C.uintptr_t, reportID C.uint32_t, data *C.uint8_t, n C.int) {
	t, ok := cgo.Handle(ctx).Value().(*hidTransport)
	if !ok {
		return
	}
	frame := NormalizeInput(t.opts.InputID, uint32(reportID), C.GoBytes(unsafe.Pointer(data), n))
	if frame == nil {
		return // another collection of the composite device
	}
	select {
	case t.reports <- frame:
	default:
		t.dropped.Add(1) // never block the run loop
	}
}

//export cm2GoRemoved
func cm2GoRemoved(ctx C.uintptr_t) {
	if t, ok := cgo.Handle(ctx).Value().(*hidTransport); ok {
		t.removed.Store(true)
	}
}
