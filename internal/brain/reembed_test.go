package brain

// Unit tests for the injectable core of the full-corpus re-embed tool
// (reembed.go). Mirrors mine_test.go's shape: a fake EmbedFunc stands in
// for a live Voyage call so overwrite semantics, dry-run, and the --table
// filter are all verifiable without hitting the real API.

import (
	"context"
	"testing"
)

// staleVector is a fixed marker written before a reembedAll run so a test
// can assert it got replaced (real re-embed) or left alone (dry-run).
func staleVector() []float32 { return []float32{-1, -1} }

// fakeEmbedFunc returns one vector per input text, keyed by first-seen
// order -- distinct enough from staleVector to prove a real overwrite
// happened, and it fails the test if called when it shouldn't be (pass a
// nil-returning func for that case instead).
func fakeEmbedFunc(t *testing.T) EmbedFunc {
	t.Helper()
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{float32(i + 1), 9}
		}
		return out, nil
	}
}

func failIfCalledEmbedFunc(t *testing.T) EmbedFunc {
	t.Helper()
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		t.Fatalf("embed func called with %d texts, want no call (dry-run)", len(texts))
		return nil, nil
	}
}

// newReembedTestBrain opens a throwaway brain.db and seeds one row per
// embedded table (skipping routing_rules, which stays empty like
// production) with a stale embedding, so reembedAll has something real to
// overwrite in every table.
func newReembedTestBrain(t *testing.T) *Brain {
	t.Helper()
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	b := &Brain{Path: dir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "work"}

	if err := db.InsertLesson(&LessonRecord{
		ID: "lesson-1", Timestamp: "2026-01-01T00:00:00Z", Profile: "work",
		Question: "what is our GMV for pine-hollow", Embedding: staleVector(),
	}); err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	if err := db.UpsertEntity(&EntityRecord{
		Slug: "pine-hollow", Type: "partner", Name: "Pine Hollow Campground",
		Summary: "Pine Hollow Campground is a seasonal campground.", Embedding: staleVector(),
	}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := db.InsertMemory(&MemoryRecord{
		ID: "mem-1", Type: MemoryFact, Body: "Ryan prefers Fahrenheit.",
		Profile: "work", Created: "2026-01-01T00:00:00Z", Embedding: staleVector(),
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if err := db.UpsertWikiPage("wiki-1", "Pine Hollow Campground", "wiki/entities/pine-hollow.md",
		"Pine Hollow Campground body text.", staleVector(), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed wiki page: %v", err)
	}
	if err := db.UpsertKnowledgePage("its-llc", "ITS", "knowledge/domains/its-llc.md",
		"ITS is the consulting LLC.", staleVector(), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed knowledge page: %v", err)
	}
	if err := db.InsertJarvisLesson(&JarvisLesson{
		ID: "jl-1", Timestamp: "2026-01-01T00:00:00Z", Profile: "work",
		RatedTurnStartedAt: "2026-01-01T00:00:00Z", Query: "what's the weather",
		Reply: "sunny", Feedback: "up", Embedding: staleVector(),
	}); err != nil {
		t.Fatalf("seed jarvis lesson: %v", err)
	}
	if err := db.InsertRunCache(&RunCacheRecord{
		ID: "rc-1", Profile: "work", Question: "what is pine-hollow gmv",
		Embedding: staleVector(), Answer: "$2.4M", Created: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed run cache: %v", err)
	}

	return b
}

func vecEmbedding(t *testing.T, b *Brain, table, idCol, id, embedCol string) []float32 {
	t.Helper()
	var blob []byte
	err := b.DB.conn.QueryRow(
		"SELECT "+embedCol+" FROM "+table+" WHERE "+idCol+" = ?", id,
	).Scan(&blob)
	if err != nil {
		t.Fatalf("query %s.%s: %v", table, embedCol, err)
	}
	return DecodeVector(blob)
}

func TestReembedAllOverwritesEveryTable(t *testing.T) {
	b := newReembedTestBrain(t)

	result, err := b.reembedAll(context.Background(), fakeEmbedFunc(t), nil, false)
	if err != nil {
		t.Fatalf("reembedAll: %v", err)
	}

	// Every table in reembedTables gets a result, including the always-empty
	// routing_rules.
	if len(result.Tables) != len(reembedTables) {
		t.Fatalf("got %d table results, want %d", len(result.Tables), len(reembedTables))
	}

	want := map[string]struct{ rows, updated int }{
		"lessons":         {1, 1},
		"routing_rules":   {0, 0},
		"entities":        {1, 1},
		"memory_records":  {1, 1},
		"wiki_pages":      {1, 1},
		"knowledge_pages": {1, 1},
		"jarvis_lessons":  {1, 1},
		"run_cache":       {1, 1},
	}
	for _, tr := range result.Tables {
		w, ok := want[tr.Table]
		if !ok {
			t.Errorf("unexpected table %q in result", tr.Table)
			continue
		}
		if tr.Rows != w.rows || tr.Updated != w.updated {
			t.Errorf("table %s: Rows=%d Updated=%d, want Rows=%d Updated=%d", tr.Table, tr.Rows, tr.Updated, w.rows, w.updated)
		}
	}
	if got := result.TotalRows(); got != 7 {
		t.Errorf("TotalRows() = %d, want 7", got)
	}
	if got := result.TotalUpdated(); got != 7 {
		t.Errorf("TotalUpdated() = %d, want 7", got)
	}

	// Every seeded row's embedding actually changed from the stale marker.
	checks := []struct{ table, idCol, id, embedCol string }{
		{"lessons", "id", "lesson-1", "question_embedding"},
		{"entities", "slug", "pine-hollow", "summary_embedding"},
		{"memory_records", "id", "mem-1", "body_embedding"},
		{"wiki_pages", "slug", "wiki-1", "embedding"},
		{"knowledge_pages", "slug", "its-llc", "embedding"},
		{"jarvis_lessons", "id", "jl-1", "query_embedding"},
		{"run_cache", "id", "rc-1", "question_embedding"},
	}
	for _, c := range checks {
		got := vecEmbedding(t, b, c.table, c.idCol, c.id, c.embedCol)
		if len(got) == 0 {
			t.Errorf("%s: embedding is empty after reembed", c.table)
			continue
		}
		if got[0] == staleVector()[0] && got[1] == staleVector()[1] {
			t.Errorf("%s: embedding still matches the stale marker, want overwritten", c.table)
		}
	}
}

func TestReembedAllDryRunDoesNotWriteOrCallEmbed(t *testing.T) {
	b := newReembedTestBrain(t)

	result, err := b.reembedAll(context.Background(), failIfCalledEmbedFunc(t), nil, true)
	if err != nil {
		t.Fatalf("reembedAll dry-run: %v", err)
	}
	if got := result.TotalRows(); got != 7 {
		t.Errorf("TotalRows() = %d, want 7", got)
	}
	if got := result.TotalUpdated(); got != 0 {
		t.Errorf("TotalUpdated() = %d, want 0 on dry-run", got)
	}

	// The lesson's embedding must still be the stale marker -- dry-run
	// wrote nothing.
	got := vecEmbedding(t, b, "lessons", "id", "lesson-1", "question_embedding")
	if len(got) != 2 || got[0] != staleVector()[0] || got[1] != staleVector()[1] {
		t.Errorf("lessons embedding after dry-run = %v, want unchanged stale marker %v", got, staleVector())
	}
}

func TestReembedAllTableFilter(t *testing.T) {
	b := newReembedTestBrain(t)

	result, err := b.reembedAll(context.Background(), fakeEmbedFunc(t), []string{"lessons", "entities"}, false)
	if err != nil {
		t.Fatalf("reembedAll: %v", err)
	}
	if len(result.Tables) != 2 {
		t.Fatalf("got %d table results, want 2 (filtered to lessons, entities)", len(result.Tables))
	}
	for _, tr := range result.Tables {
		if tr.Table != "lessons" && tr.Table != "entities" {
			t.Errorf("unexpected table %q in filtered result", tr.Table)
		}
	}

	// wiki_pages wasn't in the filter -- its stale embedding must survive.
	got := vecEmbedding(t, b, "wiki_pages", "slug", "wiki-1", "embedding")
	if len(got) != 2 || got[0] != staleVector()[0] || got[1] != staleVector()[1] {
		t.Errorf("wiki_pages embedding after filtered reembed = %v, want unchanged stale marker %v", got, staleVector())
	}
}

func TestReembedAllSkipsEmptyText(t *testing.T) {
	b := newReembedTestBrain(t)
	// A lesson with a blank question shouldn't be picked up for re-embedding
	// -- there's nothing meaningful to embed.
	if err := b.DB.InsertLesson(&LessonRecord{
		ID: "lesson-blank", Timestamp: "2026-01-01T00:00:00Z", Profile: "work",
		Question: "   ", Embedding: staleVector(),
	}); err != nil {
		t.Fatalf("seed blank lesson: %v", err)
	}

	result, err := b.reembedAll(context.Background(), fakeEmbedFunc(t), []string{"lessons"}, false)
	if err != nil {
		t.Fatalf("reembedAll: %v", err)
	}
	if len(result.Tables) != 1 || result.Tables[0].Rows != 1 {
		t.Fatalf("lessons result = %+v, want Rows=1 (blank-question row excluded)", result.Tables)
	}
}
