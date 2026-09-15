package brain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAddTask(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	task, err := b.AddTask("Fix mobile nav", []string{"project:website", "p1"}, "")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	if task.Title != "Fix mobile nav" {
		t.Errorf("title = %q, want %q", task.Title, "Fix mobile nav")
	}
	if task.Completed {
		t.Error("new task should not be completed")
	}
	if task.Status != "open" {
		t.Errorf("status = %q, want %q", task.Status, "open")
	}

	// File should exist
	filePath := filepath.Join(dir, task.PagePath)
	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("task file not created at %s", filePath)
	}

	// Should have a stable task ID
	if task.TaskID <= 0 {
		t.Errorf("task.TaskID = %d, want > 0", task.TaskID)
	}

	// Should be in DB with the same ID
	got, err := db.GetTask(task.Slug)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Title != "Fix mobile nav" {
		t.Errorf("DB title = %q, want %q", got.Title, "Fix mobile nav")
	}
	if got.TaskID != task.TaskID {
		t.Errorf("DB TaskID = %d, want %d", got.TaskID, task.TaskID)
	}

	// Second task should get the next sequential ID
	task2, err := b.AddTask("Update docs", []string{"p2"}, "")
	if err != nil {
		t.Fatalf("AddTask 2: %v", err)
	}
	if task2.TaskID != task.TaskID+1 {
		t.Errorf("task2.TaskID = %d, want %d", task2.TaskID, task.TaskID+1)
	}
}

// TestAddTask_SlugCollision covers the data-loss bug where two tasks
// created the same day with the same (truncated) title hashed to the exact
// same slug: AddTask overwrote the first task's markdown file while still
// handing out a fresh sequential TaskID for the second, orphaning that ID
// and silently losing the first task's tags/body. This mirrors the real
// incident: two identical minecraft_ask timeout errors filed a few seconds
// apart on the same day should now produce two distinct tasks instead of
// one clobbering the other.
func TestAddTask_SlugCollision(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	first, err := b.AddTask("jarvis: minecraft_ask failed", []string{"jarvis-error"}, "first error body")
	if err != nil {
		t.Fatalf("AddTask (first): %v", err)
	}
	second, err := b.AddTask("jarvis: minecraft_ask failed", []string{"jarvis-error"}, "second error body")
	if err != nil {
		t.Fatalf("AddTask (second): %v", err)
	}

	if first.Slug == second.Slug {
		t.Fatalf("second task got the same slug as the first (%q) - would overwrite its file", first.Slug)
	}
	if second.TaskID == first.TaskID {
		t.Fatalf("second task reused TaskID %d instead of allocating a fresh one", first.TaskID)
	}

	// Both files must exist on disk, each with its own body - proof neither
	// write clobbered the other.
	firstPath := filepath.Join(dir, first.PagePath)
	if _, err := os.Stat(firstPath); err != nil {
		t.Errorf("first task file missing at %s: %v", firstPath, err)
	}
	secondPath := filepath.Join(dir, second.PagePath)
	if _, err := os.Stat(secondPath); err != nil {
		t.Errorf("second task file missing at %s: %v", secondPath, err)
	}
	if firstPath == secondPath {
		t.Fatalf("both tasks resolved to the same file path %s", firstPath)
	}

	_, body, err := parseTaskFile(firstPath)
	if err != nil {
		t.Fatalf("parseTaskFile(first): %v", err)
	}
	if body != "first error body" {
		t.Errorf("first task body = %q, want %q (was it overwritten?)", body, "first error body")
	}
	_, body, err = parseTaskFile(secondPath)
	if err != nil {
		t.Fatalf("parseTaskFile(second): %v", err)
	}
	if body != "second error body" {
		t.Errorf("second task body = %q, want %q", body, "second error body")
	}

	// Both should still resolve independently via the DB.
	got, err := db.GetTask(first.Slug)
	if err != nil || got.TaskID != first.TaskID {
		t.Errorf("db.GetTask(%q) = %+v, err=%v, want TaskID %d", first.Slug, got, err, first.TaskID)
	}
	got, err = db.GetTask(second.Slug)
	if err != nil || got.TaskID != second.TaskID {
		t.Errorf("db.GetTask(%q) = %+v, err=%v, want TaskID %d", second.Slug, got, err, second.TaskID)
	}

	// A third collision should keep counting up rather than looping back.
	third, err := b.AddTask("jarvis: minecraft_ask failed", []string{"jarvis-error"}, "third error body")
	if err != nil {
		t.Fatalf("AddTask (third): %v", err)
	}
	if third.Slug == first.Slug || third.Slug == second.Slug {
		t.Fatalf("third task slug %q collided with an earlier one", third.Slug)
	}
}

