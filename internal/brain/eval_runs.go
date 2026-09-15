package brain

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/eval"
)

// evalRunsDirName is the directory under the brain repo that holds
// per-invocation eval records. Source of truth for the eval signal -
// brain.db (when we add an indexed view) is rebuilt from these files.
//
// Files live in the brain git repo (NOT gitignored), so they sync
// between machines via the existing brain auto-commit/pull pattern.
// One JSON file per run keeps writes append-only and conflict-free
// when home and work machines run aida concurrently.
const evalRunsDirName = "eval-runs"

// EvalRun is the durable record of one synthesized answer's reviewer
// pass. Cross-referenced to the local-only runs.Run by RunID.
type EvalRun struct {
	RunID            string              `json:"run_id"`
	Timestamp        time.Time           `json:"timestamp"`
	Question         string              `json:"question"`
	Model            string              `json:"model,omitempty"`
	Profile          string              `json:"profile,omitempty"`
	AggregateVerdict string              `json:"aggregate_verdict"`
	Reviewers        []eval.ReviewRecord `json:"reviewers"`
}

// EvalRunsDir returns the absolute path to the eval-runs directory
// for a given brain root. Caller is responsible for ensuring the
// brain root exists; this function does not create it.
func EvalRunsDir(brainPath string) string {
	return filepath.Join(brainPath, evalRunsDirName)
}

// WriteEvalRun persists an EvalRun to the brain's eval-runs/ directory
// as <run_id>.json. Creates the directory on first write. Records
// missing a RunID return an error rather than silently picking one,
// because the caller's run id is the cross-reference key.
func WriteEvalRun(brainPath string, r *EvalRun) error {
	if r == nil {
		return fmt.Errorf("WriteEvalRun: nil record")
	}
	if r.RunID == "" {
		return fmt.Errorf("WriteEvalRun: empty RunID")
	}
	dir := EvalRunsDir(brainPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal eval run: %w", err)
	}
	path := filepath.Join(dir, r.RunID+".json")
	return os.WriteFile(path, data, 0644)
}

// ReadEvalRun reads a single EvalRun by run id. Returns os.ErrNotExist
// (wrapped) when the file is missing, so callers can distinguish
// "no record yet" from a real read failure.
func ReadEvalRun(brainPath, runID string) (*EvalRun, error) {
	if runID == "" {
		return nil, fmt.Errorf("ReadEvalRun: empty runID")
	}
	path := filepath.Join(EvalRunsDir(brainPath), runID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r EvalRun
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return &r, nil
}

// RecentFailedReviewsForEntities returns flattened ReviewRecords
// from recent failed eval-runs whose stored Question contains any
// of the given entity strings (case-insensitive substring match).
//
// This is the router-side consumer of the eval signal: pass in
// the parsed entities for the current question, get back a list
// of past reviewer records that flagged failures on similar
// questions. Hand the result to eval.FailureBoosts to convert
// into per-source score adjustments.
//
// lookback caps how many of the most-recent eval-runs to scan
// (regardless of match). Empty entities returns no records -
// without an entity filter we'd apply cross-domain failure
// signal to unrelated questions.
func RecentFailedReviewsForEntities(brainPath string, entities []string, lookback int) ([]eval.ReviewRecord, error) {
	if len(entities) == 0 {
		return nil, nil
	}
	runs, err := ListRecentEvalRuns(brainPath, lookback)
	if err != nil {
		return nil, err
	}
	var out []eval.ReviewRecord
	for _, r := range runs {
		if r.AggregateVerdict != "fail" {
			continue
		}
		if !questionMatchesAnyEntity(r.Question, entities) {
			continue
		}
		out = append(out, r.Reviewers...)
	}
	return out, nil
}

// questionMatchesAnyEntity is the v1 similarity heuristic for
// eval-runs: case-insensitive substring match between the past
// question and any of the current question's entities.
//
// Embedding-based similarity (matching the lessons path) is a
// follow-up - for now this is good enough because the router
// only uses these records for small ±25 adjustments, and false
// positives just cost a single nudge.
func questionMatchesAnyEntity(question string, entities []string) bool {
	if len(entities) == 0 {
		return false
	}
	q := strings.ToLower(question)
	for _, e := range entities {
		if e == "" {
			continue
		}
		if strings.Contains(q, strings.ToLower(e)) {
			return true
		}
	}
	return false
}

// ListRecentEvalRuns returns up to `limit` EvalRuns ordered newest
// first by RunID (which embeds a UTC timestamp prefix per
// runs.NewID). Malformed or unreadable files are skipped quietly,
// because a single bad record must not break the router boost
// that consumes this list.
//
// limit <= 0 means "no limit" - return everything available.
func ListRecentEvalRuns(brainPath string, limit int) ([]EvalRun, error) {
	dir := EvalRunsDir(brainPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".json"))
	}
	// Run IDs sort lexically newest-first when reversed because of the
	// "20060102-150405-…" timestamp prefix used by runs.NewID.
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	var out []EvalRun
	for _, id := range ids {
		if limit > 0 && len(out) >= limit {
			break
		}
		r, err := ReadEvalRun(brainPath, id)
		if err != nil || r == nil {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}
