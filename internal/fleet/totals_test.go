package fleet

import "testing"

func TestParseSizeToGB(t *testing.T) {
	tests := []struct {
		in     string
		wantGB float64
		wantOK bool
	}{
		{"24GB", 24, true},
		{"16GB", 16, true},
		{"930GB", 930, true},
		{"23Gi", 23 * 1.073741824, true},
		{"9.7Gi", 9.7 * 1.073741824, true},
		{"", 0, false},
		{"garbage", 0, false},
		{"12 GB", 12, true}, // internal whitespace before the unit
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			gb, ok := parseSizeToGB(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("parseSizeToGB(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			}
			if ok && !approxEqual(gb, tt.wantGB, 0.001) {
				t.Errorf("parseSizeToGB(%q) = %v, want %v", tt.in, gb, tt.wantGB)
			}
		})
	}
}

func TestParseDiskGB(t *testing.T) {
	t.Run("free of total", func(t *testing.T) {
		free, total, ok := parseDiskGB("44G free of 926G")
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if free != 44 || total != 926 {
			t.Errorf("free=%v total=%v, want 44/926", free, total)
		}
	})
	t.Run("binary units free of total", func(t *testing.T) {
		free, total, ok := parseDiskGB("30Gi free of 460Gi")
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if !approxEqual(free, 30*1.073741824, 0.001) || !approxEqual(total, 460*1.073741824, 0.001) {
			t.Errorf("free=%v total=%v", free, total)
		}
	})
	t.Run("bare capacity counts only toward total", func(t *testing.T) {
		free, total, ok := parseDiskGB("930GB")
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if free != 0 {
			t.Errorf("free = %v, want 0 (unknown free space must not be assumed)", free)
		}
		if total != 930 {
			t.Errorf("total = %v, want 930", total)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, _, ok := parseDiskGB(""); ok {
			t.Error("ok = true, want false for empty string")
		}
	})
}

func TestParseGPUVRAMGB(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		wantGB float64
	}{
		{"empty", "", 0},
		{"no parenthetical (integrated GPU)", "Apple M4 Pro", 0},
		{"MiB figure", "NVIDIA GeForce RTX 4070 (12282 MiB)", 12282 * 0.001048576},
		{"static GB figure", "NVIDIA GeForce RTX 4070 (12 GB)", 12},
		{
			"two GPUs summed",
			"NVIDIA T4 (16384 MiB); NVIDIA T4 (16384 MiB)",
			2 * 16384 * 0.001048576,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGPUVRAMGB(tt.raw); !approxEqual(got, tt.wantGB, 0.001) {
				t.Errorf("parseGPUVRAMGB(%q) = %v, want %v", tt.raw, got, tt.wantGB)
			}
		})
	}
}

// TestComputeFleetTotalsExcludesWSLSubNodes is the critical no-double-
// counting regression: beast-wsl runs INSIDE beast and its live probe
// would report the SAME physical CPU and the SAME RTX 4070 beast's own
// (static) Hardware already carries. Only beast may contribute to the
// sums; beast-wsl must be skipped entirely, even though it still gets a
// device card in the Devices list.
func TestComputeFleetTotalsExcludesWSLSubNodes(t *testing.T) {
	devices := []DeviceStatus{
		{
			Name: "beast",
			Hardware: &Hardware{
				CPU: "Intel Core i7-12700F (18 threads)", Cores: 18,
				Mem: "16GB", GPU: "NVIDIA GeForce RTX 4070 (12 GB)", Disk: "930GB",
			},
		},
		{
			Name: "beast-wsl",
			Hardware: &Hardware{
				CPU: "12th Gen Intel(R) Core(TM) i7-12700F", Cores: 8,
				Mem: "9.7Gi", GPU: "NVIDIA GeForce RTX 4070 (12282 MiB)", Disk: "767G free of 1007G",
			},
		},
		{Name: "wanda-wsl", Hardware: nil}, // offline, tailscale-only, never gets Hardware anyway
	}

	got := ComputeFleetTotals(devices)

	// Machines: beast counts, beast-wsl and wanda-wsl (both "-wsl") do not.
	if got.Machines != 1 {
		t.Errorf("Machines = %d, want 1 (beast-wsl and wanda-wsl excluded)", got.Machines)
	}
	if got.Cores != 18 {
		t.Errorf("Cores = %d, want 18 (only beast's static value, not beast-wsl's 8 on top)", got.Cores)
	}
	if !approxEqual(got.MemGB, 16, 0.01) {
		t.Errorf("MemGB = %v, want 16 (beast-wsl's 9.7Gi must not be added)", got.MemGB)
	}
	if !approxEqual(got.GPUGB, 12, 0.01) {
		t.Errorf("GPUGB = %v, want 12 (a single RTX 4070's worth, not doubled)", got.GPUGB)
	}
	if got.DiskFreeGB != 0 {
		t.Errorf("DiskFreeGB = %v, want 0 (beast's static disk has no free figure, beast-wsl excluded)", got.DiskFreeGB)
	}
	if !approxEqual(got.DiskTotalGB, 930, 0.01) {
		t.Errorf("DiskTotalGB = %v, want 930", got.DiskTotalGB)
	}
}

func TestComputeFleetTotalsCountsOfflineMachinesWithoutHardware(t *testing.T) {
	devices := []DeviceStatus{
		{Name: "edith", Hardware: &Hardware{Mem: "24GB"}},
		{Name: "photon", Hardware: nil}, // offline: still a real physical machine
	}

	got := ComputeFleetTotals(devices)

	if got.Machines != 2 {
		t.Errorf("Machines = %d, want 2 (photon still counts even with no Hardware)", got.Machines)
	}
	if !approxEqual(got.MemGB, 24, 0.01) {
		t.Errorf("MemGB = %v, want 24", got.MemGB)
	}
}

func TestComputeFleetTotalsEmpty(t *testing.T) {
	got := ComputeFleetTotals(nil)
	if got.Machines != 0 || got.Cores != 0 || got.MemGB != 0 || got.GPUGB != 0 || got.DiskFreeGB != 0 || got.DiskTotalGB != 0 {
		t.Errorf("ComputeFleetTotals(nil) = %+v, want the zero value", got)
	}
}

func approxEqual(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}
