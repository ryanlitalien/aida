package brain

import (
	"context"
	"testing"
)

func TestLooksRecencyShaped(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"last memory", "what was my last memory", true},
		{"latest note", "latest note", true},
		{"most recent thing", "most recent thing", true},
		{"unrelated question", "what are my kids names", false},
		{"word-boundary guard", "the lasting impact", false},
		{"empty query", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LooksRecencyShaped(c.query); got != c.want {
				t.Errorf("LooksRecencyShaped(%q) = %v, want %v", c.query, got, c.want)
			}
		})
	}
}

func TestMemoryScopeMatch(t *testing.T) {
	cases := []struct {
		name  string
		tags  []string
		scope string
		want  bool
	}{
		{
			name:  "empty scope matches a project record",
			tags:  []string{"claude-code", "scope:project", "project:projA"},
			scope: "",
			want:  true,
		},
		{
			name:  "global scope matches a global-tagged record",
			tags:  []string{"claude-code", "scope:global"},
			scope: "global",
			want:  true,
		},
		{
			name:  "global scope does not match a project-only record",
			tags:  []string{"claude-code", "scope:project", "project:projA"},
			scope: "global",
			want:  false,
		},
		{
			name:  "project scope matches its own project record",
			tags:  []string{"claude-code", "scope:project", "project:projA"},
			scope: "project:projA",
			want:  true,
		},
		{
			name:  "project scope matches a global record",
			tags:  []string{"claude-code", "scope:global"},
			scope: "project:projA",
			want:  true,
		},
		{
			name:  "project scope does not match a different project's record",
			tags:  []string{"claude-code", "scope:project", "project:projB"},
			scope: "project:projA",
			want:  false,
		},
		{
			name:  "unrecognized scope matches anything",
			tags:  []string{"claude-code", "scope:project", "project:projB"},
			scope: "bogus",
			want:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryScopeMatch(c.tags, c.scope); got != c.want {
				t.Errorf("memoryScopeMatch(%v, %q) = %v, want %v", c.tags, c.scope, got, c.want)
			}
		})
	}
}

// writeScopedRecall writes a fact-type memory record with an explicit
// embedding/created timestamp/profile so recall ordering and filtering are
// fully controlled by the test rather than by WriteMemory's defaults (which
// require a live embedding client / wall-clock timestamps).
func writeScopedRecall(t *testing.T, b *Brain, key, body string, emb []float32, tags []string, profile, created string) *MemoryRecord {
	t.Helper()
	rec, err := b.WriteMemory(context.Background(), MemoryRecord{
		Type:      MemoryFact,
		Key:       key,
		Body:      body,
		Embedding: emb,
		Tags:      tags,
		Profile:   profile,
		Created:   created,
	})
	if err != nil {
		t.Fatalf("WriteMemory(%q): %v", key, err)
	}
	return rec
}

