package brain

// Scope-aware recall over captured typed-memory records - Phase 3 of the
// Claude Code <-> Aida memory bridge. Capture (Phase 1) embeds every
// mirrored Claude memory into memory_records.body_embedding; until now
// nothing read that embedding for retrieval. This adds two recall modes
// over those records:
//
//	semantic - embed the query, rank active records by cosine similarity.
//	recency - list the newest records by created DESC (for "what's my
//	           last/latest memory" questions, which are recency- not
//	           similarity-shaped).
//
// Both are scope-aware: a project-scoped query sees scope:global records
// plus its own project's, never another project's. This is deliberately a
// separate path from Brain.Search / SearchMulti (lesson/entity-shaped), so
// those callers are untouched.

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/ryanlitalien/aida/internal/ui"
)

// ClaudeMemoryProfile is the fixed profile every captured Claude Code
// memory is pinned to (see internal/cli/brain_capture.go). Recall defaults
// to it so Claude memories are reachable regardless of the caller's own
// auto-detected work/home profile.
const ClaudeMemoryProfile = "claude"

const (
	// memoryRecallThreshold is the cosine floor for a semantic hit. Matches
	// the lessons vector channel (FindSimilar) - lenient enough that a
	// paraphrase still lands, strict enough to drop unrelated records.
	memoryRecallThreshold = 0.15
	// memoryRecallScanLimit caps how many active records are loaded per
	// recall before scope-filtering + ranking. Far above the current corpus
	// size; a backstop, not a real limit.
	memoryRecallScanLimit = 2000
	// defaultRecallK is the result count when the caller passes k <= 0.
	defaultRecallK = 5
)

// SimilarMemory pairs a memory record with its cosine similarity to a query
// embedding. Similarity is 0 for recency-ordered results.
type SimilarMemory struct {
	Record     MemoryRecord
	Similarity float64
}

// RecallResult is the outcome of Brain.RecallMemories. BySimilarity reports
// which mode produced Memories: true = semantic (Similarity is meaningful),
// false = recency (newest-first, Similarity is 0).
type RecallResult struct {
	Memories     []SimilarMemory
	BySimilarity bool
}

// recencyRe matches recency-shaped queries ("what's my last memory",
// "latest note", ...). Word-bounded so it doesn't fire on "lasting" etc.
var recencyRe = regexp.MustCompile(`\b(last|latest|newest|recent|recently)\b`)

// LooksRecencyShaped reports whether a query reads as a recency request
// rather than a semantic one. Callers use it to pick the recall mode when
// the user did not ask explicitly.
func LooksRecencyShaped(query string) bool {
	return recencyRe.MatchString(strings.ToLower(query))
}

// memoryTopicRe matches phrasing that asks to recall something previously
// captured, as opposed to a general knowledge/data question. Deliberately
// narrower than the recall tools' LLM-judged triggers (claude_memory_recall,
// brain_recall) - this gates a real embedding call out of the general query
// pipeline, so it favors precision over recall: a missed match still falls
// back to the existing lessons/entity search, while a false-positive match
// only costs one wasted vector search.
var memoryTopicRe = regexp.MustCompile(`(?i)\b(what do you (know|remember)|what did (i|you) (say|save|tell|note|mention|build|make|create)|do you remember|did i (tell|mention|say|ask|build|make|create|set up|write|add)|my (preference|note)s? (on|about|for)|(last|latest|newest) (claude )?memory|remind me what)\b`)

// LooksMemoryShaped reports whether a query is asking to recall something
// previously captured (a Claude Code memory), as opposed to a general
// knowledge, data, or codebase question. Used to gate RecallMemories out of
// the general aida <query> pipeline so it only pays the embedding+search
// cost on questions plausibly shaped that way.
func LooksMemoryShaped(query string) bool {
	return memoryTopicRe.MatchString(query)
}

// recallEligible reports whether a record should surface in recall. It drops
// two kinds of non-episodic artifacts - both still captured, stored, and
// git-backed-up, just not surfaced by retrieval - applied before the top-k
// cut so filtered records don't consume result slots:
//
//   - "…:MEMORY" index blobs: mirrors of ~/.claude/**/MEMORY.md whose every
//     line is already captured as its own record, so surfacing the index just
//     repeats content as one noisy blob (the "duplicate every entry as one
//     blob" failure the design doc warned about).
//   - claude:global:CLAUDE: the captured global instruction file. It is
//     standing directives, not an episodic memory - already loaded into every
//     Claude Code session's context, its individual rules captured separately,
//     and it truncates mid-bullet when recited. Kept for durable backup; out
//     of recall.
func recallEligible(rec MemoryRecord) bool {
	if rec.Key == "claude:global:CLAUDE" {
		return false
	}
	return !strings.HasSuffix(rec.Key, ":MEMORY")
}

