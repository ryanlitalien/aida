package brain

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRRFFuse_DocsInMultipleChannelsRankFirst(t *testing.T) {
	rankings := map[SearchChannel][]string{
		ChannelVector:    {"a", "b", "c"},
		ChannelFTS:       {"b", "d"},
		ChannelSubstring: {"a", "e"},
	}
	out := rrfFuse(rankings)

	// "a" appears in vector(rank 1) + substring(rank 1) → highest.
	// "b" appears in vector(rank 2) + fts(rank 1).
	// "c", "d", "e" each appear once.
	if out[0].DocID != "a" {
		t.Errorf("top result = %q, want 'a' (in 2 channels at rank 1)", out[0].DocID)
	}
	if len(out[0].Channels) != 2 {
		t.Errorf("'a' should be in 2 channels, got %d: %+v", len(out[0].Channels), out[0].Channels)
	}
	// Single-channel docs come after.
	for _, r := range out[2:] {
		if len(r.Channels) > 1 {
			t.Errorf("multi-channel doc %q ranked below single-channel docs", r.DocID)
		}
	}
}

func TestRRFFuse_OrderStableForTies(t *testing.T) {
	// Same scores → SliceStable preserves insertion order from the
	// map iteration, but the test focuses on score correctness.
	rankings := map[SearchChannel][]string{
		ChannelVector: {"x", "y"},
	}
	out := rrfFuse(rankings)
	if out[0].DocID != "x" || out[1].DocID != "y" {
		t.Errorf("order = [%s, %s], want [x, y]", out[0].DocID, out[1].DocID)
	}
	// Score for rank-1 entry: 1/(60+1) ≈ 0.01639
	if out[0].Score < 0.016 || out[0].Score > 0.017 {
		t.Errorf("rank-1 score = %f, expected ≈ 0.0164", out[0].Score)
	}
}

func TestBuildFTSQuery_KeepsLongTokens(t *testing.T) {
	got := buildFTSQuery("is what firstchair partner config")
	// "is" (<3 chars) dropped; rest kept.
	if !strings.Contains(got, "what") || !strings.Contains(got, "firstchair") {
		t.Errorf("expected 'what' and 'firstchair' in output: %q", got)
	}
	if strings.Contains(got, " is ") || strings.HasPrefix(got, "is ") {
		t.Errorf("short token 'is' leaked: %q", got)
	}
	if !strings.Contains(got, "OR") {
		t.Errorf("expected OR-joined tokens: %q", got)
	}
}

func TestBuildFTSQuery_EmptyForNothing(t *testing.T) {
	if got := buildFTSQuery("a b c"); got != "" {
		t.Errorf("expected empty for all-short tokens, got %q", got)
	}
	if got := buildFTSQuery(""); got != "" {
		t.Errorf("empty input should yield empty: %q", got)
	}
}

func TestBuildFTSQuery_StripsOperators(t *testing.T) {
	got := buildFTSQuery(`"firstchair" partner-id (test)`)
	for _, ch := range []string{"\"", "(", ")", "*"} {
		if strings.Contains(got, ch) {
			t.Errorf("operator %q leaked: %q", ch, got)
		}
	}
}

func TestSearchMulti_FactKeyHitsOnEntity(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	// Plant a fact about "user-units" in typed memory.
	if _, err := b.WriteMemory(ctx, MemoryRecord{
		Type: MemoryFact, Key: "user-units", Body: "user prefers US units (miles, °F, lbs)",
	}); err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	got, err := b.SearchMulti(ctx, "what units does the user prefer", []string{"user-units"}, 5)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least one result")
	}
	// Top result should be the fact memory, found by fact-key channel.
	if !strings.HasPrefix(got[0].DocID, "memory:") {
		t.Errorf("top result = %q, want a memory: doc", got[0].DocID)
	}
	if _, ok := got[0].Channels[ChannelFactKey]; !ok {
		t.Errorf("fact-key channel did not flag the memory: %+v", got[0].Channels)
	}
	if !strings.Contains(got[0].Body, "US units") {
		t.Errorf("hydrated body lost: %q", got[0].Body)
	}
}

func TestSearchMulti_FTSAndSubstringFindMemory(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	// Memory whose body contains a distinctive substring/keyword
	// but no fact-key match (entities slice empty).
	if _, err := b.WriteMemory(ctx, MemoryRecord{
		Type: MemoryEvent, Body: "saw a transaction with token 53CU-ABCD on 2026-05-05",
	}); err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	got, err := b.SearchMulti(ctx, "what was the 53cu-abcd transaction", nil, 10)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least one result")
	}
	// Either FTS or substring should have flagged it. (FTS's tokenizer
	// may stem 53cu-abcd; substring should still hit on "transaction".)
	top := got[0]
	if _, hasFTS := top.Channels[ChannelFTS]; !hasFTS {
		if _, hasSub := top.Channels[ChannelSubstring]; !hasSub {
			t.Errorf("neither FTS nor substring matched: %+v", top.Channels)
		}
	}
}