func TestFindSimilarMemories(t *testing.T) {
	b := newTestBrain(t)

	global := writeScopedRecall(t, b, "global-1", "global record",
		[]float32{1, 0, 0}, []string{"claude-code", "scope:global"}, "claude", "2026-07-01T00:00:00Z")
	projA := writeScopedRecall(t, b, "proja-1", "projA record",
		[]float32{0, 1, 0}, []string{"claude-code", "scope:project", "project:projA"}, "claude", "2026-07-02T00:00:00Z")
	_ = writeScopedRecall(t, b, "projb-1", "projB record",
		[]float32{0, 0, 1}, []string{"claude-code", "scope:project", "project:projB"}, "claude", "2026-07-03T00:00:00Z")
	home := writeScopedRecall(t, b, "home-1", "home record",
		[]float32{1, 0, 0}, []string{"claude-code", "scope:global"}, "home", "2026-07-01T00:00:00Z")

	t.Run("projA query returns only projA", func(t *testing.T) {
		got, err := b.DB.FindSimilarMemories([]float32{0, 1, 0}, 5, "claude", "", "project:projA")
		if err != nil {
			t.Fatalf("FindSimilarMemories: %v", err)
		}
		if len(got) != 1 || got[0].Record.ID != projA.ID {
			t.Fatalf("got %d results, want exactly [projA]; got=%+v", len(got), got)
		}
	})

	t.Run("global-vector query in projA scope returns only global", func(t *testing.T) {
		got, err := b.DB.FindSimilarMemories([]float32{1, 0, 0}, 5, "claude", "", "project:projA")
		if err != nil {
			t.Fatalf("FindSimilarMemories: %v", err)
		}
		if len(got) != 1 || got[0].Record.ID != global.ID {
			t.Fatalf("got %d results, want exactly [global]; got=%+v", len(got), got)
		}
	})

	t.Run("home-profile record is invisible to a claude-profile query", func(t *testing.T) {
		got, err := b.DB.FindSimilarMemories([]float32{1, 0, 0}, 5, "claude", "")
		if err != nil {
			t.Fatalf("FindSimilarMemories: %v", err)
		}
		for _, sm := range got {
			if sm.Record.ID == home.ID {
				t.Fatalf("home-profile record leaked into claude-profile results: %+v", got)
			}
		}
	})
}

func TestRecallMemoriesRecency(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	global := writeScopedRecall(t, b, "global-1", "global record",
		nil, []string{"claude-code", "scope:global"}, "claude", "2026-07-01T00:00:00Z")
	projA := writeScopedRecall(t, b, "proja-1", "projA record",
		nil, []string{"claude-code", "scope:project", "project:projA"}, "claude", "2026-07-02T00:00:00Z")
	projB := writeScopedRecall(t, b, "projb-1", "projB record",
		nil, []string{"claude-code", "scope:project", "project:projB"}, "claude", "2026-07-03T00:00:00Z")

	t.Run("no scope: newest-first across everything", func(t *testing.T) {
		res, err := b.RecallMemories(ctx, "", 5, "claude", "", "", true)
		if err != nil {
			t.Fatalf("RecallMemories: %v", err)
		}
		if res.BySimilarity {
			t.Errorf("BySimilarity = true, want false (recency mode)")
		}
		if len(res.Memories) != 3 {
			t.Fatalf("got %d memories, want 3; got=%+v", len(res.Memories), res.Memories)
		}
		wantOrder := []string{projB.ID, projA.ID, global.ID}
		for i, id := range wantOrder {
			if res.Memories[i].Record.ID != id {
				t.Errorf("Memories[%d].Record.ID = %q, want %q (order: %v)", i, res.Memories[i].Record.ID, id, wantOrder)
			}
		}
	})

	t.Run("project:projA scope excludes projB, orders projA before global", func(t *testing.T) {
		res, err := b.RecallMemories(ctx, "", 5, "claude", "project:projA", "", true)
		if err != nil {
			t.Fatalf("RecallMemories: %v", err)
		}
		if res.BySimilarity {
			t.Errorf("BySimilarity = true, want false (recency mode)")
		}
		if len(res.Memories) != 2 {
			t.Fatalf("got %d memories, want 2 (projA, global); got=%+v", len(res.Memories), res.Memories)
		}
		for _, sm := range res.Memories {
			if sm.Record.ID == projB.ID {
				t.Fatalf("projB leaked into project:projA scoped recall: %+v", res.Memories)
			}
		}
		if res.Memories[0].Record.ID != projA.ID || res.Memories[1].Record.ID != global.ID {
			t.Errorf("order = [%q, %q], want [projA, global]", res.Memories[0].Record.ID, res.Memories[1].Record.ID)
		}
	})

	t.Run("recency-shaped query takes the recency path", func(t *testing.T) {
		res, err := b.RecallMemories(ctx, "what is my latest memory", 5, "claude", "", "", false)
		if err != nil {
			t.Fatalf("RecallMemories: %v", err)
		}
		if res.BySimilarity {
			t.Errorf("BySimilarity = true, want false (LooksRecencyShaped should force recency mode)")
		}
	})
}

