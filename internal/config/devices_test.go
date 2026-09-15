package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestResolveDevicesSynthesizesSelf(t *testing.T) {
	t.Run("empty devices synthesizes one self device", func(t *testing.T) {
		cfg := &Config{}
		got := cfg.ResolveDevices("edith")
		if len(got) != 1 {
			t.Fatalf("expected 1 synthesized device, got %d: %+v", len(got), got)
		}
		d := got[0]
		if d.Name != "edith" {
			t.Errorf("Name = %q, want %q", d.Name, "edith")
		}
		if !d.Self {
			t.Error("expected Self = true")
		}
		if d.Probe != ProbeLocal {
			t.Errorf("Probe = %q, want %q", d.Probe, ProbeLocal)
		}
		if d.Role != "primary" {
			t.Errorf("Role = %q, want %q", d.Role, "primary")
		}
	})

	t.Run("empty machine name falls back to this machine", func(t *testing.T) {
		cfg := &Config{}
		got := cfg.ResolveDevices("")
		if len(got) != 1 {
			t.Fatalf("expected 1 synthesized device, got %d", len(got))
		}
		if got[0].Name != "this machine" {
			t.Errorf("Name = %q, want %q", got[0].Name, "this machine")
		}
	})
}

func TestResolveDevicesPassesThrough(t *testing.T) {
	devices := []DeviceConfig{
		{Name: "edith", Self: true, Role: "primary"},
		{Name: "photon", Role: "satellite"},
	}
	cfg := &Config{Devices: devices}

	got := cfg.ResolveDevices("edith")
	if len(got) != len(devices) {
		t.Fatalf("expected %d devices, got %d", len(devices), len(got))
	}
	for i, d := range devices {
		if got[i] != d {
			t.Errorf("device[%d] = %+v, want %+v", i, got[i], d)
		}
	}
}

func TestValidateDevices(t *testing.T) {
	tests := []struct {
		name    string
		devices []DeviceConfig
		wantErr int
	}{
		{
			name: "single valid device",
			devices: []DeviceConfig{
				{Name: "edith", Self: true, Probe: ProbeLocal},
			},
			wantErr: 0,
		},
		{
			name: "duplicate names case-insensitive",
			devices: []DeviceConfig{
				{Name: "edith"},
				{Name: "Edith"},
			},
			wantErr: 1,
		},
		{
			name: "two self devices",
			devices: []DeviceConfig{
				{Name: "a", Self: true},
				{Name: "b", Self: true},
			},
			wantErr: 1,
		},
		{
			name: "two herdr devices",
			devices: []DeviceConfig{
				{Name: "a", Herdr: true},
				{Name: "b", Herdr: true},
			},
			wantErr: 1, // just the herdr-count error; neither device sets probe:none
		},
		{
			name: "bad probe enum",
			devices: []DeviceConfig{
				{Name: "a", Probe: "carrier-pigeon"},
			},
			wantErr: 1,
		},
		{
			name: "herdr with probe none",
			devices: []DeviceConfig{
				{Name: "a", Herdr: true, Probe: ProbeNone},
			},
			wantErr: 1,
		},
		{
			name: "port zero is valid",
			devices: []DeviceConfig{
				{Name: "a", AidaPort: 0},
			},
			wantErr: 0,
		},
		{
			name: "port valid range",
			devices: []DeviceConfig{
				{Name: "a", AidaPort: 1610},
			},
			wantErr: 0,
		},
		{
			name: "port too high",
			devices: []DeviceConfig{
				{Name: "a", AidaPort: 70000},
			},
			wantErr: 1,
		},
		{
			name: "port negative",
			devices: []DeviceConfig{
				{Name: "a", AidaPort: -1},
			},
			wantErr: 1,
		},
		{
			name: "injection-shaped ssh_host proxy command",
			devices: []DeviceConfig{
				{Name: "a", SSHHost: "-oProxyCommand=x"},
			},
			wantErr: 1,
		},
		{
			name: "injection-shaped ssh_host semicolon",
			devices: []DeviceConfig{
				{Name: "a", SSHHost: "a;id"},
			},
			wantErr: 1,
		},
		{
			name: "injection-shaped ssh_host space",
			devices: []DeviceConfig{
				{Name: "a", SSHHost: "a b"},
			},
			wantErr: 1,
		},
		{
			name: "injection-shaped ssh_host command substitution",
			devices: []DeviceConfig{
				{Name: "a", SSHHost: "$(id)"},
			},
			wantErr: 1,
		},
		{
			name: "static hardware override with valid cores",
			devices: []DeviceConfig{
				{Name: "beast", Probe: ProbeTailscaleOnly, Hardware: &HardwareConfig{
					CPU: "Intel Core i7-12700F (18 threads)", Mem: "16GB",
					GPU: "NVIDIA GeForce RTX 4070 (12 GB)", Disk: "930GB", Cores: 18,
				}},
			},
			wantErr: 0,
		},
		{
			name: "negative hardware cores",
			devices: []DeviceConfig{
				{Name: "a", Hardware: &HardwareConfig{Cores: -1}},
			},
			wantErr: 1,
		},
		{
			name: "everything wrong at once accumulates all errors",
			devices: []DeviceConfig{
				{Name: "dup", Self: true, Probe: "bogus", AidaPort: -5},
				{Name: "Dup", Self: true, Herdr: true},
				{Name: "other", Herdr: true, Probe: ProbeNone, SSHHost: "-oEvil=1"},
			},
			// dup name (1) + two self (1) + two herdr (1) + bad probe (1) +
			// bad port (1) + herdr+none (1) + bad ssh_host (1) = 7
			wantErr: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Devices: tt.devices}
			errs := cfg.ValidateDevices()
			if len(errs) != tt.wantErr {
				t.Errorf("got %d errors, want %d: %v", len(errs), tt.wantErr, errs)
			}
		})
	}
}

