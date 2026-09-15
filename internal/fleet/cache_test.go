package fleet

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/remotex"
)

// eventually polls cond every 5ms until it returns true or timeout elapses,
// failing the test on timeout. Used instead of a bare sleep wherever a
// result depends on an async goroutine (Refresh) completing.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// minimalTailscaleJSON is a tiny synthetic `tailscale status --json`
// payload for cache-level tests that don't care about matching specifics
// (those are covered by tailscale_test.go against the real fixture).
const minimalTailscaleJSON = `{"Self":{"HostName":"self","Online":true,"OS":"macOS"},"Peer":{}}`

func TestSnapshotNeverBlocks(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match:  func(name string, args []string, stdin []byte) bool { return name == "tailscale" },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte(minimalTailscaleJSON)},
		},
		{
			// Any ssh call takes 10s -- Snapshot must not wait for it.
			Match:  func(name string, args []string, stdin []byte) bool { return name == "ssh" },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte("ok")},
			Delay:  10 * time.Second,
		},
	}}
	p := &Prober{Runner: fake}
	devices := []config.DeviceConfig{
		{Name: "dev-a", Probe: config.ProbeSSH},
		{Name: "dev-b", Probe: config.ProbeSSH},
		{Name: "dev-c", Probe: config.ProbeSSH},
	}
	fc := config.FleetConfig{ProbeTimeoutSeconds: 20}
	c := NewCache(p, devices, fc)

	start := time.Now()
	snap := c.Snapshot(context.Background())
	elapsed := time.Since(start)

	if elapsed > 50*time.Millisecond {
		t.Fatalf("Snapshot took %v on a cold cache, want <50ms", elapsed)
	}
	if !snap.Refreshing {
		t.Error("expected Refreshing = true on a cold snapshot")
	}
	if len(snap.Devices) != 3 {
		t.Fatalf("expected 3 placeholder devices, got %d", len(snap.Devices))
	}
	for _, d := range snap.Devices {
		if d.Reach != ReachUnknown {
			t.Errorf("device %s: Reach = %q, want %q", d.Name, d.Reach, ReachUnknown)
		}
	}
}

func TestSnapshotServesStale(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match:  func(name string, args []string, stdin []byte) bool { return name == "tailscale" },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte(minimalTailscaleJSON)},
		},
	}}
	p := &Prober{Runner: fake}
	devices := []config.DeviceConfig{{Name: "dev-a", Probe: config.ProbeTailscaleOnly}}
	fc := config.FleetConfig{TTLSeconds: 30, ProbeTimeoutSeconds: 5}
	c := NewCache(p, devices, fc)

	// Inject a controllable clock and seed the cache synchronously (no
	// need to wait on the async Refresh path just to get a first result).
	now := time.Now()
	var mu sync.Mutex
	c.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	c.doRefresh()

	seededCalls := fake.CallCount()
	if seededCalls == 0 {
		t.Fatal("expected doRefresh to have made at least one call")
	}

	// Still fresh: no Stale, no growth in call count.
	fresh := c.Snapshot(context.Background())
	if fresh.Devices[0].Stale {
		t.Error("expected fresh snapshot to not be stale")
	}

	// Jump the clock past TTL.
	mu.Lock()
	now = now.Add(31 * time.Second)
	mu.Unlock()

	stale := c.Snapshot(context.Background())
	if !stale.Devices[0].Stale {
		t.Error("expected Stale = true past TTL")
	}
	if !stale.Refreshing {
		t.Error("expected Refreshing = true on a stale snapshot")
	}

	// The stale Snapshot() call above must have kicked an async refresh;
	// wait (don't sleep-and-hope) for the call count to grow.
	eventually(t, 2*time.Second, func() bool { return fake.CallCount() > seededCalls })
}

func TestSingleflightCollapsesConcurrentRefreshes(t *testing.T) {
	var sshCalls int32
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match:  func(name string, args []string, stdin []byte) bool { return name == "tailscale" },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte(minimalTailscaleJSON)},
		},
		{
			Match: func(name string, args []string, stdin []byte) bool {
				if name == "ssh" {
					atomic.AddInt32(&sshCalls, 1)
				}
				return name == "ssh"
			},
			Result: remotex.Result{ExitCode: 0, Stdout: []byte("ok")},
			Delay:  200 * time.Millisecond, // slow enough for 20 concurrent Refresh() calls to pile up and join
		},
	}}
	p := &Prober{Runner: fake}
	devices := []config.DeviceConfig{{Name: "dev-a", Probe: config.ProbeSSH}}
	fc := config.FleetConfig{ProbeTimeoutSeconds: 5}
	c := NewCache(p, devices, fc)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Refresh()
		}()
	}
	wg.Wait()

	eventually(t, 3*time.Second, func() bool {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.haveSnap
	})

	// One round of probing collapsed from 20 concurrent Refresh() calls:
	// exactly two ssh calls for the one device (healthz + the combined
	// hardware probe), not 40.
	if got := atomic.LoadInt32(&sshCalls); got != 2 {
		t.Errorf("ssh call count = %d, want 2 (singleflight should have collapsed 20 concurrent Refresh() calls)", got)
	}
}

func TestConcurrencyCapRespected(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match:  func(name string, args []string, stdin []byte) bool { return name == "tailscale" },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte(minimalTailscaleJSON)},
		},
		{
			Match:  func(name string, args []string, stdin []byte) bool { return name == "ssh" },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte("ok")},
			Delay:  50 * time.Millisecond,
		},
	}}
	p := &Prober{Runner: fake}

	devices := make([]config.DeviceConfig, 20)
	for i := range devices {
		devices[i] = config.DeviceConfig{Name: deviceName(i), Probe: config.ProbeSSH}
	}
	fc := config.FleetConfig{MaxConcurrentProbes: 8, ProbeTimeoutSeconds: 5}
	c := NewCache(p, devices, fc)

	c.doRefresh() // synchronous: exercise the fan-out directly

	if got := fake.MaxInFlight(); got > 8 {
		t.Errorf("MaxInFlight() = %d, want <= 8", got)
	}
	if got := fake.MaxInFlight(); got < 2 {
		// Sanity check the test actually exercised concurrency at all
		// (a MaxInFlight of 1 would mean the semaphore was pointlessly
		// serializing everything, likely a test bug rather than a real
		// pass).
		t.Errorf("MaxInFlight() = %d, expected some real overlap given a 50ms delay and 20 devices", got)
	}
}

func deviceName(i int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	return "dev-" + string(letters[i%len(letters)]) + string(letters[(i/len(letters))%len(letters)])
}
