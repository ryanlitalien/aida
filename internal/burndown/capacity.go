package burndown

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/models"
)

// FloorSource values -- see Capacity.FloorSource.
const (
	// FloorSourcePace means the effective floor came from a bar's live
	// burn pace (PaceFor), not the static config.
	FloorSourcePace = "pace"
	// FloorSourceStatic means no pace signal was available, so the
	// configured daytime/overnight/last-24h floor was used as-is.
	FloorSourceStatic = "static"
	// FloorSourceHard means the window's hard_floor was the binding
	// constraint -- it raised a pace- or drain-to-zero-derived floor
	// back up.
	FloorSourceHard = "hard"
	// FloorSourceDrainToZero means the floor is 0 because this window
	// resets before Ryan wakes (or, for a weekly window, within the
	// last 24h before reset with a configured last_24h floor of 0), and
	// no hard_floor overrode it.
	FloorSourceDrainToZero = "drain-to-zero"
)

// noFloorNote is Capacity.Note's value when no floor entry matches a
// bar's provider/window -- see CapacityFor's doc comment.
const noFloorNote = "no floor configured"

// Capacity is one rate-limit window's burn-down headroom: how much of it
// the unattended loop may consume right now without eating into the
// reserve Ryan needs for his own interactive work.
type Capacity struct {
	Provider    string  `json:"provider"`
	Label       string  `json:"label"` // the bar label (or spend key)
	UsedPct     float64 `json:"used_pct"`
	LeftPct     float64 `json:"left_pct"`
	StaticFloor float64 `json:"static_floor"` // the configured floor for this time of day, before pace/hard-floor/drain adjustments
	Floor       float64 `json:"floor"`        // the EFFECTIVE floor after pace adjustment
	// FloorSource is one of FloorSourcePace, FloorSourceStatic,
	// FloorSourceHard, FloorSourceDrainToZero, or "" when no floor is
	// configured at all (see Note).
	FloorSource string       `json:"floor_source,omitempty"`
	Headroom    float64      `json:"headroom"` // max(0, LeftPct - Floor); what the loop may take
	DrainToZero bool         `json:"drain_to_zero,omitempty"`
	Pace        *models.Pace `json:"pace,omitempty"` // nil when unknown
	ResetsAt    time.Time    `json:"resets_at,omitempty"`
	Note        string       `json:"note,omitempty"` // e.g. "no floor configured", a window's own config note, or a spend row's dollar breakdown
	// IsSpend is true for a dollar-denominated budget row (built by
	// CapacityForSpend), false for a rate-limit window bar (built by
	// CapacityFor). A spend row's percent is percent-of-budget, not
	// percent-of-quota, so it isn't comparable to a Bar row's percent --
	// callers ranking "biggest headroom" across rows must exclude spend
	// rows from that ranking (see the CLI's formatBurndownSummary).
	IsSpend bool `json:"is_spend,omitempty"`
}

// windowKeyForLabel maps a Bar or Spend row's Label to the window key
// used in the floors config's `windows:` map ("5h", "7d", "7d-fable",
// "image"). Defensive: an unrecognized label returns "", which
// CapacityFor/CapacityForSpend both treat as "no floor configured"
// rather than a crash -- a bar this package has never seen must never
// panic or silently vanish.
func windowKeyForLabel(label string) string {
	lower := strings.ToLower(label)
	switch {
	case strings.Contains(lower, "fable"):
		return "7d-fable"
	case strings.HasPrefix(lower, "5-hour"):
		return "5h"
	case strings.HasPrefix(lower, "7-day"):
		return "7d"
	case strings.Contains(lower, "image"):
		return "image"
	default:
		return ""
	}
}

