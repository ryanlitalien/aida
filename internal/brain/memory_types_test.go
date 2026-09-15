package brain

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestBrain creates a brain rooted at t.TempDir() with no embedding
// client (Voyage key unset → Embeddings.Available() returns false), so
// memory writes don't try to hit the network in tests.
func newTestBrain(t *testing.T) *Brain {
	t.Helper()
	tmp := t.TempDir()
	b, err := Open(tmp, "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestWriteMemory_FactRoundTrip(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	rec, err := b.WriteMemory(ctx, MemoryRecord{
		Type: MemoryFact,
		Key:  "user-units",
		Body: "user prefers US units (miles, °F, lbs)",
		Tags: []string{"preference"},
	})
	if err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	if rec.ID == "" || rec.Created == "" {
		t.Errorf("ID/Created not auto-filled: %+v", rec)
	}
	if rec.Profile != "test" {
		t.Errorf("Profile not auto-filled: %q", rec.Profile)
	}

	got, err := b.DB.GetMemory(rec.ID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if got.Body != rec.Body || got.Type != MemoryFact || got.Key != "user-units" {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "preference" {
		t.Errorf("tags lost: %v", got.Tags)
	}

	// File mirror exists.
	path := filepath.Join(MemoryDir(b.Path), "fact", rec.ID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file mirror at %s, got %v", path, err)
	}
}

func TestWriteMemory_SupersedesPriorFact(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	first, err := b.WriteMemory(ctx, MemoryRecord{
		Type: MemoryFact, Key: "user-units", Body: "user uses metric",
	})
	if err != nil {
		t.Fatalf("first WriteMemory: %v", err)
	}
	// Sleep enough to guarantee distinct millisecond ID prefixes - the
	// timestamp in newMemoryID is millisecond-resolution.
	time.Sleep(2 * time.Millisecond)
	second, err := b.WriteMemory(ctx, MemoryRecord{
		Type: MemoryFact, Key: "user-units", Body: "user uses US units",
	})
	if err != nil {
		t.Fatalf("second WriteMemory: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("expected distinct IDs, both = %q", first.ID)
	}

	// Active lookup returns the newer record.
	active, err := b.DB.ActiveMemoryByKey(MemoryFact, "user-units", "test")
	if err != nil {
		t.Fatalf("ActiveMemoryByKey: %v", err)
	}
	if active.ID != second.ID {
		t.Errorf("active = %q, want %q (second)", active.ID, second.ID)
	}

	// First record now points to second.
	prior, err := b.DB.GetMemory(first.ID)
	if err != nil {
		t.Fatalf("GetMemory first: %v", err)
	}
	if prior.SupersededBy != second.ID {
		t.Errorf("first.SupersededBy = %q, want %q", prior.SupersededBy, second.ID)
	}

	// ListMemory active only excludes the superseded one.
	active2, _ := b.DB.ListMemory(MemoryListOpts{
		Types: []MemoryType{MemoryFact}, Profile: "test",
	})
	if len(active2) != 1 || active2[0].ID != second.ID {
		t.Errorf("active list = %+v, want [second]", active2)
	}

	// Including superseded brings both back.
	all, _ := b.DB.ListMemory(MemoryListOpts{
		Types: []MemoryType{MemoryFact}, Profile: "test", IncludeSuperseded: true,
	})
	if len(all) != 2 {
		t.Errorf("incl-superseded list len = %d, want 2", len(all))
	}
}

func TestWriteMemory_EventDoesNotSupersede(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	// Two events with the same key (which events ignore for supersession).
	first, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Key: "x", Body: "first"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Key: "x", Body: "second"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	all, _ := b.DB.ListMemory(MemoryListOpts{
		Types: []MemoryType{MemoryEvent}, Profile: "test",
	})
	if len(all) != 2 {
		t.Errorf("expected 2 active events, got %d", len(all))
	}
	prior, _ := b.DB.GetMemory(first.ID)
	if prior.SupersededBy != "" {
		t.Errorf("event was superseded (SupersededBy=%q), want untouched", prior.SupersededBy)
	}
	_ = second
}

func TestWriteMemory_InstructionSupersedesLikeFact(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	_, err := b.WriteMemory(ctx, MemoryRecord{
		Type: MemoryInstruction, Key: "build-after-changes", Body: "run make install",
	})
	if err != nil {
		t.Fatalf("first instr: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := b.WriteMemory(ctx, MemoryRecord{
		Type: MemoryInstruction, Key: "build-after-changes", Body: "run make install && go test",
	})
	if err != nil {
		t.Fatalf("second instr: %v", err)
	}
	got, err := b.DB.ActiveMemoryByKey(MemoryInstruction, "build-after-changes", "test")
	if err != nil {
		t.Fatalf("ActiveMemoryByKey: %v", err)
	}
	if got.ID != second.ID {
		t.Errorf("active instruction = %q, want %q", got.ID, second.ID)
	}
}

func TestWriteMemory_RejectsInvalidType(t *testing.T) {
	b := newTestBrain(t)
	_, err := b.WriteMemory(context.Background(), MemoryRecord{
		Type: MemoryType("garbage"), Body: "x",
	})
	if err == nil {
		t.Errorf("expected error for invalid type, got nil")
	}
}

func TestWriteMemory_RejectsEmptyBody(t *testing.T) {
	b := newTestBrain(t)
	_, err := b.WriteMemory(context.Background(), MemoryRecord{
		Type: MemoryFact, Body: "   ",
	})
	if err == nil {
		t.Errorf("expected error for empty body, got nil")
	}
}

func TestActiveMemoryByKey_NotFound(t *testing.T) {
	b := newTestBrain(t)
	_, err := b.DB.ActiveMemoryByKey(MemoryFact, "missing", "test")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestReadMemoryFile_RoundTrip(t *testing.T) {
	b := newTestBrain(t)
	rec, _ := b.WriteMemory(context.Background(), MemoryRecord{
		Type: MemoryFact, Key: "k", Body: "from disk", Tags: []string{"a", "b"},
	})
	got, err := ReadMemoryFile(b.Path, MemoryFact, rec.ID)
	if err != nil {
		t.Fatalf("ReadMemoryFile: %v", err)
	}
	if got.Body != rec.Body || strings.Join(got.Tags, ",") != "a,b" {
		t.Errorf("disk mismatch: got %+v", got)
	}
}

func TestMemoryCounts(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	_, _ = b.WriteMemory(ctx, MemoryRecord{Type: MemoryFact, Key: "a", Body: "1"})
	time.Sleep(2 * time.Millisecond)
	_, _ = b.WriteMemory(ctx, MemoryRecord{Type: MemoryFact, Key: "a", Body: "2"})
	_, _ = b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: "ev1"})
	_, _ = b.WriteMemory(ctx, MemoryRecord{Type: MemoryInstruction, Key: "i", Body: "rule"})

	counts, err := b.DB.MemoryCounts()
	if err != nil {
		t.Fatalf("MemoryCounts: %v", err)
	}
	got := map[MemoryType]MemoryCount{}
	for _, c := range counts {
		got[c.Type] = c
	}
	if got[MemoryFact].Active != 1 || got[MemoryFact].Superseded != 1 {
		t.Errorf("fact counts = %+v, want {Active:1, Superseded:1}", got[MemoryFact])
	}
	if got[MemoryEvent].Active != 1 {
		t.Errorf("event active = %d, want 1", got[MemoryEvent].Active)
	}
	if got[MemoryInstruction].Active != 1 {
		t.Errorf("instruction active = %d, want 1", got[MemoryInstruction].Active)
	}
}

func TestSupersedesByKey(t *testing.T) {
	cases := []struct {
		t    MemoryType
		want bool
	}{
		{MemoryFact, true},
		{MemoryInstruction, true},
		{MemoryEvent, false},
	}
	for _, c := range cases {
		if got := c.t.SupersedesByKey(); got != c.want {
			t.Errorf("%s.SupersedesByKey() = %v, want %v", c.t, got, c.want)
		}
	}
}
