package brain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestInsertAndFindSimilarJarvisLesson(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	emb := make([]float32, VectorDims)
	emb[0] = 1.0

	l := &JarvisLesson{
		ID:                      "jl-test-1",
		Timestamp:               "2026-05-24T10:00:00Z",
		Profile:                 "home",
		RatedTurnStartedAt:      "2026-05-24T09:59:50Z",
		Transcript:              "Hey Jarvis, what time is it",
		Query:                   "what time is it",
		Embedding:               emb,
		Reply:                   "It is five PM, sir.",
		ToolCalls:               []JarvisToolCall{{Name: "current_time", TookMs: 12}},
		Feedback:                "down",
		FeedbackReason:          "should have included the date",
		FeedbackStyleDirectives: []string{"include the date with the time"},
	}
	if err := db.InsertJarvisLesson(l); err != nil {
		t.Fatalf("InsertJarvisLesson: %v", err)
	}

	results, err := db.FindSimilarJarvisLessons(emb, 5, "home", 0.15)
	if err != nil {
		t.Fatalf("FindSimilarJarvisLessons: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	got := results[0].Lesson
	if got.ID != "jl-test-1" {
		t.Errorf("ID: want jl-test-1, got %s", got.ID)
	}
	if got.Feedback != "down" {
		t.Errorf("Feedback: want down, got %s", got.Feedback)
	}
	if got.FeedbackReason != "should have included the date" {
		t.Errorf("FeedbackReason mismatch: %s", got.FeedbackReason)
	}
	if len(got.FeedbackStyleDirectives) != 1 || got.FeedbackStyleDirectives[0] != "include the date with the time" {
		t.Errorf("FeedbackStyleDirectives mismatch: %+v", got.FeedbackStyleDirectives)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "current_time" {
		t.Errorf("ToolCalls mismatch: %+v", got.ToolCalls)
	}
	if results[0].Similarity < 0.99 {
		t.Errorf("Similarity ~1.0 expected, got %f", results[0].Similarity)
	}
}

func TestFindSimilarJarvisLesson_ThresholdAndK(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// Two lessons: one aligned with query, one orthogonal.
	aligned := make([]float32, VectorDims)
	aligned[0] = 1.0
	orthogonal := make([]float32, VectorDims)
	orthogonal[1] = 1.0

	if err := db.InsertJarvisLesson(&JarvisLesson{
		ID: "a", Timestamp: "2026-05-24T10:00:00Z", Profile: "home",
		RatedTurnStartedAt: "x", Transcript: "x", Query: "match", Embedding: aligned,
		Reply: "r", Feedback: "up",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertJarvisLesson(&JarvisLesson{
		ID: "b", Timestamp: "2026-05-24T10:00:01Z", Profile: "home",
		RatedTurnStartedAt: "x", Transcript: "x", Query: "miss", Embedding: orthogonal,
		Reply: "r", Feedback: "down",
	}); err != nil {
		t.Fatal(err)
	}

	results, err := db.FindSimilarJarvisLessons(aligned, 5, "home", 0.25)
	if err != nil {
		t.Fatalf("FindSimilarJarvisLessons: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (orthogonal filtered by threshold), got %d", len(results))
	}
	if results[0].Lesson.ID != "a" {
		t.Errorf("expected aligned lesson 'a', got %s", results[0].Lesson.ID)
	}
}

func TestFormatJarvisRecallProse_GroupsByFeedback(t *testing.T) {
	recalled := []SimilarJarvisLesson{
		{Lesson: JarvisLesson{
			Query: "what time is it", Reply: "It is five PM, sir.",
			Feedback: "down", FeedbackReason: "should have said the date too",
		}, Similarity: 0.9},
		{Lesson: JarvisLesson{
			Query: "what's the weather", Reply: "Fifty-five and sunny, sir.",
			Feedback: "up", FeedbackReason: "good brevity",
		}, Similarity: 0.8},
		{Lesson: JarvisLesson{
			Query: "any directive", Feedback: "note",
			FeedbackStyleDirectives: []string{"use Fahrenheit"},
		}, Similarity: 0.7},
	}
	out := FormatJarvisRecallProse(recalled)
	for _, want := range []string{
		"Past feedback on similar requests",
		"To avoid (thumbs-down)",
		"what time is it",
		"should have said the date too",
		"Approved style (thumbs-up)",
		"what's the weather",
		"good brevity",
		"Standing directives (notes)",
		"use Fahrenheit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatJarvisRecallProse_EmptyReturnsEmpty(t *testing.T) {
	if got := FormatJarvisRecallProse(nil); got != "" {
		t.Errorf("nil input should yield empty string, got %q", got)
	}
	if got := FormatJarvisRecallProse([]SimilarJarvisLesson{}); got != "" {
		t.Errorf("empty slice should yield empty string, got %q", got)
	}
}

func TestFormatJarvisRecallProse_TruncatesLongReply(t *testing.T) {
	long := strings.Repeat("x", 500)
	out := FormatJarvisRecallProse([]SimilarJarvisLesson{
		{Lesson: JarvisLesson{Query: "q", Reply: long, Feedback: "down", FeedbackReason: "r"}},
	})
	if strings.Contains(out, strings.Repeat("x", 300)) {
		t.Errorf("expected reply to be truncated below 300 chars, got len %d", len(out))
	}
	if !strings.Contains(out, "…") {
		t.Errorf("expected ellipsis on truncation, got:\n%s", out)
	}
}

func TestJarvisLessonCounts(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if got := db.JarvisLessonCount(); got != 0 {
		t.Errorf("empty count: want 0, got %d", got)
	}

	for i, fb := range []string{"up", "down", "down", "note"} {
		emb := make([]float32, VectorDims)
		emb[i] = 1.0
		if err := db.InsertJarvisLesson(&JarvisLesson{
			ID: string(rune('a' + i)), Timestamp: "2026-05-24T10:00:00Z", Profile: "home",
			RatedTurnStartedAt: "x", Transcript: "x", Query: "q", Embedding: emb,
			Reply: "r", Feedback: fb,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if got := db.JarvisLessonCount(); got != 4 {
		t.Errorf("count: want 4, got %d", got)
	}
	by, err := db.JarvisLessonCountByFeedback()
	if err != nil {
		t.Fatal(err)
	}
	if by["up"] != 1 || by["down"] != 2 || by["note"] != 1 {
		t.Errorf("by feedback: %+v", by)
	}
}

// truncateForPrompt must never split a multi-byte rune - lesson bodies carry
// °/€/… and an invalid-UTF-8 fragment would flow into the system prompt.
func TestTruncateForPromptRuneSafe(t *testing.T) {
	if got := truncateForPrompt("short", 10); got != "short" {
		t.Errorf("under-limit string changed: %q", got)
	}
	if got := truncateForPrompt("a€bcdef", 2); got != "a…" {
		t.Errorf("mid-rune cut = %q, want %q", got, "a…")
	}
	for _, n := range []int{1, 2, 3, 4, 5} {
		if got := truncateForPrompt("°°°°", n); !utf8.ValidString(got) {
			t.Errorf("truncateForPrompt(°°°°, %d) = %q is invalid UTF-8", n, got)
		}
	}
}
