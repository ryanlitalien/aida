package burndown

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/models"
)

// testFloorsYAML is the anthropic-max / anthropic-pro-bs / openai-plus /
// google-ai-pro / litellm-keys shape from examples/burndown.yaml, trimmed
// to exactly what these tests exercise.
const testFloorsYAML = `
floors:
  anthropic-max:
    provider: "Anthropic / Claude"
    windows:
      5h: {daytime: 30, overnight: 0}
      7d: {daytime: 30, last_24h_before_reset: 0}
      7d-fable: {daytime: 50, overnight: 50, hard_floor: 50}
  anthropic-pro-bs:
    provider: "Anthropic / Claude Pro (Acme Widgets)"
    windows:
      7d: {daytime: 15, last_24h_before_reset: 0}
  openai-plus:
    provider: "OpenAI / ChatGPT + Codex"
    windows:
      5h: {daytime: 40, overnight: 0}
      7d: {daytime: 25, last_24h_before_reset: 0}
  google-ai-pro:
    provider: "Google / Gemini"
    windows:
      7d: {daytime: 20, last_24h_before_reset: 0}
  litellm-keys:
    provider: "LiteLLM proxy (minty)"
    monthly: {floor_usd: 2, note: "keep $2 for Jarvis"}

overnight:
  start: "23:00"
  end: "07:00"
  timezone: "America/New_York"

pace:
  safety_multiplier: 1.25
`

func testConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "burndown.yaml")
	if err := os.WriteFile(path, []byte(testFloorsYAML), 0644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

// A fixed "daytime" instant well outside the 23:00-07:00 ET overnight
// window and more than 24h from any test bar's reset, so the daytime
// static floor branch applies unless a test says otherwise.
func daytimeNow(t *testing.T) time.Time {
	t.Helper()
	loc := mustLoc(t, "America/New_York")
	return time.Date(2026, 9, 14, 14, 0, 0, 0, loc)
}

func approxEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.01
}

