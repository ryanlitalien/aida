package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/refusal"
	"github.com/ryanlitalien/aida/internal/sources"
	"github.com/ryanlitalien/aida/internal/ui"
)

const (
	maxSourceRetries       = 2   // max retries per source (not counting original attempt)
	lowConfidenceThreshold = 0.3 // below this, retry with rephrase
)

// currentDatetimeLine returns the current local datetime, formatted for
// injection into the query-construct and synthesis prompts, so the LLM
// has ground truth for "today"/"now"/"latest" questions and can flag
// stale volatile values (clocks, prices, scores) embedded in web
// snippets -- see the VOLATILE DATA GUARD in SynthesisSystemPrompt.
//
// Loads config to resolve the user's configured timezone; falls back to
// the OS-local zone if config can't be loaded (a missing/broken config
// file shouldn't block every query over a cosmetic prompt detail). Shared
// by executor.go (query construction) and synthesizer.go (synthesis) --
// both are in this package, so no exported API surface needs to change.
func currentDatetimeLine() string {
	loc := time.Local
	if cfg, err := config.LoadConfig(); err == nil && cfg != nil {
		loc = cfg.Location()
	}
	return time.Now().In(loc).Format(time.RFC1123)
}

// ExecutionResult holds results from all phases.
type ExecutionResult struct {
	PhaseResults []PhaseResult
	AllResults   []sources.SourceResult
}

// PhaseResult holds results from a single phase.
type PhaseResult struct {
	Phase   string
	Results []sources.SourceResult
}

// Execute is Step 5 of the pipeline: LLM Call #2 for query construction + parallel shell execution.
//
// libraryContext is an optional pre-materialized markdown blob from the
// aida library (layers resolved by routes or by the all-available
// fallback). It is prepended to every source's per-source context doc so the
// LLM has the same baseline knowledge regardless of whether the target
// project folder contains a CLAUDE.md.
//
// When a phase has DependsOnPrior=true, the cumulative artifact summary
// from all earlier phases is passed into the LLM Call #2 prompt for that
// phase's sources -- turning DependsOnPrior from "sequencing" into actual
// data flow.
//
// Execution is driven by the orchestrator's composite primitives:
// parallel phases fan out via ParallelAgent, sequential/dependent
// phases run one at a time with prior results summarized as context.
// Structured SourceResults are captured via a side-channel so the
// synthesizer gets the same data shape it always has.
func Execute(ctx context.Context, client *llm.Client, plan *ExecutionPlan, intent *Intent, resolved *ResolvedContext, libraryContext, repoHint string, allowedGHRepos map[string]bool, pastLessons []lessons.SimilarLesson) (*ExecutionResult, error) {
	execResult := &ExecutionResult{}

	feedbackCtx := buildExecutorFeedback(pastLessons)

	for _, phase := range plan.Phases {
		var priorSummary string
		if phase.DependsOnPrior && len(execResult.AllResults) > 0 {
			priorSummary = summarizePriorResults(execResult.AllResults)
		}

		// Build a SourceExecFunc for each source that captures its
		// structured result in a concurrent-safe map while returning
		// formatted text for the orchestrator's composition.
		var mu sync.Mutex
		sourceResults := make(map[string]sources.SourceResult)

		execs := make(map[string]SourceExecFunc)
		for _, scored := range phase.Sources {
			scored := scored
			ps := priorSummary
			execs[scored.Name] = func(ctx context.Context, _ string) (string, error) {
				sr, err := executeSource(ctx, client, scored, intent, resolved, libraryContext, repoHint, allowedGHRepos, ps, feedbackCtx)
				if err != nil {
					sr = sources.SourceResult{
						Source:  scored.Name,
						Status:  "error",
						Summary: err.Error(),
					}
				}
				mu.Lock()
				sourceResults[scored.Name] = sr
				mu.Unlock()
				return formatSourceResult(sr), nil
			}
		}

		// Run the phase through the orchestrator.
		phasePlan := &ExecutionPlan{Phases: []Phase{phase}}
		runnable := PlanToRunnable(phasePlan, execs)
		if _, err := runnable.Run(ctx, intent.EffectiveQuery()); err != nil {
			return nil, fmt.Errorf("phase %s failed: %w", phase.Name, err)
		}

		// Collect structured results in plan order.
		pr := PhaseResult{Phase: phase.Name}
		for _, scored := range phase.Sources {
			if r, ok := sourceResults[scored.Name]; ok {
				pr.Results = append(pr.Results, r)
			}
		}
		execResult.PhaseResults = append(execResult.PhaseResults, pr)
		execResult.AllResults = append(execResult.AllResults, pr.Results...)
	}

	return execResult, nil
}

