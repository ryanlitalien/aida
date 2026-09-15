package brain

// TestTier1CacheGoldenReplay is the end-to-end golden test for the whole
// Tier-1 mined-question cache feature: it proves Phase 2's writer
// (MineRuns/mineRuns, mine.go) and Phase 1's reader (LookupRunCache/
// lookupRunCache, runcache.go) still agree with each other -- not just that
// each passes its own isolated unit tests. A silent drift between the
// clustering math and the lookup margin gate (e.g. a threshold change on
// one side but not the other) would slip past mine_test.go and
// runcache_test.go individually while breaking the feature end-to-end; this
// test replays a full mine -> lookup cycle to catch exactly that.

import (
	"context"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/runs"
)

// axisVector builds a VectorDims-length vector with a single component set
// -- the "distinct axis" shape used by TestClusterMineCandidates (mine_test.go)
// and reused here so the mining fixtures and the lookup fakes are provably
// built from the same numbers.
func axisVector(dim int, val float32) []float32 {
	v := make([]float32, VectorDims)
	v[dim] = val
	return v
}

// nearDupVector builds on axisVector by also setting a second component --
// the "near-duplicate" shape (cosine ~0.99 against the pure axisVector) used
// by TestClusterMineCandidates for its near-dup pair.
func nearDupVector(dim1 int, val1 float32, dim2 int, val2 float32) []float32 {
	v := axisVector(dim1, val1)
	v[dim2] = val2
	return v
}

