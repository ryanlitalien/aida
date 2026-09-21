package brain

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDB_CreateAndMigrate(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	// Verify the file was created
	if _, err := os.Stat(filepath.Join(dir, "brain.db")); err != nil {
		t.Fatalf("brain.db not created: %v", err)
	}
}

func TestInsertAndFindSimilar(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	// Insert a lesson with a known embedding
	emb := make([]float32, 512)
	emb[0] = 1.0 // unit vector in first dimension
	err = db.InsertLesson(&LessonRecord{
		ID:              "test-1",
		Timestamp:       "2026-04-11T10:00:00Z",
		Profile:         "test",
		Question:        "test question",
		Embedding:       emb,
		Sources:         []string{"source-a"},
		PerSourceStatus: map[string]string{"source-a": "success"},
	})
	if err != nil {
		t.Fatalf("InsertLesson failed: %v", err)
	}

	// Search with the same embedding - should find it
	results, err := db.FindSimilar(emb, 5, "")
	if err != nil {
		t.Fatalf("FindSimilar failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Lesson.ID != "test-1" {
		t.Errorf("expected ID test-1, got %s", results[0].Lesson.ID)
	}
	if results[0].Similarity < 0.99 {
		t.Errorf("expected similarity ~1.0, got %f", results[0].Similarity)
	}

	// Search with orthogonal embedding - should not find it
	ortho := make([]float32, 512)
	ortho[1] = 1.0
	results, err = db.FindSimilar(ortho, 5, "")
	if err != nil {
		t.Fatalf("FindSimilar failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for orthogonal query, got %d", len(results))
	}
}

func TestUpsertAndFindEntities(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	err = db.UpsertEntity(&EntityRecord{
		Slug:    "thrive",
		Type:    "partner",
		Name:    "Thrive",
		Aliases: []string{"thrive-game", "thrv"},
	})
	if err != nil {
		t.Fatalf("UpsertEntity failed: %v", err)
	}

	// Find by name
	results, err := db.FindEntities([]string{"Thrive"})
	if err != nil {
		t.Fatalf("FindEntities failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(results))
	}

	// Find by alias
	results, err = db.FindEntities([]string{"thrive-game"})
	if err != nil {
		t.Fatalf("FindEntities failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entity by alias, got %d", len(results))
	}

	// Find by normalized alias
	results, err = db.FindEntities([]string{"THRV"})
	if err != nil {
		t.Fatalf("FindEntities failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entity by normalized alias, got %d", len(results))
	}

	// No match
	results, err = db.FindEntities([]string{"nonexistent"})
	if err != nil {
		t.Fatalf("FindEntities failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 entities, got %d", len(results))
	}
}

// TestListTasks_ProfileIsolation guards against the cross-profile leak that
// surfaced when "what should I work on next?" on the home machine listed
// every profile:work task. The DB layer must never return foreign-profile
// rows when an isolation key is supplied - even if a buggy CLI caller
// constructs the wrong tag filter.
func TestListTasks_ProfileIsolation(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	mustUpsert := func(slug, profile string, extraTags ...string) {
		t.Helper()
		tags := append([]string{}, extraTags...)
		if profile != "" {
			tags = append(tags, "profile:"+profile)
		}
		if err := db.UpsertTask(&TaskRecord{
			Slug:    slug,
			Title:   slug,
			Status:  "open",
			Tags:    tags,
			Created: "2026-04-24T00:00:00Z",
		}); err != nil {
			t.Fatalf("UpsertTask %s: %v", slug, err)
		}
	}

	mustUpsert("work-only", "work", "p1")
	mustUpsert("home-only", "home", "p2")
	mustUpsert("untagged", "", "p3")

	// Active profile = home: should see home + untagged, never work.
	got, err := db.ListTasks(false, nil, 0, 0, "home", nil)
	if err != nil {
		t.Fatalf("ListTasks home: %v", err)
	}
	slugs := taskSlugs(got)
	if hasSlug(slugs, "work-only") {
		t.Errorf("home profile leaked work-only task: %v", slugs)
	}
	if !hasSlug(slugs, "home-only") || !hasSlug(slugs, "untagged") {
		t.Errorf("home profile missing expected tasks: %v", slugs)
	}

	// Active profile = work: should see work + untagged, never home.
	got, err = db.ListTasks(false, nil, 0, 0, "work", nil)
	if err != nil {
		t.Fatalf("ListTasks work: %v", err)
	}
	slugs = taskSlugs(got)
	if hasSlug(slugs, "home-only") {
		t.Errorf("work profile leaked home-only task: %v", slugs)
	}
	if !hasSlug(slugs, "work-only") || !hasSlug(slugs, "untagged") {
		t.Errorf("work profile missing expected tasks: %v", slugs)
	}

	// Empty isolation: see everything (the --all-profiles escape hatch).
	got, err = db.ListTasks(false, nil, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("ListTasks all: %v", err)
	}
	slugs = taskSlugs(got)
	if len(slugs) != 3 {
		t.Errorf("all-profiles should return 3 tasks, got %d: %v", len(slugs), slugs)
	}
}

// TestListTasks_ProfileIsolationOverridesTagFilter - the original bug: a
// caller passes tag=profile:work while active profile is home. Isolation
// must win and return zero rows rather than the work tasks the tag filter
// would otherwise admit.
func TestListTasks_ProfileIsolationOverridesTagFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	if err := db.UpsertTask(&TaskRecord{
		Slug: "work-task", Title: "work-task", Status: "open",
		Tags: []string{"profile:work"}, Created: "2026-04-24T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	got, err := db.ListTasks(false, []string{"profile:work"}, 0, 0, "home", nil)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("home profile leaked work tasks despite isolation: %v", taskSlugs(got))
	}
}

func taskSlugs(tasks []TaskRecord) []string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.Slug
	}
	return out
}

