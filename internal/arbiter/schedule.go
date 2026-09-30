package arbiter

import (
	"fmt"
	"sort"
	"time"

	"github.com/ryanlitalien/aida/internal/burndown"
)

// schedule.go is the pure policy half of the overnight scheduler
// (`aida serve --arbiter`, docs/arbiter-plan.md section 4): given the
// live capacity view and the clock, when should a wave run, when should
// the scheduler wake up again, and which candidate tasks actually belong
// in a wave. No IO here -- runArbiterScheduler (internal/cli/serve_arbiter.go)
// is the only caller and owns every side effect (probing capacity,
// reading/writing state files, spawning the loop).

// ScheduleConfig is the overnight scheduler's own policy knobs, layered
// on top of the burn-down floors' OvernightConfig (the same 23:00-8am-ish
// window CapacityFor already uses for drain-to-zero) rather than
// duplicating it -- one clock definition for "overnight," shared by the
// floor rule and the wake rule.
type ScheduleConfig struct {
	// Overnight is reused from burndown.Config.Overnight so "is it
	// nighttime" means the same thing to the scheduler as it already
	// does to CapacityFor's drain-to-zero rule.
	Overnight burndown.OvernightConfig
	// NightlyAt is the "HH:MM" (24h, local to Overnight.Timezone) the
	// scheduler always wakes at, regardless of capacity -- plan section
	// 4's "wakes on a plan (23:00 nightly, ...)".
	NightlyAt string
	// WeeklyDrain is how far ahead of a 7-day window's reset the
	// scheduler starts treating "the last N hours before a weekly
	// reset" as its own wake trigger (plan section 4: "plus the last 24
	// hours before each weekly reset").
	WeeklyDrain time.Duration
}

// defaultNightlyAt and defaultWeeklyDrain are ScheduleConfig's defaults,
// matching docs/arbiter-plan.md section 4 exactly (23:00, 24h).
const (
	defaultNightlyAt   = "23:00"
	defaultWeeklyDrain = 24 * time.Hour
)

// DefaultScheduleConfig returns the canonical scheduler policy: nightly
// wake at 23:00, weekly-drain window of the last 24h before a 7-day
// reset, riding whatever OvernightConfig the burn-down floors already
// define (so a Timezone/Start/End edit in ~/.aida/burndown.yaml moves
// both the floor rule and the wake rule together).
func DefaultScheduleConfig(overnight burndown.OvernightConfig) ScheduleConfig {
	return ScheduleConfig{
		Overnight:   overnight,
		NightlyAt:   defaultNightlyAt,
		WeeklyDrain: defaultWeeklyDrain,
	}
}

// ShouldRun reports whether the scheduler should run a wave right now,
// and why: true inside the overnight window (drain-to-zero territory --
// plan section 4's "midnight to 8am holds about two full windows"), or
// true when any non-spend 7-day window is within WeeklyDrain of its
// reset (the "last 24 hours before each weekly reset" trigger, since
// that window drains to zero right up to reset regardless of time of
// day -- see docs/arbiter-plan.md section 4). Spend rows and non-7d
// windows never trigger the weekly-drain reason: a 5-hour window
// refilling five times a day is not a scarce weekly event worth waking
// the scheduler outside its normal hours for, and a spend row has no
// "drains to zero at reset" behavior at all.
func ShouldRun(cfg ScheduleConfig, caps []burndown.Capacity, now time.Time) (bool, string) {
	if burndown.IsOvernight(cfg.Overnight, now) {
		return true, "overnight window"
	}
	for _, c := range caps {
		if c.IsSpend {
			continue
		}
		if WindowKey(c.Label) != "7d" {
			continue
		}
		if c.ResetsAt.IsZero() {
			continue
		}
		d := c.ResetsAt.Sub(now)
		if d >= 0 && d <= cfg.WeeklyDrain {
			return true, fmt.Sprintf("weekly drain: %s %s resets at %s", c.Provider, c.Label, c.ResetsAt.Format(time.RFC3339))
		}
	}
	return false, fmt.Sprintf("outside the overnight window and no weekly reset within %s", cfg.WeeklyDrain)
}

// wakeCandidate is one instant NextWake considers, paired with the
// reason it would report if that instant wins.
type wakeCandidate struct {
	at     time.Time
	reason string
}

