package burndown

import (
	"os"
	"path/filepath"
	"testing"
)

const testBurndownYAML = `
meta:
  compiled: 2026-09-11

floors:
  anthropic-max:
    provider: "Anthropic / Claude"
    windows:
      5h: {daytime: 30, overnight: 0}
      7d: {daytime: 30, last_24h_before_reset: 0}
      7d-fable: {daytime: 50, overnight: 50, hard_floor: 50, note: "never below half"}
  litellm-keys:
    provider: "LiteLLM proxy (minty)"
    monthly: {floor_usd: 2, note: "keep some for Jarvis"}

overnight:
  start: "23:00"
  end: "07:00"
  timezone: "America/New_York"

pace:
  safety_multiplier: 1.25
`

func loadTestConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "burndown.yaml")
	if err := os.WriteFile(path, []byte(testBurndownYAML), 0644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestLoad(t *testing.T) {
	c := loadTestConfig(t)

	if c.Meta.Compiled != "2026-09-11" {
		t.Errorf("Meta.Compiled = %q, want 2026-09-11", c.Meta.Compiled)
	}
	if len(c.Floors) != 2 {
		t.Fatalf("Floors = %d entries, want 2", len(c.Floors))
	}
	if c.Overnight.Start != "23:00" || c.Overnight.End != "07:00" {
		t.Errorf("Overnight = %+v, want start 23:00 end 07:00", c.Overnight)
	}
	if c.Overnight.Timezone != "America/New_York" {
		t.Errorf("Overnight.Timezone = %q", c.Overnight.Timezone)
	}
	if c.Pace.SafetyMultiplier != 1.25 {
		t.Errorf("Pace.SafetyMultiplier = %v, want 1.25", c.Pace.SafetyMultiplier)
	}

	entry, ok := c.Floors["anthropic-max"]
	if !ok {
		t.Fatal("anthropic-max floor entry missing")
	}
	if entry.Provider != "Anthropic / Claude" {
		t.Errorf("anthropic-max.Provider = %q", entry.Provider)
	}
	wf, ok := entry.Windows["5h"]
	if !ok {
		t.Fatal("anthropic-max 5h window missing")
	}
	if wf.Daytime != 30 {
		t.Errorf("5h.Daytime = %v, want 30", wf.Daytime)
	}
	if wf.Overnight == nil || *wf.Overnight != 0 {
		t.Errorf("5h.Overnight = %v, want pointer to 0", wf.Overnight)
	}

	fable, ok := entry.Windows["7d-fable"]
	if !ok {
		t.Fatal("anthropic-max 7d-fable window missing")
	}
	if fable.HardFloor == nil || *fable.HardFloor != 50 {
		t.Errorf("7d-fable.HardFloor = %v, want pointer to 50", fable.HardFloor)
	}

	litellm, ok := c.Floors["litellm-keys"]
	if !ok {
		t.Fatal("litellm-keys floor entry missing")
	}
	if litellm.Monthly == nil || litellm.Monthly.FloorUSD != 2 {
		t.Errorf("litellm-keys.Monthly = %+v, want floor_usd 2", litellm.Monthly)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected an error for a missing burndown config, got nil")
	}
}

// TestLoad_RealExample loads the actual examples/burndown.yaml shipped in
// this repo, so a future edit to that file that breaks parsing (a bad
// indent, a duplicate key) fails CI instead of only being caught the next
// time someone runs `aida burndown capacity` by hand.
func TestLoad_RealExample(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "burndown.yaml")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load(examples/burndown.yaml): %v", err)
	}
	if len(c.Floors) == 0 {
		t.Error("examples/burndown.yaml parsed with zero floors entries")
	}
	if c.Overnight.Timezone == "" {
		t.Error("examples/burndown.yaml parsed with no overnight timezone")
	}
	if c.Pace.SafetyMultiplier <= 0 {
		t.Errorf("examples/burndown.yaml Pace.SafetyMultiplier = %v, want > 0", c.Pace.SafetyMultiplier)
	}
}

func TestFindFloorEntry(t *testing.T) {
	c := loadTestConfig(t)

	if _, ok := c.findFloorEntry("Anthropic / Claude"); !ok {
		t.Error("findFloorEntry(\"Anthropic / Claude\") = not found, want found")
	}
	if _, ok := c.findFloorEntry("Some Unknown Provider"); ok {
		t.Error("findFloorEntry(unknown) = found, want not found")
	}

	var nilConfig *Config
	if _, ok := nilConfig.findFloorEntry("Anthropic / Claude"); ok {
		t.Error("findFloorEntry on a nil Config = found, want not found (and no panic)")
	}
}
