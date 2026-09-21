package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/sources"
)

// TestRetryContextBuildingError verifies that the retry context string for
// error results includes the attempt number and the error message.
func TestRetryContextBuildingError(t *testing.T) {
	result := sources.SourceResult{
		Source:  "test-source",
		Status:  "error",
		Summary: "table USERS not found in schema PUBLIC",
		Command: "SELECT * FROM USERS",
	}

	// Simulate the retry context building logic from executeSource
	retryContext := ""
	attempt := 0
	if result.Status == "error" && result.Summary != "" {
		retryContext = fmt.Sprintf(
			"RETRY (attempt %d/%d): Your prior query failed with this error:\n%s\n\nFix the query to avoid this error. Try a completely different approach.",
			attempt+2, maxSourceRetries+1, truncateLine(result.Summary, 300),
		)
	}

	if retryContext == "" {
		t.Fatal("expected non-empty retryContext for error result")
	}
	if got := fmt.Sprintf("attempt %d/%d", attempt+2, maxSourceRetries+1); !containsSubstring(retryContext, got) {
		t.Errorf("retryContext missing attempt info %q: %s", got, retryContext)
	}
	if !containsSubstring(retryContext, "table USERS not found") {
		t.Error("retryContext missing error message")
	}
	if !containsSubstring(retryContext, "RETRY") {
		t.Error("retryContext missing RETRY prefix")
	}
}

// TestRetryContextBuildingEmpty verifies that the retry context string for
// empty results includes the attempt number and the original command.
func TestRetryContextBuildingEmpty(t *testing.T) {
	result := sources.SourceResult{
		Source:  "test-source",
		Status:  "empty",
		Command: "grep -r 'nonexistent' /project",
	}

	retryContext := ""
	attempt := 0
	if result.Status == "empty" {
		retryContext = fmt.Sprintf(
			"RETRY (attempt %d/%d): Your prior query returned NO results. The query was:\n%s\n\nTry a broader search pattern, different table names, or a different approach entirely.",
			attempt+2, maxSourceRetries+1, truncateLine(result.Command, 300),
		)
	}

	if retryContext == "" {
		t.Fatal("expected non-empty retryContext for empty result")
	}
	if !containsSubstring(retryContext, "NO results") {
		t.Error("retryContext missing 'NO results' phrase")
	}
	if !containsSubstring(retryContext, "grep -r") {
		t.Error("retryContext missing original command")
	}
}

// TestRetryContextBuildingTimeout verifies that the retry context string for
// timeout results includes the attempt number and the original command --
// a timeout must trigger a retry rather than being accepted as a good
// result on the first attempt.
func TestRetryContextBuildingTimeout(t *testing.T) {
	result := sources.SourceResult{
		Source:  "test-source",
		Status:  "timeout",
		Summary: "Command timed out after 30s",
		Command: "SELECT * FROM huge_table",
	}

	retryContext := ""
	attempt := 0
	if result.Status == "timeout" {
		retryContext = fmt.Sprintf(
			"RETRY (attempt %d/%d): Your prior query timed out:\n%s\n\nTry a narrower or cheaper query -- e.g. a tighter filter, a smaller time window, or fewer rows/joins.",
			attempt+2, maxSourceRetries+1, truncateLine(result.Command, 300),
		)
	}

	if retryContext == "" {
		t.Fatal("expected non-empty retryContext for timeout result")
	}
	if !containsSubstring(retryContext, "timed out") {
		t.Error("retryContext missing 'timed out' phrase")
	}
	if !containsSubstring(retryContext, "SELECT * FROM huge_table") {
		t.Error("retryContext missing original command")
	}
	if !containsSubstring(retryContext, "RETRY") {
		t.Error("retryContext missing RETRY prefix")
	}
}

// onceTimeoutFakeAdapter simulates an adapter (like exec.go post-fix) that
// reports a timeout via BOTH Status: "timeout" AND a non-nil Go error, so
// executeSourceOnce's `if err != nil` branch must consult result.Status
// instead of unconditionally flattening the outcome to "error".
type onceTimeoutFakeAdapter struct{}

func (onceTimeoutFakeAdapter) Execute(ctx context.Context, command string, src config.Source) (sources.SourceResult, error) {
	return sources.SourceResult{Status: "timeout", Summary: "Command timed out after 30s"}, fmt.Errorf("Command timed out after 30s")
}

func (onceTimeoutFakeAdapter) ParseOutput(raw []byte, src config.Source) ([]sources.Artifact, error) {
	return nil, nil
}

