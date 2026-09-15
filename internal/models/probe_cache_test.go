package models

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- probeTTLFor ----

func TestProbeTTLFor(t *testing.T) {
	tests := []struct {
		kind string
		want time.Duration
	}{
		{"claude-oauth", 5 * time.Minute},
		{"codex-sessions", 60 * time.Second},
		{"agy-quota", 5 * time.Minute},
		{"litellm", 60 * time.Second},
		{"gemini-local", 0},
		{"none", 0},
		{"", 0},
		{"some-future-probe-kind", 0},
	}
	for _, tt := range tests {
		if got := probeTTLFor(tt.kind); got != tt.want {
			t.Errorf("probeTTLFor(%q) = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

// ---- last-good memory + isTransientErr ----

func TestIsTransientErr(t *testing.T) {
	tests := []struct {
		err  string
		want bool
	}{
		{"error: usage endpoint rate-limited this token (429); retry in a minute", true},
		{"error: parsing usage response: unexpected EOF", true},
		{"no key (Claude Code not logged in on this machine)", false},
		{"no data", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isTransientErr(tt.err); got != tt.want {
			t.Errorf("isTransientErr(%q) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestRememberIfGood_SkipsErrorsAndEmptySuccess(t *testing.T) {
	name := t.Name()

	rememberIfGood(name, Usage{Provider: name, Err: "error: boom"})
	if _, ok := lastGoodFor(name); ok {
		t.Fatalf("an errored Usage was remembered, want it skipped")
	}

	rememberIfGood(name, Usage{Provider: name, ProbedAt: time.Now()}) // no bars, no spend, no err
	if _, ok := lastGoodFor(name); ok {
		t.Fatalf("a data-free success was remembered, want it skipped (nothing useful to fall back to)")
	}

	rememberIfGood(name, Usage{Provider: name, Bars: []Bar{{Label: "5-hour", Percent: 10}}, ProbedAt: time.Now()})
	if _, ok := lastGoodFor(name); !ok {
		t.Fatalf("a real success (with bars) was not remembered")
	}
}

// TestWithLastGoodFallback_TransientErrorFallsBackToLastGood is the core
// "last-good memory" scenario from a fake prober's 429: a prior success is
// in memory, the next probe comes back with the exact rate-limit message
// fetchClaudeUsage (probe_claude.go) returns, and the fallback must swap
// in the old bars with Stale/StaleSince/Warn set and Err cleared.
func TestWithLastGoodFallback_TransientErrorFallsBackToLastGood(t *testing.T) {
	name := t.Name()
	probedAt := time.Now().Add(-3 * time.Minute)
	good := Usage{
		Provider: name,
		Plan:     "Claude Max 20x",
		Bars:     []Bar{{Label: "5-hour", Percent: 42, Severity: SeverityWarning}},
		ProbedAt: probedAt,
	}

	// A clean success passes through unchanged and seeds the memory.
	seeded := withLastGoodFallback(name, good)
	if seeded.Err != "" || len(seeded.Bars) != 1 {
		t.Fatalf("seeding call returned %+v, want the clean success passed through untouched", seeded)
	}

	// A fake prober's 429, in the same shape probeClaudeOAuth produces.
	failed := Usage{
		Provider: name,
		ProbedAt: time.Now(),
		Err:      "error: usage endpoint rate-limited this token (429); retry in a minute",
	}
	fallback := withLastGoodFallback(name, failed)

	if fallback.Err != "" {
		t.Errorf("Err = %q, want empty (fallback must clear it)", fallback.Err)
	}
	if !fallback.Stale {
		t.Errorf("Stale = false, want true")
	}
	if !fallback.StaleSince.Equal(probedAt) {
		t.Errorf("StaleSince = %v, want the last-good reading's ProbedAt %v", fallback.StaleSince, probedAt)
	}
	if fallback.Warn == "" || !strings.Contains(fallback.Warn, "rate-limited") {
		t.Errorf("Warn = %q, want a rate-limited explanation", fallback.Warn)
	}
	if len(fallback.Bars) != 1 || fallback.Bars[0].Label != "5-hour" {
		t.Errorf("Bars = %v, want the last-good bars carried through so the UI keeps them visible", fallback.Bars)
	}
	if fallback.Plan != "Claude Max 20x" {
		t.Errorf("Plan = %q, want the last-good reading's plan carried through", fallback.Plan)
	}
}

func TestWithLastGoodFallback_NoMemoryPassesErrorThrough(t *testing.T) {
	name := t.Name()
	failed := Usage{Provider: name, Err: "error: network unreachable"}
	got := withLastGoodFallback(name, failed)
	if got.Err != failed.Err {
		t.Errorf("Err = %q, want unchanged %q (nothing to fall back to)", got.Err, failed.Err)
	}
	if got.Stale || got.Warn != "" {
		t.Errorf("got %+v, want no staleness with no last-good memory", got)
	}
}

func TestWithLastGoodFallback_DurableErrorNeverFallsBack(t *testing.T) {
	name := t.Name()
	withLastGoodFallback(name, Usage{ // seed memory with a real success
		Provider: name,
		Bars:     []Bar{{Label: "5-hour", Percent: 10}},
		ProbedAt: time.Now(),
	})

	failed := Usage{Provider: name, Err: "no key (Claude Code not logged in on this machine)"}
	got := withLastGoodFallback(name, failed)
	if got.Err != failed.Err || got.Stale || len(got.Bars) != 0 {
		t.Errorf("got %+v, want the durable \"no key\" error passed through unchanged -- stale bars would be misleading for a logged-out account", got)
	}
}

// ---- ProbeWithOptions: TTL skip vs forced refresh ----

// TestProbeWithOptions_TTLSkipVsForce drives the actual public entry point
// (not just probeTTLFor/withLastGoodFallback in isolation). It uses a
// "litellm" provider with a nil LiteLLMConfig so a real (forced) probe is
// deterministic and does zero I/O -- probeLiteLLM returns "no key (litellm
// block missing from roster)" immediately, never touching the network --
// which doubles as this test's fake prober.
func TestProbeWithOptions_TTLSkipVsForce(t *testing.T) {
	name := t.Name()
	r := &Roster{Providers: []Provider{
		{Name: name, Plan: "test plan", Probe: "litellm"},
	}}

	seeded := Usage{
		Provider: name,
		Plan:     "test plan",
		Spend:    []Spend{{Key: "test-key", Spend: 1, Budget: 10}},
		ProbedAt: time.Now(),
	}
	rememberIfGood(name, seeded)

	// Within TTL (litellmTTL = 60s), force=false: the seeded reading comes
	// back untouched -- the real litellm prober, which would report "no
	// key", is never even called.
	got := ProbeWithOptions(context.Background(), r, ProbeOptions{})
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Err != "" {
		t.Errorf("Err = %q, want empty (a TTL cache hit must skip the real probe entirely)", got[0].Err)
	}
	if len(got[0].Spend) != 1 || got[0].Spend[0].Key != "test-key" {
		t.Errorf("Spend = %v, want the seeded last-good reading", got[0].Spend)
	}
	if got[0].Stale {
		t.Errorf("Stale = true, want false (a TTL cache hit is simply still fresh, not a fallback from failure)")
	}

	// Force=true bypasses the TTL cache and actually re-probes; with
	// LiteLLM nil that's the deterministic, network-free "no key" error.
	forced := ProbeWithOptions(context.Background(), r, ProbeOptions{Force: true})
	if forced[0].Err == "" || !strings.Contains(forced[0].Err, "no key") {
		t.Errorf("forced Err = %q, want the real litellm prober's \"no key\" error (Force must bypass the TTL cache)", forced[0].Err)
	}
}

// ---- Usage JSON: Warn/Stale/StaleSince serialize, and omit when unset ----

func TestUsageJSON_WarnStaleSerialize(t *testing.T) {
	since := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	u := Usage{
		Provider:   "anthropic",
		Stale:      true,
		StaleSince: since,
		Warn:       "showing usage from 3m ago (usage endpoint rate-limited)",
	}
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["stale"] != true {
		t.Errorf("stale = %v, want true", got["stale"])
	}
	if got["warn"] != u.Warn {
		t.Errorf("warn = %v, want %q", got["warn"], u.Warn)
	}
	ss, _ := got["stale_since"].(string)
	parsed, err := time.Parse(time.RFC3339, ss)
	if err != nil || !parsed.Equal(since) {
		t.Errorf("stale_since = %q, want an RFC3339 encoding of %v", ss, since)
	}
	if _, ok := got["err"]; ok {
		t.Errorf("err key present in JSON, want omitted (a Warn/Stale reading never also carries Err)")
	}

	// A plain, never-stale Usage omits stale and warn (omitempty works for
	// bool/string). StaleSince stays a plain time.Time like Bar.ResetsAt
	// elsewhere in this package -- encoding/json's omitempty never omits a
	// zero-value struct, so it round-trips as the zero time rather than
	// disappearing; callers already handle that via IsZero() the same way
	// they do for ResetsAt.
	plain := Usage{Provider: "anthropic", ProbedAt: since}
	data2, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got2 map[string]any
	if err := json.Unmarshal(data2, &got2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"stale", "warn"} {
		if _, ok := got2[k]; ok {
			t.Errorf("key %q present for a non-stale Usage, want omitted", k)
		}
	}
	if ss, ok := got2["stale_since"].(string); !ok || !mustParseRFC3339(t, ss).IsZero() {
		t.Errorf("stale_since = %v, want the zero time", got2["stale_since"])
	}
}

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return tm
}

// ---- Claude OAuth stagger ----

func TestClaudeOAuthStagger_SpacesConcurrentCalls(t *testing.T) {
	s := &claudeOAuthStagger{}
	var starts [2]time.Time
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range starts {
		go func(i int) {
			defer wg.Done()
			s.wait(context.Background())
			starts[i] = time.Now()
		}(i)
	}
	wg.Wait()

	gap := starts[1].Sub(starts[0])
	if gap < 0 {
		gap = -gap
	}
	const tolerance = 100 * time.Millisecond
	if gap < claudeStaggerDelay-tolerance {
		t.Errorf("gap between two staggered claude-oauth calls = %v, want at least ~%v", gap, claudeStaggerDelay)
	}
}

func TestClaudeOAuthStagger_ContextCancelDoesNotBlockForever(t *testing.T) {
	s := &claudeOAuthStagger{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	// Reserve the first slot so the second wait() has something to wait on.
	s.wait(context.Background())

	done := make(chan struct{})
	go func() {
		s.wait(ctx) // would otherwise sleep ~1.5s
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("wait() did not return promptly when ctx was canceled")
	}
}
