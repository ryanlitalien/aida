//go:build darwin && cgo

// Package hid reads button presses from a single USB HID keyboard-like device,
// matched by USB vendor/product ID, via IOKit's IOHIDManager. It exists so a
// dedicated macro key (e.g. a SayoDevice button) can drive Jarvis push-to-talk
// without hijacking the *system* keyboard: only the matched device's key
// transitions are delivered, so pressing the same letter on a real keyboard is
// untouched.
package hid

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include "bridge.h"
*/
import "C"

import (
	"fmt"
	"runtime"
	"runtime/cgo"
	"sync"
	"unsafe"
)

// Event is a single button transition from the target device.
type Event struct {
	Usage int  // HID keyboard usage (e.g. 0x1D='z', 0x1B='x', 0x68=F13, 0x69=F14)
	Down  bool // true on press, false on release
}

// Monitor streams keyboard-usage transitions from one matched USB HID device.
// It owns a dedicated CoreFoundation run loop on a locked OS thread.
type Monitor struct {
	handle    cgo.Handle
	events    chan Event
	rl        unsafe.Pointer // CFRunLoopRef of the run-loop thread
	mgr       unsafe.Pointer // IOHIDManagerRef
	done      chan struct{}
	closeOnce sync.Once
}

// Open matches the USB device with the given vendor and product IDs and begins
// monitoring its keyboard usages. seize requests exclusive access
// (kIOHIDOptionsTypeSeizeDevice), which stops the device emitting its normal
// keystrokes to the focused app - but macOS only permits that for a root
// process, so most callers pass false and suppress leakage with a key remap.
func Open(vid, pid uint16, seize bool) (*Monitor, error) {
	m := &Monitor{
		events: make(chan Event, 64),
		done:   make(chan struct{}),
	}
	m.handle = cgo.NewHandle(m)

	started := make(chan error, 1)
	go func() {
		// The run loop and its input callbacks are bound to this thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		var rl, mgr unsafe.Pointer
		seizeI := C.int(0)
		if seize {
			seizeI = C.int(1)
		}
		r := C.pttOpen(C.uint32_t(vid), C.uint32_t(pid), seizeI,
			C.uintptr_t(uintptr(m.handle)), &rl, &mgr)
		if r != 0 {
			started <- openError(int(r))
			return
		}
		m.rl = rl
		m.mgr = mgr
		started <- nil

		C.pttRunLoop() // blocks until Close() calls pttStop
		C.pttClose(mgr)
		close(m.done)
	}()

	if err := <-started; err != nil {
		m.handle.Delete()
		return nil, err
	}
	return m, nil
}

// Events yields button transitions until Close is called (which closes it).
func (m *Monitor) Events() <-chan Event { return m.events }

// Close stops the run loop, releases the device, and closes Events().
func (m *Monitor) Close() {
	m.closeOnce.Do(func() {
		if m.rl != nil {
			C.pttStop(m.rl)
			<-m.done // wait for the loop thread to release the device
		}
		m.handle.Delete()
		close(m.events)
	})
}

// AccessType is this process's macOS Input Monitoring state for HID listen
// events - the permission IOHIDManagerOpen silently depends on. TCC can deny
// it while IOHIDManagerOpen still returns success, so checking Open's error
// alone can't tell a dead button from a working one.
type AccessType int

const (
	AccessGranted AccessType = iota // kIOHIDAccessTypeGranted
	AccessDenied                    // kIOHIDAccessTypeDenied
	AccessUnknown                   // kIOHIDAccessTypeUnknown - not yet decided
)

// CheckAccess reports the current Input Monitoring state without prompting
// the user or registering this process in the Input Monitoring list.
func CheckAccess() AccessType {
	return AccessType(C.pttCheckAccess())
}

// RequestAccess asks macOS for Input Monitoring access, which triggers the
// one-time system prompt when the state is AccessUnknown (or just registers
// the process in the Input Monitoring list if it's already AccessDenied).
// Returns true if access is granted.
func RequestAccess() bool {
	return C.pttRequestAccess() != 0
}

func openError(code int) error {
	// kIOReturnNotPrivileged - seize (or, absent Input Monitoring, any open)
	// was denied. Point the caller at the two ways out.
	if uint32(code) == 0xE00002C1 {
		return fmt.Errorf("device open denied (kIOReturnNotPrivileged): grant Input Monitoring to this process, and drop seize (exclusive grab needs root)")
	}
	return fmt.Errorf("IOHIDManagerOpen failed: 0x%08X", uint32(code))
}

//export goHIDValue
func goHIDValue(h C.uintptr_t, usage C.long, value C.long) {
	m, ok := cgo.Handle(uintptr(h)).Value().(*Monitor)
	if !ok {
		return
	}
	// Non-blocking: the run loop must never stall on a slow consumer. Button
	// transitions are sparse, so a 64-deep buffer never realistically fills.
	select {
	case m.events <- Event{Usage: int(usage), Down: value != 0}:
	default:
	}
}
