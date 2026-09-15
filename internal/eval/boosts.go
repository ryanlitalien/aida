package eval

import (
	"regexp"
	"strings"
)

// failureBoostMagnitudes is the policy table mapping issue types to
// per-occurrence score adjustments. Conservative defaults: explicit
// thumbs-down lessons already use ±100; auto-generated eval signal
// is noisier, so we use ±25 here. A single recent failure is a hint;
// multiple repeated failures across recent runs add up to a real
// adjustment, capped by MaxAdjustment.
//
// Keys correspond to Issue.Type values produced by Reviewers in
// this package. Unknown types contribute zero - adding a new
// failure type that should drive routing means adding an entry
// here, not changing call sites.
var failureBoostMagnitudes = map[string]int{
	// CitationReviewer signals
	// ------------------------
	// Synth cited an artifact that doesn't exist in the source's
	// returned results - the source had thin or wrong-shaped data
	// for this kind of question. Demote it for similar questions.
	"missing-citation": -25,

	// Synth cited a source that wasn't queried at all. Either the
	// LLM hallucinated the source, or the planner failed to route
	// to a source the synth thought it needed. Slight boost on
	// the cited name so it shows up next time and we can confirm.
	"missing-source": +25,

	// Synth bug, not a routing decision - no router action.
	"placeholder-citation": 0,

	// CompletenessReviewer signals
	// ----------------------------
	// "early-stopping" means the synth received N artifacts but
	// produced a single-statement / single-bullet answer instead
	// of a list. Often a synth-side prompt issue rather than a
	// source-side one - the source returned the right data, the
	// synth collapsed it. Magnitude smaller than missing-citation
	// because the source isn't necessarily at fault.
	"early-stopping": -10,

	// "no-numeric-answer" fires when the user asked "how many"
	// and the synth's answer has no number. Same reasoning: this
	// is a synth fluency issue more than a routing issue, but a
	// small demotion is reasonable when one source repeatedly
	// produces results that the synth can't count from.
	"no-numeric-answer": -10,

	// ScopeReviewer signals
	// ---------------------
	// "scope-mismatch" means the user asked about token X but no
	// source's Command/Summary/artifact-ids contain X - typically
	// the planner routed correctly by name but the executor's
	// LLM call dropped the scope filter. Demote magnitude is
	// noticeable but capped: a routing-side failure deserves
	// boost-table action, but a single missed token shouldn't
	// dominate explicit thumbs-down feedback.
	//
	// Note: scope tokens are anchored as the bare token (no
	// "(source: id)" wrapper) so extractSourceFromAnchor returns
	// "" for them. That's intentional - scope-mismatch isn't
	// attributable to ONE source; the demote applies only when
	// future reviewers anchor a source name explicitly.
	"scope-mismatch": -15,
}

// MaxAdjustment caps the cumulative per-source boost so a single
// repeating failure type can't dominate explicit user feedback
// (which uses ±100) or strong deterministic signals like the
// +100 route boost in the planner.
const MaxAdjustment = 50

// citationAnchorPattern parses Issue.Anchor strings of the form
// "(source: id)" - same shape as the citation reviewer's matches.
// Source name is the captured group; the id half doesn't matter
// for routing decisions.
var citationAnchorPattern = regexp.MustCompile(`\(([a-z0-9_-]+)\s*:\s*[A-Za-z0-9_./-]+\)`)

// FailureBoosts converts a slice of ReviewRecords (typically loaded
// from recent failed eval-runs) into per-source score adjustments
// the router can apply additively to its existing prior_score.
//
// Positive value → boost this source for similar questions.
// Negative value → demote this source for similar questions.
//
// Per-source results are clamped to ±MaxAdjustment.
//
// Pass-verdict records and records with no Issues contribute
// nothing - only failure signals shape routing.
func FailureBoosts(records []ReviewRecord) map[string]int {
	out := map[string]int{}
	for _, r := range records {
		if r.Verdict != VerdictFail {
			continue
		}
		for _, iss := range r.Issues {
			mag, ok := failureBoostMagnitudes[iss.Type]
			if !ok || mag == 0 {
				continue
			}
			src := extractSourceFromAnchor(iss.Anchor)
			if src == "" {
				continue
			}
			out[src] += mag
		}
	}
	// Clamp each per-source adjustment to ±MaxAdjustment so a
	// single repeating issue type can't overpower stronger
	// deterministic signals.
	for k, v := range out {
		switch {
		case v > MaxAdjustment:
			out[k] = MaxAdjustment
		case v < -MaxAdjustment:
			out[k] = -MaxAdjustment
		}
	}
	return out
}

// extractSourceFromAnchor parses the source name out of a
// "(source: id)" anchor string. Returns "" when the anchor
// doesn't match the expected shape - caller treats that as
// "no signal."
func extractSourceFromAnchor(anchor string) string {
	m := citationAnchorPattern.FindStringSubmatch(anchor)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// FailureAttribution decodes a single Issue into the
// (source, type, severity) triple consumers like `aida brain analyze`
// need to group and summarize. Issues whose Anchor doesn't carry
// a parseable source name are dropped; the router boost path
// already ignores those so they don't contribute usable signal.
type FailureAttribution struct {
	Source    string
	IssueType string
	Severity  string
	Anchor    string
}

// ParseFailures flattens a slice of ReviewRecords into a list of
// FailureAttributions ready for grouping. Pass-verdict records and
// records whose Issues lack a parseable source are silently
// excluded - only the actionable failure signal flows through.
func ParseFailures(records []ReviewRecord) []FailureAttribution {
	var out []FailureAttribution
	for _, r := range records {
		if r.Verdict != VerdictFail {
			continue
		}
		for _, iss := range r.Issues {
			src := extractSourceFromAnchor(iss.Anchor)
			if src == "" {
				continue
			}
			out = append(out, FailureAttribution{
				Source:    src,
				IssueType: iss.Type,
				Severity:  iss.Severity,
				Anchor:    iss.Anchor,
			})
		}
	}
	return out
}
