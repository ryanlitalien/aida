package brain

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// RunCacheRecord is one Tier-1 mined-question cache entry: a question paired
// with the answer a full engine run previously produced for it, so a
// confident repeat match can skip Parse/Classify/Resolve/Plan/Execute/
// Synthesize entirely.
type RunCacheRecord struct {
	ID          string    `json:"id"`
	Profile     string    `json:"profile"`
	Question    string    `json:"question"`
	Embedding   []float32 `json:"-"`
	Answer      string    `json:"answer"`
	Sources     []string  `json:"sources"`
	Confidence  float64   `json:"confidence"`
	HitCount    int       `json:"hit_count"`
	Created     string    `json:"created"`
	LastUsed    string    `json:"last_used"`
	SourceRunID string    `json:"source_run_id,omitempty"`
}

// SimilarRunCache pairs a RunCacheRecord with its cosine similarity to a
// query embedding.
type SimilarRunCache struct {
	Record     RunCacheRecord
	Similarity float64
}

// InsertRunCache writes one run_cache row. Replaces on id conflict.
func (db *DB) InsertRunCache(r *RunCacheRecord) error {
	sources, _ := json.Marshal(r.Sources)
	var embedding []byte
	if len(r.Embedding) > 0 {
		embedding = EncodeVector(r.Embedding)
	}
	_, err := db.conn.Exec(`
		INSERT OR REPLACE INTO run_cache
		(id, profile, question, question_embedding, answer, sources_json,
		 confidence, hit_count, created, last_used, source_run_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Profile, r.Question, embedding, r.Answer, string(sources),
		r.Confidence, r.HitCount, r.Created, r.LastUsed, r.SourceRunID,
	)
	return err
}

// FindSimilarRunCache runs the cosine search. Reads up to 500 most recent
// rows from the profile, scores each, returns the top k above threshold
// sorted by similarity desc. Mirrors FindSimilarJarvisLessons.
func (db *DB) FindSimilarRunCache(queryEmbedding []float32, k int, profile string, threshold float64) ([]SimilarRunCache, error) {
	rows, err := db.conn.Query(`
		SELECT id, profile, question, question_embedding, answer, sources_json,
		       confidence, hit_count, created, last_used, source_run_id
		FROM run_cache
		WHERE question_embedding IS NOT NULL
		  AND (? = '' OR profile = ? OR profile = '')
		ORDER BY created DESC
		LIMIT 500`, profile, profile)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SimilarRunCache
	for rows.Next() {
		var r RunCacheRecord
		var embBlob []byte
		var sourcesJSON string
		if err := rows.Scan(
			&r.ID, &r.Profile, &r.Question, &embBlob, &r.Answer, &sourcesJSON,
			&r.Confidence, &r.HitCount, &r.Created, &r.LastUsed, &r.SourceRunID,
		); err != nil {
			continue
		}
		json.Unmarshal([]byte(sourcesJSON), &r.Sources)

		if len(embBlob) == 0 {
			continue
		}
		stored := DecodeVector(embBlob)
		sim := CosineSimilarity(queryEmbedding, stored)
		if sim > threshold {
			r.Embedding = stored
			results = append(results, SimilarRunCache{Record: r, Similarity: sim})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})
	if len(results) > k {
		results = results[:k]
	}
	return results, nil
}

// TouchRunCache increments hit_count and records last_used for a cache hit.
func (db *DB) TouchRunCache(id string, usedAt string) error {
	_, err := db.conn.Exec(
		`UPDATE run_cache SET hit_count = hit_count + 1, last_used = ? WHERE id = ?`,
		usedAt, id,
	)
	return err
}

// RunCacheCount returns the total number of cached questions.
func (db *DB) RunCacheCount() int {
	var count int
	db.conn.QueryRow("SELECT COUNT(*) FROM run_cache").Scan(&count)
	return count
}

// ClearRunCache deletes every row. MineRuns (a later phase) does a full
// rebuild each time it runs - simplicity over incremental merge for this
// first pass - so it needs a way to clear the table before repopulating.
func (db *DB) ClearRunCache() error {
	_, err := db.conn.Exec(`DELETE FROM run_cache`)
	return err
}

// runCacheSimilarityThreshold is the cosine floor for a Tier-1 cache hit.
// Deliberately much stricter than the lessons (0.15) / Jarvis lessons (0.25)
// floors: those feed soft context into an LLM that still reasons over it,
// but a Tier-1 hit returns the cached answer VERBATIM with no LLM call to
// catch a bad match. Precision must dominate recall here.
const runCacheSimilarityThreshold = 0.90

// runCacheMinMargin is the minimum similarity gap required between the
// top-1 and top-2 candidates when they carry DIFFERENT cached answers.
// Guards against confidently serving the wrong one of two near-tied,
// genuinely distinct cached questions. If the top two candidates happen
// to share the same cached answer, the margin doesn't matter -- either
// pick reaches the same user-visible result.
const runCacheMinMargin = 0.02

// runCacheMaxAge bounds worst-case staleness for a Tier-1 hit. The mining-
// time admission filter (runEligibleForMining / looksTimeSensitive,
// mine.go) is the PRIMARY defense against caching an answer that goes
// stale -- this is only the backstop for whatever slips past it anyway (a
// temporal marker looksTimeSensitive doesn't know about yet, a question
// that looked stable but whose real-world answer drifted regardless). A
// record older than this is refused at lookup time no matter how good its
// cosine match is.
const runCacheMaxAge = 30 * 24 * time.Hour

// runCacheExpired reports whether a run_cache record is older than
// runCacheMaxAge as of now. An empty or unparseable Created is treated as
// EXPIRED (fail closed) -- the record's age can't be established, so the
// safe assumption is that it's too old to trust rather than that it's
// fresh. Mirrors runEligibleForMining rejecting a zero StartedAt at mining
// time for the identical reason.
func runCacheExpired(r RunCacheRecord, now time.Time) bool {
	if r.Created == "" {
		return true
	}
	created, err := time.Parse(time.RFC3339, r.Created)
	if err != nil {
		return true
	}
	return now.Sub(created) > runCacheMaxAge
}

// embedQueryFunc abstracts single-text embedding generation so
// LookupRunCache's matching logic is testable without a live Voyage key.
// Mirrors EmbedFunc's role for MineRuns (mine.go).
type embedQueryFunc func(ctx context.Context, text string) ([]float32, error)

// LookupRunCache checks the Tier-1 mined-question cache for a confident
// match to question. Returns (hit, true, nil) on a confident hit, or
// (RunCacheRecord{}, false, nil) on a miss/low-confidence match. Errors
// are only returned for real failures (DB error); an unavailable
// embedding client is treated as a miss, not an error, so callers can
// fall through to the full pipeline unconditionally.
//
// Deliberately embeds question via EmbedDocument, not EmbedQuery: run_cache's
// question_embedding column is written by MineRuns using EmbedDocuments
// (mine.go), and Voyage's "query"/"document" input types are asymmetric --
// even verbatim-identical text lands at ~0.81 cosine across types, well
// under the 0.90 floor below. Comparing across types silently turned every
// lookup into a near-guaranteed miss (caught via a real ~/.aida/brain.db
// after mining genuinely populated run_cache); using the same input type on
// both sides restores ~1.0 for an exact repeat and a wide margin for a
// genuine paraphrase.
func (b *Brain) LookupRunCache(ctx context.Context, question string, profile string) (RunCacheRecord, bool, error) {
	if b == nil || b.Embeddings == nil || !b.Embeddings.Available() {
		return RunCacheRecord{}, false, nil
	}
	return b.lookupRunCache(ctx, question, profile, b.Embeddings.EmbedDocument, time.Now)
}

// lookupRunCache is the injectable core -- embed and now are both
// swappable in tests (now mirrors embed's role: it lets the TTL backstop
// below be tested against a fixed clock instead of the real wall-clock).
func (b *Brain) lookupRunCache(ctx context.Context, question, profile string, embed embedQueryFunc, now func() time.Time) (RunCacheRecord, bool, error) {
	if b == nil || b.DB == nil || embed == nil {
		return RunCacheRecord{}, false, nil
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return RunCacheRecord{}, false, nil
	}
	emb, err := embed(ctx, question)
	if err != nil || len(emb) == 0 {
		return RunCacheRecord{}, false, nil
	}
	// FindSimilarRunCache fetches only the top k=2 candidates -- fine even
	// after the expiry filter below shrinks the set further, since
	// pickRunCacheHit already handles 0 or 1 remaining candidates correctly
	// (miss, or an unconditional hit with no margin to check).
	results, err := b.DB.FindSimilarRunCache(emb, 2, profile, runCacheSimilarityThreshold)
	if err != nil {
		return RunCacheRecord{}, false, err
	}

	// Drop expired records BEFORE pickRunCacheHit's margin guard runs, not
	// after. Filtering afterward would let an expired top-1 -- which is
	// never actually going to be served -- still suppress a perfectly
	// valid, fresh top-2 by forcing pickRunCacheHit's "distinct answers
	// within margin -> ambiguous, miss" rule to fire against a candidate
	// that was already dead. Filtering first lets a fresh top-2 stand on
	// its own.
	nowVal := now()
	fresh := results[:0]
	for _, res := range results {
		if !runCacheExpired(res.Record, nowVal) {
			fresh = append(fresh, res)
		}
	}

	return pickRunCacheHit(fresh)
}

// pickRunCacheHit applies the margin guard: ambiguous near-ties between
// DISTINCT cached answers are rejected as a miss; ties sharing the same
// answer are fine since either pick reaches the same user-visible result.
// Pure and DB-free so it's directly unit-testable.
func pickRunCacheHit(results []SimilarRunCache) (RunCacheRecord, bool, error) {
	if len(results) == 0 {
		return RunCacheRecord{}, false, nil
	}
	if len(results) >= 2 {
		top, second := results[0], results[1]
		if top.Record.Answer != second.Record.Answer && (top.Similarity-second.Similarity) < runCacheMinMargin {
			return RunCacheRecord{}, false, nil // ambiguous -- fall through
		}
	}
	return results[0].Record, true, nil
}
