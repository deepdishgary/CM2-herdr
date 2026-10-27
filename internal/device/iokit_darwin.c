// C side of the macOS HID backend. See cm2hid.h.
#include <IOKit/hid/IOHIDLib.h>
#include <IOKit/hid/IOHIDManager.h>
#include <IOKit/hidsystem/IOHIDLib.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>
#include "cm2hid.h"
#include "_cgo_export.h"

#define IN_BUF 256

struct cm2_dev {
  IOHIDManagerRef mgr;
  IOHIDDeviceRef dev;
  uint8_t *buf;
  CFRunLoopRef rl;
  uintptr_t ctx;
  int stop;
  int opened;
  int scheduled;
  int mgr_opened;
};

// ---- property helpers ------------------------------------------------------

static int prop_int(IOHIDDeviceRef d, CFStringRef key, int def) {
  CFTypeRef v = IOHIDDeviceGetProperty(d, key);
  int out = def;
  if (v && CFGetTypeID(v) == CFNumberGetTypeID()) {
    SInt64 n;
    if (CFNumberGetValue((CFNumberRef)v, kCFNumberSInt64Type, &n)) out = (int)n;
  }
  return out;
}

static void prop_str(IOHIDDeviceRef d, CFStringRef key, char *out, size_t n) {
  out[0] = 0;
  CFTypeRef v = IOHIDDeviceGetProperty(d, key);
  if (v && CFGetTypeID(v) == CFStringGetTypeID())
    CFStringGetCString((CFStringRef)v, out, (CFIndex)n, kCFStringEncodingUTF8);
}

static int dict_int(CFDictionaryRef d, CFStringRef key) {
  CFTypeRef v = CFDictionaryGetValue(d, key);
  SInt64 n = 0;
  if (v && CFGetTypeID(v) == CFNumberGetTypeID()) CFNumberGetValue((CFNumberRef)v, kCFNumberSInt64Type, &n);
  return (int)n;
}

static void fill_info(IOHIDDeviceRef d, cm2_info *i) {
  memset(i, 0, sizeof *i);
  i->vendor_id = prop_int(d, CFSTR(kIOHIDVendorIDKey), 0);
  i->product_id = prop_int(d, CFSTR(kIOHIDProductIDKey), 0);
  i->primary_page = prop_int(d, CFSTR(kIOHIDPrimaryUsagePageKey), 0);
  i->primary_usage = prop_int(d, CFSTR(kIOHIDPrimaryUsageKey), 0);
  i->max_in = prop_int(d, CFSTR(kIOHIDMaxInputReportSizeKey), 0);
  i->max_out = prop_int(d, CFSTR(kIOHIDMaxOutputReportSizeKey), 0);
  i->max_feature = prop_int(d, CFSTR(kIOHIDMaxFeatureReportSizeKey), 0);
  i->location_id = (long)(uint32_t)prop_int(d, CFSTR(kIOHIDLocationIDKey), 0);
  prop_str(d, CFSTR(kIOHIDProductKey), i->product, sizeof i->product);
  prop_str(d, CFSTR(kIOHIDManufacturerKey), i->manufacturer, sizeof i->manufacturer);
  prop_str(d, CFSTR(kIOHIDSerialNumberKey), i->serial, sizeof i->serial);
  prop_str(d, CFSTR(kIOHIDTransportKey), i->transport, sizeof i->transport);

  CFTypeRef pairs = IOHIDDeviceGetProperty(d, CFSTR(kIOHIDDeviceUsagePairsKey));
  if (pairs && CFGetTypeID(pairs) == CFArrayGetTypeID()) {
    CFIndex n = CFArrayGetCount((CFArrayRef)pairs);
    for (CFIndex k = 0; k < n && i->pair_count < CM2_MAX_PAIRS; k++) {
      CFTypeRef e = CFArrayGetValueAtIndex((CFArrayRef)pairs, k);
      if (!e || CFGetTypeID(e) != CFDictionaryGetTypeID()) continue;
      i->pairs[i->pair_count][0] = dict_int((CFDictionaryRef)e, CFSTR(kIOHIDDeviceUsagePageKey));
      i->pairs[i->pair_count][1] = dict_int((CFDictionaryRef)e, CFSTR(kIOHIDDeviceUsageKey));
      i->pair_count++;
    }
  }

  CFTypeRef desc = IOHIDDeviceGetProperty(d, CFSTR(kIOHIDReportDescriptorKey));
  if (desc && CFGetTypeID(desc) == CFDataGetTypeID()) {
    CFIndex n = CFDataGetLength((CFDataRef)desc);
    if (n > CM2_DESC_MAX) n = CM2_DESC_MAX;
    memcpy(i->desc, CFDataGetBytePtr((CFDataRef)desc), (size_t)n);
    i->desc_len = (int)n;
  }
}

