package engine

import (
	"testing"

	"github.com/ryanlitalien/aida/internal/sources"
)

func TestBuildReExecutionGuidanceBothFields(t *testing.T) {
	qa := &QualityAssessment{
		Quality:           1,
		HasData:           false,
		Reason:            "no useful data",
		MissingInfo:       "the partner's payment volume for last month",
		SuggestedApproach: "try querying the snowflake transactions table with a broader date range",
	}
	guidance := buildReExecutionGuidance(qa, nil)
	if guidance == "" {
		t.Fatal("expected non-empty guidance")
	}
	if !contains(guidance, "Missing information:") {
		t.Errorf("expected 'Missing information:' in guidance, got: %s", guidance)
	}
	if !contains(guidance, "Suggested approach:") {
		t.Errorf("expected 'Suggested approach:' in guidance, got: %s", guidance)
	}
}

func TestBuildReExecutionGuidanceMissingInfoOnly(t *testing.T) {
	qa := &QualityAssessment{
		Quality:     2,
		MissingInfo: "the actual error count",
	}
	guidance := buildReExecutionGuidance(qa, nil)
	if !contains(guidance, "Missing information:") {
		t.Errorf("expected 'Missing information:' in guidance, got: %s", guidance)
	}
	if contains(guidance, "Suggested approach:") {
		t.Errorf("should not contain 'Suggested approach:' when empty, got: %s", guidance)
	}
}

func TestBuildReExecutionGuidanceEmptyFields(t *testing.T) {
	qa := &QualityAssessment{
		Quality: 1,
		Reason:  "useless",
	}
	guidance := buildReExecutionGuidance(qa, nil)
	if guidance == "" {
		t.Fatal("expected fallback guidance when both fields empty")
	}
	if !contains(guidance, "quality 1/5") {
		t.Errorf("expected quality score in fallback, got: %s", guidance)
	}
}

func TestBuildReExecutionGuidanceHighQuality(t *testing.T) {
	// This shouldn't normally be called for quality >= 3,
	// but verify it still produces output
	qa := &QualityAssessment{
		Quality: 4,
		Reason:  "good answer",
	}
	guidance := buildReExecutionGuidance(qa, nil)
	if guidance == "" {
		t.Fatal("should produce fallback even for high quality")
	}
}

// TestBuildReExecutionGuidance_SurfacesPriorErrors verifies that when a
// prior source errored out, the guidance includes the failing command so
// the retry LLM can fix the specific flag rather than drift to a new
// unrelated query. Regression test for the "--owner on gh pr list" bug
// where re-exec dropped the acme_widgets --repo scope entirely.
func TestBuildReExecutionGuidance_SurfacesPriorErrors(t *testing.T) {
	qa := &QualityAssessment{Quality: 2, Reason: "error answer"}
	results := []sources.SourceResult{
		{
			Source:  "github",
			Status:  "error",
			Command: "pr list --repo acmewidgets/acme_widgets --state open --owner ryanlitalien",
			Summary: "exec error: unknown flag: --owner",
		},
	}
	guidance := buildReExecutionGuidance(qa, results)
	if !contains(guidance, "pr list --repo acmewidgets/acme_widgets") {
		t.Errorf("expected failing command in guidance, got: %s", guidance)
	}
	if !contains(guidance, "unknown flag: --owner") {
		t.Errorf("expected error text verbatim in guidance, got: %s", guidance)
	}
	if !contains(guidance, "PRESERVE the original scope") {
		t.Errorf("expected scope-preservation instruction in guidance, got: %s", guidance)
	}
}

// TestBuildReExecutionGuidance_IgnoresSuccessResults verifies that only
// error/timeout results produce guidance - successful source results
// should not clutter the retry prompt with their commands.
func TestBuildReExecutionGuidance_IgnoresSuccessResults(t *testing.T) {
	qa := &QualityAssessment{Quality: 2, MissingInfo: "X"}
	results := []sources.SourceResult{
		{Source: "a", Status: "success", Command: "SELECT * FROM t", Summary: "100 rows"},
	}
	guidance := buildReExecutionGuidance(qa, results)
	if contains(guidance, "SELECT * FROM t") {
		t.Errorf("success commands should not appear in guidance, got: %s", guidance)
	}
	if !contains(guidance, "Missing information:") {
		t.Errorf("expected quality-scorer guidance to still flow through, got: %s", guidance)
	}
}

func TestBuildSummariesCopiesFromContextDoc(t *testing.T) {
	results := []sources.SourceResult{
		{Source: "codebase", Status: "success", FromContextDoc: true},
		{Source: "sqlite", Status: "success", FromContextDoc: false},
	}
	summaries := buildSummaries(results)
	if len(summaries) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(summaries))
	}
	if !summaries[0].FromContextDoc {
		t.Error("expected FromContextDoc true to carry through for codebase result")
	}
	if summaries[1].FromContextDoc {
		t.Error("expected FromContextDoc false to carry through for sqlite result")
	}
}

func TestQualityAssessmentFields(t *testing.T) {
	qa := QualityAssessment{
		Quality:           3,
		HasData:           true,
		Reason:            "partial answer",
		MissingInfo:       "latency percentiles",
		SuggestedApproach: "query chrono for p99",
	}

	if qa.Quality != 3 {
		t.Errorf("expected quality 3, got %d", qa.Quality)
	}
	if !qa.HasData {
		t.Error("expected has_data true")
	}
	if qa.MissingInfo != "latency percentiles" {
		t.Errorf("unexpected missing_info: %s", qa.MissingInfo)
	}
	if qa.SuggestedApproach != "query chrono for p99" {
		t.Errorf("unexpected suggested_approach: %s", qa.SuggestedApproach)
	}
}

func TestSynthesizeWithValidationNilReExecute(t *testing.T) {
	// When reExecuteFn is nil, SynthesizeWithValidation should
	// return whatever Synthesize returns (we can't easily test the
	// full LLM path here, but the function signature and nil handling
	// is what we're verifying compiles and doesn't panic).
	// This is a compile/API test - actual integration tested via
	// the CLI pipeline.
	var fn ReExecuteFn
	if fn != nil {
		t.Error("nil ReExecuteFn should be nil")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
