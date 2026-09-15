package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine"
	"github.com/ryanlitalien/aida/internal/eval"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/refusal"
	"github.com/ryanlitalien/aida/internal/runs"
	"github.com/ryanlitalien/aida/internal/sources"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

// runQuery is the default command handler. It executes the full 6-step pipeline.
func runQuery(cmd *cobra.Command, args []string) error {
	var question string
	if len(args) == 0 {
		// No args: read from stdin. Avoids shell expansion issues with
		// $, ', and other special characters.
		// Only show prompt if stdin is a terminal (interactive mode).
		if isTerminal(os.Stdin) {
			fmt.Fprint(os.Stderr, "aida> ")
		}
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			if isTerminal(os.Stdin) {
				return nil // user hit ctrl-D
			}
			return cmd.Help() // non-interactive with no input
		}
		question = strings.TrimSpace(scanner.Text())
		if question == "" {
			return nil
		}
	} else {
		question = strings.Join(args, " ")
	}
	ctx := context.Background()
	totalStart := time.Now()

	// ---------------------------------------------------------------
	// Load configuration
	// ---------------------------------------------------------------
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// ---------------------------------------------------------------
	// Pre-pipeline: handle #N task shortcuts (need to resolve before
	// the LLM runs because #N is positional, not semantic).
	// ---------------------------------------------------------------
	_, profileNameEarly := cfg.ActiveProfileConfig()
	if HandleTaskShortcut(question, cfg, profileNameEarly) {
		return nil
	}

	// Zero-LLM fast-path for exact "what time is it" / "what's today's
	// date" style questions -- see internal/cli/time_shortcut.go.
	if HandleTimeShortcut(question, cfg) {
		return nil
	}

	// Swarm intercept: "swarm on PR 456" / "work through my prio tasks" is a
	// natural-language request to run the autonomous loop. Recognize it,
	// confirm a plan, and hand off to the loop (task set) or a PR-continuation
	// agent. handled=true means it owned the request (ran or was declined);
	// a non-swarm or non-interactive request falls straight through.
	if handled, serr := HandleSwarmIntent(ctx, question, cfg, profileNameEarly); handled {
		return serr
	}

	// Sources may come from the library (preferred) or from the legacy
	// ~/.aida/sources.yaml as a fallback. resolveSources
	// encapsulates that decision so query.go and
	// golden.go behave identically.
	srcs, srcOrigin, err := resolveSources()
	if err != nil {
		return err
	}
	ui.PrintVerbose("Sources from", srcOrigin)

	// ---------------------------------------------------------------
	// Detect active profile
	// ---------------------------------------------------------------
	profile, profileName := cfg.ActiveProfileConfig()
	ui.PrintVerbose("Profile", profileName)

	// ---------------------------------------------------------------
	// Brain startup: open brain, background git pull, stale check.
	// ---------------------------------------------------------------
	brn, brainErr := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if brainErr != nil {
		ui.PrintVerbose("Brain", "unavailable: "+brainErr.Error())
	} else {
		defer brn.Close()
		// Background git pull (non-blocking, runs during parse step)
		if cfg.Brain.AutoSync {
			<-brain.Pull(cfg.BrainPath()) // wait for pull to complete before we read
		}
		// Stale check. Phase 4.2: instead of blocking for ~3 minutes to
		// re-embed every lesson (the real cost of a full rebuild with
		// ~750+ lessons), we detach a background `aida brain index` child
		// process so the current query proceeds immediately. The child
		// survives parent exit via Setpgid and writes brain.db in place.
		// Subsequent queries pick up the rebuilt index naturally via
		// brain.Open.
		if brain.IsStale(cfg.BrainPath()) {
			if err := spawnDetachedBrainIndex(); err != nil {
				// Fallback to synchronous rebuild if we can't detach
				// (unknown env, locked-down sandbox). Better to block
				// than to skip indexing silently.
				ui.PrintVerbose("Brain index", "detach failed, falling back to sync: "+err.Error())
				brainSpinner := ui.NewSpinner()
				brainSpinner.Start("Rebuilding brain index (sync fallback)...")
				if err := brn.Index(ctx); err != nil {
					brainSpinner.Stop(fmt.Sprintf("%s Brain index rebuild error", ui.WarnIcon))
					ui.PrintVerbose("Brain index", "rebuild error: "+err.Error())
				} else {
					brain.TouchDB(cfg.BrainPath())
					brainSpinner.Stop(fmt.Sprintf("%s Brain indexed", ui.SuccessIcon))
				}
			} else {
				ui.PrintVerbose("Brain index", "detached rebuild in background")
			}
		}

		// Wiki stale check -- same detach-and-rebuild pattern, applied to
		// the wiki_pages recall channel (see (*brain.DB).WikiIsStale). No
		// sync fallback here: unlike the brain index, a stale wiki_pages
		// table doesn't block anything in this query, it just means the
		// wiki: recall channel serves slightly older results until the
		// background run finishes. Skipped when offline, mirroring the
		// auto-consolidate guard below -- a background index run still
		// calls out to Voyage for embeddings when a key is configured, so
		// don't kick one off while the user explicitly wants to stay local.
		if !offline && !cfg.Model.OfflineMode && brn.DB.WikiIsStale(cfg.WikiPath()) {
			if err := spawnDetachedWikiIndex(); err != nil {
				ui.PrintVerbose("Wiki index", "detach failed: "+err.Error())
			} else {
				ui.PrintVerbose("Wiki index", "detached rebuild in background")
			}
		}
	}

	// ---------------------------------------------------------------
	// Tier-1 mined-question cache: a confident match here means a past
	// run already answered this exact (or near-duplicate) question, so
	// skip Parse/Classify/Resolve/Plan/Execute/Synthesize entirely --
	// zero LLM calls, not just a skipped router/execute/synthesize.
	// Disabled for --agent (explicit agent-loop request), --dry-run and
	// --explain (both want to see the real routing plan, not a cached
	// shortcut), and --continue (wants prior-turn context, not a flat
	// cache reuse). brn == nil (brain unavailable) or no confident
	// match both fall straight through to the normal pipeline below --
	// LookupRunCache never errors for those cases, only for a genuine
	// DB failure.
	// ---------------------------------------------------------------
	if brn != nil && !agentMode && !dryRun && !ui.Explain && !continueSession {
		if hit, ok, cacheErr := brn.LookupRunCache(ctx, question, profileName); cacheErr != nil {
			ui.PrintVerbose("Tier-1 cache", "lookup error: "+cacheErr.Error())
		} else if ok {
			ui.PrintResult(hit.Answer)
			ui.PrintVerbose("Tier-1 cache", fmt.Sprintf("hit (source run %s, %d prior hit(s))", hit.SourceRunID, hit.HitCount))

			cacheCwd, _ := os.Getwd()
			cacheRun := &runs.Run{
				StartedAt: totalStart,
				TotalMs:   time.Since(totalStart).Milliseconds(),
				Question:  question,
				Cwd:       cacheCwd,
				Profile:   profileName,
				Action:    "cache_hit",
				Strategy:  "tier1-cache",
				Answer:    hit.Answer,
			}
			if _, saveErr := runs.Save(cacheRun); saveErr != nil {
				ui.PrintVerbose("Run log", "save error: "+saveErr.Error())
			}
			fmt.Fprintf(os.Stderr, "[aida-run-id:%s]\n", cacheRun.ID)

			if hit.ID != "" {
				if touchErr := brn.DB.TouchRunCache(hit.ID, time.Now().UTC().Format(time.RFC3339)); touchErr != nil {
					ui.PrintVerbose("Tier-1 cache", "touch error: "+touchErr.Error())
				}
			}
			return nil
		}
	}

	// ---------------------------------------------------------------
	// Load aida library (multi-root, tool-filtered).
	// ---------------------------------------------------------------
	libRegistry, libErr := library.LoadRegistry(config.Dir())
	if libErr != nil {
		ui.PrintVerbose("Library", "load error: "+libErr.Error())
	}

	// ---------------------------------------------------------------
	// Set up LLM client
	// ---------------------------------------------------------------
	apiKey := cfg.GetAPIKey()
	model := cfg.Model.Primary
	isOffline := offline || cfg.Model.OfflineMode

	if isOffline {
		model = llm.ResolveOfflineModel(cfg.Model.Fallback, cfg.Model.Offline)
	}

	if !isOffline && apiKey == "" {
		return fmt.Errorf("no API key found -- set %s or run with --offline", cfg.API.AnthropicKeyEnv)
	}

	client := llm.NewClient(apiKey, model, isOffline)
	if cfg.Model.Stages != nil {
		client.SetStageModels(cfg.Model.Stages)
	}
	ui.PrintVerbose("Model", client.Model())

	// Load soul.yaml - the user's identity and routing preferences.
	// Empty soul (no file) is fine - prompts just skip the block.
	soul, _ := config.LoadSoul()
	soulCtx := soul.ForPrompt()

	// --continue: inject prior run context for follow-up queries
	if continueSession {
		if lastRun, err := runs.Latest(); err == nil && lastRun != nil {
			priorCtx := buildContinuationContext(lastRun)
			soulCtx = soulCtx + "\n\n" + priorCtx
			ui.PrintVerbose("Continue", fmt.Sprintf("loaded context from run %s", lastRun.ID))
		}
	}

	// Re-query detection: if the user asked a very similar question in
	// the last 10 minutes, mark the prior lesson as RecoverableError
	// (implicit negative feedback) so the router doesn't follow it as
	// positive precedent. The new lesson records requery_of=<prior-run-id>.
	var requeryOfRunID string
	if priorLesson, _ := lessons.FindRecentSimilar(question, 10); priorLesson != nil {
		if !priorLesson.Lesson.RecoverableError {
			ui.PrintVerbose("Re-query", fmt.Sprintf("marked prior run %s as unsatisfactory (similarity %.2f)", priorLesson.Lesson.RunID, priorLesson.Score))
		}
		requeryOfRunID = priorLesson.Lesson.RunID
	}

	spinner := ui.NewSpinner()

	// ---------------------------------------------------------------
	// Prior-turn context: look up the most recent run in this cwd so
	// the parser can resolve referential follow-ups ("do the same
	// thing, but for issues") by inheriting scope. Ordinal (most-recent
	// -in-cwd), not similarity - anaphora have no content to match on.
	// 10-minute window is generous for natural follow-ups, tight enough
	// to avoid stale carry-over across unrelated sessions.
	// ---------------------------------------------------------------
	cwdForPrior, _ := os.Getwd()
	var priorTurn *engine.PriorTurn
	if prior, _ := runs.FindLatestInCwd(cwdForPrior, 10); prior != nil {
		priorTurn = &engine.PriorTurn{
			Question:  prior.Question,
			Entities:  prior.Entities,
			Action:    prior.Action,
			Timestamp: prior.StartedAt.UTC().Format(time.RFC3339),
		}
		ui.PrintVerbose("Prior turn", fmt.Sprintf("%s (%s ago)", prior.ID, time.Since(prior.StartedAt).Round(time.Second)))
	}

	// ---------------------------------------------------------------
	// Step 1: Parse (LLM Call #1)
	// ---------------------------------------------------------------
	parseStart := time.Now()
	spinner.Start("Parsing question...")
	intent, err := engine.Parse(ctx, client, question, soulCtx, priorTurn)
	parseDuration := time.Since(parseStart)
	spinner.Stop(fmt.Sprintf("%s Parsed", ui.SuccessIcon))
	if err != nil {
		return fmt.Errorf("parse step failed: %w", err)
	}

	// Ambiguous referential follow-up guard: if the question is a
	// "same thing" / "what about X" / "and for Y" style reference,
	// produced zero entities, and no prior turn was found, the pipeline
	// would otherwise run an unscoped query (the classic failure mode:
	// "do the same thing, but for issues" → query every repo). Refuse
	// early with an actionable message instead.
	if len(intent.RawEntities) == 0 && priorTurn == nil && engine.IsReferentialFollowUp(question) {
		return fmt.Errorf("this looks like a follow-up (%q), but I don't have prior-turn context in this cwd. Restate with explicit scope, e.g. `aida \"<question with specific repos/entities>\"`.", question)
	}

	// Deterministic post-parse normalizer: promote `#N` literals to
	// action=task / task_action=lookup so the task intercept fires for
	// "show me task #117" even when the parser drifted to action=lookup.
	engine.PromoteTaskIntent(intent)

	entities := strings.Join(intent.RawEntities, ", ")
	ui.PrintParsed(intent.Action, entities, intent.Timeframe)
	ui.PrintVerbose("Keywords", strings.Join(intent.Keywords, ", "))

	// ---------------------------------------------------------------
	// Task intercept: if the parser detected a task intent, handle it
	// directly from brain.db. No routing, execution, or synthesis needed.
	//
	// Respect prior feedback: if a similar past question was classified
	// as `task` but the user left negative/neutral feedback, skip the
	// intercept and let the full pipeline answer -- the feedback is a
	// signal that the task short-circuit was wrong for this shape of
	// question.
	//
	// Explicit --agent bypasses: the user asked for the agent loop, not
	// for a one-shot task short-circuit. Without this, agent runs whose
	// prompt happens to match a task verb pattern ("review X", "follow
	// up on Y") get short-circuited into HandleTaskIntent and never
	// reach the agent loop. Discovered via the meeting → tasks → drafts
	// pipeline: 6 of 9 auto-solve runs hit the fast path because the
	// extracted task titles read as task verbs.
	// ---------------------------------------------------------------
	if intent.Action == "task" && !agentMode {
		if !taskIntentOverriddenByFeedback(ctx, brn, question) {
			if HandleTaskIntent(intent, cfg, profileName) {
				return nil
			}
		}
	}

	// ---------------------------------------------------------------
	// Step 2: Classify (deterministic)
	// ---------------------------------------------------------------
	classified := engine.Classify(intent)
	ui.PrintVerbose("Strategy", string(classified.Strategy))

	// ---------------------------------------------------------------
	// Step 3: Resolve (deterministic)
	// ---------------------------------------------------------------
	resolved := engine.Resolve(classified)

	// ---------------------------------------------------------------
	// Agent mode intercept: if --agent flag or StrategyAgent, use the
	// agent loop instead of the deterministic pipeline.
	// ---------------------------------------------------------------
	if agentMode || classified.Strategy == engine.StrategyAgent {
		// Pass library load issues to the agent so it can surface
		// them as remediation context when relevant - sources that
		// would have helped answer the question but were rejected.
		var libIssues []library.LibraryIssue
		if libRegistry != nil {
			libIssues = libRegistry.Issues()
		}
		return runAgentMode(ctx, cfg, apiKey, model, question, soulCtx,
			srcs, brn, nil, intent, classified, resolved,
			profileName, totalStart, libIssues, libRegistry)
	}

	// ---------------------------------------------------------------
	// Route resolution: walk the library's routes.yaml files for the
	// current cwd + parsed entities and accumulate the active layer/
	// source/skill set. If no routes match, fall back to all available
	// library layers (the Step 2 behavior).
	// ---------------------------------------------------------------
	cwd, _ := os.Getwd()
	entityValues := append([]string(nil), intent.RawEntities...)
	for _, e := range classified.Entities {
		entityValues = append(entityValues, e.Raw)
	}
	routeResolution, routeErr := libRegistry.Resolve(cwd, entityValues)
	if routeErr != nil {
		ui.PrintVerbose("Library routes", "error: "+routeErr.Error())
		routeResolution = &library.Resolution{}
	}
	var libBundle *library.LayerBundle
	if len(routeResolution.Layers) > 0 {
		libBundle = libRegistry.MaterializeLayers(routeResolution.Layers)
		ui.PrintVerbose("Library route", fmt.Sprintf("%d rules matched", routeResolution.MatchedRules))
	} else {
		libBundle = libRegistry.MaterializeAllAvailable()
	}
	if names := libBundle.Names(); len(names) > 0 {
		ui.PrintVerbose("Library layers", strings.Join(names, ", "))
	}
	libContext := libBundle.String()

	// ---------------------------------------------------------------
	// Step 4: Plan (deterministic) -- candidate generation
	// ---------------------------------------------------------------
	routedSources := routeResolution.Sources
	if pinnedSource != "" {
		// --source pins execution to exactly one library source: bypass
		// route resolution (and, below, the LLM router) so the plan cannot
		// drift off it. restrictAndBoost narrows candidates to this one name.
		routedSources = []string{pinnedSource}
	}
	plan := engine.Plan(classified, resolved, srcs, profile, routedSources)
	ui.PrintProfile(profileName, len(srcs))

	// ---------------------------------------------------------------
	// Step 4.5: LLM-driven router (4th LLM call). Refines the
	// deterministic plan using past lessons + semantic judgment.
	// Also annotates the run log with the LLM's reasoning.
	// ---------------------------------------------------------------

	// Brain search: find similar past lessons (semantic) + entity pages + routing wisdom.
	var brainContext *brain.SearchContext
	pastLessons, _ := lessons.FindSimilar(question, 5)
	if brn != nil {
		brainEntities := append([]string(nil), intent.RawEntities...)
		sc, err := brn.Search(ctx, question, brainEntities, 5)
		if err != nil {
			ui.PrintVerbose("Brain search", "failed: "+err.Error())
		} else {
			brainContext = sc
			if len(sc.SimilarLessons) > 0 {
				ui.PrintVerbose("Brain lessons", fmt.Sprintf("%d semantically similar lessons found", len(sc.SimilarLessons)))
				// Merge brain's semantic lessons into pastLessons so
				// the executor and synthesizer see them. Brain lessons
				// with feedback are especially valuable - they carry
				// the rich feedback_reason data.
				pastLessons = mergeBrainLessons(pastLessons, sc.SimilarLessons)
			}
			if len(sc.EntityPages) > 0 {
				ui.PrintVerbose("Brain entities", fmt.Sprintf("%d entity pages loaded", len(sc.EntityPages)))
			}
			if sc.RoutingWisdom != "" {
				ui.PrintVerbose("Brain wisdom", "compiled routing wisdom loaded")
			}
		}
	}

	// Build brain context string for the router
	brainCtxStr := ""
	if brainContext != nil {
		brainCtxStr = brainContext.FormatContextForRouter()
	}

	// Augment soul context with brain context for the router
	routerSoulCtx := soulCtx
	if brainCtxStr != "" {
		routerSoulCtx = soulCtx + "\n\n" + brainCtxStr
	}

	candidatesForLLM := flattenCandidates(plan)
	// Load domain keywords from brain for semantic source injection
	var domainKW map[string][]string
	if brn != nil {
		domainKW = brain.LoadDomainKeywords(brn.Path)
	}
	// Pull recent failed eval-runs whose questions overlap with the
	// current entities so the router can apply per-source boosts
	// from the sensor side of the harness (Action #2 phase 4).
	var recentFailedReviews []eval.ReviewRecord
	if brn != nil && intent != nil && len(intent.RawEntities) > 0 {
		if revs, err := brain.RecentFailedReviewsForEntities(brn.Path, intent.RawEntities, 50); err == nil {
			recentFailedReviews = revs
			if len(revs) > 0 {
				ui.PrintVerbose("Eval signal", fmt.Sprintf("%d reviewer record(s) from recent failed runs feeding router", len(revs)))
			}
		} else {
			ui.PrintVerbose("Eval signal", "load error: "+err.Error())
		}
	}
	var llmRoute *engine.LLMRouteResult
	var refinedSources []engine.ScoredSource
	var llmRouteErr error
	if pinnedSource == "" {
		llmRoute, refinedSources, llmRouteErr = engine.LLMRoute(ctx, client, intent.EffectiveQuery(), intent, candidatesForLLM, srcs, pastLessons, routerSoulCtx, domainKW, recentFailedReviews)
		if llmRouteErr != nil {
			ui.PrintVerbose("LLM router", "fallback to deterministic: "+llmRouteErr.Error())
		}
		if llmRoute != nil {
			ui.PrintVerbose("LLM router", fmt.Sprintf("picked %v -- %s", llmRoute.Sources, llmRoute.Reason))
		}
		if len(refinedSources) > 0 && llmRouteErr == nil {
			// Replace plan phases with the LLM-refined source set. We
			// preserve the existing phase structure (parallel/serial,
			// dependency wiring) but swap the sources within each phase
			// where the LLM picked a clear winner.
			plan = rebuildPlanWithRefinedSources(plan, refinedSources)
		}
	} else {
		ui.PrintVerbose("Source pin", "--source "+pinnedSource+": skipping LLM router")
	}

	if ui.Explain {
		fmt.Println()
		fmt.Println(engine.FormatExplain(plan))
	}

	// emitFallbackAnswer prints a claude -p connector-fallback answer and records
	// a fallback-flavored run + lesson, so thumbs-up/down still targets it and
	// the router gains a "claude-fallback" precedent. Shared by the give-up site
	// (no native source) and the upfront connector route below.
	emitFallbackAnswer := func(fbAnswer, note string) {
		ui.PrintResult(fbAnswer)
		fbRun := buildRunRecord(question, cwd, profileName, intent, classified, resolved, routeResolution, libBundle, plan, &engine.ExecutionResult{}, fbAnswer, totalStart, time.Since(totalStart))
		fbRun.Errors = append(fbRun.Errors, note)
		if _, err := runs.Save(fbRun); err != nil {
			ui.PrintVerbose("Run log", "save error: "+err.Error())
		}
		fmt.Fprintf(os.Stderr, "[aida-run-id:%s]\n", fbRun.ID)
		if brn != nil {
			_ = brn.RecordLesson(ctx, &lessons.Lesson{
				Timestamp:     time.Now().UTC().Format(time.RFC3339),
				RunID:         fbRun.ID,
				Question:      strings.ToLower(strings.TrimSpace(question)),
				Cwd:           cwd,
				Action:        intent.Action,
				Strategy:      string(classified.Strategy),
				Sources:       []string{"claude-fallback"},
				AnswerSnippet: truncateRunSummary(fbAnswer, 200),
			})
		}
	}

	// ---------------------------------------------------------------
	// Step 4.5b: honest "nothing here can answer this" escape (Fix 3).
	// The LLM router looked at every candidate and concluded none could
	// plausibly answer the question -- rather than let a doomed
	// execute -> synthesize round trip hallucinate an answer (or, per
	// the original incident, let an arbitrary alphabetical-filler
	// source steamroll everything via the flat LLM-pick boost), tell
	// the user directly. Mirrors the totalPlanned == 0 give-up path
	// below: try the claude -p connector fallback first, and only if
	// that also comes up empty do we print the honest message and
	// record a RecoverableError lesson (so this never reads as positive
	// routing precedent for either source).
	// ---------------------------------------------------------------
	if llmRoute != nil && llmRoute.NoneViable {
		if fbAnswer, ok := maybeClaudeFallback(ctx, question, cfg); ok {
			emitFallbackAnswer(fbAnswer, fmt.Sprintf("answered via claude -p connector fallback (router: none_viable -- %s)", llmRoute.Reason))
			return nil
		}

		msg := formatNoneViableAnswer(llmRoute.Reason, candidatesForLLM)
		fmt.Printf("\n%s %s\n", ui.WarnIcon, msg)

		noneViableRun := buildRunRecord(question, cwd, profileName, intent, classified, resolved, routeResolution, libBundle, plan, &engine.ExecutionResult{}, msg, totalStart, time.Since(totalStart))
		noneViableRun.Errors = append(noneViableRun.Errors, "router reported none_viable: "+llmRoute.Reason)
		if _, err := runs.Save(noneViableRun); err != nil {
			ui.PrintVerbose("Run log", "save error: "+err.Error())
		}
		fmt.Fprintf(os.Stderr, "[aida-run-id:%s]\n", noneViableRun.ID)
		if brn != nil {
			_ = brn.RecordLesson(ctx, &lessons.Lesson{
				Timestamp:        time.Now().UTC().Format(time.RFC3339),
				RunID:            noneViableRun.ID,
				Question:         strings.ToLower(strings.TrimSpace(question)),
				Cwd:              cwd,
				Action:           intent.Action,
				Strategy:         string(classified.Strategy),
				AnswerSnippet:    truncateRunSummary(msg, 200),
				RecoverableError: true,
			})
		}
		return nil
	}

	// If the planner couldn't pick a single source, synthesis will just
	// hallucinate a "no data available" answer. Tell the user exactly
	// why and what to do about it, then exit without wasting an LLM call.
	totalPlanned := 0
	for _, ph := range plan.Phases {
		totalPlanned += len(ph.Sources)
	}
	if totalPlanned == 0 {
		// Before giving up, try the claude -p connector fallback: a
		// connector-shaped question (email/calendar/slack/...) has no native
		// source here, but claude can reach the user's OAuth connectors. On a
		// hit, show only the claude answer (clean for Jarvis voice) and record
		// a fallback-flavored run+lesson so thumbs-up/down still targets it.
		if fbAnswer, ok := maybeClaudeFallback(ctx, question, cfg); ok {
			emitFallbackAnswer(fbAnswer, "answered via claude -p connector fallback (no native source matched)")
			return nil
		}

		fmt.Printf("\n%s No sources matched this query.\n", ui.WarnIcon)
		fmt.Printf("  profile:      %s (%d sources configured)\n", profileName, len(srcs))
		fmt.Printf("  strategy:     %s\n", classified.Strategy)
		if len(routeResolution.Sources) > 0 {
			fmt.Printf("  routes want:  %s (but those sources aren't configured)\n", strings.Join(routeResolution.Sources, ", "))
		}
		fmt.Println()
		fmt.Println("Fixes:")
		if len(srcs) == 0 {
			fmt.Println("  1. You have zero configured sources. Add one via the library:")
			fmt.Println("     - edit ~/.aida/library/sources/<name>.yaml")
			fmt.Println("     - register it in ~/.aida/library/library.yaml under `sources:`")
			fmt.Println("     - (optional) add a route in ~/.aida/library/routes.yaml")
			fmt.Println("  2. If you have a legacy ~/.aida/sources.yaml somewhere, run `aida library import`.")
		} else {
			fmt.Println("  - Add a matching capability or entity to an existing source, or")
			fmt.Println("  - Add a route in ~/.aida/library/routes.yaml that activates it for this cwd.")
		}
		fmt.Println()
		fmt.Println("Run `aida library list` to see what's currently registered.")

		// Still write a run log so `aida tune` can explain why routing
		// failed for THIS query (rather than showing the previous
		// successful run). Empty phases + errors list document the
		// short-circuit, and the recorded entities/keywords make it
		// obvious what the intent parse extracted.
		failRun := buildRunRecord(question, cwd, profileName, intent, classified, resolved, routeResolution, nil, plan, &engine.ExecutionResult{}, "(no sources matched -- routing short-circuited)", totalStart, time.Since(totalStart))
		failRun.Errors = append(failRun.Errors, "no sources matched the planned query (0 phases)")
		if _, err := runs.Save(failRun); err != nil {
			ui.PrintVerbose("Run log", "save error: "+err.Error())
		}
		// Record a lesson so future routing knows this question found no source.
		failLesson := &lessons.Lesson{
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
			RunID:         failRun.ID,
			Question:      strings.ToLower(strings.TrimSpace(question)),
			Cwd:           cwd,
			Action:        intent.Action,
			Strategy:      string(classified.Strategy),
			AnswerSnippet: "(no sources matched)",
		}
		if brn != nil {
			_ = brn.RecordLesson(ctx, failLesson)
		}
		return nil
	}

	// Display routing information
	var scoredDisplay []ui.ScoredSourceDisplay
	for _, phase := range plan.Phases {
		for _, ss := range phase.Sources {
			scoredDisplay = append(scoredDisplay, ui.ScoredSourceDisplay{
				Name:  ss.Name,
				Score: ss.Score,
			})
		}
	}
	if len(scoredDisplay) > 0 {
		ui.PrintRouting(scoredDisplay)
	}

	// ---------------------------------------------------------------
	// Dry-run: show plan and exit
	// ---------------------------------------------------------------
	if dryRun {
		planDisplay := buildPlanDisplay(plan)
		ui.PrintDryRun(planDisplay)
		return nil
	}

	// Phase 2 - upfront connector routing: when the router could only land on
	// the web-search catch-all, aida has no source that specializes in this query.
	// For a connector-shaped question (email/calendar/slack/...), skip the
	// doomed execute → synthesize → re-execute round-trip and answer straight
	// from the claude -p fallback. A real source (e.g. butter-stack for attio)
	// is not weak, so it takes the normal path; maybeClaudeFallback's connector
	// gate lets a plain web query fall through to web-search unchanged.
	if onlyWeakSources(collectPlanSourceNames(plan)) {
		if fbAnswer, ok := maybeClaudeFallback(ctx, question, cfg); ok {
			emitFallbackAnswer(fbAnswer, "answered via claude -p connector fallback (upfront: only web-search matched)")
			return nil
		}
	}

	// ---------------------------------------------------------------
	// Step 5: Execute (LLM Call #2 + parallel shell exec)
	// ---------------------------------------------------------------
	execStart := time.Now()
	execSources := collectPlanSourceNames(plan)
	execMsg := fmt.Sprintf("Querying %s...", strings.Join(execSources, ", "))
	if classified.Strategy == engine.StrategyRecord {
		for _, s := range execSources {
			if s == "workouts" {
				execMsg = "Logging your food, this'll take a second..."
				break
			}
		}
	}
	spinner.Start(execMsg)
	// Known-repo hint built once per query - any gh-based source picked by
	// the planner gets a lookup table of entity → owner/repo so the
	// query-construction LLM can use --repo correctly instead of guessing
	// --owner <current user>. When the current query includes glob
	// entities ("aida*"), append an explicit expansion block listing
	// every matching owner/repo so the LLM fans --repo across them all
	// instead of silently dropping to the closest single match.
	repoHint := config.BuildKnownGitHubReposHint(srcs)
	if globHint := config.BuildGlobExpansionHint(intent.RawEntities, srcs); globHint != "" {
		repoHint += globHint
		ui.PrintVerbose("Glob expansion", fmt.Sprintf("expanded entities %v against %d known-repo sources", intent.RawEntities, countReposInSources(srcs)))
	}
	allowedGHRepos := config.BuildGHRepoAllowlist(srcs)
	execResult, err := engine.Execute(ctx, client, plan, intent, resolved, libContext, repoHint, allowedGHRepos, pastLessons)
	execDuration := time.Since(execStart)
	spinner.Stop(fmt.Sprintf("%s Executed", ui.SuccessIcon))
	if err != nil {
		return fmt.Errorf("execute step failed: %w", err)
	}

	ui.PrintVerbose("Results", fmt.Sprintf("%d source results", len(execResult.AllResults)))

	// Inject brain feedback as synthetic source results when brain has
	// rich data from prior user feedback that is highly relevant.
	brainResults := buildBrainSourceResults(pastLessons)
	if len(brainResults) > 0 {
		execResult.AllResults = append(execResult.AllResults, brainResults...)
		ui.PrintVerbose("Brain inject", fmt.Sprintf("%d brain feedback result(s) injected as source data", len(brainResults)))
	}

	// Recall captured Claude Code memories, gated on the question reading
	// as memory-shaped ("what do you know about X") so this doesn't pay an
	// embedding+search cost on every unrelated query.
	if brn != nil && brain.LooksMemoryShaped(question) {
		if res, mErr := brn.RecallMemories(ctx, question, 3, brain.ClaudeMemoryProfile, "", "", false); mErr != nil {
			ui.PrintVerbose("Memory recall", "failed: "+mErr.Error())
		} else if memoryResults := buildMemorySourceResults(res); len(memoryResults) > 0 {
			execResult.AllResults = append(execResult.AllResults, memoryResults...)
			ui.PrintVerbose("Memory recall", fmt.Sprintf("%d claude memory result(s) injected", len(memoryResults)))
		}
	}

	// ---------------------------------------------------------------
	// Step 5.5: Verify (confidence scoring on results)
	// ---------------------------------------------------------------
	var verifiedResults []engine.VerifiedResult
	if !isOffline {
		verifiedResults = engine.Verify(ctx, client, question, intent, execResult.AllResults)
		// Filter out very low-confidence results from synthesis
		var filteredResults []sources.SourceResult
		for _, vr := range verifiedResults {
			if vr.Confidence >= 0.2 {
				filteredResults = append(filteredResults, vr.Result)
			} else {
				ui.PrintVerbose("Verify drop", fmt.Sprintf("%s: confidence %.2f too low, excluded from synthesis", vr.Result.Source, vr.Confidence))
			}
		}
		if len(filteredResults) > 0 {
			execResult.AllResults = filteredResults
		}
		// If all results were filtered, keep originals so synthesizer can report "no useful data"
	}

	// ---------------------------------------------------------------
	// Step 6: Synthesize (LLM Call #3) with optional validation loop
	// ---------------------------------------------------------------
	synthStart := time.Now()
	spinner.Start("Synthesizing answer...")

	// Build re-execution callback for the validation loop. When the
	// first synthesis scores quality <= 2, this re-runs execution with
	// additional guidance about what's missing.
	var reExecuteFn engine.ReExecuteFn
	if !isOffline {
		reExecuteFn = func(ctx context.Context, guidance string) ([]sources.SourceResult, error) {
			guidedIntent := &engine.Intent{
				RawQuery:    question + "\n\nAdditional guidance: " + guidance,
				RawEntities: intent.RawEntities,
				Timeframe:   intent.Timeframe,
				Action:      intent.Action,
				Keywords:    intent.Keywords,
			}
			reExecResult, err := engine.Execute(ctx, client, plan, guidedIntent, resolved, libContext, repoHint, allowedGHRepos, pastLessons)
			if err != nil {
				return nil, err
			}
			return reExecResult.AllResults, nil
		}
	}

	answer, qualityResult, reviewRecords, err := engine.SynthesizeWithValidation(
		ctx, client, intent.EffectiveQuery(), soulCtx, libContext, execResult.AllResults, pastLessons,
		reExecuteFn, 0.10, // $0.10 cost budget for re-execution
	)
	synthDuration := time.Since(synthStart)
	spinner.Stop(fmt.Sprintf("%s Synthesized", ui.SuccessIcon))
	if err != nil {
		return fmt.Errorf("synthesis step failed: %w", err)
	}

	totalDuration := time.Since(totalStart)

	// If the pipeline synthesized a refusal ("I don't have access to ...") for a
	// connector-shaped question, replace it with the claude -p connector
	// fallback before printing. Overwriting `answer` here means the run/lesson
	// recording below stores what the user actually saw, and the run-id sentinel
	// fires once. Guarding the PrintResult is mandatory - otherwise the caller
	// (Jarvis) gets the useless answer AND the claude answer concatenated.
	overrode := false
	if answerIsUseless(answer) {
		if fbAnswer, ok := maybeClaudeFallback(ctx, question, cfg); ok {
			answer = fbAnswer
			ui.PrintResult(answer)
			ui.PrintVerbose("claude-fallback", "replaced useless answer with claude -p result")
			overrode = true
		}
	}
	if !overrode {
		ui.PrintResult(answer)
	}
	ui.PrintTiming(parseDuration, execDuration, synthDuration, totalDuration, execResult.AllResults)

	// Show LLM cost breakdown if any tokens were tracked.
	if costSummary := client.CostSummary(); costSummary != "" {
		ui.PrintVerbose("Cost", costSummary)
	}

	// Record this run to ~/.aida/runs/ for replay/tune workflow.
	run := buildRunRecord(question, cwd, profileName, intent, classified, resolved, routeResolution, libBundle, plan, execResult, answer, totalStart, totalDuration)

	// Attach tracing data from cost tracker
	run.Tracing = buildTracingData(client, parseDuration, execDuration, synthDuration)

	if path, err := runs.Save(run); err != nil {
		ui.PrintVerbose("Run log", "save error: "+err.Error())
	} else {
		ui.PrintVerbose("Run log", path)
	}

	// Emit a machine-parseable run-id sentinel on stderr. The Jarvis
	// aida_query tool greps this out so voice thumbs-down/up can
	// fire `aida thumbs-* <run-id>` against the exact run, eliminating
	// the race with concurrent terminal `aida` invocations. Terminal
	// users see one extra trailing stderr line; harmless.
	fmt.Fprintf(os.Stderr, "[aida-run-id:%s]\n", run.ID)

	// Persist reviewer records to the brain's eval-runs/ directory.
	// Source of truth lives in the brain git repo so eval signal
	// syncs across machines via the existing brain auto-commit/pull;
	// brain.db (when an indexed view is added later) will be rebuilt
	// from these files. Skipped when there are no records, when the
	// brain is unavailable, or when persistence fails - broken eval
	// storage must not break the existing run-save path.
	var evalRunForTrajectory *brain.EvalRun
	if brn != nil && len(reviewRecords) > 0 {
		evalRun := &brain.EvalRun{
			RunID:            run.ID,
			Question:         question,
			Model:            client.Model(),
			Profile:          profileName,
			AggregateVerdict: string(eval.AggregateVerdict(reviewRecords)),
			Reviewers:        reviewRecords,
		}
		if err := brain.WriteEvalRun(brn.Path, evalRun); err != nil {
			ui.PrintVerbose("Eval run", "save error: "+err.Error())
		} else {
			ui.PrintVerbose("Eval run", fmt.Sprintf("%s (%d reviewers, aggregate=%s)",
				run.ID, len(reviewRecords), evalRun.AggregateVerdict))
		}
		evalRunForTrajectory = evalRun
	}

	// Export the run as a tool-calling trajectory under
	// brain/trajectories/. Compatible with Atropos-style RL
	// training pipelines (Nous Research) - an opt-in fine-tune
	// of a smaller parser/router/synth model can be built from
	// these files without a schema migration. Best-effort:
	// trajectory failure must not regress the existing pipeline.
	if brn != nil {
		traj := brain.BuildTrajectory(run, evalRunForTrajectory, client.Model())
		if traj != nil {
			if err := brain.WriteTrajectory(brn.Path, traj); err != nil {
				ui.PrintVerbose("Trajectory", "save error: "+err.Error())
			} else {
				ui.PrintVerbose("Trajectory", fmt.Sprintf("%s (%d messages)", run.ID, len(traj.Messages)))
			}
		}
	}

	// Auto-record a Lesson so the next routing decision benefits from
	// this outcome. Silent learning -- runs the same on success, error,
	// or empty. Explicit thumbs-up / thumbs-down append additional
	// lessons later via `aida thumbs-up <run-id>`.
	lesson := buildLessonFromRun(run, execResult)
	lesson.RequeryOf = requeryOfRunID

	// Store routing confidence from LLM router
	if llmRoute != nil && llmRoute.Confidence != nil {
		lesson.RoutingConfidence = llmRoute.Confidence
	}

	// Store result confidence from verifier
	if len(verifiedResults) > 0 {
		lesson.ResultConfidence = make(map[string]float64)
		for _, vr := range verifiedResults {
			lesson.ResultConfidence[vr.Result.Source] = vr.Confidence
		}
	}

	// PRM-lite: apply the quality score from the synthesis validation
	// loop if available. Otherwise fall back to an explicit scoring
	// call. The score is stored on the lesson and used by the router's
	// past-learning section to annotate precedent strength.
	if qualityResult != nil {
		lesson.Quality = qualityResult.Quality
		lesson.HasData = qualityResult.HasData
		lesson.QualityReason = qualityResult.Reason
		if qualityResult.Quality <= 2 {
			lesson.RecoverableError = true
		}
		ui.PrintVerbose("Quality scorer", fmt.Sprintf("%d/5 - %s (from validation loop)", qualityResult.Quality, qualityResult.Reason))
	} else if !isOffline && answer != "" {
		scoreAnswer(ctx, client, question, answer, lesson)
	}

	// Generate routing hint for high-quality results
	if shouldWriteRoutingHint(lesson.Quality, lesson.Sources, reviewRecords) {
		lesson.RoutingHint = lessons.BuildRoutingHint(
			lesson.Sources, intent.Action, string(classified.Strategy), intent.Keywords,
		)
		ui.PrintVerbose("Routing hint", lesson.RoutingHint)
	} else if lesson.Quality >= 4 && len(lesson.Sources) > 0 {
		ui.PrintVerbose("Routing hint", fmt.Sprintf("skipped - reviewers failed (%s)", eval.AggregateVerdict(reviewRecords)))
	}

	// Write to brain (JSON file + brain.db + entity evidence).
	if brn != nil {
		if err := brn.RecordLesson(ctx, lesson); err != nil {
			ui.PrintVerbose("Brain write", "error: "+err.Error())
		} else {
			ui.PrintVerbose("Brain write", fmt.Sprintf("recorded (%d sources, quality %d/5)", len(lesson.Sources), lesson.Quality))
		}

		// Append evidence to entity pages for sources used
		for _, src := range lesson.Sources {
			status := lesson.PerSourceStatus[src]
			evidence := fmt.Sprintf("%s → %s [quality %d/5, %d artifacts]",
				truncateRunSummary(question, 60), status, lesson.Quality, lesson.ArtifactCount)
			_ = brn.AppendEvidence("tools", src, evidence)
		}

		// Auto-compile: check if enough lessons have accumulated since
		// the last compile and trigger a non-blocking compile in a goroutine.
		if brn.ShouldAutoCompile() && !isOffline {
			ui.PrintVerbose("Brain auto-compile", "triggering background compile")
			go func() {
				compileClient := llm.NewClient(apiKey, model, false)
				if cfg.Model.Stages != nil {
					compileClient.SetStageModels(cfg.Model.Stages)
				}
				compileCtx := context.Background()
				if err := brn.CompileRoutingWisdom(compileCtx, compileClient); err != nil {
					ui.PrintVerbose("Brain auto-compile", "error: "+err.Error())
				} else {
					_ = brn.MarkCompiled()
					ui.PrintVerbose("Brain auto-compile", "completed")
				}
			}()
		}

		// Auto-consolidate: check if enough typed-memory events have
		// accumulated since the last consolidation and trigger a
		// non-blocking consolidate in a goroutine - mirrors the
		// auto-compile trigger above but promotes events into
		// facts/instructions instead of compiling routing wisdom.
		if brn.ShouldAutoConsolidate() && !isOffline {
			ui.PrintVerbose("Brain auto-consolidate", "triggering background consolidate")
			go func() {
				consolidateClient := llm.NewClient(apiKey, model, false)
				if cfg.Model.Stages != nil {
					consolidateClient.SetStageModels(cfg.Model.Stages)
				}
				consolidateCtx := context.Background()
				if _, err := brn.Consolidate(consolidateCtx, consolidateClient, false); err != nil {
					ui.PrintVerbose("Brain auto-consolidate", "error: "+err.Error())
				} else if err := brn.MarkConsolidated(); err != nil {
					ui.PrintVerbose("Brain auto-consolidate", "mark error: "+err.Error())
				} else {
					ui.PrintVerbose("Brain auto-consolidate", "completed")
				}
			}()
		}

		// Background git commit + push (fire-and-forget)
		if cfg.Brain.AutoSync {
			brain.CommitAndPush(cfg.BrainPath(), profileName)
		}
	}

	// Proactive suggestion: if similar question asked 3+ times in 7 days, hint.
	if !isOffline && len(pastLessons) >= 3 {
		recentCount := 0
		weekAgo := time.Now().AddDate(0, 0, -7).Format(time.RFC3339)
		for _, ls := range pastLessons {
			if ls.Score > 0.7 && ls.Lesson.Timestamp > weekAgo {
				recentCount++
			}
		}
		if recentCount >= 3 {
			fmt.Printf("\n%s You've asked similar questions %d times this week. Try `aida session` for follow-ups or `aida --agent` for multi-step tasks.\n",
				ui.DimStyle.Render("tip:"), recentCount)
		}
	}

	return nil
}