// TestDedupeTaskSlug exercises the helper directly: non-colliding titles
// must keep the plain slug (existing on-disk slugs must not shift), and
// repeated collisions must count up deterministically.
func TestDedupeTaskSlug(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	slug, pagePath := dedupeTaskSlug(dir, "2026-06-25-minecraft-ask-failed")
	if slug != "2026-06-25-minecraft-ask-failed" {
		t.Errorf("non-colliding slug changed to %q", slug)
	}
	if pagePath != filepath.Join("tasks", slug+".md") {
		t.Errorf("pagePath = %q, want tasks/%s.md", pagePath, slug)
	}

	// Simulate the first task's file already existing on disk.
	if err := os.WriteFile(filepath.Join(dir, pagePath), []byte("---\nstatus: open\n---\n\n# x\n"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	slug2, _ := dedupeTaskSlug(dir, "2026-06-25-minecraft-ask-failed")
	if slug2 != "2026-06-25-minecraft-ask-failed-2" {
		t.Errorf("collision slug = %q, want suffix -2", slug2)
	}

	// Seed the -2 file too and confirm it advances to -3.
	if err := os.WriteFile(filepath.Join(dir, "tasks", slug2+".md"), []byte("---\nstatus: open\n---\n\n# x\n"), 0644); err != nil {
		t.Fatalf("seed file 2: %v", err)
	}
	slug3, _ := dedupeTaskSlug(dir, "2026-06-25-minecraft-ask-failed")
	if slug3 != "2026-06-25-minecraft-ask-failed-3" {
		t.Errorf("second collision slug = %q, want suffix -3", slug3)
	}
}

func TestCompleteTask(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	task, _ := b.AddTask("Test task", []string{"p2"}, "")

	completed, err := b.CompleteTask(task.Slug)
	if err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	if !completed.Completed {
		t.Error("task should be completed")
	}
	if completed.Status != "done" {
		t.Errorf("status = %q, want %q", completed.Status, "done")
	}

	// DB should reflect completion
	got, _ := db.GetTask(task.Slug)
	if !got.Completed {
		t.Error("DB should show task as completed")
	}

	// File should reflect completion
	record, _, err := parseTaskFile(filepath.Join(dir, task.PagePath))
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}
	if !record.Completed {
		t.Error("file frontmatter should show completed=true")
	}
}

func TestCompleteTask_PartialSlug(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	b.AddTask("Investigate checkout timeout", []string{"p1"}, "")

	// Complete by partial slug
	completed, err := b.CompleteTask("checkout")
	if err != nil {
		t.Fatalf("CompleteTask partial: %v", err)
	}
	if !completed.Completed {
		t.Error("task should be completed via partial slug")
	}
}

func TestListTasks_FilterByTag(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	b.AddTask("Task A", []string{"project:aida", "p1"}, "")
	b.AddTask("Task B", []string{"project:website", "p2"}, "")
	b.AddTask("Task C", []string{"project:aida", "p2"}, "")

	// Filter by aida
	tasks, err := b.ListTasks(false, []string{"aida"}, 0, 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 aida tasks, got %d", len(tasks))
	}

	// Filter by p1
	tasks, err = b.ListTasks(false, []string{"p1"}, 0, 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 p1 task, got %d", len(tasks))
	}

	// Filter by aida AND p2
	tasks, err = b.ListTasks(false, []string{"aida", "p2"}, 0, 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 aida+p2 task, got %d", len(tasks))
	}
}

func TestListTasks_FilterByDays(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// Insert tasks with different dates directly in DB
	now := time.Now().UTC()
	db.UpsertTask(&TaskRecord{
		Slug: "recent", Title: "Recent task", Status: "open", Tags: []string{"p1"},
		Created: now.Format(time.RFC3339),
	})
	db.UpsertTask(&TaskRecord{
		Slug: "old", Title: "Old task", Status: "open", Tags: []string{"p2"},
		Created: now.AddDate(0, 0, -10).Format(time.RFC3339),
	})

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	tasks, _ := b.ListTasks(false, nil, 5, 0)
	if len(tasks) != 1 {
		t.Errorf("expected 1 task in last 5 days, got %d", len(tasks))
	}
	if len(tasks) > 0 && tasks[0].Slug != "recent" {
		t.Errorf("expected 'recent' task, got %q", tasks[0].Slug)
	}
}

func TestListTasks_Limit(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	b.AddTask("Task 1", []string{"p1"}, "")
	b.AddTask("Task 2", []string{"p1"}, "")
	b.AddTask("Task 3", []string{"p1"}, "")

	tasks, _ := b.ListTasks(false, nil, 0, 2)
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks with limit=2, got %d", len(tasks))
	}
}

