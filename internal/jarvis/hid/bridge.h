//go:build darwin && cgo

#ifndef AIDA_HID_BRIDGE_H
#define AIDA_HID_BRIDGE_H
#include <stdint.h>

// pttOpen creates an IOHIDManager matching the given USB vendor/product,
// registers a keyboard-usage value callback (dispatched into Go's goHIDValue
// with the supplied handle as context), schedules it on the CURRENT thread's
// run loop, and opens the device. On success it returns 0 and writes the
// current CFRunLoopRef and IOHIDManagerRef to the out params; on failure it
// returns the non-zero IOReturn code. Must be called on the same thread that
// will subsequently run the loop (pttRunLoop).
int pttOpen(uint32_t vid, uint32_t pid, int seize, uintptr_t handle,
            void **outRunLoop, void **outManager);

// pttRunLoop runs the current thread's CoreFoundation run loop until stopped.
void pttRunLoop(void);

// pttStop stops the given run loop. Safe to call from another thread.
void pttStop(void *runLoop);

// pttClose closes and releases the manager.
void pttClose(void *manager);

// pttAccessGranted / pttAccessDenied / pttAccessUnknown mirror
// kIOHIDAccessTypeGranted/Denied/Unknown (IOKit/hidsystem/IOHIDLib.h), but as
// our own constants so the Go side never depends on the raw IOKit enum
// values. bridge.c maps them explicitly via a switch rather than casting.
enum {
    pttAccessGranted = 0,
    pttAccessDenied  = 1,
    pttAccessUnknown = 2,
};

// pttCheckAccess reports whether this process currently has macOS Input
// Monitoring access for HID listen events (kIOHIDRequestTypeListenEvent).
// This is the check that IOHIDManagerOpen itself cannot give you: TCC can
// silently withhold keyboard-page events even after a successful open, so a
// dead button and a working one look identical without this call. Returns
// one of the pttAccess* constants above.
int pttCheckAccess(void);

// pttRequestAccess asks macOS to grant Input Monitoring access for HID listen
// events, which triggers the one-time system prompt (or, if already denied,
// just registers the process in the Input Monitoring list so the user can
// grant it manually). Returns 1 if access is granted, 0 otherwise.
int pttRequestAccess(void);

#endif
