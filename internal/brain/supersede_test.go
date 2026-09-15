package brain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ryanlitalien/aida/internal/lessons"
)

// TestSupersedeLessonsByRunID_RetractsAutoLessonForSameRun reproduces the
// real incident that motivated Fix 4: an auto-quality reviewer scores a
// wrong answer 4/5 and writes a reinforcing routing_hint lesson, then the
// user thumbs-downs the same run a minute later. Before this fix, both
// lessons persisted and competed in recall forever because neither record
// carried the run id. After RecordLesson writes the thumbs-down lesson,
// SupersedeLessonsByRunID must retract the earlier auto lesson: its db row
// gets superseded=1/quality=0/routing_hint="", its JSON file is rewritten
// to match (preserving every other field), and it drops out of recall -
// while the new thumbs-down lesson (excepted by id) is untouched.
func TestSupersedeLessonsByRunID_RetractsAutoLessonForSameRun(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir, "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()

	ctx := context.Background()
	const runID = "run-incident-1"

	// 1. Auto-recorded lesson: quality 4, routing_hint written (the
	// wrong-but-confident answer).
	auto := &lessons.Lesson{
		Timestamp:     "2026-05-04T10:00:00Z",
		RunID:         runID,
		Question:      "what is the gmv for merchant x",
		Sources:       []string{"snowflake"},
		Quality:       4,
		QualityReason: "looked confident",
		RoutingHint:   "route to [snowflake], effective for query queries about gmv",
	}
	if err := b.RecordLesson(ctx, auto); err != nil {
		t.Fatalf("RecordLesson auto: %v", err)
	}
	autoID := LessonID(auto.Timestamp, auto.Question)

	// 2. One minute later: explicit thumbs-down for the same run.
	feedback := &lessons.Lesson{
		Timestamp:      "2026-05-04T10:01:00Z",
		RunID:          runID,
		Question:       "what is the gmv for merchant x",
		Sources:        []string{"snowflake"},
		Feedback:       lessons.FeedbackThumbsDown,
		FeedbackReason: "wrong number, should have used chronosphere",
	}
	if err := b.RecordLesson(ctx, feedback); err != nil {
		t.Fatalf("RecordLesson feedback: %v", err)
	}
	feedbackID := LessonID(feedback.Timestamp, feedback.Question)

	// 3. Supersede: exclude the feedback lesson itself.
	n, err := b.SupersedeLessonsByRunID(ctx, runID, feedbackID)
	if err != nil {
		t.Fatalf("SupersedeLessonsByRunID: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 lesson superseded, got %d", n)
	}

	// 4. DB row for the auto lesson is flagged, zeroed, cleared.
	var superseded, quality int
	var routingHint string
	if err := b.DB.conn.QueryRow(
		`SELECT superseded, quality, routing_hint FROM lessons WHERE id = ?`, autoID,
	).Scan(&superseded, &quality, &routingHint); err != nil {
		t.Fatalf("query auto lesson row: %v", err)
	}
	if superseded != 1 {
		t.Errorf("expected superseded=1, got %d", superseded)
	}
	if quality != 0 {
		t.Errorf("expected quality=0, got %d", quality)
	}
	if routingHint != "" {
		t.Errorf("expected routing_hint cleared, got %q", routingHint)
	}

	// 5. Feedback lesson's own row must be untouched.
	var fbSuperseded, fbQuality int
	if err := b.DB.conn.QueryRow(
		`SELECT superseded, quality FROM lessons WHERE id = ?`, feedbackID,
	).Scan(&fbSuperseded, &fbQuality); err != nil {
		t.Fatalf("query feedback lesson row: %v", err)
	}
	if fbSuperseded != 0 {
		t.Errorf("feedback lesson should not be superseded, got %d", fbSuperseded)
	}

	// 6. JSON file rewritten to match, preserving other fields (Sources,
	// QualityReason, Question, RunID).
	path := filepath.Join(dir, "lessons", "test", autoID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lesson file: %v", err)
	}
	var record LessonRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("unmarshal lesson file: %v", err)
	}
	if !record.Superseded {
		t.Errorf("expected file Superseded=true")
	}
	if record.Quality != 0 {
		t.Errorf("expected file Quality=0, got %d", record.Quality)
	}
	if record.RoutingHint != "" {
		t.Errorf("expected file RoutingHint cleared, got %q", record.RoutingHint)
	}
	if record.QualityReason != "looked confident" {
		t.Errorf("expected QualityReason preserved, got %q", record.QualityReason)
	}
	if len(record.Sources) != 1 || record.Sources[0] != "snowflake" {
		t.Errorf("expected Sources preserved, got %v", record.Sources)
	}
	if record.RunID != runID {
		t.Errorf("expected RunID preserved, got %q", record.RunID)
	}

	// 7. Recall exclusion: RecordLesson didn't embed either lesson (no
	// Voyage key in this test env), so backfill a matching embedding on
	// both rows directly and confirm FindSimilar surfaces only the
	// surviving thumbs-down lesson, never the superseded auto lesson.
	emb := make([]float32, 512)
	emb[0] = 1.0
	blob := EncodeVector(emb)
	if _, err := b.DB.conn.Exec(`UPDATE lessons SET question_embedding = ? WHERE id IN (?, ?)`, blob, autoID, feedbackID); err != nil {
		t.Fatalf("backfill embeddings: %v", err)
	}

	results, err := b.DB.FindSimilar(emb, 5, "")
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result after supersede, got %d", len(results))
	}
	if results[0].Lesson.ID != feedbackID {
		t.Errorf("expected surviving lesson %s, got %s", feedbackID, results[0].Lesson.ID)
	}
}

