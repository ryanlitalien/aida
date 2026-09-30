package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/arbiter"
)

func TestRenderArbiterStatusNoData(t *testing.T) {
	out := renderArbiterStatus(arbiterStatusView{})
	if !strings.Contains(out, "unknown") {
		t.Errorf("expected an unknown-state hint when scheduler.json has never been written, got:\n%s", out)
	}
	if !strings.Contains(out, "lane marks:\n  none") {
		t.Errorf("expected 'none' lane marks, got:\n%s", out)
	}
	if !strings.Contains(out, "last wave: none yet") {
		t.Errorf("expected 'none yet' last wave, got:\n%s", out)
	}
}

func TestRenderArbiterStatusPopulated(t *testing.T) {
	nextWake := time.Date(2026, 9, 25, 23, 0, 0, 0, time.UTC)
	emptyUntil := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	wave := arbiter.WaveSummary{
		Tasks: 2, Passed: 1, Held: 1,
		ByLane: map[string]int{"claude-max": 2},
	}
	view := arbiterStatusView{
		Scheduler:       schedulerState{State: "sleeping", NextWake: nextWake, Reason: "outside the overnight window"},
		ShouldRunNow:    false,
		ShouldRunReason: "outside the overnight window",
		LaneMarks: []laneMarkView{
			{Lane: "claude-max", EmptyUntil: emptyUntil, Source: "preflight", Ambiguous: false},
		},
		LastWave:       &wave,
		LastWaveSpoken: arbiter.SpeakSummary(wave),
	}

	out := renderArbiterStatus(view)
	for _, want := range []string{
		"scheduler: sleeping",
		"next wake: " + nextWake.Format(time.RFC3339),
		"reason: outside the overnight window",
		"should run now: false",
		"claude-max: empty until " + emptyUntil.Format(time.RFC3339),
		"source preflight, ambiguous false",
		"tasks=2 passed=1 held=1 no-lane=0",
		"lane claude-max: 2 attempt(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q, got:\n%s", want, out)
		}
	}
}
