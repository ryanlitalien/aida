package arbiter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/burndown"
)

// WindowSnapshot is one window's used-percent reading, captured before
// and after a run for Entry's ledger row.
type WindowSnapshot struct {
	Provider string  `json:"provider"`
	Label    string  `json:"label"`
	UsedPct  float64 `json:"used_pct"`
}

// Entry is one ledger row -- docs/arbiter-plan.md section 7: "every run
// appends one JSON line: task, wave id, correlation id, lane, model,
// effort, window percent before and after per bar, tokens, wall time,
// verifier result, PR URL, cost for metered lanes."
type Entry struct {
	At       time.Time        `json:"at"`
	TaskSlug string           `json:"task_slug"`
	TaskID   int              `json:"task_id"`
	Lane     string           `json:"lane"`
	Model    string           `json:"model"`
	Role     string           `json:"role"`
	Host     string           `json:"host"`
	RunID    string           `json:"run_id"`
	Attempt  int              `json:"attempt"`
	WallMS   int64            `json:"wall_ms"`
	Verdict  string           `json:"verdict"`
	PRURL    string           `json:"pr_url,omitempty"`
	CostUSD  float64          `json:"cost_usd,omitempty"`
	Before   []WindowSnapshot `json:"before,omitempty"`
	After    []WindowSnapshot `json:"after,omitempty"`
	Note     string           `json:"note,omitempty"`
}

// SnapshotFor extracts the WindowSnapshot rows relevant to lane from caps
// -- the same Provider/Windows-or-Spend filter LaneCapacityFor applies,
// so a ledger entry's Before/After reads like the lane's own dashboard
// slice, not the whole provider's row set.
func SnapshotFor(lane Lane, caps []burndown.Capacity) []WindowSnapshot {
	lc := LaneCapacityFor(lane, caps)
	out := make([]WindowSnapshot, 0, len(lc.Windows))
	for _, c := range lc.Windows {
		out = append(out, WindowSnapshot{Provider: c.Provider, Label: c.Label, UsedPct: c.UsedPct})
	}
	return out
}

// AppendLedger appends e as one JSON line to the ledger at path, creating
// its parent directory if needed. Like Store.Record, this is append-only
// -- a run's history is never rewritten, only added to.
func AppendLedger(path string, e Entry) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("arbiter: creating ledger dir for %q: %w", path, err)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("arbiter: marshaling ledger entry: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("arbiter: opening ledger %q: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("arbiter: writing ledger entry: %w", err)
	}
	return nil
}

// ReadLedger returns every entry recorded at path, in file order,
// tolerating (skipping) any corrupt line the same way Store.ReadAll does
// -- a morning summary over months of runs must survive one bad line, not
// refuse to render at all.
func ReadLedger(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("arbiter: reading ledger %q: %w", path, err)
	}
	var out []Entry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