func TestRecallExcludesMemoryIndex(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	// A "…:MEMORY" record is the mirror of a MEMORY.md index file - its
	// content duplicates the real per-memory records, so recall must skip it.
	idx := writeScopedRecall(t, b, "claude:project:projA:MEMORY", "# Memory index\n- [thing](thing.md)",
		[]float32{1, 0, 0}, []string{"claude-code", "scope:project", "project:projA"}, "claude", "2026-07-09T00:00:00Z")
	real := writeScopedRecall(t, b, "claude:project:projA:thing", "the real captured memory",
		[]float32{1, 0, 0}, []string{"claude-code", "scope:project", "project:projA"}, "claude", "2026-07-08T00:00:00Z")

	t.Run("recency skips the MEMORY index blob", func(t *testing.T) {
		res, err := b.RecallMemories(ctx, "", 5, "claude", "", "", true)
		if err != nil {
			t.Fatalf("RecallMemories: %v", err)
		}
		if len(res.Memories) != 1 || res.Memories[0].Record.ID != real.ID {
			t.Fatalf("got %d memories, want exactly [real]; got=%+v", len(res.Memories), res.Memories)
		}
		for _, sm := range res.Memories {
			if sm.Record.ID == idx.ID {
				t.Fatalf("MEMORY index blob leaked into recency recall: %+v", res.Memories)
			}
		}
	})

	t.Run("semantic skips the MEMORY index blob", func(t *testing.T) {
		got, err := b.DB.FindSimilarMemories([]float32{1, 0, 0}, 5, "claude", "")
		if err != nil {
			t.Fatalf("FindSimilarMemories: %v", err)
		}
		if len(got) != 1 || got[0].Record.ID != real.ID {
			t.Fatalf("got %d results, want exactly [real]; got=%+v", len(got), got)
		}
	})
}

func TestRecallExcludesGlobalClaudeMd(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	// claude:global:CLAUDE is the captured global instruction file - stored
	// and backed up, but not an episodic memory, so recall must skip it even
	// though (here) it is the newest and a perfect vector match.
	_ = writeScopedRecall(t, b, "claude:global:CLAUDE", "my global instructions",
		[]float32{1, 0, 0}, []string{"claude-code", "scope:global"}, "claude", "2026-07-18T05:00:00Z")
	real := writeScopedRecall(t, b, "claude:global:reference_minty", "the minty linux box",
		[]float32{1, 0, 0}, []string{"claude-code", "scope:global"}, "claude", "2026-07-01T00:00:00Z")

	t.Run("recency skips claude:global:CLAUDE", func(t *testing.T) {
		res, err := b.RecallMemories(ctx, "", 5, "claude", "", "", true)
		if err != nil {
			t.Fatalf("RecallMemories: %v", err)
		}
		if len(res.Memories) != 1 || res.Memories[0].Record.ID != real.ID {
			t.Fatalf("got %d memories, want exactly [reference_minty]; got=%+v", len(res.Memories), res.Memories)
		}
	})

	t.Run("semantic skips claude:global:CLAUDE", func(t *testing.T) {
		got, err := b.DB.FindSimilarMemories([]float32{1, 0, 0}, 5, "claude", "")
		if err != nil {
			t.Fatalf("FindSimilarMemories: %v", err)
		}
		for _, sm := range got {
			if sm.Record.Key == "claude:global:CLAUDE" {
				t.Fatalf("claude:global:CLAUDE leaked into semantic recall: %+v", got)
			}
		}
	})
}