// buildExecutorFeedback extracts actionable context from past lessons
// with user feedback. This gives the query constructor knowledge about
// what worked or failed in prior runs of similar questions.
func buildExecutorFeedback(pastLessons []lessons.SimilarLesson) string {
	var parts []string
	for _, ls := range pastLessons {
		l := ls.Lesson
		if l.FeedbackReason == "" {
			continue
		}
		var prefix string
		switch l.Feedback {
		case lessons.FeedbackNote:
			prefix = "User provided additional context on a similar query"
		case lessons.FeedbackThumbsDown:
			prefix = "User flagged a similar query as BAD"
		case lessons.FeedbackThumbsUp:
			prefix = "User confirmed a similar query worked well"
		default:
			continue
		}
		// Full feedback for executor - no truncation. This is where
		// rich data (ARI tables, correction details) lives.
		parts = append(parts, fmt.Sprintf("%s (question: %q):\n%s", prefix, l.Question, l.FeedbackReason))
		// Include the prior answer snippet so the LLM can see what was insufficient
		if l.AnswerSnippet != "" && l.Feedback != lessons.FeedbackThumbsUp {
			parts = append(parts, fmt.Sprintf("Prior answer (which was insufficient): %s", l.AnswerSnippet))
		}
	}
	// Inject prior successful queries from high-quality lessons so the
	// query constructor can reuse known-good SQL/commands instead of
	// generating from scratch (which risks hallucinating column names).
	for _, ls := range pastLessons {
		l := ls.Lesson
		if (l.Quality >= 4 || l.Feedback == lessons.FeedbackThumbsUp) && l.ExecutedQueries != nil {
			for src, cmd := range l.ExecutedQueries {
				parts = append(parts, fmt.Sprintf("Prior successful query for source %q (similar question %q):\n%s", src, l.Question, cmd))
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// summarizePriorResults builds a compact, LLM-readable summary of artifacts
// from previous phases, used as additional context for chained phases. Only
// successful results contribute artifacts; errored sources are noted as
// having failed so the next phase doesn't try to extract data from nothing.
func summarizePriorResults(results []sources.SourceResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.Status == "error" {
			fmt.Fprintf(&b, "- %s: ERROR (%s)\n", r.Source, truncateLine(r.Summary, 200))
			continue
		}
		fmt.Fprintf(&b, "- %s: %d artifact(s)\n", r.Source, len(r.Artifacts))
		for i, a := range r.Artifacts {
			if i >= 5 {
				fmt.Fprintf(&b, "  ... +%d more\n", len(r.Artifacts)-5)
				break
			}
			line := fmt.Sprintf("[%s] %s", a.Type, a.ID)
			if a.Snippet != "" {
				line += ": " + truncateLine(a.Snippet, 200)
			}
			fmt.Fprintf(&b, "  - %s\n", line)
		}
	}
	return b.String()
}

func truncateLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// executeSource handles a single source with retry logic: calls executeSourceOnce
// in a loop, retrying on errors, empty results, or low-confidence results.
func executeSource(ctx context.Context, client *llm.Client, scored ScoredSource, intent *Intent, resolved *ResolvedContext, libraryContext, repoHint string, allowedGHRepos map[string]bool, priorSummary, feedbackCtx string) (sources.SourceResult, error) {
	src := scored.Source

	// Don't retry claude-project or legacy-DocsAdapter sources - they
	// handle their own retries or bypass query construction entirely.
	// Grep-mode docs sources DO benefit from retries because the LLM's
	// first regex pattern may return empty and a broader pattern may
	// hit on the second try.
	legacyDocs := src.Type == "docs" && (src.Search == nil || src.Search.Mode == "" || src.Search.Mode == "none")
	inventoryType := src.Type == "inventory" || src.Type == "library-inventory"
	skipRetry := src.Type == "claude-project" || legacyDocs || inventoryType

	var lastResult sources.SourceResult
	var retryContext string
	totalDuration := time.Duration(0)

	for attempt := 0; attempt <= maxSourceRetries; attempt++ {
		// Check context cancellation
		if ctx.Err() != nil {
			return sources.SourceResult{
				Source:  scored.Name,
				Status:  "error",
				Summary: ctx.Err().Error(),
			}, nil
		}

		result, err := executeSourceOnce(ctx, client, scored, intent, resolved, libraryContext, repoHint, allowedGHRepos, priorSummary, retryContext, feedbackCtx)
		if err != nil {
			return result, err
		}

		totalDuration += result.Duration
		result.Duration = totalDuration
		lastResult = result

		// Don't retry these source types
		if skipRetry {
			return result, nil
		}

		// No more retries left
		if attempt >= maxSourceRetries {
			break
		}

		// Decide whether to retry based on result quality
		retryContext = ""

		if result.Status == "error" && result.Summary != "" {
			// Don't retry unrecoverable environment errors - rephrasing
			// the query can't fix a missing binary or permission issue.
			if isUnrecoverableError(result.Summary) {
				ui.PrintVerbose(fmt.Sprintf("Retry [%s]", scored.Name), "skipping: unrecoverable error")
				return result, nil
			}
			retryContext = fmt.Sprintf(
				"RETRY (attempt %d/%d): Your prior query failed with this error:\n%s\n\nFix the query to avoid this error. Try a completely different approach.",
				attempt+2, maxSourceRetries+1, truncateLine(result.Summary, 300),
			)
		} else if result.Status == "timeout" {
			// A timeout can be transient (load, a slow upstream) or a sign
			// the query itself is too broad/expensive -- retry with a
			// narrower or cheaper query rather than silently accepting
			// "timed out" as good enough. If every retry also times out,
			// this falls through to the maxSourceRetries break below and
			// lastResult keeps Status "timeout", so the caller never sees
			// it coerced into a clean result.
			retryContext = fmt.Sprintf(
				"RETRY (attempt %d/%d): Your prior query timed out:\n%s\n\nTry a narrower or cheaper query -- e.g. a tighter filter, a smaller time window, or fewer rows/joins.",
				attempt+2, maxSourceRetries+1, truncateLine(result.Command, 300),
			)
		} else if result.Status == "empty" {
			retryContext = fmt.Sprintf(
				"RETRY (attempt %d/%d): Your prior query returned NO results. The query was:\n%s\n\nTry a broader search pattern, different table names, or a different approach entirely.",
				attempt+2, maxSourceRetries+1, truncateLine(result.Command, 300),
			)
		} else if result.Status == "success" {
			// Quick computational confidence check (~0ms, no LLM call)
			confidence, _, reason := computationalScore(result, intent.RawQuery, intent)
			if confidence < lowConfidenceThreshold {
				retryContext = fmt.Sprintf(
					"RETRY (attempt %d/%d): Your prior query returned results but they seem unrelated to the question (confidence: %.2f, reason: %s). The query was:\n%s\n\nTry a more targeted query that directly addresses: %s",
					attempt+2, maxSourceRetries+1, confidence, reason, truncateLine(result.Command, 300), truncateLine(intent.RawQuery, 200),
				)
			}
		}

		// If no retry context was set, the result is good enough
		if retryContext == "" {
			return result, nil
		}

		ui.PrintVerbose(fmt.Sprintf("Retry [%s]", scored.Name), fmt.Sprintf("attempt %d/%d: %s", attempt+2, maxSourceRetries+1, truncateLine(retryContext, 100)))
	}

	return lastResult, nil
}

// executeSourceOnce handles a single attempt at executing a source: load context,
// build query via LLM, execute. The retryContext parameter, when non-empty, is
// appended to the LLM query constructor prompt so the LLM knows what went wrong
// on the previous attempt.
func executeSourceOnce(ctx context.Context, client *llm.Client, scored ScoredSource, intent *Intent, resolved *ResolvedContext, libraryContext, repoHint string, allowedGHRepos map[string]bool, priorSummary string, retryContext, feedbackCtx string) (sources.SourceResult, error) {
	src := scored.Source

	// 1. Load the source's context file. If a library context bundle was
	// provided, prepend it so the LLM gets library layers as the baseline
	// knowledge -- this is what makes aida work for read-only project
	// folders that have no CLAUDE.md of their own.
	contextDoc, _ := src.LoadContextFile()
	if libraryContext != "" {
		if contextDoc == "" {
			contextDoc = libraryContext
		} else {
			contextDoc = libraryContext + "\n\n---\n\n" + contextDoc
		}
	}

	// 1b. Append relevant query templates if source has them
	if queryTemplates := src.LoadQueryTemplates(""); queryTemplates != "" {
		contextDoc += queryTemplates
	}

	// 2. Determine exec template
	execTemplate := getExecTemplate(src)

	// 2b. For gh-based sources, append a "known repos" table built from
	// every other library source that has a `repo:` field. Without this
	// the query-construction LLM has no map from entity names the user
	// speaks (e.g. "acme-widgets", "aida") to the actual owner/repo
	// pairs stored on those sources - it historically fell back to
	// guessing `--owner ryanlitalien` and returned off-topic results.
	// The hint is only relevant for gh because `--repo owner/name` is
	// a gh-specific concept; injecting it for sqlite/exec/etc would
	// be context bloat.
	if repoHint != "" && isGHExecTemplate(execTemplate) {
		contextDoc += repoHint
	}

	// 3. Build the query for the adapter.
	//
	// For claude-project sources we BYPASS LLM Call #2 entirely. The
	// delegated Claude Code sub-agent running inside the project folder
	// already has its own CLAUDE.md, .mcp.json, and any project-specific
	// persona -- rewriting the question here only introduces bias from
	// the project's context doc (e.g. a "Coach Agent" persona would
	// reframe "latest workout" as "today's workout"). Passing the user's
	// raw question through is both cheaper and more accurate.
	//
	// For docs sources in the legacy DocsAdapter path (no search.mode or
	// search.mode="" / "none"), we also bypass - DocsAdapter reads the
	// context file directly. But when search.mode == "grep" the source
	// is routed to GrepAdapter and needs a regex pattern, not the raw
	// question. Skipping LLM #2 in that case sends the user's English
	// sentence to grep, which matches nothing. So grep-mode docs sources
	// go through the normal query-construction path.
	docsBypass := src.Type == "docs" && (src.Search == nil || src.Search.Mode == "" || src.Search.Mode == "none")
	inventoryBypass := src.Type == "inventory" || src.Type == "library-inventory"
	var command string
	if src.Type == "claude-project" || docsBypass || inventoryBypass {
		command = strings.TrimSpace(intent.EffectiveQuery())
		// Enrich the question with prior context so the sub-agent can
		// resolve references like "it", "that", "#486" from earlier turns.
		if priorSummary != "" && src.Type == "claude-project" {
			command = command + "\n\nContext from prior results:\n" + priorSummary
		}
	} else {
		resolvedValues := resolved.GetResolvedValues()
		if intent.Timeframe != "" {
			resolvedValues["timeframe"] = intent.Timeframe
		}
		if intent.ComprehensiveIntent {
			resolvedValues["_comprehensive_intent"] = "true"
		}
		userPrompt := llm.QueryConstructUserPrompt(scored.Name, contextDoc, execTemplate, intent.EffectiveQuery(), resolvedValues, priorSummary, feedbackCtx, currentDatetimeLine())
		if retryContext != "" {
			userPrompt += "\n\n" + retryContext
		}
		raw, err := client.CompleteWithStage(
			ctx,
			"execute",
			llm.QueryConstructSystemPrompt,
			userPrompt,
		)
		if err != nil {
			return sources.SourceResult{}, fmt.Errorf("query construction for %s: %w", scored.Name, err)
		}
		command = stripCodeFences(strings.TrimSpace(raw))

		// LLM Call #2 occasionally emits refusal prose ("I cannot build a
		// query for...") instead of an actual command. Left unchecked,
		// that prose gets executed verbatim as a shell command / SQL
		// query / gh invocation. Catch it here (LLM-generated path only -
		// the claude-project/docs/inventory bypass above never reaches
		// this branch) and turn it into a failed attempt with no error,
		// so executeSource's retry loop treats it exactly like any other
		// "error" status: one more attempt, then land on "error" with no
		// artifacts for the synthesizer to cite.
		if result, refused := refusalResult(scored.Name, command); refused {
			return result, nil
		}
	}

	if sanitized, note := sanitizeGHCommand(command); note != "" {
		ui.PrintVerbose(fmt.Sprintf("Sanitize [%s]", scored.Name), note)
		command = sanitized
	}

	ui.PrintVerbose(fmt.Sprintf("Command [%s]", scored.Name), command)

	// 4. Get the appropriate adapter (source-aware selection)
	adapter := sources.GetAdapterForSource(scored.Name, *src)

	// 5. Execute via adapter
	execStart := time.Now()
	result, err := adapter.Execute(ctx, command, *src)
	execDuration := time.Since(execStart)
	if err != nil {
		// An adapter that sets result.Status alongside its error (e.g.
		// exec.go's "timeout") is reporting a specific KIND of failure,
		// not just "something went wrong" -- the retry chain above and
		// the synthesizer both branch on that distinction, so it must
		// survive this error path instead of being flattened to the
		// generic "error" status. Only fall back to "error" when the
		// adapter returned a zero-value result (a genuine adapter-level
		// failure with no status of its own).
		status := "error"
		summary := err.Error()
		if result.Status != "" {
			status = result.Status
			if result.Summary != "" {
				summary = result.Summary
			}
		}
		return sources.SourceResult{
			Source:   scored.Name,
			Status:   status,
			Summary:  summary,
			Duration: execDuration,
		}, nil
	}

	result.Source = scored.Name
	result.Duration = execDuration
	result.Command = command

	// Profile-strict GH allowlist: drop any artifact whose repo is not in
	// the active profile's allowlist. Belt-and-suspenders against the LLM
	// emitting a broader `--owner` query than the layer doc allows. The
	// allowlist is built from every profile-active source yaml's `repo:`
	// field, so untagged repos and other-profile repos are silently dropped.
	if isGHExecTemplate(execTemplate) && len(allowedGHRepos) > 0 && len(result.Artifacts) > 0 {
		filtered, dropped := filterGHArtifactsByAllowlist(result.Artifacts, allowedGHRepos)
		if dropped > 0 {
			ui.PrintVerbose(fmt.Sprintf("Allowlist [%s]", scored.Name), fmt.Sprintf("dropped %d off-profile artifact(s)", dropped))
			result.Artifacts = filtered
			if len(filtered) == 0 {
				result.Status = "empty"
				result.Summary = fmt.Sprintf("all %d artifacts were outside the active profile's allowlist", dropped)
			}
		}
	}

	// Context file fallback: if a grep/codebase source returned empty and
	// the source has a context file (CLAUDE.md, README.md), try reading
	// the context file directly via the docs adapter. This handles the
	// common case where a documentation question gets routed to a codebase
	// source but grep can't find the answer in source files.
	if result.Status == "empty" && src.Context != "" && contextDoc != "" {
		docsAdapter := sources.GetAdapter("docs")
		if docsAdapter != nil {
			docsResult, docsErr := docsAdapter.Execute(ctx, command, *src)
			if docsErr == nil && docsResult.Status == "success" && len(docsResult.Artifacts) > 0 {
				docsResult.Source = scored.Name
				docsResult.Duration = execDuration
				docsResult.Command = command
				// These artifacts are the source's documentation ABOUT
				// itself, not data queried FROM it -- flag that so
				// downstream consumers (synthesizer, verifier) don't
				// treat a doc snippet as if it answered a data question.
				docsResult.FromContextDoc = true
				ui.PrintVerbose(fmt.Sprintf("Fallback [%s]", scored.Name), "grep empty, used context file")
				return docsResult, nil
			}
		}
	}

	ui.PrintVerbose(fmt.Sprintf("Result [%s]", scored.Name), fmt.Sprintf("status=%s, artifacts=%d, duration=%s", result.Status, len(result.Artifacts), execDuration.Round(time.Millisecond)))

	return result, nil
}

// refusalResult builds the SourceResult for a query-construction refusal.
// Split out from executeSourceOnce so the classification itself is
// unit-testable without a live (or faked) LLM client -- callers only need
// to pass the already-generated command string.
//
// Uses the NARROW command-refusal check: a generated web-search query that
// echoes the user's phrasing ("why i can't focus in the mornings") must
// not be misclassified just because it contains "i can't" near the head.
func refusalResult(sourceName, command string) (sources.SourceResult, bool) {
	if !refusal.LooksLikeCommandRefusal(command) {
		return sources.SourceResult{}, false
	}
	return sources.SourceResult{
		Source:  sourceName,
		Status:  "error",
		Summary: "query generation refused: " + refusal.FirstLine(command),
	}, true
}

// isUnrecoverableError returns true for errors that can't be fixed by
// rephrasing the query - e.g. the tool binary isn't installed, or the
// user doesn't have permission. Retrying these just burns LLM cost.
func isUnrecoverableError(summary string) bool {
	lower := strings.ToLower(summary)
	for _, marker := range []string{
		"command not found",
		"exit status 127", // shell: command not found
		"no such file or directory",
		"permission denied",
		"exit status 126", // shell: permission denied
		"connection refused",
		"no route to host",
		"network is unreachable",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// getExecTemplate returns the primary exec template for a source.
func getExecTemplate(src *config.Source) string {
	if src.Exec == nil {
		// Grep-mode docs sources and plain codebase sources have no
		// exec map - the adapter is grep. The prompt needs an
		// unambiguous marker so the LLM does not fall back to
		// gh-style output based on keywords in the question (e.g.
		// "issue" made Run A emit "search issues --owner ..." as the
		// grep pattern). "GREP_PATTERN" in the synthetic template is
		// the signal.
		if (src.Type == "docs" && src.Search != nil && src.Search.Mode == "grep") ||
			src.Type == "codebase" {
			return "# GREP source - return ONLY a regex pattern (no flags, no shell, no gh command): grep -rn -E 'GREP_PATTERN' " + src.Path
		}
		return ""
	}
	// Try common keys in order
	for _, key := range []string{"query", "search", "run", "test", "start"} {
		if tmpl, ok := src.Exec[key]; ok {
			return tmpl
		}
	}
	// Return first available
	for _, tmpl := range src.Exec {
		return tmpl
	}
	return ""
}

// stripCodeFences removes markdown code fences from LLM output.
func stripCodeFences(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) >= 2 && strings.HasPrefix(lines[0], "```") && strings.HasSuffix(lines[len(lines)-1], "```") {
		return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	}
	return s
}

// filterGHArtifactsByAllowlist drops github-shaped artifacts whose repo is
// not in `allowed` (lowercase owner/repo keys). Returns the filtered slice
// and the count dropped. Artifacts whose snippet doesn't yield a parseable
// repo are kept (we can't verify, and dropping them risks losing legitimate
// results from non-search subcommands).
func filterGHArtifactsByAllowlist(artifacts []sources.Artifact, allowed map[string]bool) ([]sources.Artifact, int) {
	out := make([]sources.Artifact, 0, len(artifacts))
	dropped := 0
	for _, a := range artifacts {
		repo := extractRepoFromArtifact(a)
		if repo == "" {
			out = append(out, a)
			continue
		}
		if allowed[strings.ToLower(repo)] {
			out = append(out, a)
		} else {
			dropped++
		}
	}
	return out, dropped
}

// extractRepoFromArtifact pulls owner/repo out of a github CLI artifact's
// JSON snippet. Handles the common shapes: top-level fullName / nameWithOwner
// / full_name, plus a nested `repository` object using the same fields.
// Returns "" when no repo identifier is found.
func extractRepoFromArtifact(a sources.Artifact) string {
	if a.Snippet == "" {
		return ""
	}
	var row map[string]interface{}
	if err := json.Unmarshal([]byte(a.Snippet), &row); err != nil {
		return ""
	}
	for _, key := range []string{"fullName", "nameWithOwner", "full_name"} {
		if v, ok := row[key].(string); ok && strings.Contains(v, "/") {
			return v
		}
	}
	if repo, ok := row["repository"]; ok {
		switch r := repo.(type) {
		case string:
			if strings.Contains(r, "/") {
				return r
			}
		case map[string]interface{}:
			for _, key := range []string{"nameWithOwner", "full_name", "fullName"} {
				if v, ok := r[key].(string); ok && strings.Contains(v, "/") {
					return v
				}
			}
		}
	}
	return ""
}

// isGHExecTemplate reports whether a source's exec template shells out to
// the GitHub CLI (`gh ...`). We check both unsubstituted templates like
// "gh {query}" and post-substitution strings by matching the leading token.
func isGHExecTemplate(tmpl string) bool {
	t := strings.TrimSpace(tmpl)
	return strings.HasPrefix(t, "gh ") || t == "gh" || strings.HasPrefix(t, "gh\t")
}

// sanitizeGHCommand strips flags that the LLM sometimes emits on the wrong
// gh subcommand. Today's one rule: `--owner` is NOT valid on `gh pr list` /
// `gh issue list` (it's a `gh search prs` / `gh search issues` flag). The
// system prompt documents this but the LLM drifts under retry pressure, so
// a deterministic belt-and-suspenders pass saves the run from erroring.
//
// No-op for non-gh commands because the prefix check fails. Returns the
// possibly-rewritten command and a short note when something changed
// (empty string means no change - caller skips the verbose log).
func sanitizeGHCommand(cmd string) (string, string) {
	trimmed := strings.TrimSpace(cmd)
	if !(strings.HasPrefix(trimmed, "pr list") || strings.HasPrefix(trimmed, "issue list")) {
		return cmd, ""
	}
	tokens := strings.Fields(trimmed)
	out := make([]string, 0, len(tokens))
	removed := false
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if t == "--owner" {
			removed = true
			// Skip the flag AND its value if the next token isn't another flag.
			if i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "--") {
				i++
			}
			continue
		}
		if strings.HasPrefix(t, "--owner=") {
			removed = true
			continue
		}
		out = append(out, t)
	}
	if !removed {
		return cmd, ""
	}
	return strings.Join(out, " "), "stripped invalid --owner flag (not supported by 'gh pr list'/'gh issue list'; use 'gh search prs --owner' for cross-repo scope)"
}
