package models

import (
	"context"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"
)

// Severity values for a Bar.
const (
	SeverityNormal   = "normal"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// probeTimeout bounds a single provider's probe: local file reads,
// keychain access, an ssh fallback, and an outbound HTTP call all share
// this one ceiling, applied per-provider in Probe below. A wedged ssh or
// a slow proxy can therefore never make /dashboard (or `aida models`)
// wait past it, and never blocks any other provider's own probe.
const probeTimeout = 8 * time.Second

// Usage is one provider's live-usage snapshot: rate-limit bars (Claude,
// Codex), budget spend rows (LiteLLM), or neither (a provider with no
// live probe wired up). Err carries a short, specific reason when the
// probe didn't produce data -- see the three-way "no key" / "no data" /
// real-error convention documented on each probe function.
//
// Stale/StaleSince/Warn are set only by the last-good fallback in
// probe_cache.go: when a fresh probe hits a transient failure (a 429, a
// network blip) but a prior successful reading for this provider is still
// in memory, that reading is returned instead of the error -- Err is
// cleared, Stale is true, StaleSince is the prior reading's ProbedAt, and
// Warn explains why (e.g. "showing usage from 3m ago (usage endpoint
// rate-limited)"). A provider that has never failed, or has no
// last-good memory to fall back to, always has Stale false and Warn "".
type Usage struct {
	Provider   string            `json:"provider"`
	Plan       string            `json:"plan,omitempty"`
	Account    string            `json:"account,omitempty"`
	Bars       []Bar             `json:"bars,omitempty"`
	Spend      []Spend           `json:"spend,omitempty"`
	Detail     map[string]string `json:"detail,omitempty"`
	Err        string            `json:"err,omitempty"`
	Warn       string            `json:"warn,omitempty"`
	Stale      bool              `json:"stale,omitempty"`
	StaleSince time.Time         `json:"stale_since,omitempty"`
	ProbedAt   time.Time         `json:"probed_at"`
}

// Bar is one rate-limit window (a 5-hour session cap, a 7-day cap, a
// model-scoped cap, ...).
//
// Invariant: Percent is ALWAYS the used percentage of this window, never
// remaining -- every prober in this package (Claude's limits[].percent,
// Codex's used_percent, agy's 100-minus-remaining conversion) normalizes
// to that convention before building a Bar, so a caller never has to
// guess which way a given provider's number points. Left (100 - Percent)
// and Pace (internal/models/pace.go's PaceFor) are filled in once,
// generically, by ProbeWithOptions below -- individual probeXxx functions
// never set either themselves.
type Bar struct {
	Label    string    `json:"label"`
	Percent  float64   `json:"percent"`
	Left     float64   `json:"left"`
	ResetsAt time.Time `json:"resets_at,omitempty"`
	Severity string    `json:"severity"`
	Active   bool      `json:"active,omitempty"`
	// WindowMins is this bar's rate-limit window length in minutes (300
	// for a 5-hour window, 10080 for a 7-day window, ...), when the
	// prober that built this Bar knows it. Zero means "unknown" -- a
	// caller must never guess a window length that isn't here.
	WindowMins float64 `json:"window_mins,omitempty"`
	// Pace is this bar's burn-rate verdict relative to the clock, or nil
	// when PaceFor doesn't have enough to go on (unknown/short window, no
	// reset time) -- see PaceFor's doc comment.
	Pace *Pace `json:"pace,omitempty"`
}

// Spend is one budget row -- a LiteLLM virtual key's spend against its
// hard cap.
type Spend struct {
	Key      string    `json:"key"`
	Spend    float64   `json:"spend"`
	Budget   float64   `json:"budget"`
	ResetsAt time.Time `json:"resets_at,omitempty"`
	Models   []string  `json:"models,omitempty"`
	// WindowMins is this budget's period length in minutes, parsed from
	// LiteLLM's own budget_duration string on the key (see
	// litellmBudgetDurationMins in probe_litellm.go) -- never guessed as a
	// fixed "one month" here. Zero means "unparsed/unknown", the same
	// convention as Bar.WindowMins.
	WindowMins float64 `json:"window_mins,omitempty"`
	// Pace mirrors Bar.Pace, computed from Spend/Budget as the "percent
	// used" (see paceForSpend in pace.go). Nil under the same conditions
	// PaceFor documents.
	Pace *Pace `json:"pace,omitempty"`
}

// ProbeOptions controls one ProbeWithOptions run.
type ProbeOptions struct {
	// Force skips both the per-provider TTL cache and re-probes every
	// provider unconditionally, regardless of how recently it last
	// succeeded. Set by `?refresh=1`, `POST /api/models/refresh`, and
	// `aida models --fresh`.
	Force bool
}

// Probe runs every provider's probe in parallel (one goroutine each,
// individually bounded to probeTimeout regardless of ctx's own deadline)
// and returns one Usage per provider, in Roster.Providers order.
// Equivalent to ProbeWithOptions(ctx, r, ProbeOptions{}) -- i.e. the
// per-provider TTL cache (probeTTLFor) applies and nothing is forced.
//
// It never fails as a whole. Every probeXxx function below reports its
// own failure into the returned Usage.Err rather than returning a Go
// error to the errgroup, so a single wedged or misconfigured provider
// (an expired keychain entry, an unreachable proxy) can never blank out
// the other providers' results -- callers always get len(r.Providers)
// entries back.
func Probe(ctx context.Context, r *Roster) []Usage {
	return ProbeWithOptions(ctx, r, ProbeOptions{})
}

// ProbeWithOptions is Probe with control over per-provider caching (see
// ProbeOptions). Three things happen around each provider's own probeXxx
// call, none of which probeXxx itself knows about:
//
//  1. TTL skip: unless opts.Force, a provider whose last-good reading
//     (probe_cache.go) is younger than probeTTLFor(p.Probe) is returned
//     from memory without probing at all -- not marked Stale, since
//     it's simply still fresh, not a fallback from a failure.
//  2. Claude stagger: when two or more claude-oauth providers are
//     probed in this round (skipping only those that hit the TTL cache
//     above), their outbound HTTP calls are spaced by
//     claudeStaggerDelay so two accounts never hit
//     api.anthropic.com/api/oauth/usage in the same instant -- see
//     claudeOAuthStagger.
//  3. Last-good fallback: a probe that comes back with a transient
//     error (isTransientErr) falls back to the last remembered good
//     reading for that provider, if any, with Stale/StaleSince/Warn set
//     instead of Err (see withLastGoodFallback). A successful probe
//     updates that memory for next time.
func ProbeWithOptions(ctx context.Context, r *Roster, opts ProbeOptions) []Usage {
	if r == nil {
		return nil
	}
	out := make([]Usage, len(r.Providers))
	var g errgroup.Group
	stagger := &claudeOAuthStagger{}
	for i, p := range r.Providers {
		g.Go(func() error {
			if !opts.Force {
				if ttl := probeTTLFor(p.Probe); ttl > 0 {
					if good, ok := lastGoodFor(p.Name); ok && time.Since(good.ProbedAt) < ttl {
						out[i] = good
						return nil
					}
				}
			}
			if p.Probe == "claude-oauth" {
				stagger.wait(ctx)
			}
			pctx, cancel := context.WithTimeout(ctx, probeTimeoutFor(p.Probe))
			defer cancel()
			u := fillPace(fillBarLeft(probeOne(pctx, p)))
			out[i] = withLastGoodFallback(p.Name, u)
			return nil
		})
	}
	_ = g.Wait() // every probeXxx always returns a Usage, never a group error
	return out
}

// probeOne dispatches to the prober named by p.Probe. An empty, unknown,
// or "none" value degrades to a no-op Usage (no bars, no error) rather
// than failing -- that's the documented state for a provider with no
// live-usage probe wired up yet (see the roster's own `probe: none`
// comments).
func probeOne(ctx context.Context, p Provider) Usage {
	switch p.Probe {
	case "claude-oauth":
		return probeClaudeOAuth(ctx, p)
	case "codex-sessions":
		return probeCodexSessions(ctx, p)
	case "litellm":
		return probeLiteLLM(ctx, p)
	case "gemini-local":
		return probeGeminiLocal(ctx, p)
	case "agy-quota":
		return probeAgyQuota(ctx, p)
	default:
		return Usage{Provider: p.Name, Plan: p.Plan, Account: p.Account, ProbedAt: time.Now()}
	}
}

// fillBarLeft sets Left = 100 - Percent on every Bar in u, in the one
// place every probe's output passes through (Probe, right after
// probeOne returns), rather than trusting each probeXxx function to
// compute it itself -- see the invariant documented on Bar. Pure and
// unit tested directly.
func fillBarLeft(u Usage) Usage {
	for i := range u.Bars {
		u.Bars[i].Left = 100 - u.Bars[i].Percent
	}
	return u
}

// probeTimeoutFor returns the per-provider ceiling probeKind's own probe
// gets inside Probe. Every kind shares probeTimeout (8s) except
// "agy-quota": `agy --print /quota` alone is bounded to agyQuotaTimeout
// (60s -- it's genuinely slow, ~10s observed even on a fast run) and the
// unlisted-model diff's `agy models` call runs after it, so this probe
// kind needs a longer individual ceiling instead of racing everything
// else's 8s.
func probeTimeoutFor(probeKind string) time.Duration {
	if probeKind == "agy-quota" {
		return agyProbeTimeout
	}
	return probeTimeout
}

// severityForPercent derives a Bar's Severity from its Percent when the
// upstream response doesn't already carry one (or carries one this
// package doesn't recognize): >=90 critical, >=70 warning, else normal.
func severityForPercent(pct float64) string {
	switch {
	case pct >= 90:
		return SeverityCritical
	case pct >= 70:
		return SeverityWarning
	default:
		return SeverityNormal
	}
}

// parseTimeLoose parses a resets-at value that may arrive as RFC3339 or
// as a bare unix-seconds string (both show up across these APIs). A
// value that matches neither -- including "" -- returns the zero Time,
// which every caller (the CLI, the dashboard) already renders as "resets
// ?" rather than a bogus date.
func parseTimeLoose(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0)
	}
	return time.Time{}
}
