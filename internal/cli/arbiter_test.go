package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/arbiter"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/burndown"
	"github.com/ryanlitalien/aida/internal/taskstate"
)

// ---- lanes table ----

func TestArbiterRenderLanesTable_DefaultConfig(t *testing.T) {
	got := renderLanesTable(arbiter.DefaultConfig().Sorted())

	for _, want := range []string{"claude-personal", "claude-company", "gemini", "codex", "proxy-model", "litellm"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderLanesTable missing lane %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "ORDER") || !strings.Contains(got, "EXECUTOR") {
		t.Errorf("renderLanesTable missing header columns in:\n%s", got)
	}
	// proxy-model is the one allow_unprobed lane in the default roster.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "proxy-model") && !strings.Contains(line, "yes") {
			t.Errorf("proxy-model row missing 'yes' (allow_unprobed) in: %q", line)
		}
	}
}

func TestArbiterRenderLanesTable_RolesAllVsRestricted(t *testing.T) {
	lanes := []arbiter.Lane{
		{ID: "a", Order: 1, Runner: arbiter.RunnerClaude, Auth: arbiter.AuthSubscription,
			DataClasses: []arbiter.DataClass{arbiter.DataClassPersonal}, Models: map[string]string{arbiter.RoleExecutor: "model-a"}},
		{ID: "b", Order: 2, Runner: arbiter.RunnerExec, Auth: arbiter.AuthSubscription,
			DataClasses: []arbiter.DataClass{arbiter.DataClassPersonal}, Roles: []string{arbiter.RoleThinker},
			Models: map[string]string{arbiter.RoleExecutor: "model-b"}},
	}
	got := renderLanesTable(lanes)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + 2 rows, got %d lines:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[1], "all") {
		t.Errorf("lane a (no Roles) should render roles as 'all': %q", lines[1])
	}
	if !strings.Contains(lines[2], arbiter.RoleThinker) {
		t.Errorf("lane b should render its restricted role: %q", lines[2])
	}
}

// ---- required lane roster (2026-09-25 decision) ----

func TestLoadLanesConfigMissingFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lanes.yaml")
	_, err := loadLanesConfig(path)
	if err == nil {
		t.Fatal("loadLanesConfig(missing file) = nil error, want an error naming the path")
	}
	if !strings.Contains(err.Error(), "no lane roster at "+path) {
		t.Errorf("error = %q, want it to name the missing path", err)
	}
	if !strings.Contains(err.Error(), "examples/lanes.yaml") {
		t.Errorf("error = %q, want it to point at examples/lanes.yaml", err)
	}
}

func TestLoadLanesConfigMalformedFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lanes.yaml")
	if err := os.WriteFile(path, []byte("not: [valid: yaml"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := loadLanesConfig(path); err == nil {
		t.Fatal("loadLanesConfig(malformed file) = nil error, want a parse error")
	}
}

// ---- plan row builder ----

func mustDefaultConfig(t *testing.T) *arbiter.Config {
	t.Helper()
	cfg := arbiter.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() = %v, want nil", err)
	}
	return cfg
}

// fullHeadroomCaps returns a canned []burndown.Capacity giving every
// provider in the default lane roster full headroom, so Pick's only
// discriminator left is the task's data class.
func fullHeadroomCaps() []burndown.Capacity {
	mk := func(provider, label string) burndown.Capacity {
		return burndown.Capacity{Provider: provider, Label: label, UsedPct: 10, LeftPct: 90, Headroom: 90}
	}
	var caps []burndown.Capacity
	for _, p := range []string{"Anthropic / Claude", "Anthropic / Claude (company seat)", "Google / Gemini", "OpenAI / ChatGPT + Codex", "LiteLLM proxy"} {
		caps = append(caps, mk(p, "5-hour"), mk(p, "7-day"))
	}
	return caps
}

func TestArbiterBuildPlanRow_PersonalTaskLandsOnClaudePersonal(t *testing.T) {
	cfg := mustDefaultConfig(t)
	task := brain.TaskRecord{TaskID: 1, Slug: "personal-task", Tags: []string{"personal"}}
	row := buildPlanRow(cfg, task, arbiter.RoleExecutor, fullHeadroomCaps(), nil, time.Now())

	if row.Class != string(arbiter.DataClassPersonal) {
		t.Errorf("Class = %q, want %q", row.Class, arbiter.DataClassPersonal)
	}
	if row.Lane != "claude-personal" {
		t.Errorf("Lane = %q, want claude-personal (got reason %q)", row.Lane, row.Reason)
	}
	if row.Model == "" {
		t.Errorf("Model is empty for a winning row")
	}
}

