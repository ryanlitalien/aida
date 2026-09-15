package brain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestWikiRepo builds a minimal wiki repo bundle (mirrors the real
// aida-wiki layout: wiki/{projects,entities,concepts} + evernotes/)
// under dir, for exercising IndexWiki end to end.
func writeTestWikiRepo(t *testing.T, dir string) {
	t.Helper()
	mustWrite := func(rel, content string) {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	mustWrite("wiki/projects/fighterbrands.md", `---
type: project
title: "FighterBrands"
aliases: ["fighterbrands"]
sources: ["abc123"]
wiki_status: synthesized
timestamp: "2026-07-04T04:36:21Z"
---
FighterBrands was a Rails/GKE product Ryan built and operated on Constant
Contact's campaign-sending API in the post-EIG-acquisition window.
`)

	mustWrite("wiki/entities/constant-contact.md", `---
type: entity
title: "Constant Contact"
aliases: ["constant-contact", "CC"]
sources: ["def456"]
wiki_status: synthesized
timestamp: "2026-07-01T00:00:00Z"
---
Ryan's DevOps/engineering employer where he ran CI/CD for the Contacts
platform.
`)

	// Kept source note -- should be indexed.
	mustWrite("evernotes/StartWire-SQL/Tyler email messages.md", `---
type: source_note
title: "Tyler email messages"
note_id: "50a0de2f0424"
created: "2012-08-29T14:58:05Z"
updated: "2012-08-31T15:16:30Z"
wiki_status: triaged_keep
---
Based on the daily users report, a SQL query joining users/profiles/blobs.
`)

	// Skipped source note -- should NOT be indexed.
	mustWrite("evernotes/StartWire-SQL/irrelevant.md", `---
type: source_note
title: "Irrelevant"
note_id: "zzz999"
wiki_status: triaged_skip
---
Nothing useful here.
`)
}

func TestIndexWiki_RoundTripWithoutEmbeddings(t *testing.T) {
	brainDir := t.TempDir()
	b, err := Open(brainDir, "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()
	if b.Embeddings.Available() {
		t.Fatalf("test setup: embeddings should be unavailable (no key configured)")
	}

	wikiRoot := t.TempDir()
	writeTestWikiRepo(t, wikiRoot)

	ctx := context.Background()
	result, err := b.IndexWiki(ctx, wikiRoot)
	if err != nil {
		t.Fatalf("IndexWiki: %v", err)
	}
	if result.Embedded {
		t.Errorf("result.Embedded = true, want false (no API key)")
	}
	// 2 wiki pages + 1 kept source note (irrelevant.md is triaged_skip,
	// excluded).
	if result.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", result.Scanned)
	}
	if result.Reindexed != 3 {
		t.Errorf("Reindexed = %d, want 3 (first run, everything is stale)", result.Reindexed)
	}

	// Body + FTS rows must still be written despite no embeddings --
	// matches WriteMemory's non-fatal embed degrade.
	fbSlug := wikiSlug("wiki/projects/fighterbrands.md")
	rec, err := b.DB.GetWikiPage(fbSlug)
	if err != nil {
		t.Fatalf("GetWikiPage(%s): %v", fbSlug, err)
	}
	if rec.Title != "FighterBrands" {
		t.Errorf("Title = %q, want FighterBrands", rec.Title)
	}
	if !strings.Contains(rec.Body, "Rails/GKE product") {
		t.Errorf("Body missing expected content: %q", rec.Body)
	}
	if rec.Embedding != nil {
		t.Errorf("expected nil embedding when no API key, got %v", rec.Embedding)
	}
	if rec.PageTimestamp != "2026-07-04T04:36:21Z" {
		t.Errorf("PageTimestamp = %q, want frontmatter timestamp", rec.PageTimestamp)
	}

	// The kept source note should also be indexed, using created/updated
	// as its timestamp fallback (no "timestamp" field on source notes).
	noteSlug := wikiSlug("evernotes/StartWire-SQL/Tyler email messages.md")
	noteRec, err := b.DB.GetWikiPage(noteSlug)
	if err != nil {
		t.Fatalf("GetWikiPage(%s): %v", noteSlug, err)
	}
	if noteRec.PageTimestamp != "2012-08-31T15:16:30Z" {
		t.Errorf("note PageTimestamp = %q, want the 'updated' frontmatter field", noteRec.PageTimestamp)
	}

	// The skipped source note must not be indexed at all.
	skippedSlug := wikiSlug("evernotes/StartWire-SQL/irrelevant.md")
	if _, err := b.DB.GetWikiPage(skippedSlug); err == nil {
		t.Errorf("triaged_skip note should not be indexed, but GetWikiPage succeeded")
	}

	// FTS corpus should carry the wiki row so keyword recall works.
	hits, err := b.DB.FTSSearch("FighterBrands GKE", 10, "")
	if err != nil {
		t.Fatalf("FTSSearch: %v", err)
	}
	found := false
	for _, h := range hits {
		if h == "wiki:"+fbSlug {
			found = true
		}
	}
	if !found {
		t.Errorf("FTSSearch didn't surface wiki:%s, got %v", fbSlug, hits)
	}
}

func TestIndexWiki_IncrementalSkipsUnchangedFiles(t *testing.T) {
	brainDir := t.TempDir()
	b, err := Open(brainDir, "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	wikiRoot := t.TempDir()
	writeTestWikiRepo(t, wikiRoot)
	ctx := context.Background()

	first, err := b.IndexWiki(ctx, wikiRoot)
	if err != nil {
		t.Fatalf("first IndexWiki: %v", err)
	}
	if first.Reindexed != 3 {
		t.Fatalf("first run Reindexed = %d, want 3", first.Reindexed)
	}

	// Re-run immediately with no file changes -- nothing should need
	// re-indexing (mtimes are all older than the indexed_at just written).
	second, err := b.IndexWiki(ctx, wikiRoot)
	if err != nil {
		t.Fatalf("second IndexWiki: %v", err)
	}
	if second.Scanned != 3 {
		t.Errorf("second Scanned = %d, want 3", second.Scanned)
	}
	if second.Reindexed != 0 {
		t.Errorf("second Reindexed = %d, want 0 (nothing changed)", second.Reindexed)
	}

	// Touch one file into the future so its mtime is unambiguously newer
	// than the indexed_at watermark, then re-run -- only that one file
	// should be re-indexed.
	touched := filepath.Join(wikiRoot, "wiki", "projects", "fighterbrands.md")
	future := time.Now().Add(1 * time.Hour)
	if err := os.Chtimes(touched, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	third, err := b.IndexWiki(ctx, wikiRoot)
	if err != nil {
		t.Fatalf("third IndexWiki: %v", err)
	}
	if third.Reindexed != 1 {
		t.Errorf("third Reindexed = %d, want 1 (only the touched file)", third.Reindexed)
	}
}

// TestWikiIsStale walks WikiIsStale through its four cases: no wiki repo
// cloned, first run (wiki_pages empty but pages exist), caught up right
// after an index run, and stale again once a page is touched.
func TestWikiIsStale(t *testing.T) {
	brainDir := t.TempDir()
	b, err := Open(brainDir, "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	wikiRoot := t.TempDir()

	// No wiki/ dir at all -- nothing to index, never stale.
	if b.DB.WikiIsStale(wikiRoot) {
		t.Errorf("WikiIsStale = true with no wiki repo cloned, want false")
	}

	writeTestWikiRepo(t, wikiRoot)
	ctx := context.Background()

	// wiki_pages is still empty but the wiki dir now has pages -- the
	// first-run case.
	if !b.DB.WikiIsStale(wikiRoot) {
		t.Errorf("WikiIsStale = false before the first index, want true (first-run case)")
	}

	if _, err := b.IndexWiki(ctx, wikiRoot); err != nil {
		t.Fatalf("IndexWiki: %v", err)
	}

	// Just indexed, no file changes since -- caught up.
	if b.DB.WikiIsStale(wikiRoot) {
		t.Errorf("WikiIsStale = true immediately after IndexWiki, want false")
	}

	// Touch a page into the future -- its mtime now beats the indexed_at
	// watermark IndexWiki just wrote.
	touched := filepath.Join(wikiRoot, "wiki", "projects", "fighterbrands.md")
	future := time.Now().Add(1 * time.Hour)
	if err := os.Chtimes(touched, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if !b.DB.WikiIsStale(wikiRoot) {
		t.Errorf("WikiIsStale = false after touching a page, want true")
	}
}

// TestWikiIsStale_EmptyWikiDirNotStale guards the other empty-wiki_pages
// branch: a wiki/ dir that exists but holds no pages yet shouldn't trigger
// a rebuild just because wiki_pages also happens to be empty.
func TestWikiIsStale_EmptyWikiDirNotStale(t *testing.T) {
	brainDir := t.TempDir()
	b, err := Open(brainDir, "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	wikiRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wikiRoot, "wiki"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if b.DB.WikiIsStale(wikiRoot) {
		t.Errorf("WikiIsStale = true for an empty wiki/ dir, want false (nothing to index)")
	}
}

func TestWikiSlug(t *testing.T) {
	cases := map[string]string{
		"wiki/projects/fighterbrands.md":                  "wiki-projects-fighterbrands",
		"evernotes/StartWire-SQL/Tyler email messages.md": "evernotes-startwire-sql-tyler-email-messages",
		"wiki/entities/Foo.md":                            "wiki-entities-foo",
	}
	for in, want := range cases {
		if got := wikiSlug(in); got != want {
			t.Errorf("wikiSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
