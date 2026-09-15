package models

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ---- per-provider TTL (item 2: skip a re-probe within TTL) ----

// Per-probe-kind TTLs consumed by ProbeWithOptions. claude-oauth and
// agy-quota get the longer 5-minute TTL: claude-oauth because
// api.anthropic.com/api/oauth/usage rate-limits a token after a few calls
// a minute (the whole reason this file exists), agy-quota because
// `agy --print /quota` is itself slow (~10s) and its numbers don't move
// fast enough to justify re-running it every probe round. codex-sessions
// and litellm are cheap local/proxy reads and keep the original
// effectively-always-fresh cadence, bounded only by the outer 60s
// response cache in internal/cli/models_web.go.
const (
	claudeOAuthTTL   = 5 * time.Minute
	codexSessionsTTL = 60 * time.Second
	agyQuotaTTL      = 5 * time.Minute
	litellmTTL       = 60 * time.Second
)

// probeTTLFor returns how long a provider's last-good reading stays fresh
// enough that ProbeWithOptions skips re-probing it outright. A kind with
// no entry here (gemini-local, none, anything unrecognized) returns 0,
// meaning "always probe" -- those are cheap local-only reads with no
// rate-limit concern, so caching them buys nothing and would only make
// their output feel more stale than it needs to.
func probeTTLFor(probeKind string) time.Duration {
	switch probeKind {
	case "claude-oauth":
		return claudeOAuthTTL
	case "codex-sessions":
		return codexSessionsTTL
	case "agy-quota":
		return agyQuotaTTL
	case "litellm":
		return litellmTTL
	default:
		return 0
	}
}

// ---- last-good memory (item 1: fall back to the last successful reading) ----

// lastGood is the in-process (never persisted, never shared across
// daemons) memory of the most recent successful Usage per provider name.
// Keyed by Provider.Name, not Provider.Probe -- two providers can share a
// probe kind (both Claude accounts probe "claude-oauth") but never a
// name, so this map can't conflate the personal and ButterStack accounts.
var lastGood = struct {
	mu sync.Mutex
	m  map[string]Usage
}{m: make(map[string]Usage)}

// rememberIfGood stores u as name's last-good reading when it actually
// carries data. A Usage with an Err, or a data-free no-op success (a
// `probe: none` provider, or one that ran clean but found nothing to
// show), is never worth remembering -- only a reading with real bars or
// spend rows is useful as a fallback later.
func rememberIfGood(name string, u Usage) {
	if u.Err != "" || (len(u.Bars) == 0 && len(u.Spend) == 0) {
		return
	}
	clone := u
	lastGood.mu.Lock()
	lastGood.m[name] = clone
	lastGood.mu.Unlock()
}

// lastGoodFor returns name's remembered last-good reading, if any.
func lastGoodFor(name string) (Usage, bool) {
	lastGood.mu.Lock()
	defer lastGood.mu.Unlock()
	u, ok := lastGood.m[name]
	return u, ok
}

// isTransientErr reports whether err represents a transient probe failure
// worth falling back to stale data for, as opposed to a durable "no key"
// or "no data" condition where showing old bars would be actively
// misleading (e.g. the account is genuinely logged out, not just
// rate-limited for a minute). Every probeXxx function in this package
// follows the same convention -- "no key (...)" / "no data (...)" for the
// two durable outcomes, "error: ..." for everything else (network
// failures, timeouts, and Anthropic's usage-endpoint 429) -- documented
// on Usage.Err and in probe_claude.go's fetchClaudeUsage.
func isTransientErr(err string) bool {
	return strings.HasPrefix(err, "error:")
}

// withLastGoodFallback is applied to every provider's freshly probed
// Usage inside ProbeWithOptions. A clean success updates the memory for
// next time and passes through unchanged. A transient failure
// (isTransientErr) with a remembered last-good reading returns that
// reading's bars/spend/detail/plan instead, with Err cleared and
// Stale/StaleSince/Warn set so callers can render it as "old but real"
// rather than a hard error. A durable failure, or a transient one with no
// memory to fall back to, passes u through unchanged.
func withLastGoodFallback(name string, u Usage) Usage {
	if u.Err == "" {
		rememberIfGood(name, u)
		return u
	}
	if !isTransientErr(u.Err) {
		return u
	}
	good, ok := lastGoodFor(name)
	if !ok {
		return u
	}
	stale := u
	stale.Bars = good.Bars
	stale.Spend = good.Spend
	stale.Detail = good.Detail
	if good.Plan != "" {
		stale.Plan = good.Plan
	}
	stale.Err = ""
	stale.Stale = true
	stale.StaleSince = good.ProbedAt
	stale.Warn = fmt.Sprintf("showing usage from %s ago (usage endpoint rate-limited)",
		FormatDurationHuman(time.Since(good.ProbedAt)))
	return stale
}

// ---- Claude OAuth stagger (item 3: space the two accounts' HTTP calls) ----

// claudeStaggerDelay is the minimum gap ProbeWithOptions enforces between
// two claude-oauth providers' outbound calls to
// api.anthropic.com/api/oauth/usage within the same probe round, so both
// Claude accounts (the personal Max login and the ButterStack Pro login)
// never hit the endpoint in the same instant and trip its per-token rate
// limit together.
const claudeStaggerDelay = 1500 * time.Millisecond

// claudeOAuthStagger serializes claude-oauth probes within ONE
// ProbeWithOptions call -- it is created fresh per round (see the local
// `stagger` in ProbeWithOptions), never shared across rounds or callers,
// so it only spaces out providers that are actually being probed
// together right now. A provider skipped by the TTL cache never calls
// wait, so a round with only one claude-oauth provider actually probing
// incurs no delay at all.
type claudeOAuthStagger struct {
	mu   sync.Mutex
	next time.Time // earliest time the next claude-oauth call may start
}

// wait blocks the calling goroutine until it's this stagger's turn (its
// reserved slot, claudeStaggerDelay after the previous one) or ctx ends,
// whichever comes first, then reserves the following slot for whoever
// calls next.
func (s *claudeOAuthStagger) wait(ctx context.Context) {
	s.mu.Lock()
	start := time.Now()
	if s.next.After(start) {
		start = s.next
	}
	s.next = start.Add(claudeStaggerDelay)
	s.mu.Unlock()

	d := time.Until(start)
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