func TestSearchMulti_WikiPageSurfacedViaFTS(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	// Wiki pages are indexed via UpsertWikiPage (aida wiki index; see
	// wiki_index.go), not WriteMemory -- no embedding here (mirrors an
	// index run with no VOYAGE_API_KEY configured), so the wiki page
	// must be reachable purely through the FTS channel: wiki_index.go
	// writes "wiki:<slug>" rows into the same corpus_fts table Channel 2
	// (fts) already queries, so no vector signal is needed to find it.
	slug := "wiki-projects-fighterbrands"
	if err := b.DB.UpsertWikiPage(
		slug, "FighterBrands", "wiki/projects/fighterbrands.md",
		"FighterBrands was a Rails/GKE product built on Constant Contact's campaign API.",
		nil, "2026-07-04T04:36:21Z", time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("UpsertWikiPage: %v", err)
	}

	got, err := b.SearchMulti(ctx, "what was FighterBrands built on", nil, 10)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least one result")
	}

	var wikiResult *MultiResult
	for i := range got {
		if got[i].DocID == "wiki:"+slug {
			wikiResult = &got[i]
			break
		}
	}
	if wikiResult == nil {
		t.Fatalf("expected wiki:%s among results, got %+v", slug, got)
	}
	if _, ok := wikiResult.Channels[ChannelFTS]; !ok {
		t.Errorf("expected wiki page to be found via the fts channel, got channels %+v", wikiResult.Channels)
	}
	if wikiResult.DocType != "wiki:page" {
		t.Errorf("DocType = %q, want wiki:page", wikiResult.DocType)
	}
	if !strings.Contains(wikiResult.Body, "FighterBrands") {
		t.Errorf("hydrated body lost title/snippet: %q", wikiResult.Body)
	}
	if wikiResult.Created != "2026-07-04T04:36:21Z" {
		t.Errorf("Created = %q, want the page's frontmatter timestamp (decay anchor)", wikiResult.Created)
	}
}

func TestSearchMulti_EmptyResultsOnNothing(t *testing.T) {
	b := newTestBrain(t)
	got, err := b.SearchMulti(context.Background(), "obscure question", nil, 5)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 results on empty brain, got %d: %+v", len(got), got)
	}
}

func TestCountTokenHits(t *testing.T) {
	body := "the quick brown fox jumped over the lazy dog"
	cases := []struct {
		tokens []string
		want   int
	}{
		{[]string{"quick", "fox"}, 2},
		{[]string{"missing"}, 0},
		{[]string{"the", "the"}, 2}, // double-counts repeated input tokens
	}
	for _, c := range cases {
		if got := countTokenHits(body, c.tokens); got != c.want {
			t.Errorf("countTokenHits(%v) = %d, want %d", c.tokens, got, c.want)
		}
	}
}

func TestRecallDecay_Behavior(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		anchor string
		min    float64
		max    float64
	}{
		{"empty anchor", "", 1.0, 1.0},
		{"unparseable anchor", "yesterday-ish", 1.0, 1.0},
		{"fresh (inside grace)", now.AddDate(0, -1, 0).Format(time.RFC3339), 1.0, 1.0},
		{"exactly at offset", now.AddDate(0, 0, -180).Format(time.RFC3339), 1.0, 1.0},
		{"3 years stale", now.AddDate(-3, 0, 0).Format(time.RFC3339), decayFloor, 0.999},
		{"20 years stale hits floor", now.AddDate(-20, 0, 0).Format(time.RFC3339), decayFloor, decayFloor},
	}
	for _, c := range cases {
		got := recallDecay(c.anchor, now)
		if got < c.min || got > c.max {
			t.Errorf("%s: recallDecay = %f, want in [%f, %f]", c.name, got, c.min, c.max)
		}
	}
	// Monotonic: staler anchors never decay less.
	d1 := recallDecay(now.AddDate(-1, 0, 0).Format(time.RFC3339), now)
	d3 := recallDecay(now.AddDate(-3, 0, 0).Format(time.RFC3339), now)
	if d3 > d1 {
		t.Errorf("3y decay (%f) > 1y decay (%f); want monotonic non-increasing", d3, d1)
	}
}