// countReposInSources counts sources whose Repo field is non-empty -
// used for a verbose breadcrumb when glob expansion finds no matches.
func countReposInSources(srcs config.Sources) int {
	n := 0
	for _, s := range srcs {
		if s != nil && s.Repo != "" {
			n++
		}
	}
	return n
}

// buildBrainSourceResults converts past lessons with rich feedback data
// into synthetic SourceResult entries so the synthesizer can cite them
// as first-class source data (not just hints).
func buildBrainSourceResults(pastLessons []lessons.SimilarLesson) []sources.SourceResult {
	var results []sources.SourceResult
	for _, ls := range pastLessons {
		l := ls.Lesson
		if l.FeedbackReason == "" {
			continue
		}
		// Only inject notes and thumbs-up (confirmed data). Thumbs-down
		// feedback is correction metadata, not answer data.
		if l.Feedback != lessons.FeedbackNote && l.Feedback != lessons.FeedbackThumbsUp {
			continue
		}
		// Require minimum similarity to avoid injecting irrelevant lessons.
		if ls.Score < 0.5 {
			continue
		}

		results = append(results, sources.SourceResult{
			Source:  "brain",
			Status:  "success",
			Summary: l.FeedbackReason,
			Artifacts: []sources.Artifact{{
				Type:    "brain-feedback",
				ID:      "feedback-" + l.Timestamp,
				Snippet: l.FeedbackReason,
			}},
		})
	}
	return results
}

