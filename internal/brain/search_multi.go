package brain

// Multi-channel retrieval with Reciprocal Rank Fusion - Action #1d of
// the harness roadmap.
//
// Channels (each returns a ranked list of doc IDs):
//
//   vector - Voyage embedding cosine similarity over lessons.
//   fts - SQLite FTS5 keyword search over lessons + memory + wiki
//               bodies (one shared corpus_fts table across all three).
//   substring - fallback raw-token-overlap scan (catches IDs / ARI strings
//               where embeddings drift and tokenizer-stemmed FTS misses).
//   fact-key - exact-key lookup against typed memory facts/instructions
//               whose key matches a parsed entity.
//   wiki - Voyage embedding cosine similarity over indexed wiki pages
//               (Phase C: `aida wiki index`, internal/brain/wiki_index.go).
//               Keyword coverage over the same pages comes for free via
//               the "fts" channel above - wiki_index.go writes "wiki:"
//               doc_ids into the shared corpus_fts table, so this channel
//               only needs to add the vector signal FTS can't provide.
//   knowledge - Voyage embedding cosine similarity over indexed
//               brain/knowledge/domains pages (aida brain index,
//               internal/brain/knowledge_index.go). Same split as wiki:
//               keyword coverage rides the shared corpus_fts table via
//               "knowledge:" doc_ids, this channel adds the vector side.
//
// HyDE (generate hypothetical answer, embed, run vector search again) is
// the canonical 6th channel from Cloudflare's Agent Memory beta. We skip
// it in v1 - it requires an extra LLM call and the marginal recall lift
// over good embeddings is small for the kinds of facts aida stores.
// Adding HyDE is a follow-up: implement as another channel returning
// ranked IDs, no other call sites change.
//
// Fusion: standard Reciprocal Rank Fusion. score(d) = Σ over channels c
// of 1 / (k + rank_c(d)) with k=60. Documents found by multiple channels
// dominate documents found by only one, regardless of any single
// channel's score scale - that's the point.

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/ui"
)

// rrfConst is the additive constant in 1 / (k + rank). Cormack et al.'s
// original paper used k=60; the value isn't sensitive within an order
// of magnitude.
const rrfConst = 60.0

// Decay + use-count recall scoring (Atlas pattern). Fused scores are
// multiplied by a recency decay (gaussian past a grace offset, floored so
// stale-but-relevant docs never vanish) and a retrieval-practice boost
// (recalling a memory keeps it fresh - relevance decay, not truth decay;
// truth is supersession's job). Instructions are exempt from decay:
// recency ≠ effectiveness for standing rules.
const (
	decayOffsetDays = 180.0  // grace period before decay starts
	decayScaleDays  = 1825.0 // gaussian scale (~5 years)
	decayFloor      = 0.5    // decay multiplier never drops below this
	useCountWeight  = 0.2    // boost = 1 + log10(1+use_count) * this
)

// SearchChannel identifies which retrieval channel found a document.
type SearchChannel string

const (
	ChannelVector    SearchChannel = "vector"
	ChannelFTS       SearchChannel = "fts"
	ChannelSubstring SearchChannel = "substring"
	ChannelFactKey   SearchChannel = "fact-key"
	ChannelWiki      SearchChannel = "wiki"
	ChannelKnowledge SearchChannel = "knowledge"
)

// MultiResult is one fused retrieval hit.
type MultiResult struct {
	// DocID is "lesson:<id>", "memory:<id>", "wiki:<slug>", or
	// "knowledge:<slug>" - the same key used in the FTS5 corpus. Caller
	// can split on ":" to dispatch to the right table for full content.
	DocID string
	// DocType is one of "lesson", "memory:fact", "memory:event",
	// "memory:instruction", "wiki:page", "knowledge:domain".
	DocType string
	// Body is a short representative snippet (the question for
	// lessons, the body for memory records) suitable for prompt
	// injection without further processing.
	Body string
	// Channels maps each channel that found this doc to its 1-based
	// rank within that channel. Documents found by multiple channels
	// have multiple entries.
	Channels map[SearchChannel]int
	// Score is the RRF-fused score: Σ 1/(rrfConst + rank), scaled by
	// recency decay and use-count boost after hydration.
	Score float64
	// Created is the doc's creation timestamp (lessons: the lesson
	// timestamp; memory: the record's created). Decay anchor when the
	// doc has never been recalled.
	Created string
	// LastUsedAt / UseCount are recall-practice signals (memory docs
	// only; lessons aren't bumped). LastUsedAt, when set, replaces
	// Created as the decay anchor.
	LastUsedAt string
	UseCount   int
}