// TestExecuteSourceOnce_PreservesTimeoutStatus verifies that executeSourceOnce
// carries through an adapter's own Status/Summary (e.g. "timeout") instead of
// coercing every non-nil adapter error into the generic "error" status. This
// is what keeps the "timeout" branch of executeSource's retry-decision chain
// reachable, and what lets the synthesizer distinguish a timeout from a
// plain failure.
func TestExecuteSourceOnce_PreservesTimeoutStatus(t *testing.T) {
	sources.RegisterAdapter("once-timeout-source", onceTimeoutFakeAdapter{})

	scored := ScoredSource{
		Name: "once-timeout-source",
		// Type "docs" with no Search/Exec triggers the docsBypass path in
		// executeSourceOnce, so the raw query is used as the command
		// directly and no LLM client is needed for query construction.
		Source: &config.Source{Type: "docs"},
	}
	intent := &Intent{RawQuery: "anything"}
	resolved := &ResolvedContext{}

	result, err := executeSourceOnce(context.Background(), nil, scored, intent, resolved, "", "", nil, "", "", "")
	if err != nil {
		t.Fatalf("executeSourceOnce returned unexpected error: %v", err)
	}
	if result.Status != "timeout" {
		t.Errorf("Status = %q, want %q (adapter's status was flattened to generic error)", result.Status, "timeout")
	}
	if result.Summary != "Command timed out after 30s" {
		t.Errorf("Summary = %q, want the adapter's own summary preserved", result.Summary)
	}
}

// TestRetryContextBuildingLowConfidence verifies that low-confidence successful
// results produce a retry context with the confidence score and reason.
func TestRetryContextBuildingLowConfidence(t *testing.T) {
	result := sources.SourceResult{
		Source:  "test-source",
		Status:  "success",
		Summary: "x", // very short, no keyword overlap → low confidence
		Command: "SELECT 1",
	}

	intent := &Intent{
		RawQuery:    "what is the refund status for checkout ABC123?",
		RawEntities: []string{"ABC123"},
		Action:      "query",
		Keywords:    []string{"refund", "checkout"},
	}

	confidence, _, reason := computationalScore(result, intent.RawQuery, intent)

	retryContext := ""
	attempt := 0
	if confidence < lowConfidenceThreshold {
		retryContext = fmt.Sprintf(
			"RETRY (attempt %d/%d): Your prior query returned results but they seem unrelated to the question (confidence: %.2f, reason: %s). The query was:\n%s\n\nTry a more targeted query that directly addresses: %s",
			attempt+2, maxSourceRetries+1, confidence, reason, truncateLine(result.Command, 300), truncateLine(intent.RawQuery, 200),
		)
	}

	if retryContext == "" {
		t.Fatalf("expected low confidence retry, got confidence=%.2f (threshold=%.2f)", confidence, lowConfidenceThreshold)
	}
	if !containsSubstring(retryContext, "unrelated") {
		t.Error("retryContext missing 'unrelated' phrase")
	}
	if !containsSubstring(retryContext, fmt.Sprintf("%.2f", confidence)) {
		t.Errorf("retryContext missing confidence score %.2f", confidence)
	}
}

// TestRetryContextNotSetForGoodResult verifies that a successful result with
// reasonable confidence does NOT trigger a retry.
func TestRetryContextNotSetForGoodResult(t *testing.T) {
	result := sources.SourceResult{
		Source:  "test-source",
		Status:  "success",
		Summary: "The refund status for checkout ABC123 is COMPLETED. The refund was processed on 2024-01-15 for $50.00.",
		Command: "SELECT refund_status FROM refunds WHERE checkout_ari = 'ABC123'",
		Artifacts: []sources.Artifact{
			{Type: "row", ID: "1", Snippet: "refund_status=COMPLETED, amount=50.00"},
			{Type: "row", ID: "2", Snippet: "checkout_ari=ABC123, created_at=2024-01-15"},
		},
	}

	intent := &Intent{
		RawQuery:    "what is the refund status for checkout ABC123?",
		RawEntities: []string{"ABC123"},
		Action:      "query",
		Keywords:    []string{"refund", "checkout"},
	}

	confidence, _, _ := computationalScore(result, intent.RawQuery, intent)

	if confidence < lowConfidenceThreshold {
		t.Errorf("expected confidence >= %.2f for a good result, got %.2f", lowConfidenceThreshold, confidence)
	}
}

