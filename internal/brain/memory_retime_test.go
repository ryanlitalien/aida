package brain

import (
	"context"
	"testing"
)

// TestRetimeMemory verifies RetimeMemory corrects a record's Created
// timestamp in both brain.db and the JSON mirror without superseding it
// or touching ID/body, and that re-applying the same timestamp is a
// true no-op (returns the already-current value, nil error).
func TestRetimeMemory(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	rec, err := b.WriteMemory(ctx, MemoryRecord{
		Type:    MemoryFact,
		Key:     "k1",
		Body:    "body",
		Profile: "claude",
		Created: "2026-07-18T01:12:00Z",
	})
	if err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}

	prior, err := b.RetimeMemory(rec.ID, "2026-07-10T09:00:00Z")
	if err != nil {
		t.Fatalf("RetimeMemory: %v", err)
	}
	if prior != "2026-07-18T01:12:00Z" {
		t.Errorf("prior = %q, want %q", prior, "2026-07-18T01:12:00Z")
	}

	got, err := b.DB.GetMemory(rec.ID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if got.Created != "2026-07-10T09:00:00Z" {
		t.Errorf("DB Created = %q, want %q", got.Created, "2026-07-10T09:00:00Z")
	}
	if got.SupersededBy != "" {
		t.Errorf("SupersededBy = %q, want empty (still active)", got.SupersededBy)
	}

	disk, err := ReadMemoryFile(b.Path, MemoryFact, rec.ID)
	if err != nil {
		t.Fatalf("ReadMemoryFile: %v", err)
	}
	if disk.Created != "2026-07-10T09:00:00Z" {
		t.Errorf("disk mirror Created = %q, want %q", disk.Created, "2026-07-10T09:00:00Z")
	}

	// No-op case: Created already equals the requested value.
	p2, err := b.RetimeMemory(rec.ID, "2026-07-10T09:00:00Z")
	if err != nil {
		t.Fatalf("RetimeMemory (no-op): %v", err)
	}
	if p2 != "2026-07-10T09:00:00Z" {
		t.Errorf("no-op prior = %q, want %q (unchanged)", p2, "2026-07-10T09:00:00Z")
	}
}
