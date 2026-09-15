package fleet

import (
	"regexp"
	"strconv"
	"strings"
)

// ComputeFleetTotals aggregates hardware across the fleet's physical
// machines for the dashboard's "Fleet totals" card.
//
// A "-wsl" sub-node (beast-wsl, and wanda-wsl should it ever carry
// Hardware) is EXCLUDED entirely -- both from Machines and from every sum.
// beast-wsl runs INSIDE beast and its live probe reports the SAME
// physical CPU and the SAME RTX 4070 beast's own (static) Hardware
// already accounts for; summing both would double-count one machine's
// worth of cores/RAM/GPU as two. Its device card still renders normally
// (see dashboard_web.html) -- it's only the totals that skip it.
//
// Machines counts every non-wsl device regardless of reach or whether it
// has any Hardware at all (an offline/unprobed machine is still a real
// physical machine); the numeric sums only add what a device's Hardware
// actually parsed.
func ComputeFleetTotals(devices []DeviceStatus) FleetTotals {
	var t FleetTotals
	for _, d := range devices {
		if isWSLSubNode(d.Name) {
			continue
		}
		t.Machines++
		if d.Hardware == nil {
			continue
		}
		hw := d.Hardware
		t.Cores += hw.Cores
		if gb, ok := parseSizeToGB(hw.Mem); ok {
			t.MemGB += gb
		}
		t.GPUGB += parseGPUVRAMGB(hw.GPU)
		free, total, ok := parseDiskGB(hw.Disk)
		if ok {
			t.DiskFreeGB += free
			t.DiskTotalGB += total
		}
	}
	return t
}

// isWSLSubNode reports whether name identifies a WSL guest sub-node of
// some other declared device (by convention, a "-wsl" name suffix -- e.g.
// beast-wsl is the Linux side living inside the beast device). Case-
// insensitive since device names are free-form operator text.
func isWSLSubNode(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), "-wsl")
}

// sizeRe matches a leading numeric value and a binary-or-decimal size
// unit: G/GB (decimal gigabyte), Gi/GiB (binary gibibyte), and the T/M/K
// variants of both, case-insensitive. Used for both plain size strings
// (Hardware.Mem, Hardware.Disk) and the "(NNN unit)" VRAM suffix on
// Hardware.GPU.
var sizeRe = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*([kmgt]i?b?)\s*$`)

// unitToGB returns the multiplier that converts a value in the given unit
// to decimal gigabytes (1e9 bytes), distinguishing binary units (Gi/GiB,
// Mi/MiB, ...) from decimal ones (GB, MB, ...) -- e.g. "23Gi" (from
// Linux's `free -h`) and "24GB" (from this package's own Darwin memsize
// calc) must both land in the same GB-denominated total rather than being
// summed as if the units matched.
func unitToGB(unit string) (float64, bool) {
	switch strings.ToLower(unit) {
	case "g", "gb":
		return 1, true
	case "gi", "gib":
		return 1.073741824, true
	case "t", "tb":
		return 1000, true
	case "ti", "tib":
		return 1099.511627776, true
	case "m", "mb":
		return 0.001, true
	case "mi", "mib":
		return 0.001048576, true
	case "k", "kb":
		return 0.000001, true
	case "ki", "kib":
		return 0.000001048576, true
	default:
		return 0, false
	}
}

// parseSizeToGB parses a plain size string like "23Gi", "24GB", "16GB",
// or "930GB" into decimal gigabytes. ok is false for anything it doesn't
// recognize (empty string, unexpected unit, ...) -- callers must treat
// that as "contributes nothing," never as a hard error.
func parseSizeToGB(s string) (gb float64, ok bool) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	mult, ok := unitToGB(m[2])
	if !ok {
		return 0, false
	}
	return val * mult, true
}

// diskFreeOfRe splits Hardware.Disk's live-probe shape, "<free> free of
// <total>" (e.g. "44G free of 926G"), into its two size strings.
var diskFreeOfRe = regexp.MustCompile(`(?i)^\s*(.+?)\s+free of\s+(.+?)\s*$`)

// parseDiskGB parses Hardware.Disk into (free, total) decimal gigabytes.
// Two shapes are recognized: the live-probe "<free> free of <total>"
// form, and a bare capacity string with no free-space figure -- the shape
// a hand-entered config.yaml hardware.disk override uses (e.g. beast's
// "930GB", since there's no way to learn free space on a device aida
// never probes live). A bare capacity counts only toward total, never
// toward free -- we genuinely don't know free space for it, and
// attributing it all to "free" would overstate what's actually available.
func parseDiskGB(s string) (free, total float64, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}
	if m := diskFreeOfRe.FindStringSubmatch(s); m != nil {
		f, fok := parseSizeToGB(m[1])
		t, tok := parseSizeToGB(m[2])
		if fok && tok {
			return f, t, true
		}
		return 0, 0, false
	}
	if t, tok := parseSizeToGB(s); tok {
		return 0, t, true
	}
	return 0, 0, false
}

// gpuVRAMRe pulls the trailing "(NNN unit)" VRAM figure off one GPU
// entry, e.g. "NVIDIA GeForce RTX 4070 (12282 MiB)" or a hand-entered
// "NVIDIA GeForce RTX 4070 (12 GB)". A GPU string with no parenthetical
// (integrated graphics with no discrete VRAM figure, e.g. "Apple M4 Pro")
// simply contributes 0 -- that's correct, not a parse failure, since
// unified memory is already counted in the fleet's RAM total.
var gpuVRAMRe = regexp.MustCompile(`(?i)\(\s*([0-9]+(?:\.[0-9]+)?)\s*([kmgt]i?b?)\s*\)\s*$`)

// parseGPUVRAMGB sums VRAM across every ';'-separated GPU entry in a
// formatGPU-produced Hardware.GPU string, converting each to decimal
// gigabytes. Never errors -- an entry it can't parse just adds 0.
func parseGPUVRAMGB(raw string) float64 {
	if raw == "" {
		return 0
	}
	var sum float64
	for _, part := range strings.Split(raw, ";") {
		m := gpuVRAMRe.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			continue
		}
		val, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		mult, ok := unitToGB(m[2])
		if !ok {
			continue
		}
		sum += val * mult
	}
	return sum
}