// TestComputationalScoreThresholds tests that computationalScore returns
// values below lowConfidenceThreshold for truly bad results and above for
// results that actually answer the question.
func TestComputationalScoreThresholds(t *testing.T) {
	intent := &Intent{
		RawQuery:    "what was the latest ACH deposit for umbrella?",
		RawEntities: []string{"umbrella"},
		Action:      "query",
		Keywords:    []string{"ACH", "deposit", "umbrella"},
	}

	tests := []struct {
		name        string
		result      sources.SourceResult
		expectAbove float64
		expectBelow float64
		desc        string
	}{
		{
			name: "error result",
			result: sources.SourceResult{
				Status:  "error",
				Summary: "connection refused",
			},
			expectAbove: -0.1,
			expectBelow: lowConfidenceThreshold,
			desc:        "error should be below threshold",
		},
		{
			name: "empty result",
			result: sources.SourceResult{
				Status: "empty",
			},
			expectAbove: -0.1,
			expectBelow: lowConfidenceThreshold,
			desc:        "empty should be below threshold",
		},
		{
			name: "irrelevant short result",
			result: sources.SourceResult{
				Status:  "success",
				Summary: "ok",
			},
			expectAbove: -0.1,
			expectBelow: lowConfidenceThreshold,
			desc:        "very short irrelevant summary should be below threshold",
		},
		{
			name: "relevant result with artifacts",
			result: sources.SourceResult{
				Status:  "success",
				Summary: "The latest ACH deposit for umbrella was $1,234,567.89 on 2024-03-15. The deposit ID is DEP-12345.",
				Artifacts: []sources.Artifact{
					{Type: "row", ID: "1", Snippet: "umbrella, ACH, $1234567.89, 2024-03-15"},
					{Type: "row", ID: "2", Snippet: "deposit_id=DEP-12345"},
				},
			},
			expectAbove: lowConfidenceThreshold,
			expectBelow: 1.1,
			desc:        "relevant result with matching keywords and entities should be above threshold",
		},
		{
			name: "success but no artifacts and short",
			result: sources.SourceResult{
				Status:  "success",
				Summary: "done",
			},
			expectAbove: -0.1,
			expectBelow: lowConfidenceThreshold,
			desc:        "success with no artifacts and short summary should be below threshold",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			confidence, _, reason := computationalScore(tt.result, intent.RawQuery, intent)
			if confidence < tt.expectAbove {
				t.Errorf("%s: confidence %.2f < expected minimum %.2f (reason: %s)", tt.desc, confidence, tt.expectAbove, reason)
			}
			if confidence > tt.expectBelow {
				t.Errorf("%s: confidence %.2f > expected maximum %.2f (reason: %s)", tt.desc, confidence, tt.expectBelow, reason)
			}
		})
	}
}

// TestMaxSourceRetriesConstant verifies the constant is set as expected.
func TestMaxSourceRetriesConstant(t *testing.T) {
	if maxSourceRetries != 2 {
		t.Errorf("maxSourceRetries = %d, want 2", maxSourceRetries)
	}
}

// TestLowConfidenceThresholdConstant verifies the constant is set as expected.
func TestLowConfidenceThresholdConstant(t *testing.T) {
	if lowConfidenceThreshold != 0.3 {
		t.Errorf("lowConfidenceThreshold = %f, want 0.3", lowConfidenceThreshold)
	}
}

// TestTruncateLine verifies the truncation helper used in retry context building.
func TestTruncateLine(t *testing.T) {
	tests := []struct {
		input    string
		max      int
		expected string
	}{
		{"short", 100, "short"},
		{"line one\nline two", 100, "line one line two"},
		{"abcdefghij", 7, "abcd..."},
		{"", 5, ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := truncateLine(tt.input, tt.max)
			if got != tt.expected {
				t.Errorf("truncateLine(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.expected)
			}
		})
	}
}

// TestIsUnrecoverableError verifies that environment errors are detected
// so we don't waste LLM calls retrying something rephrasing can't fix.
func TestIsUnrecoverableError(t *testing.T) {
	tests := []struct {
		summary       string
		unrecoverable bool
	}{
		{"chrono CLI error: exit status 127: sh: chrono: command not found", true},
		{"sh: snow: command not found", true},
		{"bash: /usr/bin/snow: Permission denied", true},
		{"exit status 126", true},
		{"dial tcp 127.0.0.1:5432: connect: connection refused", true},
		{"open /etc/secret: no such file or directory", true},
		{"connect: network is unreachable", true},
		{"connect: no route to host", true},
		// These ARE recoverable - bad SQL, wrong table name, etc.
		{"SQL compilation error: Object 'USERS' does not exist", false},
		{"table not found: transactions", false},
		{"syntax error at position 42", false},
		{"column 'foo' not found in schema", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.summary, func(t *testing.T) {
			got := isUnrecoverableError(tt.summary)
			if got != tt.unrecoverable {
				t.Errorf("isUnrecoverableError(%q) = %v, want %v", tt.summary, got, tt.unrecoverable)
			}
		})
	}
}