func TestParseTaskFile_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-task.md")

	original := &TaskRecord{
		Slug:      "test-task",
		Title:     "Test roundtrip",
		Status:    "open",
		Completed: false,
		Tags:      []string{"project:aida", "p1"},
		Created:   "2026-04-12T10:00:00Z",
		TaskID:    42,
	}

	if err := writeTaskFile(path, original, "Some extra notes here."); err != nil {
		t.Fatalf("writeTaskFile: %v", err)
	}

	parsed, body, err := parseTaskFile(path)
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}

	if parsed.Title != "Test roundtrip" {
		t.Errorf("title = %q, want %q", parsed.Title, "Test roundtrip")
	}
	if parsed.Status != "open" {
		t.Errorf("status = %q, want %q", parsed.Status, "open")
	}
	if parsed.Completed {
		t.Error("should not be completed")
	}
	if len(parsed.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(parsed.Tags))
	}
	if body != "Some extra notes here." {
		t.Errorf("body = %q, want %q", body, "Some extra notes here.")
	}
	if parsed.TaskID != 42 {
		t.Errorf("task_id = %d, want 42", parsed.TaskID)
	}
}

func TestSlugifyTask(t *testing.T) {
	now := time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		title string
		want  string
	}{
		{"Fix mobile nav", "2026-04-12-fix-mobile-nav"},
		{"Investigate checkout timeout spike!", "2026-04-12-investigate-checkout-timeout-spike"},
		{"What's the deal with   spaces", "2026-04-12-whats-the-deal-with-spaces"},
	}
	for _, tt := range tests {
		got := slugifyTask(tt.title, now)
		if got != tt.want {
			t.Errorf("slugifyTask(%q) = %q, want %q", tt.title, got, tt.want)
		}
	}
}

// TestSlugifyTaskTitle covers the shared normalization both slugifyTask
// (task-file slugs) and internal/jarvis/tools' normalizeTaskRef
// (conversational task references) call through SlugifyTaskTitle. These
// two callers used to be separate, independently-maintained implementations
// (slugifyTask/dedupeTaskSlug landed in one PR, normalizeTaskRef's
// now-removed local slugifyRef duplicate landed in a parallel one); this
// table pins the exact output for representative inputs so a future change
// to SlugifyTaskTitle cannot silently change one caller's behavior without
// the other's.
func TestSlugifyTaskTitle(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{"simple spaces", "Fix mobile nav", "fix-mobile-nav"},
		{"punctuation", "Ryan's oil change!!", "ryans-oil-change"},
		{"repeated separators", "a   b---c", "a-b-c"},
		{"mixed case", "MIXED Case TITLE", "mixed-case-title"},
		{"leading and trailing whitespace", "  leading and trailing whitespace  ", "leading-and-trailing-whitespace"},
		{"leading and trailing hyphens", "---leading and trailing---", "leading-and-trailing"},
		{"underscores", "UPPER_CASE_WITH_UNDERSCORES", "upper-case-with-underscores"},
		{"unicode letters are dropped, not transliterated", "Café déjà vu: naïve résumé", "caf-dj-vu-nave-rsum"},
		{"unicode-only title has no ascii to keep", "日本語のタスク", ""},
		{"emoji dropped like other non-ascii", "emoji task \U0001F389\U0001F389 party", "emoji-task-party"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{
			"very long title truncates to 50 chars",
			"This is a very long task title that definitely exceeds the fifty character truncation limit by quite a lot of characters",
			"this-is-a-very-long-task-title-that-definitely-exc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SlugifyTaskTitle(tt.title)
			if got != tt.want {
				t.Errorf("SlugifyTaskTitle(%q) = %q, want %q", tt.title, got, tt.want)
			}
			if len(got) > 50 {
				t.Errorf("SlugifyTaskTitle(%q) length = %d, want <= 50", tt.title, len(got))
			}
		})
	}
}

func TestTaskCount(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	b.AddTask("Task 1", nil, "")
	b.AddTask("Task 2", nil, "")
	task3, _ := b.AddTask("Task 3", nil, "")
	b.CompleteTask(task3.Slug)

	open, done := db.TaskCount()
	if open != 2 {
		t.Errorf("open = %d, want 2", open)
	}
	if done != 1 {
		t.Errorf("done = %d, want 1", done)
	}
}

func TestIndexTasks(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	// Add task via file directly (simulating git pull from another machine)
	writeTaskFile(filepath.Join(dir, "tasks", "imported-task.md"), &TaskRecord{
		Title:   "Imported from remote",
		Status:  "open",
		Tags:    []string{"project:aida"},
		Created: "2026-04-12T10:00:00Z",
	}, "")

	count, skipped := b.IndexTasks()
	if len(skipped) != 0 {
		t.Fatalf("IndexTasks skipped: %v", skipped)
	}
	if count != 1 {
		t.Errorf("indexed %d tasks, want 1", count)
	}

	// Verify it's in the DB with a stable ID
	task, err := db.GetTask("imported-task")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Title != "Imported from remote" {
		t.Errorf("title = %q, want %q", task.Title, "Imported from remote")
	}
	if task.TaskID <= 0 {
		t.Errorf("imported task should have been assigned a TaskID, got %d", task.TaskID)
	}

	// Re-indexing should preserve the same ID
	firstID := task.TaskID
	b.IndexTasks()
	task, _ = db.GetTask("imported-task")
	if task.TaskID != firstID {
		t.Errorf("re-index changed TaskID from %d to %d", firstID, task.TaskID)
	}
}

