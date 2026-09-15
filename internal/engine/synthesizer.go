package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ryanlitalien/aida/internal/eval"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/sources"
	"github.com/ryanlitalien/aida/internal/ui"
)

// Synthesize is Step 6 of the pipeline: LLM Call #3 to aggregate results into a cited answer.
// soulContext is the pre-formatted user identity block from soul.yaml (may be "").
// libraryContext is the active-route layer body bundle (may be "") - the same
// content the executor sees in step 5, threaded through so format/scoping
// rules in per-source layer docs actually reach the answer-writer.
// pastLessons carries similar past lessons so the synthesizer can learn from
// prior feedback - e.g. "the summary was too short" notes influence answer style.
func Synthesize(ctx context.Context, client *llm.Client, question, soulContext, libraryContext string, results []sources.SourceResult, pastLessons []lessons.SimilarLesson) (string, error) {
	summaries := buildSummaries(results)
	feedbackHints := buildFeedbackHints(pastLessons)
	if feedbackHints != "" {
		dCount := 0
		for _, ls := range pastLessons {
			dCount += len(ls.Lesson.FeedbackOutputDirectives)
		}
		ui.PrintVerbose("Synthesizer feedback", fmt.Sprintf("%d past lessons, %d output directives applied", len(pastLessons), dCount))
	}

	response, err := client.CompleteWithStage(
		ctx,
		"synthesize",
		llm.SynthesisSystemPrompt,
		llm.SynthesisUserPrompt(question, soulContext, libraryContext, summaries, feedbackHints, currentDatetimeLine()),
	)
	if err != nil {
		return "", fmt.Errorf("synthesis failed: %w", err)
	}

	return response, nil
}

// QualityAssessment holds the result of the post-synthesis quality check.
type QualityAssessment struct {
	Quality           int    `json:"quality"`
	HasData           bool   `json:"has_data"`
	Reason            string `json:"reason"`
	MissingInfo       string `json:"missing_info"`
	SuggestedApproach string `json:"suggested_approach"`
}

// ReExecuteFn is called by SynthesizeWithValidation when the first synthesis
// produces a low-quality answer. It re-runs execution with additional guidance
// about what information is missing. Returns supplemental results to merge.
type ReExecuteFn func(ctx context.Context, guidance string) ([]sources.SourceResult, error)

