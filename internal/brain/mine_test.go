package brain

import (
	"context"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/runs"
)

func TestRunEligibleForMining(t *testing.T) {
	lessonByRunID := map[string]lessons.Lesson{
		"run-thumbsdown":  {RunID: "run-thumbsdown", Feedback: lessons.FeedbackThumbsDown},
		"run-recoverable": {RunID: "run-recoverable", RecoverableError: true},
	}

	// Arbitrary non-zero timestamp shared by every case below except the
	// zero-StartedAt case, which is exercising that check specifically.
	startedAt := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		run  runs.Run
		want bool
	}{
		{
			name: "empty answer",
			run:  runs.Run{ID: "run-empty", Answer: "", StartedAt: startedAt},
			want: false,
		},
		{
			name: "task intercept run",
			run:  runs.Run{ID: "run-task", Answer: "task #118 is on hold", Action: "task", StartedAt: startedAt},
			want: false,
		},
		{
			// The identical staleness reasoning as the task-intercept case
			// above, but for a question whose ANSWER (not the run's Action)
			// is inherently time-varying. Real case: this exact question got
			// mined and served an 84-day-old front page.
			name: "time-sensitive question",
			run: runs.Run{ID: "run-timesensitive", StartedAt: startedAt,
				Question: "what's the top news on nytimes today",
				Answer:   "Today's top stories: ..."},
			want: false,
		},
		{
			name: "zero StartedAt",
			run:  runs.Run{ID: "run-no-timestamp", Answer: "GMV is $1.2M for pine-hollow"},
			want: false,
		},
		{
			name: "refusal-shaped answer",
			run:  runs.Run{ID: "run-refusal", Answer: "I don't know the answer to that.", StartedAt: startedAt},
			want: false,
		},
		{
			name: "thumbs-down'd run",
			run:  runs.Run{ID: "run-thumbsdown", Answer: "GMV is $1.2M", StartedAt: startedAt},
			want: false,
		},
		{
			name: "recoverable-error'd run",
			run:  runs.Run{ID: "run-recoverable", Answer: "GMV is $1.2M", StartedAt: startedAt},
			want: false,
		},
		{
			name: "normal eligible run, no matched lesson",
			run:  runs.Run{ID: "run-good", Answer: "GMV is $1.2M for pine-hollow", StartedAt: startedAt},
			want: true,
		},
		{
			// Regression case: caught in production. This exact answer slipped
			// past refusal.LooksLikeRefusal because it only scans the FIRST
			// sentence, and "I don't have any information..." doesn't exactly
			// match any marker (the "any" breaks the "i don't have information"
			// substring) -- the marker that DOES match, "the search returned
			// no", only appears in the second sentence. runEligibleForMining
			// must catch this via a full-answer scan, not just the head.
			name: "refusal marker in second sentence, not the first",
			run: runs.Run{ID: "run-refusal-2nd-sentence", StartedAt: startedAt, Answer: "I don't have any information about csv-viewer in the " +
				"available data sources. The search returned no results."},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runEligibleForMining(&tt.run, lessonByRunID)
			if got != tt.want {
				t.Errorf("runEligibleForMining(%q) = %v, want %v", tt.run.ID, got, tt.want)
			}
		})
	}
}

