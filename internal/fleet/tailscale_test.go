package fleet

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/remotex"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/tailscale_status.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

// tailscaleRule builds a FakeRule that answers `tailscale status --json`
// with the given payload and exit code, matching only the tailscale
// command so ssh calls in the same test never accidentally hit it.
func tailscaleRule(stdout []byte, exitCode int, err error) remotex.FakeRule {
	return remotex.FakeRule{
		Match: func(name string, args []string, stdin []byte) bool {
			return name == "tailscale"
		},
		Result: remotex.Result{Stdout: stdout, ExitCode: exitCode},
		Err:    err,
	}
}

func TestTailscaleStatusParse(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{tailscaleRule(loadFixture(t), 0, nil)}}
	p := &Prober{Runner: fake}

	nodes, err := p.TailscaleStatus(context.Background())
	if err != nil {
		t.Fatalf("TailscaleStatus: %v", err)
	}

	// photon is a known peer, reported offline in the fixture.
	photon, ok := resolveTSNode("photon", nodes)
	if !ok {
		t.Fatal("expected to resolve photon")
	}
	if photon.Online {
		t.Error("expected photon.Online = false")
	}

	// minty is a known peer, reported online in the fixture.
	minty, ok := resolveTSNode("minty", nodes)
	if !ok {
		t.Fatal("expected to resolve minty")
	}
	if !minty.Online {
		t.Error("expected minty.Online = true")
	}

	// A device not present in the fixture at all resolves to nothing --
	// callers must treat that as ReachUnknown, not ReachOffline.
	if _, ok := resolveTSNode("nonexistent-device", nodes); ok {
		t.Error("expected nonexistent-device to NOT resolve")
	}

	// Self (edith) and the space-containing peer name both parse without
	// choking -- exercises the edge case called out in the task: a
	// tailscale peer name can contain spaces and must never be assumed
	// ssh-safe by the matching code.
	if _, ok := resolveTSNode("edith", nodes); !ok {
		t.Error("expected to resolve self (edith)")
	}
	pixel, ok := resolveTSNode("Pixel 9 Pro", nodes)
	if !ok {
		t.Fatal("expected to resolve 'Pixel 9 Pro'")
	}
	if pixel.OS != "android" {
		t.Errorf("Pixel 9 Pro OS = %q, want android", pixel.OS)
	}
}

func TestTailscaleMissingIsNotFatal(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		tailscaleRule(nil, 127, nil), // "command not found" shape
	}}
	p := &Prober{Runner: fake}
	devices := []config.DeviceConfig{
		{Name: "dev-a", Probe: config.ProbeSSH},
		{Name: "dev-b", Probe: config.ProbeTailscaleOnly},
	}
	cache := NewCache(p, devices, config.FleetConfig{})
	cache.doRefresh()

	snap := cache.Snapshot(context.Background())
	if snap.TailscaleOK {
		t.Error("expected Snapshot.TailscaleOK = false")
	}
	if len(snap.Devices) != len(devices) {
		t.Fatalf("expected %d devices, got %d", len(devices), len(snap.Devices))
	}
	for _, d := range snap.Devices {
		if d.Reach != ReachUnknown {
			t.Errorf("device %s: Reach = %q, want %q", d.Name, d.Reach, ReachUnknown)
		}
	}
}

func TestResolveTSNodeCaseInsensitiveAndDNSFallback(t *testing.T) {
	nodes := map[string]TSNode{
		"minty": {HostName: "minty", DNSName: "minty.tail1a2b3c.ts.net."},
	}
	if _, ok := resolveTSNode("MINTY", nodes); !ok {
		t.Error("expected case-insensitive HostName match")
	}
	nodesByDNSOnly := map[string]TSNode{
		"weird-host": {HostName: "weird-host", DNSName: "renamed.tail1a2b3c.ts.net."},
	}
	if _, ok := resolveTSNode("renamed", nodesByDNSOnly); !ok {
		t.Error("expected DNSName-label fallback match")
	}
}

func TestProbeCallTimeoutFallsBackWithoutDeadline(t *testing.T) {
	if got := probeCallTimeout(context.Background()); got != defaultProbeTimeout {
		t.Errorf("probeCallTimeout(no deadline) = %v, want %v", got, defaultProbeTimeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if got := probeCallTimeout(ctx); got <= 0 || got > 50*time.Millisecond {
		t.Errorf("probeCallTimeout(50ms deadline) = %v, want (0, 50ms]", got)
	}
}
