package brain

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const dbFile = "brain.db"

// DB wraps a SQLite connection to the brain index.
type DB struct {
	conn *sql.DB
	path string
}

// OpenDB opens (or creates) the brain.db file at brainPath.
func OpenDB(brainPath string) (*DB, error) {
	dbPath := filepath.Join(brainPath, dbFile)
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open brain.db: %w", err)
	}
	db := &DB{conn: conn, path: dbPath}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate brain.db: %w", err)
	}
	return db, nil
}

// Close closes the database connection.
func (db *DB) Close() error {
	if db.conn != nil {
		return db.conn.Close()
	}
	return nil
}

func (db *DB) migrate() error {
	_, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS lessons (
			id TEXT PRIMARY KEY,
			timestamp TEXT NOT NULL,
			profile TEXT NOT NULL DEFAULT '',
			question TEXT NOT NULL,
			question_embedding BLOB,
			action TEXT DEFAULT '',
			strategy TEXT DEFAULT '',
			sources_used TEXT DEFAULT '[]',
			per_source_status TEXT DEFAULT '{}',
			artifact_count INTEGER DEFAULT 0,
			quality INTEGER DEFAULT 0,
			quality_reason TEXT DEFAULT '',
			feedback TEXT DEFAULT '',
			feedback_reason TEXT DEFAULT '',
			answer_snippet TEXT DEFAULT '',
			routing_confidence TEXT DEFAULT '{}',
			result_confidence TEXT DEFAULT '{}',
			answer_confidence REAL DEFAULT 0,
			grounding TEXT DEFAULT '',
			source_contribs TEXT DEFAULT '{}'
		);

		CREATE TABLE IF NOT EXISTS routing_rules (
			id TEXT PRIMARY KEY,
			pattern TEXT NOT NULL,
			pattern_embedding BLOB,
			recommended_sources TEXT DEFAULT '[]',
			avoid_sources TEXT DEFAULT '[]',
			confidence REAL DEFAULT 0,
			evidence_count INTEGER DEFAULT 0,
			last_compiled TEXT DEFAULT ''
		);

		CREATE TABLE IF NOT EXISTS entities (
			slug TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			name TEXT NOT NULL,
			aliases TEXT DEFAULT '[]',
			page_path TEXT DEFAULT '',
			summary TEXT DEFAULT '',
			summary_embedding BLOB,
			last_indexed TEXT DEFAULT ''
		);

		CREATE INDEX IF NOT EXISTS idx_lessons_timestamp ON lessons(timestamp);
		CREATE INDEX IF NOT EXISTS idx_lessons_profile ON lessons(profile);`)
	if err != nil {
		return err
	}

	// Add executed_queries column if it doesn't exist (added after initial schema).
	db.conn.Exec(`ALTER TABLE lessons ADD COLUMN executed_queries TEXT DEFAULT '{}'`)

	// Phase 3.2 migration: feedback directive fields. Idempotent - ALTER
	// errors on duplicate column are ignored. Older lessons default to
	// empty string / empty JSON array, which the router handles as "no
	// preference" (no boost/demote applied).
	db.conn.Exec(`ALTER TABLE lessons ADD COLUMN feedback_intended_source TEXT DEFAULT ''`)
	db.conn.Exec(`ALTER TABLE lessons ADD COLUMN feedback_intended_sources TEXT DEFAULT '[]'`)
	db.conn.Exec(`ALTER TABLE lessons ADD COLUMN feedback_excluded_sources TEXT DEFAULT '[]'`)
	db.conn.Exec(`ALTER TABLE lessons ADD COLUMN feedback_failure_type TEXT DEFAULT ''`)
	// Part D migration: forward-looking output/format directives extracted
	// from --because text. Persisted as JSON array; older rows default to
	// '[]' (no directives). Synthesizer reads this on similar-lesson
	// lookup and renders each entry as a MUST-FOLLOW rule.
	db.conn.Exec(`ALTER TABLE lessons ADD COLUMN feedback_output_directives TEXT DEFAULT '[]'`)

	// Fix 4 migration: run_id links a lesson back to the aida run that
	// produced it, so an explicit thumbs-down can find and retract the
	// auto-recorded lesson for the SAME run instead of the two competing
	// in recall forever. superseded marks a retracted lesson row (still
	// kept for audit history, but excluded from every recall path).
	// routing_hint persists the one-line auto-routing summary that was
	// previously written to the lesson JSON file but never round-tripped
	// through brain.db -- without a column here, "clear routing_hint on
	// supersede" had no column to clear.
	db.conn.Exec(`ALTER TABLE lessons ADD COLUMN run_id TEXT DEFAULT ''`)
	db.conn.Exec(`ALTER TABLE lessons ADD COLUMN superseded INTEGER DEFAULT 0`)
	db.conn.Exec(`ALTER TABLE lessons ADD COLUMN routing_hint TEXT DEFAULT ''`)

	_, err = db.conn.Exec(`
		CREATE INDEX IF NOT EXISTS idx_entities_type ON entities(type);

		CREATE TABLE IF NOT EXISTS tasks (
			slug TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'open',
			completed INTEGER NOT NULL DEFAULT 0,
			tags TEXT DEFAULT '[]',
			created TEXT NOT NULL,
			page_path TEXT DEFAULT '',
			description TEXT DEFAULT '',
			issue_number INTEGER DEFAULT 0,
			completed_at TEXT DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
		CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created);

		CREATE TABLE IF NOT EXISTS task_seq (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			next_val INTEGER NOT NULL DEFAULT 1
		);
		INSERT OR IGNORE INTO task_seq (id, next_val) VALUES (1, 1);
	`)
	if err != nil {
		return err
	}

	// Add task_id column if missing (idempotent migration for existing DBs)
	var hasTaskID int
	db.conn.QueryRow("SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name='task_id'").Scan(&hasTaskID)
	if hasTaskID == 0 {
		if _, err := db.conn.Exec("ALTER TABLE tasks ADD COLUMN task_id INTEGER DEFAULT 0"); err != nil {
			return err
		}
		// Backfill: assign sequential IDs to existing tasks ordered by creation date
		rows, err := db.conn.Query("SELECT slug FROM tasks ORDER BY created ASC")
		if err != nil {
			return err
		}
		var slugs []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			slugs = append(slugs, s)
		}
		rows.Close()
		for i, s := range slugs {
			db.conn.Exec("UPDATE tasks SET task_id = ? WHERE slug = ?", i+1, s)
		}
		// Sync the counter
		db.conn.Exec("UPDATE task_seq SET next_val = (SELECT COALESCE(MAX(task_id), 0) + 1 FROM tasks) WHERE id = 1")
	}
	// Create unique index on task_id (only for non-zero values)
	db.conn.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_task_id ON tasks(task_id) WHERE task_id > 0")

	// Idempotent: add completed_at column if missing. Existing rows get NULL;
	// readers use COALESCE(completed_at, '') so NULL surfaces as the empty
	// string. Historical rows on Ryan's machine were backfilled via a
	// one-shot `aida brain backfill-completed-at` subcommand (since removed -
	// see git history) that walked the brain repo's git log.
	db.conn.Exec(`ALTER TABLE tasks ADD COLUMN completed_at TEXT DEFAULT ''`)

	// Action #1a: typed memory schema. Stores facts/events/instructions as
	// a unified table with supersession-by-key (active row per (type, key,
	// profile); older rows have superseded_by set to the newer record's id).
	// Tasks live in their own dedicated table - see TaskRecord above.
	_, err = db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS memory_records (
			id TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			key TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL,
			body_embedding BLOB,
			tags TEXT NOT NULL DEFAULT '[]',
			profile TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			created TEXT NOT NULL,
			superseded_by TEXT NOT NULL DEFAULT '',
			provenance TEXT NOT NULL DEFAULT '[]',
			confidence REAL NOT NULL DEFAULT 0,
			last_used_at TEXT NOT NULL DEFAULT '',
			use_count INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_memory_type ON memory_records(type);
		CREATE INDEX IF NOT EXISTS idx_memory_key ON memory_records(type, key, profile) WHERE key != '';
		CREATE INDEX IF NOT EXISTS idx_memory_active ON memory_records(superseded_by) WHERE superseded_by = '';
		CREATE INDEX IF NOT EXISTS idx_memory_created ON memory_records(created);
	`)
	if err != nil {
		return err
	}

	// Idempotent migration for pre-existing DBs: consolidation provenance +
	// confidence (Atlas pattern) and recall-practice columns (decay scoring).
	// ALTER errors when the column already exists - ignored by design,
	// matching the completed_at pattern above.
	db.conn.Exec(`ALTER TABLE memory_records ADD COLUMN provenance TEXT NOT NULL DEFAULT '[]'`)
	db.conn.Exec(`ALTER TABLE memory_records ADD COLUMN confidence REAL NOT NULL DEFAULT 0`)
	db.conn.Exec(`ALTER TABLE memory_records ADD COLUMN last_used_at TEXT NOT NULL DEFAULT ''`)
	db.conn.Exec(`ALTER TABLE memory_records ADD COLUMN use_count INTEGER NOT NULL DEFAULT 0`)

	// Action #1d: FTS5 corpus over lessons + memory_records, used as the
	// keyword channel in multi-channel retrieval. Standalone (not external-
	// content) so the index survives even if the underlying tables are
	// rebuilt; doc_id is the cross-table reference. Sync is via
	// upsertCorpusFTS / deleteCorpusFTS calls in InsertLesson and
	// InsertMemory; backfill is best-effort and idempotent.
	_, err = db.conn.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS corpus_fts USING fts5(
			doc_id UNINDEXED,
			doc_type UNINDEXED,
			body,
			tokenize = 'unicode61 remove_diacritics 2'
		);
	`)
	if err != nil {
		// FTS5 is best-effort. If the SQLite build doesn't support it,
		// SearchMulti falls back to vector + fact-key channels only.
		// No error propagation - log via the caller's UI layer instead.
	}

	// Phase C: wiki recall channel. wiki_pages mirrors wiki/*.md pages +
	// triaged_keep source notes from the separate aida-wiki repo (see
	// config.WikiPath / internal/brain/wiki_index.go). Files there are
	// the source of truth; this table -- and the FTS rows synced
	// alongside it into the shared corpus_fts table -- is a derived
	// index exactly like the rest of brain.db, safe to delete and
	// rebuild via `aida wiki index`. page_timestamp holds the page's own
	// frontmatter timestamp (OKF `timestamp:`, or a source note's
	// `updated`/`created`) -- the recall-decay anchor in SearchMulti;
	// indexed_at is purely the incremental-reindex watermark.
	_, err = db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS wiki_pages (
			slug TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			embedding BLOB,
			page_timestamp TEXT NOT NULL DEFAULT '',
			indexed_at TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_wiki_pages_indexed_at ON wiki_pages(indexed_at);
	`)
	if err != nil {
		return err
	}

	// Jarvis voice feedback. Separate from the engine `lessons` table
	// because the signals (tool selection, persona, brevity, standing
	// directives) don't fit engine-shaped columns. On-demand inserts only -
	// rows are written when the user rates a turn, not per turn.
	_, err = db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS jarvis_lessons (
			id TEXT PRIMARY KEY,
			timestamp TEXT NOT NULL,
			profile TEXT NOT NULL DEFAULT '',
			rated_turn_started_at TEXT NOT NULL,
			transcript TEXT NOT NULL,
			query TEXT NOT NULL,
			query_embedding BLOB,
			reply TEXT NOT NULL,
			tool_calls TEXT DEFAULT '[]',
			feedback TEXT NOT NULL,
			feedback_reason TEXT DEFAULT '',
			feedback_intended_tool TEXT DEFAULT '',
			feedback_avoid_tool TEXT DEFAULT '',
			feedback_style_directives TEXT DEFAULT '[]'
		);
		CREATE INDEX IF NOT EXISTS idx_jarvis_lessons_timestamp ON jarvis_lessons(timestamp);
		CREATE INDEX IF NOT EXISTS idx_jarvis_lessons_feedback ON jarvis_lessons(feedback);
	`)
	if err != nil {
		return err
	}

	// Tier-1 mined-question cache: populated by `aida brain mine-runs` (a
	// later phase), read by the query pipeline's Tier-1 lookup before Parse
	// (also a later phase) to answer a repeat question without a full
	// six-step pipeline run. `profile` exists for the same reason
	// lessons/memory_records/jarvis_lessons all have it: cross-profile
	// cache leakage would surface one profile's cached answer (which may
	// contain partner-specific data) in another profile's session.
	// `source_run_id` records which ~/.aida/runs/<id>.json run produced
	// this cache entry, for audit/debugging.
	_, err = db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS run_cache (
			id TEXT PRIMARY KEY,
			profile TEXT NOT NULL DEFAULT '',
			question TEXT NOT NULL,
			question_embedding BLOB,
			answer TEXT NOT NULL,
			sources_json TEXT NOT NULL DEFAULT '[]',
			confidence REAL NOT NULL DEFAULT 0,
			hit_count INTEGER NOT NULL DEFAULT 0,
			created TEXT NOT NULL,
			last_used TEXT NOT NULL DEFAULT '',
			source_run_id TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_run_cache_profile ON run_cache(profile);
		CREATE INDEX IF NOT EXISTS idx_run_cache_created ON run_cache(created);
	`)
	if err != nil {
		return err
	}

	return nil
}

