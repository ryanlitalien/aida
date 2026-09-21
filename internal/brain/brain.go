// Package brain implements the shared memory/knowledge system for Aida.
//
// The brain is a two-layer system:
//  1. A git repository of markdown files (source of truth, human-readable)
//  2. A SQLite index with vector embeddings (fast semantic search)
//
// The markdown files sync via git across machines. The SQLite database is
// local-only (.gitignore'd) and rebuilt from the markdown files.
//
// On each query, the brain provides context to the LLM router:
//   - Semantically similar past lessons (replaces Jaccard word-overlap)
//   - Entity pages for mentioned partners/tools
//   - Compiled routing wisdom
//
// After each query, the brain records:
//   - A lesson JSON file in brain/lessons/{profile}/
//   - A row in brain.db with the embedding
//   - Evidence lines appended to entity markdown pages
package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/ui"
)

// Brain is the main interface to the shared memory system.
type Brain struct {
	Path       string
	DB         *DB
	Embeddings *EmbeddingClient
	GitHubRepo string // owner/repo for GH issue mirroring (empty = disabled)
	profile    string
}

// Open initializes the brain at the given path. Creates the directory
// structure and opens the SQLite index. The profile is used to partition
// lesson files (e.g., "work", "home").
func Open(brainPath, profile, voyageKeyEnv, githubRepo string) (*Brain, error) {
	if brainPath == "" {
		home, _ := os.UserHomeDir()
		brainPath = filepath.Join(home, ".aida", "brain")
	}

	b := &Brain{
		Path:       brainPath,
		Embeddings: NewEmbeddingClient(voyageKeyEnv),
		GitHubRepo: githubRepo,
		profile:    profile,
	}

	// Ensure directory structure exists
	if err := b.ensureDirs(); err != nil {
		return nil, fmt.Errorf("brain dirs: %w", err)
	}

	// Open SQLite index
	db, err := OpenDB(brainPath)
	if err != nil {
		return nil, fmt.Errorf("brain db: %w", err)
	}
	b.DB = db

	// Loud-fail startup check: if the user configured a Voyage key env var
	// but it's empty, warn them once to stderr. This catches the silent
	// degrade where RecordLesson / Search quietly skip embeddings and
	// every semantic-similarity feature falls back to Jaccard.
	warnIfEmbeddingsMisconfigured(voyageKeyEnv, b)

	return b, nil
}

// startupWarnOnce guards warnIfEmbeddingsMisconfigured so the message is
// only printed the first time a brain is opened in a given process.
var startupWarnOnce sync.Once

// warnIfEmbeddingsMisconfigured prints a single stderr warning when the
// user has declared `brain.embeddings.api_key_env` in config but the
// referenced env var is not set. Separately, it also warns when the
// embedding client IS available but brain.db has lessons without stored
// embeddings -- the signal that a backfill is needed.
func warnIfEmbeddingsMisconfigured(voyageKeyEnv string, b *Brain) {
	startupWarnOnce.Do(func() {
		if voyageKeyEnv == "" {
			return // no key env configured -- user opted out
		}
		if os.Getenv(voyageKeyEnv) == "" {
			fmt.Fprintf(os.Stderr,
				"warn: %s is unset -- semantic search disabled, "+
					"lessons will not be embedded (add it to ~/.aida/.env)\n",
				voyageKeyEnv)
			return
		}
		if b.DB == nil {
			return
		}
		var total, withEmb int
		if err := b.DB.conn.QueryRow(
			`SELECT COUNT(*), COUNT(question_embedding) FROM lessons WHERE profile = ?`,
			b.profile,
		).Scan(&total, &withEmb); err != nil {
			return
		}
		missing := total - withEmb
		// Only nag when a meaningful fraction is missing (> 10%).
		if total > 10 && missing*10 > total {
			fmt.Fprintf(os.Stderr,
				"warn: %d of %d lessons in profile %q are missing embeddings -- "+
					"run `aida brain reembed --table lessons` to backfill\n",
				missing, total, b.profile)
		}
	})
}

// Profile returns the active profile the brain was opened for.
func (b *Brain) Profile() string { return b.profile }

// Close closes the brain's database connection.
func (b *Brain) Close() error {
	if b.DB != nil {
		return b.DB.Close()
	}
	return nil
}

