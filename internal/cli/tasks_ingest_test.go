package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/jobs"
)

func TestIngestSourceHash_StableAndUnique(t *testing.T) {
	a := ingestSourceHash("notion:abc", "hello world")
	b := ingestSourceHash("notion:abc", "hello world")
	if a != b {
		t.Errorf("same input should produce same hash: %q vs %q", a, b)
	}
	if len(a) != 12 { // 6 bytes hex
		t.Errorf("hash length = %d, want 12 hex chars", len(a))
	}
	c := ingestSourceHash("notion:abc", "hello world!")
	if a == c {
		t.Errorf("differing prose should produce different hash")
	}
	d := ingestSourceHash("notion:def", "hello world")
	if a == d {
		t.Errorf("differing source ref should produce different hash")
	}
}

func TestExcerptForAutoSolve_ShortProsePassesThrough(t *testing.T) {
	prose := "short content"
	got := excerptForAutoSolve(prose, "mem-abc")
	if got != prose {
		t.Errorf("got %q, want passthrough %q", got, prose)
	}
}

func TestExcerptForAutoSolve_LongProseTruncatesWithPointer(t *testing.T) {
	prose := strings.Repeat("a", sourceContextLimit+500)
	got := excerptForAutoSolve(prose, "mem-xyz")
	if len(got) >= len(prose) {
		t.Errorf("expected truncation, got len=%d (input=%d)", len(got), len(prose))
	}
	if !strings.Contains(got, "[truncated") {
		t.Errorf("expected truncation marker, got tail: ...%q", got[max(0, len(got)-80):])
	}
	if !strings.Contains(got, "mem-xyz") {
		t.Errorf("expected memory event id in truncation pointer, got: %s", got[max(0, len(got)-200):])
	}
}

func TestExcerptForAutoSolve_PrefersLineBoundaryWhenClose(t *testing.T) {
	// Build prose where total length > cap, with a newline within
	// 300 chars of the cap so the truncator backs up to it instead
	// of cutting mid-line. Filler past the cap forces truncation.
	head := strings.Repeat("x", sourceContextLimit-150)
	prose := head + "\nMORE_CONTENT_AFTER_BOUNDARY" + strings.Repeat("y", 200)
	got := excerptForAutoSolve(prose, "")
	if !strings.Contains(got, "[truncated]") {
		t.Fatalf("expected truncation, got len=%d (input=%d): %q", len(got), len(prose), got[max(0, len(got)-80):])
	}
	if strings.Contains(got, "MORE_CONTENT_AFTER_BOUNDARY") {
		t.Errorf("should have backed up to line boundary instead of cutting mid-line:\n%s", got[max(0, len(got)-100):])
	}
}

func TestExcerptForAutoSolve_EmptyInputEmptyOutput(t *testing.T) {
	if got := excerptForAutoSolve("", "mem-x"); got != "" {
		t.Errorf("empty input should yield empty output, got %q", got)
	}
	if got := excerptForAutoSolve("   \n\t  ", ""); got != "" {
		t.Errorf("whitespace input should yield empty output, got %q", got)
	}
}

func TestBuildAutoSolveQuestion_WithContext(t *testing.T) {
	q := buildAutoSolveQuestion("notion:abc", "Decision: ship X", "Ship X", "Verify and ship X by Friday")
	for _, want := range []string{"Investigate as an agent", "Context from notion:abc", "Decision: ship X", "Action item:", "Verify and ship X by Friday"} {
		if !strings.Contains(q, want) {
			t.Errorf("missing %q in:\n%s", want, q)
		}
	}
}

func TestBuildAutoSolveQuestion_AvoidsTriggerWord(t *testing.T) {
	// Regression: an earlier version used "Task:\n" which caused
	// aida's parser to classify these prompts as action=task and
	// short-circuit into HandleTaskIntent instead of running the
	// agent loop. Make sure the trigger word is gone.
	q := buildAutoSolveQuestion("notion:abc", "ctx", "Some title", "Some body")
	if strings.Contains(q, "Task:") {
		t.Errorf("'Task:' header still present (would trip task-intent classifier):\n%s", q)
	}
}

func TestBuildAutoSolveQuestion_BodyOnlyWhenContextEmpty(t *testing.T) {
	q := buildAutoSolveQuestion("notion:abc", "", "Title", "Body content")
	if !strings.Contains(q, "Body content") {
		t.Errorf("body lost: %q", q)
	}
	// Even body-only mode should include the "investigate as an
	// agent" framing so the parser routes to investigation, not task.
	if !strings.Contains(q, "Investigate as an agent") {
		t.Errorf("expected investigate framing in body-only mode, got %q", q)
	}
}

