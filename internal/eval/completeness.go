package eval

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// CompletenessReviewer is a deterministic Reviewer that checks
// whether a synthesized answer covers the shape the user asked
// for. Maps to the FAMA paper's "Incomplete Fulfillment /
// Early Stopping" failure bucket - the answer-shape failures
// the citation reviewer can't see.
//
// What it catches in v1:
//   - List question ("all my PRs", "show me my tasks", "list X")
//     with results returning multiple artifacts, but the answer
//     has no markdown bullets and only mentions one item.
//   - Count question ("how many", "count of") with no number
//     anywhere in the answer.
//
// What it does NOT catch (deferred to inferential reviewers):
//   - Answers that have the right SHAPE but cover the wrong
//     ENTITIES (a separate scope reviewer's job).
//   - Subtle "answered the wrong question" failures where the
//     synth pivoted to a related but distinct topic.
type CompletenessReviewer struct{}

func NewCompletenessReviewer() *CompletenessReviewer { return &CompletenessReviewer{} }

func (CompletenessReviewer) Name() string { return "completeness" }

// listQuestionPattern catches list-shaped requests. Order
// matters slightly: "list X" should match before "X" so we
// don't false-trigger on every question that happens to
// contain "all". Keep this conservative - false positives
// produce noisy failures that demote sources unfairly via the
// router-boost mechanism.
var listQuestionPattern = regexp.MustCompile(
	`(?i)\b(all my|all the|list (all|every|my|the)|show me (all|every|my|the)|what are (my|the|all)|every )\b`,
)

// countQuestionPattern catches count-shaped requests. "How
// many" is the canonical form; "count of" / "number of" / "total"
// also count.
var countQuestionPattern = regexp.MustCompile(
	`(?i)\b(how many|count of|count the|number of|total (number|count) of)\b`,
)

// numericAnswerPattern detects whether the answer contains an
// integer or float anywhere. Used to gate the count-question check.
var numericAnswerPattern = regexp.MustCompile(`\b\d+(\.\d+)?\b`)

// bulletLinePattern catches markdown bullet lines starting with
// "- " or "* " at the line start. Tabs / leading whitespace
// before the bullet are tolerated (nested list).
var bulletLinePattern = regexp.MustCompile(`(?m)^\s*[-*]\s+`)

// Review implements Reviewer. Returns Pass when the answer's
// shape matches the question's shape; Fail with one Issue per
// shape mismatch; Pass when the question doesn't trigger any
// shape rule (the reviewer has no opinion on free-form questions).
func (CompletenessReviewer) Review(ctx context.Context, in ReviewInput) (*ReviewRecord, error) {
	q := strings.TrimSpace(in.Question)
	a := strings.TrimSpace(in.Answer)

	if q == "" || a == "" {
		// No opinion on empty input - the citation reviewer
		// or a dedicated empty-answer reviewer will catch this.
		return nil, nil
	}

	// Total artifact count across all results - used as a
	// gate so we don't flag a list-question with no results
	// as "early stopping" (it's just empty data, not stopped).
	totalArtifacts := 0
	for _, r := range in.Results {
		totalArtifacts += len(r.Artifacts)
	}

	var issues []Issue

	// List-shape check: question wants a list, answer should
	// have bullets (or at minimum mention multiple items).
	// Skip when no artifacts came back - that's a different
	// failure mode (the synth correctly says "no results").
	if listQuestionPattern.MatchString(q) && totalArtifacts >= 2 {
		bulletCount := len(bulletLinePattern.FindAllString(a, -1))
		if bulletCount == 0 {
			issues = append(issues, Issue{
				Type:     "early-stopping",
				Severity: "warn",
				Message: fmt.Sprintf(
					"question asked for a list but answer has no markdown bullets (- / *) - "+
						"results returned %d artifact(s) yet the answer reads as a single statement",
					totalArtifacts),
			})
		} else if bulletCount == 1 && totalArtifacts >= 3 {
			// One bullet on a list question with 3+ results is
			// also suspicious - likely the synth dropped most
			// of the artifacts. Use warn rather than fail
			// because a deliberate "top result" answer is valid
			// in some contexts.
			issues = append(issues, Issue{
				Type:     "early-stopping",
				Severity: "warn",
				Message: fmt.Sprintf(
					"list question got %d artifact(s) but the answer has only 1 bullet - "+
						"likely truncated mid-list",
					totalArtifacts),
			})
		}
	}

	// Count-shape check: "how many X" should produce a number.
	if countQuestionPattern.MatchString(q) && !numericAnswerPattern.MatchString(a) {
		issues = append(issues, Issue{
			Type:     "no-numeric-answer",
			Severity: "error",
			Message:  "question asked for a count but the answer contains no numeric value",
		})
	}

	if len(issues) == 0 {
		return &ReviewRecord{
			Reviewer:  "completeness",
			Verdict:   VerdictPass,
			Score:     1.0,
			Rationale: "answer shape matches question shape",
		}, nil
	}

	verdict := VerdictWarn
	for _, iss := range issues {
		if iss.Severity == "error" {
			verdict = VerdictFail
			break
		}
	}
	return &ReviewRecord{
		Reviewer:  "completeness",
		Verdict:   verdict,
		Issues:    issues,
		Rationale: fmt.Sprintf("%d shape issue(s)", len(issues)),
	}, nil
}