// TestRetractLesson_MarksSupersededAndErrorsOnUnknownID covers the
// `aida brain lesson retract <id>` path: a manual retraction independent
// of run-based auto-supersession. It must flip the same superseded /
// quality / routing_hint fields (db row + JSON file) as
// SupersedeLessonsByRunID, and return a clear error for an id that
// doesn't exist rather than silently succeeding.
func TestRetractLesson_MarksSupersededAndErrorsOnUnknownID(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir, "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()

	ctx := context.Background()

	lesson := &lessons.Lesson{
		Timestamp:   "2026-05-04T11:00:00Z",
		Question:    "stray bad lesson with no known run id",
		Sources:     []string{"github"},
		Quality:     5,
		RoutingHint: "route to [github], effective for query queries about prs",
	}
	if err := b.RecordLesson(ctx, lesson); err != nil {
		t.Fatalf("RecordLesson: %v", err)
	}
	id := LessonID(lesson.Timestamp, lesson.Question)

	if err := b.RetractLesson(ctx, id); err != nil {
		t.Fatalf("RetractLesson: %v", err)
	}

	var superseded, quality int
	var routingHint string
	if err := b.DB.conn.QueryRow(
		`SELECT superseded, quality, routing_hint FROM lessons WHERE id = ?`, id,
	).Scan(&superseded, &quality, &routingHint); err != nil {
		t.Fatalf("query retracted lesson row: %v", err)
	}
	if superseded != 1 || quality != 0 || routingHint != "" {
		t.Errorf("expected superseded=1/quality=0/routing_hint='', got superseded=%d quality=%d routing_hint=%q",
			superseded, quality, routingHint)
	}

	path := filepath.Join(dir, "lessons", "test", id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lesson file: %v", err)
	}
	var record LessonRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("unmarshal lesson file: %v", err)
	}
	if !record.Superseded {
		t.Errorf("expected file Superseded=true")
	}
	if len(record.Sources) != 1 || record.Sources[0] != "github" {
		t.Errorf("expected Sources preserved, got %v", record.Sources)
	}

	// Nonexistent id: clear error, no panic.
	if err := b.RetractLesson(ctx, "does-not-exist"); err == nil {
		t.Errorf("expected error retracting unknown lesson id, got nil")
	}
}
