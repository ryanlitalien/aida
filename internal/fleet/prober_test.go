package fleet

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/remotex"
)

func sshRule(match func(args []string) bool, result remotex.Result, err error, delay time.Duration) remotex.FakeRule {
	return remotex.FakeRule{
		Match: func(name string, args []string, stdin []byte) bool {
			return name == "ssh" && (match == nil || match(args))
		},
		Result: result,
		Err:    err,
		Delay:  delay,
	}
}

func TestProbeOneLocalMakesNoNetworkCalls(t *testing.T) {
	// Self is trusted without any reachability/healthz probe -- that part
	// of the "no network call" invariant still holds. What changed is
	// hardware: gathering CPU/mem/GPU/disk for the LOCAL box still needs
	// one direct (non-ssh) Runner.Run("sh", ...) call, since there's no
	// way to learn e.g. free disk space from within the Go process
	// itself. That single call is local, not a network round trip.
	fake := &remotex.FakeRunner{}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{Name: "self", Probe: config.ProbeLocal, Self: true}

	status := p.ProbeOne(context.Background(), d, nil)

	if fake.CallCount() != 1 {
		t.Errorf("CallCount() = %d, want 1 (the local hardware probe)", fake.CallCount())
	}
	for _, c := range fake.Calls {
		if c.Name != "sh" {
			t.Errorf("call Name = %q, want %q (no ssh transport for a local device)", c.Name, "sh")
		}
	}
	if status.Reach != ReachLocal {
		t.Errorf("Reach = %q, want %q", status.Reach, ReachLocal)
	}
	if status.Aida == nil || !status.Aida.Healthz {
		t.Errorf("Aida = %+v, want Healthz true", status.Aida)
	}
}

func TestProbeOneLocalHardware(t *testing.T) {
	const macHW = "CPU=Apple M4 Pro\nMEM=24GB\nDISK=44G free of 926G\nGPU=Apple M4 Pro\n"
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match: func(name string, args []string, _ []byte) bool {
				return name == "sh" && len(args) == 2 && args[0] == "-c" && args[1] == hardwareProbeScript
			},
			Result: remotex.Result{ExitCode: 0, Stdout: []byte(macHW)},
		},
	}}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{Name: "edith", Probe: config.ProbeLocal, Self: true}

	status := p.ProbeOne(context.Background(), d, nil)

	if status.Reach != ReachLocal {
		t.Errorf("Reach = %q, want %q", status.Reach, ReachLocal)
	}
	if status.Hardware == nil {
		t.Fatal("Hardware = nil, want populated")
	}
	if status.Hardware.CPU != "Apple M4 Pro" {
		t.Errorf("CPU = %q, want Apple M4 Pro", status.Hardware.CPU)
	}
	if status.Hardware.Mem != "24GB" {
		t.Errorf("Mem = %q, want 24GB", status.Hardware.Mem)
	}
	if status.Hardware.Disk != "44G free of 926G" {
		t.Errorf("Disk = %q, want 44G free of 926G", status.Hardware.Disk)
	}
	// No comma in the raw GPU value (system_profiler's Chipset Model has
	// no VRAM figure for Apple Silicon's integrated GPU) -- formatGPU
	// must pass it through verbatim rather than mangling it.
	if status.Hardware.GPU != "Apple M4 Pro" {
		t.Errorf("GPU = %q, want passthrough with no VRAM suffix", status.Hardware.GPU)
	}
	if fake.CallCount() != 1 {
		t.Errorf("CallCount() = %d, want 1", fake.CallCount())
	}
}

func TestProbeOneNoneMakesNoCalls(t *testing.T) {
	fake := &remotex.FakeRunner{}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{Name: "nas", Probe: config.ProbeNone}

	status := p.ProbeOne(context.Background(), d, nil)

	if fake.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0", fake.CallCount())
	}
	if status.Reach != ReachUnknown {
		t.Errorf("Reach = %q, want %q", status.Reach, ReachUnknown)
	}
	if status.Aida != nil {
		t.Errorf("Aida = %+v, want nil", status.Aida)
	}
}

