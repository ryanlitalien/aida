// Package eval houses the sensor side of the aida harness - the
// reviewers that grade synthesized answers, the loop that runs them,
// and (in follow-up commits) the persistence layer that turns those
// grades into a routing signal.
//
// Conceptually this is the "Ralph Wiggum Loop" from OpenAI's harness
// engineering writeup: a generator (the synthesizer) produces an
// answer, then specialist reviewers grade it on narrow concerns
// (citations, scope, entity resolution, completeness), and the
// system can iterate. v1 ships the interface plus a single
// deterministic citation reviewer. The Loop runs reviewers once;
// the iterate-until-clean variant lives in a follow-up.
package eval

import (
	"context"

	"github.com/ryanlitalien/aida/internal/sources"
)

// ReviewInput is the full state a Reviewer receives. It carries the
// synthesized answer plus the upstream context that produced it.
// More fields may be added as new reviewers need them, but reviewers
// must tolerate zero values for any field they do not use.
type ReviewInput struct {
	Question string
	Answer   string
	Results  []sources.SourceResult

	// WorkDir is the directory a code-correctness reviewer (CodeReviewer)
	// runs its checks in. Empty means the current working directory.
	// Answer-grading reviewers ignore it.
	WorkDir string

	// Commands are the build/test/lint checks a CodeReviewer runs against
	// WorkDir. Empty means the CodeReviewer skips (no opinion); other
	// reviewers ignore it.
	Commands []CodeCheck
}

// Verdict is a Reviewer's bottom-line judgment.
type Verdict string

const (
	VerdictPass Verdict = "pass"
	VerdictWarn Verdict = "warn"
	VerdictFail Verdict = "fail"
)

// Issue is a single concern raised by a Reviewer. Multiple issues
// may be present in a Warn or Fail record.
type Issue struct {
	// Type is a short machine-readable identifier for the concern,
	// e.g. "missing-citation" or "scope-mismatch". Consumers (the
	// router boost, the brain analyzer) group on this.
	Type string `json:"type"`

	// Severity is one of "info" | "warn" | "error".
	Severity string `json:"severity"`

	// Message is the human-readable explanation.
	Message string `json:"message"`

	// Anchor is an optional pointer to the offending span - for a
	// citation issue this is the citation string itself, e.g.
	// "(github: bs-pr-571)". Empty when the issue is structural.
	Anchor string `json:"anchor,omitempty"`
}

// ReviewRecord is the structured output of a single Reviewer.
type ReviewRecord struct {
	// Reviewer matches the producing Reviewer's Name(). Loop fills
	// this in if a Reviewer leaves it empty, but Reviewers may set
	// it explicitly to make their tests independent of Loop.
	Reviewer string `json:"reviewer"`

	Verdict Verdict `json:"verdict"`

	// Score is an optional 0..1 quality score. Zero means
	// "not applicable" or "not used by this reviewer."
	Score float64 `json:"score,omitempty"`

	Issues []Issue `json:"issues,omitempty"`

	// Rationale is free-text explanation, written for human audit
	// (in `aida brain analyze` output) and for downstream consumers
	// that want to surface the reasoning.
	Rationale string `json:"rationale,omitempty"`
}

// Reviewer is a single specialist sensor. Each Reviewer has a
// narrow concern and produces a structured verdict on whether the
// answer satisfies it.
//
// Implementations may be deterministic (regex/scan over the answer
// + results) or inferential (LLM call). The Loop is agnostic.
type Reviewer interface {
	// Name returns the reviewer identifier. Used in records and as
	// the lookup key for category-specific routing boosts.
	Name() string

	// Review examines the input and returns a record. A nil record
	// (with nil error) is treated as "skipped - no opinion."
	Review(ctx context.Context, in ReviewInput) (*ReviewRecord, error)
}

// Loop runs every Reviewer over the input and returns all records.
// Reviewers run sequentially in the order provided - early results
// may inform later reviewers' work, and stable order makes tests
// deterministic.
//
// v1 makes one pass. The iterate-until-clean ("Ralph Wiggum")
// variant requires regenerating the answer in response to issues,
// which lives in the synthesizer integration commit.
func Loop(ctx context.Context, reviewers []Reviewer, in ReviewInput) ([]ReviewRecord, error) {
	records := make([]ReviewRecord, 0, len(reviewers))
	for _, r := range reviewers {
		rec, err := r.Review(ctx, in)
		if err != nil {
			return records, err
		}
		if rec == nil {
			continue
		}
		if rec.Reviewer == "" {
			rec.Reviewer = r.Name()
		}
		records = append(records, *rec)
	}
	return records, nil
}

// AggregateVerdict reduces a slice of records to a single verdict
// using fail-loud semantics: any Fail wins, then any Warn, else Pass.
// Empty input returns Pass.
func AggregateVerdict(records []ReviewRecord) Verdict {
	worst := VerdictPass
	for _, r := range records {
		switch r.Verdict {
		case VerdictFail:
			return VerdictFail
		case VerdictWarn:
			worst = VerdictWarn
		}
	}
	return worst
}

// DefaultReviewers is the canonical reviewer list used by the
// engine pipeline today. Test code that wants a custom set should
// build its own slice; production callers should use this so a
// new reviewer added here lights up everywhere at once.
//
// v1.2: citation + completeness + scope. Future entries:
// entity-resolution (every entity in the question is addressed
// in the answer) and hallucination (answer claims facts not in
// any artifact). Each reviewer maps to one FAMA failure bucket.
func DefaultReviewers() []Reviewer {
	return []Reviewer{
		NewCitationReviewer(),
		NewCompletenessReviewer(),
		NewScopeReviewer(),
	}
}
