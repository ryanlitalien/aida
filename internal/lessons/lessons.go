// Package lessons implements outcome-based learning for aida.
//
// Every aida invocation appends a Lesson to ~/.aida/lessons.jsonl. A
// lesson records what was asked, where it was asked from, which sources
// were picked, and how the run turned out (success/empty/error/artifact
// count). Optionally a user thumbs-up/thumbs-down/reason can be attached.
//
// At routing time, the planner consults this file: it finds the K most
// similar past questions and uses their outcomes to bias source ranking.
// The LLM router (engine/router.go) also gets these lessons inlined into
// its prompt as a "past learning" section, which is the strongest signal
// the orchestrator has access to.
//
// The file is JSON-lines (append-only) so it survives crashes, can be
// inspected with `jq`, and never needs locking for the common case
// (single aida process per shell session).
package lessons

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

const LessonsFile = "lessons.jsonl"

// Status is the observable outcome of a single source within a query.
type Status string

const (
	StatusSuccess Status = "success"
	StatusEmpty   Status = "empty"
	StatusError   Status = "error"
	StatusTimeout Status = "timeout"
)

// Feedback is the explicit user verdict on a lesson, when present.
// "" means no explicit feedback (the lesson was recorded automatically).
type Feedback string

const (
	FeedbackNone       Feedback = ""
	FeedbackThumbsUp   Feedback = "thumbs-up"
	FeedbackThumbsDown Feedback = "thumbs-down"
	FeedbackNote       Feedback = "note"
)