func TestCapacityFor_TableDriven(t *testing.T) {
	cfg := testConfig(t)
	now := daytimeNow(t)
	farFuture := now.Add(100 * time.Hour)

	tests := []struct {
		name         string
		provider     string
		bar          models.Bar
		wantFloor    float64
		wantHeadroom float64
		wantSource   string
	}{
		{
			// Codex 7-day, hot: the loop correctly takes nothing.
			name:     "codex 7-day hot",
			provider: "OpenAI / ChatGPT + Codex",
			bar: models.Bar{
				Label: "7-day", Percent: 39, Left: 61, ResetsAt: farFuture,
				Pace: &models.Pace{Verdict: models.PaceHot, Projected: 285},
			},
			wantFloor: 61, wantHeadroom: 0, wantSource: FloorSourcePace,
		},
		{
			// Gemini 7-day, idle: the window is unlocked.
			name:     "gemini 7-day idle",
			provider: "Google / Gemini",
			bar: models.Bar{
				Label: "7-day Gemini", Percent: 10, Left: 90, ResetsAt: farFuture,
				Pace: &models.Pace{Verdict: models.PaceIdle, Projected: 25},
			},
			wantFloor: 18.75, wantHeadroom: 71.25, wantSource: FloorSourcePace,
		},
		{
			name:     "max 7-day on pace",
			provider: "Anthropic / Claude",
			bar: models.Bar{
				Label: "7-day", Percent: 40, Left: 60, ResetsAt: farFuture,
				Pace: &models.Pace{Verdict: models.PaceOnPace, Projected: 85},
			},
			wantFloor: 56.25, wantHeadroom: 3.75, wantSource: FloorSourcePace,
		},
		{
			// hello@ Pro 7-day: pace floor clamps down to what's left.
			name:     "hello pro 7-day clamped to left",
			provider: "Anthropic / Claude Pro (Acme Widgets)",
			bar: models.Bar{
				Label: "7-day", Percent: 81, Left: 19, ResetsAt: farFuture,
				Pace: &models.Pace{Verdict: models.PaceHot, Projected: 99},
			},
			wantFloor: 19, wantHeadroom: 0, wantSource: FloorSourcePace,
		},
		{
			// Max "7-day Fable", active: additional 55 * 1.25 = 68.75,
			// above the 50 hard floor -- BUT the floor can never exceed
			// what's left (Left = 100-49 = 51), so it clamps down to 51,
			// not 68.75. The design doc's worked example states 68.75
			// without applying this clamp; see the report for why this
			// test asserts the mathematically consistent value instead
			// (rule 3 is explicit: "the floor can never exceed LeftPct").
			name:     "fable active clamped to left not to raw pace floor",
			provider: "Anthropic / Claude",
			bar: models.Bar{
				Label: "7-day Fable", Percent: 49, Left: 51, ResetsAt: farFuture,
				Pace: &models.Pace{Verdict: models.PaceHot, Projected: 104},
			},
			wantFloor: 51, wantHeadroom: 0, wantSource: FloorSourcePace,
		},
		{
			// Max "7-day Fable", idle: pace floor (12.5) is below the
			// hard floor (50), so it clamps UP to 50.
			name:     "fable idle clamps up to hard floor",
			provider: "Anthropic / Claude",
			bar: models.Bar{
				Label: "7-day Fable", Percent: 10, Left: 90, ResetsAt: farFuture,
				Pace: &models.Pace{Verdict: models.PaceIdle, Projected: 20},
			},
			wantFloor: 50, wantHeadroom: 40, wantSource: FloorSourceHard,
		},
		{
			// A 5-hour bar: Pace is nil (PaceFor never scores a
			// sub-24h window), so the static daytime floor applies.
			name:     "5-hour bar no pace uses static floor",
			provider: "OpenAI / ChatGPT + Codex",
			bar: models.Bar{
				Label: "5-hour", Percent: 20, Left: 80, ResetsAt: farFuture,
				Pace: nil,
			},
			wantFloor: 40, wantHeadroom: 40, wantSource: FloorSourceStatic,
		},
		{
			// PaceEarly: too little of the window has elapsed for a
			// verdict, so it falls back to the static floor exactly
			// like an unpaced bar.
			name:     "pace early uses static floor",
			provider: "Anthropic / Claude",
			bar: models.Bar{
				Label: "7-day", Percent: 2, Left: 98, ResetsAt: farFuture,
				Pace: &models.Pace{Verdict: models.PaceEarly, Elapsed: 0.01},
			},
			wantFloor: 30, wantHeadroom: 68, wantSource: FloorSourceStatic,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CapacityFor(tt.provider, tt.bar, cfg, now)
			if !approxEqual(got.Floor, tt.wantFloor) {
				t.Errorf("Floor = %v, want %v", got.Floor, tt.wantFloor)
			}
			if !approxEqual(got.Headroom, tt.wantHeadroom) {
				t.Errorf("Headroom = %v, want %v", got.Headroom, tt.wantHeadroom)
			}
			if got.FloorSource != tt.wantSource {
				t.Errorf("FloorSource = %q, want %q", got.FloorSource, tt.wantSource)
			}
		})
	}
}

func TestCapacityFor_NoFloorConfigured(t *testing.T) {
	cfg := testConfig(t)
	now := daytimeNow(t)

	t.Run("unknown provider", func(t *testing.T) {
		bar := models.Bar{Label: "5-hour", Percent: 30, Left: 70}
		got := CapacityFor("Some Unknown Provider", bar, cfg, now)
		if got.Floor != 0 {
			t.Errorf("Floor = %v, want 0", got.Floor)
		}
		if got.Headroom != 70 {
			t.Errorf("Headroom = %v, want 70 (= LeftPct)", got.Headroom)
		}
		if got.Note != noFloorNote {
			t.Errorf("Note = %q, want %q", got.Note, noFloorNote)
		}
	})

	t.Run("known provider unrecognized window label", func(t *testing.T) {
		bar := models.Bar{Label: "3-hour mystery window", Percent: 10, Left: 90}
		got := CapacityFor("Anthropic / Claude", bar, cfg, now)
		if got.Floor != 0 || got.Headroom != 90 || got.Note != noFloorNote {
			t.Errorf("got %+v, want no-floor-configured with Headroom 90", got)
		}
	})

	t.Run("nil config never panics", func(t *testing.T) {
		bar := models.Bar{Label: "5-hour", Percent: 10, Left: 90}
		got := CapacityFor("Anthropic / Claude", bar, nil, now)
		if got.Floor != 0 || got.Headroom != 90 {
			t.Errorf("got %+v", got)
		}
	})
}

