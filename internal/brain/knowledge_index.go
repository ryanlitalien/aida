package brain

// Knowledge-domain page index. brain/knowledge/domains/*.md holds two
// kinds of page under one directory: generated source profiles (`aida
// index --generate` / compile_sources.go, carrying a "When To Route Here"
// section that domain.go's LoadDomainKeywords greps for planner routing)
// and hand-written prose pages (an LLC's filing facts, a writing-style
// guide, a home-network map) with no Keywords line at all. Before this
// file existed Brain.Index only counted the directory, so neither kind
// was ever embedded or written to corpus_fts - brain_search could not
// find them. Now every page is indexed regardless of shape; the routing
// keyword path in domain.go is untouched and keeps reading the files
// directly.
//
// Shape mirrors wiki_index.go: a knowledge_pages table + a
// "knowledge:<slug>" row in the shared corpus_fts channel with doc_type
// "knowledge:domain", so SearchMulti's fts channel finds them for free and
// hydrateMultiResult dispatches on the prefix. Files are the source of
// truth; the table is a derived index, safe to delete and rebuild via
// `aida brain index`.
//
// Incremental: a page is only re-embedded when it has never been indexed,
// its mtime is newer than its indexed_at, or it has no embedding yet while
// an embedding client is now available. Pages deleted from disk are pruned
// from the table (and corpus_fts) on the next run.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/ui"
)

// KnowledgeDocType is the corpus_fts doc_type for knowledge-domain pages.
const KnowledgeDocType = "knowledge:domain"

// knowledgeEmbedMaxChars caps the text sent to the embedding API per page.
// The largest domain page today is ~48KB; voyage-4-lite accepts 32k tokens
// per input, so this stays well inside the limit for English prose while
// still covering any realistic page in full. FTS always gets the whole
// body regardless.
const knowledgeEmbedMaxChars = 60000

// KnowledgePageRecord is one indexed knowledge-domain page.
type KnowledgePageRecord struct {
	Slug      string
	Title     string
	Path      string // brain-root-relative, e.g. knowledge/domains/its-llc.md
	Body      string
	Embedding []float32
	IndexedAt string
}

// knowledgeSourceFile is one page discovered on disk, ready to index.
type knowledgeSourceFile struct {
	Slug    string
	Title   string
	Path    string
	Body    string
	ModTime time.Time
}

// KnowledgeIndexResult summarizes one IndexKnowledgeDomains run.
type KnowledgeIndexResult struct {
	Scanned   int  // pages found on disk
	Indexed   int  // pages present in knowledge_pages after the run
	Reindexed int  // subset actually (re)written this run
	Pruned    int  // rows removed because their file is gone
	Embedded  bool // whether embeddings were generated this run
}

