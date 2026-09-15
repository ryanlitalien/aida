package models

import (
	"math"
	"strings"
	"testing"
	"time"
)

// paceTestNow is the fixed reference instant every table case below
// measures "hours until reset" from -- arbitrary, but fixed so the tests
// are deterministic.
var paceTestNow = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

func paceTestBar(percent, windowMins, hoursUntilReset float64) Bar {
	return Bar{
		Percent:    percent,
		WindowMins: windowMins,
		ResetsAt:   paceTestNow.Add(time.Duration(hoursUntilReset * float64(time.Hour))),
	}
}

func approxEqual(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}

// TestPaceFor_RealWorldCases covers the exact bars a 2026-09-14 probe of
// a real 7-day window produced, asserting the verdict and approximate
// ratio for each.
func TestPaceFor_RealWorldCases(t *testing.T) {
	const sevenDayMins = 10080

	tests := []struct {
		name            string
		percent         float64
		hoursUntilReset float64
		wantVerdict     string
		wantRatio       float64
		ratioTol        float64
	}{
		{"81% used, 31h until reset", 81, 31, PaceOnPace, 0.99, 0.01},
		{"39% used, 89h until reset (3d17h)", 39, 89, PaceOnPace, 0.83, 0.01},
		{"39% used, 145h until reset (6d1h)", 39, 145, PaceHot, 2.85, 0.01},
		{"9% used, 68h until reset (2d20h)", 9, 68, PaceIdle, 0.15, 0.01},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := paceTestBar(tt.percent, sevenDayMins, tt.hoursUntilReset)
			p := PaceFor(b, paceTestNow)
			if p == nil {
				t.Fatal("PaceFor returned nil, want a Pace")
			}
			if p.Verdict != tt.wantVerdict {
				t.Errorf("Verdict = %q, want %q", p.Verdict, tt.wantVerdict)
			}
			if !approxEqual(p.Ratio, tt.wantRatio, tt.ratioTol) {
				t.Errorf("Ratio = %v, want ~%v (+/- %v)", p.Ratio, tt.wantRatio, tt.ratioTol)
			}
		})
	}
}

func TestPaceFor_Early(t *testing.T) {
	// 2% used, 166h until reset on a 7-day window -> elapsed ~1.2%,
	// too little signal for a verdict.
	b := paceTestBar(2, 10080, 166)
	p := PaceFor(b, paceTestNow)
	if p == nil {
		t.Fatal("PaceFor returned nil, want a Pace with verdict early")
	}
	if p.Verdict != PaceEarly {
		t.Errorf("Verdict = %q, want %q", p.Verdict, PaceEarly)
	}
	if !approxEqual(p.Elapsed, 0.012, 0.005) {
		t.Errorf("Elapsed = %v, want ~0.012", p.Elapsed)
	}
	if p.Detail != "" {
		t.Errorf("Detail = %q, want empty for an early verdict", p.Detail)
	}
}

func TestPaceFor_ShortWindowReturnsNil(t *testing.T) {
	// A 5-hour window is bursty and resets constantly -- pace there is
	// noise, so PaceFor never returns a verdict for it regardless of
	// percent/elapsed.
	b := paceTestBar(90, 300, 1)
	if p := PaceFor(b, paceTestNow); p != nil {
		t.Errorf("PaceFor(5-hour window) = %+v, want nil", p)
	}
}

func TestPaceFor_UnknownWindowReturnsNil(t *testing.T) {
	b := paceTestBar(50, 0, 84)
	if p := PaceFor(b, paceTestNow); p != nil {
		t.Errorf("PaceFor(WindowMins=0) = %+v, want nil", p)
	}
}

func TestPaceFor_ZeroResetsAtReturnsNil(t *testing.T) {
	b := Bar{Percent: 50, WindowMins: 10080}
	if p := PaceFor(b, paceTestNow); p != nil {
		t.Errorf("PaceFor(zero ResetsAt) = %+v, want nil", p)
	}
}

func TestPaceFor_IdleZeroPercent(t *testing.T) {
	// 0% used, halfway through a 7-day window -> idle, and the detail
	// string must be exact: "ends ~100% unused".
	b := paceTestBar(0, 10080, 84)
	p := PaceFor(b, paceTestNow)
	if p == nil {
		t.Fatal("PaceFor returned nil, want a Pace")
	}
	if p.Verdict != PaceIdle {
		t.Errorf("Verdict = %q, want %q", p.Verdict, PaceIdle)
	}
	if p.Detail != "ends ~100% unused" {
		t.Errorf("Detail = %q, want %q", p.Detail, "ends ~100% unused")
	}
}

