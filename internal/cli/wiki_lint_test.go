package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ----- Pure auditWiki tests (in-memory fixtures, no disk) ------------------
//
// Mirrors brain_garden_test.go's approach: build the loaded-data shape
// directly and assert on findings. This is also the only reliable way to
// exercise the same-directory casefold-collision half of dup-slug --
// APFS (this dev machine's filesystem) silently merges two on-disk
// filenames differing only by case into one directory entry, so that
// case can never be reproduced via real files in a t.TempDir() fixture.

func TestAuditWiki_FrontmatterMissing(t *testing.T) {
	pages := []wikiPage{{RelPath: "wiki/projects/nofm.md", HasFM: false, Body: strings.Repeat("x", 200)}}
	got := auditWiki(pages, nil, "", nil)
	if !hasFinding(got, "frontmatter", "wiki/projects/nofm.md") {
		t.Errorf("expected frontmatter finding for missing frontmatter, got %+v", got)
	}
}

func TestAuditWiki_FrontmatterBadTypeAndStatus(t *testing.T) {
	pages := []wikiPage{{
		RelPath: "wiki/projects/bad.md", HasFM: true,
		Type: "gadget", Title: "Bad", WikiStatus: "draft",
		Aliases: []string{"bad"}, Sources: []string{"abc"},
		Body: strings.Repeat("x", 200),
	}}
	got := auditWiki(pages, nil, "", nil)
	if !hasFindingContains(got, "frontmatter", "invalid type") {
		t.Errorf("expected invalid-type finding, got %+v", got)
	}
	if !hasFindingContains(got, "frontmatter", "invalid wiki_status") {
		t.Errorf("expected invalid-wiki_status finding, got %+v", got)
	}
}

func TestAuditWiki_SynthesizedWithoutSources(t *testing.T) {
	pages := []wikiPage{{
		RelPath: "wiki/projects/nosrc.md", HasFM: true,
		Type: "project", Title: "No Source", WikiStatus: "synthesized",
		Aliases: []string{"nosrc"}, Sources: nil,
		Body: strings.Repeat("x", 200),
	}}
	got := auditWiki(pages, nil, "", nil)
	if !hasFindingContains(got, "frontmatter", "no sources") {
		t.Errorf("expected synthesized-without-sources finding, got %+v", got)
	}
}

func TestAuditWiki_StubWithoutSourcesIsFine(t *testing.T) {
	pages := []wikiPage{{
		RelPath: "wiki/projects/stub.md", HasFM: true,
		Type: "project", Title: "Stub", WikiStatus: "stub",
		Aliases: []string{"stub"}, Sources: nil,
		Body: strings.Repeat("x", 200),
	}}
	got := auditWiki(pages, nil, "", nil)
	if hasFindingContains(got, "frontmatter", "no sources") {
		t.Errorf("stub pages should not require sources, got %+v", got)
	}
}

func TestAuditWiki_MissingAliases(t *testing.T) {
	pages := []wikiPage{{
		RelPath: "wiki/concepts/noalias.md", HasFM: true,
		Type: "concept", Title: "No Alias", WikiStatus: "stub",
		Body: strings.Repeat("x", 200),
	}}
	got := auditWiki(pages, nil, "", nil)
	if !hasFinding(got, "missing-aliases", "wiki/concepts/noalias.md") {
		t.Errorf("expected missing-aliases finding, got %+v", got)
	}
}

func TestAuditWiki_EmptyPage(t *testing.T) {
	pages := []wikiPage{{
		RelPath: "wiki/concepts/short.md", HasFM: true,
		Type: "concept", Title: "Short", WikiStatus: "stub",
		Aliases: []string{"short"},
		Body:    "too short",
	}}
	got := auditWiki(pages, nil, "", nil)
	if !hasFinding(got, "empty-page", "wiki/concepts/short.md") {
		t.Errorf("expected empty-page finding, got %+v", got)
	}
}

func TestAuditWiki_DeadLink(t *testing.T) {
	pages := []wikiPage{{
		RelPath: "wiki/projects/a.md", HasFM: true,
		Type: "project", Title: "A", WikiStatus: "stub",
		Aliases: []string{"a"},
		Body:    "See [B](../projects/does-not-exist.md) for details. " + strings.Repeat("x", 200),
	}}
	got := auditWiki(pages, nil, "", nil)
	if !hasFinding(got, "dead-link", "wiki/projects/a.md") {
		t.Errorf("expected dead-link finding, got %+v", got)
	}
}

