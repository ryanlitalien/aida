package arbiter

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/burndown"
)

func TestSnapshotFor(t *testing.T) {
	lane := Lane{ID: "claude-max", Provider: "Anthropic / Claude", Windows: []string{"5h", "7d"}}
	caps := []burndown.Capacity{
		cap5h("Anthropic / Claude", 2, 98, 90),
		cap7d("Anthropic / Claude", 5, 95, 85),
		cap5h("Google / Gemini", 0, 100, 95),
	}
	got := SnapshotFor(lane, caps)
	if len(got) != 2 {
		t.Fatalf("expected 2 snapshots, got %d: %+v", len(got), got)
	}
	if got[0].Provider != "Anthropic / Claude" || got[0].UsedPct != 2 {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].UsedPct != 5 {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestSnapshotForSpendLane(t *testing.T) {
	lane := Lane{ID: "litellm", Provider: "LiteLLM proxy (minty)", Spend: true}
	caps := []burndown.Capacity{
		capSpend("LiteLLM proxy (minty)", 10, 80),
		cap5h("Anthropic / Claude", 2, 98, 90),
	}
	got := SnapshotFor(lane, caps)
	if len(got) != 1 || got[0].UsedPct != 10 {
		t.Fatalf("got = %+v", got)
	}
}

func TestAppendAndReadLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "ledger.ndjson")

	e1 := Entry{
		At:       time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
		TaskSlug: "fix-thing",
		TaskID:   42,
		Lane:     "claude-max",
		Model:    "claude-sonnet-5",
		Role:     RoleExecutor,
		Host:     "minty",
		RunID:    "run-1",
		Attempt:  1,
		WallMS:   12345,
		Verdict:  "pass",
		Before:   []WindowSnapshot{{Provider: "Anthropic / Claude", Label: "5-hour", UsedPct: 2}},
		After:    []WindowSnapshot{{Provider: "Anthropic / Claude", Label: "5-hour", UsedPct: 4}},
	}
	e2 := Entry{
		TaskSlug: "add-feature",
		TaskID:   43,
		Lane:     "litellm",
		Verdict:  "hold",
		CostUSD:  0.42,
	}

	if err := AppendLedger(path, e1); err != nil {
		t.Fatalf("AppendLedger e1: %v", err)
	}
	if err := AppendLedger(path, e2); err != nil {
		t.Fatalf("AppendLedger e2: %v", err)
	}

	got, err := ReadLedger(path)
	if err != nil {
		t.Fatalf("ReadLedger: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].TaskSlug != "fix-thing" || got[0].TaskID != 42 {
		t.Errorf("got[0] = %+v", got[0])
	}
	if len(got[0].Before) != 1 || got[0].Before[0].UsedPct != 2 {
		t.Errorf("got[0].Before = %+v", got[0].Before)
	}
	if got[1].TaskSlug != "add-feature" || got[1].CostUSD != 0.42 {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestReadLedgerMissingFile(t *testing.T) {
	got, err := ReadLedger("/nonexistent/ledger.ndjson")
	if err != nil {
		t.Fatalf("ReadLedger(missing) error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestReadLedgerToleratesCorruptLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.ndjson")
	if err := AppendLedger(path, Entry{TaskSlug: "a"}); err != nil {
		t.Fatalf("AppendLedger: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString("not json\n"); err != nil {
		t.Fatalf("write corrupt line: %v", err)
	}
	f.Close()
	if err := AppendLedger(path, Entry{TaskSlug: "b"}); err != nil {
		t.Fatalf("AppendLedger: %v", err)
	}

	got, err := ReadLedger(path)
	if err != nil {
		t.Fatalf("ReadLedger: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 valid entries despite the corrupt line, got %d", len(got))
	}
}

func TestAppendLedgerDefaultsAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.ndjson")
	before := time.Now()
	if err := AppendLedger(path, Entry{TaskSlug: "a"}); err != nil {
		t.Fatalf("AppendLedger: %v", err)
	}
	got, err := ReadLedger(path)
	if err != nil {
		t.Fatalf("ReadLedger: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].At.Before(before) {
		t.Errorf("expected At to default to roughly now, got %v (before %v)", got[0].At, before)
	}
}