func TestRecallMemoriesSemanticFallback(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	writeScopedRecall(t, b, "global-1", "global record",
		nil, []string{"claude-code", "scope:global"}, "claude", "2026-07-01T00:00:00Z")

	// b.Embeddings has no Voyage key configured (newTestBrain uses
	// "VOYAGE_TEST_KEY_UNSET"), so Available() is false and the semantic
	// branch must fall back to recency rather than erroring or returning
	// nothing.
	res, err := b.RecallMemories(ctx, "some semantic query", 5, "claude", "", "", false)
	if err != nil {
		t.Fatalf("RecallMemories: %v", err)
	}
	if res.BySimilarity {
		t.Errorf("BySimilarity = true, want false (no embedding client available, should fall back to recency)")
	}
	if len(res.Memories) == 0 {
		t.Errorf("expected non-empty fallback recall, got 0 memories")
	}
}

func TestMemoryHasTag(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		tag  string
		want bool
	}{
		{"empty tag matches everything", []string{"meetily-call"}, "", true},
		{"empty tag matches even with no tags", nil, "", true},
		{"exact match", []string{"meetily-call", "cta"}, "cta", true},
		{"case-insensitive match", []string{"meetily-call", "CTA"}, "cta", true},
		{"no match", []string{"meetily-call", "hoa"}, "cta", false},
		{"no tags at all", nil, "cta", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryHasTag(c.tags, c.tag); got != c.want {
				t.Errorf("memoryHasTag(%v, %q) = %v, want %v", c.tags, c.tag, got, c.want)
			}
		})
	}
}

// TestRecallMemories_TagFilter verifies a tag filter restricts both the
// recency and semantic (via FindSimilarMemories) paths to records
// carrying that tag -- the meetily use case of recalling only one
// project's calls.
func TestRecallMemories_TagFilter(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	cta := writeScopedRecall(t, b, "meetily-1", "CTA call notes",
		[]float32{1, 0, 0}, []string{"meetily-call", "cta", "scope:global"}, "meetily", "2026-08-24T00:00:00Z")
	hoa := writeScopedRecall(t, b, "meetily-2", "HOA call notes",
		[]float32{1, 0, 0}, []string{"meetily-call", "hoa", "scope:global"}, "meetily", "2026-08-25T00:00:00Z")

	t.Run("recency mode filters by tag", func(t *testing.T) {
		res, err := b.RecallMemories(ctx, "", 5, "meetily", "", "cta", true)
		if err != nil {
			t.Fatalf("RecallMemories: %v", err)
		}
		if len(res.Memories) != 1 || res.Memories[0].Record.ID != cta.ID {
			t.Fatalf("got %d memories, want exactly [cta]; got=%+v", len(res.Memories), res.Memories)
		}
	})

	t.Run("semantic mode filters by tag", func(t *testing.T) {
		got, err := b.DB.FindSimilarMemories([]float32{1, 0, 0}, 5, "meetily", "hoa")
		if err != nil {
			t.Fatalf("FindSimilarMemories: %v", err)
		}
		if len(got) != 1 || got[0].Record.ID != hoa.ID {
			t.Fatalf("got %d results, want exactly [hoa]; got=%+v", len(got), got)
		}
	})

	t.Run("no tag filter returns both", func(t *testing.T) {
		res, err := b.RecallMemories(ctx, "", 5, "meetily", "", "", true)
		if err != nil {
			t.Fatalf("RecallMemories: %v", err)
		}
		if len(res.Memories) != 2 {
			t.Fatalf("got %d memories, want 2 (no tag filter); got=%+v", len(res.Memories), res.Memories)
		}
	})

	t.Run("unmatched tag returns nothing", func(t *testing.T) {
		res, err := b.RecallMemories(ctx, "", 5, "meetily", "", "no-such-tag", true)
		if err != nil {
			t.Fatalf("RecallMemories: %v", err)
		}
		if len(res.Memories) != 0 {
			t.Fatalf("got %d memories, want 0; got=%+v", len(res.Memories), res.Memories)
		}
	})
}
