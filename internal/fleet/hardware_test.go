package fleet

import (
	"context"
	"errors"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/remotex"
)

func TestParseHardwareLinux(t *testing.T) {
	out := "CPU=AMD Ryzen 9 7950X 16-Core Processor\nMEM=62Gi\nDISK=612G free of 1007G\nGPU=NVIDIA GeForce RTX 4070, 12282 MiB\n"

	hw := parseHardware([]byte(out))

	if hw.CPU != "AMD Ryzen 9 7950X 16-Core Processor" {
		t.Errorf("CPU = %q", hw.CPU)
	}
	if hw.Mem != "62Gi" {
		t.Errorf("Mem = %q", hw.Mem)
	}
	if hw.Disk != "612G free of 1007G" {
		t.Errorf("Disk = %q", hw.Disk)
	}
	if hw.GPU != "NVIDIA GeForce RTX 4070 (12282 MiB)" {
		t.Errorf("GPU = %q", hw.GPU)
	}
}

func TestParseHardwareMacOS(t *testing.T) {
	out := "CPU=Apple M4 Pro\nMEM=24GB\nDISK=44G free of 926G\nGPU=Apple M4 Pro\n"

	hw := parseHardware([]byte(out))

	if hw.CPU != "Apple M4 Pro" {
		t.Errorf("CPU = %q", hw.CPU)
	}
	if hw.Mem != "24GB" {
		t.Errorf("Mem = %q", hw.Mem)
	}
	if hw.Disk != "44G free of 926G" {
		t.Errorf("Disk = %q", hw.Disk)
	}
	// system_profiler's Chipset Model carries no VRAM figure for Apple
	// Silicon's integrated GPU -- formatGPU must not invent one.
	if hw.GPU != "Apple M4 Pro" {
		t.Errorf("GPU = %q, want passthrough", hw.GPU)
	}
}

// TestParseHardwareMissingGPU covers the common "no discrete GPU" case
// (e.g. photon, a laptop with only an integrated chip system_profiler
// doesn't report a Chipset Model for, or a Linux box with no nvidia-smi):
// every other field parses, GPU stays empty, no error.
func TestParseHardwareMissingGPU(t *testing.T) {
	out := "CPU=Intel(R) Core(TM) i7\nMEM=16Gi\nDISK=100G free of 500G\nGPU=\n"

	hw := parseHardware([]byte(out))

	if hw.CPU == "" || hw.Mem == "" || hw.Disk == "" {
		t.Errorf("expected CPU/Mem/Disk to still parse, got %+v", hw)
	}
	if hw.GPU != "" {
		t.Errorf("GPU = %q, want empty", hw.GPU)
	}
}

// TestParseHardwareGarbageNeverPanics matches ProbeOne's own contract
// (never panic, degrade instead): unparseable or empty input yields a
// non-nil Hardware with empty fields, not an error.
func TestParseHardwareGarbageNeverPanics(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte("ok\n"), // no '=' at all -- a stray healthz-style response
		[]byte("\xff\x00garbage[[["),
		[]byte("CPU\nMEM=\nnotakeyvalueline\nGPU=x, y, z"),
	}
	for _, c := range cases {
		hw := parseHardware(c)
		if hw == nil {
			t.Errorf("parseHardware(%q) = nil, want non-nil", c)
		}
	}
}

func TestFormatGPU(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"single csv pair", "NVIDIA GeForce RTX 4070, 12282 MiB", "NVIDIA GeForce RTX 4070 (12282 MiB)"},
		{"passthrough with no comma", "Apple M4 Pro", "Apple M4 Pro"},
		{
			"multiple gpus semicolon-joined",
			"NVIDIA GeForce RTX 3050, 8192 MiB;NVIDIA GeForce RTX 3050, 8192 MiB",
			"NVIDIA GeForce RTX 3050 (8192 MiB); NVIDIA GeForce RTX 3050 (8192 MiB)",
		},
		{"trailing semicolon and whitespace tolerated", " NVIDIA T4, 16384 MiB ; ", "NVIDIA T4 (16384 MiB)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatGPU(tt.raw); got != tt.want {
				t.Errorf("formatGPU(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// ---- probeHardware guard clauses ----

func TestProbeHardwareReturnsNilOnTransportError(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Err: errors.New("boom")},
	}}
	p := &Prober{Runner: fake}

	if hw := p.probeHardware(context.Background(), "minty", false); hw != nil {
		t.Errorf("probeHardware = %+v, want nil on transport error", hw)
	}
}