// TestIndexTasks_CollidingTaskIDReportedNotSilent covers the real incident
// this was written for: two task files claiming the same task_id (the
// idx_tasks_task_id UNIQUE index makes the second file's upsert fail) used
// to be swallowed by a bare `continue`, so the losing file's task vanished
// from brain.db with no signal at all - `aida tasks show '#N'` reported
// "not found" for a file sitting right there on disk. IndexTasks must now
// report the collision as a skip instead of silently dropping it.
func TestIndexTasks_CollidingTaskIDReportedNotSilent(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	// Two distinct task files, same task_id - as if one machine assigned
	// 480 locally while another machine's already-pushed file also
	// carried 480.
	writeTaskFile(filepath.Join(dir, "tasks", "task-a.md"), &TaskRecord{
		Title:   "Task A",
		Status:  "open",
		Created: "2026-04-12T10:00:00Z",
		TaskID:  480,
	}, "")
	writeTaskFile(filepath.Join(dir, "tasks", "task-b.md"), &TaskRecord{
		Title:   "Task B",
		Status:  "open",
		Created: "2026-04-12T10:01:00Z",
		TaskID:  480,
	}, "")

	count, skipped := b.IndexTasks()

	if count != 1 {
		t.Errorf("indexed %d tasks, want 1 (the winner of the task_id collision)", count)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %d errors, want 1 reporting the collision, got %v", len(skipped), skipped)
	}
	if !strings.Contains(skipped[0].Error(), "task_id 480") {
		t.Errorf("skip error = %q, want it to mention task_id 480", skipped[0].Error())
	}
}

func TestNextTaskID(t *testing.T) {
	dir := t.TempDir()

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	id1, err := db.NextTaskID()
	if err != nil {
		t.Fatalf("NextTaskID: %v", err)
	}
	id2, err := db.NextTaskID()
	if err != nil {
		t.Fatalf("NextTaskID: %v", err)
	}
	id3, err := db.NextTaskID()
	if err != nil {
		t.Fatalf("NextTaskID: %v", err)
	}

	if id1 != 1 || id2 != 2 || id3 != 3 {
		t.Errorf("expected 1,2,3 got %d,%d,%d", id1, id2, id3)
	}
}

// TestAddTask_SyncsTaskSeqFromDiskFirst covers the multi-machine drift bug:
// a task file written directly to disk (simulating one pulled in by `git
// pull` from another machine, before that file has been indexed into
// brain.db) carries a high task_id. AddTask must notice it and allocate
// past it rather than reusing/colliding with it.
func TestAddTask_SyncsTaskSeqFromDiskFirst(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	// Write a task file directly to disk, bypassing AddTask/UpsertTask, with
	// a high task_id - as if it arrived via a brain repo git pull and has
	// not yet been reindexed.
	pulledPath := filepath.Join(dir, "tasks", "pulled-from-another-machine.md")
	pulled := &TaskRecord{
		Slug:    "pulled-from-another-machine",
		Title:   "Task from another machine",
		Status:  "open",
		Created: time.Now().UTC().Format(time.RFC3339),
		TaskID:  900,
	}
	if err := writeTaskFile(pulledPath, pulled, ""); err != nil {
		t.Fatalf("writeTaskFile: %v", err)
	}

	task, err := b.AddTask("New local task", nil, "")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	if task.TaskID != 901 {
		t.Errorf("task.TaskID = %d, want 901 (past the on-disk 900)", task.TaskID)
	}
}

func TestGetTaskByID(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	task1, _ := b.AddTask("First task", []string{"p1"}, "")
	task2, _ := b.AddTask("Second task", []string{"p2"}, "")

	// Look up by stable ID
	got, err := db.GetTaskByID(task1.TaskID)
	if err != nil {
		t.Fatalf("GetTaskByID(%d): %v", task1.TaskID, err)
	}
	if got.Title != "First task" {
		t.Errorf("got %q, want %q", got.Title, "First task")
	}

	got, err = db.GetTaskByID(task2.TaskID)
	if err != nil {
		t.Fatalf("GetTaskByID(%d): %v", task2.TaskID, err)
	}
	if got.Title != "Second task" {
		t.Errorf("got %q, want %q", got.Title, "Second task")
	}

	// Non-existent ID
	_, err = db.GetTaskByID(999)
	if err == nil {
		t.Error("expected error for non-existent task ID")
	}
}

