// Package fleet probes the machines declared in config.yaml's `devices:`
// block (see internal/config/devices.go) and serves the results to the
// /dashboard page through a TTL cache that never blocks its caller on
// network I/O. See cache.go for the concurrency contract.
package fleet

import "time"

// Reach describes how reachable a device is, independently of whether its
// aida daemon answered a healthz probe. A device can be ReachOnline (up on
// the tailnet) while its Aida field is still nil, e.g. for probe:
// tailscale-only devices that never get an ssh probe at all.
type Reach string

const (
	// ReachOnline means tailscale reports the node as up.
	ReachOnline Reach = "online"
	// ReachOffline means tailscale knows the node but reports it down.
	ReachOffline Reach = "offline"
	// ReachUnknown means the device isn't in tailscale's peer/self map
	// (or tailscale itself is absent/failed), or probing is disabled for
	// it (probe: none). Distinct from ReachOffline: we simply have no
	// signal either way.
	ReachUnknown Reach = "unknown"
	// ReachLocal means this device IS the process serving the dashboard.
	// Self is trusted, not probed -- see Prober.ProbeOne's local branch.
	ReachLocal Reach = "local"
)

// AidaStatus is what an ssh healthz probe (or, for probe: local, in-process
// trust) learned about the aida daemon running on a device.
type AidaStatus struct {
	Healthz bool   `json:"healthz"`
	Uptime  string `json:"uptime,omitempty"`
	Version string `json:"version,omitempty"`
}

// Hardware is what the combined hardware-inventory probe (see hardware.go)
// learned about a device: CPU model, total system RAM, GPU name + VRAM,
// and free disk space on /. Every field is a free-form, already-formatted
// display string (not structured data) -- this is a dashboard overlay, not
// something other code parses back apart. Any field can be empty when the
// underlying tool is missing or the metric doesn't apply (e.g. no GPU);
// that never fails the probe.
type Hardware struct {
	CPU string `json:"cpu,omitempty"`
	// Cores is a plain logical-core count (nproc / sysctl hw.ncpu, or a
	// config.HardwareConfig static override), kept separate from CPU's
	// free-form string so ComputeFleetTotals has a number to sum instead
	// of having to parse "16-Core Processor" back out of a display
	// string.
	Cores int    `json:"cores,omitempty"`
	Mem   string `json:"mem,omitempty"`
	GPU   string `json:"gpu,omitempty"`
	Disk  string `json:"disk,omitempty"`
}

// DeviceStatus is one card on the fleet dashboard: the device's static
// config (name, role, ...) plus whatever the most recent probe round
// learned about it.
type DeviceStatus struct {
	Name  string `json:"name"`
	Role  string `json:"role,omitempty"`
	Probe string `json:"probe,omitempty"`
	Self  bool   `json:"self,omitempty"`
	Herdr bool   `json:"herdr,omitempty"`
	Notes string `json:"notes,omitempty"`

	Reach Reach `json:"reach"`

	TailscaleIP string `json:"tailscale_ip,omitempty"`
	TailscaleOS string `json:"tailscale_os,omitempty"`
	LastSeen    string `json:"last_seen,omitempty"`

	// Aida is nil when we never attempted (or couldn't complete) an aida
	// healthz probe -- "we don't know" -- which must render differently
	// on the dashboard from a non-nil *AidaStatus with Healthz: false
	// ("known down"). Do not collapse this to a bool.
	Aida *AidaStatus `json:"aida"`

	// Hardware is nil when we never attempted (or couldn't complete) the
	// combined hardware-inventory probe -- unprobed device (probe: none
	// / tailscale-only), unreachable ssh target, or a probe round that
	// hasn't run yet. Only probe: ssh and probe: local devices that are
	// actually reachable ever get a non-nil Hardware. Same "nil means we
	// don't know" convention as Aida above.
	Hardware *Hardware `json:"hardware,omitempty"`

	// Error is a per-device probe failure message. A non-empty Error does
	// NOT fail the overall Snapshot -- one bad device never takes down
	// the whole fleet view.
	Error string `json:"error,omitempty"`

	ProbedAt    time.Time `json:"probed_at"`
	ProbeTookMs int64     `json:"probe_took_ms"`
	Stale       bool      `json:"stale"`
}

// Snapshot is the full fleet view served to the dashboard.
type Snapshot struct {
	Devices     []DeviceStatus `json:"devices"`
	GeneratedAt time.Time      `json:"generated_at"`
	// Refreshing is true when a probe round is currently in flight (cold
	// cache, or a stale one that just kicked an async refresh) -- the
	// Devices in this Snapshot are either placeholders or last-known-good.
	Refreshing bool `json:"refreshing"`
	// TailscaleOK is false when the shared `tailscale status --json` call
	// itself failed (missing binary, not logged in, etc). This is never
	// fatal to the snapshot: every device still gets a card, just with
	// Reach: ReachUnknown.
	TailscaleOK bool     `json:"tailscale_ok"`
	Warnings    []string `json:"warnings,omitempty"`

	// Totals aggregates hardware across the fleet's physical machines for
	// the dashboard's "Fleet totals" summary card. See
	// ComputeFleetTotals's doc comment (totals.go) for how it avoids
	// double-counting a WSL guest (e.g. beast-wsl) against the same
	// physical CPU/GPU its host (beast) already reports.
	Totals FleetTotals `json:"totals"`
}

// FleetTotals is the aggregate the dashboard's Fleet-totals card renders.
// Every numeric field sums only what ComputeFleetTotals could actually
// parse or was told statically -- an unparseable or missing value
// contributes 0, it never makes the whole total bail out.
type FleetTotals struct {
	// Machines counts physical machines in the fleet -- every configured
	// device EXCEPT a "-wsl" sub-node (see ComputeFleetTotals), regardless
	// of whether it's currently reachable or has any Hardware at all.
	Machines int `json:"machines"`
	Cores    int `json:"cores,omitempty"`
	// GB fields are decimal gigabytes (1e9 bytes), normalized from
	// whatever mix of Gi/GiB (binary) and G/GB (decimal) units the
	// underlying Hardware strings used -- see parseSizeToGB.
	MemGB       float64 `json:"mem_gb,omitempty"`
	GPUGB       float64 `json:"gpu_gb,omitempty"`
	DiskFreeGB  float64 `json:"disk_free_gb,omitempty"`
	DiskTotalGB float64 `json:"disk_total_gb,omitempty"`
}