// buildMemorySourceResults converts recalled Claude Code memories into
// synthetic SourceResult entries so the synthesizer can cite them as
// first-class source data, the same way buildBrainSourceResults does for
// lesson feedback.
func buildMemorySourceResults(res brain.RecallResult) []sources.SourceResult {
	var results []sources.SourceResult
	for _, m := range res.Memories {
		body := strings.TrimSpace(m.Record.Body)
		if body == "" {
			continue
		}
		if len(body) > 500 {
			body = body[:500] + "..."
		}
		results = append(results, sources.SourceResult{
			Source:  "claude-memory",
			Status:  "success",
			Summary: body,
			Artifacts: []sources.Artifact{{
				Type:    "claude-memory",
				ID:      m.Record.Key,
				Snippet: body,
			}},
		})
	}
	return results
}

// mergeBrainLessons converts brain.SimilarLesson (vector-based) into
// lessons.SimilarLesson (the type the pipeline uses) and deduplicates
// against existing Jaccard-based results. Brain lessons with feedback
// are prioritized since they carry rich correction data.
//
// Lessons that carry feedback content (FeedbackReason or
// FeedbackOutputDirectives) are NEVER deduped away - repeated identical
// questions are common (the user reruns a query, then leaves a `--because`
// note on one of them) and the feedback-bearing variant is exactly the
// one the synthesizer needs. Without this guard, the most-recent
// no-feedback lesson wins the question-dedup race and silently drops the
// note's output directives. (Verified by the Part D verification run
// where a `3 sentences max` directive never reached the synthesizer.)
func mergeBrainLessons(existing []lessons.SimilarLesson, brainLessons []brain.SimilarLesson) []lessons.SimilarLesson {
	// Index existing by question text to deduplicate. Lessons with
	// feedback content are exempt and tracked separately.
	seen := make(map[string]bool)
	for _, ls := range existing {
		if !lessonCarriesFeedback(&ls.Lesson) {
			seen[ls.Lesson.Question] = true
		}
	}

	for _, bl := range brainLessons {
		lr := bl.Lesson
		hasFeedback := lr.FeedbackReason != "" || len(lr.FeedbackOutputDirectives) > 0
		if seen[lr.Question] && !hasFeedback {
			continue
		}
		if !hasFeedback {
			seen[lr.Question] = true
		}

		// Convert brain.LessonRecord → lessons.Lesson
		converted := lessons.Lesson{
			Timestamp:                lr.Timestamp,
			Question:                 lr.Question,
			Action:                   lr.Action,
			Strategy:                 lr.Strategy,
			Sources:                  lr.Sources,
			ArtifactCount:            lr.ArtifactCount,
			Quality:                  lr.Quality,
			QualityReason:            lr.QualityReason,
			Feedback:                 lessons.Feedback(lr.Feedback),
			FeedbackReason:           lr.FeedbackReason,
			FeedbackIntendedSource:   lr.FeedbackIntendedSource,
			FeedbackIntendedSources:  lr.FeedbackIntendedSources,
			FeedbackExcludedSources:  lr.FeedbackExcludedSources,
			FeedbackFailureType:      lr.FeedbackFailureType,
			FeedbackOutputDirectives: lr.FeedbackOutputDirectives,
			AnswerSnippet:            lr.AnswerSnippet,
			ExecutedQueries:          lr.ExecutedQueries,
		}
		// Convert per-source status
		if lr.PerSourceStatus != nil {
			converted.PerSourceStatus = make(map[string]lessons.Status)
			for k, v := range lr.PerSourceStatus {
				converted.PerSourceStatus[k] = lessons.Status(v)
			}
		}

		existing = append(existing, lessons.SimilarLesson{
			Lesson: converted,
			Score:  bl.Similarity,
		})
	}

	return existing
}