func hasSlug(slugs []string, want string) bool {
	for _, s := range slugs {
		if s == want {
			return true
		}
	}
	return false
}

func TestLessonCount(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	if db.LessonCount() != 0 {
		t.Errorf("expected 0 lessons, got %d", db.LessonCount())
	}

	db.InsertLesson(&LessonRecord{
		ID: "test-1", Timestamp: "2026-04-11T10:00:00Z", Profile: "test", Question: "q1",
	})
	db.InsertLesson(&LessonRecord{
		ID: "test-2", Timestamp: "2026-04-11T10:01:00Z", Profile: "test", Question: "q2",
	})

	if db.LessonCount() != 2 {
		t.Errorf("expected 2 lessons, got %d", db.LessonCount())
	}
}

// TestInsertLesson_RunIDAndSuperseded covers the Fix 4 recall-filtering
// contract: a superseded lesson round-trips run_id/routing_hint through
// InsertLesson but is excluded from FindSimilar once flagged, while the
// lesson that superseded it (same run_id) remains visible.
func TestInsertLesson_RunIDAndSuperseded(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	emb := make([]float32, 512)
	emb[0] = 1.0

	if err := db.InsertLesson(&LessonRecord{
		ID:          "run-a-auto",
		Timestamp:   "2026-04-11T10:00:00Z",
		Profile:     "test",
		Question:    "why did the checkout fail",
		Embedding:   emb,
		RunID:       "run-a",
		Quality:     4,
		RoutingHint: "route to [sqlite], effective for query queries about checkout",
	}); err != nil {
		t.Fatalf("InsertLesson auto: %v", err)
	}
	if err := db.InsertLesson(&LessonRecord{
		ID:        "run-a-feedback",
		Timestamp: "2026-04-11T10:01:00Z",
		Profile:   "test",
		Question:  "why did the checkout fail",
		Embedding: emb,
		RunID:     "run-a",
		Feedback:  "thumbs-down",
	}); err != nil {
		t.Fatalf("InsertLesson feedback: %v", err)
	}

	// Both visible before supersede.
	results, err := db.FindSimilar(emb, 5, "")
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results before supersede, got %d", len(results))
	}

	// Directly flag the auto lesson as superseded - mirrors what
	// SupersedeLessonsByRunID does in the feedback loop.
	if _, err := db.conn.Exec(
		`UPDATE lessons SET superseded = 1, quality = 0, routing_hint = '' WHERE id = ?`,
		"run-a-auto",
	); err != nil {
		t.Fatalf("supersede update: %v", err)
	}

	results, err = db.FindSimilar(emb, 5, "")
	if err != nil {
		t.Fatalf("FindSimilar after supersede: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result after supersede, got %d", len(results))
	}
	if results[0].Lesson.ID != "run-a-feedback" {
		t.Errorf("expected surviving lesson run-a-feedback, got %s", results[0].Lesson.ID)
	}
	if results[0].Lesson.RunID != "run-a" {
		t.Errorf("expected RunID to round-trip, got %q", results[0].Lesson.RunID)
	}
}

