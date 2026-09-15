package brain

// aida wiki index -- Phase C's ingestion path for the personal wiki (a
// separate git repo, see config.WikiPath; internal/cli/wiki.go wires the
// CLI). Walks wiki/{projects,entities,concepts}/*.md plus wiki_status:
// triaged_keep source notes under evernotes/, embeds each page's body
// (batched via EmbedDocuments, embeddings.go), and writes a wiki_pages
// row + a corpus_fts row so SearchMulti's wiki channel (search_multi.go)
// can find them.
//
// Incremental: a page is only re-embedded when its file mtime is newer
// than the indexed_at it was last written with, so re-running `aida wiki
// index` after editing three pages doesn't re-embed the other five
// hundred. Files are the source of truth; wiki_pages is a derived index
// exactly like the rest of brain.db -- safe to delete and rebuild.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ryanlitalien/aida/internal/ui"
)

// wikiIndexPageDirs are the OKF page-type subdirectories under wiki/ that
// hold real pages -- excludes wiki/index.md and wiki/log.md, which are
// OKF-reserved, not indexable pages themselves.
var wikiIndexPageDirs = []string{"projects", "entities", "concepts"}

var wikiFrontmatterPattern = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?(.*)$`)

// wikiFrontmatter is the subset of OKF frontmatter fields indexing cares
// about. Deliberately duplicated (rather than shared) with
// internal/cli/wiki_lint.go's equivalent struct -- lint is a pure
// filesystem audit with no brain/DB dependency by design; importing this
// package into it for one struct isn't worth the coupling.
type wikiFrontmatter struct {
	Title      string   `yaml:"title"`
	Aliases    []string `yaml:"aliases"`
	WikiStatus string   `yaml:"wiki_status"`
	Timestamp  string   `yaml:"timestamp"` // wiki pages: OKF "last substantive update"
	Updated    string   `yaml:"updated"`   // source notes: evernote export field
	Created    string   `yaml:"created"`   // source notes: evernote export field
}

// wikiSourceFile is one page or kept source note discovered on disk,
// ready to embed + index.
type wikiSourceFile struct {
	Slug          string // stable ID derived from the repo-relative path
	Title         string
	Path          string // repo-root-relative, for display + provenance
	Body          string // content after the frontmatter block
	PageTimestamp string // decay anchor: frontmatter timestamp/updated/created
	ModTime       time.Time
}

// WikiIndexResult summarizes one `aida wiki index` run.
type WikiIndexResult struct {
	Scanned   int  // total pages + kept source notes considered
	Reindexed int  // subset actually (re)written this run
	Embedded  bool // whether embeddings were generated this run
}

// IndexWiki walks the wiki repo at wikiRoot and (re)indexes every page and
// kept source note into wiki_pages + the shared corpus_fts channel.
// Embeddings are best-effort: when the brain has no embedding client
// configured (no VOYAGE_API_KEY), pages still get body + FTS rows written
// -- matching WriteMemory's non-fatal embed degrade (memory_types.go).
// Keyword recall still works; only the vector channel goes dark until a
// key is configured and `aida wiki index` re-runs.
func (b *Brain) IndexWiki(ctx context.Context, wikiRoot string) (WikiIndexResult, error) {
	var result WikiIndexResult

	files, err := discoverWikiFiles(wikiRoot)
	if err != nil {
		return result, fmt.Errorf("discover wiki files: %w", err)
	}
	result.Scanned = len(files)
	result.Embedded = b.Embeddings != nil && b.Embeddings.Available()

	// A file needs (re)indexing when it's never been indexed, or its
	// mtime is newer than the indexed_at it was last written with.
	var stale []wikiSourceFile
	for _, f := range files {
		indexedAt, err := b.DB.WikiPageIndexedAt(f.Slug)
		if err != nil || f.ModTime.After(indexedAt) {
			stale = append(stale, f)
		}
	}

	// Embed the stale batch in one call -- EmbedDocuments chunks
	// internally at 128 (embeddings.go). A failed embed call degrades to
	// nil embeddings for this run rather than aborting the whole index;
	// body + FTS rows still get written below.
	var vecs [][]float32
	if result.Embedded && len(stale) > 0 {
		texts := make([]string, len(stale))
		for i, f := range stale {
			texts[i] = f.Title + "\n" + f.Body
		}
		vecs, err = b.Embeddings.EmbedDocuments(ctx, texts)
		if err != nil {
			ui.PrintVerbose("Wiki index embed", "failed: "+err.Error())
			vecs = nil
		}
	}

	// RFC3339Nano, not RFC3339: plain RFC3339 truncates to whole seconds,
	// and a file written in the same wall-clock second as this index run
	// would then compare as "newer than its own indexed_at" forever,
	// forcing every subsequent run to re-embed it. time.Parse(RFC3339,
	// ...) still parses the nanosecond-precision string fine (Go's parser
	// accepts a fractional-second field the layout doesn't declare), so
	// WikiPageIndexedAt below needs no matching change.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i, f := range stale {
		var emb []float32
		if vecs != nil && i < len(vecs) {
			emb = vecs[i]
		}
		if err := b.DB.UpsertWikiPage(f.Slug, f.Title, f.Path, f.Body, emb, f.PageTimestamp, now); err != nil {
			ui.PrintVerbose("Wiki index", fmt.Sprintf("upsert %s: %s", f.Slug, err))
			continue
		}
		result.Reindexed++
	}
	return result, nil
}

// discoverWikiFiles walks wiki/{projects,entities,concepts} (every page,
// regardless of status) plus the source-note trees -- evernotes/ and
// sources/ (future corpora: claude-memories, ...) -- where only
// wiki_status: triaged_keep notes are indexed, the same scope
// `aida wiki lint` uses.
func discoverWikiFiles(wikiRoot string) ([]wikiSourceFile, error) {
	var out []wikiSourceFile

	wikiDir := filepath.Join(wikiRoot, "wiki")
	for _, sub := range wikiIndexPageDirs {
		dir := filepath.Join(wikiDir, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // missing subdir is fine on a partial/fresh repo
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			abs := filepath.Join(dir, name)
			if f, ok := loadWikiSourceFile(wikiRoot, abs, true); ok {
				out = append(out, f)
			}
		}
	}

	for _, tree := range []string{"evernotes", "sources"} {
		dir := filepath.Join(wikiRoot, tree)
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			if f, ok := loadWikiSourceFile(wikiRoot, path, false); ok {
				out = append(out, f)
			}
			return nil
		})
	}

	return out, nil
}

// loadWikiSourceFile reads and frontmatter-parses one candidate file.
// isPage, when true (wiki/ pages), skips the wiki_status gate -- every
// page under wiki/ is indexed regardless of status. Source notes
// (isPage=false) are only indexed when wiki_status is exactly
// "triaged_keep".
func loadWikiSourceFile(wikiRoot, absPath string, isPage bool) (wikiSourceFile, bool) {
	info, err := os.Stat(absPath)
	if err != nil {
		return wikiSourceFile{}, false
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return wikiSourceFile{}, false
	}
	m := wikiFrontmatterPattern.FindSubmatch(data)
	if m == nil {
		return wikiSourceFile{}, false
	}
	var fm wikiFrontmatter
	if err := yaml.Unmarshal(m[1], &fm); err != nil {
		return wikiSourceFile{}, false
	}
	if !isPage && fm.WikiStatus != "triaged_keep" {
		return wikiSourceFile{}, false
	}

	rel, err := filepath.Rel(wikiRoot, absPath)
	if err != nil {
		rel = absPath
	}
	rel = filepath.ToSlash(rel)

	title := fm.Title
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(absPath), ".md")
	}
	pageTimestamp := fm.Timestamp
	if pageTimestamp == "" {
		pageTimestamp = fm.Updated
	}
	if pageTimestamp == "" {
		pageTimestamp = fm.Created
	}

	return wikiSourceFile{
		Slug:          wikiSlug(rel),
		Title:         title,
		Path:          rel,
		Body:          string(m[2]),
		PageTimestamp: pageTimestamp,
		ModTime:       info.ModTime(),
	}, true
}

// wikiSlug derives a stable, lowercase slug from a repo-root-relative
// path -- e.g. "wiki/projects/fighterbrands.md" ->
// "wiki-projects-fighterbrands". Lowercased so it's casefold-stable
// across re-runs regardless of source filename casing (the same APFS
// lesson that motivates the lint tool's dup-slug check).
func wikiSlug(relPath string) string {
	s := strings.ToLower(strings.TrimSuffix(relPath, ".md"))
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

// ----- brain.db access -----------------------------------------------------

// WikiPageRecord is one indexed wiki page or kept source note.
type WikiPageRecord struct {
	Slug          string
	Title         string
	Path          string
	Body          string
	Embedding     []float32
	PageTimestamp string
	IndexedAt     string
}

// WikiPageIndexedAt returns the stored indexed_at for slug, or the zero
// Time if the page has never been indexed (including "not found" --
// callers treat both identically: index it).
func (db *DB) WikiPageIndexedAt(slug string) (time.Time, error) {
	var ts string
	err := db.conn.QueryRow(`SELECT indexed_at FROM wiki_pages WHERE slug = ?`, slug).Scan(&ts)
	if err != nil {
		return time.Time{}, err
	}
	if ts == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// WikiIsStale reports whether `aida wiki index` needs to run: true when any
// page under wiki/{projects,entities,concepts} has an mtime newer than the
// newest indexed_at in wiki_pages -- including the first-run case, where
// wiki_pages is empty so the zero Time watermark makes any existing page
// compare as newer. False when the wiki repo hasn't been cloned at all
// (nothing to index) or when wiki_pages is already caught up.
//
// Scoped to wikiIndexPageDirs only -- not the evernotes/sources source-note
// trees discoverWikiFiles also walks. Telling whether a source note is
// actually indexed requires parsing its wiki_status frontmatter (only
// triaged_keep notes count), which would mean opening and unmarshaling
// every file just to answer "is anything stale" -- the opposite of cheap.
// Pages under wiki/ have no such gate, so an mtime comparison is enough.
//
// Mirrors IsStale's shape (package-level, file_mtime-vs-index-mtime) but
// compares against the indexed_at column rather than brain.db's own file
// mtime, since wiki_pages already tracks per-page watermarks precisely.
// Cheap by design: one MAX(indexed_at) query, then a directory walk that
// returns on the first newer file rather than scanning everything.
func (db *DB) WikiIsStale(wikiRoot string) bool {
	wikiDir := filepath.Join(wikiRoot, "wiki")
	if _, err := os.Stat(wikiDir); err != nil {
		return false // wiki repo not cloned -- nothing to index
	}

	var maxIndexed sql.NullString
	if err := db.conn.QueryRow(`SELECT MAX(indexed_at) FROM wiki_pages`).Scan(&maxIndexed); err != nil {
		return true // can't read the watermark -- err toward rebuilding
	}

	var watermark time.Time // zero Time if wiki_pages is empty (first run)
	if maxIndexed.Valid && maxIndexed.String != "" {
		var perr error
		watermark, perr = time.Parse(time.RFC3339, maxIndexed.String)
		if perr != nil {
			return true
		}
	}

	for _, sub := range wikiIndexPageDirs {
		entries, err := os.ReadDir(filepath.Join(wikiDir, sub))
		if err != nil {
			continue // missing subdir is fine, same as discoverWikiFiles
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			info, err := e.Info()
			if err == nil && info.ModTime().After(watermark) {
				return true // one newer file is enough -- stop looking
			}
		}
	}
	return false
}

// UpsertWikiPage writes/updates a wiki_pages row and syncs the shared
// corpus_fts channel used by SearchMulti's keyword search (search_multi.go
// / FTSSearch). doc_id is "wiki:<slug>" so hydrateMultiResult can dispatch
// on the "wiki:" prefix the same way it already does for "lesson:" and
// "memory:". embedding may be nil (no API key, or a failed embed call) --
// the row still writes so keyword recall works either way.
func (db *DB) UpsertWikiPage(slug, title, path, body string, embedding []float32, pageTimestamp, indexedAt string) error {
	var emb []byte
	if embedding != nil {
		emb = EncodeVector(embedding)
	}
	_, err := db.conn.Exec(`
		INSERT INTO wiki_pages (slug, title, path, body, embedding, page_timestamp, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			title = excluded.title, path = excluded.path, body = excluded.body,
			embedding = excluded.embedding, page_timestamp = excluded.page_timestamp,
			indexed_at = excluded.indexed_at`,
		slug, title, path, body, emb, pageTimestamp, indexedAt,
	)
	if err == nil {
		db.upsertCorpusFTS("wiki:"+slug, "wiki:page", title+"\n"+body)
	}
	return err
}

// GetWikiPage loads one wiki page by slug. Returns sql.ErrNoRows when
// absent.
func (db *DB) GetWikiPage(slug string) (*WikiPageRecord, error) {
	var r WikiPageRecord
	var emb []byte
	err := db.conn.QueryRow(
		`SELECT slug, title, path, body, embedding, COALESCE(page_timestamp, ''), indexed_at
		   FROM wiki_pages WHERE slug = ?`, slug,
	).Scan(&r.Slug, &r.Title, &r.Path, &r.Body, &emb, &r.PageTimestamp, &r.IndexedAt)
	if err != nil {
		return nil, err
	}
	if len(emb) > 0 {
		r.Embedding = DecodeVector(emb)
	}
	return &r, nil
}

// FindSimilarWikiPages returns up to k wiki pages most similar to the
// query embedding by cosine similarity -- the vector half of the wiki
// recall channel (search_multi.go). Mirrors FindSimilar's lesson-side
// pattern. Pages without a stored embedding (indexed with no API key, or
// a failed embed call) are skipped; they're still reachable via the FTS
// channel since their body is indexed there regardless.
func (db *DB) FindSimilarWikiPages(queryEmbedding []float32, k int) ([]WikiPageRecord, error) {
	if queryEmbedding == nil {
		return nil, nil
	}
	rows, err := db.conn.Query(`
		SELECT slug, title, path, body, embedding, COALESCE(page_timestamp, '')
		  FROM wiki_pages WHERE embedding IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		rec WikiPageRecord
		sim float64
	}
	var candidates []scored
	for rows.Next() {
		var r WikiPageRecord
		var emb []byte
		if err := rows.Scan(&r.Slug, &r.Title, &r.Path, &r.Body, &emb, &r.PageTimestamp); err != nil {
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
	out := make([]WikiPageRecord, len(candidates))
	for i, c := range candidates {
		out[i] = c.rec
	}
	return out, nil
}