func TestLooksTimeSensitive(t *testing.T) {
	tests := []struct {
		name     string
		question string
		want     bool
	}{
		// Time-sensitive: real questions verified against the live cache
		// that produced the 84-day-stale-answer incident.
		{name: "top news today", question: "what's the top news on nytimes today", want: true},
		{name: "top headlines", question: "top two headlines from New York Times", want: true},
		{name: "stock right now", question: "what is AAPL stock trading at right now?", want: true},
		{name: "current balance", question: "what is my current balance", want: true},
		{name: "latest commit", question: "what is the latest commit on main?", want: true},
		{name: "this week comparison", question: "compare my running mileage this week vs last week", want: true},
		{name: "currently detects", question: "Which viewer apps does viewer-toolbox currently detect on my system?", want: true},
		{name: "what time is it", question: "what time is it", want: true},
		{name: "stock market today", question: "how is the stock market doing today", want: true},

		// Time-sensitive: backward-looking and year-to-date relative
		// periods, added after validating the marker set against the real
		// 270-question production cache surfaced these as under-rejected.
		{name: "strength sessions last week", question: "How many strength sessions did I log last week?", want: true},
		{name: "SaaS spend YTD", question: "How much have I spent on SaaS subscriptions YTD?", want: true},
		{name: "trip next weekend", question: "do I have the budget to go on a sexy trip with Kelly next weekend?", want: true},
		{name: "shipped last month", question: "what did I ship last month", want: true},
		{name: "on deck next week", question: "what's on deck next week", want: true},
		{name: "spending this quarter", question: "spending this quarter", want: true},

		// NOT time-sensitive: topical words alone (weather, stock, news)
		// must never trip this -- only a temporal marker should.
		{name: "stable weather API question", question: "What weather API does first-chair use?", want: false},
		{name: "what is a tool for", question: "what is csv-viewer for", want: false},
		{name: "count of projects", question: "how many projects do I have", want: false},
		{name: "planner question", question: "what does the aida planner do", want: false},

		// NOT time-sensitive: bare "last" / "next" regression guards -- these
		// must not match just because a temporal-sounding word is present
		// without one of the specific relative-period phrases.
		{name: "last release", question: "what changed in the last release", want: false},
		{name: "next section", question: "what is the next section about", want: false},
		{name: "last commit generic", question: "what does the last commit in the repo do", want: false},
		{name: "next.js build", question: "how do I configure the next.js build", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksTimeSensitive(tt.question); got != tt.want {
				t.Errorf("looksTimeSensitive(%q) = %v, want %v", tt.question, got, tt.want)
			}
		})
	}
}

// TestMineRuns_CreatedFromSourceRunStartedAt is a regression test: MineRuns
// used to stamp every entry's Created with time.Now() at mining time, and
// since mining always does ClearRunCache + a full rebuild, every entry
// looked freshly created no matter how old the underlying answer actually
// was. Real case: an 84-day-old "today's top news" answer (April 28) still
// reported as created today when served July 21. Created must come from
// the WINNING run's own StartedAt.
func TestMineRuns_CreatedFromSourceRunStartedAt(t *testing.T) {
	// Redirect HOME so runs.Save writes to a throwaway location, mirroring
	// TestTier1CacheGoldenReplay (tier1_golden_test.go).
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	oldStartedAt := time.Date(2026, 4, 28, 9, 0, 0, 0, time.UTC)
	const question = "what is our GMV for pine-hollow"
	const answer = "GMV is $1.2M"

	if _, err := runs.Save(&runs.Run{
		Question:  question,
		Answer:    answer,
		Action:    "query",
		Profile:   "work",
		StartedAt: oldStartedAt,
	}); err != nil {
		t.Fatalf("Save run: %v", err)
	}

	brainDir := t.TempDir()
	db, err := OpenDB(brainDir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	b := &Brain{Path: brainDir, DB: db, Embeddings: NewEmbeddingClient(""), profile: "work"}

	v := make([]float32, VectorDims)
	v[0] = 1.0
	fakeEmbedBatch := func(ctx context.Context, texts []string) ([][]float32, error) {
		return [][]float32{v}, nil
	}

	if _, err := b.mineRuns(context.Background(), fakeEmbedBatch, false); err != nil {
		t.Fatalf("mineRuns: %v", err)
	}

	var created string
	if err := db.conn.QueryRow(`SELECT created FROM run_cache LIMIT 1`).Scan(&created); err != nil {
		t.Fatalf("select created: %v", err)
	}
	want := oldStartedAt.UTC().Format(time.RFC3339)
	if created != want {
		t.Errorf("Created = %q, want %q (source run's StartedAt, not the mining wall-clock)", created, want)
	}
}

func TestRunConfidence(t *testing.T) {
	lessonByRunID := map[string]lessons.Lesson{
		"run-up":      {RunID: "run-up", Feedback: lessons.FeedbackThumbsUp},
		"run-quality": {RunID: "run-quality", Quality: 4},
	}

	tests := []struct {
		name string
		run  runs.Run
		want float64
	}{
		{name: "thumbs-up lesson", run: runs.Run{ID: "run-up"}, want: 1.0},
		{name: "quality 4, no feedback", run: runs.Run{ID: "run-quality"}, want: 0.8},
		{name: "no matched lesson at all", run: runs.Run{ID: "run-unrated"}, want: 0.6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runConfidence(&tt.run, lessonByRunID)
			if got != tt.want {
				t.Errorf("runConfidence(%q) = %v, want %v", tt.run.ID, got, tt.want)
			}
		})
	}
}

