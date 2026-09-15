package eval

import (
	"context"
	"testing"

	"github.com/ryanlitalien/aida/internal/sources"
)

func TestScopeReviewer_NameIsStable(t *testing.T) {
	if (ScopeReviewer{}).Name() != "scope" {
		t.Errorf("Name() = %q, want scope", (ScopeReviewer{}).Name())
	}
}

func TestScopeReviewer_PassWhenScopeInCommand(t *testing.T) {
	in := ReviewInput{
		Question: "what are my open PRs in butter-stack?",
		Answer:   "You have 13 open PRs.",
		Results: []sources.SourceResult{
			{
				Source:  "github",
				Command: "search prs --repo butterstack/butter-stack --state open",
				Artifacts: []sources.Artifact{
					{ID: "butterstack-butter-stack-571"},
				},
			},
		},
	}
	rec, _ := NewScopeReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("Verdict = %q, want pass; %+v", rec.Verdict, rec)
	}
}

func TestScopeReviewer_WarnsWhenScopeMissing(t *testing.T) {
	// User asks about Campbutz but the github command was scoped
	// elsewhere - classic dropped-filter failure.
	in := ReviewInput{
		Question: "what's the latest Campbutz YTD?",
		Answer:   "Various PRs are open.",
		Results: []sources.SourceResult{
			{
				Source:  "github",
				Command: "search prs --owner some-other-org --state open",
			},
		},
	}
	rec, _ := NewScopeReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictWarn {
		t.Errorf("Verdict = %q, want warn", rec.Verdict)
	}
	// "Campbutz" should be flagged. (YTD is also nounish but only
	// appears once; we don't dedup uppercase - both might show.)
	foundCampbutz := false
	for _, iss := range rec.Issues {
		if iss.Anchor == "campbutz" {
			foundCampbutz = true
		}
	}
	if !foundCampbutz {
		t.Errorf("expected scope-mismatch issue for campbutz, got %+v", rec.Issues)
	}
}

func TestScopeReviewer_StopwordsDoNotTrigger(t *testing.T) {
	// "What", "How" at sentence start are capitalized but should
	// not be flagged as scope tokens.
	in := ReviewInput{
		Question: "What are the latest changes?",
		Answer:   "Several recent changes.",
		Results: []sources.SourceResult{
			{Source: "github", Command: "log --oneline -10"},
		},
	}
	rec, _ := NewScopeReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("stopword-only question should pass; got %+v", rec)
	}
}

func TestScopeReviewer_FreeFormPasses(t *testing.T) {
	in := ReviewInput{
		Question: "why did the deploy fail",
		Answer:   "Missing env var.",
		Results: []sources.SourceResult{
			{Source: "plausible", Command: "search 'deploy fail'"},
		},
	}
	rec, _ := NewScopeReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("free-form question should pass; got %+v", rec)
	}
}

func TestScopeReviewer_NoResultsSkipsCheck(t *testing.T) {
	// No source ran - nothing to compare scope against.
	in := ReviewInput{
		Question: "what's in butter-stack?",
		Answer:   "(empty)",
		Results:  nil,
	}
	rec, _ := NewScopeReviewer().Review(context.Background(), in)
	if rec != nil {
		t.Errorf("empty results should yield nil record, got %+v", rec)
	}
}

func TestScopeReviewer_MatchesAcrossArtifactIDs(t *testing.T) {
	// Token doesn't appear in command but does appear in artifact
	// IDs - should still pass. butter_stack → butterstack-butter-stack-571
	in := ReviewInput{
		Question: "what's in butter-stack?",
		Answer:   "13 PRs.",
		Results: []sources.SourceResult{
			{
				Source:  "github",
				Command: "search prs --repo X/Y --state open",
				Artifacts: []sources.Artifact{
					{ID: "butterstack-butter-stack-571"},
				},
			},
		},
	}
	rec, _ := NewScopeReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("Verdict = %q, want pass (artifact id should satisfy scope)", rec.Verdict)
	}
}

func TestScopeReviewer_HyphenatedTokenExtraction(t *testing.T) {
	tokens := extractScopeTokens("show me my partner-onboarding tickets")
	wantContains := "partner-onboarding"
	found := false
	for _, t := range tokens {
		if t == wantContains {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q in tokens, got %v", wantContains, tokens)
	}
}

func TestScopeReviewer_CamelCaseTokenExtraction(t *testing.T) {
	tokens := extractScopeTokens("look up CitiConnect status")
	wantContains := "citiconnect"
	found := false
	for _, tok := range tokens {
		if tok == wantContains {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q in tokens, got %v", wantContains, tokens)
	}
}

func TestScopeReviewer_EmptyQuestion(t *testing.T) {
	if rec, _ := NewScopeReviewer().Review(context.Background(), ReviewInput{}); rec != nil {
		t.Errorf("empty question should skip; got %+v", rec)
	}
}