func TestBuildAutoSolveQuestion_TitleFallbackWhenBodyEmpty(t *testing.T) {
	q := buildAutoSolveQuestion("", "", "Just the title", "")
	if !strings.Contains(q, "Just the title") {
		t.Errorf("title fallback missing: %q", q)
	}
}

func TestBuildAutoSolveQuestion_TitleFallbackWithContext(t *testing.T) {
	q := buildAutoSolveQuestion("notion:abc", "ctx", "Title only", "")
	if !strings.Contains(q, "Context from notion:abc") {
		t.Errorf("expected context block, got %q", q)
	}
	if !strings.Contains(q, "Title only") {
		t.Errorf("expected title fallback in body slot, got %q", q)
	}
}

// TestStripANSI tests removed: the function and its supporting regexes
// were deleted in this commit. The new --run-dir mode writes ANSI-free
// output.md by construction, so the read-side strip is no longer
// needed. End-to-end coverage now lives in agent_events_test.go
// (TestRunDirSinkCompleteAndOutput asserts no ANSI bytes in output.md).

func TestSlugifyForPlan_StripsPathUnsafeChars(t *testing.T) {
	// Regression: live acme-widgets v2 ingest produced "15 tasks
	// created, 14 plans created" because the title "Review and
	// update rake file/demo control tool" included a `/` that
	// normalizeName didn't strip - WriteExecPlan tried to create
	// a subdirectory and silently lost the plan.
	got := slugifyForPlan("Review and update rake file/demo control tool", 0)
	if strings.Contains(got, "/") {
		t.Errorf("slug still contains '/': %q", got)
	}
	if strings.Contains(got, "\\") {
		t.Errorf("slug still contains '\\\\': %q", got)
	}
	// And it should still be a valid plan id (timestamp prefix +
	// hyphen + non-empty alphanumeric tail).
	if !strings.Contains(got, "rake") {
		t.Errorf("slug lost legitimate content: %q", got)
	}
}

func TestSlugifyForPlan_SeqDisambiguates(t *testing.T) {
	a := slugifyForPlan("Identical title", 0)
	b := slugifyForPlan("Identical title", 0)
	// Same second → same id (the test runs fast). Bump seq to disambiguate.
	c := slugifyForPlan("Identical title", 1)
	d := slugifyForPlan("Identical title", 2)
	if a != b {
		t.Logf("note: same title, same call sequence produced different ids - clock ticked between (%q vs %q)", a, b)
	}
	if c == a {
		t.Errorf("seq=1 should produce different id from seq=0: both = %q", a)
	}
	if d == c {
		t.Errorf("seq=2 should produce different id from seq=1: both = %q", c)
	}
}

func TestSlugifyForPlan_EmptyTitleFallsBackToIngest(t *testing.T) {
	got := slugifyForPlan("???", 0)
	// Title normalizes to "" (only special chars); fallback to "ingest".
	if !strings.Contains(got, "ingest") {
		t.Errorf("expected ingest fallback in id, got %q", got)
	}
}

