// Package burndown is the capacity view over the AI model roster's live
// usage: how much of each rate-limit window an unattended burn-down loop
// could spend right now without eating into Ryan's own interactive
// reserve. Only the capacity view is built so far -- no picker, no
// scheduler, no runners, no task dispatch. See CLAUDE.md, "Burn-down
// capacity (aida burndown)".
//
// The floors config (~/.aida/burndown.yaml, config.Config.BurndownPath)
// is loaded here, the same pattern internal/models/roster.go follows for
// ~/.aida/models.yaml: this package only reads it and never writes it.
package burndown

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the parsed shape of the burn-down floors config
// (examples/burndown.yaml, normally installed at ~/.aida/burndown.yaml).
type Config struct {
	Meta      Meta                  `yaml:"meta,omitempty"`
	Floors    map[string]FloorEntry `yaml:"floors"`
	Overnight OvernightConfig       `yaml:"overnight"`
	Pace      PaceConfig            `yaml:"pace"`
}

// Meta is free-standing bookkeeping about when the floors were last
// transcribed from the design doc's own data files.
type Meta struct {
	Compiled string `yaml:"compiled,omitempty"`
}

// FloorEntry is one provider's floor configuration: per-window percent
// floors for rate-limit bars (Windows), a flat monthly dollar floor for a
// budget row (Monthly), or both -- CapacityFor and CapacityForSpend each
// read only the one they need.
//
// Provider must match a provider's `label:` field in ~/.aida/models.yaml
// exactly (e.g. "Anthropic / Claude") -- that's how a live usage bar or
// spend row gets matched back to its floor. An entry naming a provider
// that isn't in the live roster (yet) is not an error; it simply never
// matches anything.
type FloorEntry struct {
	Provider string                 `yaml:"provider,omitempty"`
	Windows  map[string]WindowFloor `yaml:"windows,omitempty"`
	Monthly  *MonthlyFloor          `yaml:"monthly,omitempty"`
}

// WindowFloor is the percent floor for one rate-limit window, by time of
// day. Overnight and Last24hBeforeReset are pointers so an explicit 0
// (drain to zero) is distinguishable from "not configured" (nil, falls
// back to Daytime) -- see CapacityFor's floor rule for exactly how these
// three combine.
type WindowFloor struct {
	Daytime            float64  `yaml:"daytime"`
	Overnight          *float64 `yaml:"overnight,omitempty"`
	Last24hBeforeReset *float64 `yaml:"last_24h_before_reset,omitempty"`
	// HardFloor is an absolute minimum this window's floor can never go
	// below, regardless of pace or drain-to-zero (e.g. Fable's "never
	// below half, any day"). Zero (unset) means no hard floor.
	HardFloor *float64 `yaml:"hard_floor,omitempty"`
	Note      string   `yaml:"note,omitempty"`
}

// MonthlyFloor is a flat dollar floor for a budget row (a LiteLLM key, a
// company Bedrock cap) that doesn't have a rate-limit window shape.
type MonthlyFloor struct {
	FloorUSD float64 `yaml:"floor_usd"`
	Note     string  `yaml:"note,omitempty"`
}

// OvernightConfig is the window the burn-down runs unattended in --
// after Start, before End (both "HH:MM", 24h, wrapping past midnight),
// local to Timezone (an IANA zone name, e.g. "America/New_York").
type OvernightConfig struct {
	Start    string `yaml:"start"`
	End      string `yaml:"end"`
	Timezone string `yaml:"timezone"`
}

// PaceConfig holds the burn-down's pace-derived floor math.
type PaceConfig struct {
	// SafetyMultiplier scales a pace-projected personal consumption
	// before it becomes a floor -- see CapacityFor.
	SafetyMultiplier float64 `yaml:"safety_multiplier"`
}

// Load reads and parses the burn-down floors config at path
// (config.Config's BurndownPath, normally ~/.aida/burndown.yaml). Like
// models.Load, a missing file IS an error here: the floors config is
// small, hand-maintained, and expected to exist once this feature is in
// use, so silently returning an empty Config would hide a real
// misconfiguration behind a burn-down that reports "no floor configured"
// for everything instead of a clear error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading burndown config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing burndown config %q: %w", path, err)
	}
	return &c, nil
}

// findFloorEntry returns the floors entry whose Provider field matches
// providerLabel exactly (a live roster provider's own Label, e.g.
// "Anthropic / Claude"), or ok=false when no entry names this provider.
// A nil Config or an unmatched provider are both treated the same way by
// callers: "no floor configured", never a crash.
func (c *Config) findFloorEntry(providerLabel string) (FloorEntry, bool) {
	if c == nil {
		return FloorEntry{}, false
	}
	for _, e := range c.Floors {
		if e.Provider != "" && e.Provider == providerLabel {
			return e, true
		}
	}
	return FloorEntry{}, false
}
