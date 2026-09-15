package jobs

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrNoJobsAvailable is returned by Claim when the queue is empty
// (no rows in state=queued). Callers should treat this as a normal
// "nothing to do right now" signal, not an error.
var ErrNoJobsAvailable = errors.New("jobs: no queued jobs")

// Claim atomically transitions the oldest queued job to running and
// returns it. Mirrors brain.NextTaskID's BEGIN IMMEDIATE pattern at
// internal/brain/db.go:649: take a write lock at transaction start
// so two concurrent claimers serialize cleanly.
//
// On success: the SQL row is updated with hostname/pid/started_at,
// the manifest on disk is rewritten to reflect the new state, and
// a pid file is dropped at <run-dir>/pid for the reaper.
//
// Returns ErrNoJobsAvailable if the queue is empty.
func (s *Store) Claim(hostname string, pid int) (*Job, error) {
	if hostname == "" {
		return nil, fmt.Errorf("Claim: empty hostname")
	}
	if pid <= 0 {
		return nil, fmt.Errorf("Claim: invalid pid %d", pid)
	}

	tx, err := s.conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	// BEGIN IMMEDIATE behavior: modernc/sqlite issues a write lock on
	// first mutation in the tx. Force it now with a no-op UPDATE so
	// concurrent claimers can't both pick the same row between the
	// SELECT below and the UPDATE that follows.
	if _, err := tx.Exec(`UPDATE jobs SET kind=kind WHERE 0=1`); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("acquire write lock: %w", err)
	}

	row := tx.QueryRow(`
		SELECT run_id FROM jobs
		 WHERE state = ?
	  ORDER BY enqueued_at ASC
		 LIMIT 1`, StateQueued)
	var runID string
	if err := row.Scan(&runID); err != nil {
		tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoJobsAvailable
		}
		return nil, fmt.Errorf("select queued: %w", err)
	}

	now := time.Now().UTC()
	res, err := tx.Exec(`
		UPDATE jobs
		   SET state = ?, claimed_by_host = ?, claimed_by_pid = ?, started_at = ?
		 WHERE run_id = ? AND state = ?`,
		StateRunning, hostname, pid, now.Format(time.RFC3339),
		runID, StateQueued,
	)
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("update claim: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Lost the race despite the write lock - treat as queue
		// empty for this caller; the other claimer got it.
		tx.Rollback()
		return nil, ErrNoJobsAvailable
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}

	// SQL committed. Read the now-updated row, refresh the manifest
	// on disk, and drop the pid file.
	job, err := s.Get(runID)
	if err != nil {
		return nil, fmt.Errorf("get after claim: %w", err)
	}
	if err := WriteManifest(s.profile, jobToManifest(job)); err != nil {
		// SQL is the new source of truth temporarily - Reindex on
		// next start will reconcile. Surface the error so the
		// caller knows the manifest is stale.
		return job, fmt.Errorf("write manifest after claim: %w", err)
	}
	if err := writePIDFile(s.profile, runID, pid); err != nil {
		return job, fmt.Errorf("write pid file: %w", err)
	}
	return job, nil
}

// MarkRunning transitions a SPECIFIC row to state=running and records the
// executing host + pid, mirroring what Claim does for the oldest queued
// row. For run-dir agents spawned directly against a known row (voice
// job_start, web draft) where no worker Claim ever happens - without this
// the row reads "queued" for the whole run, /runs can't show liveness,
// the notifier has no transition to announce, and cancel has no pid to
// signal. Refuses terminal rows (a cancelled job's process racing to
// start must not resurrect the row).
func (s *Store) MarkRunning(runID, hostname string, pid int) error {
	if hostname == "" {
		return fmt.Errorf("MarkRunning: empty hostname")
	}
	if pid <= 0 {
		return fmt.Errorf("MarkRunning: invalid pid %d", pid)
	}
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	if IsTerminalState(j.State) {
		return fmt.Errorf("MarkRunning: job already %s", j.State)
	}
	j.State = StateRunning
	j.ClaimedByHost = hostname
	j.ClaimedByPID = pid
	j.StartedAt = time.Now().UTC()

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if _, err := s.conn.Exec(`
		UPDATE jobs
		   SET state = ?, claimed_by_host = ?, claimed_by_pid = ?, started_at = ?
		 WHERE run_id = ?`,
		j.State, j.ClaimedByHost, j.ClaimedByPID, j.StartedAt.Format(time.RFC3339), runID,
	); err != nil {
		return err
	}
	return writePIDFile(s.profile, runID, pid)
}

// writePIDFile drops <run-dir>/pid containing the worker pid as text.
// Used by Reap to detect dead workers; removed when the job reaches a
// terminal state.
func writePIDFile(profile, runID string, pid int) error {
	path := PIDPath(profile, runID)
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0644)
}