func TestArbiterBuildPlanRow_ButterstackTaskLandsOnClaudeCompany(t *testing.T) {
	cfg := mustDefaultConfig(t)
	task := brain.TaskRecord{TaskID: 2, Slug: "bs-task", Tags: []string{"project:butterstack"}}
	row := buildPlanRow(cfg, task, arbiter.RoleExecutor, fullHeadroomCaps(), nil, time.Now())

	if row.Class != string(arbiter.DataClassButterstack) {
		t.Errorf("Class = %q, want %q", row.Class, arbiter.DataClassButterstack)
	}
	if row.Lane != "claude-company" {
		t.Errorf("Lane = %q, want claude-company (got reason %q)", row.Lane, row.Reason)
	}
}

func TestArbiterBuildPlanRow_ConflictingTagsIsCONFLICT(t *testing.T) {
	cfg := mustDefaultConfig(t)
	task := brain.TaskRecord{TaskID: 3, Slug: "bad-task", Tags: []string{"personal", "project:butterstack"}}
	row := buildPlanRow(cfg, task, arbiter.RoleExecutor, fullHeadroomCaps(), nil, time.Now())

	if row.Class != "CONFLICT" {
		t.Errorf("Class = %q, want CONFLICT", row.Class)
	}
	if row.Lane != "none" {
		t.Errorf("Lane = %q, want none for a conflict row", row.Lane)
	}
	if row.Reason == "" {
		t.Errorf("Reason is empty for a conflict row")
	}
}

func TestArbiterBuildPlanRow_NoEligibleLaneJoinsRejections(t *testing.T) {
	cfg := mustDefaultConfig(t)
	task := brain.TaskRecord{TaskID: 4, Slug: "no-headroom", Tags: []string{"personal"}}
	// No capacity rows at all, and none of the personal-eligible lanes
	// (claude-personal, gemini, codex) allow running unprobed -- every
	// one of them should be rejected.
	row := buildPlanRow(cfg, task, arbiter.RoleExecutor, nil, nil, time.Now())

	if row.Lane != "none" {
		t.Errorf("Lane = %q, want none", row.Lane)
	}
	if !strings.Contains(row.Reason, "claude-personal") {
		t.Errorf("Reason = %q, want it to mention the rejected claude-personal lane", row.Reason)
	}
	if !strings.Contains(row.Reason, "; ") && strings.Count(row.Reason, ":") < 2 {
		t.Errorf("Reason = %q, want multiple rejections joined", row.Reason)
	}
}

// ---- plan footer ----

func TestArbiterPlanFooter_CountsPerLaneAndNoLane(t *testing.T) {
	rows := []planRow{
		{Lane: "claude-max"},
		{Lane: "claude-max"},
		{Lane: "claude-pro-bs"},
		{Lane: "none"},
	}
	got := planFooter(rows)
	if !strings.Contains(got, "4 task(s)") {
		t.Errorf("planFooter = %q, want a total of 4 tasks", got)
	}
	if !strings.Contains(got, "claude-max: 2") {
		t.Errorf("planFooter = %q, want claude-max: 2", got)
	}
	if !strings.Contains(got, "claude-pro-bs: 1") {
		t.Errorf("planFooter = %q, want claude-pro-bs: 1", got)
	}
	if !strings.Contains(got, "no lane: 1") {
		t.Errorf("planFooter = %q, want no lane: 1", got)
	}
}

// ---- misc plan helpers ----

func TestArbiterFormatRejections_Empty(t *testing.T) {
	if got := formatRejections(nil); got != "no lanes configured" {
		t.Errorf("formatRejections(nil) = %q, want the no-lanes-configured message", got)
	}
}

