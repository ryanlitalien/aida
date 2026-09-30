package arbiter

import (
	"testing"

	"github.com/ryanlitalien/aida/internal/burndown"
)

func TestWindowKey(t *testing.T) {
	tests := []struct {
		label string
		want  string
	}{
		{"5-hour", "5h"},
		{"5-hour session", "5h"},
		{"7-day", "7d"},
		{"7-day (Sonnet)", "7d"},
		{"7-day (Fable)", "7d-fable"},
		{"Fable weekly", "7d-fable"},
		{"image generation", "image"},
		{"something unrecognized", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := WindowKey(tt.label); got != tt.want {
			t.Errorf("WindowKey(%q) = %q, want %q", tt.label, got, tt.want)
		}
	}
}

func cap5h(provider string, used, left, headroom float64) burndown.Capacity {
	return burndown.Capacity{Provider: provider, Label: "5-hour", UsedPct: used, LeftPct: left, Headroom: headroom}
}

func cap7d(provider string, used, left, headroom float64) burndown.Capacity {
	return burndown.Capacity{Provider: provider, Label: "7-day", UsedPct: used, LeftPct: left, Headroom: headroom}
}

func capSpend(provider string, used, headroom float64) burndown.Capacity {
	return burndown.Capacity{Provider: provider, Label: "spend", UsedPct: used, Headroom: headroom, IsSpend: true}
}

func TestLaneCapacityForFullyProbed(t *testing.T) {
	lane := Lane{ID: "claude-max", Provider: "Anthropic / Claude", Windows: []string{"5h", "7d"}}
	caps := []burndown.Capacity{
		cap5h("Anthropic / Claude", 2, 98, 90),
		cap7d("Anthropic / Claude", 5, 95, 85),
	}
	lc := LaneCapacityFor(lane, caps)
	if !lc.Probed {
		t.Fatal("expected Probed = true")
	}
	if len(lc.Missing) != 0 {
		t.Errorf("expected no missing windows, got %v", lc.Missing)
	}
	if len(lc.Windows) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(lc.Windows))
	}
	if lc.MaxUsedPct != 5 {
		t.Errorf("MaxUsedPct = %v, want 5", lc.MaxUsedPct)
	}
}

func TestLaneCapacityForMissingWindow(t *testing.T) {
	lane := Lane{ID: "claude-max", Provider: "Anthropic / Claude", Windows: []string{"5h", "7d"}}
	caps := []burndown.Capacity{
		cap5h("Anthropic / Claude", 2, 98, 90),
	}
	lc := LaneCapacityFor(lane, caps)
	if !lc.Probed {
		t.Fatal("expected Probed = true (5h row exists)")
	}
	if len(lc.Missing) != 1 || lc.Missing[0] != "7d" {
		t.Errorf("Missing = %v, want [7d]", lc.Missing)
	}
	if len(lc.Windows) != 1 {
		t.Errorf("expected 1 window, got %d", len(lc.Windows))
	}
}

func TestLaneCapacityForUnprobed(t *testing.T) {
	lane := Lane{ID: "qwen-ec2", Provider: "", AllowUnprobed: true}
	lc := LaneCapacityFor(lane, []burndown.Capacity{
		cap5h("Anthropic / Claude", 2, 98, 90),
	})
	if lc.Probed {
		t.Error("expected Probed = false for a lane with no matching provider rows")
	}
	if len(lc.Windows) != 0 {
		t.Errorf("expected no windows, got %v", lc.Windows)
	}
	if len(lc.Missing) != 0 {
		t.Errorf("expected no missing entries when unprobed (Windows is empty), got %v", lc.Missing)
	}
}

func TestLaneCapacityForIgnoresUnrelatedWindows(t *testing.T) {
	lane := Lane{ID: "claude-max", Provider: "Anthropic / Claude", Windows: []string{"5h", "7d"}}
	caps := []burndown.Capacity{
		cap5h("Anthropic / Claude", 2, 98, 90),
		cap7d("Anthropic / Claude", 2, 98, 90),
		{Provider: "Anthropic / Claude", Label: "7-day (Fable)", UsedPct: 99, Headroom: 0},
	}
	lc := LaneCapacityFor(lane, caps)
	if len(lc.Windows) != 2 {
		t.Fatalf("expected the fable row to be ignored, got %d windows: %+v", len(lc.Windows), lc.Windows)
	}
	if len(lc.Missing) != 0 {
		t.Errorf("expected no missing windows, got %v", lc.Missing)
	}
}

func TestLaneCapacityForSpendLane(t *testing.T) {
	lane := Lane{ID: "litellm", Provider: "LiteLLM proxy (minty)", Spend: true}
	caps := []burndown.Capacity{
		capSpend("LiteLLM proxy (minty)", 10, 80),
		cap5h("Anthropic / Claude", 2, 98, 90),
	}
	lc := LaneCapacityFor(lane, caps)
	if !lc.Probed {
		t.Fatal("expected Probed = true")
	}
	if len(lc.Windows) != 1 || !lc.Windows[0].IsSpend {
		t.Fatalf("expected 1 spend window, got %+v", lc.Windows)
	}
	if len(lc.Missing) != 0 {
		t.Errorf("expected no missing entries, got %v", lc.Missing)
	}
}

func TestLaneCapacityForSpendLaneMissing(t *testing.T) {
	lane := Lane{ID: "litellm", Provider: "LiteLLM proxy (minty)", Spend: true}
	caps := []burndown.Capacity{
		// Probed (provider has a row) but no spend row specifically.
		{Provider: "LiteLLM proxy (minty)", Label: "5-hour", UsedPct: 1, IsSpend: false},
	}
	lc := LaneCapacityFor(lane, caps)
	if !lc.Probed {
		t.Fatal("expected Probed = true")
	}
	if len(lc.Missing) != 1 || lc.Missing[0] != "spend" {
		t.Errorf("Missing = %v, want [spend]", lc.Missing)
	}
}

func TestNearExhaustion(t *testing.T) {
	lc := LaneCapacity{Windows: []burndown.Capacity{
		{UsedPct: 50},
		{UsedPct: 92},
	}}
	if !NearExhaustion(lc, 90) {
		t.Error("expected NearExhaustion(90) = true when a window is at 92")
	}
	if NearExhaustion(lc, 95) {
		t.Error("expected NearExhaustion(95) = false when max used is 92")
	}
}

func TestNearExhaustionNoWindows(t *testing.T) {
	if NearExhaustion(LaneCapacity{}, 90) {
		t.Error("expected NearExhaustion = false with no windows")
	}
}
