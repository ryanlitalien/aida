//go:build darwin && cgo

#include "bridge.h"
#include <IOKit/hid/IOHIDManager.h>
#include <IOKit/hid/IOHIDKeys.h>
#include <IOKit/hidsystem/IOHIDLib.h> // IOHIDCheckAccess / IOHIDRequestAccess (macOS 10.15+)
#include <CoreFoundation/CoreFoundation.h>
#include "_cgo_export.h"

#define KB_PAGE 0x07 // kHIDPage_KeyboardOrKeypad

// valueCallback fires for every element value change on the matched device.
// We forward only keyboard-page usages (letters, F-keys) to Go, tagged with
// the cgo handle we stashed as the callback context.
static void valueCallback(void *context, IOReturn result, void *sender, IOHIDValueRef value) {
    IOHIDElementRef elem = IOHIDValueGetElement(value);
    if (!elem) return;
    if (IOHIDElementGetUsagePage(elem) != KB_PAGE) return;
    uint32_t usage = IOHIDElementGetUsage(elem);
    CFIndex v = IOHIDValueGetIntegerValue(value);
    goHIDValue((uintptr_t)context, (long)usage, (long)v);
}

static CFMutableDictionaryRef matchDict(uint32_t vid, uint32_t pid) {
    CFMutableDictionaryRef d = CFDictionaryCreateMutable(kCFAllocatorDefault, 0,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFNumberRef nvid = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &vid);
    CFNumberRef npid = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &pid);
    CFDictionarySetValue(d, CFSTR(kIOHIDVendorIDKey), nvid);
    CFDictionarySetValue(d, CFSTR(kIOHIDProductIDKey), npid);
    CFRelease(nvid);
    CFRelease(npid);
    return d;
}

int pttOpen(uint32_t vid, uint32_t pid, int seize, uintptr_t handle,
            void **outRunLoop, void **outManager) {
    IOHIDManagerRef mgr = IOHIDManagerCreate(kCFAllocatorDefault, kIOHIDOptionsTypeNone);
    if (!mgr) return -1;

    CFMutableDictionaryRef d = matchDict(vid, pid);
    IOHIDManagerSetDeviceMatching(mgr, d);
    CFRelease(d);

    IOHIDManagerRegisterInputValueCallback(mgr, valueCallback, (void *)handle);
    IOHIDManagerScheduleWithRunLoop(mgr, CFRunLoopGetCurrent(), kCFRunLoopDefaultMode);

    IOOptionBits opts = seize ? kIOHIDOptionsTypeSeizeDevice : kIOHIDOptionsTypeNone;
    IOReturn r = IOHIDManagerOpen(mgr, opts);
    if (r != kIOReturnSuccess) {
        IOHIDManagerUnscheduleFromRunLoop(mgr, CFRunLoopGetCurrent(), kCFRunLoopDefaultMode);
        CFRelease(mgr);
        return (int)r;
    }

    *outRunLoop = (void *)CFRunLoopGetCurrent();
    *outManager = (void *)mgr;
    return 0;
}

void pttRunLoop(void) { CFRunLoopRun(); }

void pttStop(void *runLoop) {
    if (runLoop) CFRunLoopStop((CFRunLoopRef)runLoop);
}

void pttClose(void *manager) {
    if (!manager) return;
    IOHIDManagerRef mgr = (IOHIDManagerRef)manager;
    IOHIDManagerClose(mgr, kIOHIDOptionsTypeNone);
    CFRelease(mgr);
}

int pttCheckAccess(void) {
    // Explicit switch, not a cast: keeps the pttAccess* constants (and thus
    // the Go AccessType values) independent of IOHIDAccessType's raw enum.
    switch (IOHIDCheckAccess(kIOHIDRequestTypeListenEvent)) {
        case kIOHIDAccessTypeGranted: return pttAccessGranted;
        case kIOHIDAccessTypeDenied:  return pttAccessDenied;
        default:                      return pttAccessUnknown;
    }
}

int pttRequestAccess(void) {
    return IOHIDRequestAccess(kIOHIDRequestTypeListenEvent) ? 1 : 0;
}