// SynthesizeWithValidation performs synthesis, scores the answer quality, and
// if quality <= 2, re-executes with guidance from the quality scorer and
// synthesizes again. At most 1 re-query cycle runs (not a general loop).
//
// If reExecuteFn is nil, the validation loop is disabled and this behaves
// identically to Synthesize (plus a quality score).
//
// costBudget is the max USD to spend before skipping re-execution (0 = no limit).
//
// Returns: (answer, quality assessment, reviewer records, error).
// The reviewer records reflect the FINAL answer returned (after any
// re-synthesis), so the caller can persist them as the eval signal
// for that run.
func SynthesizeWithValidation(
	ctx context.Context,
	client *llm.Client,
	question, soulContext, libraryContext string,
	results []sources.SourceResult,
	pastLessons []lessons.SimilarLesson,
	reExecuteFn ReExecuteFn,
	costBudget float64,
) (string, *QualityAssessment, []eval.ReviewRecord, error) {
	// First synthesis
	answer, err := Synthesize(ctx, client, question, soulContext, libraryContext, results, pastLessons)
	if err != nil {
		return "", nil, nil, err
	}

	// Run the reviewer loop (sensor side of the harness). Failures
	// here never block the answer; the loop is observational in v1.
	// Records are returned to the caller for persistence - the
	// synthesizer doesn't write to disk itself, keeping the engine
	// package decoupled from brain storage.
	reviews := runReviewers(ctx, question, answer, results)

	// Score quality
	qa := scoreQuality(ctx, client, question, answer)
	if qa == nil {
		// Quality scoring failed - return the answer as-is
		return answer, nil, reviews, nil
	}

	ui.PrintVerbose("Synthesis quality", fmt.Sprintf("%d/5 - %s", qa.Quality, qa.Reason))

	// If quality is acceptable or we can't re-execute, return
	if qa.Quality >= 3 || reExecuteFn == nil {
		return answer, qa, reviews, nil
	}

	// Check cost budget before re-executing
	if costBudget > 0 && client.TotalCost() >= costBudget {
		ui.PrintVerbose("Synthesis validation", "skipping re-execution: cost budget exceeded")
		return answer, qa, reviews, nil
	}

	// Build guidance from the quality assessment AND the prior source
	// errors - otherwise the retry LLM sees only the quality scorer's
	// opinion about the answer and may rewrite the query from scratch,
	// dropping the original scope. Passing the failed commands + stderr
	// lets it fix the specific flag (e.g. an invalid --owner) while
	// keeping the --repo / entity filters the user actually asked about.
	guidance := buildReExecutionGuidance(qa, results)
	if guidance == "" {
		return answer, qa, reviews, nil
	}

	ui.PrintVerbose("Synthesis validation", fmt.Sprintf("quality %d/5, re-executing with guidance: %s", qa.Quality, truncate(guidance, 100)))

	// Re-execute with guidance
	newResults, err := reExecuteFn(ctx, guidance)
	if err != nil {
		ui.PrintVerbose("Synthesis validation", "re-execution failed: "+err.Error())
		return answer, qa, reviews, nil // return original answer on re-execution failure
	}

	if len(newResults) == 0 {
		ui.PrintVerbose("Synthesis validation", "re-execution returned no results")
		return answer, qa, reviews, nil
	}

	// Merge new results with original results
	mergedResults := append(results, newResults...)

	// Second synthesis with merged results
	answer2, err := Synthesize(ctx, client, question, soulContext, libraryContext, mergedResults, pastLessons)
	if err != nil {
		ui.PrintVerbose("Synthesis validation", "second synthesis failed: "+err.Error())
		return answer, qa, reviews, nil // return original answer on second synthesis failure
	}

	// Score the second answer
	qa2 := scoreQuality(ctx, client, question, answer2)
	if qa2 != nil {
		ui.PrintVerbose("Synthesis validation", fmt.Sprintf("re-synthesis quality: %d/5 (was %d/5)", qa2.Quality, qa.Quality))
		// Use the better answer. When we keep answer2, re-run reviewers
		// on it - the eval signal must reflect the FINAL returned answer.
		if qa2.Quality >= qa.Quality {
			reviews2 := runReviewers(ctx, question, answer2, mergedResults)
			return answer2, qa2, reviews2, nil
		}
		ui.PrintVerbose("Synthesis validation", "re-synthesis was worse, keeping original")
	}

	return answer, qa, reviews, nil
}

// runReviewers runs the default reviewer loop, emits verbose-mode
// log lines for the resulting records, and returns the records to
// the caller for downstream persistence. The loop is observational
// in v1: failures and warnings never block the answer or alter
// retry behavior.
//
// Reviewers run in best-effort mode - a reviewer error surfaces
// once in the verbose log and is otherwise swallowed, because a
// broken sensor must not regress the existing pipeline.
func runReviewers(ctx context.Context, question, answer string, results []sources.SourceResult) []eval.ReviewRecord {
	in := eval.ReviewInput{
		Question: question,
		Answer:   answer,
		Results:  results,
	}
	records, err := eval.Loop(ctx, eval.DefaultReviewers(), in)
	if err != nil {
		ui.PrintVerbose("Reviewers", "loop error: "+err.Error())
	}
	if len(records) == 0 {
		return nil
	}
	verdict := eval.AggregateVerdict(records)
	ui.PrintVerbose("Reviewers", fmt.Sprintf("%d reviewer(s) ran, aggregate=%s", len(records), verdict))
	for _, r := range records {
		ui.PrintVerbose("Reviewer "+r.Reviewer,
			fmt.Sprintf("%s (score=%.2f) - %s", r.Verdict, r.Score, r.Rationale))
		for _, iss := range r.Issues {
			ui.PrintVerbose("Reviewer "+r.Reviewer,
				fmt.Sprintf("  %s [%s]: %s", iss.Type, iss.Severity, iss.Message))
		}
	}
	return records
}

// scoreQuality runs the PRM-lite quality scorer and returns a QualityAssessment.
// Returns nil if the scoring call fails.
func scoreQuality(ctx context.Context, client *llm.Client, question, answer string) *QualityAssessment {
	raw, err := client.CompleteJSONWithStage(
		ctx,
		"quality",
		llm.QualityScoreSystemPrompt,
		llm.QualityScoreUserPrompt(question, answer),
		llm.QualityScoreSchema(),
	)
	if err != nil {
		ui.PrintVerbose("Quality scorer", "failed: "+err.Error())
		return nil
	}

	var qa QualityAssessment
	if err := json.Unmarshal([]byte(raw), &qa); err != nil {
		ui.PrintVerbose("Quality scorer", "parse error: "+err.Error())
		return nil
	}
	return &qa
}

