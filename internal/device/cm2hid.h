// C helpers for iokit_darwin.go. Only built on darwin (file name suffix).
#ifndef CM2HID_H
#define CM2HID_H

#include <stdint.h>

#define CM2_MAX_PAIRS 16
#define CM2_DESC_MAX 1024

// Result codes above the IOReturn range, for "no match" cases.
#define CM2_ERR_NO_DEVICE 0xFFFF0001u
#define CM2_ERR_NO_VENDOR 0xFFFF0002u

typedef struct {
  int vendor_id, product_id;
  int primary_page, primary_usage;
  int pair_count;
  int pairs[CM2_MAX_PAIRS][2]; // page, usage
  int max_in, max_out, max_feature;
  long location_id;
  char product[128], manufacturer[128], serial[128], transport[64];
  int desc_len;
  uint8_t desc[CM2_DESC_MAX];
} cm2_info;

typedef struct cm2_dev cm2_dev;

// Reads the properties of every device that matches vid/pid. Does not open any
// device. Returns 0, or an IOReturn. *count is the number stored in out.
uint32_t cm2_enumerate(int vid, int pid, cm2_info *out, int max, int *count);

// Picks the device with usage page `page` (primary, else in the usage pairs),
// opens it with kIOHIDOptionsTypeNone (shared, not seized), registers the
// callbacks (and first calls IOHIDManagerOpen, shared, if open_manager) and schedules the device on the CALLING thread's run loop. The
// caller must call cm2_run on the same thread. ctx is passed back to the Go
// callbacks. Returns 0, CM2_ERR_*, or an IOReturn from IOHIDDeviceOpen.
uint32_t cm2_open(int vid, int pid, int page, int open_manager, uintptr_t ctx, cm2_dev **out, cm2_info *info);

// Runs the run loop until cm2_stop, device removal, or the loop has no sources.
void cm2_run(cm2_dev *d);

// Asks cm2_run to return. Safe from any thread, before or during cm2_run.
void cm2_stop(cm2_dev *d);

// Closes and frees the device. Call on the run loop thread after cm2_run.
void cm2_finish(cm2_dev *d);

// IOHIDDeviceSetReport, output type. Returns an IOReturn.
uint32_t cm2_set_report(cm2_dev *d, int report_id, const uint8_t *buf, long len);

// IOHIDCheckAccess(kIOHIDRequestTypeListenEvent): 0 granted, 1 denied, 2 unknown.
int cm2_access(void);

// IOHIDRequestAccess(kIOHIDRequestTypeListenEvent). Raises a macOS prompt for
// the responsible app. Only the probe's `access --request` may call it.
int cm2_request_access(void);

#endif
