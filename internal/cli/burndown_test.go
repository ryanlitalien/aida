package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/burndown"
	"github.com/ryanlitalien/aida/internal/models"
)

func TestFormatCapacityLine_Static(t *testing.T) {
	c := burndown.Capacity{
		Label: "5-hour", LeftPct: 94, Floor: 30, FloorSource: burndown.FloorSourceStatic, Headroom: 64,
	}
	got := formatCapacityLine(c)
	if !strings.Contains(got, "94% left") {
		t.Errorf("formatCapacityLine = %q, want 94%% left", got)
	}
	if !strings.Contains(got, "30%") || !strings.Contains(got, "(static)") {
		t.Errorf("formatCapacityLine = %q, want a static floor of 30%%", got)
	}
	if !strings.Contains(got, "headroom") || !strings.Contains(got, "64%") {
		t.Errorf("formatCapacityLine = %q, want headroom 64%%", got)
	}
}

func TestFormatCapacityLine_Hot(t *testing.T) {
	c := burndown.Capacity{
		Label: "7-day", LeftPct: 61, Floor: 61, FloorSource: burndown.FloorSourcePace, Headroom: 0,
		Pace: &models.Pace{Verdict: models.PaceHot, Ratio: 2.9},
	}
	got := formatCapacityLine(c)
	if !strings.Contains(got, "(pace)") {
		t.Errorf("formatCapacityLine = %q, want source pace", got)
	}
	if !strings.Contains(got, "HOT") || !strings.Contains(got, "2.9x") {
		t.Errorf("formatCapacityLine = %q, want a HOT 2.9x annotation", got)
	}
}

func TestFormatCapacityLine_Idle(t *testing.T) {
	c := burndown.Capacity{
		Label: "7-day Gemini", LeftPct: 90, Floor: 18.75, FloorSource: burndown.FloorSourcePace, Headroom: 71.25,
		Pace: &models.Pace{Verdict: models.PaceIdle, Ratio: 0.2},
	}
	got := formatCapacityLine(c)
	if !strings.Contains(got, "IDLE") || !strings.Contains(got, "0.2x") {
		t.Errorf("formatCapacityLine = %q, want an IDLE 0.2x annotation", got)
	}
}

func TestFormatCapacityLine_DrainToZero(t *testing.T) {
	c := burndown.Capacity{
		Label: "5-hour", LeftPct: 80, Floor: 0, FloorSource: burndown.FloorSourceDrainToZero, Headroom: 80,
		DrainToZero: true,
	}
	got := formatCapacityLine(c)
	if !strings.Contains(got, "DRAIN") {
		t.Errorf("formatCapacityLine = %q, want a DRAIN marker", got)
	}
	if !strings.Contains(got, "(drain-to-zero)") {
		t.Errorf("formatCapacityLine = %q, want source drain-to-zero", got)
	}
}

func TestFormatCapacityLine_NoFloorConfigured(t *testing.T) {
	c := burndown.Capacity{
		Label: "5-hour", LeftPct: 70, Floor: 0, FloorSource: "", Headroom: 70,
		Note: "no floor configured",
	}
	got := formatCapacityLine(c)
	if !strings.Contains(got, "no floor configured") {
		t.Errorf("formatCapacityLine = %q, want \"no floor configured\"", got)
	}
	if strings.Contains(got, "headroom") {
		t.Errorf("formatCapacityLine = %q, want no headroom field when unconfigured", got)
	}
	// The note shouldn't ALSO be appended in parens -- it's already the
	// floor field's own text.
	if strings.Count(got, "no floor configured") != 1 {
		t.Errorf("formatCapacityLine = %q, want \"no floor configured\" exactly once", got)
	}
}

func TestFormatCapacityLine_NoteAppended(t *testing.T) {
	c := burndown.Capacity{
		Label: "7-day Fable", LeftPct: 51, Floor: 50, FloorSource: burndown.FloorSourceHard, Headroom: 1,
		Note: "never below half",
	}
	got := formatCapacityLine(c)
	if !strings.Contains(got, "never below half") {
		t.Errorf("formatCapacityLine = %q, want the config note appended", got)
	}
}

func TestFormatBurndownHeader(t *testing.T) {
	now := time.Date(2026, 9, 14, 21, 14, 0, 0, time.UTC)
	got := formatBurndownHeader(now, time.UTC, false)
	if !strings.Contains(got, "daytime floors") {
		t.Errorf("formatBurndownHeader(daytime) = %q", got)
	}
	got = formatBurndownHeader(now, time.UTC, true)
	if !strings.Contains(got, "overnight floors") {
		t.Errorf("formatBurndownHeader(overnight) = %q", got)
	}
}

