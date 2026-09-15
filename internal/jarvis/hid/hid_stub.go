//go:build !darwin || !cgo

package hid

import "errors"

// Event mirrors the darwin type so callers compile on every platform.
type Event struct {
	Usage int
	Down  bool
}

// Monitor is a non-functional placeholder off macOS / without cgo.
type Monitor struct{}

// Open always fails: HID button capture is implemented only for macOS + cgo.
func Open(vid, pid uint16, seize bool) (*Monitor, error) {
	return nil, errors.New("hid button capture requires macOS with cgo enabled")
}

// Events returns a nil channel (blocks forever); never reached after Open errs.
func (m *Monitor) Events() <-chan Event { return nil }

// Close is a no-op.
func (m *Monitor) Close() {}

// AccessType mirrors the darwin type so callers compile on every platform.
type AccessType int

const (
	AccessGranted AccessType = iota
	AccessDenied
	AccessUnknown
)

// CheckAccess always reports unknown off macOS / without cgo.
func CheckAccess() AccessType { return AccessUnknown }

// RequestAccess always fails off macOS / without cgo.
func RequestAccess() bool { return false }