// parseHHMM parses a "HH:MM" 24-hour clock string.
func parseHHMM(s string) (hour, min int, ok bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

// overnightWindow reports whether now falls inside cfg's overnight
// window (Start..End, local to Timezone, wrapping past midnight when End
// <= Start) and, when it does, the instant that window ends -- the
// "Ryan wakes" boundary CapacityFor compares a bar's ResetsAt against for
// drain-to-zero. ok is false when Start/End can't be parsed as "HH:MM",
// or now simply isn't inside an overnight window right now; callers must
// not use end when ok is false.
func overnightWindow(cfg OvernightConfig, now time.Time) (end time.Time, ok bool) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	sh, sm, ok1 := parseHHMM(cfg.Start)
	eh, em, ok2 := parseHHMM(cfg.End)
	if !ok1 || !ok2 {
		return time.Time{}, false
	}
	local := now.In(loc)
	crossesMidnight := eh < sh || (eh == sh && em < sm)

	window := func(dayOffset int) (start, end time.Time) {
		start = time.Date(local.Year(), local.Month(), local.Day()+dayOffset, sh, sm, 0, 0, loc)
		end = time.Date(local.Year(), local.Month(), local.Day()+dayOffset, eh, em, 0, 0, loc)
		if crossesMidnight {
			end = end.AddDate(0, 0, 1)
		}
		return start, end
	}

	// Try the window that starts "today" (covers the evening case, e.g.
	// now=23:30) and the one that started "yesterday" (covers the
	// early-morning case, e.g. now=02:00, which belongs to the window
	// that opened the previous evening).
	for _, offset := range []int{0, -1} {
		start, end := window(offset)
		if !local.Before(start) && local.Before(end) {
			return end, true
		}
	}
	return time.Time{}, false
}

// IsOvernight reports whether now falls within cfg's overnight window --
// exported so a caller (the CLI's header line) can label output
// "daytime floors" vs "overnight floors" without duplicating the window
// math CapacityFor already does.
func IsOvernight(cfg OvernightConfig, now time.Time) bool {
	_, ok := overnightWindow(cfg, now)
	return ok
}

// withinNext24h reports whether resetsAt falls at or after now and at
// most 24 hours later. A zero resetsAt (unknown) is never "within 24h".
func withinNext24h(resetsAt, now time.Time) bool {
	if resetsAt.IsZero() {
		return false
	}
	d := resetsAt.Sub(now)
	return d >= 0 && d <= 24*time.Hour
}