func TestResolveTaskRef(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	t1, _ := b.AddTask("Investigate checkout timeout", []string{"p1"}, "")
	t2, _ := b.AddTask("Update README", []string{"p3"}, "")

	// #N lookup
	got, err := b.ResolveTaskRef("#" + toStr(t1.TaskID))
	if err != nil {
		t.Fatalf("ResolveTaskRef(#%d): %v", t1.TaskID, err)
	}
	if got.Slug != t1.Slug {
		t.Errorf("got slug %q, want %q", got.Slug, t1.Slug)
	}

	// Exact slug lookup
	got, err = b.ResolveTaskRef(t2.Slug)
	if err != nil {
		t.Fatalf("ResolveTaskRef(%q): %v", t2.Slug, err)
	}
	if got.Slug != t2.Slug {
		t.Errorf("got slug %q, want %q", got.Slug, t2.Slug)
	}

	// Partial slug lookup
	got, err = b.ResolveTaskRef("checkout")
	if err != nil {
		t.Fatalf("ResolveTaskRef partial: %v", err)
	}
	if got.Slug != t1.Slug {
		t.Errorf("partial got slug %q, want %q", got.Slug, t1.Slug)
	}

	// Invalid #N
	if _, err := b.ResolveTaskRef("#abc"); err == nil {
		t.Error("expected error for #abc")
	}
	if _, err := b.ResolveTaskRef("#0"); err == nil {
		t.Error("expected error for #0")
	}

	// Unknown #N
	if _, err := b.ResolveTaskRef("#999"); err == nil {
		t.Error("expected error for #999")
	}

	// Empty
	if _, err := b.ResolveTaskRef(""); err == nil {
		t.Error("expected error for empty ref")
	}
}

func TestUpdateTask_Title(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}
	task, _ := b.AddTask("Old title", []string{"p2"}, "")

	newTitle := "New title"
	updated, err := b.UpdateTask(task.Slug, TaskPatch{Title: &newTitle})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Title != "New title" {
		t.Errorf("title = %q, want %q", updated.Title, "New title")
	}
	if updated.TaskID != task.TaskID {
		t.Errorf("TaskID changed from %d to %d", task.TaskID, updated.TaskID)
	}

	// File reflects update
	rec, _, err := parseTaskFile(filepath.Join(dir, task.PagePath))
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}
	if rec.Title != "New title" {
		t.Errorf("file title = %q, want %q", rec.Title, "New title")
	}
}

func TestUpdateTask_BodyAndTags(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}
	task, _ := b.AddTask("With body", []string{"p3"}, "initial body")

	newBody := "updated body text"
	_, err = b.UpdateTask(task.Slug, TaskPatch{
		Body:    &newBody,
		AddTags: []string{"new-tag", "p3"}, // p3 already present - should dedup
	})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	rec, body, err := parseTaskFile(filepath.Join(dir, task.PagePath))
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}
	if body != "updated body text" {
		t.Errorf("body = %q, want %q", body, "updated body text")
	}
	hasNewTag := false
	p3Count := 0
	for _, tag := range rec.Tags {
		if tag == "new-tag" {
			hasNewTag = true
		}
		if tag == "p3" {
			p3Count++
		}
	}
	if !hasNewTag {
		t.Errorf("expected new-tag in %v", rec.Tags)
	}
	if p3Count != 1 {
		t.Errorf("expected p3 once in %v (dedup), got %d", rec.Tags, p3Count)
	}
}

func TestUpdateTask_RemoveTags(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, _ := OpenDB(dir)
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}
	task, _ := b.AddTask("Prune me", []string{"keep", "drop", "p2"}, "")

	_, err := b.UpdateTask(task.Slug, TaskPatch{RemoveTags: []string{"drop"}})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	got, _ := db.GetTask(task.Slug)
	for _, tag := range got.Tags {
		if tag == "drop" {
			t.Errorf("tag 'drop' should have been removed from %v", got.Tags)
		}
	}
}

func TestUpdateTask_TagsExclusive(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, _ := OpenDB(dir)
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}
	task, _ := b.AddTask("Exclusive check", []string{"p2"}, "")

	replace := []string{"x", "y"}
	_, err := b.UpdateTask(task.Slug, TaskPatch{
		Tags:    &replace,
		AddTags: []string{"z"},
	})
	if err == nil {
		t.Error("expected error when Tags and AddTags both set")
	}
}