func TestProbeOneNoneUsesStaticHardware(t *testing.T) {
	fake := &remotex.FakeRunner{}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{
		Name:     "nas",
		Probe:    config.ProbeNone,
		Hardware: &config.HardwareConfig{CPU: "static-nas-cpu"},
	}

	status := p.ProbeOne(context.Background(), d, nil)

	if status.Hardware == nil || status.Hardware.CPU != "static-nas-cpu" {
		t.Errorf("Hardware = %+v, want the static override", status.Hardware)
	}
	if fake.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0 (probe: none never calls out, live or static)", fake.CallCount())
	}
}

func TestSkipsSSHWhenTailscaleSaysOffline(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		sshRule(nil, remotex.Result{ExitCode: 0, Stdout: []byte("ok")}, nil, 0),
	}}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{Name: "photon", Probe: config.ProbeSSH}
	ts := map[string]TSNode{
		"photon": {HostName: "photon", Online: false, OS: "macOS"},
	}

	status := p.ProbeOne(context.Background(), d, ts)

	if status.Reach != ReachOffline {
		t.Errorf("Reach = %q, want %q", status.Reach, ReachOffline)
	}
	for _, c := range fake.Calls {
		if c.Name == "ssh" {
			t.Fatalf("expected no ssh call, got one: %+v", c)
		}
	}
	if fake.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0 (only the caller-supplied ts map should have been consulted)", fake.CallCount())
	}
}

func TestProbeOneSSHSucceeds(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		sshRule(nil, remotex.Result{ExitCode: 0, Stdout: []byte("ok\n")}, nil, 0),
	}}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{Name: "minty", Probe: config.ProbeSSH}
	ts := map[string]TSNode{
		"minty": {HostName: "minty", Online: true, OS: "linux", TailscaleIPs: []string{"100.1.2.3"}},
	}

	status := p.ProbeOne(context.Background(), d, ts)

	if status.Reach != ReachOnline {
		t.Errorf("Reach = %q, want %q", status.Reach, ReachOnline)
	}
	if status.Aida == nil || !status.Aida.Healthz {
		t.Errorf("Aida = %+v, want Healthz true", status.Aida)
	}
	if status.TailscaleIP != "100.1.2.3" {
		t.Errorf("TailscaleIP = %q, want 100.1.2.3", status.TailscaleIP)
	}
	// healthz curl + the combined hardware probe -- both fire once ssh
	// reachability is confirmed. The catch-all sshRule answers both with
	// the same "ok" stdout, so Hardware ends up non-nil but empty; the
	// dedicated TestProbeOneSSHHardware* tests below exercise real
	// parsing end to end.
	if fake.CallCount() != 2 {
		t.Errorf("CallCount() = %d, want 2 (healthz + hardware)", fake.CallCount())
	}
}