func TestArbiterFormatRejections_JoinsWithSemicolon(t *testing.T) {
	rejs := []arbiter.Rejection{
		{Lane: "claude-max", Reason: "signalled empty"},
		{Lane: "codex-plus", Reason: "unprobed lane without allow_unprobed"},
	}
	got := formatRejections(rejs)
	if !strings.Contains(got, "claude-max: signalled empty") || !strings.Contains(got, "codex-plus: unprobed lane") {
		t.Errorf("formatRejections = %q, missing an expected lane reason", got)
	}
	if !strings.Contains(got, "; ") {
		t.Errorf("formatRejections = %q, want entries joined with '; '", got)
	}
}

// ---- signal summary line ----

func TestArbiterFormatSignalLine_Empty(t *testing.T) {
	until := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	got := formatSignalLine("claude-max", arbiter.VerdictEmpty, "usage limit reached", until)
	if !strings.Contains(got, "lane claude-max") || !strings.Contains(got, "empty") {
		t.Errorf("formatSignalLine = %q, want lane + empty verdict", got)
	}
	if !strings.Contains(got, `"usage limit reached"`) {
		t.Errorf("formatSignalLine = %q, want the matched phrase quoted", got)
	}
	if !strings.Contains(got, until.Format(time.RFC3339)) {
		t.Errorf("formatSignalLine = %q, want the empty-until timestamp", got)
	}
}

func TestArbiterFormatSignalLine_Ambiguous(t *testing.T) {
	until := time.Now().Add(time.Hour)
	got := formatSignalLine("codex-plus", arbiter.VerdictAmbiguous, "", until)
	if !strings.Contains(got, "ambiguous") {
		t.Errorf("formatSignalLine = %q, want the ambiguous verdict", got)
	}
	if !strings.Contains(got, "empty until") {
		t.Errorf("formatSignalLine = %q, want an empty-until mark (fail open)", got)
	}
}

func TestArbiterFormatSignalLine_NotEmpty(t *testing.T) {
	got := formatSignalLine("gemini-agy", arbiter.VerdictNotEmpty, "", time.Time{})
	if got != "lane gemini-agy: not-empty, no mark" {
		t.Errorf("formatSignalLine = %q, want the exact not-empty form", got)
	}
}

// ---- signals footer ----

func TestArbiterSignalsFooter_Math(t *testing.T) {
	got := signalsFooter(4, 1)
	if !strings.Contains(got, "4 signals") || !strings.Contains(got, "1 ambiguous") {
		t.Errorf("signalsFooter = %q, want counts of 4 and 1", got)
	}
	if !strings.Contains(got, "25.0%") {
		t.Errorf("signalsFooter = %q, want a 25%% ambiguous rate", got)
	}
}

func TestArbiterSignalsFooter_Zero(t *testing.T) {
	got := signalsFooter(0, 0)
	if !strings.Contains(got, "0 signals") || !strings.Contains(got, "n/a") {
		t.Errorf("signalsFooter = %q, want a zero-signal, n/a-rate form", got)
	}
}

func TestArbiterFilterSignalsSince(t *testing.T) {
	now := time.Now()
	sigs := []arbiter.Signal{
		{At: now.Add(-48 * time.Hour), Lane: "old"},
		{At: now.Add(-1 * time.Hour), Lane: "recent"},
	}
	got := filterSignalsSince(sigs, now.Add(-24*time.Hour))
	if len(got) != 1 || got[0].Lane != "recent" {
		t.Errorf("filterSignalsSince = %+v, want only the recent signal", got)
	}
	if all := filterSignalsSince(sigs, time.Time{}); len(all) != 2 {
		t.Errorf("filterSignalsSince with zero cutoff = %+v, want all signals", all)
	}
}

// fakeSignalReader is a minimal arbiter.SignalReader for testing
// renderLaneMarks without a real on-disk Store.
type fakeSignalReader map[string]time.Time

func (f fakeSignalReader) EmptyUntil(lane string, now time.Time) (time.Time, bool) {
	until, ok := f[lane]
	if !ok || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}

func TestArbiterRenderLaneMarks(t *testing.T) {
	now := time.Now()
	lanes := []arbiter.Lane{{ID: "claude-max"}, {ID: "codex-plus"}}
	marks := fakeSignalReader{"claude-max": now.Add(time.Hour)}

	got := renderLaneMarks(lanes, marks, now)
	if !strings.Contains(got, "claude-max") {
		t.Errorf("renderLaneMarks = %q, want claude-max listed", got)
	}
	if strings.Contains(got, "codex-plus") {
		t.Errorf("renderLaneMarks = %q, want codex-plus NOT listed (no mark)", got)
	}

	none := renderLaneMarks(lanes, fakeSignalReader{}, now)
	if !strings.Contains(none, "No lanes currently marked empty") {
		t.Errorf("renderLaneMarks with no marks = %q, want the empty-state message", none)
	}
}