// lessonCarriesFeedback reports whether a lesson has actionable feedback
// content (a `--because` reason or extracted output directives) that the
// synthesizer / router should never silently dedup away.
func lessonCarriesFeedback(l *lessons.Lesson) bool {
	return l.FeedbackReason != "" || len(l.FeedbackOutputDirectives) > 0
}

// buildLessonFromRun translates a runs.Run into a lessons.Lesson. The
// lesson keeps just the fields the planner / LLM router care about for
// future routing decisions: question text, picked sources, per-source
// status, total artifact count, and a truncated answer snippet.
//
// Issue #14 Bug D: a lesson is marked RecoverableError when EITHER
//  1. every executed source returned StatusError (adapter failure), OR
//  2. every executed source returned status=success but the synthesized
//     answer is an "I don't know" (no actionable content).
//
// Both cases mean the routing was correct in spirit (or at least not
// proven wrong) and the source should not be poisoned for similar
// future questions. Without case (2), a routing mistake that produces
// a polite "I cannot find this" answer gets recorded as a "successful"
// past lesson and the LLM router sees it as positive precedent. That
// creates a feedback loop where the same wrong source keeps winning
// the same kind of question.
// shouldWriteRoutingHint decides whether a run's outcome is trustworthy
// enough to write an auto-generated routing_hint lesson. Fix 4: the real
// incident was a wrong-but-confident answer that the PRM-lite quality
// scorer rated 4/5 (scorers judge fluency/completeness, not factual
// correctness) while the deterministic citation reviewer had already
// failed the same run. The quality>=4 gate alone let that reinforcing
// hint through; requiring the reviewers to not have failed closes it.
//
// When records is empty (no reviewers ran - offline mode, or a source mix
// the reviewer set doesn't cover) the reviewer signal is unavailable, so
// the gate falls back to quality alone rather than blocking every hint.
func shouldWriteRoutingHint(quality int, sources []string, records []eval.ReviewRecord) bool {
	if quality < 4 || len(sources) == 0 {
		return false
	}
	if len(records) == 0 {
		return true
	}
	return eval.AggregateVerdict(records) != eval.VerdictFail
}