func TestUpdateTask_StatusTransition(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, _ := OpenDB(dir)
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}
	task, _ := b.AddTask("Flip me", []string{"p2"}, "")

	done := "done"
	updated, err := b.UpdateTask(task.Slug, TaskPatch{Status: &done})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if !updated.Completed || updated.Status != "done" {
		t.Errorf("expected completed=true status=done, got %+v", updated)
	}

	// Invalid status rejected
	bogus := "archived"
	if _, err := b.UpdateTask(task.Slug, TaskPatch{Status: &bogus}); err == nil {
		t.Error("expected error for invalid status")
	}
}

func TestUpdateTask_UnknownSlug(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, _ := OpenDB(dir)
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}
	newTitle := "whatever"
	if _, err := b.UpdateTask("no-such-task", TaskPatch{Title: &newTitle}); err == nil {
		t.Error("expected error for unknown slug")
	}
}

func TestReopenTask(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, _ := OpenDB(dir)
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}
	task, _ := b.AddTask("Close then reopen", []string{"p2"}, "")
	b.CompleteTask(task.Slug)

	reopened, err := b.ReopenTask(task.Slug)
	if err != nil {
		t.Fatalf("ReopenTask: %v", err)
	}
	if reopened.Completed || reopened.Status != "open" {
		t.Errorf("expected completed=false status=open, got %+v", reopened)
	}

	// DB reflects
	got, _ := db.GetTask(task.Slug)
	if got.Completed {
		t.Error("DB should show task as open")
	}

	// File reflects
	rec, _, _ := parseTaskFile(filepath.Join(dir, task.PagePath))
	if rec.Completed {
		t.Error("file frontmatter should show completed=false")
	}
}

func TestReopenTask_Idempotent(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, _ := OpenDB(dir)
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}
	task, _ := b.AddTask("Already open", []string{"p2"}, "")

	// Reopening an already-open task must not error
	if _, err := b.ReopenTask(task.Slug); err != nil {
		t.Errorf("reopen on open task should not error: %v", err)
	}
}

func toStr(n int) string {
	if n == 0 {
		return "0"
	}
	var s []byte
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	return string(s)
}

func TestUpsertPreservesTaskID(t *testing.T) {
	dir := t.TempDir()

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// Insert with a task ID
	original := &TaskRecord{
		Slug:    "test-preserve",
		Title:   "Original title",
		Status:  "open",
		Tags:    []string{"p1"},
		Created: "2026-04-12T10:00:00Z",
		TaskID:  42,
	}
	db.UpsertTask(original)

	// Upsert with TaskID=0 (simulating re-index from file without task_id)
	updated := &TaskRecord{
		Slug:    "test-preserve",
		Title:   "Updated title",
		Status:  "open",
		Tags:    []string{"p1"},
		Created: "2026-04-12T10:00:00Z",
		TaskID:  0,
	}
	db.UpsertTask(updated)

	got, err := db.GetTask("test-preserve")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Title != "Updated title" {
		t.Errorf("title = %q, want %q", got.Title, "Updated title")
	}
	if got.TaskID != 42 {
		t.Errorf("TaskID = %d, want 42 (should be preserved)", got.TaskID)
	}
}

func TestSetTaskStatus_AllStatuses(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	task, err := b.AddTask("Status walk", nil, "")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// Walk through every canonical status, verify completed bit follows.
	cases := []struct {
		status   string
		terminal bool
	}{
		{StatusInProgress, false},
		{StatusHold, false},
		{StatusDeferred, false},
		{StatusDone, true},
		{StatusClosed, true},
		{StatusOpen, false},
	}
	for _, c := range cases {
		got, err := b.SetTaskStatus(task.Slug, c.status)
		if err != nil {
			t.Fatalf("SetTaskStatus(%s): %v", c.status, err)
		}
		if got.Status != c.status {
			t.Errorf("status = %q, want %q", got.Status, c.status)
		}
		if got.Completed != c.terminal {
			t.Errorf("status %q completed = %v, want %v", c.status, got.Completed, c.terminal)
		}
	}

	// Invalid status rejected.
	if _, err := b.SetTaskStatus(task.Slug, "garbage"); err == nil {
		t.Error("expected error for invalid status")
	}
}