// buildReExecutionGuidance turns a QualityAssessment plus the prior source
// results into actionable guidance for the re-execution step.
//
// It surfaces any failed commands verbatim so the retry LLM fixes the
// specific failing flag (e.g. `--owner` on `gh pr list`) while preserving
// the user's original scope filters (--repo, entity names). Without this,
// the LLM historically rewrote the whole query under generic "try a
// different approach" guidance and dropped the scope - e.g. turning a
// butter_stack-scoped query into `search prs --owner ryanlitalien`,
// which confidently returned 30 off-topic PRs.
func buildReExecutionGuidance(qa *QualityAssessment, results []sources.SourceResult) string {
	var parts []string

	for _, r := range results {
		if r.Status != "error" && r.Status != "timeout" {
			continue
		}
		if r.Command == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf(
			"The previous %q command FAILED: `%s` → %s\nFIX the specific error in that command (e.g. wrong flag, wrong subcommand) and PRESERVE the original scope (keep the same --repo, entity filters, and ARIs). Do NOT rewrite the query from scratch or broaden the scope to an unrelated owner.",
			r.Source, truncate(r.Command, 300), truncate(strings.TrimSpace(r.Summary), 400),
		))
	}

	if qa.MissingInfo != "" {
		parts = append(parts, "Missing information: "+qa.MissingInfo)
	}
	if qa.SuggestedApproach != "" {
		parts = append(parts, "Suggested approach: "+qa.SuggestedApproach)
	}
	if len(parts) == 0 {
		return "The previous answer was insufficient (quality " + fmt.Sprintf("%d", qa.Quality) + "/5). Try a different query approach."
	}
	return strings.Join(parts, "\n\n")
}

// buildSummaries converts SourceResults into the format needed for the synthesis prompt.
func buildSummaries(results []sources.SourceResult) []llm.SourceResultSummary {
	summaries := make([]llm.SourceResultSummary, 0, len(results))

	for _, r := range results {
		summary := llm.SourceResultSummary{
			Source:         r.Source,
			Status:         r.Status,
			Command:        r.Command,
			Summary:        r.Summary,
			Artifacts:      make([]string, 0, len(r.Artifacts)),
			FromContextDoc: r.FromContextDoc,
		}

		// Build artifact descriptions. Whole-response artifacts (subagent
		// answers, single-object JSON responses, fetched pages, file
		// dumps) get a generous limit because chopping them at 200
		// strips everything past the wrapper / first paragraph. Per-row
		// artifacts (one element from a JSON array - sqlite rows,
		// CSV records) keep the narrower 200-char limit so a 50-row
		// query doesn't blow out the prompt budget.
		//
		// "page" gets 16000 (vs 4000 for the other whole-response
		// types) because it's used by DocsAdapter, which dumps an
		// entire context file (often ~8KB, which used
		// to be cut in half at 4000 - fixed 2026-05-01).
		//
		// For per-row artifacts whose JSON contains a title and URL
		// (NYT articles, GitHub PRs/issues, etc.), we surface those
		// two fields explicitly BEFORE truncation so the synthesizer
		// can render Markdown links - otherwise the 200-char cap
		// strips the URL and citations collapse to bare ids.
		for _, a := range r.Artifacts {
			desc := fmt.Sprintf("[%s] %s", a.Type, a.ID)
			if a.Snippet != "" {
				limit := 200
				switch a.Type {
				case "subagent", "subagent-response", "result", "file":
					limit = 4000
				case "page":
					limit = 16000
				}
				if title, url := extractTitleAndURL(a.Snippet); url != "" {
					if title != "" {
						desc += fmt.Sprintf(" - title=%q url=%s", title, url)
					} else {
						desc += " - url=" + url
					}
				}
				desc += ": " + truncate(a.Snippet, limit)
			}
			summary.Artifacts = append(summary.Artifacts, desc)
		}

		// If no summary but we have data, use the raw data as summary
		if summary.Summary == "" && len(r.Data) > 0 {
			summary.Summary = truncate(string(r.Data), 2000)
		}

		summaries = append(summaries, summary)
	}

	return summaries
}