// TestMigration_AddsRunIDSupersededRoutingHintColumns checks the Fix 4
// migration columns exist after a fresh OpenDB, and that re-running
// migrate() against an already-migrated db (the idempotence case: a second
// aida process opening the same brain.db) does not error.
func TestMigration_AddsRunIDSupersededRoutingHintColumns(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}

	for _, col := range []string{"run_id", "superseded", "routing_hint"} {
		var count int
		db.conn.QueryRow("SELECT COUNT(*) FROM pragma_table_info('lessons') WHERE name=?", col).Scan(&count)
		if count != 1 {
			t.Errorf("expected column %q to exist after OpenDB, got count=%d", col, count)
		}
	}

	// Idempotence: re-running migrate() on an already-migrated db (fresh
	// AND populated) must not error.
	if err := db.migrate(); err != nil {
		t.Fatalf("re-running migrate on fresh db: %v", err)
	}
	db.Close()

	db2, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("re-OpenDB on existing (already-migrated) db: %v", err)
	}
	defer db2.Close()
}

func TestMigration_AddsCompletedAtColumn(t *testing.T) {
	dir := t.TempDir()

	// Open once to create the schema with completed_at present.
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}

	// Drop the column manually to simulate a pre-migration DB. SQLite
	// supports DROP COLUMN as of 3.35; modernc.org/sqlite is well past that.
	if _, err := db.conn.Exec("ALTER TABLE tasks DROP COLUMN completed_at"); err != nil {
		t.Fatalf("simulating pre-migration DROP COLUMN: %v", err)
	}

	// Confirm it's gone.
	var hasCompletedAt int
	db.conn.QueryRow("SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name='completed_at'").Scan(&hasCompletedAt)
	if hasCompletedAt != 0 {
		t.Fatalf("setup: completed_at still present after DROP")
	}

	// Re-run migrate; the idempotent ALTER should re-add the column.
	if err := db.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	db.conn.QueryRow("SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name='completed_at'").Scan(&hasCompletedAt)
	if hasCompletedAt != 1 {
		t.Errorf("migrate did not re-add completed_at column")
	}

	db.Close()
}

func TestListEntitySlugs(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	empty, err := db.ListEntitySlugs()
	if err != nil {
		t.Fatalf("ListEntitySlugs (empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected no slugs before any entity is upserted, got %v", empty)
	}

	if err := db.UpsertEntity(&EntityRecord{Slug: "acme-widgets", Type: "tools", Name: "Acme Widgets"}); err != nil {
		t.Fatalf("UpsertEntity: %v", err)
	}
	if err := db.UpsertEntity(&EntityRecord{Slug: "ralph", Type: "people", Name: "Ralph"}); err != nil {
		t.Fatalf("UpsertEntity: %v", err)
	}

	slugs, err := db.ListEntitySlugs()
	if err != nil {
		t.Fatalf("ListEntitySlugs: %v", err)
	}
	want := map[string]bool{"acme-widgets": true, "ralph": true}
	if len(slugs) != len(want) {
		t.Fatalf("ListEntitySlugs = %v, want %v", slugs, want)
	}
	for _, s := range slugs {
		if !want[s] {
			t.Errorf("unexpected slug %q", s)
		}
	}
}