func TestListTasksByStatus_FiltersExactly(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	// Three tasks, three statuses.
	t1, _ := b.AddTask("active", nil, "")
	t2, _ := b.AddTask("on hold", nil, "")
	t3, _ := b.AddTask("did it", nil, "")
	if _, err := b.SetTaskStatus(t2.Slug, StatusHold); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err := b.SetTaskStatus(t3.Slug, StatusDone); err != nil {
		t.Fatalf("done: %v", err)
	}

	// Default ListTasks (showCompleted=false) hides terminal only.
	got, _ := b.ListTasks(false, nil, 0, 0)
	if len(got) != 2 {
		t.Errorf("ListTasks(false) got %d tasks, want 2 (open + hold)", len(got))
	}

	// ListTasksByStatus(active) returns only the open task.
	got, _ = b.ListTasksByStatus(ActiveStatuses(), nil, 0, 0)
	if len(got) != 1 || got[0].Slug != t1.Slug {
		t.Errorf("ActiveStatuses got %v, want [%s]", taskSlugsFromRecords(got), t1.Slug)
	}

	// ListTasksByStatus(closed) returns nothing - no closed tasks.
	got, _ = b.ListTasksByStatus([]string{StatusClosed}, nil, 0, 0)
	if len(got) != 0 {
		t.Errorf("closed filter got %d, want 0", len(got))
	}

	// ListTasksByStatus(done) returns the done task.
	got, _ = b.ListTasksByStatus([]string{StatusDone}, nil, 0, 0)
	if len(got) != 1 || got[0].Slug != t3.Slug {
		t.Errorf("done filter got %v, want [%s]", taskSlugsFromRecords(got), t3.Slug)
	}
}

func TestTaskCountByStatus_AndFormat(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	t1, _ := b.AddTask("a", nil, "")
	t2, _ := b.AddTask("b", nil, "")
	t3, _ := b.AddTask("c", nil, "")
	t4, _ := b.AddTask("d", nil, "")
	if _, err := b.SetTaskStatus(t2.Slug, StatusInProgress); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetTaskStatus(t3.Slug, StatusDone); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetTaskStatus(t4.Slug, StatusClosed); err != nil {
		t.Fatal(err)
	}
	_ = t1

	counts, err := db.TaskCountByStatus()
	if err != nil {
		t.Fatalf("TaskCountByStatus: %v", err)
	}
	want := map[string]int{StatusOpen: 1, StatusInProgress: 1, StatusDone: 1, StatusClosed: 1}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("counts[%s] = %d, want %d", k, counts[k], v)
		}
	}

	got := FormatTaskCounts(counts)
	wantStr := "1 open, 1 in-progress, 1 done, 1 closed"
	if got != wantStr {
		t.Errorf("FormatTaskCounts = %q, want %q", got, wantStr)
	}

	if FormatTaskCounts(nil) != "" {
		t.Error("empty counts should format to empty string")
	}
	// Unknown legacy status passes through.
	mixed := map[string]int{StatusOpen: 2, "legacy-blocked": 1}
	got = FormatTaskCounts(mixed)
	if got != "2 open, 1 legacy-blocked" {
		t.Errorf("legacy passthrough = %q", got)
	}
}

func TestCompleteTask_SetsCompletedAt(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	task, _ := b.AddTask("stamp me", nil, "")
	if task.CompletedAt != "" {
		t.Errorf("new task CompletedAt = %q, want empty", task.CompletedAt)
	}

	before := time.Now().UTC()
	completed, err := b.CompleteTask(task.Slug)
	if err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	if completed.CompletedAt == "" {
		t.Fatal("CompletedAt should be set after CompleteTask")
	}
	parsed, err := time.Parse(time.RFC3339, completed.CompletedAt)
	if err != nil {
		t.Fatalf("CompletedAt %q not RFC3339: %v", completed.CompletedAt, err)
	}
	if parsed.Before(before.Add(-time.Second)) || parsed.After(after) {
		t.Errorf("CompletedAt %v outside [%v, %v]", parsed, before, after)
	}

	// DB should reflect it
	got, _ := db.GetTask(task.Slug)
	if got.CompletedAt != completed.CompletedAt {
		t.Errorf("DB CompletedAt = %q, want %q", got.CompletedAt, completed.CompletedAt)
	}

	// File frontmatter should reflect it
	record, _, err := parseTaskFile(filepath.Join(dir, task.PagePath))
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}
	if record.CompletedAt != completed.CompletedAt {
		t.Errorf("file CompletedAt = %q, want %q", record.CompletedAt, completed.CompletedAt)
	}
}

func TestReopenTask_ClearsCompletedAt(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	task, _ := b.AddTask("stamp and clear", nil, "")
	if _, err := b.CompleteTask(task.Slug); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	reopened, err := b.ReopenTask(task.Slug)
	if err != nil {
		t.Fatalf("ReopenTask: %v", err)
	}
	if reopened.CompletedAt != "" {
		t.Errorf("ReopenTask CompletedAt = %q, want empty", reopened.CompletedAt)
	}

	// DB
	got, _ := db.GetTask(task.Slug)
	if got.CompletedAt != "" {
		t.Errorf("DB CompletedAt = %q, want empty", got.CompletedAt)
	}

	// File
	record, _, err := parseTaskFile(filepath.Join(dir, task.PagePath))
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}
	if record.CompletedAt != "" {
		t.Errorf("file CompletedAt = %q, want empty", record.CompletedAt)
	}
}

