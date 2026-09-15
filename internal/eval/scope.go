package eval

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// ScopeReviewer is a deterministic Reviewer that checks whether the
// scope the user asked about (specific repos, partner names,
// hyphenated project tokens) actually appears in the source
// commands that ran. Maps to the FAMA paper's "Domain Policy
// Violation" / "Incorrect Retrieval" bucket - answer-truthfulness
// failures that arise when the planner routed correctly by name
// but the executor's command dropped the scope filter.
//
// What it catches in v1:
//   - Question contains a multi-word/hyphenated/CamelCase token
//     (e.g. "butterstack", "butter-stack", "Citi Connect") that
//     doesn't appear in any source's actual Command.
//   - Surfaces every offending token so the router-boost
//     mechanism can demote sources that consistently lose scope.
//
// What it does NOT catch (deferred to inferential reviewers):
//   - Semantic scope drift (synth answered about topic Y when the
//     user asked about topic X without naming it explicitly).
//   - Time-window mismatches ("last week" vs. command without a
//     date filter) - needs intent/timeframe wiring through to
//     ReviewInput.
type ScopeReviewer struct{}

// NewScopeReviewer returns a stateless reviewer instance.
func NewScopeReviewer() *ScopeReviewer { return &ScopeReviewer{} }

// Name implements Reviewer.
func (ScopeReviewer) Name() string { return "scope" }

// scopeTokenPattern matches "named-thing" tokens in a question:
//   - hyphenated lowercase (butter-stack)
//   - CamelCase (ButterStack, MyProject)
//   - Capitalized standalone (Campbutz)
//   - all-caps abbreviations 2+ chars (NYT, GMV) - note: must
//     come AFTER the more specific patterns or it'll greedy-match
//
// Stopwords like "All", "What", "How" are dropped post-match so
// they don't trigger false positives at the start of a sentence.
var scopeTokenPattern = regexp.MustCompile(
	`\b(?:[a-z]+-[a-z][a-z\-]*|[A-Z][a-z]+(?:[A-Z][a-z]*)+|[A-Z][a-z]{3,}|[A-Z]{2,})\b`,
)

// scopeStopwords are tokens the regex catches that aren't real
// scope filters. Lowercase comparison.
var scopeStopwords = map[string]bool{
	// Common sentence-starters that get capitalized
	"what":  true,
	"which": true,
	"where": true,
	"when":  true,
	"how":   true,
	"why":   true,
	"who":   true,
	"show":  true,
	"list":  true,
	"give":  true,
	"tell":  true,
	"find":  true,
	// Common verbs that appear capitalized
	"create": true,
	"delete": true,
	"update": true,
	"fetch":  true,
	"check":  true,
	// Quantifiers
	"all":   true,
	"every": true,
	"some":  true,
	"each":  true,
	// Question-shape words
	"open":   true,
	"closed": true,
	"latest": true,
	"recent": true,
	"first":  true,
	"last":   true,
	// Time
	"yesterday": true,
	"today":     true,
	"week":      true,
	"month":     true,
	"year":      true,
}

// Review implements Reviewer. Returns Pass when every nounish
// token from the question appears somewhere in the source
// commands that ran; Fail with one Issue per missing token
// otherwise. Pass also when no nounish tokens are detected
// (free-form questions get no opinion).
func (ScopeReviewer) Review(ctx context.Context, in ReviewInput) (*ReviewRecord, error) {
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return nil, nil
	}
	if len(in.Results) == 0 {
		// No source commands to compare against - skip rather
		// than reporting every token as a miss. Empty-result
		// failures are a different reviewer's concern.
		return nil, nil
	}

	tokens := extractScopeTokens(q)
	if len(tokens) == 0 {
		return &ReviewRecord{
			Reviewer:  "scope",
			Verdict:   VerdictPass,
			Score:     1.0,
			Rationale: "no scope tokens in question",
		}, nil
	}

	// Concatenate every command + (where present) the result
	// summary and the artifact ids so a token captured in an
	// artifact path also counts as "in scope" - handles cases
	// like "show me PRs in butter_stack" where butter_stack
	// appears as butterstack-butter-stack-571 in artifact ids.
	var pool strings.Builder
	for _, r := range in.Results {
		pool.WriteString(r.Command)
		pool.WriteString(" ")
		pool.WriteString(r.Summary)
		pool.WriteString(" ")
		for _, a := range r.Artifacts {
			pool.WriteString(a.ID)
			pool.WriteString(" ")
		}
	}
	poolLower := strings.ToLower(pool.String())

	var missing []string
	for _, tok := range tokens {
		if !strings.Contains(poolLower, tok) {
			missing = append(missing, tok)
		}
	}

	if len(missing) == 0 {
		return &ReviewRecord{
			Reviewer:  "scope",
			Verdict:   VerdictPass,
			Score:     1.0,
			Rationale: fmt.Sprintf("all %d scope token(s) found in commands/results", len(tokens)),
		}, nil
	}

	// One issue per missing token so the router-boost mechanism
	// can attribute demotion correctly.
	var issues []Issue
	for _, tok := range missing {
		issues = append(issues, Issue{
			Type:     "scope-mismatch",
			Severity: "warn",
			Message: fmt.Sprintf(
				"question mentions %q but no source command, summary, or artifact id contains it - the planner may have dropped the scope filter",
				tok),
			Anchor: tok,
		})
	}

	return &ReviewRecord{
		Reviewer:  "scope",
		Verdict:   VerdictWarn,
		Score:     float64(len(tokens)-len(missing)) / float64(len(tokens)),
		Issues:    issues,
		Rationale: fmt.Sprintf("%d of %d scope token(s) missing from commands", len(missing), len(tokens)),
	}, nil
}

// extractScopeTokens pulls candidate scope tokens out of a
// question and lowercases + deduplicates them. Stopwords are
// dropped here, not by the regex, so the regex stays
// readable.
func extractScopeTokens(question string) []string {
	matches := scopeTokenPattern.FindAllString(question, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		lower := strings.ToLower(m)
		if scopeStopwords[lower] {
			continue
		}
		if seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, lower)
	}
	return out
}
