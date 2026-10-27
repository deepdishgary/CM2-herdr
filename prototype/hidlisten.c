// hidlisten <label> <product> <seconds> [seize]
// Opens the named HID device (non-exclusive by default, or SeizeDevice),
// sets ReportInterval to request streaming, counts input reports.
#include <IOKit/hid/IOHIDManager.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdio.h>
#include <string.h>
#include <mach/mach_time.h>

static const char *label = "?";
static long t0ms;
static long nowms(void) {
  static mach_timebase_info_data_t tb;
  if (!tb.denom) mach_timebase_info(&tb);
  return (long)(mach_absolute_time() * tb.numer / tb.denom / 1000000);
}
static int count = 0;
static uint8_t firstbuf[8];
static int firstlen = 0;

static void report_cb(void *ctx, IOReturn res, void *sender,
                      IOHIDReportType type, uint32_t id,
                      uint8_t *report, CFIndex len) {
  count++;
  if (count == 1) {
    firstlen = (int)(len < 8 ? len : 8);
    memcpy(firstbuf, report, firstlen);
    printf("[%s +%ldms] first report len=%ld id=%u\n", label, nowms() - t0ms, (long)len, id);
    fflush(stdout);
  }
}

int main(int argc, char **argv) {
  if (argc < 4) { fprintf(stderr, "usage: %s label product seconds [seize]\n", argv[0]); return 1; }
  label = argv[1];
  const char *product = argv[2];
  int secs = atoi(argv[3]);
  int seize = argc > 4 && !strcmp(argv[4], "seize");
  t0ms = nowms();

  IOHIDManagerRef mgr = IOHIDManagerCreate(kCFAllocatorDefault, kIOHIDOptionsTypeNone);
  IOHIDManagerSetDeviceMatching(mgr, NULL);
  IOHIDManagerOpen(mgr, kIOHIDOptionsTypeNone);
  CFSetRef set = IOHIDManagerCopyDevices(mgr);
  if (!set) { printf("[%s] no devices\n", label); return 2; }
  CFIndex n = CFSetGetCount(set);
  const void **devs = malloc(sizeof(void *) * n);
  CFSetGetValues(set, devs);
  IOHIDDeviceRef dev = NULL;
  for (CFIndex i = 0; i < n; i++) {
    IOHIDDeviceRef d = (IOHIDDeviceRef)devs[i];
    CFStringRef p = IOHIDDeviceGetProperty(d, CFSTR(kIOHIDProductKey));
    char buf[256] = {0};
    if (p && CFStringGetCString(p, buf, sizeof buf, kCFStringEncodingUTF8) && !strcmp(buf, product)) {
      dev = d; break;
    }
  }
  if (!dev) { printf("[%s] device \"%s\" not found\n", label, product); return 2; }

  IOReturn r = IOHIDDeviceOpen(dev, seize ? kIOHIDOptionsTypeSeizeDevice : kIOHIDOptionsTypeNone);
  printf("[%s +%ldms] open(%s) -> 0x%08X %s\n", label, nowms() - t0ms,
         seize ? "SEIZE" : "none", r, r == kIOReturnSuccess ? "OK" : "FAIL");
  fflush(stdout);
  if (r != kIOReturnSuccess) return 3;

  static uint8_t rbuf[256];
  IOHIDDeviceRegisterInputReportCallback(dev, rbuf, sizeof rbuf, report_cb, NULL);
  IOHIDDeviceScheduleWithRunLoop(dev, CFRunLoopGetCurrent(), kCFRunLoopDefaultMode);

  // Ask the sensor to stream: ReportInterval in microseconds.
  int interval = 100000;
  CFNumberRef num = CFNumberCreate(NULL, kCFNumberIntType, &interval);
  IOHIDDeviceSetProperty(dev, CFSTR(kIOHIDReportIntervalKey), num);
  CFRelease(num);

  CFRunLoopRunInMode(kCFRunLoopDefaultMode, secs, false);
  printf("[%s +%ldms] TOTAL=%d first8=", label, nowms() - t0ms, count);
  for (int i = 0; i < firstlen; i++) printf("%02x", firstbuf[i]);
  printf("\n");
  IOHIDDeviceClose(dev, seize ? kIOHIDOptionsTypeSeizeDevice : kIOHIDOptionsTypeNone);
  return 0;
}