// SearchMulti runs every available retrieval channel against the brain
// and returns up to k results, ordered by RRF score descending.
//
// It is additive over Brain.Search: existing call sites that need the
// SearchContext shape still use Search. New callers (the agent, future
// router work) can use SearchMulti for higher-recall lookup, especially
// when typed memory (Action #1a) entries carry the right answer for an
// entity-keyed question.
//
// Channels that fail (e.g. FTS5 disabled, embeddings unavailable) are
// silently dropped - RRF degrades gracefully when fewer channels
// contribute. Callers get fewer results, not an error.
func (b *Brain) SearchMulti(ctx context.Context, question string, entities []string, k int) ([]MultiResult, error) {
	if k <= 0 {
		k = 10
	}

	// Query embedding, computed once and shared by every vector-based
	// channel below (lessons + wiki + knowledge) rather than re-embedding
	// per channel.
	rankings := make(map[SearchChannel][]string)
	var queryEmbedding []float32
	if b.Embeddings != nil && b.Embeddings.Available() {
		emb, err := b.Embeddings.EmbedQuery(ctx, question)
		if err != nil {
			ui.PrintVerbose("MultiSearch embed", "embed failed: "+err.Error())
		} else {
			queryEmbedding = emb
		}
	}

	// Channel 1: vector (lessons only). Reuses the existing
	// FindSimilar path - same embedding model, same profile filter.
	if queryEmbedding != nil {
		lessons, err := b.DB.FindSimilar(queryEmbedding, k*2, b.profile)
		if err == nil {
			for _, l := range lessons {
				rankings[ChannelVector] = append(rankings[ChannelVector], "lesson:"+l.Lesson.ID)
			}
		}
	}

	// Channel 2: FTS5. Tokenize question into a keyword query - FTS5
	// expects MATCH syntax, which is space-separated tokens by default.
	if ftsHits, err := b.DB.FTSSearch(question, k*2, b.profile); err == nil {
		rankings[ChannelFTS] = ftsHits
	} else {
		ui.PrintVerbose("MultiSearch fts", "skip: "+err.Error())
	}

	// Channel 3: substring scan. Useful for short literal tokens
	// (entity IDs, partner ARIs) that FTS5 may stem away or vector
	// search may dilute.
	if substrHits, err := b.DB.SubstringSearch(question, k*2, b.profile); err == nil {
		rankings[ChannelSubstring] = substrHits
	}

	// Channel 4: fact-key lookup against typed memory. For each
	// parsed entity, look up an active record (any type) whose key
	// matches. Facts/instructions supersede by key so there's at
	// most one active record; events don't supersede but can still
	// be keyed (e.g. ingest-source:<hash> on a source prose event).
	// We check all three types so retrieval works regardless of the
	// type the writer chose.
	for _, e := range entities {
		raw := strings.TrimSpace(e)
		if raw == "" {
			continue
		}
		// Try the entity verbatim and lowercased - different writers
		// store keys in different cases (slug-style "user-units" vs
		// hex-prefixed "ingest-source:abc123"); this catches both.
		variants := []string{raw}
		if lower := strings.ToLower(raw); lower != raw {
			variants = append(variants, lower)
		}
		seen := map[string]bool{}
		for _, k := range variants {
			for _, t := range []MemoryType{MemoryFact, MemoryInstruction, MemoryEvent} {
				if rec, err := b.DB.ActiveMemoryByKey(t, k, b.profile); err == nil && rec != nil {
					if seen[rec.ID] {
						continue
					}
					seen[rec.ID] = true
					rankings[ChannelFactKey] = append(rankings[ChannelFactKey], "memory:"+rec.ID)
				}
			}
		}
	}

	// Channel 5: wiki (Phase C). Vector search over wiki_pages'
	// embeddings (aida wiki index; wiki_index.go). The keyword side is
	// already covered - wiki_index.go writes "wiki:<slug>" rows into
	// the same corpus_fts table Channel 2 (fts, above) queries, so wiki
	// pages surface there automatically with no code change to that
	// channel.
	if queryEmbedding != nil {
		pages, err := b.DB.FindSimilarWikiPages(queryEmbedding, k*2)
		if err == nil {
			for _, p := range pages {
				rankings[ChannelWiki] = append(rankings[ChannelWiki], "wiki:"+p.Slug)
			}
		}
	}

	// Channel 6: knowledge-domain pages (knowledge_index.go). Vector
	// search over knowledge_pages' embeddings; the keyword side is again
	// already covered by the "knowledge:<slug>" rows in corpus_fts.
	if queryEmbedding != nil {
		pages, err := b.DB.FindSimilarKnowledgePages(queryEmbedding, k*2)
		if err == nil {
			for _, p := range pages {
				rankings[ChannelKnowledge] = append(rankings[ChannelKnowledge], "knowledge:"+p.Slug)
			}
		}
	}

	fused := rrfFuse(rankings)
	if len(fused) > k {
		fused = fused[:k]
	}

	// Hydrate each result with type + body. We do this after RRF so
	// we don't waste DB reads on docs that lose the rank battle.
	for i := range fused {
		hydrateMultiResult(b.DB, &fused[i])
	}

	// Decay + use-count scoring, then re-rank. Applied post-hydration
	// (the anchors live on the hydrated fields), so it reorders within
	// the fused top-k rather than re-running the rank battle.
	now := time.Now().UTC()
	var bumpIDs []string
	for i := range fused {
		r := &fused[i]
		decay := 1.0
		if r.DocType != "memory:instruction" {
			anchor := r.LastUsedAt
			if anchor == "" {
				anchor = r.Created
			}
			decay = recallDecay(anchor, now)
		}
		boost := 1 + math.Log10(1+float64(r.UseCount))*useCountWeight
		r.Score *= decay * boost
		if id, ok := strings.CutPrefix(r.DocID, "memory:"); ok {
			bumpIDs = append(bumpIDs, id)
		}
	}
	sort.SliceStable(fused, func(i, j int) bool { return fused[i].Score > fused[j].Score })

	// Retrieval practice: bump the returned memory docs after scoring,
	// so a query's own bump doesn't inflate its results. Lessons aren't
	// bumped (lesson recall isn't tracked yet).
	if err := b.DB.BumpMemoryUse(bumpIDs, now); err != nil {
		ui.PrintVerbose("MultiSearch bump", "failed: "+err.Error())
	}
	return fused, nil
}

