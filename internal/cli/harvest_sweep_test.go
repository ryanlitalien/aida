package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
)

func TestDefaultHarvestSweepOptsIsFifteenMinutes(t *testing.T) {
	o := defaultHarvestSweepOpts()
	if o.interval != 15*time.Minute {
		t.Fatalf("defaultHarvestSweepOpts().interval = %v, want 15m", o.interval)
	}
	if !o.enabled() {
		t.Fatal("default sweep opts should be enabled")
	}
}

func TestHarvestSweepOptsEnabled(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     bool
	}{
		{"positive interval enables", 15 * time.Minute, true},
		{"zero disables", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := harvestSweepOpts{interval: tc.interval}
			if got := o.enabled(); got != tc.want {
				t.Errorf("harvestSweepOpts{interval: %v}.enabled() = %v, want %v", tc.interval, got, tc.want)
			}
		})
	}
}

// TestHarvestSweepOptsValidate mirrors loopOpts.validate's fail-fast
// contract: a negative --harvest-sweep duration must be rejected up front
// (from newServeCmd's RunE) rather than left to misbehave inside the
// goroutine. Zero (disables) and any positive duration are both valid.
func TestHarvestSweepOptsValidate(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		wantErr  bool
	}{
		{"default 15m is valid", 15 * time.Minute, false},
		{"zero (disabled) is valid", 0, false},
		{"negative is invalid", -1 * time.Minute, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := harvestSweepOpts{interval: tc.interval}.validate()
			if tc.wantErr && err == nil {
				t.Fatalf("validate() with interval %v: expected error, got nil", tc.interval)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validate() with interval %v: unexpected error: %v", tc.interval, err)
			}
		})
	}
}

// TestHarvestSweepSummary exercises the quiet-vs-noteworthy decision that
// keeps a no-op sweep silent: nothing written and nothing dropped must
// report ok=false so runHarvestSweepOnce logs nothing, while a written
// record (either LLM-distilled or direct-mirrored) or a dropped one must
// report ok=true with a summary line.
func TestHarvestSweepSummary(t *testing.T) {
	if _, ok := harvestSweepSummary(nil); ok {
		t.Error("nil result should not be noteworthy")
	}

	quiet := &brain.HarvestResult{CandidatesTotal: 3, Selected: 0, SkippedQuiet: 3}
	if _, ok := harvestSweepSummary(quiet); ok {
		t.Error("a pass with nothing selected/written/dropped should not be noteworthy")
	}

	selectedButEmpty := &brain.HarvestResult{
		CandidatesTotal: 2,
		Selected:        2,
		Sessions: []brain.HarvestSessionOutcome{
			{SessionID: "a"}, // zero memories written -- the expected common case
		},
	}
	if _, ok := harvestSweepSummary(selectedButEmpty); ok {
		t.Error("sessions processed with zero memories written should not be noteworthy")
	}

	wroteFromSession := &brain.HarvestResult{
		CandidatesTotal: 1,
		Selected:        1,
		Sessions: []brain.HarvestSessionOutcome{
			{SessionID: "a", Written: []brain.MemoryRecord{{Key: "codex:a:foo"}}},
		},
	}
	summary, ok := harvestSweepSummary(wroteFromSession)
	if !ok {
		t.Fatal("a written session memory should be noteworthy")
	}
	if !strings.Contains(summary, "1 written") {
		t.Errorf("summary %q should mention 1 written", summary)
	}

	wroteDirect := &brain.HarvestResult{
		CandidatesTotal: 0,
		DirectMirrored:  []brain.MemoryRecord{{Key: "gemini-md"}},
	}
	if _, ok := harvestSweepSummary(wroteDirect); !ok {
		t.Error("a direct-mirrored memory should be noteworthy")
	}

	dropped := &brain.HarvestResult{CandidatesTotal: 1, Selected: 1, Dropped: 1}
	if _, ok := harvestSweepSummary(dropped); !ok {
		t.Error("a dropped memory should be noteworthy even with nothing written")
	}
}