// Lesson is one record of "this question + this routing -> this outcome."
type Lesson struct {
	// When the query ran, RFC3339.
	Timestamp string `json:"ts"`

	// RunID matches the aida runs/<id>.json file so a lesson and its
	// run log can be cross-referenced.
	RunID string `json:"run_id"`

	// Question is the user's raw question, lowercased and trimmed for
	// stable matching. The original-cased copy lives in the run log.
	Question string `json:"question"`

	// Cwd of the aida invocation. Useful for inferring "this question
	// was asked inside a specific project."
	Cwd string `json:"cwd,omitempty"`

	// Action / Strategy parsed by the classifier (query / lookup / etc.).
	Action   string `json:"action,omitempty"`
	Strategy string `json:"strategy,omitempty"`

	// Sources is the ordered list of source NAMES that were actually
	// executed (not just planned -- only the ones the executor ran).
	Sources []string `json:"sources"`

	// PerSourceStatus is keyed by source name with its observable
	// outcome. Together with ArtifactCount this is the auto-learning
	// signal: a source that returned status=success and artifacts>0
	// "worked" for this question.
	PerSourceStatus map[string]Status `json:"per_source_status"`

	// ArtifactCount is the total number of artifacts across all
	// successful sources for this run.
	ArtifactCount int `json:"artifact_count"`

	// Feedback is "" by default; set by `aida thumbs-up` / `aida thumbs-down`.
	Feedback Feedback `json:"feedback,omitempty"`

	// FeedbackReason is the optional --because text from the user.
	FeedbackReason string `json:"feedback_reason,omitempty"`

	// AnswerSnippet is a truncated copy of the synthesized answer so
	// the LLM router can see what kind of response a past run produced
	// without having to load the full run log.
	AnswerSnippet string `json:"answer_snippet,omitempty"`

	// FeedbackIntendedSource is the source name the user said should
	// have been picked, extracted from the --because text. Only set
	// on thumbs-down lessons where the reason mentions a source name.
	// Kept for backwards compatibility with existing lessons; new code
	// should prefer FeedbackIntendedSources (plural) which captures
	// multi-source directives like "should be X and Y".
	FeedbackIntendedSource string `json:"feedback_intended_source,omitempty"`

	// FeedbackIntendedSources is the ordered list of sources the user
	// positively called out in the --because text. Phase 3.1 replaced
	// the single-source extractDirective with a tokenized parser that
	// handles negation and multi-mention reasons.
	FeedbackIntendedSources []string `json:"feedback_intended_sources,omitempty"`

	// FeedbackExcludedSources is the ordered list of sources the user
	// said should NOT have been picked ("no need for github"). The
	// router demotes these in candidate ranking.
	FeedbackExcludedSources []string `json:"feedback_excluded_sources,omitempty"`

	// FeedbackFailureType categorizes what went wrong: "wrong_source",
	// "wrong_answer", "too_slow", or "" (uncategorized).
	FeedbackFailureType string `json:"feedback_failure_type,omitempty"`

	// FeedbackOutputDirectives are clauses extracted from FeedbackReason
	// that describe HOW the answer should be formatted on future similar
	// questions ("3 sentences max", "include the URL", "be concise").
	// Distinct from intended/excluded sources (routing) because format
	// guidance must reach the SYNTHESIZER as a strong rule, not a soft
	// hint. The synthesizer renders these as MUST-FOLLOW requirements.
	// Extracted from notes, thumbs-up, and thumbs-down - not just
	// thumbs-down - because the user's "aida ok --because '3 sentences'"
	// is functionally a forward-looking style rule.
	FeedbackOutputDirectives []string `json:"feedback_output_directives,omitempty"`

	// RequeryOf links this lesson to a prior run that the user
	// re-queried. When set, the prior lesson is treated as implicit
	// negative feedback (the user wasn't satisfied with the first answer).
	RequeryOf string `json:"requery_of,omitempty"`

	// Quality is a 1-5 score from the PRM-lite quality scorer (Haiku).
	// 1=useless, 2=wrong-domain, 3=partial, 4=good, 5=excellent.
	// 0 means the scorer hasn't run (legacy lesson or offline mode).
	Quality int `json:"quality,omitempty"`

	// HasData is true when the answer contains specific facts, numbers,
	// dates, or file paths - not just a disclaimer or refusal.
	HasData bool `json:"has_data,omitempty"`

	// QualityReason is a one-sentence explanation from the scorer.
	QualityReason string `json:"quality_reason,omitempty"`

	// RoutingConfidence is the per-source confidence from the LLM router (0.0-1.0).
	RoutingConfidence map[string]float64 `json:"routing_confidence,omitempty"`

	// ResultConfidence is the per-source confidence from the verifier (0.0-1.0).
	ResultConfidence map[string]float64 `json:"result_confidence,omitempty"`

	// AnswerConfidence is the overall answer confidence (0.0-1.0).
	AnswerConfidence float64 `json:"answer_confidence,omitempty"`

	// Grounding describes how well the answer is supported by source evidence.
	Grounding string `json:"grounding,omitempty"` // fully, partially, weakly, ungrounded

	// SourceContribs maps each source to its fractional contribution to the answer.
	SourceContribs map[string]float64 `json:"source_contribs,omitempty"`

	// RecoverableError marks lessons where the routing was correct but
	// the source's adapter (exec/grep/sql/git) returned an error. Issue
	// #14 Bug D: without this distinction, an exec failure on Q30
	// poisons routing for Q31 (same source/domain) because the past-
	// learning loader sees a "failed" run for that source. With this
	// flag set, the router treats the lesson as adapter-fault, not
	// routing-fault, and does NOT avoid the source on similar future
	// questions.
	RecoverableError bool `json:"recoverable_error,omitempty"`

	// RoutingHint is a one-line summary of which sources worked for this
	// query type, auto-generated from run metadata when quality >= 4.
	// Example: "route to [sqlite, plausible], effective for query queries about merchant volume"
	RoutingHint string `json:"routing_hint,omitempty"`

	// ExecutedQueries maps source name to the exact command/SQL that was
	// executed successfully. Stored so that future similar questions can
	// reuse the known-good query instead of generating from scratch.
	ExecutedQueries map[string]string `json:"executed_queries,omitempty"`
}

// Path returns the absolute path to ~/.aida/lessons.jsonl.
func Path() string {
	return filepath.Join(config.Dir(), LessonsFile)
}

// Append writes one lesson as a single JSON line. Idempotent for a
// (RunID + Sources) tuple in the sense that you can call it multiple
// times safely -- the file is append-only and the consumer dedupes by
// RunID when a single run appears more than once.
//
// Append is the auto-learning entry point: query.go calls it after each
// invocation regardless of success/failure. Explicit feedback commands
// (thumbs-up/down) write a NEW lesson rather than mutating an existing
// one, so the JSONL file remains append-only.
func Append(l *Lesson) error {
	if l.Timestamp == "" {
		l.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(config.Dir(), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(Path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return enc.Encode(l)
}

// LoadAll reads every lesson in the file. Returns an empty slice if the
// file does not yet exist (first run, no learning yet).
func LoadAll() ([]Lesson, error) {
	f, err := os.Open(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Lesson
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // long answer snippets are OK
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var l Lesson
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			continue // tolerate corrupted lines
		}
		out = append(out, l)
	}
	return out, scanner.Err()
}

// FindByRunID returns the most recent lesson with the given RunID, or
// nil if none exists. Used by `aida thumbs-up` / `aida thumbs-down` to grab
// the original recording so the feedback lesson can copy its fields.
func FindByRunID(runID string) (*Lesson, error) {
	all, err := LoadAll()
	if err != nil {
		return nil, err
	}
	// Walk in reverse so we get the latest if duplicates exist.
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].RunID == runID {
			return &all[i], nil
		}
	}
	return nil, nil
}

