package brain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShouldAutoCompile_NoMetaFile(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	b := &Brain{
		Path: dir,
		DB:   db,
	}

	// Ensure meta directory exists
	os.MkdirAll(filepath.Join(dir, "meta"), 0755)

	// No meta file, no lessons - should not compile
	if b.ShouldAutoCompile() {
		t.Error("expected ShouldAutoCompile=false with 0 lessons and no meta file")
	}

	// Add lessons below threshold
	for i := 0; i < autoCompileThreshold-1; i++ {
		db.InsertLesson(&LessonRecord{
			ID:        "test-" + time.Now().Add(time.Duration(i)*time.Second).Format("20060102150405"),
			Timestamp: time.Now().Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339),
			Profile:   "test",
			Question:  "question " + string(rune('a'+i)),
		})
	}
	if b.ShouldAutoCompile() {
		t.Errorf("expected ShouldAutoCompile=false with %d lessons (threshold=%d)", autoCompileThreshold-1, autoCompileThreshold)
	}

	// Add one more lesson to reach threshold
	db.InsertLesson(&LessonRecord{
		ID:        "test-threshold",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Profile:   "test",
		Question:  "threshold question",
	})
	if !b.ShouldAutoCompile() {
		t.Errorf("expected ShouldAutoCompile=true with %d lessons (threshold=%d)", autoCompileThreshold, autoCompileThreshold)
	}
}

func TestShouldAutoCompile_WithRecentCompile(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	b := &Brain{
		Path: dir,
		DB:   db,
	}

	// Ensure meta directory exists
	os.MkdirAll(filepath.Join(dir, "meta"), 0755)

	// Insert some lessons BEFORE the compile timestamp
	pastTime := time.Now().Add(-1 * time.Hour).UTC()
	for i := 0; i < 15; i++ {
		ts := pastTime.Add(time.Duration(i) * time.Second)
		db.InsertLesson(&LessonRecord{
			ID:        "old-" + ts.Format("20060102150405"),
			Timestamp: ts.Format(time.RFC3339),
			Profile:   "test",
			Question:  "old question",
		})
	}

	// Write a meta file with a compile timestamp AFTER those lessons
	compileTime := time.Now().Add(-30 * time.Minute).UTC()
	meta := autoCompileMeta{
		LastCompile: compileTime.Format(time.RFC3339),
		LessonCount: 15,
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(filepath.Join(dir, "meta", "last_compile.json"), data, 0644)

	// No new lessons since compile - should not compile
	if b.ShouldAutoCompile() {
		t.Error("expected ShouldAutoCompile=false immediately after compile")
	}

	// Add lessons AFTER the compile timestamp, but below threshold
	for i := 0; i < autoCompileThreshold-1; i++ {
		ts := time.Now().Add(time.Duration(i) * time.Second).UTC()
		db.InsertLesson(&LessonRecord{
			ID:        "new-" + ts.Format("20060102150405"),
			Timestamp: ts.Format(time.RFC3339),
			Profile:   "test",
			Question:  "new question",
		})
	}
	if b.ShouldAutoCompile() {
		t.Errorf("expected ShouldAutoCompile=false with %d new lessons", autoCompileThreshold-1)
	}

	// Add one more to hit threshold
	db.InsertLesson(&LessonRecord{
		ID:        "new-threshold",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Profile:   "test",
		Question:  "threshold question",
	})
	if !b.ShouldAutoCompile() {
		t.Error("expected ShouldAutoCompile=true after threshold new lessons since last compile")
	}
}

func TestMarkCompiled(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	b := &Brain{
		Path: dir,
		DB:   db,
	}
	os.MkdirAll(filepath.Join(dir, "meta"), 0755)

	// Add enough lessons to trigger compile, all in the past
	totalLessons := autoCompileThreshold + 5
	pastBase := time.Now().Add(-1 * time.Hour).UTC()
	for i := 0; i < totalLessons; i++ {
		ts := pastBase.Add(time.Duration(i) * time.Second)
		db.InsertLesson(&LessonRecord{
			ID:        "test-" + ts.Format("20060102150405"),
			Timestamp: ts.Format(time.RFC3339),
			Profile:   "test",
			Question:  "question",
		})
	}

	if !b.ShouldAutoCompile() {
		t.Fatal("expected ShouldAutoCompile=true before MarkCompiled")
	}

	// Mark as compiled
	if err := b.MarkCompiled(); err != nil {
		t.Fatalf("MarkCompiled failed: %v", err)
	}

	// Should no longer need compile (all lessons are before compile timestamp)
	if b.ShouldAutoCompile() {
		t.Error("expected ShouldAutoCompile=false after MarkCompiled")
	}

	// Verify the meta file was written
	metaPath := filepath.Join(dir, "meta", "last_compile.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("meta file not found: %v", err)
	}
	var meta autoCompileMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("meta file parse error: %v", err)
	}
	if meta.LessonCount != totalLessons {
		t.Errorf("expected lesson count %d, got %d", totalLessons, meta.LessonCount)
	}
	if meta.LastCompile == "" {
		t.Error("expected non-empty LastCompile timestamp")
	}
}

func TestLessonCountSince(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	// Insert lessons at different times
	past := time.Now().Add(-2 * time.Hour).UTC()
	recent := time.Now().Add(-10 * time.Minute).UTC()
	cutoff := time.Now().Add(-1 * time.Hour).UTC()

	db.InsertLesson(&LessonRecord{
		ID: "old-1", Timestamp: past.Format(time.RFC3339), Profile: "test", Question: "old",
	})
	db.InsertLesson(&LessonRecord{
		ID: "new-1", Timestamp: recent.Format(time.RFC3339), Profile: "test", Question: "new1",
	})
	db.InsertLesson(&LessonRecord{
		ID: "new-2", Timestamp: recent.Add(time.Minute).Format(time.RFC3339), Profile: "test", Question: "new2",
	})

	count := db.LessonCountSince(cutoff.Format(time.RFC3339))
	if count != 2 {
		t.Errorf("expected 2 lessons since cutoff, got %d", count)
	}

	// All lessons should be counted when using an old cutoff
	count = db.LessonCountSince(past.Add(-time.Hour).Format(time.RFC3339))
	if count != 3 {
		t.Errorf("expected 3 lessons since very old cutoff, got %d", count)
	}
}