static int has_page(const cm2_info *i, int page) {
  if (i->primary_page == page) return 1;
  for (int k = 0; k < i->pair_count; k++)
    if (i->pairs[k][0] == page) return 1;
  return 0;
}

// ---- manager ---------------------------------------------------------------

static void set_num(CFMutableDictionaryRef d, CFStringRef key, int v) {
  CFNumberRef n = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &v);
  CFDictionarySetValue(d, key, n);
  CFRelease(n);
}

// Creates a manager that matches vid/pid, schedules it on the current run
// loop (no IOHIDManagerOpen, so no device is opened) and waits briefly for the
// initial match. Returns the manager or NULL. *set is the matched device set
// (may be empty, caller releases it).
static IOHIDManagerRef match_devices(int vid, int pid, CFSetRef *set) {
  *set = NULL;
  IOHIDManagerRef mgr = IOHIDManagerCreate(kCFAllocatorDefault, kIOHIDOptionsTypeNone);
  if (!mgr) return NULL;
  CFMutableDictionaryRef m = CFDictionaryCreateMutable(kCFAllocatorDefault, 0, &kCFTypeDictionaryKeyCallBacks,
                                                       &kCFTypeDictionaryValueCallBacks);
  set_num(m, CFSTR(kIOHIDVendorIDKey), vid);
  set_num(m, CFSTR(kIOHIDProductIDKey), pid);
  IOHIDManagerSetDeviceMatching(mgr, m);
  CFRelease(m);
  IOHIDManagerScheduleWithRunLoop(mgr, CFRunLoopGetCurrent(), kCFRunLoopDefaultMode);
  // Let the initial device matching run. Up to 500 ms, in 10 ms steps, and
  // stop at the first non-empty answer.
  for (int step = 0; step < 50; step++) {
    CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.01, false);
    CFSetRef s = IOHIDManagerCopyDevices(mgr);
    if (s && CFSetGetCount(s) > 0) {
      *set = s;
      break;
    }
    if (s) CFRelease(s);
  }
  return mgr;
}

static void drop_manager(IOHIDManagerRef mgr) {
  if (!mgr) return;
  IOHIDManagerUnscheduleFromRunLoop(mgr, CFRunLoopGetCurrent(), kCFRunLoopDefaultMode);
  CFRelease(mgr);
}

uint32_t cm2_enumerate(int vid, int pid, cm2_info *out, int max, int *count) {
  *count = 0;
  CFSetRef set = NULL;
  IOHIDManagerRef mgr = match_devices(vid, pid, &set);
  if (!mgr) return (uint32_t)kIOReturnError;
  if (set) {
    CFIndex n = CFSetGetCount(set);
    const void **devs = malloc(sizeof(void *) * (size_t)n);
    if (devs) {
      CFSetGetValues(set, devs);
      for (CFIndex k = 0; k < n && *count < max; k++) {
        fill_info((IOHIDDeviceRef)devs[k], &out[*count]);
        (*count)++;
      }
      free(devs);
    }
    CFRelease(set);
  }
  drop_manager(mgr);
  return 0;
}

// ---- open / run ------------------------------------------------------------

static void report_cb(void *context, IOReturn result, void *sender, IOHIDReportType type, uint32_t report_id,
                      uint8_t *report, CFIndex len) {
  struct cm2_dev *d = context;
  if (result != kIOReturnSuccess || type != kIOHIDReportTypeInput || len <= 0) return;
  cm2GoReport(d->ctx, report_id, report, (int)len);
}

static void removal_cb(void *context, IOReturn result, void *sender) {
  struct cm2_dev *d = context;
  __atomic_store_n(&d->stop, 1, __ATOMIC_SEQ_CST);
  cm2GoRemoved(d->ctx);
}

