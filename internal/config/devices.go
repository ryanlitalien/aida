package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Probe modes for DeviceConfig.Probe / ProbeMode(). "" is a valid config
// value (meaning "use the default") but is never returned by ProbeMode --
// callers always get one of these four.
const (
	ProbeSSH           = "ssh"
	ProbeTailscaleOnly = "tailscale-only"
	ProbeLocal         = "local"
	ProbeNone          = "none"
)

// DeviceConfig declares one machine in the fleet for the /dashboard page.
// Purely descriptive: aida never installs, configures, or mutates a device
// from this block -- it only probes what is declared.
type DeviceConfig struct {
	Name          string `yaml:"name"`
	Role          string `yaml:"role,omitempty"`  // free-form badge: primary, satellite, gpu, nas
	Probe         string `yaml:"probe,omitempty"` // ssh | tailscale-only | local | none
	Self          bool   `yaml:"self,omitempty"`
	Herdr         bool   `yaml:"herdr,omitempty"`
	SSHHost       string `yaml:"ssh_host,omitempty"`       // defaults to Name
	TailscaleName string `yaml:"tailscale_name,omitempty"` // defaults to Name
	AidaPort      int    `yaml:"aida_port,omitempty"`      // 0 => 1610
	Notes         string `yaml:"notes,omitempty"`

	// Hardware is a hand-entered fallback for CPU/mem/GPU/disk specs when
	// aida can't learn them by live-probing -- probe: tailscale-only or
	// none (e.g. beast, a Windows host aida never ssh's into), or a
	// probe: ssh/local device whose live hardware probe comes back with
	// nothing. nil means "no static override configured."
	Hardware *HardwareConfig `yaml:"hardware,omitempty"`
}

// HardwareConfig is that hand-entered fallback. Every string field mirrors
// fleet.Hardware's shape (a free-form, already-formatted display string,
// not something parsed back apart at config-load time); Cores is broken
// out separately because it feeds the fleet-totals numeric sum rather
// than being embedded in the CPU string.
type HardwareConfig struct {
	CPU   string `yaml:"cpu,omitempty"`
	Mem   string `yaml:"mem,omitempty"`
	GPU   string `yaml:"gpu,omitempty"`
	Disk  string `yaml:"disk,omitempty"`
	Cores int    `yaml:"cores,omitempty"`
}

// FleetConfig holds probe tunables, kept separate so `devices:` stays a
// clean list.
type FleetConfig struct {
	TTLSeconds          int    `yaml:"ttl_seconds,omitempty"`           // 0 => 30
	ProbeTimeoutSeconds int    `yaml:"probe_timeout_seconds,omitempty"` // 0 => 6
	RefreshSeconds      int    `yaml:"refresh_seconds,omitempty"`       // 0 => 45; negative disables the ticker
	MaxConcurrentProbes int    `yaml:"max_concurrent_probes,omitempty"` // 0 => 8
	BifrostUser         string `yaml:"bifrost_user,omitempty"`          // "" => heimdall; single-lane fallback, see BifrostLanes

	// BifrostUsers configures multiple /bifrost lanes -- one bifrost.Client
	// per user, all against the same herdr: true device (see
	// internal/cli/dashboard_web.go's newDashboardDeps). Unset means "just
	// the one lane," so BifrostUser keeps working unchanged. Use
	// BifrostLanes(), not this field directly, to get the effective list.
	BifrostUsers []string `yaml:"bifrost_users,omitempty"`
}

// TTL returns how long a probe result stays fresh before it must be re-run.
func (f FleetConfig) TTL() time.Duration {
	if f.TTLSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(f.TTLSeconds) * time.Second
}

// ProbeTimeout returns the per-device probe deadline.
func (f FleetConfig) ProbeTimeout() time.Duration {
	if f.ProbeTimeoutSeconds <= 0 {
		return 6 * time.Second
	}
	return time.Duration(f.ProbeTimeoutSeconds) * time.Second
}