// LessonRecord is a lesson stored in brain.db.
type LessonRecord struct {
	ID              string            `json:"id"`
	Timestamp       string            `json:"ts"`
	Profile         string            `json:"profile"`
	Question        string            `json:"question"`
	Embedding       []float32         `json:"-"`
	Action          string            `json:"action"`
	Strategy        string            `json:"strategy"`
	Sources         []string          `json:"sources"`
	PerSourceStatus map[string]string `json:"per_source_status"`
	ArtifactCount   int               `json:"artifact_count"`
	Quality         int               `json:"quality"`
	QualityReason   string            `json:"quality_reason"`
	Feedback        string            `json:"feedback"`
	FeedbackReason  string            `json:"feedback_reason"`
	// RunID matches the aida runs/<id>.json file this lesson was recorded
	// from. Lets feedback commands find and retract the auto-recorded
	// lesson for the same run instead of leaving two contradictory
	// lessons to compete in recall forever.
	RunID string `json:"run_id,omitempty"`
	// FeedbackIntendedSource is the legacy single-source directive. Kept
	// for backwards compat with lessons written before Phase 3.1. New
	// code should prefer FeedbackIntendedSources (plural).
	FeedbackIntendedSource   string             `json:"feedback_intended_source,omitempty"`
	FeedbackIntendedSources  []string           `json:"feedback_intended_sources,omitempty"`
	FeedbackExcludedSources  []string           `json:"feedback_excluded_sources,omitempty"`
	FeedbackFailureType      string             `json:"feedback_failure_type,omitempty"`
	FeedbackOutputDirectives []string           `json:"feedback_output_directives,omitempty"`
	AnswerSnippet            string             `json:"answer_snippet"`
	RoutingConfidence        map[string]float64 `json:"routing_confidence,omitempty"`
	ResultConfidence         map[string]float64 `json:"result_confidence,omitempty"`
	AnswerConfidence         float64            `json:"answer_confidence,omitempty"`
	Grounding                string             `json:"grounding,omitempty"`
	SourceContribs           map[string]float64 `json:"source_contribs,omitempty"`
	RoutingHint              string             `json:"routing_hint,omitempty"`
	ExecutedQueries          map[string]string  `json:"executed_queries,omitempty"`
	// Superseded marks a lesson retracted by a later, more authoritative
	// signal (typically explicit user feedback correcting the
	// auto-recorded lesson for the same run). Superseded lessons are
	// excluded from every recall path; the row and JSON file are kept
	// for audit history rather than deleted.
	Superseded bool `json:"superseded,omitempty"`
}