// TestProbeOneSSHHardwareLinux and TestProbeOneSSHHardwareMacOS drive
// ProbeOne through a FakeRunner that distinguishes the healthz curl call
// from the combined hardware-probe call by matching each call's exact
// stdin script (the same bytes RunRemote would put on ssh's stdin, via
// mustScript -- see TestStartAgent_HappyPath's doc comment for why this
// is the robust way to tell two ssh calls apart when their argv/name are
// identical). This proves the wiring end to end, not just parseHardware
// in isolation: ProbeOne must actually issue the second call and store
// its parsed result on DeviceStatus.Hardware.
func TestProbeOneSSHHardwareLinux(t *testing.T) {
	healthzScript := mustScript(t, []string{
		"curl", "-fsS", "--max-time", "3", "http://127.0.0.1:1610/healthz",
	})
	hwScript := mustScript(t, []string{"sh", "-c", hardwareProbeScript})
	const linuxHW = "CPU=AMD Ryzen 9 7950X 16-Core Processor\nMEM=62Gi\nDISK=612G free of 1007G\nGPU=NVIDIA GeForce RTX 4070, 12282 MiB\n"

	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match: func(name string, _ []string, stdin []byte) bool {
				return name == "ssh" && bytes.Equal(stdin, healthzScript)
			},
			Result: remotex.Result{ExitCode: 0, Stdout: []byte("ok\n")},
		},
		{
			Match:  func(name string, _ []string, stdin []byte) bool { return name == "ssh" && bytes.Equal(stdin, hwScript) },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte(linuxHW)},
		},
	}}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{Name: "beast-wsl", Probe: config.ProbeSSH}
	ts := map[string]TSNode{"beast-wsl": {HostName: "beast-wsl", Online: true}}

	status := p.ProbeOne(context.Background(), d, ts)

	if status.Hardware == nil {
		t.Fatal("Hardware = nil, want populated")
	}
	hw := status.Hardware
	if hw.CPU != "AMD Ryzen 9 7950X 16-Core Processor" {
		t.Errorf("CPU = %q", hw.CPU)
	}
	if hw.Mem != "62Gi" {
		t.Errorf("Mem = %q", hw.Mem)
	}
	if hw.Disk != "612G free of 1007G" {
		t.Errorf("Disk = %q", hw.Disk)
	}
	// The whole point of beast-wsl: the WSL2 CUDA-passthrough fallback in
	// hardwareProbeScript is what makes an RTX 4070 show up here at all.
	if hw.GPU != "NVIDIA GeForce RTX 4070 (12282 MiB)" {
		t.Errorf("GPU = %q, want formatted name + VRAM", hw.GPU)
	}
	if fake.CallCount() != 2 {
		t.Errorf("CallCount() = %d, want 2 (healthz + hardware)", fake.CallCount())
	}
}

func TestProbeOneSSHHardwareMacOS(t *testing.T) {
	healthzScript := mustScript(t, []string{
		"curl", "-fsS", "--max-time", "3", "http://127.0.0.1:1610/healthz",
	})
	hwScript := mustScript(t, []string{"sh", "-c", hardwareProbeScript})
	const macHW = "CPU=Apple M4 Pro\nMEM=24GB\nDISK=44G free of 926G\nGPU=Apple M4 Pro\n"

	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match: func(name string, _ []string, stdin []byte) bool {
				return name == "ssh" && bytes.Equal(stdin, healthzScript)
			},
			Result: remotex.Result{ExitCode: 0, Stdout: []byte("ok\n")},
		},
		{
			Match:  func(name string, _ []string, stdin []byte) bool { return name == "ssh" && bytes.Equal(stdin, hwScript) },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte(macHW)},
		},
	}}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{Name: "edith", Probe: config.ProbeSSH}
	ts := map[string]TSNode{"edith": {HostName: "edith", Online: true}}

	status := p.ProbeOne(context.Background(), d, ts)

	if status.Hardware == nil {
		t.Fatal("Hardware = nil, want populated")
	}
	hw := status.Hardware
	if hw.CPU != "Apple M4 Pro" {
		t.Errorf("CPU = %q", hw.CPU)
	}
	if hw.Mem != "24GB" {
		t.Errorf("Mem = %q", hw.Mem)
	}
	if hw.Disk != "44G free of 926G" {
		t.Errorf("Disk = %q", hw.Disk)
	}
	if hw.GPU != "Apple M4 Pro" {
		t.Errorf("GPU = %q, want passthrough (no VRAM figure on Apple Silicon)", hw.GPU)
	}
}

func TestProbeOneTailscaleOnlyNeverProbesAida(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		sshRule(nil, remotex.Result{ExitCode: 0, Stdout: []byte("ok")}, nil, 0),
	}}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{Name: "minty", Probe: config.ProbeTailscaleOnly}
	ts := map[string]TSNode{"minty": {HostName: "minty", Online: true}}

	status := p.ProbeOne(context.Background(), d, ts)

	if status.Reach != ReachOnline {
		t.Errorf("Reach = %q, want %q", status.Reach, ReachOnline)
	}
	if status.Aida != nil {
		t.Errorf("Aida = %+v, want nil (tailscale-only never probes aida)", status.Aida)
	}
	if status.Hardware != nil {
		t.Errorf("Hardware = %+v, want nil (no live probe for tailscale-only, and no static override configured)", status.Hardware)
	}
	if fake.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0", fake.CallCount())
	}
}

