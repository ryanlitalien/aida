package brain

import (
	"context"
	"strings"
	"testing"
)

// WriteThreadMemory should persist an append-only MemoryEvent carrying the
// jarvis-thread tag and voice source.
func TestWriteThreadMemory_InsertsTaggedEvent(t *testing.T) {
	b := newTestBrain(t)
	rec, err := b.WriteThreadMemory(context.Background(),
		"building dome 50 layer 6 at y=144 in light blue stained glass")
	if err != nil {
		t.Fatalf("WriteThreadMemory: %v", err)
	}
	if rec == nil {
		t.Fatal("expected a record, got nil")
	}
	if rec.Type != MemoryEvent {
		t.Errorf("Type = %q, want event", rec.Type)
	}
	if rec.Source != threadMemorySource {
		t.Errorf("Source = %q, want %q", rec.Source, threadMemorySource)
	}
	if !threadTagged(rec.Tags) {
		t.Errorf("record missing %q tag: %v", ThreadMemoryTag, rec.Tags)
	}

	got, err := b.DB.GetMemory(rec.ID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if got.Type != MemoryEvent || !threadTagged(got.Tags) {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

// Empty/whitespace summaries are a no-op.
func TestWriteThreadMemory_EmptyIsNoOp(t *testing.T) {
	b := newTestBrain(t)
	rec, err := b.WriteThreadMemory(context.Background(), "   ")
	if err != nil || rec != nil {
		t.Errorf("empty summary should be a no-op, got (%v, %v)", rec, err)
	}
}

// FindSimilarThreadMemory returns tagged thread events for a similar query
// and excludes untagged events.
func TestFindSimilarThreadMemory_FiltersByTag(t *testing.T) {
	b := newTestBrain(t)

	emb := make([]float32, VectorDims)
	emb[0] = 1.0 // points along dimension 0

	// A tagged thread event, embedded along dim 0.
	thread := MemoryRecord{
		ID: "mem-thread-1", Type: MemoryEvent, Profile: "test",
		Body: "dome 50 layer 6, light blue glass, next layer 7 at y=143",
		Tags: []string{ThreadMemoryTag}, Source: threadMemorySource,
		Created: "2026-06-24T10:00:00Z", Embedding: emb,
	}
	if err := b.DB.InsertMemory(&thread); err != nil {
		t.Fatalf("InsertMemory thread: %v", err)
	}

	// An untagged event with the SAME embedding - must be excluded.
	other := MemoryRecord{
		ID: "mem-other-1", Type: MemoryEvent, Profile: "test",
		Body: "unrelated event", Tags: []string{"misc"},
		Created: "2026-06-24T10:01:00Z", Embedding: emb,
	}
	if err := b.DB.InsertMemory(&other); err != nil {
		t.Fatalf("InsertMemory other: %v", err)
	}

	got, err := b.FindSimilarThreadMemory(context.Background(), emb, 5)
	if err != nil {
		t.Fatalf("FindSimilarThreadMemory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 tagged result, got %d", len(got))
	}
	if got[0].Record.ID != "mem-thread-1" {
		t.Errorf("wrong record: %s", got[0].Record.ID)
	}
	if got[0].Similarity < 0.99 {
		t.Errorf("identical embeddings should score ~1.0, got %f", got[0].Similarity)
	}
}

// An empty query embedding (no Voyage key) is a graceful no-op.
func TestFindSimilarThreadMemory_NoEmbeddingNoOp(t *testing.T) {
	b := newTestBrain(t)
	got, err := b.FindSimilarThreadMemory(context.Background(), nil, 5)
	if err != nil || got != nil {
		t.Errorf("nil embedding should no-op, got (%v, %v)", got, err)
	}
}

// FormatThreadRecallProse renders a block for non-empty input and "" for empty.
func TestFormatThreadRecallProse(t *testing.T) {
	if s := FormatThreadRecallProse(nil); s != "" {
		t.Errorf("empty input should yield empty string, got %q", s)
	}
	recs := []SimilarThreadMemory{
		{Record: MemoryRecord{Body: "dome 50 layer 6, light blue glass"}, Similarity: 0.9},
	}
	s := FormatThreadRecallProse(recs)
	if s == "" {
		t.Fatal("expected a prose block")
	}
	if !strings.Contains(s, "Earlier in this conversation") || !strings.Contains(s, "light blue glass") {
		t.Errorf("prose missing expected content:\n%s", s)
	}
}
