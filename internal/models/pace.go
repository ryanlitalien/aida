package models

import (
	"fmt"
	"math"
	"time"
)

// Pace verdict constants -- see Pace.Verdict.
const (
	PaceEarly  = "early"
	PaceHot    = "hot"
	PaceIdle   = "idle"
	PaceOnPace = "on-pace"
)

// paceEarlyThreshold is the minimum fraction of a window that must have
// elapsed before a pace verdict means anything: below it, Ratio (used% /
// elapsed%) explodes toward infinity for even a modest used%, so PaceFor
// reports PaceEarly instead of a noisy hot/idle call.
const paceEarlyThreshold = 0.10

// paceHotThreshold / paceIdleThreshold bound the "on-pace" band: a Ratio
// at or above paceHotThreshold is burning faster than the clock (hot), at
// or below paceIdleThreshold slower (idle), and anything in between is
// on-pace.
const (
	paceHotThreshold  = 1.25
	paceIdleThreshold = 0.75
)

// Pace is a bar's (or budget row's) burn-rate verdict relative to the
// clock: whether it's on track to land near 100% used right at reset, or
// running hot (will exhaust before reset) or idle (will land with
// headroom to spare). See PaceFor.
type Pace struct {
	Ratio     float64 `json:"ratio"`     // used% / elapsed% -- 1.0 means exactly on pace
	Elapsed   float64 `json:"elapsed"`   // 0..1 fraction of the window gone
	Projected float64 `json:"projected"` // percent of the window this rate would consume by reset
	Verdict   string  `json:"verdict"`
	Detail    string  `json:"detail,omitempty"` // "empty in 23h0m, 5d early" / "ends ~85% unused"
}

// PaceFor returns b's burn-rate pace at time now, or nil when the window
// is unknown (WindowMins <= 0), shorter than a day (pace is noise on a
// bursty 5-hour window -- it resets too often for a burn rate to mean
// anything), or the reset time is missing.
func PaceFor(b Bar, now time.Time) *Pace {
	return paceFor(b.Percent, b.WindowMins, b.ResetsAt, now)
}

// paceForSpend mirrors PaceFor for a LiteLLM budget row: percent used is
// spend/budget rather than a Bar's own Percent, and the window is
// whatever mapLiteLLMSpend derived from the key's own budget_duration
// (litellmBudgetDurationMins) -- a budget period genuinely can be "30d",
// "1mo", or something this package doesn't recognize, and Spend.WindowMins
// is left 0 rather than guessed when it can't be parsed, same as PaceFor
// above.
func paceForSpend(s Spend, now time.Time) *Pace {
	if s.Budget <= 0 {
		return nil
	}
	percent := (s.Spend / s.Budget) * 100
	return paceFor(percent, s.WindowMins, s.ResetsAt, now)
}

// paceFor is the shared core both PaceFor and paceForSpend call: percent
// is the used percentage of a window windowMins long, ending at resetsAt.
func paceFor(percent, windowMins float64, resetsAt, now time.Time) *Pace {
	if windowMins < 24*60 || resetsAt.IsZero() {
		return nil
	}

	minutesUntilReset := resetsAt.Sub(now).Minutes()
	elapsed := (windowMins - minutesUntilReset) / windowMins
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > 1 {
		elapsed = 1
	}

	if elapsed < paceEarlyThreshold {
		// Too little signal for a verdict -- deliberately leave
		// Ratio/Projected at their zero value rather than reporting a
		// number that "explodes toward infinity" this early in a window.
		return &Pace{Elapsed: elapsed, Verdict: PaceEarly}
	}

	ratio := percent / (elapsed * 100)
	projected := percent / elapsed

	p := &Pace{Ratio: ratio, Elapsed: elapsed, Projected: projected}
	switch {
	case ratio >= paceHotThreshold:
		p.Verdict = PaceHot
		p.Detail = hotDetail(percent, elapsed, windowMins, minutesUntilReset)
	case ratio <= paceIdleThreshold:
		p.Verdict = PaceIdle
		p.Detail = idleDetail(projected)
	default:
		p.Verdict = PaceOnPace
	}
	return p
}

// hotDetail renders how soon a hot bar will hit 100% used at its current
// burn rate ("empty in <duration>") and, when that lands before the
// window's own reset, how many whole days early ("empty in 23h0m, 5d
// early"). Reuses FormatDurationHuman for the primary duration rather
// than reinventing a formatter; the "Nd early" suffix is a coarser,
// deliberately day-only rounding since precision there is noise.
func hotDetail(percent, elapsed, windowMins, minutesUntilReset float64) string {
	hoursElapsed := elapsed * windowMins / 60
	if percent <= 0 || hoursElapsed <= 0 {
		return ""
	}
	ratePerHour := percent / hoursElapsed
	hoursUntilEmpty := (100 - percent) / ratePerHour
	if hoursUntilEmpty < 0 {
		hoursUntilEmpty = 0
	}

	detail := fmt.Sprintf("empty in %s", FormatDurationHuman(time.Duration(hoursUntilEmpty*float64(time.Hour))))

	hoursUntilReset := minutesUntilReset / 60
	if hoursUntilEmpty < hoursUntilReset {
		earlyDays := int(math.Round((hoursUntilReset - hoursUntilEmpty) / 24))
		if earlyDays >= 1 {
			detail += fmt.Sprintf(", %dd early", earlyDays)
		}
	}
	return detail
}

// idleDetail renders how much of the window an idle bar is projected to
// leave unused at reset, floored at 0 so rounding right at the
// on-pace/idle boundary never reads as negative headroom.
func idleDetail(projected float64) string {
	unused := 100 - projected
	if unused < 0 {
		unused = 0
	}
	return fmt.Sprintf("ends ~%.0f%% unused", unused)
}

// fillPace computes Pace for every Bar and Spend row in u, using
// u.ProbedAt as "now" -- the same instant every other field in u already
// reflects. Called once, generically, right after fillBarLeft inside
// ProbeWithOptions, the same pattern Bar.Left follows: individual
// probeXxx functions never set Pace themselves.
func fillPace(u Usage) Usage {
	now := u.ProbedAt
	for i := range u.Bars {
		u.Bars[i].Pace = PaceFor(u.Bars[i], now)
	}
	for i := range u.Spend {
		u.Spend[i].Pace = paceForSpend(u.Spend[i], now)
	}
	return u
}