// SimilarLesson is a Lesson + similarity score, returned by FindSimilar.
type SimilarLesson struct {
	Lesson Lesson
	Score  float64 // 0..1, fraction of question tokens that overlap
}

// FindSimilar returns up to k past lessons most relevant to the new
// question, ranked by similarity. Similarity is a deliberately simple
// jaccard-style overlap on lowercased word tokens (drops trivial stop
// words). It is good enough to surface "this looks like a question we
// answered before" without pulling in an embedding model.
//
// Lessons explicitly marked thumbs-down are still returned (the LLM
// router needs to see them to AVOID those sources), but their score is
// preserved as-is so the caller can weight them negatively.
//
// Recency tiebreak: when two lessons have the same overlap, the more
// recent one wins.
func FindSimilar(question string, k int) ([]SimilarLesson, error) {
	all, err := LoadAll()
	if err != nil {
		return nil, err
	}
	if k <= 0 || len(all) == 0 {
		return nil, nil
	}
	qTokens := tokenize(question)
	if len(qTokens) == 0 {
		return nil, nil
	}

	scored := make([]SimilarLesson, 0, len(all))
	for _, l := range all {
		lTokens := tokenize(l.Question)
		if len(lTokens) == 0 {
			continue
		}
		overlap := jaccard(qTokens, lTokens)
		if overlap == 0 {
			continue
		}
		scored = append(scored, SimilarLesson{Lesson: l, Score: overlap})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		// On ties (very common: many auto-recorded runs of the same
		// question all hit Jaccard 1.0), prefer lessons that carry
		// forward-looking signal (feedback verdict or output
		// directives) over plain auto-recorded runs. Without this,
		// every new run pollutes the top-K and evicts the
		// hand-tagged lesson that holds the format spec / routing
		// correction the user wants applied.
		iFB := lessonHasFeedbackSignal(scored[i].Lesson)
		jFB := lessonHasFeedbackSignal(scored[j].Lesson)
		if iFB != jFB {
			return iFB
		}
		return scored[i].Lesson.Timestamp > scored[j].Lesson.Timestamp
	})
	if len(scored) > k {
		scored = scored[:k]
	}
	return scored, nil
}

// lessonHasFeedbackSignal returns true when the lesson carries
// forward-looking guidance: an explicit feedback verdict, a
// feedback_reason, intended/excluded source overrides, or output
// directives. Used by FindSimilar's tiebreak so these lessons
// outrank auto-recorded runs of the same question.
func lessonHasFeedbackSignal(l Lesson) bool {
	if l.Feedback != "" && l.Feedback != FeedbackNone {
		return true
	}
	if l.FeedbackReason != "" {
		return true
	}
	if len(l.FeedbackOutputDirectives) > 0 {
		return true
	}
	if len(l.FeedbackIntendedSources) > 0 || l.FeedbackIntendedSource != "" {
		return true
	}
	if len(l.FeedbackExcludedSources) > 0 {
		return true
	}
	return false
}

// FindRecentSimilar returns the single most similar lesson from the last
// `withinMinutes`. Returns nil if no match exceeds the similarity threshold
// (0.7). Used by the re-query detector: if the user asks a very similar
// question within 10 minutes, the prior run's answer was unsatisfactory.
func FindRecentSimilar(question string, withinMinutes int) (*SimilarLesson, error) {
	all, err := LoadAll()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(withinMinutes) * time.Minute)
	qTokens := tokenize(question)
	if len(qTokens) == 0 {
		return nil, nil
	}
	var best *SimilarLesson
	for _, l := range all {
		ts, err := time.Parse(time.RFC3339, l.Timestamp)
		if err != nil || ts.Before(cutoff) {
			continue
		}
		lTokens := tokenize(l.Question)
		overlap := jaccard(qTokens, lTokens)
		if overlap < 0.7 {
			continue
		}
		if best == nil || overlap > best.Score ||
			(overlap == best.Score && l.Timestamp > best.Lesson.Timestamp) {
			best = &SimilarLesson{Lesson: l, Score: overlap}
		}
	}
	return best, nil
}

// tokenize lowercases the input and splits on whitespace + punctuation,
// dropping a small set of stop words and one-character tokens.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	parts := strings.Fields(b.String())
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) <= 1 || stopWords[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}