// CapacityFor computes the burn-down headroom for one bar at time now.
//
// The floor rule:
//
//  1. Pick the static floor for the moment: the configured `overnight`
//     value when now is inside cfg.Overnight's window, else the
//     configured `last_24h_before_reset` value when the bar resets
//     within 24h, else `daytime`.
//  2. Drain-to-zero: DrainToZero is true (and the floor becomes 0,
//     before hard_floor) when the bar's reset lands before the end of
//     the current overnight window (it resets before Ryan wakes, so
//     anything left in it expires unused), or when last_24h_before_reset
//     is configured as 0 and the reset is within 24h.
//  3. Pace adjustment: once the bar carries a real pace verdict (Pace
//     != nil, Verdict != PaceEarly), the floor is instead
//     max(hardFloor, min(projectedAdditional * safety_multiplier,
//     LeftPct)), where projectedAdditional = max(0, Pace.Projected -
//     Percent) -- this REPLACES the static/drain-to-zero floor, since
//     it's a better estimate of the same quantity. FloorSource is
//     "hard" when hard_floor is what's binding, "pace" otherwise (even
//     when the LeftPct clamp is what binds).
//  4. No pace signal (a 5h bar, PaceEarly, an unknown window, no reset
//     time): floor = max(staticFloor-or-0-if-draining, hardFloor).
//     FloorSource is "hard" when hard_floor raised it, "drain-to-zero"
//     when the drain rule is what's zeroing it, else "static".
//  5. hard_floor always wins: it can raise a pace or drain-to-zero floor
//     back up (Fable never drops below 50%, even overnight, even when
//     its window resets before Ryan wakes).
//  6. Headroom = max(0, LeftPct - Floor).
//
// A bar whose provider/window isn't named anywhere in cfg is not an
// error: Floor comes back 0, Headroom equals LeftPct, FloorSource is
// "", and Note explains why ("no floor configured"). CapacityFor never
// panics on a nil cfg, an empty Floors map, or a Label
// windowKeyForLabel doesn't recognize.
func CapacityFor(providerLabel string, b models.Bar, cfg *Config, now time.Time) Capacity {
	out := Capacity{
		Provider: providerLabel,
		Label:    b.Label,
		UsedPct:  b.Percent,
		LeftPct:  b.Left,
		Pace:     b.Pace,
		ResetsAt: b.ResetsAt,
	}

	if cfg == nil {
		out.Note = noFloorNote
		out.Headroom = out.LeftPct
		return out
	}

	entry, found := cfg.findFloorEntry(providerLabel)
	key := windowKeyForLabel(b.Label)
	var wf WindowFloor
	var foundWindow bool
	if found && key != "" {
		wf, foundWindow = entry.Windows[key]
	}
	if !found || key == "" || !foundWindow {
		out.Note = noFloorNote
		out.Headroom = out.LeftPct
		return out
	}
	if wf.Note != "" {
		out.Note = wf.Note
	}

	hardFloor := 0.0
	if wf.HardFloor != nil {
		hardFloor = *wf.HardFloor
	}

	overnightEnd, overnightNow := overnightWindow(cfg.Overnight, now)
	within24h := withinNext24h(b.ResetsAt, now)

	staticFloor := wf.Daytime
	switch {
	case overnightNow && wf.Overnight != nil:
		staticFloor = *wf.Overnight
	case within24h && wf.Last24hBeforeReset != nil:
		staticFloor = *wf.Last24hBeforeReset
	}
	out.StaticFloor = staticFloor

	drainToZero := overnightNow && !b.ResetsAt.IsZero() && b.ResetsAt.Before(overnightEnd)
	if within24h && wf.Last24hBeforeReset != nil && *wf.Last24hBeforeReset == 0 {
		drainToZero = true
	}
	out.DrainToZero = drainToZero

	if b.Pace != nil && b.Pace.Verdict != models.PaceEarly {
		additional := math.Max(0, b.Pace.Projected-b.Percent)
		paceFloor := additional * cfg.Pace.SafetyMultiplier
		raised := math.Max(paceFloor, hardFloor)
		out.Floor = math.Min(raised, b.Left)
		if hardFloor > paceFloor {
			out.FloorSource = FloorSourceHard
		} else {
			out.FloorSource = FloorSourcePace
		}
	} else {
		base := staticFloor
		if drainToZero {
			base = 0
		}
		out.Floor = math.Max(base, hardFloor)
		switch {
		case hardFloor > base:
			out.FloorSource = FloorSourceHard
		case drainToZero:
			out.FloorSource = FloorSourceDrainToZero
		default:
			out.FloorSource = FloorSourceStatic
		}
	}

	out.Headroom = math.Max(0, out.LeftPct-out.Floor)
	return out
}