// RefreshInterval returns how often the dashboard re-probes the fleet in the
// background. A negative RefreshSeconds is an explicit opt-out (poll on
// demand only); callers must treat any return <=0 as "ticker disabled"
// rather than clamping it to a minimum.
func (f FleetConfig) RefreshInterval() time.Duration {
	if f.RefreshSeconds == 0 {
		return 45 * time.Second
	}
	return time.Duration(f.RefreshSeconds) * time.Second
}

// Concurrency returns the max number of in-flight device probes.
func (f FleetConfig) Concurrency() int {
	if f.MaxConcurrentProbes <= 0 {
		return 8
	}
	return f.MaxConcurrentProbes
}

// BifrostUserOrDefault returns the SSH user the bifrost probe connects as.
func (f FleetConfig) BifrostUserOrDefault() string {
	if f.BifrostUser == "" {
		return "heimdall"
	}
	return f.BifrostUser
}

// BifrostLanes returns the effective list of /bifrost lanes: BifrostUsers
// when set, else a single-element slice holding BifrostUserOrDefault().
// The list order matters -- index 0 is the default lane a request without
// an explicit ?lane=/"lane" selects (see registerBifrostRoutes).
func (f FleetConfig) BifrostLanes() []string {
	if len(f.BifrostUsers) > 0 {
		return f.BifrostUsers
	}
	return []string{f.BifrostUserOrDefault()}
}

// SSHTarget returns the host to pass to `ssh` for this device.
func (d DeviceConfig) SSHTarget() string {
	if d.SSHHost != "" {
		return d.SSHHost
	}
	return d.Name
}

// TailscaleHost returns the name to resolve via `tailscale status` / MagicDNS.
func (d DeviceConfig) TailscaleHost() string {
	if d.TailscaleName != "" {
		return d.TailscaleName
	}
	return d.Name
}

// Port returns the port aida's HTTP daemon listens on for this device.
func (d DeviceConfig) Port() int {
	if d.AidaPort == 0 {
		return 1610
	}
	return d.AidaPort
}

// ProbeMode returns the effective probe strategy: the configured Probe, or
// the default (local for the self device, ssh otherwise) when unset.
func (d DeviceConfig) ProbeMode() string {
	if d.Probe != "" {
		return d.Probe
	}
	if d.Self {
		return ProbeLocal
	}
	return ProbeSSH
}

// ResolveDevices returns the effective device list. When cfg.Devices is
// empty it synthesizes a single self device from machineName, so a fresh
// install renders a working one-node dashboard with zero config and zero
// network calls.
//
// machineName is passed in rather than resolved here to keep internal/config
// free of a dependency on internal/jarvis/audio.
func (c *Config) ResolveDevices(machineName string) []DeviceConfig {
	if len(c.Devices) > 0 {
		return c.Devices
	}
	name := machineName
	if name == "" {
		name = "this machine"
	}
	return []DeviceConfig{
		{Name: name, Role: "primary", Self: true, Probe: ProbeLocal},
	}
}