func TestFleetConfigDefaults(t *testing.T) {
	t.Run("zero value applies defaults", func(t *testing.T) {
		var f FleetConfig
		if got, want := f.TTL(), 30*time.Second; got != want {
			t.Errorf("TTL() = %v, want %v", got, want)
		}
		if got, want := f.ProbeTimeout(), 6*time.Second; got != want {
			t.Errorf("ProbeTimeout() = %v, want %v", got, want)
		}
		if got, want := f.RefreshInterval(), 45*time.Second; got != want {
			t.Errorf("RefreshInterval() = %v, want %v", got, want)
		}
		if got, want := f.Concurrency(), 8; got != want {
			t.Errorf("Concurrency() = %d, want %d", got, want)
		}
		if got, want := f.BifrostUserOrDefault(), "heimdall"; got != want {
			t.Errorf("BifrostUserOrDefault() = %q, want %q", got, want)
		}
	})

	t.Run("explicit values pass through", func(t *testing.T) {
		f := FleetConfig{
			TTLSeconds:          10,
			ProbeTimeoutSeconds: 3,
			RefreshSeconds:      120,
			MaxConcurrentProbes: 2,
			BifrostUser:         "ryan",
		}
		if got, want := f.TTL(), 10*time.Second; got != want {
			t.Errorf("TTL() = %v, want %v", got, want)
		}
		if got, want := f.ProbeTimeout(), 3*time.Second; got != want {
			t.Errorf("ProbeTimeout() = %v, want %v", got, want)
		}
		if got, want := f.RefreshInterval(), 120*time.Second; got != want {
			t.Errorf("RefreshInterval() = %v, want %v", got, want)
		}
		if got, want := f.Concurrency(), 2; got != want {
			t.Errorf("Concurrency() = %d, want %d", got, want)
		}
		if got, want := f.BifrostUserOrDefault(), "ryan"; got != want {
			t.Errorf("BifrostUserOrDefault() = %q, want %q", got, want)
		}
	})

	t.Run("negative refresh seconds disables the ticker", func(t *testing.T) {
		f := FleetConfig{RefreshSeconds: -1}
		if got := f.RefreshInterval(); got > 0 {
			t.Errorf("RefreshInterval() = %v, want <= 0 (disabled)", got)
		}
	})
}

func TestFleetConfigBifrostLanes(t *testing.T) {
	t.Run("empty BifrostUsers falls back to BifrostUserOrDefault", func(t *testing.T) {
		var f FleetConfig
		got := f.BifrostLanes()
		want := []string{"heimdall"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("BifrostLanes() = %v, want %v", got, want)
		}
	})

	t.Run("empty BifrostUsers with BifrostUser set falls back to it", func(t *testing.T) {
		f := FleetConfig{BifrostUser: "ryan"}
		got := f.BifrostLanes()
		want := []string{"ryan"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("BifrostLanes() = %v, want %v", got, want)
		}
	})

	t.Run("BifrostUsers set takes priority, order preserved", func(t *testing.T) {
		f := FleetConfig{BifrostUser: "ryan", BifrostUsers: []string{"heimdall", "ryan"}}
		got := f.BifrostLanes()
		want := []string{"heimdall", "ryan"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("BifrostLanes() = %v, want %v", got, want)
		}
	})
}

func TestValidateFleet(t *testing.T) {
	tests := []struct {
		name    string
		fleet   FleetConfig
		wantErr int
	}{
		{
			name:    "no bifrost_users configured is valid",
			fleet:   FleetConfig{},
			wantErr: 0,
		},
		{
			name:    "single lane is valid",
			fleet:   FleetConfig{BifrostUsers: []string{"ryan"}},
			wantErr: 0,
		},
		{
			name:    "two distinct lanes is valid",
			fleet:   FleetConfig{BifrostUsers: []string{"heimdall", "ryan"}},
			wantErr: 0,
		},
		{
			name:    "empty user rejected",
			fleet:   FleetConfig{BifrostUsers: []string{"ryan", ""}},
			wantErr: 1,
		},
		{
			name:    "whitespace-only user rejected",
			fleet:   FleetConfig{BifrostUsers: []string{"   "}},
			wantErr: 1,
		},
		{
			name:    "duplicate user rejected, case-insensitive",
			fleet:   FleetConfig{BifrostUsers: []string{"ryan", "Ryan"}},
			wantErr: 1,
		},
		{
			name:    "injection-shaped user rejected",
			fleet:   FleetConfig{BifrostUsers: []string{"-oProxyCommand=x"}},
			wantErr: 1,
		},
		{
			name:    "empty and duplicate both reported",
			fleet:   FleetConfig{BifrostUsers: []string{"ryan", "ryan", ""}},
			wantErr: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Fleet: tt.fleet}
			errs := cfg.ValidateFleet()
			if len(errs) != tt.wantErr {
				t.Errorf("got %d errors, want %d: %v", len(errs), tt.wantErr, errs)
			}
		})
	}
}