// TestTier1CacheGoldenReplay writes 3 fixture runs (a camp-alder-GMV question, a
// paraphrase of it, and a genuinely distinct staging-deploy question),
// mines them into run_cache via the injectable mineRuns core, then queries
// the injectable lookupRunCache core to confirm: a paraphrase of the
// camp-alder question hits the tie-broken cluster winner's answer, a novel
// question misses, and the distinct control question survives mining as
// its own separate entry.
func TestTier1CacheGoldenReplay(t *testing.T) {
	// Redirect HOME so runs.Save (Phase-4 call site's dependency) writes to
	// a throwaway location -- see withTempHome in internal/runs/task_ref_test.go.
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	tBase := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Deliberately free of any looksTimeSensitive marker (mine.go) -- this
	// test is exercising clustering/margin behavior, not the time-sensitive
	// admission filter, so the fixture questions must stay eligible for
	// mining on their own merits. ("this month" / "current" would otherwise
	// trip the filter and zero out RunsEligible below.)
	const (
		questionA = "what is our overall GMV for camp-alder"
		answerA   = "Camp Alder's GMV is $2.4M."
		questionB = "what's camp-alder's overall gmv looking like"
		answerB   = "Camp Alder GMV is roughly $2.4M."
		questionC = "what's the staging deploy version"
		answerC   = "The staging deploy is v2.3.1."
	)

	if _, err := runs.Save(&runs.Run{
		Question:  questionA,
		Answer:    answerA,
		Action:    "query",
		Profile:   "work",
		StartedAt: tBase,
	}); err != nil {
		t.Fatalf("Save run A: %v", err)
	}
	if _, err := runs.Save(&runs.Run{
		Question:  questionB,
		Answer:    answerB,
		Action:    "query",
		Profile:   "work",
		StartedAt: tBase.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("Save run B: %v", err)
	}
	if _, err := runs.Save(&runs.Run{
		Question:  questionC,
		Answer:    answerC,
		Action:    "query",
		Profile:   "work",
		StartedAt: tBase.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("Save run C: %v", err)
	}

	// A separate temp dir for the brain's own DB -- brain.db and the run
	// files always live at distinct paths in production too (~/.aida/brain/
	// vs ~/.aida/runs/), so this mirrors that rather than unifying it.
	brainDir := t.TempDir()
	db, err := OpenDB(brainDir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	b := &Brain{Path: brainDir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "work"}

	// vA/vC are pure one-hot vectors on distinct axes (0 and 2). vB leans
	// 0.99 onto A's axis with a small nudge on axis 1 -- cosine ~0.99
	// against vA, comfortably above the 0.90 runCacheSimilarityThreshold --
	// so B clusters with A as a near-duplicate while C stays its own
	// cluster.
	vA := axisVector(0, 1.0)
	vB := nearDupVector(0, 0.99, 1, 0.1411)
	vC := axisVector(2, 1.0)

	fakeEmbedBatch := func(ctx context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, q := range texts {
			switch q {
			case questionA:
				out[i] = vA
			case questionB:
				out[i] = vB
			case questionC:
				out[i] = vC
			default:
				t.Fatalf("fakeEmbedBatch: unexpected question %q", q)
			}
		}
		return out, nil
	}

	result, err := b.mineRuns(context.Background(), fakeEmbedBatch, false)
	if err != nil {
		t.Fatalf("mineRuns: %v", err)
	}
	if result.RunsConsidered != 3 {
		t.Errorf("RunsConsidered = %d, want 3", result.RunsConsidered)
	}
	if result.RunsEligible != 3 {
		t.Errorf("RunsEligible = %d, want 3", result.RunsEligible)
	}
	if result.ClustersFormed != 2 {
		t.Errorf("ClustersFormed = %d, want 2", result.ClustersFormed)
	}
	if result.EntriesWritten != 2 {
		t.Errorf("EntriesWritten = %d, want 2", result.EntriesWritten)
	}
	if got := b.DB.RunCacheCount(); got != 2 {
		t.Errorf("RunCacheCount = %d, want 2", got)
	}

	// fakeEmbedQuery maps a handful of known lookup phrasings onto the same
	// axes used above: a camp-alder paraphrase onto B's near-dup-of-A axis, an
	// unrelated question onto a 4th axis nothing was mined on, and a
	// staging-deploy paraphrase onto C's axis.
	const (
		lookupCampAlderParaphrase = "some new phrasing of the camp-alder gmv question"
		lookupUnrelated           = "some question about an unrelated topic"
		lookupStagingParaphrase   = "some phrasing of the staging deploy question"
	)
	fakeEmbedQuery := func(ctx context.Context, text string) ([]float32, error) {
		switch text {
		case lookupCampAlderParaphrase:
			return vB, nil
		case lookupUnrelated:
			return axisVector(4, 1.0), nil
		case lookupStagingParaphrase:
			return vC, nil
		default:
			t.Fatalf("fakeEmbedQuery: unexpected text %q", text)
			return nil, nil
		}
	}

	// Fixed lookup clock shortly after tBase -- well within runCacheMaxAge
	// of every mined entry's Created (stamped from each winning run's own
	// StartedAt, all at or before tBase). This test is exercising the
	// mine -> lookup cycle end to end, not the TTL backstop, so the lookup
	// clock must not accidentally expire the very entries it just mined.
	lookupNow := func() time.Time { return tBase.Add(1 * time.Hour) }

	// Golden assertion #1: a near-duplicate phrasing of the camp-alder question
	// hits, and the served answer is run A's -- the cluster's tie-broken
	// winner (A, not B) -- proving the winner selected during mining is
	// what actually gets served.
	hit, ok, err := b.lookupRunCache(context.Background(), lookupCampAlderParaphrase, "work", fakeEmbedQuery, lookupNow)
	if err != nil {
		t.Fatalf("lookupRunCache(camp-alder paraphrase): %v", err)
	}
	if !ok {
		t.Fatal("lookupRunCache(camp-alder paraphrase): want hit, got miss")
	}
	if hit.Answer != answerA {
		t.Errorf("lookupRunCache(camp-alder paraphrase) answer = %q, want %q (run A, not run B's %q)", hit.Answer, answerA, answerB)
	}

	// Golden assertion #2: a genuinely novel question, embedded onto an
	// axis nothing was mined on, misses.
	_, ok, err = b.lookupRunCache(context.Background(), lookupUnrelated, "work", fakeEmbedQuery, lookupNow)
	if err != nil {
		t.Fatalf("lookupRunCache(unrelated): %v", err)
	}
	if ok {
		t.Error("lookupRunCache(unrelated): want miss, got hit")
	}

	// Golden assertion #3: the distinct control question C, looked up on
	// its own axis, hits its own answer -- proving it survived mining as a
	// separate cache entry rather than being merged into the A/B cluster.
	hit, ok, err = b.lookupRunCache(context.Background(), lookupStagingParaphrase, "work", fakeEmbedQuery, lookupNow)
	if err != nil {
		t.Fatalf("lookupRunCache(staging paraphrase): %v", err)
	}
	if !ok {
		t.Fatal("lookupRunCache(staging paraphrase): want hit, got miss")
	}
	if hit.Answer != answerC {
		t.Errorf("lookupRunCache(staging paraphrase) answer = %q, want %q", hit.Answer, answerC)
	}
}
