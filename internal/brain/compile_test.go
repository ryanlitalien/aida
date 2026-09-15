package brain

import (
	"context"
	"testing"
)

// TestIndex_ReindexesMemoryRecords verifies that Brain.Index rebuilds the
// memory_records table from the brain/memory/<type>/*.json mirrors, and
// that supersession (which lives in brain.db only, not in the JSON files)
// is correctly recomputed from Created order rather than carried over.
func TestIndex_ReindexesMemoryRecords(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	// Two facts sharing a key: after a rebuild-from-disk, the newer one
	// should be active and the older one superseded, even though neither
	// JSON mirror records that relationship (superseded_by is DB-only).
	// Created is set explicitly (rather than relying on wall-clock
	// separation) since Created is RFC3339-with-second-precision - two
	// records written within the same second would otherwise tie.
	older, err := b.WriteMemory(ctx, MemoryRecord{
		Type: MemoryFact, Key: "user-units", Body: "user uses metric",
		Created: "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("WriteMemory older: %v", err)
	}
	newer, err := b.WriteMemory(ctx, MemoryRecord{
		Type: MemoryFact, Key: "user-units", Body: "user uses US units",
		Created: "2026-01-02T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("WriteMemory newer: %v", err)
	}
	if _, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: "an event happened"}); err != nil {
		t.Fatalf("WriteMemory event: %v", err)
	}

	// Wipe the derived index to simulate rebuilding brain.db from scratch:
	// Index must repopulate memory_records entirely from the JSON mirrors.
	if _, err := b.DB.conn.Exec(`DELETE FROM memory_records`); err != nil {
		t.Fatalf("clearing memory_records: %v", err)
	}

	if err := b.Index(ctx); err != nil {
		t.Fatalf("Index: %v", err)
	}

	counts, err := b.DB.MemoryCounts()
	if err != nil {
		t.Fatalf("MemoryCounts: %v", err)
	}
	got := map[MemoryType]MemoryCount{}
	for _, c := range counts {
		got[c.Type] = c
	}
	if got[MemoryFact].Active != 1 || got[MemoryFact].Superseded != 1 {
		t.Errorf("fact counts after reindex = %+v, want {Active:1, Superseded:1}", got[MemoryFact])
	}
	if got[MemoryEvent].Active != 1 || got[MemoryEvent].Superseded != 0 {
		t.Errorf("event counts after reindex = %+v, want {Active:1, Superseded:0}", got[MemoryEvent])
	}

	active, err := b.DB.ActiveMemoryByKey(MemoryFact, "user-units", "test")
	if err != nil {
		t.Fatalf("ActiveMemoryByKey after reindex: %v", err)
	}
	if active.ID != newer.ID {
		t.Errorf("active fact after reindex = %q, want %q (newer)", active.ID, newer.ID)
	}

	prior, err := b.DB.GetMemory(older.ID)
	if err != nil {
		t.Fatalf("GetMemory older after reindex: %v", err)
	}
	if prior.SupersededBy != newer.ID {
		t.Errorf("older.SupersededBy after reindex = %q, want %q", prior.SupersededBy, newer.ID)
	}
}