func TestClusterMineCandidates(t *testing.T) {
	tOld := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	tMid := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	tNew := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tNewer := time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)

	// v1 and vNearDup are near-identical (cosine ~0.99, above the 0.90
	// threshold): a one-hot vector and a vector that leans 0.99 onto the
	// same axis. vDistinct/vDistinct2 form a second, unrelated near-dup
	// pair on a different axis so cross-cluster similarity is ~0.
	v1 := make([]float32, VectorDims)
	v1[0] = 1.0
	vNearDup := make([]float32, VectorDims)
	vNearDup[0] = 0.99
	vNearDup[1] = 0.1411

	vDistinct := make([]float32, VectorDims)
	vDistinct[2] = 1.0
	vDistinct2 := make([]float32, VectorDims)
	vDistinct2[2] = 0.99
	vDistinct2[3] = 0.1411

	candidates := []mineCandidate{
		// Starts cluster 1 as its sole (lowest-confidence) member.
		{Run: &runs.Run{ID: "r-old", StartedAt: tOld}, Embedding: v1, Confidence: 0.6},
		// Near-dup of r-old, same confidence: tie broken by recency ->
		// displaces r-old as cluster 1's winner.
		{Run: &runs.Run{ID: "r-new-lowconf", StartedAt: tNew}, Embedding: vNearDup, Confidence: 0.6},
		// Near-dup again, higher confidence but an EARLIER timestamp than
		// r-new-lowconf: confidence must win outright over recency.
		{Run: &runs.Run{ID: "r-newer-highconf", StartedAt: tMid}, Embedding: v1, Confidence: 1.0},
		// Clearly distinct embedding: starts a second cluster.
		{Run: &runs.Run{ID: "r-distinct", StartedAt: tOld}, Embedding: vDistinct, Confidence: 0.8},
		// Near-dup of r-distinct but lower confidence: joins cluster 2,
		// must NOT displace r-distinct.
		{Run: &runs.Run{ID: "r-distinct2", StartedAt: tNewer}, Embedding: vDistinct2, Confidence: 0.5},
	}

	winners := clusterMineCandidates(candidates)

	if len(winners) != 2 {
		t.Fatalf("expected 2 clusters, got %d: %+v", len(winners), winners)
	}
	if winners[0].Run.ID != "r-newer-highconf" {
		t.Errorf("cluster 1 winner: want r-newer-highconf, got %s", winners[0].Run.ID)
	}
	if winners[0].Confidence != 1.0 {
		t.Errorf("cluster 1 winner confidence: want 1.0, got %v", winners[0].Confidence)
	}
	if winners[1].Run.ID != "r-distinct" {
		t.Errorf("cluster 2 winner: want r-distinct, got %s", winners[1].Run.ID)
	}
	if winners[1].Confidence != 0.8 {
		t.Errorf("cluster 2 winner confidence: want 0.8, got %v", winners[1].Confidence)
	}
}