var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "being": true,
	"do": true, "does": true, "did": true, "have": true, "has": true,
	"had": true, "of": true, "in": true, "on": true, "at": true,
	"to": true, "for": true, "with": true, "by": true, "from": true,
	"and": true, "or": true, "but": true, "not": true, "no": true,
	"so": true, "if": true, "then": true, "this": true, "that": true,
	"i": true, "me": true, "my": true, "you": true, "your": true,
	"it": true, "its": true, "we": true, "our": true,
	"what": true, "which": true, "who": true, "when": true, "where": true,
	"why": true, "how": true, "much": true, "many": true,
}

// jaccard returns the size of the intersection over the union, a value
// in [0, 1]. Order-insensitive, dedupe-friendly.
func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	setA := make(map[string]bool, len(a))
	for _, t := range a {
		setA[t] = true
	}
	setB := make(map[string]bool, len(b))
	for _, t := range b {
		setB[t] = true
	}
	intersection := 0
	for t := range setA {
		if setB[t] {
			intersection++
		}
	}
	union := len(setA)
	for t := range setB {
		if !setA[t] {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// BuildRoutingHint constructs a one-line routing hint from run metadata.
// No LLM call needed - it's assembled from the sources, strategy, and keywords
// that were used in a successful query. Only called when quality >= 4.
func BuildRoutingHint(sources []string, action, strategy string, keywords []string) string {
	if len(sources) == 0 {
		return ""
	}

	srcList := strings.Join(sources, ", ")

	var kwPart string
	// Use up to 5 keywords for the hint
	if len(keywords) > 5 {
		keywords = keywords[:5]
	}
	if len(keywords) > 0 {
		kwPart = strings.Join(keywords, ", ")
	}

	if action == "" {
		action = "general"
	}

	if kwPart != "" {
		return fmt.Sprintf("route to [%s], effective for %s queries about %s", srcList, action, kwPart)
	}
	return fmt.Sprintf("route to [%s], effective for %s queries", srcList, action)
}

// CompositeWeight computes a unified signal strength for this lesson
// given its jaccard similarity to the current question. Combines
// quality, recency, and feedback into a single float so the router
// can show the strongest lessons first. Higher = more informative.
//
// Inspired by OpenClaw-RL's GRPO advantage weighting.
func (l Lesson) CompositeWeight(similarity float64) float64 {
	w := similarity

	// Quality multiplier (quality 0 = unscored, treat as neutral 1.0).
	switch {
	case l.Quality >= 5:
		w *= 1.5
	case l.Quality == 4:
		w *= 1.2
	case l.Quality == 3:
		w *= 1.0
	case l.Quality == 2:
		w *= 0.5
	case l.Quality == 1:
		w *= 0.2
	}

	// Recency factor: exponential decay from timestamp.
	if ts, err := time.Parse(time.RFC3339, l.Timestamp); err == nil {
		hours := time.Since(ts).Hours()
		switch {
		case hours < 24:
			w *= 1.0
		case hours < 48:
			w *= 0.9
		case hours < 168: // 1 week
			w *= 0.7
		default:
			w *= 0.5
		}
	}

	// Feedback boost: explicit feedback (either direction) is a strong
	// signal that should dominate over auto-recorded lessons.
	if l.Feedback == FeedbackThumbsUp || l.Feedback == FeedbackThumbsDown {
		w *= 2.0
	}

	return w
}

// Summary returns a one-line human-readable description of a lesson,
// used by `aida lessons` and `aida tune` for compact display.
func (l Lesson) Summary() string {
	srcs := strings.Join(l.Sources, ",")
	if srcs == "" {
		srcs = "(no sources)"
	}
	feedback := ""
	if l.Feedback == FeedbackThumbsUp {
		feedback = " 👍"
	} else if l.Feedback == FeedbackThumbsDown {
		feedback = " 👎"
	}
	q := l.Question
	if len(q) > 50 {
		q = q[:50] + "..."
	}
	return fmt.Sprintf("%s [%s -> %s, %d artifacts]%s  %q",
		l.Timestamp, srcs, l.SuccessSummary(), l.ArtifactCount, feedback, q)
}

// SuccessSummary returns a short status string like "1 ok" / "2 failed" /
// "1 ok, 1 failed" describing the per-source outcomes for this lesson.
func (l Lesson) SuccessSummary() string {
	good, bad := 0, 0
	for _, s := range l.PerSourceStatus {
		if s == StatusSuccess {
			good++
		} else {
			bad++
		}
	}
	if bad == 0 {
		return fmt.Sprintf("%d ok", good)
	}
	if good == 0 {
		return fmt.Sprintf("%d failed", bad)
	}
	return fmt.Sprintf("%d ok, %d failed", good, bad)
}