// sshHostPattern bounds anything that ends up as an `ssh` destination
// argument (device name when ssh_host is unset, or ssh_host itself).
// internal/remotex exports a ValidateHost with this same regex for the
// values it drives directly; internal/config intentionally does not import
// remotex (it would be a dependency the wrong way round -- config is read by
// everything, including remotex), so the pattern is duplicated here rather
// than shared. Keep the two in sync by hand if this ever changes.
//
// The security property that matters: reject a leading '-'. Without it, a
// config typo/injection like `name: -oProxyCommand=...evil...` gets handed
// straight to `ssh` as an option flag instead of a hostname, which is local
// command execution. Anchoring the first character to alphanumeric closes
// that off structurally rather than blocklisting `-o`, `-i`, etc.
var sshHostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,253}$`)

// ValidateDevices checks the devices block, returning EVERY problem rather
// than just the first so a user fixing config.yaml sees the whole list.
func (c *Config) ValidateDevices() []error {
	var errs []error

	names := make(map[string]int, len(c.Devices)) // lowercased name -> occurrences
	selfCount, herdrCount := 0, 0

	for i, d := range c.Devices {
		label := deviceErrLabel(i, d.Name)

		if strings.TrimSpace(d.Name) == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", label))
		} else {
			names[strings.ToLower(d.Name)]++
		}

		switch d.Probe {
		case "", ProbeSSH, ProbeTailscaleOnly, ProbeLocal, ProbeNone:
			// valid
		default:
			errs = append(errs, fmt.Errorf("%s: probe %q must be one of ssh, tailscale-only, local, none (or empty for the default)", label, d.Probe))
		}

		if d.Self {
			selfCount++
		}
		if d.Herdr {
			herdrCount++
		}
		if d.Herdr && d.Probe == ProbeNone {
			errs = append(errs, fmt.Errorf("%s: herdr device cannot set probe: none -- it would be undrivable", label))
		}

		if d.AidaPort != 0 && (d.AidaPort < 1 || d.AidaPort > 65535) {
			errs = append(errs, fmt.Errorf("%s: aida_port %d must be 0 (default) or in 1..65535", label, d.AidaPort))
		}

		if d.Hardware != nil && d.Hardware.Cores < 0 {
			errs = append(errs, fmt.Errorf("%s: hardware.cores %d must be >= 0", label, d.Hardware.Cores))
		}

		// See sshHostPattern's comment: this is a security check, not a
		// cosmetic one. Both name (the implicit ssh destination) and an
		// explicit ssh_host are checked.
		if d.Name != "" && !sshHostPattern.MatchString(d.Name) {
			errs = append(errs, fmt.Errorf("%s: name %q is not a safe ssh destination", label, d.Name))
		}
		if d.SSHHost != "" && !sshHostPattern.MatchString(d.SSHHost) {
			errs = append(errs, fmt.Errorf("%s: ssh_host %q is not a safe ssh destination", label, d.SSHHost))
		}
	}

	dupNames := make([]string, 0, len(names))
	for name, count := range names {
		if count > 1 {
			dupNames = append(dupNames, name)
		}
	}
	sort.Strings(dupNames) // deterministic error ordering
	for _, name := range dupNames {
		errs = append(errs, fmt.Errorf("devices: name %q is used more than once (case-insensitive)", name))
	}

	if selfCount > 1 {
		errs = append(errs, fmt.Errorf("devices: at most one device may set self: true, found %d", selfCount))
	}
	if herdrCount > 1 {
		errs = append(errs, fmt.Errorf("devices: at most one device may set herdr: true, found %d", herdrCount))
	}

	return errs
}

// ValidateFleet checks the fleet block's bifrost_users list, returning
// EVERY problem rather than just the first (same policy as
// ValidateDevices). An empty entry would resolve to remotex's
// sudo -iu "" -- not "the default user," a broken invocation -- and a
// duplicate entry would silently collapse two distinct /bifrost lanes
// into one bifrost.Client, one of them shadowing the other's presets.
func (c *Config) ValidateFleet() []error {
	var errs []error

	seen := make(map[string]int, len(c.Fleet.BifrostUsers)) // lowercased user -> occurrences
	for i, u := range c.Fleet.BifrostUsers {
		if strings.TrimSpace(u) == "" {
			errs = append(errs, fmt.Errorf("fleet.bifrost_users[%d]: empty user", i))
			continue
		}
		// Same rationale as sshHostPattern's use in ValidateDevices: this
		// value ends up as a `sudo -iu <user>` target (see remotex), so an
		// injection-shaped string must be rejected structurally.
		if !sshHostPattern.MatchString(u) {
			errs = append(errs, fmt.Errorf("fleet.bifrost_users[%d]: %q is not a safe sudo target", i, u))
		}
		seen[strings.ToLower(u)]++
	}

	dupUsers := make([]string, 0, len(seen))
	for u, count := range seen {
		if count > 1 {
			dupUsers = append(dupUsers, u)
		}
	}
	sort.Strings(dupUsers) // deterministic error ordering
	for _, u := range dupUsers {
		errs = append(errs, fmt.Errorf("fleet.bifrost_users: user %q is used more than once (case-insensitive)", u))
	}

	return errs
}

// deviceErrLabel identifies a device in a ValidateDevices error message even
// when its name field is the thing being complained about.
func deviceErrLabel(index int, name string) string {
	if name == "" {
		return fmt.Sprintf("devices[%d]", index)
	}
	return fmt.Sprintf("devices[%d] (%q)", index, name)
}