func TestFormatBurndownSummary(t *testing.T) {
	groups := []burndownGroup{
		{
			Label: "Anthropic / Claude",
			Capacities: []burndown.Capacity{
				{Provider: "Anthropic / Claude", Label: "5-hour", Headroom: 64},
				{Provider: "Anthropic / Claude", Label: "7-day", Headroom: 0},
			},
		},
		{
			Label: "Google / Gemini",
			Capacities: []burndown.Capacity{
				{Provider: "Google / Gemini", Label: "7-day Gemini", Headroom: 71.25},
			},
		},
	}
	got := formatBurndownSummary(groups)
	if !strings.Contains(got, "2 windows") {
		t.Errorf("formatBurndownSummary = %q, want 2 windows (only strictly-positive headroom counts)", got)
	}
	if !strings.Contains(got, "Google / Gemini 7-day Gemini") {
		t.Errorf("formatBurndownSummary = %q, want the largest headroom window named", got)
	}
	if !strings.Contains(got, "71%") {
		t.Errorf("formatBurndownSummary = %q, want 71%%", got)
	}
}

func TestFormatBurndownSummary_ExcludesSpendRowsFromLargestPick(t *testing.T) {
	groups := []burndownGroup{
		{
			Label: "LiteLLM proxy (minty)",
			Capacities: []burndown.Capacity{
				{Provider: "LiteLLM proxy (minty)", Label: "heimdall", Headroom: 80, IsSpend: true},
				{Provider: "LiteLLM proxy (minty)", Label: "minty-fleet", Headroom: 80, IsSpend: true},
			},
		},
		{
			Label: "Google / Gemini",
			Capacities: []burndown.Capacity{
				{Provider: "Google / Gemini", Label: "7-day Gemini", Headroom: 10},
			},
		},
	}
	got := formatBurndownSummary(groups)
	// Both LiteLLM spend rows have more headroom (80%) than the Gemini
	// window (10%), but a dollar reserve isn't a quota window -- the
	// quota window must still win the "largest" pick.
	if !strings.Contains(got, "Google / Gemini 7-day Gemini") {
		t.Errorf("formatBurndownSummary = %q, want the quota window named as largest, not a spend row", got)
	}
	if strings.Contains(got, "heimdall") || strings.Contains(got, "minty-fleet") {
		t.Errorf("formatBurndownSummary = %q, want no spend row named as largest", got)
	}
	// All three rows still count toward the tally.
	if !strings.Contains(got, "3 windows") {
		t.Errorf("formatBurndownSummary = %q, want 3 windows in the tally", got)
	}
}

func TestFormatBurndownSummary_AllSpendRows(t *testing.T) {
	groups := []burndownGroup{
		{
			Label: "LiteLLM proxy (minty)",
			Capacities: []burndown.Capacity{
				{Provider: "LiteLLM proxy (minty)", Label: "heimdall", Headroom: 80, IsSpend: true},
			},
		},
	}
	got := formatBurndownSummary(groups)
	if strings.Contains(got, "largest") {
		t.Errorf("formatBurndownSummary = %q, want no misleading \"largest\" winner when every row is a spend row", got)
	}
	if !strings.Contains(got, "1 window") {
		t.Errorf("formatBurndownSummary = %q, want the spend row still counted", got)
	}
}

func TestFormatBurndownSummary_NoneUsable(t *testing.T) {
	groups := []burndownGroup{
		{Capacities: []burndown.Capacity{{Headroom: 0}}},
	}
	got := formatBurndownSummary(groups)
	if !strings.Contains(got, "no windows with usable capacity") {
		t.Errorf("formatBurndownSummary = %q, want the none-usable message", got)
	}
}

func TestBuildBurndownGroups(t *testing.T) {
	providers := []models.Provider{
		{Name: "anthropic", Label: "Anthropic / Claude", Plan: "Claude Max 20x"},
	}
	usages := []models.Usage{
		{
			Bars: []models.Bar{{Label: "5-hour", Percent: 10, Left: 90}},
		},
	}
	groups := buildBurndownGroups(providers, usages, nil, time.Now())
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}
	if groups[0].Label != "Anthropic / Claude" {
		t.Errorf("groups[0].Label = %q", groups[0].Label)
	}
	if groups[0].Plan != "Claude Max 20x" {
		t.Errorf("groups[0].Plan = %q", groups[0].Plan)
	}
	if len(groups[0].Capacities) != 1 {
		t.Fatalf("len(groups[0].Capacities) = %d, want 1", len(groups[0].Capacities))
	}
	if groups[0].Capacities[0].Note != "no floor configured" {
		t.Errorf("with a nil burndown config, Note = %q, want \"no floor configured\"", groups[0].Capacities[0].Note)
	}
}
