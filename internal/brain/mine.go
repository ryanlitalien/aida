package brain

// Tier-1 mined-question cache -- Phase 2 (mining). Phase 1 (runcache.go)
// laid down the run_cache table and lookup path. This file is the writer:
// it walks every recorded aida invocation, drops the ones that shouldn't
// be cached verbatim (empty/refusal/task-intercept/known-bad answers),
// clusters the survivors by question-embedding similarity so near-duplicate
// phrasings collapse into one entry, and rebuilds run_cache with the single
// best answer per cluster.
//
// Mirrors the Consolidate/applyConsolidation split in consolidate.go: a
// thin method (MineRuns) supplies the real embedding client and does the
// file/DB I/O; the injectable core (mineRuns) and the pure clustering math
// (clusterMineCandidates) take everything as plain values so they're
// testable without a live Voyage key, a brain.db, or files on disk.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/refusal"
	"github.com/ryanlitalien/aida/internal/runs"
)

// EmbedFunc abstracts embedding generation so MineRuns's clustering logic
// is testable without a live Voyage key.
type EmbedFunc func(ctx context.Context, texts []string) ([][]float32, error)

// MineResult summarizes one MineRuns call.
type MineResult struct {
	DryRun         bool
	RunsConsidered int // total run files read
	RunsEligible   int // survived the answer/action/refusal/feedback filters
	ClustersFormed int
	EntriesWritten int // == ClustersFormed unless dryRun
}

// mineCandidate is one eligible run carrying its question embedding and
// computed confidence -- the unit clusterMineCandidates operates on.
type mineCandidate struct {
	Run        *runs.Run
	Embedding  []float32
	Confidence float64
}

// MineRuns reads ~/.aida/runs/*.json, clusters near-duplicate questions via
// embedding similarity, and rebuilds run_cache with the best-scoring answer
// per cluster. dryRun skips the ClearRunCache + InsertRunCache writes so the
// CLI (a later phase) can preview what would change.
func (b *Brain) MineRuns(ctx context.Context, dryRun bool) (*MineResult, error) {
	return b.mineRuns(ctx, b.Embeddings.EmbedDocuments, dryRun)
}

// mineRuns is the injectable core -- embed is swappable in tests so
// clustering can be verified without a live Voyage key.
func (b *Brain) mineRuns(ctx context.Context, embed EmbedFunc, dryRun bool) (*MineResult, error) {
	ids, err := runs.List()
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}

	lessonByRunID, err := latestLessonsByRunID()
	if err != nil {
		return nil, fmt.Errorf("load lessons: %w", err)
	}

	result := &MineResult{DryRun: dryRun, RunsConsidered: len(ids)}

	var eligible []*runs.Run
	for _, id := range ids {
		r, err := runs.Load(id)
		if err != nil || r == nil {
			continue
		}
		if runEligibleForMining(r, lessonByRunID) {
			eligible = append(eligible, r)
		}
	}
	result.RunsEligible = len(eligible)

	// Deterministic processing order (newest first): map iteration order
	// in Go is random and clustering needs to be reproducible for a golden
	// test in a later phase.
	sort.SliceStable(eligible, func(i, j int) bool {
		return eligible[i].StartedAt.After(eligible[j].StartedAt)
	})

	questions := make([]string, len(eligible))
	for i, r := range eligible {
		questions[i] = r.Question
	}
	embeddings, err := embed(ctx, questions)
	if err != nil {
		return nil, fmt.Errorf("embed questions: %w", err)
	}
	if len(embeddings) != len(eligible) {
		return nil, fmt.Errorf("embed questions: got %d embeddings for %d questions", len(embeddings), len(eligible))
	}

	candidates := make([]mineCandidate, len(eligible))
	for i, r := range eligible {
		candidates[i] = mineCandidate{
			Run:        r,
			Embedding:  embeddings[i],
			Confidence: runConfidence(r, lessonByRunID),
		}
	}

	winners := clusterMineCandidates(candidates)
	result.ClustersFormed = len(winners)

	if dryRun {
		return result, nil
	}

	if err := b.DB.ClearRunCache(); err != nil {
		return nil, fmt.Errorf("clear run cache: %w", err)
	}
	for _, w := range winners {
		// Created must reflect the SOURCE RUN's own timestamp, not the
		// mining wall-clock -- mining does a full ClearRunCache + rebuild
		// every time it runs, so stamping time.Now() here would make every
		// entry report as "created today" regardless of how old the
		// underlying answer actually is. A TTL keyed on that would be
		// silently useless (caught in production: an 84-day-old "today's
		// top news" answer still looked freshly created).
		rec := &RunCacheRecord{
			ID:          "mined-" + w.Run.ID,
			Profile:     w.Run.Profile,
			Question:    w.Run.Question,
			Embedding:   w.Embedding,
			Answer:      w.Run.Answer,
			Sources:     collectSourceNames(w.Run),
			Confidence:  w.Confidence,
			Created:     w.Run.StartedAt.UTC().Format(time.RFC3339),
			HitCount:    0,
			LastUsed:    "",
			SourceRunID: w.Run.ID,
		}
		if err := b.DB.InsertRunCache(rec); err != nil {
			return nil, fmt.Errorf("insert run_cache for %s: %w", w.Run.ID, err)
		}
	}
	result.EntriesWritten = len(winners)

	return result, nil
}