func TestProbeHardwareReturnsNilOnTimeout(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{TimedOut: true}, Delay: 0},
	}}
	// Force TimedOut directly via the rule's Result rather than a real
	// timer -- the rule matches unconditionally (nil Match), and no Delay
	// means it returns immediately with TimedOut already set.
	p := &Prober{Runner: fake}

	if hw := p.probeHardware(context.Background(), "minty", false); hw != nil {
		t.Errorf("probeHardware = %+v, want nil on timeout", hw)
	}
}

func TestProbeHardwareReturnsNilOnEmptyStdout(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{ExitCode: 0, Stdout: []byte("   \n")}},
	}}
	p := &Prober{Runner: fake}

	if hw := p.probeHardware(context.Background(), "minty", false); hw != nil {
		t.Errorf("probeHardware = %+v, want nil on blank stdout", hw)
	}
}

func TestProbeHardwareLocalUsesDirectShCall(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{ExitCode: 0, Stdout: []byte("CPU=Apple M4 Pro\n")}},
	}}
	p := &Prober{Runner: fake}

	hw := p.probeHardware(context.Background(), "", true)

	if hw == nil || hw.CPU != "Apple M4 Pro" {
		t.Fatalf("probeHardware(local) = %+v, want CPU=Apple M4 Pro", hw)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("Calls = %d, want 1", len(fake.Calls))
	}
	if fake.Calls[0].Name != "sh" {
		t.Errorf("call Name = %q, want %q (no ssh transport for local)", fake.Calls[0].Name, "sh")
	}
}

// ---- static hardware override ----

func TestStaticHardwareNilWhenUnconfigured(t *testing.T) {
	d := config.DeviceConfig{Name: "beast"}
	if hw := staticHardware(d); hw != nil {
		t.Errorf("staticHardware = %+v, want nil (no hardware: block configured)", hw)
	}
}

func TestStaticHardwareConvertsConfig(t *testing.T) {
	d := config.DeviceConfig{
		Name: "beast",
		Hardware: &config.HardwareConfig{
			CPU:   "Intel Core i7-12700F (18 threads)",
			Mem:   "16GB",
			GPU:   "NVIDIA GeForce RTX 4070 (12 GB)",
			Disk:  "930GB",
			Cores: 18,
		},
	}

	hw := staticHardware(d)

	if hw == nil {
		t.Fatal("staticHardware = nil, want populated")
	}
	if hw.CPU != "Intel Core i7-12700F (18 threads)" {
		t.Errorf("CPU = %q", hw.CPU)
	}
	if hw.Cores != 18 {
		t.Errorf("Cores = %d, want 18", hw.Cores)
	}
	if hw.Mem != "16GB" {
		t.Errorf("Mem = %q", hw.Mem)
	}
	if hw.GPU != "NVIDIA GeForce RTX 4070 (12 GB)" {
		t.Errorf("GPU = %q", hw.GPU)
	}
	if hw.Disk != "930GB" {
		t.Errorf("Disk = %q", hw.Disk)
	}
}

func TestProbeOrStaticHardwarePrefersLiveProbe(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{ExitCode: 0, Stdout: []byte("CPU=live-cpu\n")}},
	}}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{
		Name:     "minty",
		Hardware: &config.HardwareConfig{CPU: "static-cpu-should-not-be-used"},
	}

	hw := p.probeOrStaticHardware(context.Background(), d, "minty", false)

	if hw == nil || hw.CPU != "live-cpu" {
		t.Errorf("probeOrStaticHardware = %+v, want the live probe's CPU", hw)
	}
}

func TestProbeOrStaticHardwareFallsBackWhenLiveProbeEmpty(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Err: errors.New("ssh transport failed")},
	}}
	p := &Prober{Runner: fake}
	d := config.DeviceConfig{
		Name:     "beast",
		Hardware: &config.HardwareConfig{CPU: "static-cpu", Cores: 18},
	}

	hw := p.probeOrStaticHardware(context.Background(), d, "beast", false)

	if hw == nil || hw.CPU != "static-cpu" || hw.Cores != 18 {
		t.Errorf("probeOrStaticHardware = %+v, want the static fallback", hw)
	}
}