func TestSlugSafeChars(t *testing.T) {
	cases := map[string]string{
		"abc/def":     "abcdef",
		"hello-world": "helloworld", // hyphen normalizeName already strips, but double-defense
		"x:y\\z":      "xyz",
		"only-alpha":  "onlyalpha",
		"a1b2c3":      "a1b2c3",
		"":            "",
	}
	for in, want := range cases {
		if got := slugSafeChars(in); got != want {
			t.Errorf("slugSafeChars(%q) = %q, want %q", in, got, want)
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestIngestIdempotency_DetectsPriorRun verifies that an ingest of
// the same source content twice surfaces the prior tasks via the
// source-hash tag. We test the idempotency lookup directly (the
// runTasksIngest function would also need an LLM and a temp config;
// the lookup is the load-bearing piece).
func TestIngestIdempotency_DetectsPriorRun(t *testing.T) {
	// Set up a brain with a task already tagged with a source-hash.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, ".aida", "brain"), 0755); err != nil {
		t.Fatal(err)
	}
	brainPath := filepath.Join(tmp, ".aida", "brain")

	b, err := brain.Open(brainPath, "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	// Compute the same hash the ingest path would.
	srcRef := "file:/tmp/standup-2026-05-04.md"
	prose := "Decided to ship X by Friday. Aida will own."
	hash := ingestSourceHash(srcRef, prose)
	srcTag := sourceTagPrefix + hash

	if _, err := b.AddTask("Ship X by Friday", []string{srcTag, "owner:Aida"}, "Body."); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// Now look up the prior cohort the way runTasksIngest does.
	prior, err := b.DB.ListTasks(true, []string{srcTag}, 0, 5, "test", nil)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(prior) != 1 {
		t.Errorf("expected 1 prior task, got %d", len(prior))
	}
	if prior[0].Title != "Ship X by Friday" {
		t.Errorf("prior task title = %q", prior[0].Title)
	}

	// A different prose (even on the same source ref) produces a
	// different hash and finds no prior cohort.
	otherHash := ingestSourceHash(srcRef, prose+" (edited)")
	otherPrior, _ := b.DB.ListTasks(true, []string{sourceTagPrefix + otherHash}, 0, 5, "test", nil)
	if len(otherPrior) != 0 {
		t.Errorf("edited prose should not match prior cohort, got %d", len(otherPrior))
	}
}

// TestIngestIdempotency_MemoryEventForSourceProse verifies that the
// source prose, after being persisted as a typed-memory event during
// real ingest, can be retrieved by the source-hash key. This is the
// integration point: auto-solve agents pull the full source via the
// 5-channel retrieval surface keyed on this event.
func TestIngestIdempotency_MemoryEventForSourceProse(t *testing.T) {
	tmp := t.TempDir()
	b, err := brain.Open(tmp, "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	hash := ingestSourceHash("notion:abc", "long prose body")
	rec, err := b.WriteMemory(context.Background(), brain.MemoryRecord{
		Type:   brain.MemoryEvent,
		Key:    "ingest-source:" + hash,
		Body:   "long prose body",
		Tags:   []string{"ingest", sourceTagPrefix + hash, "source:notion:abc"},
		Source: "notion:abc",
	})
	if err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	if rec == nil || rec.ID == "" {
		t.Fatalf("expected memory record id")
	}

	// Verify the multi-channel surface picks it up by the hash key.
	multi, err := b.SearchMulti(context.Background(), "anything", []string{"ingest-source:" + hash}, 5)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if len(multi) == 0 {
		t.Fatalf("expected at least one result from fact-key channel")
	}
	if !strings.HasPrefix(multi[0].DocID, "memory:") {
		t.Errorf("top result should be the memory record, got DocID=%q", multi[0].DocID)
	}
	if !strings.Contains(multi[0].Body, "long prose body") {
		t.Errorf("body lost in retrieval: %q", multi[0].Body)
	}
}

// TestResolveDestinationForTags_MatchesPartnerAlias and
// TestDestinationFromConfig_* used to cover resolveDestinationForTags and
// destinationFromConfig, which resolved a task's tags to a typed delivery
// destination via the partner registry's per-partner Destination config.
// Both functions were removed with the registry (issue #58's tag->destination
// mapping had no non-partner replacement); appendDestinationInstructions
// below now always receives a nil *jobs.Destination in production until a
// registry-free mechanism exists.

func TestAppendDestinationInstructions_GitHubPR(t *testing.T) {
	q := appendDestinationInstructions(
		"Investigate as an agent...",
		"20260511-abcdef",
		&jobs.Destination{
			Type: jobs.DestinationTypeGitHubPR,
			Repo: "ryanlitalien/acme_widgets",
		},
	)
	if !strings.Contains(q, "DESTINATION: github-pr") {
		t.Errorf("missing destination header:\n%s", q)
	}
	if !strings.Contains(q, "ryanlitalien/acme_widgets") {
		t.Errorf("repo not in prompt")
	}
	if !strings.Contains(q, "Title:") {
		t.Errorf("prompt should ask for Title: shape; got:\n%s", q)
	}
	if !strings.Contains(q, "20260511-abcdef") {
		t.Errorf("run id should appear in the set-artifact-url hint")
	}
}

func TestAppendDestinationInstructions_SlackMessage(t *testing.T) {
	q := appendDestinationInstructions(
		"Investigate...",
		"run-1",
		&jobs.Destination{
			Type:    jobs.DestinationTypeSlackMessage,
			Channel: "#partners-umbrella",
		},
	)
	if !strings.Contains(q, "DESTINATION: slack-message") {
		t.Errorf("missing destination header:\n%s", q)
	}
	if !strings.Contains(q, "#partners-umbrella") {
		t.Errorf("channel hint missing")
	}
	if !strings.Contains(q, "plain text") {
		t.Errorf("plain-text guidance missing")
	}
}

func TestAppendDestinationInstructions_NilLeavesUnchanged(t *testing.T) {
	base := "Investigate as an agent and produce a draft response:\nDo X"
	got := appendDestinationInstructions(base, "run-1", nil)
	if got != base {
		t.Errorf("nil destination should leave prompt unchanged:\n%s", got)
	}
}

func TestAppendDestinationInstructions_UnknownTypeLeavesUnchanged(t *testing.T) {
	base := "Investigate..."
	got := appendDestinationInstructions(base, "run-1", &jobs.Destination{Type: "notion-page"})
	if got != base {
		t.Errorf("unknown destination type should leave prompt unchanged; got:\n%s", got)
	}
}