func TestAuditWiki_LiveLinkNotFlaggedDead(t *testing.T) {
	pages := []wikiPage{
		{
			RelPath: "wiki/projects/a.md", HasFM: true,
			Type: "project", Title: "A", WikiStatus: "stub",
			Aliases: []string{"a"},
			Body:    "See [B](../projects/b.md). " + strings.Repeat("x", 200),
		},
		{
			RelPath: "wiki/projects/b.md", HasFM: true,
			Type: "project", Title: "B", WikiStatus: "stub",
			Aliases: []string{"b"},
			Body:    strings.Repeat("x", 200),
		},
	}
	got := auditWiki(pages, nil, "", nil)
	if hasFinding(got, "dead-link", "wiki/projects/a.md") {
		t.Errorf("valid link should not be flagged dead: %+v", got)
	}
	// b.md is linked from a.md, so it should not be an orphan.
	if hasFinding(got, "orphan-page", "wiki/projects/b.md") {
		t.Errorf("b.md is linked from a.md, should not be orphan: %+v", got)
	}
	// a.md has no inbound link and isn't in index.md -- orphan.
	if !hasFinding(got, "orphan-page", "wiki/projects/a.md") {
		t.Errorf("a.md has no inbound link, expected orphan finding: %+v", got)
	}
}

func TestAuditWiki_OrphanExemptViaIndex(t *testing.T) {
	pages := []wikiPage{{
		RelPath: "wiki/projects/solo.md", HasFM: true,
		Type: "project", Title: "Solo", WikiStatus: "stub",
		Aliases: []string{"solo"},
		Body:    strings.Repeat("x", 200),
	}}
	indexText := "- [Solo](projects/solo.md)\n"
	got := auditWiki(pages, nil, indexText, nil)
	if hasFinding(got, "orphan-page", "wiki/projects/solo.md") {
		t.Errorf("page referenced from index.md should not be orphan: %+v", got)
	}
}

func TestAuditWiki_WikiStyleLink(t *testing.T) {
	pages := []wikiPage{
		{
			RelPath: "wiki/concepts/a.md", HasFM: true,
			Type: "concept", Title: "A", WikiStatus: "stub",
			Aliases: []string{"a"},
			Body:    "[[../projects/b.md]] " + strings.Repeat("x", 200),
		},
		{
			RelPath: "wiki/projects/b.md", HasFM: true,
			Type: "project", Title: "B", WikiStatus: "stub",
			Aliases: []string{"b"},
			Body:    strings.Repeat("x", 200),
		},
	}
	got := auditWiki(pages, nil, "", nil)
	if hasFinding(got, "orphan-page", "wiki/projects/b.md") {
		t.Errorf("[[wikilink]] should count as an inbound link: %+v", got)
	}
}

func TestAuditWiki_DupSlugGlobalAcrossPageTypes(t *testing.T) {
	// Two different, legitimately-created files (different parent dirs)
	// whose basenames casefold-collide -- OKF requires slugs unique
	// across the *whole* bundle, not just within one type directory.
	pages := []wikiPage{
		{RelPath: "wiki/projects/foo.md", HasFM: true, Type: "project", Title: "Foo", WikiStatus: "stub", Aliases: []string{"foo"}, Body: strings.Repeat("x", 200)},
		{RelPath: "wiki/entities/Foo.md", HasFM: true, Type: "entity", Title: "Foo Inc", WikiStatus: "stub", Aliases: []string{"foo-inc"}, Body: strings.Repeat("x", 200)},
	}
	got := auditWiki(pages, nil, "", nil)
	if !hasFindingContains(got, "dup-slug", "foo") {
		t.Errorf("expected dup-slug finding for casefold-colliding basenames, got %+v", got)
	}
}

func TestAuditWiki_DupSlugSourceNotesSameDirectory(t *testing.T) {
	// Two source notes casefold-colliding in the *same* notebook
	// directory -- the real APFS silent-overwrite bug from the
	// evernotes collision repair. Built in-memory since the collision
	// can't be reproduced as two real files on a case-insensitive fs.
	notes := []wikiSourceNote{
		{RelPath: "evernotes/StartWire-SQL/Notes.md", HasFM: true, Type: "source_note", Title: "Notes"},
		{RelPath: "evernotes/StartWire-SQL/NOTES.md", HasFM: true, Type: "source_note", Title: "Notes 2"},
	}
	got := auditWiki(nil, notes, "", nil)
	if !hasFindingContains(got, "dup-slug", "casefold-collide") {
		t.Errorf("expected dup-slug finding for same-directory note collision, got %+v", got)
	}
}

func TestAuditWiki_DupSlugSourceNotesDifferentDirectoryOK(t *testing.T) {
	// Same title, different notebook directories -- no real on-disk
	// conflict, should not be flagged.
	notes := []wikiSourceNote{
		{RelPath: "evernotes/StartWire-SQL/Notes.md", HasFM: true, Type: "source_note", Title: "Notes"},
		{RelPath: "evernotes/Galileo/Notes.md", HasFM: true, Type: "source_note", Title: "Notes"},
	}
	got := auditWiki(nil, notes, "", nil)
	if hasFindingContains(got, "dup-slug", "") {
		t.Errorf("notes in different directories should not collide, got %+v", got)
	}
}

