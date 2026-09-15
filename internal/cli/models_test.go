package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/models"
)

func TestFormatBarLine_Unpaced(t *testing.T) {
	b := models.Bar{Percent: 62, Left: 38}
	got := formatBarLine(b, time.UTC)
	if !strings.Contains(got, "62% used") || !strings.Contains(got, "38% left") {
		t.Errorf("formatBarLine(unpaced) = %q, want the plain used/left form", got)
	}
	if strings.Contains(got, "HOT") || strings.Contains(got, "IDLE") || strings.Contains(got, "on pace") {
		t.Errorf("formatBarLine(unpaced) = %q, want no pace chip", got)
	}
}

func TestFormatBarLine_Hot(t *testing.T) {
	b := models.Bar{
		Percent:  39,
		ResetsAt: time.Now().Add(145 * time.Hour),
		Pace: &models.Pace{
			Verdict: models.PaceHot,
			Ratio:   2.85,
			Elapsed: 0.137,
			Detail:  "empty in 23h0m, 5d early",
		},
	}
	got := formatBarLine(b, time.UTC)
	if !strings.Contains(got, "39% used") {
		t.Errorf("formatBarLine(hot) = %q, want used%% present", got)
	}
	if !strings.Contains(got, "HOT") {
		t.Errorf("formatBarLine(hot) = %q, want a HOT chip", got)
	}
	if !strings.Contains(got, "empty in 23h0m, 5d early") {
		t.Errorf("formatBarLine(hot) = %q, want the detail string", got)
	}
	if strings.Contains(got, "left") {
		t.Errorf("formatBarLine(hot) = %q, want \"%% left\" dropped for a paced row", got)
	}
}

func TestFormatBarLine_EarlyFallsBackToPlain(t *testing.T) {
	// A bar with a Pace whose verdict is "early" renders exactly like an
	// unpaced one -- no chip, no detail, the original used/left form.
	b := models.Bar{
		Percent: 2, Left: 98,
		Pace: &models.Pace{Verdict: models.PaceEarly, Elapsed: 0.012},
	}
	got := formatBarLine(b, time.UTC)
	if !strings.Contains(got, "2% used") || !strings.Contains(got, "98% left") {
		t.Errorf("formatBarLine(early) = %q, want the plain used/left form", got)
	}
}

func TestFormatSpendLine_Idle(t *testing.T) {
	s := models.Spend{
		Spend: 9, Budget: 100,
		ResetsAt: time.Now().Add(68 * time.Hour),
		Pace: &models.Pace{
			Verdict: models.PaceIdle,
			Ratio:   0.15,
			Elapsed: 0.595,
			Detail:  "ends ~85% unused",
		},
	}
	got := formatSpendLine(s, time.UTC)
	if !strings.Contains(got, "IDLE") {
		t.Errorf("formatSpendLine(idle) = %q, want an IDLE chip", got)
	}
	if !strings.Contains(got, "ends ~85% unused") {
		t.Errorf("formatSpendLine(idle) = %q, want the detail string", got)
	}
}

func TestFormatSpendLine_Unpaced(t *testing.T) {
	s := models.Spend{Spend: 32.81, Budget: 75}
	got := formatSpendLine(s, time.UTC)
	if !strings.Contains(got, "$32.81") || !strings.Contains(got, "$75") {
		t.Errorf("formatSpendLine(unpaced) = %q, want the plain amounts form", got)
	}
}

func TestCollectPaceExceptions(t *testing.T) {
	providers := []modelsProviderOut{
		{
			Provider: models.Provider{Label: "Codex"},
			Usage: models.Usage{Bars: []models.Bar{
				{Label: "7-day", Pace: &models.Pace{Verdict: models.PaceHot}},
				{Label: "5-hour", Pace: nil},
			}},
		},
		{
			Provider: models.Provider{Label: "Gemini"},
			Usage: models.Usage{Bars: []models.Bar{
				{Label: "7-day", Pace: &models.Pace{Verdict: models.PaceIdle}},
			}},
		},
		{
			Provider: models.Provider{Label: "Bedrock BS"},
			Usage: models.Usage{Spend: []models.Spend{
				{Key: "minty-fleet", Pace: &models.Pace{Verdict: models.PaceIdle}},
			}},
		},
		{
			Provider: models.Provider{Label: "OnPaceProvider"},
			Usage: models.Usage{Bars: []models.Bar{
				{Label: "7-day", Pace: &models.Pace{Verdict: models.PaceOnPace}},
			}},
		},
	}

	hot, idle := collectPaceExceptions(providers)
	if len(hot) != 1 || hot[0] != "Codex 7-day" {
		t.Errorf("hot = %v, want [\"Codex 7-day\"]", hot)
	}
	if len(idle) != 2 {
		t.Fatalf("idle = %v, want 2 entries", idle)
	}
	if idle[0] != "Gemini 7-day" || idle[1] != "Bedrock BS minty-fleet" {
		t.Errorf("idle = %v, want [\"Gemini 7-day\", \"Bedrock BS minty-fleet\"]", idle)
	}

	strip := paceExceptionStrip(providers)
	if !strings.Contains(strip, "HOT") || !strings.Contains(strip, "Codex 7-day") {
		t.Errorf("paceExceptionStrip = %q, want a HOT section with Codex 7-day", strip)
	}
	if !strings.Contains(strip, "IDLE") || !strings.Contains(strip, "Gemini 7-day") || !strings.Contains(strip, "Bedrock BS minty-fleet") {
		t.Errorf("paceExceptionStrip = %q, want an IDLE section with both entries", strip)
	}
}

func TestPaceExceptionStrip_AllOnPace(t *testing.T) {
	providers := []modelsProviderOut{
		{
			Provider: models.Provider{Label: "Codex"},
			Usage: models.Usage{Bars: []models.Bar{
				{Label: "7-day", Pace: &models.Pace{Verdict: models.PaceOnPace}},
			}},
		},
	}
	if got := paceExceptionStrip(providers); got != "" {
		t.Errorf("paceExceptionStrip(all on-pace) = %q, want \"\"", got)
	}
}

func TestPaceExceptionStrip_Empty(t *testing.T) {
	if got := paceExceptionStrip(nil); got != "" {
		t.Errorf("paceExceptionStrip(nil) = %q, want \"\"", got)
	}
}

func TestStyledPaceChip_TextSurvives(t *testing.T) {
	// Whatever lipgloss styling is applied (or not, outside a tty), the
	// literal chip text must still be a substring of the styled result.
	for _, verdict := range []string{models.PaceHot, models.PaceIdle, models.PaceOnPace, "unknown"} {
		got := styledPaceChip(verdict, "2.9x HOT")
		if !strings.Contains(got, "2.9x HOT") {
			t.Errorf("styledPaceChip(%q, ...) = %q, want it to contain the chip text", verdict, got)
		}
	}
}