// TestProbeOneTailscaleOnlyUsesStaticHardware is the beast case: a
// Windows host aida can never ssh into (no POSIX /bin/sh), so its
// hardware comes entirely from config.yaml's hardware: override rather
// than any live probe.
func TestProbeOneTailscaleOnlyUsesStaticHardware(t *testing.T) {
	fake := &remotex.FakeRunner{}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{
		Name:  "beast",
		Probe: config.ProbeTailscaleOnly,
		Hardware: &config.HardwareConfig{
			CPU: "Intel Core i7-12700F (18 threads)", Mem: "16GB",
			GPU: "NVIDIA GeForce RTX 4070 (12 GB)", Disk: "930GB", Cores: 18,
		},
	}
	ts := map[string]TSNode{"beast": {HostName: "beast", Online: true}}

	status := p.ProbeOne(context.Background(), d, ts)

	if status.Hardware == nil {
		t.Fatal("Hardware = nil, want the static override")
	}
	if status.Hardware.GPU != "NVIDIA GeForce RTX 4070 (12 GB)" {
		t.Errorf("GPU = %q", status.Hardware.GPU)
	}
	if status.Hardware.Cores != 18 {
		t.Errorf("Cores = %d, want 18", status.Hardware.Cores)
	}
	if fake.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0 (a static override never triggers a live call)", fake.CallCount())
	}
}

func TestProbeOneSSHFallsBackToStaticHardwareOnEmptyLiveProbe(t *testing.T) {
	healthzScript := mustScript(t, []string{
		"curl", "-fsS", "--max-time", "3", "http://127.0.0.1:1610/healthz",
	})
	hwScript := mustScript(t, []string{"sh", "-c", hardwareProbeScript})

	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match: func(name string, _ []string, stdin []byte) bool {
				return name == "ssh" && bytes.Equal(stdin, healthzScript)
			},
			Result: remotex.Result{ExitCode: 0, Stdout: []byte("ok\n")},
		},
		{
			// Live hardware probe ran but came back empty (every tool it
			// tried was missing) -- ProbeOne must still fall back to the
			// static override rather than leaving Hardware nil.
			Match:  func(name string, _ []string, stdin []byte) bool { return name == "ssh" && bytes.Equal(stdin, hwScript) },
			Result: remotex.Result{ExitCode: 0, Stdout: []byte("")},
		},
	}}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{
		Name:     "quirky-host",
		Probe:    config.ProbeSSH,
		Hardware: &config.HardwareConfig{CPU: "static-fallback-cpu"},
	}
	ts := map[string]TSNode{"quirky-host": {HostName: "quirky-host", Online: true}}

	status := p.ProbeOne(context.Background(), d, ts)

	if status.Hardware == nil || status.Hardware.CPU != "static-fallback-cpu" {
		t.Errorf("Hardware = %+v, want the static fallback", status.Hardware)
	}
}

func TestProbeOneTimeoutDegrades(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		sshRule(nil, remotex.Result{ExitCode: 0, Stdout: []byte("ok")}, nil, 5*time.Second),
	}}
	p := &Prober{Runner: fake}
	// Device absent from the tailscale map (not present -> ssh probe is
	// still attempted; only a KNOWN-offline node skips ssh). Reach starts
	// and stays ReachUnknown since we have no tailscale signal at all.
	d := config.DeviceConfig{Name: "slow-host", Probe: config.ProbeSSH}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	status := p.ProbeOne(ctx, d, nil)
	elapsed := time.Since(start)

	if elapsed >= 5*time.Second {
		t.Fatalf("ProbeOne took %v, expected it to degrade well before the 5s fake delay", elapsed)
	}
	if status.Reach == ReachOnline {
		t.Errorf("Reach = %q, want anything but online", status.Reach)
	}
	if status.Error == "" {
		t.Error("expected Error to be set on timeout")
	}
}