// TestSanitizeGHCommand covers the belt-and-suspenders preflight that
// strips --owner from 'gh pr list' / 'gh issue list' commands (the system
// prompt forbids it but the LLM drifts). 'gh search prs --owner' is a
// legitimate use and must NOT be touched.
func TestSanitizeGHCommand(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		want       string
		wantChange bool
	}{
		{
			name:       "pr list strips trailing --owner VALUE",
			in:         "pr list --repo acmewidgets/acme_widgets --state open --owner ryanlitalien",
			want:       "pr list --repo acmewidgets/acme_widgets --state open",
			wantChange: true,
		},
		{
			name:       "pr list strips multiple --owner occurrences",
			in:         "pr list --repo acmewidgets/acme_widgets --state open --owner ryanlitalien --owner ryanlitalien-apps",
			want:       "pr list --repo acmewidgets/acme_widgets --state open",
			wantChange: true,
		},
		{
			name:       "pr list strips --owner=VALUE form",
			in:         "pr list --repo x/y --owner=ryanlitalien --state open",
			want:       "pr list --repo x/y --state open",
			wantChange: true,
		},
		{
			name:       "issue list also sanitized",
			in:         "issue list --repo x/y --owner foo --state open",
			want:       "issue list --repo x/y --state open",
			wantChange: true,
		},
		{
			name:       "search prs --owner is LEGITIMATE, untouched",
			in:         "search prs --owner ryanlitalien --state open --json repository,number,title",
			want:       "search prs --owner ryanlitalien --state open --json repository,number,title",
			wantChange: false,
		},
		{
			name:       "search issues --owner is LEGITIMATE, untouched",
			in:         "search issues --owner acmewidgets --state open",
			want:       "search issues --owner acmewidgets --state open",
			wantChange: false,
		},
		{
			name:       "pr list without --owner is a no-op",
			in:         "pr list --repo x/y --state open",
			want:       "pr list --repo x/y --state open",
			wantChange: false,
		},
		{
			name:       "non-gh command untouched",
			in:         "SELECT * FROM users LIMIT 10",
			want:       "SELECT * FROM users LIMIT 10",
			wantChange: false,
		},
		{
			name:       "pr view untouched (not a list subcommand)",
			in:         "pr view 123 --repo x/y",
			want:       "pr view 123 --repo x/y",
			wantChange: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, note := sanitizeGHCommand(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeGHCommand(%q) cmd = %q, want %q", tc.in, got, tc.want)
			}
			if (note != "") != tc.wantChange {
				t.Errorf("sanitizeGHCommand(%q) changed=%v, want %v (note=%q)", tc.in, note != "", tc.wantChange, note)
			}
		})
	}
}

// TestRefusalResult verifies that a refusal command (as LLM Call #2
// occasionally emits instead of a real query) is turned into an "error"
// status with no artifacts and a summary quoting the refusal, so the
// executeSource retry loop treats it like any other failed attempt rather
// than executing the prose as a command. Negative cases confirm real
// commands pass through untouched.
func TestRefusalResult(t *testing.T) {
	tests := []struct {
		name    string
		command string
		refused bool
	}{
		{
			name:    "gworkspace incident text",
			command: `I cannot build a query for the "gworkspace" tool to answer "what time is it".`,
			refused: true,
		},
		{
			name:    "unable to variant",
			command: "I'm unable to construct a query for that request.",
			refused: true,
		},
		{
			name:    "sql query untouched",
			command: "SELECT * FROM charges WHERE created > '2026-01-01'",
			refused: false,
		},
		{
			name:    "gh command untouched",
			command: "gh pr list --state open",
			refused: false,
		},
		{
			name:    "current time builtin untouched",
			command: "current time ET New York",
			refused: false,
		},
		// Question-echo regression guard: generated web-search queries
		// that repeat the user's own phrasing contain "i can't" /
		// "can't i" at the head of the string but are NOT refusals.
		// The command path uses the narrow verb-anchored marker list
		// so these execute normally instead of erroring out twice and
		// failing the whole question.
		{
			name:    "question-echo search query untouched",
			command: "why i can't focus in the mornings",
			refused: false,
		},
		{
			name:    "question-echo vpn query untouched",
			command: "why can't i connect to the vpn",
			refused: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, refused := refusalResult("test-source", tt.command)
			if refused != tt.refused {
				t.Fatalf("refusalResult(%q) refused = %v, want %v", tt.command, refused, tt.refused)
			}
			if !tt.refused {
				return
			}
			if result.Status != "error" {
				t.Errorf("refusalResult(%q) Status = %q, want %q", tt.command, result.Status, "error")
			}
			if len(result.Artifacts) != 0 {
				t.Errorf("refusalResult(%q) Artifacts = %v, want none", tt.command, result.Artifacts)
			}
			if !containsSubstring(result.Summary, "query generation refused") {
				t.Errorf("refusalResult(%q) Summary = %q, missing refusal prefix", tt.command, result.Summary)
			}
			if result.Source != "test-source" {
				t.Errorf("refusalResult(%q) Source = %q, want %q", tt.command, result.Source, "test-source")
			}
		})
	}
}

// containsSubstring is a test helper.
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsIdx(s, substr))
}

func containsIdx(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