// ---- handoff check exit mapping ----

func TestArbiterHandoffCheckResult(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{"fresh", nil, 0, "fresh"},
		{"stale", taskstate.ErrStaleAge, 1, taskstate.ErrStaleAge.Error()},
		{"missing", taskstate.ErrMissing, 1, taskstate.ErrMissing.Error()},
		{"no-state", taskstate.ErrNoState, 1, "no STATE.json for this task (the loop has not claimed it)"},
		{"no-state-wrapped", errors.New("taskstate: no state file: wrapped"), 1, "taskstate: no state file: wrapped"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, code := handoffCheckResult(c.err)
			if code != c.wantCode {
				t.Errorf("handoffCheckResult(%v) code = %d, want %d", c.err, code, c.wantCode)
			}
			if c.name != "no-state-wrapped" && msg != c.wantMsg {
				t.Errorf("handoffCheckResult(%v) msg = %q, want %q", c.err, msg, c.wantMsg)
			}
		})
	}
}

// ---- ledger footer ----

func TestArbiterLedgerFooter_CountsByLaneAndVerdict(t *testing.T) {
	entries := []arbiter.Entry{
		{Lane: "claude-max", Verdict: "pass"},
		{Lane: "claude-max", Verdict: "fail"},
		{Lane: "codex-plus", Verdict: "pass"},
	}
	got := ledgerFooter(entries)
	if !strings.Contains(got, "3 entries") {
		t.Errorf("ledgerFooter = %q, want 3 entries", got)
	}
	if !strings.Contains(got, "claude-max: 2") || !strings.Contains(got, "codex-plus: 1") {
		t.Errorf("ledgerFooter = %q, want per-lane counts", got)
	}
	if !strings.Contains(got, "pass: 2") || !strings.Contains(got, "fail: 1") {
		t.Errorf("ledgerFooter = %q, want per-verdict counts", got)
	}
}

func TestArbiterLastLedgerEntries(t *testing.T) {
	entries := []arbiter.Entry{{TaskSlug: "a"}, {TaskSlug: "b"}, {TaskSlug: "c"}}
	got := lastLedgerEntries(entries, 2)
	if len(got) != 2 || got[0].TaskSlug != "b" || got[1].TaskSlug != "c" {
		t.Errorf("lastLedgerEntries(_, 2) = %+v, want the last two entries", got)
	}
	if all := lastLedgerEntries(entries, 0); len(all) != 3 {
		t.Errorf("lastLedgerEntries(_, 0) = %+v, want all entries", all)
	}
	if all := lastLedgerEntries(entries, 10); len(all) != 3 {
		t.Errorf("lastLedgerEntries(_, 10) = %+v, want all entries when n exceeds len", all)
	}
}

func TestArbiterLedgerWindowDelta(t *testing.T) {
	e := arbiter.Entry{
		Before: []arbiter.WindowSnapshot{{Provider: "Anthropic / Claude", Label: "5-hour", UsedPct: 10}},
		After:  []arbiter.WindowSnapshot{{Provider: "Anthropic / Claude", Label: "5-hour", UsedPct: 40}},
	}
	got := ledgerWindowDelta(e)
	if !strings.Contains(got, "10.0%") || !strings.Contains(got, "40.0%") {
		t.Errorf("ledgerWindowDelta = %q, want the before/after used pct", got)
	}
	if ledgerWindowDelta(arbiter.Entry{}) != "" {
		t.Errorf("ledgerWindowDelta with no Before rows should be empty")
	}
}

func TestArbiterFilterLedgerByTask(t *testing.T) {
	entries := []arbiter.Entry{{TaskSlug: "a"}, {TaskSlug: "b"}, {TaskSlug: "a"}}
	got := filterLedgerByTask(entries, "a")
	if len(got) != 2 {
		t.Errorf("filterLedgerByTask = %+v, want 2 entries for slug a", got)
	}
	if all := filterLedgerByTask(entries, ""); len(all) != 3 {
		t.Errorf("filterLedgerByTask with empty slug should return all entries")
	}
}