// upsertCorpusFTS keeps the FTS5 corpus row for a doc in sync with its
// owning table. Best-effort - failures are silently swallowed because
// FTS5 is an additive retrieval channel, not the source of truth.
func (db *DB) upsertCorpusFTS(docID, docType, body string) {
	if body == "" {
		return
	}
	db.conn.Exec(`DELETE FROM corpus_fts WHERE doc_id = ?`, docID)
	db.conn.Exec(`INSERT INTO corpus_fts (doc_id, doc_type, body) VALUES (?, ?, ?)`, docID, docType, body)
}

// deleteCorpusFTS removes a doc's FTS5 row. Used when a lesson or
// memory record is deleted or superseded.
func (db *DB) deleteCorpusFTS(docID string) {
	db.conn.Exec(`DELETE FROM corpus_fts WHERE doc_id = ?`, docID)
}

// InsertLesson writes a lesson record to brain.db.
func (db *DB) InsertLesson(l *LessonRecord) error {
	sources, _ := json.Marshal(l.Sources)
	perSource, _ := json.Marshal(l.PerSourceStatus)
	routeConf, _ := json.Marshal(l.RoutingConfidence)
	resultConf, _ := json.Marshal(l.ResultConfidence)
	sourceContribs, _ := json.Marshal(l.SourceContribs)
	executedQueries, _ := json.Marshal(l.ExecutedQueries)
	var embedding []byte
	if l.Embedding != nil {
		embedding = EncodeVector(l.Embedding)
	}

	intendedSources, _ := json.Marshal(l.FeedbackIntendedSources)
	excludedSources, _ := json.Marshal(l.FeedbackExcludedSources)
	outputDirectives, _ := json.Marshal(l.FeedbackOutputDirectives)

	superseded := 0
	if l.Superseded {
		superseded = 1
	}

	_, err := db.conn.Exec(`
		INSERT OR REPLACE INTO lessons
		(id, timestamp, profile, question, question_embedding, action, strategy,
		 sources_used, per_source_status, artifact_count, quality, quality_reason,
		 feedback, feedback_reason, answer_snippet,
		 routing_confidence, result_confidence, answer_confidence, grounding, source_contribs,
		 executed_queries,
		 feedback_intended_source, feedback_intended_sources, feedback_excluded_sources, feedback_failure_type,
		 feedback_output_directives,
		 run_id, superseded, routing_hint)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.Timestamp, l.Profile, l.Question, embedding,
		l.Action, l.Strategy, string(sources), string(perSource),
		l.ArtifactCount, l.Quality, l.QualityReason,
		l.Feedback, l.FeedbackReason, l.AnswerSnippet,
		string(routeConf), string(resultConf), l.AnswerConfidence, l.Grounding, string(sourceContribs),
		string(executedQueries),
		l.FeedbackIntendedSource, string(intendedSources), string(excludedSources), l.FeedbackFailureType,
		string(outputDirectives),
		l.RunID, superseded, l.RoutingHint,
	)
	if err == nil {
		// Sync FTS5 corpus. Index the question + answer snippet so both
		// match keyword queries.
		body := l.Question
		if l.AnswerSnippet != "" {
			body += "\n" + l.AnswerSnippet
		}
		db.upsertCorpusFTS("lesson:"+l.ID, "lesson", body)
	}
	return err
}

// SupersededLesson identifies a lesson row that SupersedeLessonsByRunID
// flagged, carrying its profile so the caller can locate and rewrite the
// corresponding JSON file under brain/lessons/<profile>/.
type SupersededLesson struct {
	ID      string
	Profile string
}

// SupersedeLessonByID flags a single lesson row as superseded: quality is
// zeroed and routing_hint cleared so a wrong-but-confident auto lesson
// stops reinforcing recall, and the lesson's FTS5 corpus row is removed
// (mirrors the memory_records supersession pattern - see SupersedeByID).
// Returns the lesson's profile, needed by the caller to rewrite its JSON
// file. Returns sql.ErrNoRows if no lesson has that id.
func (db *DB) SupersedeLessonByID(id string) (string, error) {
	var profile string
	if err := db.conn.QueryRow(`SELECT profile FROM lessons WHERE id = ?`, id).Scan(&profile); err != nil {
		return "", err
	}
	if _, err := db.conn.Exec(
		`UPDATE lessons SET superseded = 1, quality = 0, routing_hint = '' WHERE id = ?`,
		id,
	); err != nil {
		return profile, err
	}
	db.deleteCorpusFTS("lesson:" + id)
	return profile, nil
}

// SupersedeLessonsByRunID flags every non-superseded lesson row sharing
// runID as superseded, excluding exceptID (the feedback lesson that was
// just written for that same run - it must not retract itself). This is
// the fix for the real incident that motivated Fix 4: an auto-quality
// reviewer scores a wrong answer 4/5 and writes a reinforcing
// routing_hint lesson, then the user thumbs-downs the same run a minute
// later - without this, both lessons persist and compete in recall
// forever because neither record knew about the other's run_id.
func (db *DB) SupersedeLessonsByRunID(runID, exceptID string) ([]SupersededLesson, error) {
	if runID == "" {
		return nil, nil
	}
	rows, err := db.conn.Query(
		`SELECT id FROM lessons WHERE run_id = ? AND id != ? AND COALESCE(superseded, 0) = 0`,
		runID, exceptID,
	)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	var matched []SupersededLesson
	for _, id := range ids {
		profile, err := db.SupersedeLessonByID(id)
		if err != nil {
			continue
		}
		matched = append(matched, SupersededLesson{ID: id, Profile: profile})
	}
	return matched, nil
}

// SimilarLesson is a lesson with its similarity score.
type SimilarLesson struct {
	Lesson     LessonRecord
	Similarity float64
}

// FindSimilar returns the top-k lessons most similar to the query embedding.
// Falls back to Jaccard word overlap if embeddings are not available.
//
// profileIsolation, when non-empty, restricts results to lessons recorded
// under that profile (plus untagged-profile lessons from before the profile
// column existed). Cross-profile leakage was the symptom that motivated
// pushing the filter into the SQL layer rather than relying on per-call-site
// tag construction.
func (db *DB) FindSimilar(queryEmbedding []float32, k int, profileIsolation string) ([]SimilarLesson, error) {
	rows, err := db.conn.Query(`
		SELECT id, timestamp, profile, question, question_embedding, action, strategy,
		       sources_used, per_source_status, artifact_count, quality, quality_reason,
		       feedback, feedback_reason, answer_snippet,
		       routing_confidence, result_confidence, answer_confidence, grounding, source_contribs,
		       executed_queries,
		       COALESCE(feedback_intended_source, ''),
		       COALESCE(feedback_intended_sources, '[]'),
		       COALESCE(feedback_excluded_sources, '[]'),
		       COALESCE(feedback_failure_type, ''),
		       COALESCE(feedback_output_directives, '[]'),
		       COALESCE(run_id, ''),
		       COALESCE(routing_hint, '')
		FROM lessons
		WHERE question_embedding IS NOT NULL
		  AND COALESCE(superseded, 0) = 0
		  AND (? = '' OR profile = ? OR profile = '')
		ORDER BY timestamp DESC
		LIMIT 500`, profileIsolation, profileIsolation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SimilarLesson
	for rows.Next() {
		var l LessonRecord
		var embBlob []byte
		var sourcesJSON, perSourceJSON, routeConfJSON, resultConfJSON, sourceContribsJSON, executedQueriesJSON string
		var intendedSourcesJSON, excludedSourcesJSON, outputDirectivesJSON string

		if err := rows.Scan(
			&l.ID, &l.Timestamp, &l.Profile, &l.Question, &embBlob,
			&l.Action, &l.Strategy, &sourcesJSON, &perSourceJSON,
			&l.ArtifactCount, &l.Quality, &l.QualityReason,
			&l.Feedback, &l.FeedbackReason, &l.AnswerSnippet,
			&routeConfJSON, &resultConfJSON, &l.AnswerConfidence, &l.Grounding, &sourceContribsJSON,
			&executedQueriesJSON,
			&l.FeedbackIntendedSource, &intendedSourcesJSON, &excludedSourcesJSON, &l.FeedbackFailureType,
			&outputDirectivesJSON,
			&l.RunID, &l.RoutingHint,
		); err != nil {
			continue
		}
		json.Unmarshal([]byte(sourcesJSON), &l.Sources)
		json.Unmarshal([]byte(perSourceJSON), &l.PerSourceStatus)
		json.Unmarshal([]byte(routeConfJSON), &l.RoutingConfidence)
		json.Unmarshal([]byte(resultConfJSON), &l.ResultConfidence)
		json.Unmarshal([]byte(sourceContribsJSON), &l.SourceContribs)
		json.Unmarshal([]byte(executedQueriesJSON), &l.ExecutedQueries)
		json.Unmarshal([]byte(intendedSourcesJSON), &l.FeedbackIntendedSources)
		json.Unmarshal([]byte(excludedSourcesJSON), &l.FeedbackExcludedSources)
		json.Unmarshal([]byte(outputDirectivesJSON), &l.FeedbackOutputDirectives)

		if queryEmbedding == nil || len(embBlob) == 0 {
			continue
		}
		stored := DecodeVector(embBlob)
		sim := CosineSimilarity(queryEmbedding, stored)
		if sim > 0.15 {
			l.Embedding = stored
			results = append(results, SimilarLesson{Lesson: l, Similarity: sim})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})
	if len(results) > k {
		results = results[:k]
	}
	return results, nil
}

// EntityRecord is an entity stored in brain.db.
type EntityRecord struct {
	Slug      string    `json:"slug"`
	Type      string    `json:"type"`
	Name      string    `json:"name"`
	Aliases   []string  `json:"aliases"`
	PagePath  string    `json:"page_path"`
	Summary   string    `json:"summary"`
	Embedding []float32 `json:"-"`
}

// UpsertEntity inserts or updates an entity record.
func (db *DB) UpsertEntity(e *EntityRecord) error {
	aliases, _ := json.Marshal(e.Aliases)
	var embedding []byte
	if e.Embedding != nil {
		embedding = EncodeVector(e.Embedding)
	}
	_, err := db.conn.Exec(`
		INSERT OR REPLACE INTO entities (slug, type, name, aliases, page_path, summary, summary_embedding, last_indexed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Slug, e.Type, e.Name, string(aliases), e.PagePath, e.Summary, embedding,
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// ListEntitySlugs returns every entity slug in brain.db, across all
// types (partners, tools, people). Unlike FindEntities (which matches
// terms against name/aliases to look up a specific entity), this is for
// callers that want the full vocabulary of "things already tracked" --
// e.g. the meetily harvester's tag-vocabulary builder.
func (db *DB) ListEntitySlugs() ([]string, error) {
	rows, err := db.conn.Query(`SELECT slug FROM entities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			continue
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

// FindEntities searches for entities matching any of the given terms
// (checked against name and aliases).
func (db *DB) FindEntities(terms []string) ([]EntityRecord, error) {
	if len(terms) == 0 {
		return nil, nil
	}

	rows, err := db.conn.Query(`SELECT slug, type, name, aliases, page_path, summary FROM entities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var matches []EntityRecord
	for rows.Next() {
		var e EntityRecord
		var aliasesJSON string
		if err := rows.Scan(&e.Slug, &e.Type, &e.Name, &aliasesJSON, &e.PagePath, &e.Summary); err != nil {
			continue
		}
		json.Unmarshal([]byte(aliasesJSON), &e.Aliases)

		for _, term := range terms {
			if matchesEntity(term, e.Name, e.Aliases) {
				matches = append(matches, e)
				break
			}
		}
	}
	return matches, nil
}

func matchesEntity(term, name string, aliases []string) bool {
	term = normalize(term)
	if term == normalize(name) {
		return true
	}
	for _, a := range aliases {
		if term == normalize(a) {
			return true
		}
	}
	return false
}

func normalize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		if c == '_' {
			c = '-'
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			out = append(out, c)
		}
	}
	return string(out)
}

// FindTaskFeedbackLessons returns every lesson with action='task' that has a
// non-empty feedback verdict (thumbs-down or note), filtered by profile.
// Unlike FindSimilar this does not require embeddings - the caller is
// expected to apply its own similarity metric (e.g., Jaccard) since the
// set is small.
func (db *DB) FindTaskFeedbackLessons(profile string) ([]LessonRecord, error) {
	rows, err := db.conn.Query(`
		SELECT id, timestamp, profile, question, action, strategy,
		       feedback, feedback_reason, answer_snippet
		FROM lessons
		WHERE action = 'task'
		  AND feedback IN ('thumbs-down', 'note')
		  AND COALESCE(superseded, 0) = 0
		  AND (? = '' OR profile = ?)
		ORDER BY timestamp DESC
		LIMIT 200`, profile, profile)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []LessonRecord
	for rows.Next() {
		var l LessonRecord
		if err := rows.Scan(
			&l.ID, &l.Timestamp, &l.Profile, &l.Question,
			&l.Action, &l.Strategy, &l.Feedback, &l.FeedbackReason, &l.AnswerSnippet,
		); err != nil {
			continue
		}
		results = append(results, l)
	}
	return results, nil
}

// LessonCount returns the total number of lessons in the database.
func (db *DB) LessonCount() int {
	var count int
	db.conn.QueryRow("SELECT COUNT(*) FROM lessons").Scan(&count)
	return count
}

// LessonCountSince returns the number of lessons recorded after the given timestamp.
func (db *DB) LessonCountSince(since string) int {
	var count int
	db.conn.QueryRow("SELECT COUNT(*) FROM lessons WHERE timestamp > ?", since).Scan(&count)
	return count
}

// EntityCount returns the total number of entities in the database.
func (db *DB) EntityCount() int {
	var count int
	db.conn.QueryRow("SELECT COUNT(*) FROM entities").Scan(&count)
	return count
}

// LastLessonTime returns the timestamp of the most recent lesson.
func (db *DB) LastLessonTime() string {
	var ts string
	db.conn.QueryRow("SELECT timestamp FROM lessons ORDER BY timestamp DESC LIMIT 1").Scan(&ts)
	return ts
}

// TaskRecord is a task stored in brain.db.
type TaskRecord struct {
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	Completed   bool     `json:"completed"`
	Tags        []string `json:"tags"`
	Created     string   `json:"created"`
	CompletedAt string   `json:"completed_at,omitempty"`
	PagePath    string   `json:"page_path"`
	Description string   `json:"description"`
	IssueNumber int      `json:"issue_number,omitempty"`
	TaskID      int      `json:"task_id,omitempty"`
}

// PriorityTag returns the highest priority tag (p1 > p2 > p3), or "p3" if none.
func (t TaskRecord) PriorityTag() string {
	for _, tag := range t.Tags {
		if tag == "p1" {
			return "p1"
		}
	}
	for _, tag := range t.Tags {
		if tag == "p2" {
			return "p2"
		}
	}
	for _, tag := range t.Tags {
		if tag == "p3" {
			return "p3"
		}
	}
	return "p3"
}

// DisplayTags returns tags excluding priority tags (p1/p2/p3) for cleaner display.
func (t TaskRecord) DisplayTags() string {
	var out []string
	for _, tag := range t.Tags {
		if tag != "p1" && tag != "p2" && tag != "p3" {
			out = append(out, tag)
		}
	}
	return strings.Join(out, ", ")
}

// UpsertTask inserts or updates a task record.
// Preserves existing task_id when upserting with TaskID=0.
func (db *DB) UpsertTask(t *TaskRecord) error {
	tags, _ := json.Marshal(t.Tags)
	completed := 0
	if t.Completed {
		completed = 1
	}
	_, err := db.conn.Exec(`
		INSERT INTO tasks (slug, title, status, completed, tags, created, page_path, description, issue_number, task_id, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			title=excluded.title, status=excluded.status, completed=excluded.completed,
			tags=excluded.tags, created=excluded.created, page_path=excluded.page_path,
			description=excluded.description, issue_number=excluded.issue_number,
			completed_at=excluded.completed_at,
			task_id = CASE
				WHEN excluded.task_id > 0 THEN excluded.task_id
				WHEN tasks.task_id > 0 THEN tasks.task_id
				ELSE excluded.task_id
			END`,
		t.Slug, t.Title, t.Status, completed, string(tags), t.Created, t.PagePath, t.Description, t.IssueNumber, t.TaskID, t.CompletedAt,
	)
	if err != nil {
		return err
	}

	// Keep task_seq in sync: if the upserted task_id is >= next_val,
	// bump the sequence so NextTaskID never collides.
	if t.TaskID > 0 {
		db.conn.Exec(
			"UPDATE task_seq SET next_val = ? WHERE id = 1 AND next_val <= ?",
			t.TaskID+1, t.TaskID,
		)
	}
	return nil
}

// NextTaskID atomically allocates the next sequential task ID.
func (db *DB) NextTaskID() (int, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var id int
	if err := tx.QueryRow("SELECT next_val FROM task_seq WHERE id = 1").Scan(&id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("UPDATE task_seq SET next_val = ? WHERE id = 1", id+1); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// BumpTaskSeq raises task_seq.next_val to minNext, leaving it unchanged if
// it is already >= minNext. Used to re-seed the counter from what is on
// disk (see Brain.SyncTaskSeqFromDisk) without ever moving it backwards.
func (db *DB) BumpTaskSeq(minNext int) error {
	_, err := db.conn.Exec(
		"UPDATE task_seq SET next_val = ? WHERE id = 1 AND next_val < ?",
		minNext, minNext,
	)
	return err
}

// GetTaskByID returns a single task by its stable sequential ID.
func (db *DB) GetTaskByID(taskID int) (*TaskRecord, error) {
	var t TaskRecord
	var tagsJSON string
	var completed int
	var completedAt sql.NullString
	err := db.conn.QueryRow(
		"SELECT slug, title, status, completed, tags, created, page_path, description, issue_number, task_id, COALESCE(completed_at, '') FROM tasks WHERE task_id = ?",
		taskID,
	).Scan(&t.Slug, &t.Title, &t.Status, &completed, &tagsJSON, &t.Created, &t.PagePath, &t.Description, &t.IssueNumber, &t.TaskID, &completedAt)
	if err != nil {
		return nil, err
	}
	t.Completed = completed != 0
	t.CompletedAt = completedAt.String
	json.Unmarshal([]byte(tagsJSON), &t.Tags)
	return &t, nil
}

// ListTasks returns tasks filtered by completion status and tags.
// If days > 0, only returns tasks created within the last N days.
// If limit > 0, caps the result count.
//
// statuses, when non-empty, filters to exactly those status values and
// supersedes showCompleted. When empty, showCompleted=false includes all
// non-terminal statuses (open, in-progress, hold, deferred); showCompleted=true
// includes everything.
//
// profileIsolation, when non-empty, drops any task tagged `profile:X` where
// X != profileIsolation. Untagged tasks (no profile: tag at all) remain
// visible to every profile so that personal/general items predating the
// profile-tag convention aren't lost. Pass "" to disable isolation -
// reserved for explicit cross-profile views (`aida tasks --all-profiles`).
func (db *DB) ListTasks(showCompleted bool, tags []string, days, limit int, profileIsolation string, statuses []string) ([]TaskRecord, error) {
	query := "SELECT slug, title, status, completed, tags, created, page_path, description, issue_number, task_id, COALESCE(completed_at, '') FROM tasks"
	var conditions []string
	var args []interface{}

	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, s := range statuses {
			placeholders[i] = "?"
			args = append(args, s)
		}
		conditions = append(conditions, "status IN ("+strings.Join(placeholders, ",")+")")
	} else if !showCompleted {
		conditions = append(conditions, "completed = 0")
	}

	if days > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
		conditions = append(conditions, "created >= ?")
		args = append(args, cutoff)
	}

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	// Sort by priority (p1 first via tag check) then by created desc
	query += " ORDER BY created DESC"

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit*3) // over-fetch for tag filtering
	}

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []TaskRecord
	for rows.Next() {
		var t TaskRecord
		var tagsJSON string
		var completed int
		var completedAt sql.NullString
		if err := rows.Scan(&t.Slug, &t.Title, &t.Status, &completed, &tagsJSON, &t.Created, &t.PagePath, &t.Description, &t.IssueNumber, &t.TaskID, &completedAt); err != nil {
			continue
		}
		t.Completed = completed != 0
		t.CompletedAt = completedAt.String
		json.Unmarshal([]byte(tagsJSON), &t.Tags)

		// Hard profile isolation - applied before any other filter so a
		// buggy caller can't accidentally surface another profile's data.
		if profileIsolation != "" && hasForeignProfileTag(t.Tags, profileIsolation) {
			continue
		}

		// Tag filter: all specified tags must match (AND, substring)
		if len(tags) > 0 && !matchesTags(t.Tags, tags) {
			continue
		}
		results = append(results, t)
	}

	// Sort by priority then created
	sort.SliceStable(results, func(i, j int) bool {
		pi, pj := results[i].PriorityTag(), results[j].PriorityTag()
		if pi != pj {
			return pi < pj // p1 < p2 < p3, so p1 sorts first
		}
		return results[i].Created > results[j].Created
	})

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// hasForeignProfileTag reports true when tags include a `profile:X` entry
// where X is not the active profile. Tasks with no `profile:` tag at all
// return false (they're considered visible to every profile).
func hasForeignProfileTag(tags []string, activeProfile string) bool {
	want := "profile:" + activeProfile
	for _, t := range tags {
		if !strings.HasPrefix(t, "profile:") {
			continue
		}
		if t != want {
			return true
		}
	}
	return false
}

func matchesTags(taskTags, filterTags []string) bool {
	for _, filter := range filterTags {
		found := false
		filterLower := strings.ToLower(filter)
		for _, tag := range taskTags {
			if strings.Contains(strings.ToLower(tag), filterLower) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// GetTask returns a single task by exact slug.
func (db *DB) GetTask(slug string) (*TaskRecord, error) {
	var t TaskRecord
	var tagsJSON string
	var completed int
	var completedAt sql.NullString
	err := db.conn.QueryRow(
		"SELECT slug, title, status, completed, tags, created, page_path, description, issue_number, task_id, COALESCE(completed_at, '') FROM tasks WHERE slug = ?",
		slug,
	).Scan(&t.Slug, &t.Title, &t.Status, &completed, &tagsJSON, &t.Created, &t.PagePath, &t.Description, &t.IssueNumber, &t.TaskID, &completedAt)
	if err != nil {
		return nil, err
	}
	t.Completed = completed != 0
	t.CompletedAt = completedAt.String
	json.Unmarshal([]byte(tagsJSON), &t.Tags)
	return &t, nil
}

// FindTaskByPartialSlugAny finds the first task (open or done) whose slug contains the given substring.
func (db *DB) FindTaskByPartialSlugAny(partial string) (*TaskRecord, error) {
	return db.findTaskByPartialSlug(partial, false)
}

// FindTaskByPartialSlug finds the first open task whose slug contains the given substring.
func (db *DB) FindTaskByPartialSlug(partial string) (*TaskRecord, error) {
	return db.findTaskByPartialSlug(partial, true)
}

func (db *DB) findTaskByPartialSlug(partial string, openOnly bool) (*TaskRecord, error) {
	query := "SELECT slug, title, status, completed, tags, created, page_path, description, issue_number, task_id, COALESCE(completed_at, '') FROM tasks WHERE slug LIKE ?"
	if openOnly {
		query += " AND completed = 0"
	}
	rows, err := db.conn.Query(query, "%"+partial+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var matches []TaskRecord
	for rows.Next() {
		var t TaskRecord
		var tagsJSON string
		var completed int
		var completedAt sql.NullString
		if err := rows.Scan(&t.Slug, &t.Title, &t.Status, &completed, &tagsJSON, &t.Created, &t.PagePath, &t.Description, &t.IssueNumber, &t.TaskID, &completedAt); err != nil {
			continue
		}
		t.Completed = completed != 0
		t.CompletedAt = completedAt.String
		json.Unmarshal([]byte(tagsJSON), &t.Tags)
		matches = append(matches, t)
	}
	if len(matches) == 0 {
		if openOnly {
			return nil, fmt.Errorf("no open task matching %q", partial)
		}
		return nil, fmt.Errorf("no task matching %q", partial)
	}
	if len(matches) > 1 {
		var slugs []string
		for _, m := range matches {
			slugs = append(slugs, m.Slug)
		}
		return nil, fmt.Errorf("ambiguous: %d tasks match %q: %s", len(matches), partial, strings.Join(slugs, ", "))
	}
	return &matches[0], nil
}

// SetTaskStatus updates a task's status, the derived completed bit, and
// the completed_at timestamp. Pass completedAt as the empty string to clear
// (e.g., when reopening). Callers handle "preserve across terminal swap"
// at the brain layer; this function writes whatever timestamp it's given.
// Returns an error if status is not one of the canonical values.
func (db *DB) SetTaskStatus(slug, status, completedAt string) error {
	if !IsValidStatus(status) {
		return fmt.Errorf("invalid status %q", status)
	}
	completed := 0
	if IsTerminalStatus(status) {
		completed = 1
	}
	_, err := db.conn.Exec(
		"UPDATE tasks SET status = ?, completed = ?, completed_at = ? WHERE slug = ?",
		status, completed, completedAt, slug,
	)
	return err
}

// TaskCount returns the total number of tasks split between non-terminal
// (open + in-progress + hold + deferred) and terminal (done + closed).
func (db *DB) TaskCount() (open, done int) {
	db.conn.QueryRow("SELECT COUNT(*) FROM tasks WHERE completed = 0").Scan(&open)
	db.conn.QueryRow("SELECT COUNT(*) FROM tasks WHERE completed = 1").Scan(&done)
	return
}

// TaskCountByStatus returns task counts keyed by status string. Statuses
// with zero rows are omitted. Unknown status values (legacy data) are
// included verbatim - callers can decide whether to render them.
func (db *DB) TaskCountByStatus() (map[string]int, error) {
	rows, err := db.conn.Query("SELECT status, COUNT(*) FROM tasks GROUP BY status")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			continue
		}
		counts[status] = n
	}
	return counts, nil
}

// IsStale checks if brain.db needs a full rebuild. Returns true only when
// a significant number of lesson files are newer than brain.db (e.g. after
// a git pull that brings in many new lessons from another machine). Single
// new files from the current session are already inserted by RecordLesson
// and don't warrant a full 10+ second rebuild.
func IsStale(brainPath string) bool {
	dbPath := filepath.Join(brainPath, dbFile)
	info, err := os.Stat(dbPath)
	if err != nil {
		return true // missing = stale
	}
	dbModTime := info.ModTime()

	// Count lesson files newer than brain.db
	newerCount := 0
	lessonsDir := filepath.Join(brainPath, "lessons")
	entries, err := os.ReadDir(lessonsDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			subDir := filepath.Join(lessonsDir, entry.Name())
			subEntries, err := os.ReadDir(subDir)
			if err != nil {
				continue
			}
			for _, sub := range subEntries {
				if info, err := sub.Info(); err == nil && info.ModTime().After(dbModTime) {
					newerCount++
				}
			}
		}
	}

	// Also check entity/knowledge/task dirs for staleness
	for _, dir := range []string{"entities", "knowledge", "tasks"} {
		dirPath := filepath.Join(brainPath, dir)
		_ = filepath.WalkDir(dirPath, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, err := d.Info(); err == nil && info.ModTime().After(dbModTime) {
				newerCount++
			}
			return nil
		})
	}

	// Only trigger full rebuild when 3+ files are newer. 1-2 new files
	// are from the current session's RecordLesson which already inserts
	// into brain.db directly.
	return newerCount >= 3
}

// TouchDB updates brain.db's modification time to now, preventing
// IsStale from triggering another rebuild until new files arrive.
func TouchDB(brainPath string) {
	dbPath := filepath.Join(brainPath, dbFile)
	now := time.Now()
	_ = os.Chtimes(dbPath, now, now)
}