func TestSetTaskStatus_PreservesAcrossTerminalSwap(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	task, _ := b.AddTask("done first then closed", nil, "")
	doneTask, err := b.SetTaskStatus(task.Slug, StatusDone)
	if err != nil {
		t.Fatalf("SetTaskStatus(done): %v", err)
	}
	originalCompletedAt := doneTask.CompletedAt
	if originalCompletedAt == "" {
		t.Fatal("CompletedAt should be set after open->done")
	}

	// Sleep a touch so any wrong implementation that re-stamps would produce a different value.
	time.Sleep(10 * time.Millisecond)

	closedTask, err := b.SetTaskStatus(task.Slug, StatusClosed)
	if err != nil {
		t.Fatalf("SetTaskStatus(closed): %v", err)
	}
	if closedTask.CompletedAt != originalCompletedAt {
		t.Errorf("done->closed CompletedAt = %q, want %q (preserved)", closedTask.CompletedAt, originalCompletedAt)
	}

	// Round-trip via DB and file
	got, _ := db.GetTask(task.Slug)
	if got.CompletedAt != originalCompletedAt {
		t.Errorf("DB CompletedAt = %q, want %q", got.CompletedAt, originalCompletedAt)
	}
	record, _, _ := parseTaskFile(filepath.Join(dir, task.PagePath))
	if record.CompletedAt != originalCompletedAt {
		t.Errorf("file CompletedAt = %q, want %q", record.CompletedAt, originalCompletedAt)
	}
}

func TestSetTaskStatus_AllStatuses_CompletedAtTracking(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "test"}

	task, _ := b.AddTask("walk", nil, "")

	// open -> in-progress -> hold -> deferred: all non-terminal, CompletedAt stays empty.
	for _, s := range []string{StatusInProgress, StatusHold, StatusDeferred} {
		got, err := b.SetTaskStatus(task.Slug, s)
		if err != nil {
			t.Fatalf("SetTaskStatus(%s): %v", s, err)
		}
		if got.CompletedAt != "" {
			t.Errorf("status %q CompletedAt = %q, want empty (non-terminal)", s, got.CompletedAt)
		}
	}

	// deferred -> done: stamps.
	doneTask, err := b.SetTaskStatus(task.Slug, StatusDone)
	if err != nil {
		t.Fatalf("SetTaskStatus(done): %v", err)
	}
	if doneTask.CompletedAt == "" {
		t.Error("deferred->done should stamp CompletedAt")
	}
	stampedAt := doneTask.CompletedAt

	// done -> closed: preserved (covered in dedicated test above; sanity here).
	closedTask, _ := b.SetTaskStatus(task.Slug, StatusClosed)
	if closedTask.CompletedAt != stampedAt {
		t.Errorf("done->closed CompletedAt = %q, want preserved %q", closedTask.CompletedAt, stampedAt)
	}

	// closed -> open: clears.
	openedTask, _ := b.SetTaskStatus(task.Slug, StatusOpen)
	if openedTask.CompletedAt != "" {
		t.Errorf("closed->open CompletedAt = %q, want empty", openedTask.CompletedAt)
	}
}

func TestWriteTaskFile_RoundTripCompletedAt(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tasks"), 0755)

	path := filepath.Join(dir, "tasks", "rt-test.md")
	rec := &TaskRecord{
		Slug:        "rt-test",
		Title:       "Round trip",
		Status:      StatusDone,
		Completed:   true,
		Tags:        []string{"p2", "profile:test"},
		Created:     "2026-04-28T10:00:00Z",
		CompletedAt: "2026-04-28T15:30:00Z",
		TaskID:      99,
	}
	if err := writeTaskFile(path, rec, ""); err != nil {
		t.Fatalf("writeTaskFile: %v", err)
	}

	got, _, err := parseTaskFile(path)
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}
	if got.CompletedAt != rec.CompletedAt {
		t.Errorf("CompletedAt round-trip = %q, want %q", got.CompletedAt, rec.CompletedAt)
	}

	// Empty CompletedAt should round-trip as empty (omitempty).
	rec.CompletedAt = ""
	rec.Completed = false
	rec.Status = StatusOpen
	if err := writeTaskFile(path, rec, ""); err != nil {
		t.Fatalf("writeTaskFile (empty): %v", err)
	}
	got, _, err = parseTaskFile(path)
	if err != nil {
		t.Fatalf("parseTaskFile (empty): %v", err)
	}
	if got.CompletedAt != "" {
		t.Errorf("empty CompletedAt round-trip = %q, want empty", got.CompletedAt)
	}

	// And the file shouldn't contain a `completed_at:` line at all when empty (omitempty).
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "completed_at:") {
		t.Errorf("empty CompletedAt should not write the key; file:\n%s", string(raw))
	}
}

func taskSlugsFromRecords(tasks []TaskRecord) []string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.Slug
	}
	return out
}