func TestPaceFor_HotDetailMentionsEarly(t *testing.T) {
	// A bar burning hot enough to empty well before its own reset gets
	// the ", Nd early" suffix.
	b := paceTestBar(39, 10080, 145)
	p := PaceFor(b, paceTestNow)
	if p == nil || p.Verdict != PaceHot {
		t.Fatalf("PaceFor = %+v, want a hot Pace", p)
	}
	if p.Detail == "" {
		t.Fatal("Detail is empty, want an \"empty in ...\" string")
	}
	if !strings.Contains(p.Detail, "empty in") || !strings.Contains(p.Detail, "early") {
		t.Errorf("Detail = %q, want both \"empty in\" and \"early\"", p.Detail)
	}
}

func TestPaceFor_OnPaceHasNoDetail(t *testing.T) {
	b := paceTestBar(81, 10080, 31)
	p := PaceFor(b, paceTestNow)
	if p == nil || p.Verdict != PaceOnPace {
		t.Fatalf("PaceFor = %+v, want an on-pace Pace", p)
	}
	if p.Detail != "" {
		t.Errorf("Detail = %q, want empty for on-pace", p.Detail)
	}
}

// ---- paceForSpend ----

func TestPaceForSpend(t *testing.T) {
	s := Spend{
		Spend:      39,
		Budget:     100,
		WindowMins: 10080,
		ResetsAt:   paceTestNow.Add(89 * time.Hour),
	}
	p := paceForSpend(s, paceTestNow)
	if p == nil {
		t.Fatal("paceForSpend returned nil, want a Pace")
	}
	if p.Verdict != PaceOnPace {
		t.Errorf("Verdict = %q, want %q", p.Verdict, PaceOnPace)
	}
	if !approxEqual(p.Ratio, 0.83, 0.01) {
		t.Errorf("Ratio = %v, want ~0.83", p.Ratio)
	}
}

func TestPaceForSpend_ZeroBudgetReturnsNil(t *testing.T) {
	s := Spend{Spend: 10, Budget: 0, WindowMins: 10080, ResetsAt: paceTestNow.Add(89 * time.Hour)}
	if p := paceForSpend(s, paceTestNow); p != nil {
		t.Errorf("paceForSpend(zero budget) = %+v, want nil", p)
	}
}

func TestPaceForSpend_UnknownWindowReturnsNil(t *testing.T) {
	// A budget row whose budget_duration didn't parse (WindowMins left
	// at 0) must never get a guessed verdict.
	s := Spend{Spend: 10, Budget: 100, WindowMins: 0, ResetsAt: paceTestNow.Add(89 * time.Hour)}
	if p := paceForSpend(s, paceTestNow); p != nil {
		t.Errorf("paceForSpend(WindowMins=0) = %+v, want nil", p)
	}
}

// ---- fillPace wiring ----

func TestFillPace(t *testing.T) {
	u := Usage{
		ProbedAt: paceTestNow,
		Bars: []Bar{
			paceTestBar(81, 10080, 31), // on-pace
			paceTestBar(90, 300, 1),    // 5-hour, no pace
		},
		Spend: []Spend{
			{Spend: 39, Budget: 100, WindowMins: 10080, ResetsAt: paceTestNow.Add(89 * time.Hour)},
			{Spend: 10, Budget: 100, WindowMins: 0, ResetsAt: paceTestNow.Add(89 * time.Hour)},
		},
	}
	got := fillPace(u)
	if got.Bars[0].Pace == nil || got.Bars[0].Pace.Verdict != PaceOnPace {
		t.Errorf("Bars[0].Pace = %+v, want on-pace", got.Bars[0].Pace)
	}
	if got.Bars[1].Pace != nil {
		t.Errorf("Bars[1].Pace = %+v, want nil (5-hour window)", got.Bars[1].Pace)
	}
	if got.Spend[0].Pace == nil || got.Spend[0].Pace.Verdict != PaceOnPace {
		t.Errorf("Spend[0].Pace = %+v, want on-pace", got.Spend[0].Pace)
	}
	if got.Spend[1].Pace != nil {
		t.Errorf("Spend[1].Pace = %+v, want nil (unknown window)", got.Spend[1].Pace)
	}
}

func TestFillPace_NoBarsOrSpend(t *testing.T) {
	u := Usage{ProbedAt: paceTestNow, Err: "no key"}
	got := fillPace(u)
	if len(got.Bars) != 0 || len(got.Spend) != 0 {
		t.Errorf("fillPace(no bars/spend) = %+v, want unchanged", got)
	}
}