// recallDecay maps the age of a doc's decay anchor to a (decayFloor, 1.0]
// multiplier: 1.0 inside the grace offset, then a gaussian over
// decayScaleDays, floored. Unparseable anchors decay nothing - better to
// leave a doc unscaled than to bury it on a formatting quirk.
func recallDecay(anchor string, now time.Time) float64 {
	if anchor == "" {
		return 1.0
	}
	t, err := time.Parse(time.RFC3339, anchor)
	if err != nil {
		return 1.0
	}
	ageDays := now.Sub(t).Hours() / 24
	if ageDays <= decayOffsetDays {
		return 1.0
	}
	x := ageDays - decayOffsetDays
	d := math.Exp(-(x * x) / (2 * decayScaleDays * decayScaleDays))
	if d < decayFloor {
		return decayFloor
	}
	return d
}

// BumpMemoryUse records a recall of the given memory records: sets
// last_used_at and increments use_count in one batched UPDATE. DB-only by
// design - the JSON file mirrors are not touched (see MemoryRecord).
func (db *DB) BumpMemoryUse(ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := []any{now.Format(time.RFC3339)}
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	_, err := db.conn.Exec(
		`UPDATE memory_records
		    SET last_used_at = ?, use_count = use_count + 1
		  WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	return err
}

// rrfFuse applies Reciprocal Rank Fusion across all per-channel rank
// lists and returns documents sorted by score descending.
func rrfFuse(rankings map[SearchChannel][]string) []MultiResult {
	scores := make(map[string]*MultiResult)
	for ch, ids := range rankings {
		for rank, id := range ids {
			r, ok := scores[id]
			if !ok {
				r = &MultiResult{
					DocID:    id,
					Channels: make(map[SearchChannel]int),
				}
				scores[id] = r
			}
			r.Channels[ch] = rank + 1
			r.Score += 1.0 / (rrfConst + float64(rank+1))
		}
	}
	out := make([]MultiResult, 0, len(scores))
	for _, r := range scores {
		out = append(out, *r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// hydrateMultiResult fills Body and DocType from the appropriate table.
// Failures leave the existing fields (DocID + Channels + Score) intact
// so the result is still useful even if hydration fails.
func hydrateMultiResult(db *DB, r *MultiResult) {
	parts := strings.SplitN(r.DocID, ":", 2)
	if len(parts) != 2 {
		return
	}
	switch parts[0] {
	case "lesson":
		var question, snippet, timestamp string
		err := db.conn.QueryRow(
			"SELECT question, COALESCE(answer_snippet, ''), COALESCE(timestamp, '') FROM lessons WHERE id = ?",
			parts[1],
		).Scan(&question, &snippet, &timestamp)
		if err != nil {
			return
		}
		r.DocType = "lesson"
		r.Body = question
		if snippet != "" {
			r.Body += "\n→ " + snippet
		}
		r.Created = timestamp
	case "memory":
		m, err := db.GetMemory(parts[1])
		if err != nil || m == nil {
			return
		}
		r.DocType = "memory:" + string(m.Type)
		r.Body = m.Body
		r.Created = m.Created
		r.LastUsedAt = m.LastUsedAt
		r.UseCount = m.UseCount
	case "wiki":
		p, err := db.GetWikiPage(parts[1])
		if err != nil || p == nil {
			return
		}
		r.DocType = "wiki:page"
		snippet := p.Body
		const maxSnippet = 300
		if len(snippet) > maxSnippet {
			snippet = snippet[:maxSnippet] + "..."
		}
		r.Body = p.Title + "\n" + strings.TrimSpace(snippet)
		// Decay anchor: the page's own frontmatter timestamp when known,
		// falling back to indexed_at (e.g. a page whose frontmatter
		// never set timestamp/updated/created) so decay still has
		// something to work with rather than treating it as ageless.
		r.Created = p.PageTimestamp
		if r.Created == "" {
			r.Created = p.IndexedAt
		}
	case "knowledge":
		p, err := db.GetKnowledgePage(parts[1])
		if err != nil || p == nil {
			return
		}
		r.DocType = KnowledgeDocType
		snippet := p.Body
		const maxSnippet = 300
		if len(snippet) > maxSnippet {
			snippet = snippet[:maxSnippet] + "..."
		}
		r.Body = p.Title + "\n" + strings.TrimSpace(snippet)
		// Domain pages carry no frontmatter timestamp; indexed_at is the
		// only anchor available. Curated pages are kept current by hand,
		// so anchoring decay to the last index run is the honest choice.
		r.Created = p.IndexedAt
	}
}

// FTSSearch runs an FTS5 MATCH query over the corpus_fts table and
// returns doc_ids in MATCH-rank order (best first). Empty query or
// FTS5-unsupported builds return an empty slice with no error so the
// caller can degrade gracefully.
func (db *DB) FTSSearch(question string, limit int, profile string) ([]string, error) {
	q := buildFTSQuery(question)
	if q == "" {
		return nil, nil
	}
	rows, err := db.conn.Query(`
		SELECT doc_id FROM corpus_fts
		 WHERE corpus_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		q, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out, nil
}

// FTSSearchByType is FTSSearch restricted to one corpus_fts doc_type
// (e.g. KnowledgeDocType), for callers that want a single corpus's
// keyword hits without them competing for the limit against every
// lesson and memory row. Returns doc_ids in MATCH-rank order.
func (db *DB) FTSSearchByType(question, docType string, limit int) ([]string, error) {
	q := buildFTSQuery(question)
	if q == "" {
		return nil, nil
	}
	rows, err := db.conn.Query(`
		SELECT doc_id FROM corpus_fts
		 WHERE corpus_fts MATCH ? AND doc_type = ?
		 ORDER BY rank
		 LIMIT ?`,
		q, docType, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out, nil
}

// buildFTSQuery turns a free-text question into an FTS5 MATCH query.
// Strategy: split on whitespace, drop tokens shorter than 3 chars (mostly
// stopwords), wrap the rest with OR. Returns "" when nothing usable
// remains.
func buildFTSQuery(question string) string {
	tokens := strings.Fields(strings.ToLower(question))
	keep := tokens[:0]
	for _, t := range tokens {
		t = strings.Trim(t, ".,?!:;'\"`()[]{}")
		// FTS5's syntax treats some chars as operators - strip them.
		t = strings.Map(func(r rune) rune {
			if r == '"' || r == '*' || r == '(' || r == ')' || r == ':' || r == '-' || r == '+' {
				return ' '
			}
			return r
		}, t)
		t = strings.TrimSpace(t)
		if len(t) >= 3 {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		return ""
	}
	return strings.Join(keep, " OR ")
}

// SubstringSearch is a fallback channel that ranks lessons + memory
// records by the count of distinct query tokens that appear as
// substrings of their body. Cheap, no external dependencies, and
// catches literal IDs / ARIs that FTS5 may tokenize differently.
func (db *DB) SubstringSearch(question string, limit int, profile string) ([]string, error) {
	tokens := strings.Fields(strings.ToLower(question))
	if len(tokens) == 0 {
		return nil, nil
	}
	// Drop very common short tokens.
	var keep []string
	for _, t := range tokens {
		if len(t) >= 3 {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		return nil, nil
	}

	type rankedDoc struct {
		id    string
		score int
	}
	var ranked []rankedDoc

	// Scan lessons. Superseded rows (retracted by a later feedback
	// lesson for the same run) are excluded - same recall-filtering rule
	// FindSimilar applies for the vector channel.
	rows, err := db.conn.Query(`SELECT id, LOWER(question || ' ' || COALESCE(answer_snippet, '')) FROM lessons WHERE COALESCE(superseded, 0) = 0 AND (? = '' OR profile = ? OR profile = '') LIMIT 1000`, profile, profile)
	if err == nil {
		for rows.Next() {
			var id, body string
			if rows.Scan(&id, &body) != nil {
				continue
			}
			score := countTokenHits(body, keep)
			if score > 0 {
				ranked = append(ranked, rankedDoc{"lesson:" + id, score})
			}
		}
		rows.Close()
	}

	// Scan active memory records.
	rows, err = db.conn.Query(`SELECT id, LOWER(body) FROM memory_records WHERE superseded_by = '' AND (? = '' OR profile = ? OR profile = '') LIMIT 1000`, profile, profile)
	if err == nil {
		for rows.Next() {
			var id, body string
			if rows.Scan(&id, &body) != nil {
				continue
			}
			score := countTokenHits(body, keep)
			if score > 0 {
				ranked = append(ranked, rankedDoc{"memory:" + id, score})
			}
		}
		rows.Close()
	}

	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.id
	}
	return out, nil
}

func countTokenHits(body string, tokens []string) int {
	hits := 0
	for _, t := range tokens {
		if strings.Contains(body, t) {
			hits++
		}
	}
	return hits
}
