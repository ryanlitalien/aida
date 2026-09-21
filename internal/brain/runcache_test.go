package brain

import (
	"context"
	"testing"
	"time"
)

func TestInsertAndFindSimilarRunCache(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	aligned := make([]float32, VectorDims)
	aligned[0] = 1.0
	orthogonal := make([]float32, VectorDims)
	orthogonal[1] = 1.0

	if err := db.InsertRunCache(&RunCacheRecord{
		ID: "rc-1", Profile: "work", Question: "what is our GMV for pine-hollow",
		Embedding: aligned, Answer: "GMV is $1.2M", Sources: []string{"sqlite"},
		Confidence: 0.9, Created: "2026-05-24T10:00:00Z",
	}); err != nil {
		t.Fatalf("InsertRunCache (aligned): %v", err)
	}
	if err := db.InsertRunCache(&RunCacheRecord{
		ID: "rc-2", Profile: "work", Question: "unrelated question",
		Embedding: orthogonal, Answer: "something else", Sources: []string{"notion"},
		Confidence: 0.8, Created: "2026-05-24T10:00:01Z",
	}); err != nil {
		t.Fatalf("InsertRunCache (orthogonal): %v", err)
	}

	results, err := db.FindSimilarRunCache(aligned, 5, "work", runCacheSimilarityThreshold)
	if err != nil {
		t.Fatalf("FindSimilarRunCache: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (orthogonal filtered by threshold), got %d", len(results))
	}
	got := results[0].Record
	if got.ID != "rc-1" {
		t.Errorf("ID: want rc-1, got %s", got.ID)
	}
	if got.Answer != "GMV is $1.2M" {
		t.Errorf("Answer mismatch: %s", got.Answer)
	}
	if len(got.Sources) != 1 || got.Sources[0] != "sqlite" {
		t.Errorf("Sources mismatch: %+v", got.Sources)
	}
	if results[0].Similarity < 0.99 {
		t.Errorf("Similarity ~1.0 expected, got %f", results[0].Similarity)
	}
}

func TestFindSimilarRunCache_ProfileIsolation(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	emb := make([]float32, VectorDims)
	emb[0] = 1.0

	if err := db.InsertRunCache(&RunCacheRecord{
		ID: "work-1", Profile: "work", Question: "work question",
		Embedding: emb, Answer: "work answer", Created: "2026-05-24T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertRunCache(&RunCacheRecord{
		ID: "home-1", Profile: "home", Question: "home question",
		Embedding: emb, Answer: "home answer", Created: "2026-05-24T10:00:01Z",
	}); err != nil {
		t.Fatal(err)
	}

	results, err := db.FindSimilarRunCache(emb, 5, "work", runCacheSimilarityThreshold)
	if err != nil {
		t.Fatalf("FindSimilarRunCache: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result scoped to work profile, got %d", len(results))
	}
	if results[0].Record.ID != "work-1" {
		t.Errorf("expected work-1, got %s", results[0].Record.ID)
	}

	// Untagged (profile="") rows remain visible to every profile.
	if err := db.InsertRunCache(&RunCacheRecord{
		ID: "untagged-1", Profile: "", Question: "untagged question",
		Embedding: emb, Answer: "untagged answer", Created: "2026-05-24T10:00:02Z",
	}); err != nil {
		t.Fatal(err)
	}
	results, err = db.FindSimilarRunCache(emb, 5, "work", runCacheSimilarityThreshold)
	if err != nil {
		t.Fatalf("FindSimilarRunCache: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (work + untagged), got %d", len(results))
	}
}