// parseHHMM parses a "HH:MM" 24-hour clock string. A small local twin of
// burndown's own unexported parseHHMM (capacity.go) -- that package
// deliberately doesn't export it, the same situation WindowKey's doc
// comment already explains for windowKeyForLabel.
func parseHHMM(s string) (hour, min int, ok bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

// nextNightly returns the next instant NightlyAt falls on: today at that
// clock time if it's still ahead of now, else tomorrow. false when
// NightlyAt doesn't parse as "HH:MM" (an unconfigured or malformed
// value never blocks NextWake -- it just contributes no candidate).
func nextNightly(cfg ScheduleConfig, now time.Time) (time.Time, bool) {
	h, m, ok := parseHHMM(cfg.NightlyAt)
	if !ok {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(cfg.Overnight.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, loc)
	if today.After(local) {
		return today, true
	}
	return today.AddDate(0, 0, 1), true
}

// NextWake returns the earliest instant the scheduler should wake and
// re-plan, and why: the next NightlyAt, every 7-day window's
// weekly-drain trigger point (ResetsAt minus WeeklyDrain, plan section
// 4), and every window's own ResetsAt (a window refilling is itself a
// re-plan trigger -- new headroom just became available, cheaper than
// whatever was eligible before). Ties break toward whichever candidate
// was considered first (nightly, then drain triggers, then raw resets,
// in caps order) -- not a meaningful distinction in practice since real
// timestamps essentially never collide to the second.
func NextWake(cfg ScheduleConfig, caps []burndown.Capacity, now time.Time) (time.Time, string) {
	var candidates []wakeCandidate
	if t, ok := nextNightly(cfg, now); ok {
		candidates = append(candidates, wakeCandidate{t, fmt.Sprintf("nightly at %s", cfg.NightlyAt)})
	}
	for _, c := range caps {
		if c.ResetsAt.IsZero() {
			continue
		}
		if !c.IsSpend && WindowKey(c.Label) == "7d" {
			drainAt := c.ResetsAt.Add(-cfg.WeeklyDrain)
			if drainAt.After(now) {
				candidates = append(candidates, wakeCandidate{
					drainAt,
					fmt.Sprintf("weekly drain window opens for %s %s (resets %s)", c.Provider, c.Label, c.ResetsAt.Format(time.RFC3339)),
				})
			}
		}
		if c.ResetsAt.After(now) {
			candidates = append(candidates, wakeCandidate{
				c.ResetsAt,
				fmt.Sprintf("%s %s resets at %s", c.Provider, c.Label, c.ResetsAt.Format(time.RFC3339)),
			})
		}
	}

	if len(candidates) == 0 {
		// No parseable NightlyAt and no known reset times at all -- an
		// unconfigured/degenerate state. Fall back to WeeklyDrain out
		// from now rather than never waking again.
		return now.Add(cfg.WeeklyDrain), "no schedule signal available"
	}

	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.at.Before(best.at) {
			best = c
		}
	}
	return best.at, best.reason
}

// WaveTask is one task's shape as the scheduler sees it -- just enough
// to decide whether it belongs in a wave (BuildWave) and to hand off to
// the loop (internal/cli/serve_arbiter.go builds these from
// brain.TaskRecord).
type WaveTask struct {
	Slug   string
	TaskID int
	Title  string
	Tags   []string
	// Verifier is a human-readable description of the machine verifier
	// this task carries ("check: make test", "deliverables: 3"), or
	// empty when it has none. BuildWave's requireVerifier gate reads
	// this, never re-derives it -- the caller already knows whether a
	// --arbiter-check was configured or ## Deliverables were parsed.
	Verifier string
}

// Skip is one task BuildWave left out of the wave, with why.
type Skip struct {
	Slug   string
	Reason string
}

// noVerifierReason is BuildWave's skip reason for a task with no machine
// verifier -- plan section 4: "builds a wave from tasks that carry a
// machine verifier." Without one, a failing attempt has no way to be
// told apart from a passing one, so an unattended run must never pick
// the task up at all.
const noVerifierReason = "no machine verifier (no --arbiter-check and no ## Deliverables); plan section 4 says no verifier means no unattended run"

// BuildWave filters tasks down to the ones an unattended wave may
// actually run. requireVerifier is the scheduler's --arbiter-require-verifier
// flag (on by default); when true, a task with an empty Verifier is
// skipped rather than dispatched. Order is preserved -- callers that
// already sorted tasks by priority get a wave in the same order.
func BuildWave(tasks []WaveTask, requireVerifier bool) (wave []WaveTask, skipped []Skip) {
	for _, t := range tasks {
		if requireVerifier && t.Verifier == "" {
			skipped = append(skipped, Skip{Slug: t.Slug, Reason: noVerifierReason})
			continue
		}
		wave = append(wave, t)
	}
	return wave, skipped
}

// WindowDelta is one provider window's used-percent before and after a
// wave, the number the morning summary actually cares about ("Claude Max
// 5-hour went from 12 to 71 percent used").
type WindowDelta struct {
	Provider   string
	Label      string
	UsedBefore float64
	UsedAfter  float64
}

// WaveSummary is what one overnight wave accomplished, built from the
// ledger entries it wrote (plan section 7's morning summary). Tasks
// counts distinct task slugs seen in the span; Passed/Held/NoLane are
// disjoint buckets classified from each task's LAST ledger entry in the
// span (see SummarizeWave's doc comment for exactly how a verdict string
// maps to a bucket) -- they need not sum to Tasks, since a task whose
// last observed verdict is something else entirely (an in-flight
// "lane-empty" retry that never got a follow-up entry before the span
// ended, for instance) falls into Held by the same "didn't pass, wasn't
// explicitly waiting on a lane" reasoning VerifyDeliverables-style gates
// use elsewhere in this codebase.
type WaveSummary struct {
	Started, Ended time.Time
	Tasks          int
	Passed         int
	Held           int
	NoLane         int
	ByLane         map[string]int
	ByVerdict      map[string]int
	Windows        []WindowDelta
	// Note explains an empty wave (SpeakSummary's "No overnight wave
	// ran: <Note>" case) -- e.g. "no ledger entries in this window".
	Note string
}

// SummarizeWave reduces the run ledger (arbiter.Entry, ledger.go) to a
// WaveSummary for entries whose At falls in [started, ended]. ByLane and
// ByVerdict count every entry in the span (a retried task contributes
// once per attempt); Passed/Held/NoLane count each distinct task once,
// from its last entry in the span:
//
//   - "pass" or "done" -> Passed (the gate passed, or the task was
//     marked done outright)
//   - "no-lane" -> NoLane (every lane lacked headroom for this task this
//     round -- see runArbiterLoop's outcomeNoLane)
//   - anything else ("fail", "hold", "error", "lane-empty",
//     "lane-empty-ambiguous", ...) -> Held, since none of those is a
//     terminal pass and the task still needs attention
//
// Windows aggregates every Before/After WindowSnapshot seen across the
// whole span (not per task): UsedBefore is the first time a
// provider+label pair was observed (in a Before or After list, whichever
// came first chronologically), UsedAfter is the last.
func SummarizeWave(entries []Entry, started, ended time.Time) WaveSummary {
	s := WaveSummary{Started: started, Ended: ended, ByLane: map[string]int{}, ByVerdict: map[string]int{}}

	var inSpan []Entry
	for _, e := range entries {
		if e.At.Before(started) || e.At.After(ended) {
			continue
		}
		inSpan = append(inSpan, e)
	}
	if len(inSpan) == 0 {
		s.Note = "no ledger entries in this window"
		return s
	}
	sort.Slice(inSpan, func(i, j int) bool { return inSpan[i].At.Before(inSpan[j].At) })

	lastBySlug := map[string]Entry{}
	var slugOrder []string
	for _, e := range inSpan {
		if _, ok := lastBySlug[e.TaskSlug]; !ok {
			slugOrder = append(slugOrder, e.TaskSlug)
		}
		lastBySlug[e.TaskSlug] = e
		s.ByLane[e.Lane]++
		s.ByVerdict[e.Verdict]++
	}
	s.Tasks = len(slugOrder)
	for _, slug := range slugOrder {
		switch lastBySlug[slug].Verdict {
		case "pass", "done":
			s.Passed++
		case "no-lane":
			s.NoLane++
		default:
			s.Held++
		}
	}

	type winKey struct{ Provider, Label string }
	before := map[winKey]WindowSnapshot{}
	after := map[winKey]WindowSnapshot{}
	var winOrder []winKey
	seen := func(k winKey) bool {
		_, ok := before[k]
		return ok
	}
	for _, e := range inSpan {
		for _, w := range e.Before {
			k := winKey{w.Provider, w.Label}
			if !seen(k) {
				before[k] = w
				winOrder = append(winOrder, k)
			}
		}
		for _, w := range e.After {
			k := winKey{w.Provider, w.Label}
			if !seen(k) {
				before[k] = w
				winOrder = append(winOrder, k)
			}
			after[k] = w
		}
	}
	for _, k := range winOrder {
		a := after[k]
		if a == (WindowSnapshot{}) {
			a = before[k]
		}
		s.Windows = append(s.Windows, WindowDelta{
			Provider:   k.Provider,
			Label:      k.Label,
			UsedBefore: before[k].UsedPct,
			UsedAfter:  a.UsedPct,
		})
	}
	return s
}

// SpeakSummary renders s as one or two short spoken sentences for the
// morning utterance (plan section 7): task counts and rounded window
// percents only, never raw ledger detail. An empty wave (Tasks == 0)
// speaks s.Note instead of a task breakdown.
func SpeakSummary(s WaveSummary) string {
	if s.Tasks == 0 {
		note := s.Note
		if note == "" {
			note = "no tasks ran"
		}
		return fmt.Sprintf("No overnight wave ran: %s.", note)
	}

	sentence := fmt.Sprintf("Overnight wave done: %d tasks, %d passed", s.Tasks, s.Passed)
	if s.Held > 0 {
		sentence += fmt.Sprintf(", %d on hold", s.Held)
	}
	if s.NoLane > 0 {
		sentence += fmt.Sprintf(", %d waiting on a lane", s.NoLane)
	}
	sentence += "."

	for _, w := range s.Windows {
		sentence += fmt.Sprintf(" %s %s went from %d to %d percent used.",
			w.Provider, w.Label, int(w.UsedBefore+0.5), int(w.UsedAfter+0.5))
	}
	return sentence
}
