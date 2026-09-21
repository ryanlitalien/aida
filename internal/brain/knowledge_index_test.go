package brain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeKnowledgeDomainPage drops one page under brain/knowledge/domains.
func writeKnowledgeDomainPage(t *testing.T, brainPath, name, content string) string {
	t.Helper()
	dir := filepath.Join(brainPath, "knowledge", "domains")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	abs := filepath.Join(dir, name)
	if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return abs
}

// A generated source profile, the shape compile_sources.go writes and
// domain.go's LoadDomainKeywords parses.
const testGeneratedProfile = `# csv-viewer

## What This Source Does

CSV Viewer is a macOS SwiftUI application that displays CSV files in tables.

## When To Route Here

Keywords: csv viewer, tsv file viewer, swiftui table

## How To Query

Reference specific Swift files.
`

// A hand-written prose page with no Keywords line and no
// "When To Route Here" section at all - must be indexed just the same.
const testProsePage = `# Acme Widgets Ltd

Acme Widgets is the fictional supplier standing in for a real vendor in
these tests.

| Field | Value |
|---|---|
| Vendor ID | 4815162342 |
`

func TestIndex_IndexesKnowledgeDomainPages(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	if b.Embeddings.Available() {
		t.Fatalf("test setup: embeddings should be unavailable (no key configured)")
	}

	writeKnowledgeDomainPage(t, b.Path, "csv-viewer.md", testGeneratedProfile)
	writeKnowledgeDomainPage(t, b.Path, "acme-widgets.md", testProsePage)
	// A page whose first line is prose rather than a heading falls back
	// to its slug as the title.
	writeKnowledgeDomainPage(t, b.Path, "headless.md", "Just a note about the parts warehouse.\n")
	// Non-markdown files and subdirectories are ignored.
	writeKnowledgeDomainPage(t, b.Path, "notes.txt", "not a page")
	if err := os.MkdirAll(filepath.Join(b.Path, "knowledge", "domains", "sub"), 0755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	// Brain.Index is the production entry point (aida brain index and the
	// IsStale-triggered detached rebuild); step 4 must now write rows, not
	// just count files.
	if err := b.Index(ctx); err != nil {
		t.Fatalf("Index: %v", err)
	}

	if got := b.DB.KnowledgePageCount(); got != 3 {
		t.Fatalf("KnowledgePageCount = %d, want 3", got)
	}

	profile, err := b.DB.GetKnowledgePage("csv-viewer")
	if err != nil {
		t.Fatalf("GetKnowledgePage(csv-viewer): %v", err)
	}
	if profile.Title != "csv-viewer" {
		t.Errorf("profile Title = %q, want csv-viewer", profile.Title)
	}
	if profile.Path != "knowledge/domains/csv-viewer.md" {
		t.Errorf("profile Path = %q, want knowledge/domains/csv-viewer.md", profile.Path)
	}
	if !strings.Contains(profile.Body, "Keywords: csv viewer") {
		t.Errorf("profile Body should be the whole file, got %q", profile.Body)
	}
	if profile.Embedding != nil {
		t.Errorf("expected nil embedding with no API key, got %v", profile.Embedding)
	}

	prose, err := b.DB.GetKnowledgePage("acme-widgets")
	if err != nil {
		t.Fatalf("GetKnowledgePage(acme-widgets): %v", err)
	}
	if prose.Title != "Acme Widgets Ltd" {
		t.Errorf("prose Title = %q, want the H1 heading", prose.Title)
	}
	if !strings.Contains(prose.Body, "4815162342") {
		t.Errorf("prose Body missing table content: %q", prose.Body)
	}

	headless, err := b.DB.GetKnowledgePage("headless")
	if err != nil {
		t.Fatalf("GetKnowledgePage(headless): %v", err)
	}
	if headless.Title != "headless" {
		t.Errorf("headless Title = %q, want slug fallback", headless.Title)
	}

	// Keyword recall must reach both kinds of page through corpus_fts.
	for _, tc := range []struct{ query, slug string }{
		{"fictional supplier widgets", "acme-widgets"},
		{"SwiftUI CSV viewer", "csv-viewer"},
		{"parts warehouse", "headless"},
	} {
		hits, err := b.DB.FTSSearch(tc.query, 10, "")
		if err != nil {
			t.Fatalf("FTSSearch(%q): %v", tc.query, err)
		}
		found := false
		for _, h := range hits {
			if h == "knowledge:"+tc.slug {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("FTSSearch(%q) = %v, want knowledge:%s among hits", tc.query, hits, tc.slug)
		}
	}
	var docType string
	if err := b.DB.conn.QueryRow(
		`SELECT doc_type FROM corpus_fts WHERE doc_id = ?`, "knowledge:acme-widgets",
	).Scan(&docType); err != nil {
		t.Fatalf("query corpus_fts doc_type: %v", err)
	}
	if docType != KnowledgeDocType {
		t.Errorf("corpus_fts doc_type = %q, want %q", docType, KnowledgeDocType)
	}

	// The planner's keyword path (domain.go) is unchanged by indexing:
	// the generated profile still yields its Keywords line, and the prose
	// page (no "When To Route Here" section) still yields nothing.
	kw := LoadDomainKeywords(b.Path)
	if got := kw["csv-viewer"]; len(got) != 3 || got[0] != "csv viewer" {
		t.Errorf("LoadDomainKeywords[csv-viewer] = %v, want the three Keywords", got)
	}
	if _, ok := kw["acme-widgets"]; ok {
		t.Errorf("LoadDomainKeywords should not invent keywords for a prose page, got %v", kw["acme-widgets"])
	}
}

func TestIndexKnowledgeDomains_IncrementalAndPrune(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	keep := writeKnowledgeDomainPage(t, b.Path, "keep.md", "# Keep\n\nStays on disk.\n")
	gone := writeKnowledgeDomainPage(t, b.Path, "gone.md", "# Gone\n\nWill be deleted.\n")

	first, err := b.IndexKnowledgeDomains(ctx)
	if err != nil {
		t.Fatalf("first IndexKnowledgeDomains: %v", err)
	}
	if first.Scanned != 2 || first.Reindexed != 2 || first.Indexed != 2 || first.Pruned != 0 {
		t.Errorf("first run = %+v, want Scanned=2 Reindexed=2 Indexed=2 Pruned=0", first)
	}
	if first.Embedded {
		t.Errorf("first.Embedded = true, want false (no API key)")
	}

	// Nothing changed: no rewrites, count still honest.
	second, err := b.IndexKnowledgeDomains(ctx)
	if err != nil {
		t.Fatalf("second IndexKnowledgeDomains: %v", err)
	}
	if second.Reindexed != 0 || second.Indexed != 2 {
		t.Errorf("second run = %+v, want Reindexed=0 Indexed=2", second)
	}

	// Edit one page (mtime bumped past its indexed_at) and delete the
	// other: the edit is re-read, the deletion is pruned from the table
	// and from corpus_fts.
	if err := os.WriteFile(keep, []byte("# Keep\n\nEdited body about tardigrades.\n"), 0644); err != nil {
		t.Fatalf("rewrite keep: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(keep, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove gone: %v", err)
	}

	third, err := b.IndexKnowledgeDomains(ctx)
	if err != nil {
		t.Fatalf("third IndexKnowledgeDomains: %v", err)
	}
	if third.Scanned != 1 || third.Reindexed != 1 || third.Indexed != 1 || third.Pruned != 1 {
		t.Errorf("third run = %+v, want Scanned=1 Reindexed=1 Indexed=1 Pruned=1", third)
	}
	rec, err := b.DB.GetKnowledgePage("keep")
	if err != nil {
		t.Fatalf("GetKnowledgePage(keep): %v", err)
	}
	if !strings.Contains(rec.Body, "tardigrades") {
		t.Errorf("edited page not re-read: %q", rec.Body)
	}
	if _, err := b.DB.GetKnowledgePage("gone"); err == nil {
		t.Errorf("deleted page should be pruned from knowledge_pages")
	}
	var n int
	if err := b.DB.conn.QueryRow(`SELECT COUNT(*) FROM corpus_fts WHERE doc_id = ?`, "knowledge:gone").Scan(&n); err != nil {
		t.Fatalf("query corpus_fts: %v", err)
	}
	if n != 0 {
		t.Errorf("corpus_fts still has %d row(s) for the pruned page", n)
	}
}

func TestIndexKnowledgeDomains_MissingDirIsNotAnError(t *testing.T) {
	b := newTestBrain(t)
	if err := os.RemoveAll(filepath.Join(b.Path, "knowledge", "domains")); err != nil {
		t.Fatalf("remove domains dir: %v", err)
	}
	res, err := b.IndexKnowledgeDomains(context.Background())
	if err != nil {
		t.Fatalf("IndexKnowledgeDomains on a brain with no domains dir: %v", err)
	}
	if res.Scanned != 0 || res.Indexed != 0 {
		t.Errorf("result = %+v, want all zeros", res)
	}
}

// TestSearch_ReturnsKnowledgePages covers the path the MCP brain_search
// tool and `aida brain search` take (Brain.Search, not SearchMulti): an
// indexed domain page must come back in SearchContext.KnowledgePages,
// via the keyword channel when no embedding client is configured.
func TestSearch_ReturnsKnowledgePages(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	writeKnowledgeDomainPage(t, b.Path, "acme-widgets.md", testProsePage)
	writeKnowledgeDomainPage(t, b.Path, "csv-viewer.md", testGeneratedProfile)
	if _, err := b.IndexKnowledgeDomains(ctx); err != nil {
		t.Fatalf("IndexKnowledgeDomains: %v", err)
	}

	sc, err := b.Search(ctx, "fictional supplier widgets", nil, 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(sc.KnowledgePages) == 0 {
		t.Fatalf("Search returned no KnowledgePages for an indexed domain page")
	}
	if sc.KnowledgePages[0].Slug != "acme-widgets" {
		t.Errorf("top KnowledgePage = %q, want acme-widgets (got %+v)", sc.KnowledgePages[0].Slug, sc.KnowledgePages)
	}
	for _, p := range sc.KnowledgePages {
		if p.Slug == "csv-viewer" {
			t.Errorf("csv-viewer page should not match a query about the supplier: %+v", sc.KnowledgePages)
		}
	}

	// k caps the merged list.
	sc, err = b.Search(ctx, "viewer widgets", nil, 1)
	if err != nil {
		t.Fatalf("Search k=1: %v", err)
	}
	if len(sc.KnowledgePages) > 1 {
		t.Errorf("k=1 should cap KnowledgePages, got %d", len(sc.KnowledgePages))
	}
}

// TestIsStale_KnowledgeScope pins IsStale and Index to the same knowledge
// subtree: three fresh domain pages mark brain.db stale (Index will now
// pick them up), while three fresh files elsewhere under knowledge/ do
// not, since a rebuild would ignore them.
func TestIsStale_KnowledgeScope(t *testing.T) {
	b := newTestBrain(t)
	dbPath := filepath.Join(b.Path, dbFile)
	past := time.Now().Add(-time.Hour)

	if err := os.Chtimes(dbPath, past, past); err != nil {
		t.Fatalf("chtimes brain.db: %v", err)
	}
	if IsStale(b.Path) {
		t.Fatalf("fresh brain with no newer files should not be stale")
	}

	routingDir := filepath.Join(b.Path, "knowledge", "routing")
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if err := os.WriteFile(filepath.Join(routingDir, name), []byte("# not indexed\n"), 0644); err != nil {
			t.Fatalf("write routing page: %v", err)
		}
	}
	if IsStale(b.Path) {
		t.Errorf("files under knowledge/routing are not indexed and must not mark brain.db stale")
	}

	for _, name := range []string{"a.md", "b.md", "c.md"} {
		writeKnowledgeDomainPage(t, b.Path, name, "# indexed\n")
	}
	if !IsStale(b.Path) {
		t.Errorf("three new knowledge/domains pages should mark brain.db stale")
	}
}