func (b *Brain) ensureDirs() error {
	dirs := []string{
		b.Path,
		filepath.Join(b.Path, "entities", "partners"),
		filepath.Join(b.Path, "entities", "tools"),
		filepath.Join(b.Path, "entities", "people"),
		filepath.Join(b.Path, "knowledge", "routing"),
		filepath.Join(b.Path, "knowledge", "patterns"),
		filepath.Join(b.Path, "knowledge", "domains"), // semantic source profiles
		filepath.Join(b.Path, "lessons"),
		filepath.Join(b.Path, "tasks"),
		filepath.Join(b.Path, "meta"),
	}
	if b.profile != "" {
		dirs = append(dirs, filepath.Join(b.Path, "lessons", b.profile))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	return nil
}

// SearchContext holds the brain context loaded for a query.
type SearchContext struct {
	SimilarLessons []SimilarLesson
	EntityPages    []EntityPage
	KnowledgePages []KnowledgePageRecord
	RoutingWisdom  string
	OpenTasks      []TaskRecord
}

// EntityPage is a matched entity with its compiled truth content.
type EntityPage struct {
	Entity  EntityRecord
	Content string // compiled truth section (above the HR)
}

// Search loads brain context for a query. It:
//  1. Embeds the question via Voyage AI
//  2. Finds K similar past lessons in brain.db
//  3. Looks up entities mentioned in the query
//  4. Finds K matching knowledge-domain pages (vector + keyword)
//  5. Loads compiled routing wisdom
func (b *Brain) Search(ctx context.Context, question string, entities []string, k int) (*SearchContext, error) {
	sc := &SearchContext{}

	// 1. Embed the question and find similar lessons
	var queryEmbedding []float32
	if b.Embeddings.Available() {
		emb, err := b.Embeddings.EmbedQuery(ctx, question)
		if err != nil {
			ui.PrintVerbose("Brain embeddings", "failed: "+err.Error())
		} else {
			queryEmbedding = emb
		}
	}

	similar, err := b.DB.FindSimilar(queryEmbedding, k, b.profile)
	if err != nil {
		ui.PrintVerbose("Brain search", "failed: "+err.Error())
	} else {
		sc.SimilarLessons = similar
	}

	// 2. Find matching entities
	if len(entities) > 0 {
		matched, err := b.DB.FindEntities(entities)
		if err != nil {
			ui.PrintVerbose("Brain entities", "failed: "+err.Error())
		}
		for _, e := range matched {
			if e.PagePath != "" {
				content := loadCompiledTruth(filepath.Join(b.Path, e.PagePath))
				sc.EntityPages = append(sc.EntityPages, EntityPage{Entity: e, Content: content})
			}
		}
	}

	// 3. Knowledge-domain pages (knowledge_index.go). Vector hits first
	// when the question could be embedded, then keyword hits from the
	// corpus_fts rows so pages still surface with no embedding client
	// (or for literal tokens the embedding blurs), deduped and capped.
	sc.KnowledgePages = b.findKnowledgePages(question, queryEmbedding, k)

	// 4. Load compiled routing wisdom
	wisdomPath := filepath.Join(b.Path, "knowledge", "routing", "compiled.md")
	if data, err := os.ReadFile(wisdomPath); err == nil {
		sc.RoutingWisdom = string(data)
	}

	// 5. If query looks task-related, include open tasks. Profile isolation
	// is enforced by DB.ListTasks itself - no manual tag construction.
	if isTaskQuery(question) {
		tasks, err := b.DB.ListTasks(false, nil, 0, 20, b.profile, nil)
		if err != nil {
			ui.PrintVerbose("Brain tasks", "failed: "+err.Error())
		} else {
			sc.OpenTasks = tasks
		}
	}

	return sc, nil
}

// findKnowledgePages merges the vector and keyword channels over
// knowledge_pages into one ordered, deduplicated list of at most k pages.
// Vector hits lead (they are similarity-ranked); FTS hits fill in behind
// them in MATCH-rank order.
func (b *Brain) findKnowledgePages(question string, queryEmbedding []float32, k int) []KnowledgePageRecord {
	if k <= 0 {
		return nil
	}
	var out []KnowledgePageRecord
	seen := make(map[string]bool)
	if queryEmbedding != nil {
		pages, err := b.DB.FindSimilarKnowledgePages(queryEmbedding, k)
		if err != nil {
			ui.PrintVerbose("Brain knowledge", "vector failed: "+err.Error())
		}
		for _, p := range pages {
			if !seen[p.Slug] {
				seen[p.Slug] = true
				out = append(out, p)
			}
		}
	}
	if len(out) < k {
		ids, err := b.DB.FTSSearchByType(question, KnowledgeDocType, k)
		if err != nil {
			ui.PrintVerbose("Brain knowledge", "fts failed: "+err.Error())
		}
		for _, id := range ids {
			slug := strings.TrimPrefix(id, "knowledge:")
			if seen[slug] {
				continue
			}
			p, err := b.DB.GetKnowledgePage(slug)
			if err != nil || p == nil {
				continue
			}
			seen[slug] = true
			out = append(out, *p)
			if len(out) >= k {
				break
			}
		}
	}
	return out
}

func isTaskQuery(q string) bool {
	lower := strings.ToLower(q)
	taskTerms := []string{"task", "tasks", "todo", "to do", "to-do", "what should i",
		"what do i need", "open items", "action items", "what's next", "priorities"}
	for _, term := range taskTerms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

// loadCompiledTruth reads a brain page and returns everything above the
// first horizontal rule (---). This is the "compiled truth" section.
func loadCompiledTruth(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := string(data)
	// Split at first "---" that appears on its own line (after the title)
	lines := strings.Split(content, "\n")
	var result []string
	foundContent := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if foundContent && (trimmed == "---" || trimmed == "***" || trimmed == "___") {
			break
		}
		if trimmed != "" {
			foundContent = true
		}
		result = append(result, line)
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

// LessonID deterministically computes the brain.db / lesson-file id that
// RecordLesson will assign to a lesson with the given timestamp and
// question. Exported so a caller that needs to know a lesson's id before
// (or without) writing a second lesson can compute it - e.g. feedback.go
// predicts the id of the thumbs-down lesson it's about to record so it
// can pass it as the "except" id to SupersedeLessonsByRunID and avoid the
// new lesson retracting itself.
func LessonID(timestamp, question string) string {
	hash := shortHash(question)
	return fmt.Sprintf("%s-%s", strings.ReplaceAll(strings.ReplaceAll(timestamp, ":", "-"), "+", ""), hash)
}

// RecordLesson writes a lesson to both the brain repo (JSON file) and
// brain.db (with embedding). This replaces lessons.Append().
func (b *Brain) RecordLesson(ctx context.Context, lesson *lessons.Lesson) error {
	// Generate ID from timestamp
	ts := lesson.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
		lesson.Timestamp = ts
	}
	id := LessonID(ts, lesson.Question)

	// Build the brain record
	record := &LessonRecord{
		ID:                       id,
		Timestamp:                ts,
		Profile:                  b.profile,
		Question:                 lesson.Question,
		RunID:                    lesson.RunID,
		Action:                   lesson.Action,
		Strategy:                 lesson.Strategy,
		Sources:                  lesson.Sources,
		ArtifactCount:            lesson.ArtifactCount,
		Quality:                  lesson.Quality,
		QualityReason:            lesson.QualityReason,
		Feedback:                 string(lesson.Feedback),
		FeedbackReason:           lesson.FeedbackReason,
		FeedbackIntendedSource:   lesson.FeedbackIntendedSource,
		FeedbackIntendedSources:  lesson.FeedbackIntendedSources,
		FeedbackExcludedSources:  lesson.FeedbackExcludedSources,
		FeedbackFailureType:      lesson.FeedbackFailureType,
		FeedbackOutputDirectives: lesson.FeedbackOutputDirectives,
		AnswerSnippet:            lesson.AnswerSnippet,
		RoutingHint:              lesson.RoutingHint,
		ExecutedQueries:          lesson.ExecutedQueries,
	}

	// Convert PerSourceStatus
	record.PerSourceStatus = make(map[string]string)
	for k, v := range lesson.PerSourceStatus {
		record.PerSourceStatus[k] = string(v)
	}

	// Generate embedding (use "document" type for stored lessons)
	if b.Embeddings.Available() {
		embs, err := b.Embeddings.EmbedDocuments(ctx, []string{lesson.Question})
		if err != nil {
			ui.PrintVerbose("Brain embedding", "failed: "+err.Error())
		} else if len(embs) > 0 {
			record.Embedding = embs[0]
		}
	}

	// 1. Write JSON file to brain/lessons/{profile}/{id}.json
	if err := b.writeLessonFile(id, record); err != nil {
		ui.PrintVerbose("Brain lesson file", "write error: "+err.Error())
	}

	// 2. Insert into brain.db
	if err := b.DB.InsertLesson(record); err != nil {
		return fmt.Errorf("brain db insert: %w", err)
	}

	// 3. Touch brain.db so IsStale doesn't count this just-written JSON
	// file as a reason to rebuild on the next query. RecordLesson already
	// embedded + inserted the lesson; a rebuild would be redundant.
	// TouchDB failures are non-fatal - worst case is one extra rebuild.
	TouchDB(b.Path)

	return nil
}

func (b *Brain) writeLessonFile(id string, record *LessonRecord) error {
	dir := filepath.Join(b.Path, "lessons", b.profile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, id+".json")
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// markLessonFileSuperseded reads the lesson JSON file for id under
// profile, flips Superseded true, zeroes Quality, clears RoutingHint, and
// rewrites the file - preserving every other field - so the on-disk copy
// matches the db row SupersedeLessonByID / SupersedeLessonsByRunID just
// flagged.
func (b *Brain) markLessonFileSuperseded(profile, id string) error {
	path := filepath.Join(b.Path, "lessons", profile, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var record LessonRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	record.Superseded = true
	record.Quality = 0
	record.RoutingHint = ""
	out, err := json.MarshalIndent(&record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

// SupersedeLessonsByRunID retracts every other lesson recorded for the
// same run as exceptID (typically the feedback lesson just written by
// `aida thumbs-down`): matching db rows are flagged superseded (quality
// zeroed, routing_hint cleared, FTS entry removed - see
// DB.SupersedeLessonsByRunID) and their JSON files under
// brain/lessons/<profile>/ are rewritten to match. Returns the number of
// lessons superseded. File-rewrite failures are logged (verbose) but
// non-fatal - the db row is the source of truth for recall filtering;
// the JSON file is audit history.
func (b *Brain) SupersedeLessonsByRunID(ctx context.Context, runID, exceptID string) (int, error) {
	matched, err := b.DB.SupersedeLessonsByRunID(runID, exceptID)
	if err != nil {
		return 0, err
	}
	for _, m := range matched {
		if err := b.markLessonFileSuperseded(m.Profile, m.ID); err != nil {
			ui.PrintVerbose("Brain supersede", fmt.Sprintf("rewrite %s: %s", m.ID, err))
		}
	}
	return len(matched), nil
}

// RetractLesson marks a single lesson superseded via the same db-flip +
// file-rewrite mechanism as SupersedeLessonsByRunID, letting an operator
// manually retract one bad lesson (`aida brain lesson retract <id>`)
// independent of any run-based auto-supersession. Returns a clear error
// if no lesson has that id.
func (b *Brain) RetractLesson(ctx context.Context, id string) error {
	profile, err := b.DB.SupersedeLessonByID(id)
	if err != nil {
		return fmt.Errorf("lesson %q not found: %w", id, err)
	}
	if err := b.markLessonFileSuperseded(profile, id); err != nil {
		ui.PrintVerbose("Brain retract", fmt.Sprintf("rewrite %s: %s", id, err))
	}
	return nil
}

// AppendEvidence appends an evidence line to an entity's brain page.
func (b *Brain) AppendEvidence(entityType, slug, evidenceLine string) error {
	pagePath := filepath.Join(b.Path, "entities", entityType, slug+".md")

	// Create page if it doesn't exist
	if _, err := os.Stat(pagePath); os.IsNotExist(err) {
		initial := fmt.Sprintf("# %s (%s)\n\n(No compiled truth yet - run `aida brain compile` to synthesize.)\n\n---\n\n## Evidence Trail\n\n", slug, entityType)
		if err := os.WriteFile(pagePath, []byte(initial), 0644); err != nil {
			return err
		}
	}

	f, err := os.OpenFile(pagePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	line := fmt.Sprintf("- %s: %s\n", time.Now().UTC().Format("2006-01-02"), evidenceLine)
	_, err = f.WriteString(line)
	return err
}

// FormatContextForRouter formats the brain search context into a string
// suitable for injection into the LLM router prompt.
func (sc *SearchContext) FormatContextForRouter() string {
	if sc == nil {
		return ""
	}

	var parts []string

	if sc.RoutingWisdom != "" {
		parts = append(parts, "## Compiled Routing Wisdom\n\n"+sc.RoutingWisdom)
	}

	if len(sc.EntityPages) > 0 {
		var entityParts []string
		for _, ep := range sc.EntityPages {
			if ep.Content != "" {
				entityParts = append(entityParts, ep.Content)
			}
		}
		if len(entityParts) > 0 {
			parts = append(parts, "## Entity Context\n\n"+strings.Join(entityParts, "\n\n"))
		}
	}

	if len(sc.OpenTasks) > 0 {
		var taskLines []string
		for _, t := range sc.OpenTasks {
			taskLines = append(taskLines, fmt.Sprintf("- #%d [%s] %s (%s)", t.TaskID, t.PriorityTag(), t.Title, t.DisplayTags()))
		}
		parts = append(parts, "## Open Tasks\n\n"+strings.Join(taskLines, "\n"))
	}

	if len(parts) == 0 {
		return ""
	}
	return "# Brain Context\n\n" + strings.Join(parts, "\n\n")
}

func shortHash(s string) string {
	var h uint32
	for _, c := range s {
		h = h*31 + uint32(c)
	}
	return fmt.Sprintf("%04x", h&0xFFFF)
}

// autoCompileMeta tracks when the last brain compile ran.
type autoCompileMeta struct {
	LastCompile string `json:"last_compile"`
	LessonCount int    `json:"lesson_count"`
}

// autoCompileThreshold is the minimum number of new lessons since the last
// compile before auto-compile triggers. Set conservatively: compile is an
// LLM call, so we don't want it running on every query.
const autoCompileThreshold = 10

// ShouldAutoCompile returns true if enough lessons have accumulated since
// the last compile to justify re-compiling routing wisdom. It reads
// meta/last_compile.json from the brain directory and compares the lesson
// count at that time to the current count. Returns true when at least
// autoCompileThreshold new lessons have been recorded since last compile.
func (b *Brain) ShouldAutoCompile() bool {
	metaPath := filepath.Join(b.Path, "meta", "last_compile.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		// No meta file means never compiled - check if we have enough
		// lessons to justify an initial compile.
		total := b.DB.LessonCount()
		return total >= autoCompileThreshold
	}

	var meta autoCompileMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return true // corrupt meta, recompile
	}

	// Count lessons added since last compile
	newLessons := b.DB.LessonCountSince(meta.LastCompile)
	return newLessons >= autoCompileThreshold
}

// MarkCompiled records the current timestamp and lesson count so
// ShouldAutoCompile knows when the last compile happened.
func (b *Brain) MarkCompiled() error {
	meta := autoCompileMeta{
		LastCompile: time.Now().UTC().Format(time.RFC3339),
		LessonCount: b.DB.LessonCount(),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	metaPath := filepath.Join(b.Path, "meta", "last_compile.json")
	return os.WriteFile(metaPath, data, 0644)
}

// Stats returns brain statistics for display.
type Stats struct {
	LessonCount            int
	JarvisLessonCount      int
	JarvisLessonByFeedback map[string]int
	EntityCount            int
	TasksOpen              int
	TasksDone              int
	TasksByStatus          map[string]int
	LastLesson             string
	BrainPath              string
	DBSize                 int64
	HasEmbeddings          bool
	IsStale                bool
}

// GetStats returns current brain statistics.
func (b *Brain) GetStats() Stats {
	open, done := b.DB.TaskCount()
	byStatus, _ := b.DB.TaskCountByStatus()
	jarvisByFeedback, _ := b.DB.JarvisLessonCountByFeedback()
	s := Stats{
		LessonCount:            b.DB.LessonCount(),
		JarvisLessonCount:      b.DB.JarvisLessonCount(),
		JarvisLessonByFeedback: jarvisByFeedback,
		EntityCount:            b.DB.EntityCount(),
		TasksOpen:              open,
		TasksDone:              done,
		TasksByStatus:          byStatus,
		LastLesson:             b.DB.LastLessonTime(),
		BrainPath:              b.Path,
		HasEmbeddings:          b.Embeddings.Available(),
		IsStale:                IsStale(b.Path),
	}
	if info, err := os.Stat(filepath.Join(b.Path, dbFile)); err == nil {
		s.DBSize = info.Size()
	}
	return s
}

// FormatTaskCounts renders per-status counts as "N open, M in-progress, ..."
// in the canonical AllStatuses() order. Zero-count statuses are omitted.
// Unknown statuses (legacy rows) are appended at the end. Returns an empty
// string when there are no tasks at all.
func FormatTaskCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	var parts []string
	seen := make(map[string]bool)
	for _, status := range AllStatuses() {
		if n, ok := counts[status]; ok && n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, status))
			seen[status] = true
		}
	}
	for status, n := range counts {
		if seen[status] || n == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, status))
	}
	return strings.Join(parts, ", ")
}