// removePIDFile deletes the per-run pid file. Best-effort: a missing
// file is fine (job may have been reaped or was never claimed).
func removePIDFile(profile, runID string) {
	_ = os.Remove(PIDPath(profile, runID))
}

// readPIDFile returns the pid recorded in <run-dir>/pid, or 0 when
// the file is absent or unreadable. Reap treats 0 as "no pid known."
func readPIDFile(profile, runID string) int {
	data, err := os.ReadFile(PIDPath(profile, runID))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// pidAlive reports whether a process with the given pid exists on
// the current machine. Uses kill(pid, 0) which delivers no signal
// but returns ESRCH when the process is gone. Cross-host pids are
// not checked here - Reap filters by hostname before calling.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds; the real check is the
	// signal. errno=ESRCH means the process is gone.
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return !errors.Is(err, os.ErrProcessDone) && !strings.Contains(err.Error(), "process already finished") && !strings.Contains(err.Error(), "no such process")
}

// Reap finds running jobs whose worker pid is no longer alive on
// this host and transitions them to failed. Returns the number of
// jobs reaped. Cross-host running rows (claimed_by_host != local
// hostname) are left alone - the only sane single-machine v1
// behavior; multi-machine is a future expansion via a heartbeat.
//
// Idempotent: safe to call repeatedly. A live worker's row is
// untouched; a dead worker is marked failed exactly once because
// the next call sees state=failed and skips.
func (s *Store) Reap() (int, error) {
	host, err := os.Hostname()
	if err != nil {
		return 0, fmt.Errorf("hostname: %w", err)
	}
	rows, err := s.conn.Query(`
		SELECT run_id, claimed_by_pid FROM jobs
		 WHERE state = ? AND claimed_by_host = ?`,
		StateRunning, host,
	)
	if err != nil {
		return 0, fmt.Errorf("select running: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		runID string
		pid   int
	}
	var dead []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.runID, &c.pid); err != nil {
			return 0, err
		}
		if c.pid > 0 && pidAlive(c.pid) {
			continue
		}
		// Either no pid recorded or process gone - treat as dead.
		dead = append(dead, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, c := range dead {
		msg := fmt.Sprintf("reaped: pid %d no longer alive on %s", c.pid, host)
		if err := s.Fail(c.runID, msg); err != nil {
			return 0, fmt.Errorf("fail %s: %w", c.runID, err)
		}
		removePIDFile(s.profile, c.runID)
	}
	return len(dead), nil
}

// Reindex walks the runs/ directory, parses every manifest.json, and
// upserts the corresponding row into jobs.db. Used after a DB-loss
// event (jobs.db deleted, corrupted, or a new machine cloning the
// aida config). Idempotent - INSERT OR REPLACE preserves the disk
// truth and overwrites any stale SQL state.
//
// Run dirs without a parseable manifest are skipped silently with a
// count returned in the second value (so the CLI can surface "Reindex:
// 14 manifests / 1 skipped"). Caller decides whether to investigate.
func (s *Store) Reindex() (count int, skipped int, err error) {
	dir := RunsDir(s.profile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("readdir %s: %w", dir, err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runID := e.Name()
		mPath := filepath.Join(dir, runID, "manifest.json")
		if _, statErr := os.Stat(mPath); statErr != nil {
			skipped++
			continue
		}
		m, readErr := ReadManifest(s.profile, runID)
		if readErr != nil || m == nil {
			skipped++
			continue
		}
		if m.Profile != s.profile {
			// Mismatched profile in a manifest - defensive guard.
			// Shouldn't happen because dirs are profile-scoped, but
			// catch it anyway so a misplaced file can't pollute.
			skipped++
			continue
		}
		j := manifestToJob(m)
		if upErr := s.upsertRow(j); upErr != nil {
			skipped++
			continue
		}
		count++
	}
	return count, skipped, nil
}

// upsertRow is INSERT OR REPLACE on the jobs table. Used by Reindex
// to make the DB match disk state regardless of what was there before.
// v1 manifests roundtrip cleanly: the awaiting_prompt/notified_at
// fields are zero strings in their decoded Job, and the SQL column
// defaults are also ” - so a v1-only Reindex pass produces v2 rows
// with the expected empties.
func (s *Store) upsertRow(j *Job) error {
	_, err := s.conn.Exec(`
		INSERT OR REPLACE INTO jobs (
			run_id, kind, state, task_slug, plan_id, profile,
			claimed_by_host, claimed_by_pid,
			enqueued_at, started_at, finished_at,
			error, question, source_ref,
			awaiting_prompt, notified_at, worktree_path
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.RunID, j.Kind, j.State, j.TaskSlug, j.PlanID, j.Profile,
		j.ClaimedByHost, j.ClaimedByPID,
		fmtTime(j.EnqueuedAt), fmtTime(j.StartedAt), fmtTime(j.FinishedAt),
		j.Error, j.Question, j.SourceRef,
		j.AwaitingPrompt, j.NotifiedAt, j.WorktreePath,
	)
	return err
}