// TestRunHarvestSweepOnceCallsBothToolsInOrder proves the fixed
// codex-then-gemini-then-meetily sequencing and that HarvestOptions passed
// through are the zero value (default MaxSessions, no --since) --
// matching `aida brain harvest` with no flags. Uses a fake harvestFunc; no
// brain.Open, no LLM/Voyage call, no ~/.codex, ~/.gemini, or life-log/calls
// access.
func TestRunHarvestSweepOnceCallsBothToolsInOrder(t *testing.T) {
	var mu sync.Mutex
	var calledTools []string
	var calledOpts []brain.HarvestOptions

	fake := func(ctx context.Context, tool string, opts brain.HarvestOptions) (*brain.HarvestResult, error) {
		mu.Lock()
		defer mu.Unlock()
		calledTools = append(calledTools, tool)
		calledOpts = append(calledOpts, opts)
		return &brain.HarvestResult{Tool: tool}, nil
	}

	runHarvestSweepOnce(context.Background(), fake)

	if want := []string{"codex", "gemini", "meetily"}; !equalStrings(calledTools, want) {
		t.Fatalf("called tools = %v, want %v", calledTools, want)
	}
	for i, opts := range calledOpts {
		if opts != (brain.HarvestOptions{}) {
			t.Errorf("call %d: opts = %+v, want zero value (default MaxSessions, no --since)", i, opts)
		}
	}
}

// TestRunHarvestSweepOnceContinuesAfterToolError proves one tool's error
// doesn't stop the other tools from running -- a Codex-side failure (e.g. a
// transient config load error) must not silently swallow the Gemini or
// Meetily passes.
func TestRunHarvestSweepOnceContinuesAfterToolError(t *testing.T) {
	var calledTools []string
	fake := func(ctx context.Context, tool string, opts brain.HarvestOptions) (*brain.HarvestResult, error) {
		calledTools = append(calledTools, tool)
		if tool == "codex" {
			return nil, errors.New("boom")
		}
		return &brain.HarvestResult{Tool: tool}, nil
	}

	runHarvestSweepOnce(context.Background(), fake)

	if want := []string{"codex", "gemini", "meetily"}; !equalStrings(calledTools, want) {
		t.Fatalf("called tools = %v, want %v (gemini and meetily must still run after codex errors)", calledTools, want)
	}
}

// TestRunHarvestSweepOnceStopsOnCancelledContext proves a context
// cancelled between the two tool calls (e.g. daemon shutdown mid-sweep)
// short-circuits the remaining tools rather than plowing through them.
func TestRunHarvestSweepOnceStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calledTools []string
	fake := func(ctx context.Context, tool string, opts brain.HarvestOptions) (*brain.HarvestResult, error) {
		calledTools = append(calledTools, tool)
		cancel() // cancel after the first call so the second is skipped
		return &brain.HarvestResult{Tool: tool}, nil
	}

	runHarvestSweepOnce(ctx, fake)

	if len(calledTools) != 1 {
		t.Fatalf("called tools = %v, want exactly 1 (stop after ctx cancelled)", calledTools)
	}
}

// TestRunHarvestSweepLoopDisabledNeverTicks proves the goroutine wiring
// contract from runHTTPDaemon: when the sweep is disabled (interval 0),
// runHTTPDaemon never starts runHarvestSweepLoop at all -- this test
// exercises the loop directly with a real but absurdly long interval to
// prove it does not fire before its first tick, which is the same
// "quiet until the timer says so" behavior --harvest-sweep 0 relies on
// callers not even starting.
func TestRunHarvestSweepLoopWaitsForFirstTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	calls := 0
	fake := func(ctx context.Context, tool string, opts brain.HarvestOptions) (*brain.HarvestResult, error) {
		calls++
		return &brain.HarvestResult{Tool: tool}, nil
	}

	// Interval far longer than the context's lifetime: the loop must
	// return (via ctx.Done) having made zero calls, proving it doesn't
	// fire eagerly at start.
	runHarvestSweepLoop(ctx, 10*time.Second, fake)

	if calls != 0 {
		t.Fatalf("runHarvestSweepLoop with interval > ctx lifetime made %d calls, want 0", calls)
	}
}

// TestRunHarvestSweepLoopTicksAndStopsOnCancel proves the loop fires a
// sweep pass on each tick and stops promptly once ctx is cancelled --
// the same shutdown contract the loop dispatcher's runLoopCtx honors.
func TestRunHarvestSweepLoopTicksAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	passes := 0
	fake := func(ctx context.Context, tool string, opts brain.HarvestOptions) (*brain.HarvestResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if tool == "codex" {
			passes++
		}
		return &brain.HarvestResult{Tool: tool}, nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runHarvestSweepLoop(ctx, 20*time.Millisecond, fake)
	}()

	// Let at least one tick land, then cancel and confirm the loop stops
	// promptly (bounded, no hang) -- mirrors the daemon's 15s shutdown
	// bound but with a much smaller margin since this is a unit test.
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runHarvestSweepLoop did not stop within 2s of ctx cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if passes == 0 {
		t.Error("expected at least one sweep pass to have run before cancellation")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