// IndexKnowledgeDomains walks brain/knowledge/domains/*.md and (re)indexes
// every page into knowledge_pages + the shared corpus_fts channel.
// Embeddings are best-effort, matching IndexWiki: with no embedding
// client configured the body + FTS rows are still written so keyword
// recall works, and the vector channel fills in once a key is configured
// and the index re-runs.
func (b *Brain) IndexKnowledgeDomains(ctx context.Context) (KnowledgeIndexResult, error) {
	var result KnowledgeIndexResult

	files, err := discoverKnowledgeDomainFiles(b.Path)
	if err != nil {
		return result, fmt.Errorf("discover knowledge domain pages: %w", err)
	}
	result.Scanned = len(files)
	result.Embedded = b.Embeddings != nil && b.Embeddings.Available()

	// Stale = never indexed, file newer than its indexed_at, or indexed
	// without an embedding while one can now be generated.
	var stale []knowledgeSourceFile
	for _, f := range files {
		indexedAt, hasEmbedding, err := b.DB.knowledgePageIndexState(f.Slug)
		if err != nil || f.ModTime.After(indexedAt) || (result.Embedded && !hasEmbedding) {
			stale = append(stale, f)
		}
	}

	// One batched embed call for the stale set (EmbedDocuments chunks at
	// 128 internally). A failed call degrades to nil embeddings for this
	// run rather than aborting; rows still get written below.
	var vecs [][]float32
	if result.Embedded && len(stale) > 0 {
		texts := make([]string, len(stale))
		for i, f := range stale {
			texts[i] = knowledgeEmbedText(f.Title, f.Body)
		}
		vecs, err = b.Embeddings.EmbedDocuments(ctx, texts)
		if err != nil {
			ui.PrintVerbose("Knowledge index embed", "failed: "+err.Error())
			vecs = nil
		}
	}

	// RFC3339Nano for the same reason IndexWiki uses it: whole-second
	// precision would make a file written in the same second as this run
	// look newer than its own indexed_at forever.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i, f := range stale {
		var emb []float32
		if vecs != nil && i < len(vecs) {
			emb = vecs[i]
		}
		if err := b.DB.UpsertKnowledgePage(f.Slug, f.Title, f.Path, f.Body, emb, now); err != nil {
			ui.PrintVerbose("Knowledge index", fmt.Sprintf("upsert %s: %s", f.Slug, err))
			continue
		}
		result.Reindexed++
	}

	// Prune rows whose file no longer exists so a renamed or deleted page
	// doesn't keep surfacing in search.
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		keep[f.Slug] = true
	}
	slugs, err := b.DB.listKnowledgePageSlugs()
	if err == nil {
		for _, s := range slugs {
			if keep[s] {
				result.Indexed++
				continue
			}
			if err := b.DB.DeleteKnowledgePage(s); err == nil {
				result.Pruned++
			}
		}
	}
	return result, nil
}

// discoverKnowledgeDomainFiles lists every *.md page directly under
// brain/knowledge/domains, sorted by name. A missing directory is not an
// error (a fresh brain has nothing there yet).
func discoverKnowledgeDomainFiles(brainPath string) ([]knowledgeSourceFile, error) {
	dir := filepath.Join(brainPath, "knowledge", "domains")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out []knowledgeSourceFile
	for _, name := range names {
		abs := filepath.Join(dir, name)
		info, err := os.Stat(abs)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		body := string(data)
		if strings.TrimSpace(body) == "" {
			continue
		}
		slug := strings.TrimSuffix(name, ".md")
		out = append(out, knowledgeSourceFile{
			Slug:    slug,
			Title:   knowledgePageTitle(slug, body),
			Path:    filepath.ToSlash(filepath.Join("knowledge", "domains", name)),
			Body:    body,
			ModTime: info.ModTime(),
		})
	}
	return out, nil
}

// knowledgePageTitle returns the page's first "# " heading, falling back
// to the slug for pages that open with prose or a table instead.
func knowledgePageTitle(slug, body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# ") {
			if t := strings.TrimSpace(strings.TrimPrefix(line, "# ")); t != "" {
				return t
			}
		}
	}
	return slug
}

// knowledgeEmbedText is the text embedded for a page: title + body, the
// same shape reembed.go's textExpr for this table reconstructs, capped at
// knowledgeEmbedMaxChars.
func knowledgeEmbedText(title, body string) string {
	text := title + "\n" + body
	if len(text) > knowledgeEmbedMaxChars {
		text = text[:knowledgeEmbedMaxChars]
	}
	return text
}

// ----- brain.db access -----------------------------------------------------

// knowledgePageIndexState returns the stored indexed_at for slug and
// whether the row carries an embedding. A missing row returns an error so
// the caller treats it as "index it", the same contract as
// WikiPageIndexedAt.
func (db *DB) knowledgePageIndexState(slug string) (time.Time, bool, error) {
	var ts string
	var embLen int
	err := db.conn.QueryRow(
		`SELECT indexed_at, COALESCE(LENGTH(embedding), 0) FROM knowledge_pages WHERE slug = ?`, slug,
	).Scan(&ts, &embLen)
	if err != nil {
		return time.Time{}, false, err
	}
	t, perr := time.Parse(time.RFC3339, ts)
	if perr != nil {
		t = time.Time{}
	}
	return t, embLen > 0, nil
}

