// Package jobs implements a per-profile, SQLite-backed agent job queue.
//
// One profile gets one ~/.aida/jobs/<profile>/ directory containing a
// SQLite index (jobs.db) and a runs/ subdirectory of per-job artifacts.
// Profile isolation is physical - separate DB files per profile rather
// than a profile column on a shared DB - so a forgotten WHERE clause
// can never leak across profiles. Per-machine because claim semantics
// (pid liveness, host-local processes) don't carry across machines.
//
// Source of truth for any one job is its manifest.json; jobs.db is a
// derived index, rebuildable via Reindex. Same pattern as brain.db.
package jobs

import (
	"path/filepath"

	"github.com/ryanlitalien/aida/internal/config"
)

// jobsDirName is the top-level directory under ~/.aida/ that holds
// every profile's queue. Kept distinct from internal/runs/'s
// ~/.aida/runs/ (which records synthesizer Run JSON files).
const jobsDirName = "jobs"

// dbFileName is the SQLite file inside each profile dir. Bare
// "jobs.db" rather than profile-prefixed because the parent directory
// already disambiguates.
const dbFileName = "jobs.db"

// runsSubdirName holds one subdirectory per job, keyed by run id.
const runsSubdirName = "runs"

// Root returns ~/.aida/jobs/<profile>/ - the per-profile queue dir.
// Profile must be non-empty; an empty profile would collide with the
// "no profile" default and is rejected at Open time.
func Root(profile string) string {
	return filepath.Join(config.Dir(), jobsDirName, profile)
}

// RunsDir returns the per-profile runs subdirectory holding one
// subdir per job.
func RunsDir(profile string) string {
	return filepath.Join(Root(profile), runsSubdirName)
}

// RunDir returns the absolute path to a single job's run directory.
func RunDir(profile, runID string) string {
	return filepath.Join(RunsDir(profile), runID)
}

// ManifestPath returns the source-of-truth JSON path for one job.
func ManifestPath(profile, runID string) string {
	return filepath.Join(RunDir(profile, runID), "manifest.json")
}

// EventsPath returns the append-only NDJSON event log path for one job.
func EventsPath(profile, runID string) string {
	return filepath.Join(RunDir(profile, runID), "events.ndjson")
}

// OutputPath returns the final-answer markdown path for one job.
// Written only on successful completion.
func OutputPath(profile, runID string) string {
	return filepath.Join(RunDir(profile, runID), "output.md")
}

// PIDPath returns the path to the worker pid file. Present while
// the job is running; removed when the job reaches a terminal state.
func PIDPath(profile, runID string) string {
	return filepath.Join(RunDir(profile, runID), "pid")
}

// StderrPath returns the captured-stderr path for one job (debug aid).
func StderrPath(profile, runID string) string {
	return filepath.Join(RunDir(profile, runID), "stderr.log")
}

// DBPath returns the SQLite index path for the profile.
func DBPath(profile string) string {
	return filepath.Join(Root(profile), dbFileName)
}