func TestCapacityFor_DrainToZero(t *testing.T) {
	cfg := testConfig(t)
	loc := mustLoc(t, "America/New_York")
	now := time.Date(2026, 9, 14, 23, 30, 0, 0, loc)

	t.Run("5-hour bar resetting before wake drains to zero", func(t *testing.T) {
		resetsAt := time.Date(2026, 9, 15, 2, 0, 0, 0, loc)
		bar := models.Bar{Label: "5-hour", Percent: 20, Left: 80, ResetsAt: resetsAt}
		got := CapacityFor("OpenAI / ChatGPT + Codex", bar, cfg, now)
		if !got.DrainToZero {
			t.Error("DrainToZero = false, want true")
		}
		if got.Floor != 0 {
			t.Errorf("Floor = %v, want 0", got.Floor)
		}
	})

	t.Run("Fable bar resetting overnight: hard floor beats drain-to-zero", func(t *testing.T) {
		resetsAt := time.Date(2026, 9, 15, 3, 0, 0, 0, loc)
		bar := models.Bar{Label: "7-day Fable", Percent: 20, Left: 80, ResetsAt: resetsAt}
		got := CapacityFor("Anthropic / Claude", bar, cfg, now)
		if !got.DrainToZero {
			t.Error("DrainToZero = false, want true")
		}
		if got.Floor != 50 {
			t.Errorf("Floor = %v, want 50 (hard floor overrides drain-to-zero)", got.Floor)
		}
		if got.FloorSource != FloorSourceHard {
			t.Errorf("FloorSource = %q, want %q", got.FloorSource, FloorSourceHard)
		}
		if got.Headroom != 30 {
			t.Errorf("Headroom = %v, want 30 (Left 80 - Floor 50)", got.Headroom)
		}
	})
}

func TestCapacityForSpend(t *testing.T) {
	cfg := testConfig(t)
	now := daytimeNow(t)

	t.Run("static floor, no pace", func(t *testing.T) {
		s := models.Spend{Key: "minty-fleet", Spend: 3, Budget: 10}
		got := CapacityForSpend("LiteLLM proxy (minty)", s, cfg, now)
		// floor_usd 2 of a $10 budget = 20%; left = 70%.
		if !approxEqual(got.Floor, 20) {
			t.Errorf("Floor = %v, want 20", got.Floor)
		}
		if !approxEqual(got.Headroom, 50) {
			t.Errorf("Headroom = %v, want 50", got.Headroom)
		}
		if got.FloorSource != FloorSourceStatic {
			t.Errorf("FloorSource = %q, want %q", got.FloorSource, FloorSourceStatic)
		}
		if got.Note == "" {
			t.Error("Note = \"\", want a dollar breakdown")
		}
	})

	t.Run("unmatched provider", func(t *testing.T) {
		s := models.Spend{Key: "some-key", Spend: 1, Budget: 10}
		got := CapacityForSpend("Unmatched Provider", s, cfg, now)
		if got.Note != noFloorNote {
			t.Errorf("Note = %q, want %q", got.Note, noFloorNote)
		}
		if got.Headroom != got.LeftPct {
			t.Errorf("Headroom = %v, want LeftPct %v", got.Headroom, got.LeftPct)
		}
	})

	t.Run("zero budget never divides by zero", func(t *testing.T) {
		s := models.Spend{Key: "minty-fleet", Spend: 0, Budget: 0}
		got := CapacityForSpend("LiteLLM proxy (minty)", s, cfg, now)
		if got.UsedPct != 0 {
			t.Errorf("UsedPct = %v, want 0", got.UsedPct)
		}
	})
}