func TestSearchMulti_DecayAndUseCountOrdering(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	stale := now.AddDate(-4, 0, 0).Format(time.RFC3339)
	fresh := now.AddDate(0, 0, -7).Format(time.RFC3339)

	// Two facts with identical bodies (identical channel ranks apart from
	// insertion order): one fresh, one 4 years stale.
	for _, m := range []MemoryRecord{
		{ID: "m-stale", Type: MemoryFact, Body: "wombat deploy quirk alpha", Created: stale},
		{ID: "m-fresh", Type: MemoryFact, Body: "wombat deploy quirk beta", Created: fresh},
	} {
		rec := m
		if err := db.InsertMemory(&rec); err != nil {
			t.Fatalf("InsertMemory(%s): %v", m.ID, err)
		}
	}

	b := &Brain{DB: db}
	results, err := b.SearchMulti(context.Background(), "wombat deploy quirk", nil, 10)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].DocID != "memory:m-fresh" {
		t.Errorf("fresh fact should outrank 4y-stale fact; top = %q (score %f), second = %q (score %f)",
			results[0].DocID, results[0].Score, results[1].DocID, results[1].Score)
	}

	// The recall above must have bumped both returned docs.
	m, err := db.GetMemory("m-fresh")
	if err != nil {
		t.Fatalf("GetMemory after bump: %v", err)
	}
	if m.UseCount != 1 || m.LastUsedAt == "" {
		t.Errorf("expected use_count=1 + last_used_at set after recall, got count=%d last=%q", m.UseCount, m.LastUsedAt)
	}

	// Retrieval practice: recall the stale doc many more times; its boost
	// should eventually beat the fresh doc's recency edge.
	if err := db.BumpMemoryUse([]string{"m-stale"}, now); err != nil {
		t.Fatalf("BumpMemoryUse: %v", err)
	}
	results, err = b.SearchMulti(context.Background(), "wombat deploy quirk", nil, 10)
	if err != nil {
		t.Fatalf("SearchMulti (2nd): %v", err)
	}
	if results[0].DocID != "memory:m-stale" {
		t.Errorf("bumped stale fact (fresh last_used_at) should outrank; top = %q", results[0].DocID)
	}
}

func TestSearchMulti_InstructionsExemptFromDecay(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	ancient := now.AddDate(-10, 0, 0).Format(time.RFC3339)

	for _, m := range []MemoryRecord{
		{ID: "i-old", Type: MemoryInstruction, Body: "quokka standing rule gamma", Created: ancient},
		{ID: "f-old", Type: MemoryFact, Body: "quokka standing rule delta", Created: ancient},
	} {
		rec := m
		if err := db.InsertMemory(&rec); err != nil {
			t.Fatalf("InsertMemory(%s): %v", m.ID, err)
		}
	}

	b := &Brain{DB: db}
	results, err := b.SearchMulti(context.Background(), "quokka standing rule", nil, 10)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].DocID != "memory:i-old" {
		t.Errorf("undecayed ancient instruction should outrank decayed ancient fact; top = %q", results[0].DocID)
	}
}

func TestSearchMulti_KnowledgePageSurfacedViaFTS(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	// Knowledge-domain pages are indexed by IndexKnowledgeDomains (aida
	// brain index; knowledge_index.go). No embedding here, mirroring an
	// index run with no VOYAGE_API_KEY, so the page must be reachable
	// purely through the FTS channel via its "knowledge:<slug>" row in
	// the shared corpus_fts table.
	slug := "acme-widgets"
	if err := b.DB.UpsertKnowledgePage(
		slug, "Acme Widgets Ltd", "knowledge/domains/acme-widgets.md",
		"Acme Widgets is the fictional supplier standing in for a real vendor.",
		nil, "2026-09-18T00:00:00Z",
	); err != nil {
		t.Fatalf("UpsertKnowledgePage: %v", err)
	}

	got, err := b.SearchMulti(ctx, "which supplier is the fictional vendor", nil, 10)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	var hit *MultiResult
	for i := range got {
		if got[i].DocID == "knowledge:"+slug {
			hit = &got[i]
			break
		}
	}
	if hit == nil {
		t.Fatalf("expected knowledge:%s among results, got %+v", slug, got)
	}
	if _, ok := hit.Channels[ChannelFTS]; !ok {
		t.Errorf("expected the page to be found via the fts channel, got channels %+v", hit.Channels)
	}
	if hit.DocType != KnowledgeDocType {
		t.Errorf("DocType = %q, want %q", hit.DocType, KnowledgeDocType)
	}
	if !strings.Contains(hit.Body, "Acme Widgets") || !strings.Contains(hit.Body, "fictional supplier") {
		t.Errorf("hydrated body lost title/snippet: %q", hit.Body)
	}
	if hit.Created != "2026-09-18T00:00:00Z" {
		t.Errorf("Created = %q, want indexed_at as the decay anchor", hit.Created)
	}
}