func buildLessonFromRun(run *runs.Run, execResult *engine.ExecutionResult) *lessons.Lesson {
	perSource := make(map[string]lessons.Status)
	executedQueries := make(map[string]string)
	var srcNames []string
	totalArtifacts := 0
	allError := true
	allSuccessOrEmpty := true
	anyResult := false
	for _, pr := range execResult.PhaseResults {
		for _, sr := range pr.Results {
			anyResult = true
			srcNames = append(srcNames, sr.Source)
			perSource[sr.Source] = lessons.Status(sr.Status)
			totalArtifacts += len(sr.Artifacts)
			if sr.Status == "success" && len(sr.Artifacts) > 0 && sr.Command != "" {
				executedQueries[sr.Source] = sr.Command
			}
			st := lessons.Status(sr.Status)
			if st != lessons.StatusError {
				allError = false
			}
			if st != lessons.StatusSuccess && st != lessons.StatusEmpty {
				allSuccessOrEmpty = false
			}
		}
	}
	snippet := run.Answer
	if len(snippet) > 500 {
		snippet = snippet[:500] + "..."
	}
	recoverable := false
	if anyResult && allError {
		recoverable = true
	}
	if anyResult && allSuccessOrEmpty && answerIsUseless(run.Answer) {
		recoverable = true
	}
	// Only store executed queries if any were captured.
	var eqField map[string]string
	if len(executedQueries) > 0 {
		eqField = executedQueries
	}

	return &lessons.Lesson{
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		RunID:            run.ID,
		Question:         strings.ToLower(strings.TrimSpace(run.Question)),
		Cwd:              run.Cwd,
		Action:           run.Action,
		Strategy:         run.Strategy,
		Sources:          srcNames,
		PerSourceStatus:  perSource,
		ArtifactCount:    totalArtifacts,
		AnswerSnippet:    snippet,
		RecoverableError: recoverable,
		ExecutedQueries:  eqField,
	}
}