func TestAuditWiki_SourceNoteFrontmatter(t *testing.T) {
	notes := []wikiSourceNote{
		{RelPath: "evernotes/x/bad-type.md", HasFM: true, Type: "wrong", Title: "T"},
		{RelPath: "evernotes/x/no-title.md", HasFM: true, Type: "source_note", Title: ""},
		{RelPath: "evernotes/x/no-fm.md", HasFM: false},
	}
	got := auditWiki(nil, notes, "", nil)
	if !hasFindingContains(got, "frontmatter", "want source_note") {
		t.Errorf("expected invalid-type finding for source note, got %+v", got)
	}
	if !hasFinding(got, "frontmatter", "evernotes/x/no-title.md") {
		t.Errorf("expected missing-title finding for source note, got %+v", got)
	}
	if !hasFinding(got, "frontmatter", "evernotes/x/no-fm.md") {
		t.Errorf("expected no-frontmatter finding for source note, got %+v", got)
	}
}

func TestAuditWiki_CleanBundleHasNoFindings(t *testing.T) {
	pages := []wikiPage{
		{
			RelPath: "wiki/projects/clean.md", HasFM: true,
			Type: "project", Title: "Clean", WikiStatus: "reviewed",
			Aliases: []string{"clean"}, Sources: []string{"src1"},
			Body: "[Index](../index.md) " + strings.Repeat("x", 200),
		},
	}
	notes := []wikiSourceNote{
		{RelPath: "evernotes/x/kept.md", HasFM: true, Type: "source_note", Title: "Kept"},
	}
	indexText := "[Clean](projects/clean.md)"
	got := auditWiki(pages, notes, indexText, nil)
	if len(got) != 0 {
		t.Errorf("expected zero findings for a clean bundle, got %+v", got)
	}
}

func hasFinding(findings []wikiFinding, category, subject string) bool {
	for _, f := range findings {
		if f.Category == category && f.Subject == subject {
			return true
		}
	}
	return false
}

func hasFindingContains(findings []wikiFinding, category, substr string) bool {
	for _, f := range findings {
		if f.Category != category {
			continue
		}
		if substr == "" || strings.Contains(f.Detail, substr) || strings.Contains(f.Subject, substr) {
			return true
		}
	}
	return false
}

// ----- Fixture-bundle integration test (t.TempDir(), real files) ----------

