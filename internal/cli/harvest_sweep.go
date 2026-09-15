package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
)

// Periodic harvest sweep -- `aida serve --harvest-sweep <duration>` hosts a
// background goroutine that periodically runs the harvest core (see
// brain_harvest.go's runHarvestCore) for Codex, Gemini, and Meetily,
// independent of the hook-triggered harvest wired up by `aida setup`
// (Codex/Gemini only -- Meetily has no hook mechanism to wire into, so the
// sweep is its only automatic trigger besides a manual `aida brain harvest
// --tool meetily`).
//
// The gap this closes: harvest only otherwise runs when a Codex Stop or
// Gemini AfterAgent hook fires. If a session's LAST hook fires while the
// session is still inside the harvest quiet window (updated <10 minutes
// ago -- see harvestQuietWindow in internal/brain/harvest.go), that
// hook-triggered run skips it as "still running", and if the user then
// goes idle on that tool nothing ever fires again to pick it up -- it sits
// unharvested until their next session with that tool. A periodic sweep
// re-checks after the quiet window has passed regardless of hooks.
//
// The built-in watermark + quiet window already make a sweep pass cheap
// and idempotent: with nothing new to harvest it makes zero LLM calls and
// near-zero I/O, so running it every few minutes is safe.

// harvestSweepTools is the fixed, sequential tool order each sweep pass
// covers -- codex, then gemini, then meetily, matching `aida brain harvest
// --tool`.
var harvestSweepTools = []string{"codex", "gemini", "meetily"}

// harvestSweepOpts configures the periodic harvest sweep. Zero value is
// invalid for `enabled`'s sake (interval 0 reads as disabled, which is
// also a legitimate user choice via --harvest-sweep 0) -- always construct
// via defaultHarvestSweepOpts.
type harvestSweepOpts struct {
	interval time.Duration // 0 disables the sweep
}

// defaultHarvestSweepOpts returns the canonical sweep default: every 15
// minutes. newServeCmd seeds its --harvest-sweep flag from this.
func defaultHarvestSweepOpts() harvestSweepOpts {
	return harvestSweepOpts{interval: 15 * time.Minute}
}

// validate rejects a negative --harvest-sweep duration up front, mirroring
// loopOpts.validate's fail-fast contract: a bad flag value fails `aida
// serve` outright instead of behaving oddly inside the goroutine.
func (o harvestSweepOpts) validate() error {
	if o.interval < 0 {
		return fmt.Errorf("--harvest-sweep must be >= 0 (got %s); 0 disables the sweep", o.interval)
	}
	return nil
}

func (o harvestSweepOpts) enabled() bool {
	return o.interval > 0
}

// runHarvestSweepLoop ticks every interval and runs one sweep pass (both
// tools, sequentially) until ctx is done. Split out from the ticker wiring
// in runHTTPDaemon so the scheduling/disable logic is unit-testable with
// an injected harvestOnce and a short interval -- no real LLM/Voyage
// calls, no touching ~/.codex or ~/.gemini.
func runHarvestSweepLoop(ctx context.Context, interval time.Duration, harvestOnce harvestFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runHarvestSweepOnce(ctx, harvestOnce)
	}
}

// runHarvestSweepOnce runs the harvest core for each tool in
// harvestSweepTools, sequentially, with default HarvestOptions (no
// --since, default MaxSessions -- the same defaults `aida brain harvest`
// uses with no flags). It logs only when a pass actually did something --
// wrote a memory record, or the LLM proposed something malformed that got
// dropped -- so the common quiet no-op sweep produces zero log lines. A
// per-tool error is logged and does not stop the other tool from running.
// aida task #298 tracks making the harvest command's own error reporting
// louder in general; this just surfaces whatever runHarvestCore already
// returns/reports, it doesn't change what counts as an error.
func runHarvestSweepOnce(ctx context.Context, harvestOnce harvestFunc) {
	for _, tool := range harvestSweepTools {
		if ctx.Err() != nil {
			return
		}
		result, err := harvestOnce(ctx, tool, brain.HarvestOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "harvest sweep (%s): %v\n", tool, err)
			continue
		}
		if summary, ok := harvestSweepSummary(result); ok {
			fmt.Fprintf(os.Stderr, "🌾 harvest sweep (%s): %s\n", tool, summary)
		}
	}
}

// harvestSweepSummary reports whether a harvest pass did anything worth
// logging (wrote at least one memory record, direct-mirrored or
// LLM-distilled, or dropped a malformed one) and, if so, a one-line
// summary. A nil result, or one where nothing was written or dropped,
// reports ok=false so the caller stays quiet.
func harvestSweepSummary(r *brain.HarvestResult) (summary string, ok bool) {
	if r == nil {
		return "", false
	}
	written := len(r.DirectMirrored)
	for _, s := range r.Sessions {
		written += len(s.Written)
	}
	if written == 0 && r.Dropped == 0 {
		return "", false
	}
	return fmt.Sprintf("%d candidate(s), %d selected, %d written, %d dropped",
		r.CandidatesTotal, r.Selected, written, r.Dropped), true
}