func TestDeviceConfigAccessors(t *testing.T) {
	t.Run("SSHTarget falls back to Name", func(t *testing.T) {
		d := DeviceConfig{Name: "photon"}
		if got := d.SSHTarget(); got != "photon" {
			t.Errorf("SSHTarget() = %q, want %q", got, "photon")
		}
		d.SSHHost = "photon.local"
		if got := d.SSHTarget(); got != "photon.local" {
			t.Errorf("SSHTarget() = %q, want %q", got, "photon.local")
		}
	})

	t.Run("TailscaleHost falls back to Name", func(t *testing.T) {
		d := DeviceConfig{Name: "photon"}
		if got := d.TailscaleHost(); got != "photon" {
			t.Errorf("TailscaleHost() = %q, want %q", got, "photon")
		}
		d.TailscaleName = "photon-ts"
		if got := d.TailscaleHost(); got != "photon-ts" {
			t.Errorf("TailscaleHost() = %q, want %q", got, "photon-ts")
		}
	})

	t.Run("Port falls back to 1610", func(t *testing.T) {
		d := DeviceConfig{}
		if got := d.Port(); got != 1610 {
			t.Errorf("Port() = %d, want 1610", got)
		}
		d.AidaPort = 9999
		if got := d.Port(); got != 9999 {
			t.Errorf("Port() = %d, want 9999", got)
		}
	})

	t.Run("ProbeMode defaults local when self, else ssh", func(t *testing.T) {
		self := DeviceConfig{Self: true}
		if got := self.ProbeMode(); got != ProbeLocal {
			t.Errorf("ProbeMode() = %q, want %q", got, ProbeLocal)
		}
		other := DeviceConfig{}
		if got := other.ProbeMode(); got != ProbeSSH {
			t.Errorf("ProbeMode() = %q, want %q", got, ProbeSSH)
		}
		explicit := DeviceConfig{Self: true, Probe: ProbeTailscaleOnly}
		if got := explicit.ProbeMode(); got != ProbeTailscaleOnly {
			t.Errorf("ProbeMode() = %q, want %q", got, ProbeTailscaleOnly)
		}
	})
}

func TestConfigRoundTripWithDevices(t *testing.T) {
	t.Run("devices and fleet block round-trip", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Devices = []DeviceConfig{
			{Name: "edith", Role: "primary", Self: true, Probe: ProbeLocal},
			{
				Name:          "photon",
				Role:          "satellite",
				Probe:         ProbeSSH,
				Herdr:         true,
				SSHHost:       "photon.local",
				TailscaleName: "photon-ts",
				AidaPort:      1611,
				Notes:         "MacBook Air M1",
			},
		}
		cfg.Fleet = FleetConfig{
			TTLSeconds:          10,
			ProbeTimeoutSeconds: 3,
			RefreshSeconds:      60,
			MaxConcurrentProbes: 4,
			BifrostUser:         "ryan",
			BifrostUsers:        []string{"ryan", "heimdall"},
		}

		data, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}

		var cfg2 Config
		if err := yaml.Unmarshal(data, &cfg2); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}

		if len(cfg2.Devices) != 2 {
			t.Fatalf("expected 2 devices, got %d", len(cfg2.Devices))
		}
		if cfg2.Devices[1] != cfg.Devices[1] {
			t.Errorf("device[1] mismatch: got %+v, want %+v", cfg2.Devices[1], cfg.Devices[1])
		}
		if !reflect.DeepEqual(cfg2.Fleet, cfg.Fleet) {
			t.Errorf("fleet mismatch: got %+v, want %+v", cfg2.Fleet, cfg.Fleet)
		}
	})

	t.Run("no devices omits devices and fleet keys entirely", func(t *testing.T) {
		cfg := DefaultConfig() // Devices and Fleet left zero-value

		data, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}

		// A regression here would silently rewrite everyone's config.yaml
		// with an empty `devices: []` / `fleet: {}` block on every save.
		if strings.Contains(string(data), "devices:") {
			t.Errorf("marshalled yaml unexpectedly contains 'devices:':\n%s", data)
		}
		if strings.Contains(string(data), "fleet:") {
			t.Errorf("marshalled yaml unexpectedly contains 'fleet:':\n%s", data)
		}
	})
}