uint32_t cm2_open(int vid, int pid, int page, int open_manager, uintptr_t ctx, cm2_dev **out, cm2_info *info) {
  *out = NULL;
  CFSetRef set = NULL;
  IOHIDManagerRef mgr = match_devices(vid, pid, &set);
  if (!mgr) return (uint32_t)kIOReturnError;
  if (!set || CFSetGetCount(set) == 0) {
    if (set) CFRelease(set);
    drop_manager(mgr);
    return CM2_ERR_NO_DEVICE;
  }

  // Choose: primary usage page first, then a device that lists the page.
  CFIndex n = CFSetGetCount(set);
  const void **devs = malloc(sizeof(void *) * (size_t)n);
  CFSetGetValues(set, devs);
  IOHIDDeviceRef pick = NULL;
  cm2_info pinfo;
  for (int pass = 0; pass < 2 && !pick; pass++) {
    for (CFIndex k = 0; k < n; k++) {
      cm2_info i;
      fill_info((IOHIDDeviceRef)devs[k], &i);
      if ((pass == 0 && i.primary_page == page) || (pass == 1 && has_page(&i, page))) {
        pick = (IOHIDDeviceRef)devs[k];
        pinfo = i;
        break;
      }
    }
  }
  if (pick) CFRetain(pick);
  free(devs);
  CFRelease(set);
  if (!pick) {
    drop_manager(mgr);
    return CM2_ERR_NO_VENDOR;
  }

  int mgr_opened = 0;
  if (open_manager) {
    // prototype/hidlisten.c opens its manager (shared) before the device.
    IOReturn mr = IOHIDManagerOpen(mgr, kIOHIDOptionsTypeNone);
    if (mr != kIOReturnSuccess) {
      CFRelease(pick);
      drop_manager(mgr);
      return (uint32_t)mr;
    }
    mgr_opened = 1;
  }

  // Shared open: kIOHIDOptionsTypeNone, as prototype/hidlisten.c does.
  IOReturn r = IOHIDDeviceOpen(pick, kIOHIDOptionsTypeNone);
  if (r != kIOReturnSuccess) {
    if (mgr_opened) IOHIDManagerClose(mgr, kIOHIDOptionsTypeNone);
    CFRelease(pick);
    drop_manager(mgr);
    return (uint32_t)r;
  }

  struct cm2_dev *d = calloc(1, sizeof *d);
  d->mgr = mgr;
  d->dev = pick;
  d->ctx = ctx;
  d->buf = malloc(IN_BUF);
  d->rl = CFRunLoopGetCurrent();
  d->opened = 1;
  d->mgr_opened = mgr_opened;
  IOHIDManagerUnscheduleFromRunLoop(mgr, d->rl, kCFRunLoopDefaultMode);
  IOHIDDeviceRegisterInputReportCallback(pick, d->buf, IN_BUF, report_cb, d);
  IOHIDDeviceRegisterRemovalCallback(pick, removal_cb, d);
  IOHIDDeviceScheduleWithRunLoop(pick, d->rl, kCFRunLoopDefaultMode);
  d->scheduled = 1;
  *info = pinfo;
  *out = d;
  return 0;
}

void cm2_run(cm2_dev *d) {
  // Short slices so a stop that raced the start is seen within 200 ms; the
  // CFRunLoopStop in cm2_stop ends the slice at once in the normal case.
  while (!__atomic_load_n(&d->stop, __ATOMIC_SEQ_CST)) {
    SInt32 r = CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.2, false);
    if (r == kCFRunLoopRunFinished) break; // no sources left: do not spin
  }
}

void cm2_stop(cm2_dev *d) {
  __atomic_store_n(&d->stop, 1, __ATOMIC_SEQ_CST);
  if (d->rl) CFRunLoopStop(d->rl);
}

void cm2_finish(cm2_dev *d) {
  if (d->scheduled) IOHIDDeviceUnscheduleFromRunLoop(d->dev, d->rl, kCFRunLoopDefaultMode);
  IOHIDDeviceRegisterInputReportCallback(d->dev, d->buf, IN_BUF, NULL, NULL);
  IOHIDDeviceRegisterRemovalCallback(d->dev, NULL, NULL);
  if (d->opened) IOHIDDeviceClose(d->dev, kIOHIDOptionsTypeNone);
  CFRelease(d->dev);
  if (d->mgr_opened) IOHIDManagerClose(d->mgr, kIOHIDOptionsTypeNone);
  if (d->mgr) CFRelease(d->mgr);
  free(d->buf);
  free(d);
}

uint32_t cm2_set_report(cm2_dev *d, int report_id, const uint8_t *buf, long len) {
  return (uint32_t)IOHIDDeviceSetReport(d->dev, kIOHIDReportTypeOutput, (CFIndex)report_id, buf, (CFIndex)len);
}

// ---- access ----------------------------------------------------------------

int cm2_access(void) {
  switch (IOHIDCheckAccess(kIOHIDRequestTypeListenEvent)) {
    case kIOHIDAccessTypeGranted: return 0;
    case kIOHIDAccessTypeDenied: return 1;
    default: return 2;
  }
}

int cm2_request_access(void) { return IOHIDRequestAccess(kIOHIDRequestTypeListenEvent) ? 1 : 0; }