func TestCapacityForSpend_FloorUSDIsHardFloor(t *testing.T) {
	cfg := testConfig(t)
	now := daytimeNow(t)

	// heimdall / minty-fleet: an idle 0.0x pace ("nothing ran in the last
	// window") on a $10 budget with a $2 floor_usd reserve. Before the
	// fix, a 0.0x pace floor overrode floor_usd and drove the reported
	// percent floor to 0 while the dollar breakdown still said $2.00 --
	// exactly the disagreement this guards against.
	s := models.Spend{
		Key: "heimdall", Spend: 0, Budget: 10,
		Pace: &models.Pace{Verdict: models.PaceIdle, Projected: 0, Ratio: 0},
	}
	got := CapacityForSpend("LiteLLM proxy (minty)", s, cfg, now)

	if !approxEqual(got.Floor, 20) {
		t.Errorf("Floor = %v, want 20 (floor_usd $2 of $10 budget)", got.Floor)
	}
	if !approxEqual(got.Headroom, 80) {
		t.Errorf("Headroom = %v, want 80", got.Headroom)
	}
	if got.FloorSource != FloorSourceHard {
		t.Errorf("FloorSource = %q, want %q", got.FloorSource, FloorSourceHard)
	}
	if !strings.Contains(got.Note, "floor $2.00") {
		t.Errorf("Note = %q, want \"floor $2.00\"", got.Note)
	}
	if !strings.Contains(got.Note, "headroom $8.00") {
		t.Errorf("Note = %q, want \"headroom $8.00\"", got.Note)
	}
}

// TestCapacityForSpend_FloorNoteConsistency is the assertion-style check
// the bug report asked for: Capacity.Floor (percent) and the dollar
// figures in Capacity.Note must always describe the same effective
// floor, across several budget/spend/pace combinations -- no pace
// signal, an idle pace at zero, a pace already above the reserve, and a
// hot pace clamped down to what's left. That disagreement (Floor said
// 0% while Note said $2.00) is exactly what exposed the original bug.
func TestCapacityForSpend_FloorNoteConsistency(t *testing.T) {
	cfg := testConfig(t)
	now := daytimeNow(t)

	tests := []struct {
		name string
		s    models.Spend
	}{
		{
			name: "no pace signal",
			s:    models.Spend{Key: "minty-fleet", Spend: 3, Budget: 10},
		},
		{
			name: "idle 0.0x pace cannot lower the reserve below floor_usd",
			s: models.Spend{
				Key: "heimdall", Spend: 0, Budget: 10,
				Pace: &models.Pace{Verdict: models.PaceIdle, Projected: 0, Ratio: 0},
			},
		},
		{
			name: "pace already well above floor_usd wins outright",
			s: models.Spend{
				Key: "acme-widgets", Spend: 32.81, Budget: 75,
				Pace: &models.Pace{Verdict: models.PaceOnPace, Projected: 56, Ratio: 1.05},
			},
		},
		{
			name: "hot pace clamped to what's left",
			s: models.Spend{
				Key: "minty-fleet", Spend: 9, Budget: 10,
				Pace: &models.Pace{Verdict: models.PaceHot, Projected: 95, Ratio: 3},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CapacityForSpend("LiteLLM proxy (minty)", tt.s, cfg, now)
			wantFloorUSD := got.Floor / 100 * tt.s.Budget
			wantHeadroomUSD := math.Max(0, (tt.s.Budget-tt.s.Spend)-wantFloorUSD)
			wantFloorStr := fmt.Sprintf("floor $%.2f", wantFloorUSD)
			wantHeadroomStr := fmt.Sprintf("headroom $%.2f", wantHeadroomUSD)
			if !strings.Contains(got.Note, wantFloorStr) {
				t.Errorf("Note = %q, want %q (Floor=%.4f%% of budget $%.2f)", got.Note, wantFloorStr, got.Floor, tt.s.Budget)
			}
			if !strings.Contains(got.Note, wantHeadroomStr) {
				t.Errorf("Note = %q, want %q", got.Note, wantHeadroomStr)
			}
		})
	}
}