// writeWikiFixture builds a small but realistic wiki repo bundle under
// dir, exercising all six lint categories through the real loader +
// splitFrontmatter path (loadWikiPages / loadKeptSourceNotes), not just
// the pure auditWiki function above.
func writeWikiFixture(t *testing.T, root string) {
	t.Helper()
	mustWrite := func(rel, content string) {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	longBody := strings.Repeat("Solid, cited prose about this page. ", 10)

	// Clean, fully-formed page: linked from index.md, has aliases+sources,
	// long body, valid frontmatter. Also links to "orphan.md" so that
	// page is *not* an orphan, and to a target that doesn't exist (dead
	// link).
	mustWrite("wiki/projects/good.md", `---
type: project
title: "Good Project"
aliases: ["good", "good-project"]
sources: ["abc123"]
wiki_status: synthesized
---
`+longBody+` See [orphan concept](../concepts/orphan.md), a
[dangling link](../concepts/does-not-exist.md), and
[artifacts](../../_meta/artifacts.md).
`)

	// Lives outside wiki/ and evernotes/ entirely -- lint never parses
	// it as a page or note, but a real link to it must still resolve
	// (regression: dead-link resolution used to only know about pages +
	// kept notes, false-flagging any link to a file like this).
	mustWrite("_meta/artifacts.md", "# Artifacts\n\nWhere binary artifacts referenced by wiki pages live.\n")

	// Linked from good.md -- should not be flagged orphan even though
	// it's not in index.md.
	mustWrite("wiki/concepts/orphan.md", `---
type: concept
title: "Orphan (not really)"
aliases: ["orphan-concept"]
wiki_status: stub
---
`+longBody)

	// Never linked from anywhere, and not in index.md -- true orphan.
	mustWrite("wiki/entities/lonely.md", `---
type: entity
title: "Lonely Entity"
aliases: ["lonely"]
wiki_status: stub
---
`+longBody)

	// Missing aliases + too short (empty-page) + synthesized w/o sources.
	mustWrite("wiki/concepts/thin.md", `---
type: concept
title: "Thin"
wiki_status: synthesized
---
too short
`)

	// Bad type + bad status + no frontmatter title.
	mustWrite("wiki/entities/broken.md", `---
type: widget
wiki_status: draft
aliases: ["broken"]
---
`+longBody)

	// Casefold-duplicate slug against good.md, but legitimately in a
	// different type directory (no filesystem case-collision risk).
	mustWrite("wiki/entities/Good.md", `---
type: entity
title: "Good (entity)"
aliases: ["good-entity"]
wiki_status: stub
---
`+longBody)

	mustWrite("wiki/index.md", `---
type: source_index
title: Index
---
- [Good Project](projects/good.md)
`)

	// Kept source note -- should be scanned; well-formed, so no findings.
	mustWrite("evernotes/Notebook/keeper.md", `---
type: source_note
title: "Keeper"
note_id: "abc123"
wiki_status: triaged_keep
---
Some raw historical content.
`)

	// Skipped source note -- should NOT be scanned or flagged despite
	// having no title.
	mustWrite("evernotes/Notebook/skipped.md", `---
type: source_note
title: ""
note_id: "def456"
wiki_status: triaged_skip
---
Irrelevant raw content.
`)
}

func TestWikiLintFixtureBundle(t *testing.T) {
	root := t.TempDir()
	writeWikiFixture(t, root)

	pages, err := loadWikiPages(root)
	if err != nil {
		t.Fatalf("loadWikiPages: %v", err)
	}
	// 5 pages under projects/entities/concepts (good, orphan, lonely, thin, broken, Good.md = 6 actually)
	if len(pages) != 6 {
		t.Fatalf("loadWikiPages: got %d pages, want 6: %+v", len(pages), pages)
	}

	notes, err := loadKeptSourceNotes(root)
	if err != nil {
		t.Fatalf("loadKeptSourceNotes: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("loadKeptSourceNotes: got %d notes, want 1 (only triaged_keep), got %+v", len(notes), notes)
	}
	if notes[0].Title != "Keeper" {
		t.Errorf("loaded note title = %q, want Keeper", notes[0].Title)
	}

	indexData, err := os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	if err != nil {
		t.Fatalf("read index.md: %v", err)
	}

	existingFiles, err := listWikiRepoFiles(root)
	if err != nil {
		t.Fatalf("listWikiRepoFiles: %v", err)
	}

	findings := auditWiki(pages, notes, string(indexData), existingFiles)

	byCategory := map[string]int{}
	for _, f := range findings {
		byCategory[f.Category]++
	}

	for _, cat := range wikiCategories {
		if byCategory[cat] == 0 {
			t.Errorf("expected at least one %s finding from the fixture bundle, got none (all findings: %+v)", cat, findings)
		}
	}

	if !hasFinding(findings, "dead-link", "wiki/projects/good.md") {
		t.Errorf("expected dead-link finding on good.md's dangling link")
	}
	// good.md links to _meta/artifacts.md -- a real file outside both
	// wiki/ and evernotes/, so lint never parses it as a page or note.
	// It must still resolve via listWikiRepoFiles and NOT be flagged, so
	// good.md should have exactly one dead-link finding (the genuine
	// dangling one), not two.
	deadLinksOnGood := 0
	for _, f := range findings {
		if f.Category == "dead-link" && f.Subject == "wiki/projects/good.md" {
			deadLinksOnGood++
		}
	}
	if deadLinksOnGood != 1 {
		t.Errorf("good.md: got %d dead-link findings, want exactly 1 (link to _meta/artifacts.md must resolve): %+v", deadLinksOnGood, findings)
	}
	if !hasFinding(findings, "orphan-page", "wiki/entities/lonely.md") {
		t.Errorf("expected orphan-page finding on lonely.md")
	}
	if hasFinding(findings, "orphan-page", "wiki/concepts/orphan.md") {
		t.Errorf("orphan.md is linked from good.md, should not be flagged orphan")
	}
	if !hasFinding(findings, "empty-page", "wiki/concepts/thin.md") {
		t.Errorf("expected empty-page finding on thin.md")
	}
	if !hasFinding(findings, "missing-aliases", "wiki/concepts/thin.md") {
		t.Errorf("expected missing-aliases finding on thin.md")
	}
	if !hasFindingContains(findings, "frontmatter", "invalid type") {
		t.Errorf("expected invalid-type finding on broken.md")
	}
	if !hasFindingContains(findings, "dup-slug", "good") {
		t.Errorf("expected dup-slug finding for good.md/Good.md collision")
	}
	// The skipped source note must not contribute any finding at all.
	for _, f := range findings {
		if strings.Contains(f.Subject, "skipped.md") {
			t.Errorf("triaged_skip note should be out of scope, but got finding: %+v", f)
		}
	}
}