// CapacityForSpend mirrors CapacityFor for a dollar-denominated budget
// row (a LiteLLM key, a monthly company cap): UsedPct/LeftPct/Floor/
// Headroom are all expressed as a percent of the budget (so they compose
// the same way a Bar's do), and Note additionally carries the dollar
// breakdown, since "how much of the LiteLLM key is left" is more useful
// to Ryan in dollars than in percent.
//
// There's no overnight/last-24h/drain-to-zero distinction for a monthly
// budget (it isn't a daily rate-limit window), so the floor rule
// collapses to two branches, but floor_usd is a HARD floor exactly like a
// Bar's hard_floor: pace may raise the effective floor above floor_usd's
// percent-of-budget equivalent, but must never push it below. The
// LiteLLM reserve floor_usd protects exists for Jarvis turns and browser
// jobs, which are bursty and unscheduled, so they never register in a
// pace reading -- a 0.0x pace on one of these keys means "nothing ran in
// the last window," not "this money is free." See CapacityFor step 3/5
// for the Bar-path twin of this clamp. The dollar breakdown in Note is
// always derived from the same effective Floor reported above, so the
// two can never disagree the way they used to when pace alone decided
// the percent floor.
func CapacityForSpend(providerLabel string, s models.Spend, cfg *Config, now time.Time) Capacity {
	usedPct := 0.0
	if s.Budget > 0 {
		usedPct = (s.Spend / s.Budget) * 100
	}
	leftPct := 100 - usedPct

	out := Capacity{
		Provider: providerLabel,
		Label:    s.Key,
		UsedPct:  usedPct,
		LeftPct:  leftPct,
		Pace:     s.Pace,
		ResetsAt: s.ResetsAt,
		IsSpend:  true,
	}

	if cfg == nil {
		out.Note = noFloorNote
		out.Headroom = out.LeftPct
		return out
	}

	entry, found := cfg.findFloorEntry(providerLabel)
	if !found || entry.Monthly == nil {
		out.Note = noFloorNote
		out.Headroom = out.LeftPct
		return out
	}

	floorUSD := entry.Monthly.FloorUSD
	floorPct := 0.0
	if s.Budget > 0 {
		floorPct = (floorUSD / s.Budget) * 100
	}
	out.StaticFloor = floorPct

	// floorPct IS this row's hard floor -- see the doc comment above.
	hardFloor := floorPct

	if s.Pace != nil && s.Pace.Verdict != models.PaceEarly {
		additional := math.Max(0, s.Pace.Projected-usedPct)
		paceFloor := additional * cfg.Pace.SafetyMultiplier
		raised := math.Max(paceFloor, hardFloor)
		out.Floor = math.Min(raised, leftPct)
		if hardFloor > paceFloor {
			out.FloorSource = FloorSourceHard
		} else {
			out.FloorSource = FloorSourcePace
		}
	} else {
		out.Floor = hardFloor
		out.FloorSource = FloorSourceStatic
	}
	out.Headroom = math.Max(0, out.LeftPct-out.Floor)

	// Keep the dollar breakdown in Note derived from the same effective
	// Floor computed above, not from the raw floor_usd config value --
	// that's exactly what exposed the original bug (Floor said 0% while
	// Note's dollar breakdown still said $2.00).
	leftUSD := s.Budget - s.Spend
	effectiveFloorUSD := floorUSD
	if s.Budget > 0 {
		effectiveFloorUSD = out.Floor / 100 * s.Budget
	}
	headroomUSD := math.Max(0, leftUSD-effectiveFloorUSD)
	note := formatSpendNote(leftUSD, s.Budget, effectiveFloorUSD, headroomUSD)
	if entry.Monthly.Note != "" {
		note += " (" + entry.Monthly.Note + ")"
	}
	out.Note = note
	return out
}

// formatSpendNote renders CapacityForSpend's dollar breakdown.
func formatSpendNote(leftUSD, budgetUSD, floorUSD, headroomUSD float64) string {
	return fmt.Sprintf("$%.2f left of $%.2f budget · floor $%.2f · headroom $%.2f",
		leftUSD, budgetUSD, floorUSD, headroomUSD)
}

// isAntigravityClaudeGPTBar reports whether label is one of Antigravity's
// (the `agy` CLI's) "5-hour Claude/GPT" / "7-day Claude/GPT" bars --
// those route Claude and GPT models through the Google account rather
// than measuring Gemini's own quota, and the burn-down never routes work
// through them anyway (Claude work goes through the claude CLI on an
// Anthropic plan, not through Google's quota), so Report skips them
// instead of letting them pollute the Gemini provider's rows.
func isAntigravityClaudeGPTBar(label string) bool {
	return strings.Contains(label, "Claude/GPT")
}

// Report computes every probed provider's window-by-window capacity.
// providers and usages are index-aligned, the same contract
// models.ProbeWithOptions documents and internal/cli/models.go's
// buildModelsResponse already relies on -- Report takes both (not
// models.Usage alone) because CapacityFor matches a floors entry by the
// roster's own Provider.Label, which only lives on models.Provider, not
// on the Usage a probe returns.
func Report(providers []models.Provider, usages []models.Usage, cfg *Config, now time.Time) []Capacity {
	var out []Capacity
	for i, p := range providers {
		if i >= len(usages) {
			break
		}
		u := usages[i]
		for _, b := range u.Bars {
			if isAntigravityClaudeGPTBar(b.Label) {
				continue
			}
			out = append(out, CapacityFor(p.Label, b, cfg, now))
		}
		for _, s := range u.Spend {
			out = append(out, CapacityForSpend(p.Label, s, cfg, now))
		}
	}
	return out
}