func TestReport(t *testing.T) {
	cfg := testConfig(t)
	now := daytimeNow(t)

	providers := []models.Provider{
		{Name: "anthropic", Label: "Anthropic / Claude"},
		{Name: "litellm", Label: "LiteLLM proxy (minty)"},
	}
	usages := []models.Usage{
		{
			Bars: []models.Bar{
				{Label: "5-hour", Percent: 10, Left: 90},
				{Label: "7-day", Percent: 40, Left: 60},
			},
		},
		{
			Spend: []models.Spend{
				{Key: "minty-fleet", Spend: 3, Budget: 10},
			},
		},
	}

	report := Report(providers, usages, cfg, now)
	if len(report) != 3 {
		t.Fatalf("Report returned %d entries, want 3", len(report))
	}
	if report[0].Provider != "Anthropic / Claude" || report[0].Label != "5-hour" {
		t.Errorf("report[0] = %+v", report[0])
	}
	if report[2].Provider != "LiteLLM proxy (minty)" || report[2].Label != "minty-fleet" {
		t.Errorf("report[2] = %+v", report[2])
	}
}

func TestReport_ExcludesAntigravityClaudeGPTBars(t *testing.T) {
	cfg := testConfig(t)
	now := daytimeNow(t)

	providers := []models.Provider{
		{Name: "google-ai", Label: "Google / Gemini"},
	}
	usages := []models.Usage{
		{
			Bars: []models.Bar{
				{Label: "5-hour Gemini", Percent: 10, Left: 90},
				{Label: "7-day Gemini", Percent: 20, Left: 80},
				{Label: "5-hour Claude/GPT", Percent: 30, Left: 70},
				{Label: "7-day Claude/GPT", Percent: 40, Left: 60},
			},
		},
	}

	report := Report(providers, usages, cfg, now)
	if len(report) != 2 {
		t.Fatalf("Report returned %d entries, want 2 (Claude/GPT bars excluded); got %+v", len(report), report)
	}
	for _, c := range report {
		if strings.Contains(c.Label, "Claude/GPT") {
			t.Errorf("Report included a Claude/GPT bar: %+v", c)
		}
	}
}

func TestIsAntigravityClaudeGPTBar(t *testing.T) {
	tests := []struct {
		label string
		want  bool
	}{
		{"5-hour Claude/GPT", true},
		{"7-day Claude/GPT", true},
		{"5-hour Gemini", false},
		{"7-day Gemini", false},
		{"7-day", false},
	}
	for _, tt := range tests {
		if got := isAntigravityClaudeGPTBar(tt.label); got != tt.want {
			t.Errorf("isAntigravityClaudeGPTBar(%q) = %v, want %v", tt.label, got, tt.want)
		}
	}
}

func TestIsOvernight(t *testing.T) {
	o := OvernightConfig{Start: "23:00", End: "07:00", Timezone: "America/New_York"}
	loc := mustLoc(t, "America/New_York")

	if !IsOvernight(o, time.Date(2026, 9, 14, 23, 30, 0, 0, loc)) {
		t.Error("23:30 should be overnight")
	}
	if !IsOvernight(o, time.Date(2026, 9, 15, 2, 0, 0, 0, loc)) {
		t.Error("02:00 should be overnight")
	}
	if IsOvernight(o, time.Date(2026, 9, 14, 14, 0, 0, 0, loc)) {
		t.Error("14:00 should not be overnight")
	}
}