// runEligibleForMining is the Tier-1 cache admission filter. A task-
// intercept run (Action == "task") answers from LIVE task state -- caching
// it verbatim would go stale the moment the task's status changes, and
// there's already a dedicated fast intercept for those -- so it's excluded
// outright rather than scored. The identical reasoning applies to a
// question whose ANSWER is inherently time-varying even though nothing
// about the run itself looks stale yet -- see looksTimeSensitive. A run
// with a zero StartedAt (a malformed or legacy record predating the field)
// is excluded for the same failing-closed reason: its answer's age can
// never be computed, so a downstream TTL could never tell a stale one from
// a fresh one. Rejecting it only costs one cache entry -- the full
// pipeline still answers that question fine -- whereas admitting it risks
// pinning an answer of unknown age as permanently "fresh". A matched
// lesson carrying an explicit thumbs-down or a recoverable adapter error
// marks the answer known-bad and drops the run; no matched lesson at all
// is a neutral (still eligible) signal, since most runs are never rated.
func runEligibleForMining(r *runs.Run, lessonByRunID map[string]lessons.Lesson) bool {
	if r.Answer == "" {
		return false
	}
	if r.Action == "task" {
		return false
	}
	if looksTimeSensitive(r.Question) {
		return false
	}
	if r.StartedAt.IsZero() {
		return false
	}
	if looksLikeRefusalAnywhere(r.Answer) {
		return false
	}
	if l, ok := lessonByRunID[r.ID]; ok {
		if l.Feedback == lessons.FeedbackThumbsDown || l.RecoverableError {
			return false
		}
	}
	return true
}

// timeSensitivePattern matches TEMPORAL markers -- words and phrases that
// make a QUESTION's answer inherently time-varying -- and deliberately
// never matches on TOPIC words like "weather", "stock", or "news" alone.
// A question can be entirely about one of those topics and still have a
// permanently stable answer: "What weather API does first-chair use?" is a
// stable codebase question that must stay cacheable forever, while "what
// is the current weather in Boston" must never be cached at all. The word
// "current" is what separates them, not "weather" -- so only the temporal
// marker is a valid signal here; the topic never is. The marker set was
// validated against the real 270-question production run_cache corpus
// (zero false positives); that pass also surfaced one under-rejected
// family -- backward-looking and year-to-date relative periods ("last
// week", "next month", "ytd", "year to date") -- added below alongside
// the forward/present family ("this week", "this month") that was already
// here. Every entry is a full multi-word phrase (or, for "ytd", a whole
// word) -- bare "last" or "next" are deliberately absent since those alone
// are topic-agnostic ("the last release", "the next section") and would
// false-positive constantly. Compiled once at package level, not per call,
// since runEligibleForMining runs this over every mined candidate.
var timeSensitivePattern = regexp.MustCompile(
	`\b(today's|todays|today|tonight|yesterday|tomorrow|` +
		`right now|currently|current|latest|most recent|` +
		`this week|this month|this morning|this afternoon|this year|this quarter|this weekend|` +
		`last week|last month|last night|last year|last quarter|` +
		`next week|next weekend|next month|next year|` +
		`ytd|year to date|` +
		`so far|as of now|` +
		`headlines|headline|top stories|top news|breaking news|` +
		`what time is it)\b`)

// looksTimeSensitive reports whether question's answer is inherently
// time-varying and therefore unsafe to cache verbatim forever, even when
// nothing else about the run looks disqualifying. Mirrors the Action ==
// "task" exclusion in runEligibleForMining: both catch an answer that goes
// stale on its own, independent of anything the refusal/lesson checks can
// see. Caught in production: "what's the top news on nytimes today" got
// mined and then served an 84-day-old front page.
func looksTimeSensitive(question string) bool {
	return timeSensitivePattern.MatchString(strings.ToLower(question))
}

