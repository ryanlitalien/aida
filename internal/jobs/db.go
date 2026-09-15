package jobs

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// openDB opens (or creates) the per-profile jobs.db. WAL mode and a
// 5s busy timeout match the brain.db pragmas at internal/brain/db.go:27
// so concurrent readers (web UI status fetch + a writer claiming a job)
// don't trip lock contention.
func openDB(profile string) (*sql.DB, error) {
	if profile == "" {
		return nil, fmt.Errorf("openDB: empty profile")
	}
	root := Root(profile)
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", root, err)
	}
	// Make the runs/ subdir at open-time too - Enqueue will create
	// the per-run dir but the parent must exist for atomic-rename
	// fsync semantics on the manifest write.
	if err := os.MkdirAll(filepath.Join(root, runsSubdirName), 0755); err != nil {
		return nil, fmt.Errorf("mkdir runs: %w", err)
	}

	dsn := DBPath(profile) + "?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open jobs.db: %w", err)
	}
	if err := migrate(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate jobs.db: %w", err)
	}
	return conn, nil
}

// migrate creates the schema. One CREATE TABLE block, idempotent
// indices, no version table - matches internal/brain/db.go:47-95.
// New columns added via ALTER TABLE ADD COLUMN with the duplicate-
// column error swallowed (also matching brain.db).
func migrate(conn *sql.DB) error {
	_, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS jobs (
			run_id          TEXT PRIMARY KEY,
			kind            TEXT NOT NULL DEFAULT 'draft',
			state           TEXT NOT NULL DEFAULT 'queued',
			task_slug       TEXT NOT NULL DEFAULT '',
			plan_id         TEXT NOT NULL DEFAULT '',
			profile         TEXT NOT NULL,
			claimed_by_host TEXT NOT NULL DEFAULT '',
			claimed_by_pid  INTEGER NOT NULL DEFAULT 0,
			enqueued_at     TEXT NOT NULL,
			started_at      TEXT NOT NULL DEFAULT '',
			finished_at     TEXT NOT NULL DEFAULT '',
			error           TEXT NOT NULL DEFAULT '',
			question        TEXT NOT NULL DEFAULT '',
			source_ref      TEXT NOT NULL DEFAULT '',
			awaiting_prompt TEXT NOT NULL DEFAULT '',
			notified_at     TEXT NOT NULL DEFAULT '',
			worktree_path   TEXT NOT NULL DEFAULT '',
			approval_action  TEXT NOT NULL DEFAULT '',
			approval_payload TEXT NOT NULL DEFAULT '',
			spend_cap_usd   REAL NOT NULL DEFAULT 0,
			timeout_sec     INTEGER NOT NULL DEFAULT 0,
			tool_allowlist  TEXT NOT NULL DEFAULT '',
			sandbox_tier    TEXT NOT NULL DEFAULT '',
			agent           TEXT NOT NULL DEFAULT '',
			model           TEXT NOT NULL DEFAULT '',
			artifact_url    TEXT NOT NULL DEFAULT '',
			destination     TEXT NOT NULL DEFAULT '',
			schema_version     INTEGER NOT NULL DEFAULT 0,
			termination_reason TEXT NOT NULL DEFAULT '',
			outcome            TEXT NOT NULL DEFAULT '',
			summary            TEXT NOT NULL DEFAULT '',
			failure_code       TEXT NOT NULL DEFAULT ''
		);

		CREATE INDEX IF NOT EXISTS idx_jobs_state_enqueued ON jobs(state, enqueued_at);
		CREATE INDEX IF NOT EXISTS idx_jobs_task_slug      ON jobs(task_slug) WHERE task_slug != '';
		CREATE INDEX IF NOT EXISTS idx_jobs_plan_id        ON jobs(plan_id)   WHERE plan_id != '';
		CREATE INDEX IF NOT EXISTS idx_jobs_state_finished ON jobs(state, finished_at);
	`)
	if err != nil {
		return err
	}
	// Idempotent ALTERs for pre-v2 databases. SQLite raises a
	// "duplicate column name" error when the column already exists;
	// swallow it so successive opens are no-ops.
	for _, alter := range []string{
		`ALTER TABLE jobs ADD COLUMN awaiting_prompt TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN notified_at     TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN worktree_path   TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN approval_action  TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN approval_payload TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN spend_cap_usd   REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN timeout_sec     INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN tool_allowlist  TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN sandbox_tier    TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN agent           TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN model           TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN artifact_url    TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN destination     TEXT NOT NULL DEFAULT ''`,
		// v8 result contract (Store.Finalize). A pre-existing row backfills
		// schema_version to 0, which is always below resultContractVersion,
		// exactly the "predates the contract" signal EffectiveState needs.
		`ALTER TABLE jobs ADD COLUMN schema_version     INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN termination_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN outcome            TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN summary            TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN failure_code       TEXT NOT NULL DEFAULT ''`,
	} {
		if _, aerr := conn.Exec(alter); aerr != nil {
			if !isDuplicateColumnErr(aerr) {
				return aerr
			}
		}
	}
	return nil
}

// isDuplicateColumnErr reports whether err is SQLite's
// "duplicate column name" complaint from an idempotent ALTER TABLE
// ADD COLUMN. Matches by substring because modernc.org/sqlite returns
// a wrapped error rather than a typed sentinel.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate column name") || strings.Contains(msg, "already exists")
}