func (db *DB) listKnowledgePageSlugs() ([]string, error) {
	rows, err := db.conn.Query(`SELECT slug FROM knowledge_pages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

// UpsertKnowledgePage writes/updates a knowledge_pages row and syncs the
// shared corpus_fts channel. doc_id is "knowledge:<slug>" so
// hydrateMultiResult can dispatch on the prefix like it does for
// "lesson:", "memory:", and "wiki:". embedding may be nil - the row still
// writes so keyword recall works either way.
func (db *DB) UpsertKnowledgePage(slug, title, path, body string, embedding []float32, indexedAt string) error {
	var emb []byte
	if embedding != nil {
		emb = EncodeVector(embedding)
	}
	_, err := db.conn.Exec(`
		INSERT INTO knowledge_pages (slug, title, path, body, embedding, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			title = excluded.title, path = excluded.path, body = excluded.body,
			embedding = excluded.embedding, indexed_at = excluded.indexed_at`,
		slug, title, path, body, emb, indexedAt,
	)
	if err == nil {
		db.upsertCorpusFTS("knowledge:"+slug, KnowledgeDocType, title+"\n"+body)
	}
	return err
}

// DeleteKnowledgePage removes one page's row and its corpus_fts entry.
func (db *DB) DeleteKnowledgePage(slug string) error {
	_, err := db.conn.Exec(`DELETE FROM knowledge_pages WHERE slug = ?`, slug)
	if err == nil {
		db.deleteCorpusFTS("knowledge:" + slug)
	}
	return err
}

// GetKnowledgePage loads one page by slug. Returns sql.ErrNoRows when
// absent.
func (db *DB) GetKnowledgePage(slug string) (*KnowledgePageRecord, error) {
	var r KnowledgePageRecord
	var emb []byte
	err := db.conn.QueryRow(
		`SELECT slug, title, path, body, embedding, indexed_at FROM knowledge_pages WHERE slug = ?`, slug,
	).Scan(&r.Slug, &r.Title, &r.Path, &r.Body, &emb, &r.IndexedAt)
	if err != nil {
		return nil, err
	}
	if len(emb) > 0 {
		r.Embedding = DecodeVector(emb)
	}
	return &r, nil
}

// KnowledgePageCount returns the number of indexed knowledge pages.
func (db *DB) KnowledgePageCount() int {
	var n int
	_ = db.conn.QueryRow(`SELECT COUNT(*) FROM knowledge_pages`).Scan(&n)
	return n
}

// FindSimilarKnowledgePages returns up to k pages most similar to the
// query embedding by cosine similarity, the vector half of the knowledge
// recall channel. Mirrors FindSimilarWikiPages: rows without an embedding
// are skipped here but remain reachable through the FTS channel.
func (db *DB) FindSimilarKnowledgePages(queryEmbedding []float32, k int) ([]KnowledgePageRecord, error) {
	if queryEmbedding == nil {
		return nil, nil
	}
	rows, err := db.conn.Query(`
		SELECT slug, title, path, body, embedding, indexed_at
		  FROM knowledge_pages WHERE embedding IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		rec KnowledgePageRecord
		sim float64
	}
	var candidates []scored
	for rows.Next() {
		var r KnowledgePageRecord
		var emb []byte
		if err := rows.Scan(&r.Slug, &r.Title, &r.Path, &r.Body, &emb, &r.IndexedAt); err != nil {
			continue
		}
		stored := DecodeVector(emb)
		sim := CosineSimilarity(queryEmbedding, stored)
		if sim > 0.15 {
			r.Embedding = stored
			candidates = append(candidates, scored{r, sim})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].sim > candidates[j].sim })
	if len(candidates) > k {
		candidates = candidates[:k]
	}
	out := make([]KnowledgePageRecord, len(candidates))
	for i, c := range candidates {
		out[i] = c.rec
	}
	return out, nil
}