// scoreAnswer runs the PRM-lite quality scorer against the synthesized
// answer and updates the lesson's Quality/HasData/QualityReason fields.
// If the LLM call fails (network, timeout, malformed JSON), the lesson
// is left at quality=0 and the error is logged verbosely - the system
// degrades gracefully to the old answerIsUseless() heuristic.
func scoreAnswer(ctx context.Context, client *llm.Client, question, answer string, lesson *lessons.Lesson) {
	raw, err := client.CompleteJSONWithStage(
		ctx,
		"quality",
		llm.QualityScoreSystemPrompt,
		llm.QualityScoreUserPrompt(question, answer),
		llm.QualityScoreSchema(),
	)
	if err != nil {
		ui.PrintVerbose("Quality scorer", "failed: "+err.Error())
		return
	}
	var result struct {
		Quality int    `json:"quality"`
		HasData bool   `json:"has_data"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		ui.PrintVerbose("Quality scorer", "parse error: "+err.Error())
		return
	}
	lesson.Quality = result.Quality
	lesson.HasData = result.HasData
	lesson.QualityReason = result.Reason
	// Override RecoverableError based on quality score: quality <= 2
	// means the answer was useless regardless of adapter status.
	if result.Quality <= 2 {
		lesson.RecoverableError = true
	}
	ui.PrintVerbose("Quality scorer", fmt.Sprintf("%d/5 - %s", result.Quality, result.Reason))
}

// answerIsUseless detects synthesized answers that are essentially "I
// don't know" - meaning the source ran fine but had nothing to say.
// Used by buildLessonFromRun so these answers don't poison future
// routing for the same source on the same kind of question.
//
// The heuristic is intentionally conservative: it looks for an explicit
// disclaimer in the FIRST 300 characters of the answer (where every
// "I cannot find this" answer starts), and only fires when no
// information-bearing tokens follow. False positives just mean a
// borderline lesson loses its "positive precedent" weight, which is
// the safer failure mode.
func answerIsUseless(answer string) bool {
	if answer == "" {
		return true
	}
	head := strings.ToLower(answer)
	if len(head) > 300 {
		head = head[:300]
	}
	for _, marker := range refusal.RefusalMarkers {
		if strings.Contains(head, marker) {
			return true
		}
	}
	return false
}

// buildRunRecord assembles a runs.Run from everything that happened in this
// invocation. Kept verbose-but-flat so the JSON file is easy to skim and to
// pass into a Claude Code tuning session.
func buildRunRecord(
	question, cwd, profileName string,
	intent *engine.Intent,
	classified *engine.ClassifiedIntent,
	resolved *engine.ResolvedContext,
	routeRes *library.Resolution,
	libBundle *library.LayerBundle,
	plan *engine.ExecutionPlan,
	execResult *engine.ExecutionResult,
	answer string,
	startedAt time.Time,
	total time.Duration,
) *runs.Run {
	r := &runs.Run{
		ID:        runs.NewID(startedAt, question),
		StartedAt: startedAt,
		TotalMs:   total.Milliseconds(),
		Question:  question,
		Cwd:       cwd,
		Profile:   profileName,
		Action:    intent.Action,
		Strategy:  string(classified.Strategy),
		Entities:  intent.RawEntities,
		Answer:    answer,
	}
	if routeRes != nil {
		r.LibrarySources = routeRes.Sources
		r.RouteMatches = routeRes.MatchedRules
	}
	if libBundle != nil {
		r.LibraryLayers = libBundle.Names()
	}

	// Build a name->score map from the plan so per-source rows include score.
	scores := map[string]int{}
	for _, p := range plan.Phases {
		for _, ss := range p.Sources {
			scores[ss.Name] = ss.Score
		}
	}

	for _, pr := range execResult.PhaseResults {
		ph := runs.PhaseRun{Name: pr.Phase}
		for _, sr := range pr.Results {
			ph.Sources = append(ph.Sources, runs.SourceRun{
				Name:          sr.Source,
				Score:         scores[sr.Source],
				Command:       truncateRunSummary(sr.Command, 500),
				Status:        sr.Status,
				Summary:       truncateRunSummary(sr.Summary, 1500),
				ArtifactCount: len(sr.Artifacts),
				DurationMs:    sr.Duration.Milliseconds(),
			})
			if sr.Status == "error" {
				r.Errors = append(r.Errors, fmt.Sprintf("%s: %s", sr.Source, sr.Summary))
			}
		}
		r.Phases = append(r.Phases, ph)
	}
	return r
}

func truncateRunSummary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// collectPlanSourceNames returns the unique source names from a plan for display.
func collectPlanSourceNames(plan *engine.ExecutionPlan) []string {
	seen := map[string]bool{}
	var names []string
	for _, ph := range plan.Phases {
		for _, s := range ph.Sources {
			if !seen[s.Name] {
				seen[s.Name] = true
				names = append(names, s.Name)
			}
		}
	}
	return names
}

// flattenCandidates returns the union of every phase's source list,
// deduped by name. Used as the candidate set fed to the LLM router.
// flattenCandidates builds the candidate list that LLMRoute scores over. It
// returns: (1) every source the deterministic planner picked across all
// phases, then (2) the top non-zero sources from the full ranking that
// weren't already picked. (2) is the Phase 2 fix - previously the LLM
// router only saw phase-picked sources (often narrowed to 1-2 by route
// matching), which kept high-scoring monorepo/partner candidates invisible.
// Cap at llmRouterMaxCandidates so prompt size stays bounded.
const llmRouterMaxCandidates = 12

func flattenCandidates(plan *engine.ExecutionPlan) []engine.ScoredSource {
	if plan == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []engine.ScoredSource
	for _, ph := range plan.Phases {
		for _, s := range ph.Sources {
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			out = append(out, s)
		}
	}
	for _, s := range plan.AllCandidates {
		if s.Score == 0 || seen[s.Name] || len(out) >= llmRouterMaxCandidates {
			continue
		}
		seen[s.Name] = true
		out = append(out, s)
	}
	return out
}

// formatNoneViableAnswer builds the honest "nothing here can answer
// this" message for the router's none_viable escape (Fix 3). Extracted
// as a pure function -- no ctx, no LLM client -- so it's unit-testable
// without wiring up a full runQuery invocation.
func formatNoneViableAnswer(reason string, candidates []engine.ScoredSource) string {
	msg := "No configured source can answer this question."
	if reason != "" {
		msg += fmt.Sprintf(" Router: %s.", reason)
	}
	if names := topCandidateNames(candidates, 3); len(names) > 0 {
		msg += fmt.Sprintf(" Closest candidates: %s.", strings.Join(names, ", "))
	}
	return msg
}

// topCandidateNames returns up to n candidate names ordered by score
// descending. Does not mutate the input slice.
func topCandidateNames(candidates []engine.ScoredSource, n int) []string {
	if len(candidates) == 0 {
		return nil
	}
	sorted := make([]engine.ScoredSource, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	names := make([]string, 0, len(sorted))
	for _, c := range sorted {
		names = append(names, c.Name)
	}
	return names
}

// rebuildPlanWithRefinedSources replaces the plan's source list with the
// LLM-refined picks while preserving the phase structure. For single-
// phase plans (the common case) this is just "use the new source list."
// For multi-phase plans (investigate strategy with diagnose -> contextualize)
// the picks are kept in the FIRST phase and downstream phases are dropped
// because the LLM is trusted to pick the relevant subset.
func rebuildPlanWithRefinedSources(plan *engine.ExecutionPlan, picks []engine.ScoredSource) *engine.ExecutionPlan {
	if plan == nil || len(picks) == 0 {
		return plan
	}
	// Preserve AllCandidates across rebuilds so --explain still has the
	// deterministic scoring table to render after the LLM router runs.
	all := plan.AllCandidates
	// Single-phase: replace sources in place.
	if len(plan.Phases) <= 1 {
		first := engine.Phase{
			Name:     "query",
			Parallel: len(picks) > 1,
			Sources:  picks,
		}
		if len(plan.Phases) == 1 {
			first.Name = plan.Phases[0].Name
			first.Parallel = plan.Phases[0].Parallel
		}
		return &engine.ExecutionPlan{Phases: []engine.Phase{first}, AllCandidates: all}
	}
	// Multi-phase: collapse to a single phase with the picks. Multi-hop
	// chained execution is rare and the LLM router doesn't yet model
	// dependencies; safer to flatten than to spread picks across phases.
	return &engine.ExecutionPlan{Phases: []engine.Phase{{
		Name:     plan.Phases[0].Name,
		Parallel: len(picks) > 1,
		Sources:  picks,
	}}, AllCandidates: all}
}

// spawnDetachedBrainIndex launches `aida brain index` as a detached child
// process. The child runs in its own process group (Setpgid) so that when
// the parent aida exits after the query completes, the index keeps running.
// Returns an error if we can't resolve the aida binary or if Start fails.
func spawnDetachedBrainIndex() error {
	return spawnDetachedAida("brain", "index")
}

// spawnDetachedWikiIndex launches `aida wiki index` the same way -- see
// spawnDetachedAida.
func spawnDetachedWikiIndex() error {
	return spawnDetachedAida("wiki", "index")
}

// spawnDetachedAida launches `aida <args...>` as a detached child process,
// used for background index rebuilds (brain, wiki) so the current query
// isn't blocked waiting on a slow full reindex. The child runs in its own
// process group (Setpgid) so it survives the parent's exit; there's
// nothing watching its stdio so it goes to /dev/null.
func spawnDetachedAida(args ...string) error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve aida binary: %w", err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Detach: don't Wait. Child's output goes to /dev/null.
	return cmd.Start()
}

// buildTracingData constructs tracing data from the cost tracker and pipeline durations.
func buildTracingData(client *llm.Client, parseDur, execDur, synthDur time.Duration) *runs.TracingData {
	calls := client.CostCalls()
	if len(calls) == 0 {
		return nil
	}

	td := &runs.TracingData{}

	// Build spans from cost tracker calls
	for _, c := range calls {
		span := runs.TracingSpan{
			Name:       c.Stage,
			DurationMs: c.DurationMs,
			CostUSD:    c.CostUSD,
			InTokens:   c.InputTokens,
			OutTokens:  c.OutputTokens,
			Model:      c.Model,
			Status:     "ok",
		}
		if span.Name == "" {
			span.Name = "call"
		}
		td.Spans = append(td.Spans, span)
		td.TotalCost += c.CostUSD
		td.TotalIn += c.InputTokens
		td.TotalOut += c.OutputTokens
		td.LLMCalls++
	}

	return td
}

// buildPlanDisplay converts an engine.ExecutionPlan into a ui.PlanDisplay.
func buildPlanDisplay(plan *engine.ExecutionPlan) ui.PlanDisplay {
	display := ui.PlanDisplay{
		Phases: make([]ui.PhaseDisplay, 0, len(plan.Phases)),
	}
	for _, phase := range plan.Phases {
		pd := ui.PhaseDisplay{
			Name:     phase.Name,
			Parallel: phase.Parallel,
		}
		for _, ss := range phase.Sources {
			pd.Sources = append(pd.Sources, ss.Name)
		}
		display.Phases = append(display.Phases, pd)
	}
	return display
}

// buildContinuationContext formats a prior run's data as context for
// follow-up queries using the --continue flag. This lets the LLM
// understand references like "it", "that", "the same", etc.
func buildContinuationContext(run *runs.Run) string {
	var buf strings.Builder
	buf.WriteString("## Prior conversation context\n")
	buf.WriteString(fmt.Sprintf("The user previously asked: %q\n", run.Question))

	// Truncate answer for context
	answer := run.Answer
	if len(answer) > 500 {
		answer = answer[:500] + "..."
	}
	buf.WriteString(fmt.Sprintf("Answer received: %q\n", answer))

	// Sources used
	var srcNames []string
	for _, ph := range run.Phases {
		for _, sr := range ph.Sources {
			srcNames = append(srcNames, sr.Name)
		}
	}
	if len(srcNames) > 0 {
		buf.WriteString(fmt.Sprintf("Sources used: [%s]\n", strings.Join(srcNames, ", ")))
	}

	// Entities
	if len(run.Entities) > 0 {
		buf.WriteString(fmt.Sprintf("Entities mentioned: [%s]\n", strings.Join(run.Entities, ", ")))
	}

	buf.WriteString("\nUse this context to understand follow-up references like \"it\", \"that\", \"the same\", etc.")
	return buf.String()
}

// taskIntentOverriddenByFeedback reports whether prior feedback on a similar
// question says the task short-circuit was the wrong call. Returns true when
// a prior lesson with action=task carries thumbs-down or note feedback and
// the question is similar enough to trust as the same shape of query.
//
// Two paths, tried in order:
//  1. Embeddings-backed search via brain.Search, threshold 0.75.
//  2. Jaccard fallback over the profile's task-feedback lessons - used when
//     embeddings are unavailable (no Voyage key, or lessons pre-dating the
//     embedding feature). Threshold 0.5 since Jaccard scores are typically
//     lower than cosine similarity on short CLI-style questions.
func taskIntentOverriddenByFeedback(ctx context.Context, brn *brain.Brain, question string) bool {
	if brn == nil {
		return false
	}

	// Path 1: embeddings-backed semantic search.
	if sc, err := brn.Search(ctx, question, nil, 3); err == nil && sc != nil {
		for _, sl := range sc.SimilarLessons {
			if sl.Similarity < 0.75 {
				break
			}
			if sl.Lesson.Action != "task" {
				continue
			}
			if sl.Lesson.Feedback == string(lessons.FeedbackThumbsDown) ||
				sl.Lesson.Feedback == string(lessons.FeedbackNote) {
				ui.PrintVerbose("Task intercept",
					fmt.Sprintf("skipped via embeddings -- prior run %s has %s feedback (sim=%.2f)",
						sl.Lesson.ID, sl.Lesson.Feedback, sl.Similarity))
				return true
			}
		}
	}

	// Path 2: Jaccard fallback. Cheap to run because the candidate set is
	// tiny -- only lessons that were classified as tasks AND have user
	// feedback attached.
	if brn.DB == nil {
		return false
	}
	candidates, err := brn.DB.FindTaskFeedbackLessons(brn.Profile())
	if err != nil || len(candidates) == 0 {
		return false
	}
	qTokens := jaccardTokens(question)
	if len(qTokens) == 0 {
		return false
	}
	for _, l := range candidates {
		score := jaccardScore(qTokens, jaccardTokens(l.Question))
		if score < 0.5 {
			continue
		}
		ui.PrintVerbose("Task intercept",
			fmt.Sprintf("skipped via jaccard -- prior run %s has %s feedback (score=%.2f)",
				l.ID, l.Feedback, score))
		return true
	}
	return false
}

// jaccardTokens lowercases and splits on non-alphanumeric runes, dropping
// a small stop-word set. Duplicated here (rather than calling into the
// lessons package) to avoid depending on an unexported helper.
func jaccardTokens(s string) map[string]struct{} {
	s = strings.ToLower(s)
	stop := map[string]bool{
		"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
		"be": true, "by": true, "can": true, "did": true, "do": true, "for": true,
		"from": true, "has": true, "have": true, "i": true, "in": true, "is": true,
		"it": true, "me": true, "my": true, "of": true, "on": true, "or": true,
		"that": true, "the": true, "this": true, "to": true, "was": true, "what": true,
		"when": true, "where": true, "which": true, "who": true, "why": true,
		"with": true, "you": true, "your": true, "s": true, "t": true, "left": true,
		"done": true,
	}
	out := map[string]struct{}{}
	var cur strings.Builder
	flush := func() {
		tok := cur.String()
		cur.Reset()
		if len(tok) > 1 && !stop[tok] {
			out[tok] = struct{}{}
		}
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func jaccardScore(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// isTerminal returns true if f is connected to an interactive terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