// memoryScopeMatch reports whether a record with the given tags falls
// within scope. Empty scope matches everything. "global" matches only
// scope:global records. "project:<slug>" matches scope:global records plus
// records tagged project:<slug> - a project session sees global memories
// plus its own, never another project's. An unrecognized scope spec is
// permissive (matches everything) rather than silently dropping all results.
func memoryScopeMatch(tags []string, scope string) bool {
	if scope == "" {
		return true
	}
	isGlobal := false
	for _, t := range tags {
		if t == "scope:global" {
			isGlobal = true
			break
		}
	}
	if scope == "global" {
		return isGlobal
	}
	projSlug, ok := strings.CutPrefix(scope, "project:")
	if !ok || projSlug == "" {
		return true
	}
	if isGlobal {
		return true
	}
	want := "project:" + projSlug
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// memoryHasTag reports whether tags contains tag (case-insensitive). An
// empty tag matches everything - the "no tag filter" case, same
// convention as memoryScopeMatch's empty scope.
func memoryHasTag(tags []string, tag string) bool {
	if tag == "" {
		return true
	}
	for _, t := range tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// FindSimilarMemories returns up to k active memory records whose body
// embedding is cosine-similar (above memoryRecallThreshold) to
// queryEmbedding, most-similar first. Filtered to the given profile,
// optionally to a tag (record must carry it, case-insensitive; e.g. a
// meetily classifier tag like "cta"), and, when scope is supplied, to that
// scope (global + the named project). Both filters are applied over the
// full candidate set before the top-k cut so a filtered query never
// starves because unfiltered records won the ranking.
func (db *DB) FindSimilarMemories(queryEmbedding []float32, k int, profile, tag string, scope ...string) ([]SimilarMemory, error) {
	if len(queryEmbedding) == 0 || k <= 0 {
		return nil, nil
	}
	scopeSpec := ""
	if len(scope) > 0 {
		scopeSpec = scope[0]
	}
	recs, err := db.ListMemory(MemoryListOpts{
		Profile: profile,
		Limit:   memoryRecallScanLimit,
	})
	if err != nil {
		return nil, err
	}
	var out []SimilarMemory
	for _, r := range recs {
		if len(r.Embedding) == 0 {
			continue
		}
		if !recallEligible(r) {
			continue
		}
		if !memoryScopeMatch(r.Tags, scopeSpec) {
			continue
		}
		if !memoryHasTag(r.Tags, tag) {
			continue
		}
		sim := CosineSimilarity(queryEmbedding, r.Embedding)
		if sim > memoryRecallThreshold {
			out = append(out, SimilarMemory{Record: r, Similarity: sim})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// RecallMemories recalls up to k memory records relevant to query. It picks
// a mode: recency (newest-first, no embedding) when recent is set, the query
// is empty, or the query is recency-shaped; otherwise semantic (embed +
// cosine). The caller supplies the profile ("all" or "*" disables the
// profile filter). scope, when non-empty, restricts to global + the named
// project scope. tag, when non-empty, restricts to records carrying that
// tag (case-insensitive) - e.g. a meetily classifier tag like "cta", to
// recall only that project's calls. Semantic recall with no embedding
// client available falls back to recency so the caller still gets results.
func (b *Brain) RecallMemories(ctx context.Context, query string, k int, profile, scope, tag string, recent bool) (RecallResult, error) {
	if b == nil || b.DB == nil {
		return RecallResult{}, nil
	}
	if k <= 0 {
		k = defaultRecallK
	}
	effProfile := profile
	if profile == "all" || profile == "*" {
		effProfile = ""
	}
	query = strings.TrimSpace(query)

	if recent || query == "" || LooksRecencyShaped(query) {
		mems, err := b.recencyRecall(effProfile, scope, tag, k)
		return RecallResult{Memories: mems, BySimilarity: false}, err
	}

	var emb []float32
	if b.Embeddings != nil && b.Embeddings.Available() {
		e, err := b.Embeddings.EmbedQuery(ctx, query)
		if err != nil {
			ui.PrintVerbose("Memory recall", "embed failed: "+err.Error())
		} else {
			emb = e
		}
	}
	if len(emb) == 0 {
		mems, err := b.recencyRecall(effProfile, scope, tag, k)
		return RecallResult{Memories: mems, BySimilarity: false}, err
	}

	sims, err := b.DB.FindSimilarMemories(emb, k, effProfile, tag, scope)
	if err != nil {
		return RecallResult{}, err
	}
	return RecallResult{Memories: sims, BySimilarity: true}, nil
}

// recencyRecall lists the newest active records for effProfile that match
// scope and tag, newest-first, capped at k. Shared by RecallMemories'
// explicit --recent path and its no-embedding fallback.
func (b *Brain) recencyRecall(effProfile, scope, tag string, k int) ([]SimilarMemory, error) {
	recs, err := b.DB.ListMemory(MemoryListOpts{Profile: effProfile, Limit: memoryRecallScanLimit})
	if err != nil {
		return nil, err
	}
	out := make([]SimilarMemory, 0, k)
	for _, r := range recs {
		if !recallEligible(r) {
			continue
		}
		if !memoryScopeMatch(r.Tags, scope) {
			continue
		}
		if !memoryHasTag(r.Tags, tag) {
			continue
		}
		out = append(out, SimilarMemory{Record: r})
		if len(out) >= k {
			break
		}
	}
	return out, nil
}