// looksLikeRefusalAnywhere scans the FULL answer for a refusal marker,
// unlike refusal.LooksLikeRefusal (which deliberately checks only the first
// sentence / first ~120 chars -- tuned for its other callers, which classify
// short synthesized answers or LLM-generated commands where the disclaimer
// always leads). Caught in production: a real synthesized answer opened
// with "I don't have any information about csv-viewer in the available data
// sources." (no exact marker match -- "any" breaks the "i don't have
// information" substring) and only revealed itself as a refusal in its
// SECOND sentence, "The search returned no results." -- past
// LooksLikeRefusal's window, so it slipped into run_cache and got served
// verbatim on every future repeat of the question.
//
// Mining caches an answer indefinitely, so the false-positive/false-negative
// cost asymmetry flips versus refusal.LooksLikeRefusal's other callers: a
// false positive here just means one fewer cache entry (the full pipeline
// still answers that question fine); a false negative means a bad answer
// gets served forever until the next re-mine. Favoring recall over
// precision is correct here even though it isn't for a per-call classifier.
func looksLikeRefusalAnywhere(answer string) bool {
	lower := strings.ToLower(answer)
	for _, marker := range refusal.RefusalMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// runConfidence scores an eligible run both for cluster-winner selection
// and as the Confidence value stored on the winning cache entry. A
// thumbs-up verdict is ground truth (1.0); an unrated-but-scored run falls
// back to the PRM-lite quality score; everything else gets a neutral
// default for a successful, unrated run.
func runConfidence(r *runs.Run, lessonByRunID map[string]lessons.Lesson) float64 {
	if l, ok := lessonByRunID[r.ID]; ok {
		if l.Feedback == lessons.FeedbackThumbsUp {
			return 1.0
		}
		if l.Quality > 0 {
			return float64(l.Quality) / 5.0
		}
	}
	return 0.6
}

// latestLessonsByRunID builds a RunID -> Lesson index, keeping the LAST
// occurrence for a given RunID (LoadAll returns lessons in file-append
// order, so a later thumbs-down appended after an auto-recorded lesson
// correctly overwrites it -- the most authoritative signal for that run).
func latestLessonsByRunID() (map[string]lessons.Lesson, error) {
	all, err := lessons.LoadAll()
	if err != nil {
		return nil, err
	}
	byRunID := make(map[string]lessons.Lesson, len(all))
	for _, l := range all {
		if l.RunID == "" {
			continue
		}
		byRunID[l.RunID] = l
	}
	return byRunID, nil
}

// clusterMineCandidates greedily single-link clusters candidates by cosine
// similarity against each cluster's current-winner embedding, and returns
// one winner per cluster. Candidates must already be in the deterministic
// processing order (newest StartedAt first) -- this function clusters in
// the order given, it does not sort. Pure and dependency-free, mirroring
// pickRunCacheHit's role as the unit-testable core of LookupRunCache.
func clusterMineCandidates(candidates []mineCandidate) []mineCandidate {
	var winners []mineCandidate
	for _, cand := range candidates {
		bestIdx, bestSim := -1, -1.0
		for i, w := range winners {
			if sim := CosineSimilarity(cand.Embedding, w.Embedding); sim > bestSim {
				bestSim, bestIdx = sim, i
			}
		}
		if bestIdx >= 0 && bestSim >= runCacheSimilarityThreshold {
			if candidateBeats(cand, winners[bestIdx]) {
				winners[bestIdx] = cand
			}
			continue
		}
		winners = append(winners, cand)
	}
	return winners
}

// candidateBeats reports whether challenger should replace incumbent as a
// cluster's winner: higher confidence wins outright; a tie is broken by
// more recent StartedAt.
func candidateBeats(challenger, incumbent mineCandidate) bool {
	if challenger.Confidence != incumbent.Confidence {
		return challenger.Confidence > incumbent.Confidence
	}
	return challenger.Run.StartedAt.After(incumbent.Run.StartedAt)
}

// collectSourceNames flattens and deduplicates source names across every
// phase of a run, preserving first-seen order.
func collectSourceNames(r *runs.Run) []string {
	seen := make(map[string]bool)
	var out []string
	for _, phase := range r.Phases {
		for _, src := range phase.Sources {
			if src.Name == "" || seen[src.Name] {
				continue
			}
			seen[src.Name] = true
			out = append(out, src.Name)
		}
	}
	return out
}