func TestTouchRunCache(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	emb := make([]float32, VectorDims)
	emb[0] = 1.0
	if err := db.InsertRunCache(&RunCacheRecord{
		ID: "rc-touch", Profile: "work", Question: "q",
		Embedding: emb, Answer: "a", Created: "2026-05-24T10:00:00Z", HitCount: 0,
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.TouchRunCache("rc-touch", "2026-05-24T11:00:00Z"); err != nil {
		t.Fatalf("TouchRunCache: %v", err)
	}

	var hitCount int
	var lastUsed string
	if err := db.conn.QueryRow(
		"SELECT hit_count, last_used FROM run_cache WHERE id = ?", "rc-touch",
	).Scan(&hitCount, &lastUsed); err != nil {
		t.Fatalf("select back: %v", err)
	}
	if hitCount != 1 {
		t.Errorf("hit_count: want 1, got %d", hitCount)
	}
	if lastUsed != "2026-05-24T11:00:00Z" {
		t.Errorf("last_used: want 2026-05-24T11:00:00Z, got %s", lastUsed)
	}
}

func TestRunCacheCountAndClear(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	if got := db.RunCacheCount(); got != 0 {
		t.Errorf("empty count: want 0, got %d", got)
	}

	emb := make([]float32, VectorDims)
	emb[0] = 1.0
	if err := db.InsertRunCache(&RunCacheRecord{
		ID: "rc-1", Profile: "work", Question: "q", Embedding: emb,
		Answer: "a", Created: "2026-05-24T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if got := db.RunCacheCount(); got != 1 {
		t.Errorf("count after insert: want 1, got %d", got)
	}

	if err := db.ClearRunCache(); err != nil {
		t.Fatalf("ClearRunCache: %v", err)
	}
	if got := db.RunCacheCount(); got != 0 {
		t.Errorf("count after clear: want 0, got %d", got)
	}
}

func TestPickRunCacheHit_MarginGuard(t *testing.T) {
	// Empty input -> miss.
	if _, ok, err := pickRunCacheHit(nil); ok || err != nil {
		t.Errorf("empty results: want (false, nil), got (%v, %v)", ok, err)
	}

	// Single result -> always a hit, regardless of similarity.
	single := []SimilarRunCache{
		{Record: RunCacheRecord{ID: "only", Answer: "the answer"}, Similarity: 0.91},
	}
	rec, ok, err := pickRunCacheHit(single)
	if err != nil || !ok || rec.ID != "only" {
		t.Errorf("single result: want (only, true, nil), got (%+v, %v, %v)", rec, ok, err)
	}

	// Two near-tied DIFFERENT answers within the margin -> ambiguous, miss.
	ambiguous := []SimilarRunCache{
		{Record: RunCacheRecord{ID: "a", Answer: "answer A"}, Similarity: 0.95},
		{Record: RunCacheRecord{ID: "b", Answer: "answer B"}, Similarity: 0.94},
	}
	if _, ok, err := pickRunCacheHit(ambiguous); ok || err != nil {
		t.Errorf("ambiguous distinct answers: want (false, nil), got (%v, %v)", ok, err)
	}

	// Two near-tied results sharing the SAME answer -> not ambiguous, hit.
	sameAnswer := []SimilarRunCache{
		{Record: RunCacheRecord{ID: "a", Answer: "same answer"}, Similarity: 0.95},
		{Record: RunCacheRecord{ID: "b", Answer: "same answer"}, Similarity: 0.94},
	}
	rec, ok, err = pickRunCacheHit(sameAnswer)
	if err != nil || !ok || rec.ID != "a" {
		t.Errorf("tied same-answer results: want (a, true, nil), got (%+v, %v, %v)", rec, ok, err)
	}

	// Two DIFFERENT answers with a gap >= margin -> confident hit on top-1.
	clearWinner := []SimilarRunCache{
		{Record: RunCacheRecord{ID: "a", Answer: "answer A"}, Similarity: 0.98},
		{Record: RunCacheRecord{ID: "b", Answer: "answer B"}, Similarity: 0.90},
	}
	rec, ok, err = pickRunCacheHit(clearWinner)
	if err != nil || !ok || rec.ID != "a" {
		t.Errorf("clear winner: want (a, true, nil), got (%+v, %v, %v)", rec, ok, err)
	}
}

func TestRunCacheExpired(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		created string
		want    bool // want expired
	}{
		{name: "fresh entry served", created: now.Add(-24 * time.Hour).Format(time.RFC3339), want: false},
		{name: "entry older than 30 days rejected", created: now.Add(-31 * 24 * time.Hour).Format(time.RFC3339), want: true},
		{name: "empty Created rejected", created: "", want: true},
		{name: "unparseable Created rejected", created: "not-a-timestamp", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := RunCacheRecord{ID: "rc", Created: tt.created}
			if got := runCacheExpired(rec, now); got != tt.want {
				t.Errorf("runCacheExpired(Created=%q) = %v, want %v", tt.created, got, tt.want)
			}
		})
	}
}

// TestLookupRunCache_ExpiredTopOneDoesNotSuppressFreshTopTwo proves the
// filter-before-pick ordering in lookupRunCache: two DISTINCT-answer
// records engineered to be near-tied (within runCacheMinMargin) would
// normally trip pickRunCacheHit's ambiguity guard and miss entirely. But
// the higher-scoring one ("expired") is 60 days old and the other
// ("fresh") is 1 day old. If expiry filtering ran AFTER the margin guard
// instead of before, "expired" would still count as top-1 for the
// ambiguity check and its dead near-tie would suppress a perfectly good
// hit on "fresh". Filtering first removes "expired" before the margin
// check ever sees it, so "fresh" is served as an unconditional single
// candidate.
func TestLookupRunCache_ExpiredTopOneDoesNotSuppressFreshTopTwo(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	query := make([]float32, VectorDims)
	query[0] = 1.0

	// Unit vectors angled off query so cosine(query, .) lands at exactly
	// 0.95 and 0.94 -- an 0.01 gap, well inside runCacheMinMargin (0.02).
	expiredEmb := make([]float32, VectorDims)
	expiredEmb[0] = 0.95
	expiredEmb[1] = 0.312250 // sqrt(1 - 0.95^2), keeps the vector unit-length
	freshEmb := make([]float32, VectorDims)
	freshEmb[0] = 0.94
	freshEmb[1] = 0.341174 // sqrt(1 - 0.94^2)

	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

	if err := db.InsertRunCache(&RunCacheRecord{
		ID: "expired", Profile: "work", Question: "q1", Embedding: expiredEmb,
		Answer: "expired answer", Created: now.Add(-60 * 24 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertRunCache(&RunCacheRecord{
		ID: "fresh", Profile: "work", Question: "q2", Embedding: freshEmb,
		Answer: "fresh answer", Created: now.Add(-24 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	b := &Brain{DB: db}
	fakeEmbed := func(ctx context.Context, text string) ([]float32, error) { return query, nil }
	fakeNow := func() time.Time { return now }

	hit, ok, err := b.lookupRunCache(context.Background(), "some question", "work", fakeEmbed, fakeNow)
	if err != nil {
		t.Fatalf("lookupRunCache: %v", err)
	}
	if !ok {
		t.Fatal("lookupRunCache: want hit on the fresh record, got miss")
	}
	if hit.ID != "fresh" {
		t.Errorf("lookupRunCache: want fresh record served, got %q", hit.ID)
	}
}