func TestProbeOneNeverPanics(t *testing.T) {
	cases := []struct {
		name   string
		result remotex.Result
		err    error
	}{
		{"empty stdout", remotex.Result{ExitCode: 0, Stdout: nil}, nil},
		{"invalid utf8/json stdout", remotex.Result{ExitCode: 0, Stdout: []byte("{\xff\x00garbage[[[")}, nil},
		{"huge stdout", remotex.Result{ExitCode: 0, Stdout: []byte(strings.Repeat("x", 8<<20))}, nil},
		{"nonzero exit with garbage stderr", remotex.Result{ExitCode: 17, Stderr: []byte("\x00\x01\x02")}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
				sshRule(nil, tc.result, tc.err, 0),
			}}
			p := &Prober{Runner: fake}
			d := config.DeviceConfig{Name: "weird-host", Probe: config.ProbeSSH}
			ts := map[string]TSNode{"weird-host": {HostName: "weird-host", Online: true}}

			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("ProbeOne panicked: %v", r)
					}
				}()
				status := p.ProbeOne(context.Background(), d, ts)
				if status.Name != "weird-host" {
					t.Errorf("Name = %q, want weird-host", status.Name)
				}
			}()
		})
	}
}

func TestProbeOneInvalidSSHTargetIsError(t *testing.T) {
	fake := &remotex.FakeRunner{}
	p := &Prober{Runner: fake}
	// A leading '-' would be read as an ssh flag; ValidateHost rejects it.
	d := config.DeviceConfig{Name: "-oProxyCommand=evil", Probe: config.ProbeSSH}

	status := p.ProbeOne(context.Background(), d, nil)

	if status.Error == "" {
		t.Error("expected Error to be set for an invalid ssh target")
	}
	if fake.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0 (should fail validation before any call)", fake.CallCount())
	}
}

// The dashboard shows DeviceStatus.Error verbatim, so these codes must
// resolve to something a human can act on. Before this mapping existed the
// grid rendered "exit status 7" for every host that simply doesn't run
// `aida serve` -- the normal state for most of this fleet.
func TestProbeOneMapsExitCodesToHumanErrors(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		wantErr  string
		// wantCalls documents whether ProbeOne still attempts the
		// hardware probe: exit 7 means ssh itself worked (curl ran and
		// reported "couldn't connect"), so it's worth the extra round
		// trip; exit 255 means ssh transport itself failed, so a second
		// call would just fail too and must not be attempted.
		wantCalls int
	}{
		{"curl cannot connect", 7, "no aida daemon on :1610", 2},
		{"ssh cannot connect", 255, "ssh unreachable", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// execx returns a populated Result AND an *exec.ExitError for a
			// non-zero exit; pass both so this covers the real shape rather
			// than an idealized one.
			fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
				sshRule(nil, remotex.Result{ExitCode: tc.exitCode}, fmt.Errorf("exit status %d", tc.exitCode), 0),
			}}
			p := &Prober{Runner: fake}
			d := config.DeviceConfig{Name: "minty", Probe: config.ProbeSSH}
			ts := map[string]TSNode{"minty": {HostName: "minty", Online: true}}

			status := p.ProbeOne(context.Background(), d, ts)

			if status.Error != tc.wantErr {
				t.Errorf("Error = %q, want %q", status.Error, tc.wantErr)
			}
			// nil, not &AidaStatus{Healthz:false}: we never reached the
			// daemon, so we do not know it is down.
			if status.Aida != nil {
				t.Errorf("Aida = %+v, want nil (unknown, not known-down)", status.Aida)
			}
			// The fake's own hardware response also errors (same rule
			// matches both calls), so Hardware must stay nil either way
			// -- what this checks is whether a second call was even
			// attempted.
			if status.Hardware != nil {
				t.Errorf("Hardware = %+v, want nil", status.Hardware)
			}
			if fake.CallCount() != tc.wantCalls {
				t.Errorf("CallCount() = %d, want %d", fake.CallCount(), tc.wantCalls)
			}
		})
	}
}
