package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/sources"
)

func mkResult(name string, ids ...string) sources.SourceResult {
	arts := make([]sources.Artifact, len(ids))
	for i, id := range ids {
		arts[i] = sources.Artifact{Type: "row", ID: id}
	}
	return sources.SourceResult{Source: name, Status: "success", Artifacts: arts}
}

func TestCitationReviewerPasses_AllValid(t *testing.T) {
	in := ReviewInput{
		Question: "what are my open PRs?",
		Answer: `You have 2 open PRs.

- [Foo](https://example.com/1) (github: pr-1)
- [Bar](https://example.com/2) (github: pr-2)

Sources: github: pr-1, pr-2`,
		Results: []sources.SourceResult{mkResult("github", "pr-1", "pr-2")},
	}
	rec, err := NewCitationReviewer().Review(context.Background(), in)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if rec.Verdict != VerdictPass {
		t.Errorf("Verdict = %q, want pass; rationale=%q issues=%+v", rec.Verdict, rec.Rationale, rec.Issues)
	}
	if rec.Score != 1.0 {
		t.Errorf("Score = %v, want 1.0", rec.Score)
	}
}

func TestCitationReviewerFails_MissingArtifact(t *testing.T) {
	in := ReviewInput{
		Question: "what are my open PRs?",
		Answer:   `You have 2 PRs (github: pr-1) and (github: pr-NONEXISTENT).`,
		Results:  []sources.SourceResult{mkResult("github", "pr-1", "pr-2")},
	}
	rec, _ := NewCitationReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", rec.Verdict)
	}
	if len(rec.Issues) != 1 {
		t.Fatalf("Issues count = %d, want 1; got %+v", len(rec.Issues), rec.Issues)
	}
	if rec.Issues[0].Type != "missing-citation" {
		t.Errorf("Issues[0].Type = %q, want missing-citation", rec.Issues[0].Type)
	}
	// Score reflects 1 of 2 valid.
	if rec.Score != 0.5 {
		t.Errorf("Score = %v, want 0.5", rec.Score)
	}
}

func TestCitationReviewerFails_MissingSource(t *testing.T) {
	in := ReviewInput{
		Answer:  "the answer (snowflake: row-1) and (chrono: log-99)",
		Results: []sources.SourceResult{mkResult("snowflake", "row-1")},
	}
	rec, _ := NewCitationReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail; got %+v", rec.Verdict, rec)
	}
	var found bool
	for _, iss := range rec.Issues {
		if iss.Type == "missing-source" && strings.Contains(iss.Message, "chrono") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a missing-source issue for chrono, got %+v", rec.Issues)
	}
}

func TestCitationReviewerFails_PlaceholderAntipattern(t *testing.T) {
	// The synth prompt explicitly forbids using the source name as
	// the artifact id; the reviewer must catch this.
	in := ReviewInput{
		Answer:  "summary (github: github)",
		Results: []sources.SourceResult{mkResult("github", "pr-1")},
	}
	rec, _ := NewCitationReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", rec.Verdict)
	}
	if len(rec.Issues) != 1 || rec.Issues[0].Type != "placeholder-citation" {
		t.Errorf("expected one placeholder-citation issue, got %+v", rec.Issues)
	}
}

func TestCitationReviewer_NoCitationsIsPass(t *testing.T) {
	// An answer without inline citations isn't this reviewer's
	// concern; another reviewer (completeness) will catch it.
	in := ReviewInput{
		Answer:  "no inline citations in this answer at all.",
		Results: []sources.SourceResult{mkResult("github", "pr-1")},
	}
	rec, _ := NewCitationReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("Verdict = %q, want pass; rationale=%q", rec.Verdict, rec.Rationale)
	}
}

func TestCitationReviewer_HandlesIDsWithDotsAndDashes(t *testing.T) {
	// aida artifact IDs commonly contain '.' '/' '_' '-' (see
	// e.g. butterstack-butter-stack-571 in real run output).
	in := ReviewInput{
		Answer: "result (github: butterstack-butter-stack-571) and (snowflake: rows.2026-04-30.001)",
		Results: []sources.SourceResult{
			mkResult("github", "butterstack-butter-stack-571"),
			mkResult("snowflake", "rows.2026-04-30.001"),
		},
	}
	rec, _ := NewCitationReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("Verdict = %q, want pass; got %+v", rec.Verdict, rec)
	}
}

func TestCitationReviewerFails_GenericPrefixMissingSource(t *testing.T) {
	// Generic prefixes like "source:" name the claimed source directly
	// in group2 rather than an artifact id (e.g. a synthesizer citing a
	// web page by domain instead of the queried "web-search" source).
	// The finding must name the claimed source ("timeanddate.com"), not
	// the generic word "source".
	in := ReviewInput{
		Answer:  "It's currently 3pm in Chicago (source: timeanddate.com).",
		Results: []sources.SourceResult{mkResult("web-search", "result-1")},
	}
	rec, _ := NewCitationReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail; got %+v", rec.Verdict, rec)
	}
	if len(rec.Issues) != 1 || rec.Issues[0].Type != "missing-source" {
		t.Fatalf("expected one missing-source issue, got %+v", rec.Issues)
	}
	if !strings.Contains(rec.Issues[0].Message, `"timeanddate.com"`) {
		t.Errorf("Message = %q, want it to name the claimed source %q, not the generic prefix",
			rec.Issues[0].Message, "timeanddate.com")
	}
	if strings.Contains(rec.Issues[0].Message, `source "source"`) {
		t.Errorf("Message = %q, must not report the generic prefix word as the claimed source",
			rec.Issues[0].Message)
	}
}

func TestCitationReviewerPasses_GenericPrefixQueriedSource(t *testing.T) {
	// "(source: web-search)" names a source that WAS actually queried -
	// a non-canonical but benign citation form, not fabrication.
	in := ReviewInput{
		Answer:  "It's currently 3pm in Chicago (source: web-search).",
		Results: []sources.SourceResult{mkResult("web-search", "result-1")},
	}
	rec, _ := NewCitationReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("Verdict = %q, want pass; rationale=%q issues=%+v", rec.Verdict, rec.Rationale, rec.Issues)
	}
}

func TestCitationReviewer_NameIsStable(t *testing.T) {
	if (CitationReviewer{}).Name() != "citation" {
		t.Errorf("Name() = %q, want %q", (CitationReviewer{}).Name(), "citation")
	}
}

func TestCitationReviewer_IgnoresMarkdownLinks(t *testing.T) {
	// Markdown link "[text](url)" looks like "(scheme:rest)" to a
	// naive regex. The reviewer must distinguish.
	in := ReviewInput{
		Answer: `Result is at [Foo](https://example.com/1) (github: pr-1).
Also see [Bar](http://example.com/2) (github: pr-2).`,
		Results: []sources.SourceResult{mkResult("github", "pr-1", "pr-2")},
	}
	rec, _ := NewCitationReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("Verdict = %q, want pass; rationale=%q issues=%+v",
			rec.Verdict, rec.Rationale, rec.Issues)
	}
}