// buildFeedbackHints surfaces the user's FeedbackReason from past lessons
// (notes, thumbs-up, thumbs-down) as historical guidance about how to
// answer similar questions. Only lessons with a non-empty FeedbackReason
// are included - bare verdicts carry no actionable signal.
//
// Past AnswerSnippet content is intentionally NOT surfaced here. The
// snippet is a frozen-in-time copy of what was returned previously; feeding
// it to the synthesizer alongside live source results led the LLM to
// treat stale counts/lists as if they were current and "contradicted" the
// fresh data. The actionable signal (why the prior answer was wrong, what
// style to use, what sources to prefer) lives in FeedbackReason.
func buildFeedbackHints(pastLessons []lessons.SimilarLesson) string {
	var hints []string
	directiveSeen := map[string]bool{}
	var outputDirectives []string
	for _, ls := range pastLessons {
		l := ls.Lesson
		// Pull format directives out regardless of feedback verdict -
		// they apply forward to similar questions whether the user said
		// "ok", "thumbs up", or "thumbs down".
		for _, d := range l.FeedbackOutputDirectives {
			key := strings.ToLower(strings.TrimSpace(d))
			if key == "" || directiveSeen[key] {
				continue
			}
			directiveSeen[key] = true
			outputDirectives = append(outputDirectives, d)
		}
		if l.FeedbackReason == "" {
			continue
		}
		switch l.Feedback {
		case lessons.FeedbackNote:
			hints = append(hints, fmt.Sprintf("USER PROVIDED ADDITIONAL CONTEXT on similar question %q:\n%s", l.Question, l.FeedbackReason))
		case lessons.FeedbackThumbsDown:
			hints = append(hints, fmt.Sprintf("USER FLAGGED SIMILAR ANSWER AS BAD for %q:\n%s", l.Question, l.FeedbackReason))
		case lessons.FeedbackThumbsUp:
			hints = append(hints, fmt.Sprintf("User confirmed similar answer was good for %q: %s", l.Question, truncate(l.FeedbackReason, 500)))
		}
	}
	if len(hints) == 0 && len(outputDirectives) == 0 {
		return ""
	}
	var b strings.Builder
	if len(outputDirectives) > 0 {
		// Strong, MUST-FOLLOW framing - distinct from the soft "historical
		// guidance" hints below. These are forward-looking style rules the
		// user explicitly stated; the synthesizer must apply them to the
		// answer it is about to write.
		b.WriteString("OUTPUT REQUIREMENTS FROM PRIOR USER FEEDBACK (apply these to the answer you write - these override the default ANSWER STYLE rules when they conflict):\n")
		for _, d := range outputDirectives {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		b.WriteString("\n")
	}
	if len(hints) > 0 {
		b.WriteString("PRIOR USER FEEDBACK ON SIMILAR QUESTIONS (historical guidance about style, routing, and past corrections - NOT current data; the \"Results from data sources:\" block below is the only ground truth for facts, counts, and lists):\n\n")
		b.WriteString(strings.Join(hints, "\n\n"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncate shortens a string to maxLen, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// extractTitleAndURL pulls a title-like and URL-like field from a JSON
// snippet. Per-row truncation strips URLs from synthesizer input; surfacing
// them up front lets the LLM render Markdown links per the synthesis prompt
// rule. Returns ("", "") when the snippet isn't a JSON object or has no
// recognised fields.
//
// URL precedence matches the synthesis prompt: web_url > url > html_url >
// permalink > link > short_url. Title precedence: title > headline.main >
// headline > name > subject.
func extractTitleAndURL(snippet string) (string, string) {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(snippet), &obj); err != nil {
		return "", ""
	}
	urlKeys := []string{"web_url", "url", "html_url", "permalink", "link", "short_url"}
	var url string
	for _, k := range urlKeys {
		if v, ok := obj[k].(string); ok && v != "" {
			url = v
			break
		}
	}
	var title string
	if v, ok := obj["title"].(string); ok && v != "" {
		title = v
	} else if hl, ok := obj["headline"].(map[string]interface{}); ok {
		if v, ok := hl["main"].(string); ok {
			title = v
		}
	} else if v, ok := obj["headline"].(string); ok && v != "" {
		title = v
	} else if v, ok := obj["name"].(string); ok && v != "" {
		title = v
	} else if v, ok := obj["subject"].(string); ok && v != "" {
		title = v
	}
	return title, url
}
