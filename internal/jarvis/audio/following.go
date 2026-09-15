package audio

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Following-stream tunables.
const (
	micPollInterval  = 3 * time.Second  // how often to re-check the preferred device
	micStaleCheck    = 2 * time.Second  // how often to test for a frozen capture
	micStaleAfter    = 12 * time.Second // no data for this long → force re-acquire
	micReopenBackoff = 500 * time.Millisecond
)

// FollowingStream is a microphone capture that transparently follows the
// preferred available input device. It re-runs SelectInputDevice periodically
// and hot-swaps the underlying ffmpeg capture when the winning device changes
// (laptop mic ⇄ webcam on dock/undock) - and, via a watchdog, re-acquires when
// the capture stalls (sleep/wake can leave ffmpeg alive but frozen, feeding
// silence forever). It exposes the same Read/Flush/Stop surface as *PCMStream,
// so the listener uses it interchangeably.
//
// Ownership: any given PCMStream is opened and closed under reopenMu, so only
// one swap happens at a time. The read loop and the supervisor goroutine can
// both trigger a swap; whichever holds reopenMu wins and the other no-ops.
type FollowingStream struct {
	ctx      context.Context
	prefer   []string
	onSwitch func(name string) // fired when the active device name changes

	mu      sync.Mutex // guards cur/curName/curDev
	cur     *PCMStream
	curName string
	curDev  string

	reopenMu sync.Mutex   // serializes device selection + swap
	lastRead atomic.Int64 // UnixNano of the last successful read
	stopped  atomic.Bool
}

// StartFollowingStream selects the preferred available device, opens it, and
// begins following. onSwitch (may be nil) is called whenever the active device
// changes, for logging.
func StartFollowingStream(ctx context.Context, prefer []string, onSwitch func(name string)) (*FollowingStream, error) {
	if onSwitch == nil {
		onSwitch = func(string) {}
	}
	dev, name := SelectInputDevice(ctx, prefer)
	ps, err := StartPCMStream(ctx, dev)
	if err != nil {
		return nil, err
	}
	f := &FollowingStream{ctx: ctx, prefer: prefer, onSwitch: onSwitch, cur: ps, curName: name, curDev: dev}
	f.lastRead.Store(time.Now().UnixNano())
	go f.supervise()
	return f, nil
}

func (f *FollowingStream) Read(p []byte) (int, error) {
	for {
		if err := f.ctx.Err(); err != nil {
			return 0, err
		}
		f.mu.Lock()
		s := f.cur
		f.mu.Unlock()

		if s == nil {
			// A prior reopen failed to open a device; back off and retry.
			select {
			case <-f.ctx.Done():
				return 0, f.ctx.Err()
			case <-time.After(micReopenBackoff):
			}
			f.reopenFrom(nil)
			continue
		}

		n, err := s.Read(p)
		if err == nil {
			f.lastRead.Store(time.Now().UnixNano())
			return n, nil
		}
		if e := f.ctx.Err(); e != nil {
			return 0, e
		}
		// Read failed: either the supervisor already swapped s out (device
		// change / stall recovery) - in which case we just retry on the new
		// stream - or the device genuinely died and we must reopen. reopenFrom
		// distinguishes the two so we never double-open.
		f.reopenFrom(s)
	}
}

// Flush drains the current capture's pipe backlog (self-audio suppression).
func (f *FollowingStream) Flush() int {
	f.mu.Lock()
	s := f.cur
	f.mu.Unlock()
	if s == nil {
		return 0
	}
	return s.Flush()
}

// Stop terminates the capture and the supervisor (via ctx or the stopped flag).
func (f *FollowingStream) Stop() error {
	f.stopped.Store(true)
	f.mu.Lock()
	s := f.cur
	f.cur = nil
	f.mu.Unlock()
	if s != nil {
		return s.Stop()
	}
	return nil
}

// supervise re-checks the preferred device and watches for a frozen capture.
func (f *FollowingStream) supervise() {
	poll := time.NewTicker(micPollInterval)
	stale := time.NewTicker(micStaleCheck)
	defer poll.Stop()
	defer stale.Stop()
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-poll.C:
			f.reopenIfChanged()
		case <-stale.C:
			if time.Since(time.Unix(0, f.lastRead.Load())) > micStaleAfter {
				f.forceReopen()
			}
		}
	}
}

// reopenIfChanged swaps to the preferred device only when the winning device
// differs from the one currently open (the common dock/undock case).
func (f *FollowingStream) reopenIfChanged() {
	f.reopenMu.Lock()
	defer f.reopenMu.Unlock()
	if f.stopped.Load() {
		return
	}
	dev, name := SelectInputDevice(f.ctx, f.prefer)
	f.mu.Lock()
	same := name == f.curName && dev == f.curDev && f.cur != nil
	f.mu.Unlock()
	if same {
		return
	}
	f.swap(dev, name)
}

// forceReopen re-acquires the preferred device even if it hasn't changed -
// used to recover a stalled/frozen capture.
func (f *FollowingStream) forceReopen() {
	f.reopenMu.Lock()
	defer f.reopenMu.Unlock()
	if f.stopped.Load() {
		return
	}
	dev, name := SelectInputDevice(f.ctx, f.prefer)
	f.swap(dev, name)
}

// reopenFrom is the read-loop's recovery path. If failed is still the current
// stream (a genuine error, not a swap that already happened), it re-acquires;
// otherwise it no-ops so the loop retries on the already-swapped stream.
func (f *FollowingStream) reopenFrom(failed *PCMStream) {
	f.reopenMu.Lock()
	defer f.reopenMu.Unlock()
	if f.stopped.Load() {
		return
	}
	f.mu.Lock()
	stale := f.cur == failed || f.cur == nil
	f.mu.Unlock()
	if !stale {
		return // supervisor already replaced it; retry on the new stream
	}
	dev, name := SelectInputDevice(f.ctx, f.prefer)
	f.swap(dev, name)
}

// swap opens dev, installs it as current, and stops the old capture. Caller
// holds reopenMu. On open failure it drops the current stream so Read backs
// off and retries rather than spinning on a dead handle.
func (f *FollowingStream) swap(dev, name string) {
	ps, err := StartPCMStream(f.ctx, dev)
	if err != nil {
		f.mu.Lock()
		old := f.cur
		f.cur = nil
		f.mu.Unlock()
		if old != nil {
			old.Stop()
		}
		return
	}
	f.mu.Lock()
	if f.stopped.Load() {
		f.mu.Unlock()
		ps.Stop()
		return
	}
	old, prevName := f.cur, f.curName
	f.cur, f.curName, f.curDev = ps, name, dev
	f.mu.Unlock()

	if old != nil {
		old.Stop() // unblocks any read parked on the old stream
	}
	f.lastRead.Store(time.Now().UnixNano())
	if name != prevName {
		f.onSwitch(name)
	}
}
