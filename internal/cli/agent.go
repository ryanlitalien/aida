package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/cloud"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine"
	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/mcp"
	"github.com/ryanlitalien/aida/internal/runs"
	"github.com/ryanlitalien/aida/internal/ui"
)

// agentSystemPrompt is the system prompt for the agent loop. It gives
// the LLM a persona and instructions for multi-step reasoning.
const agentSystemPrompt = `You are Aida, an autonomous agent that answers questions and completes tasks by using tools.

WORKFLOW:
1. Analyze the user's question to understand what information or action is needed.
2. Select and call the appropriate tool(s) to gather data or perform actions.
3. If the first result is insufficient, try different tools or refine your queries.
4. Once you have enough information, synthesize a clear, cited answer.

RULES:
- Always try to use tools before answering. Do not guess or hallucinate.
- For data questions, query the relevant source (a data warehouse, grep, etc.) with precise queries.
- For code tasks, use delegate_to_claude_code to hand off implementation work.
- If you need clarification from the user, use the ask_user tool.
- When you have the answer, respond with a clear, concise final answer.
- Cite the tool/source that provided each piece of information.
- If a tool returns an error, try a different approach before giving up.
- Each source tool operates on its own configured path, independent of the current working directory. Use them from anywhere.
- When asked about available projects, refer to the ENVIRONMENT section for source paths.

OUTPUT FORMAT:
- Your output is rendered directly in a terminal. Use plain text, not markdown.
- For tabular data, use ASCII box-drawing tables, not markdown tables. Example:
  ┌──────────┬────────┬───────────┐
  │ Name     │ Type   │ Status    │
  ├──────────┼────────┼───────────┤
  │ aida   │ cli    │ active    │
  └──────────┴────────┴───────────┘
- Do not use markdown headers (##), bold (**), or other markdown formatting.
- Use plain dashes or bullets for lists.

TOOL SELECTION:
- SQL/data queries → use the appropriate data source tool
- Log search → use the appropriate log/metrics tool
- Code search → use grep/codebase tools
- Code changes / PRs → use delegate_to_claude_code
- MCP tools (prefixed with mcp__) → specialized capabilities from MCP servers
`

// runAgentMode handles the agent loop execution path.
//
// libIssues carries any library load-time rejections (sources whose
// exec.query is a bare {query} placeholder, etc.). When non-empty,
// they are injected into the agent's system prompt as remediation
// context - see "LIBRARY ISSUES" section below. The OpenAI harness
// pattern: validator output becomes agent context so the agent can
// surface "this source could exist if you fix it like so" rather
// than silently routing around the broken source.
func runAgentMode(
	ctx context.Context,
	cfg *config.Config,
	apiKey string,
	model string,
	question string,
	soulCtx string,
	srcs config.Sources,
	brn *brain.Brain,
	pastLessons []lessons.SimilarLesson,
	intent *engine.Intent,
	classified *engine.ClassifiedIntent,
	resolved *engine.ResolvedContext,
	profileName string,
	totalStart time.Time,
	libIssues []library.LibraryIssue,
	libRegistry *library.Registry,
) error {
	// Pre-generate the run id so the exec-plan, the run record, the
	// eval-run, and the trajectory all key off the same string.
	// This is the join column for future `aida brain analyze` work
	// that surfaces patterns across artifacts.
	runID := runs.NewID(totalStart, question)

	// --run-dir mode: route output to events.ndjson + output.md
	// instead of TTY. Disables the spinner, suppresses TTY-only
	// helpers, forces a no-color env so any subordinate code that
	// still emits ANSI is quiet. The web UI's "Prepare draft" path
	// and the post-jobs-migration `tasks ingest --auto-solve` path
	// both depend on this.
	var runDirSink *runDirSink
	if agentRunDir != "" {
		var err error
		runDirSink, err = openRunDirSink(agentRunDir, runID, question)
		if err != nil {
			return fmt.Errorf("open run-dir sink: %w", err)
		}
		defer runDirSink.Close()
		// Force structured-output discipline. Streaming would print
		// to stdout; the run-dir contract is "no TTY output."
		agentStream = false
		os.Setenv("NO_COLOR", "1")
		os.Setenv("CLICOLOR", "0")
		os.Setenv("TERM", "dumb")
	}

	// Open an exec-plan for this agent run. Each non-trivial agent
	// invocation gets a durable file under brain/exec-plans/active/
	// that carries the question as goal, accumulates a decision log
	// as turns happen, and transitions to completed/abandoned at the
	// end. Per OpenAI's harness writeup, plans are first-class
	// artifacts so any future agent (or the user) can resume or
	// inspect mid-run.
	//
	// Best-effort: if the brain is unavailable, skip silently and
	// fall through to the existing path. Plan persistence must not
	// regress the agent loop.
	planActive := false
	if brn != nil {
		plan := brain.NewExecPlan(runID, question, brain.ExecPlanOrigin{
			Type: "cli",
			Ref:  "aida --agent",
		})
		plan.Goal = question
		plan.Tags = []string{"profile:" + profileName, "agent"}
		if err := brain.WriteExecPlan(brn.Path, plan); err != nil {
			ui.PrintVerbose("Exec-plan", "create error: "+err.Error())
		} else {
			ui.PrintVerbose("Exec-plan", fmt.Sprintf("%s (active)", runID))
			planActive = true
		}
	}

	// --continue: inject prior run context for follow-up queries
	if continueSession {
		if lastRun, err := runs.Latest(); err == nil && lastRun != nil {
			priorCtx := buildContinuationContext(lastRun)
			soulCtx = soulCtx + "\n\n" + priorCtx
			ui.PrintVerbose("Continue", fmt.Sprintf("loaded context from run %s", lastRun.ID))
		}
	}

	// NewSpinner self-mutes when ui.Quiet is set (root.go's
	// PersistentPreRun sets it from --run-dir).
	spinner := ui.NewSpinner()

	// ---------------------------------------------------------------
	// Discover MCP tools
	// ---------------------------------------------------------------
	var mcpTools []mcp.DiscoveredTool
	var mcpDiscovery *mcp.MCPDiscovery

	mcpConfigs, mcpErr := mcp.LoadMCPConfigs()
	if mcpErr != nil {
		ui.PrintVerbose("MCP", "config load error: "+mcpErr.Error())
	} else if len(mcpConfigs) > 0 {
		mcpDiscovery = mcp.NewMCPDiscovery(verbose)
		defer mcpDiscovery.Close()

		connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := mcpDiscovery.ConnectAll(connectCtx, mcpConfigs); err != nil {
			ui.PrintVerbose("MCP", "connect error: "+err.Error())
		}
		cancel()

		discovered, err := mcpDiscovery.DiscoverTools(ctx)
		if err != nil {
			ui.PrintVerbose("MCP", "discovery error: "+err.Error())
		} else {
			mcpTools = discovered
			ui.PrintVerbose("MCP tools", fmt.Sprintf("%d tools discovered", len(mcpTools)))
		}
	}

	// ---------------------------------------------------------------
	// Build agent tools
	// ---------------------------------------------------------------
	confirmFn := func(action string) bool {
		fmt.Printf("\n%s %s [y/N] ", ui.WarnIcon, action)
		var response string
		fmt.Scanln(&response)
		return strings.ToLower(strings.TrimSpace(response)) == "y"
	}

	profile, _ := cfg.ActiveProfileConfig()
	var confirmPatterns []string
	if profile != nil {
		confirmPatterns = profile.Guardrails.ConfirmAlways
	}
	// Action #6: lazy MCP tool loading. Source tools (small in number)
	// stay eager so per-source schemas remain visible in the system
	// prompt. MCP tools collapse to two meta-tools (find_mcp_tool +
	// call_mcp_tool) so the agent pulls only the definitions it needs.
	// TDS measured 55K-134K tokens of always-on tool definitions in
	// large MCP setups; this collapse trims that to ~300 tokens for
	// the two meta-tools plus whatever the agent fetches via find.
	//
	// claudeConfigDir routes delegate_to_claude_code's child process to
	// an organization-specific Claude config directory when this run's
	// profile has one configured (config.yaml's
	// agent_bounds.claude_config_dir_by_profile), and is empty for any
	// profile without an entry, which is today's behavior unchanged.
	claudeConfigDir := cfg.ClaudeConfigDirForProfile(profileName)
	tools := engine.BuildAgentToolsLazy(srcs, mcpTools, mcpDiscovery, confirmFn, confirmPatterns, claudeConfigDir)

	// L1/L2/L3 progressive disclosure (Action #1b). The L1 index of every
	// available library layer goes into the system prompt below; the L2
	// body of any single layer is fetched on demand via this tool. L3
	// (linked artifacts: source files, brain pages) is reachable through
	// the existing read/grep/notion-fetch tools.
	if libRegistry != nil {
		tools = append(tools, buildGetLayerTool(libRegistry))
	}

	// ask_user is only meaningful when the run is being managed by the
	// daemon (i.e. there's a run-dir to write input.txt into and a jobs
	// store row whose state we can toggle to awaiting_input). Opening
	// the store is best-effort - failure just means the tool is absent
	// for this run and the agent has to make its best guess instead of
	// pausing for the user.
	var agentJobsStore *jobs.Store
	var jobRunID string
	if runDirSink != nil && profileName != "" {
		// The jobs row was created by the SPAWNER (voice job_start, web
		// draft, ingest auto-solve) under the run-dir's basename. runID
		// above is this process's own artifact id, re-stamped from our
		// clock - the two differ by at least a second. Every store
		// transition (Pause/Resume/Complete/Fail) must key off the ROW
		// id or it targets a row that doesn't exist.
		jobRunID = filepath.Base(agentRunDir)
		store, openErr := jobs.Open(profileName)
		if openErr != nil {
			ui.PrintVerbose("ask_user", "jobs store unavailable: "+openErr.Error())
		} else {
			agentJobsStore = store
			defer agentJobsStore.Close()
			// upsert, NOT append: BuildAgentToolsLazy already registered a
			// generic stdin-reading ask_user. Registering a second tool
			// with the same name is an immediate API 400 ("tools: Tool
			// names must be unique") - which killed every daemon-spawned
			// job on turn 1. The run-dir version (pause row + poll
			// input.txt) must REPLACE the stdin one here.
			tools = upsertTool(tools, buildAskUserTool(runDirSink, agentJobsStore, jobRunID, agentRunDir))
			tools = upsertTool(tools, buildRequestApprovalTool(runDirSink, agentJobsStore, jobRunID, agentRunDir))
			// Flip the row to running with OUR host+pid: direct-spawned
			// agents (voice job_start, web draft) never go through
			// Claim, so without this the row reads "queued" for the
			// whole run and cancel has no pid to signal. Best-effort -
			// a run-dir without a spawner-enqueued row just logs.
			host, _ := os.Hostname()
			if err := agentJobsStore.MarkRunning(jobRunID, host, os.Getpid()); err != nil {
				ui.PrintVerbose("jobs", "MarkRunning: "+err.Error())
			}
		}
	}

	// Add cloud investigation tool if cloud config is available.
	if profile != nil && profile.Cloud != nil {
		cloudAPIKey := profile.Cloud.GetAPIKey(&cfg.API)
		if cloudAPIKey != "" {
			cloudClient := cloud.NewClient(cloudAPIKey)
			agentID, envID := profile.Cloud.AgentID, profile.Cloud.EnvironmentID
			var vaultIDs []string
			if profile.Cloud.VaultID != "" {
				vaultIDs = []string{profile.Cloud.VaultID}
			}
			if tool := buildCloudInvestigateTool(cloudClient, agentID, envID, vaultIDs); tool != nil {
				tools = append(tools, *tool)
				ui.PrintVerbose("Cloud investigate", "tool available")
			}
		}
	}

	ui.PrintVerbose("Agent tools", fmt.Sprintf("%d tools available", len(tools)))

	// ---------------------------------------------------------------
	// Build system prompt - split into a cacheable stable prefix and
	// a variable per-question suffix (Action #1c, frozen-snapshot
	// pattern). The Anthropic adapter marks the prefix cache_control:
	// ephemeral so repeat aida calls within ~5min hit the prompt cache;
	// OpenAI's prefix cache is automatic on identical prefix.
	//
	// What goes where:
	//   PREFIX (stable across calls within a session)
	//     - agentSystemPrompt template
	//     - USER CONTEXT (soul context, profile-stable)
	//     - ENVIRONMENT (home, cwd, profile - typically stable across
	//       consecutive calls in the same shell)
	//     - AVAILABLE SOURCE PATHS
	//     - LIBRARY ISSUES (config-time rejections)
	//     - LIBRARY LAYERS (L1 index)
	//     - AVAILABLE TOOLS
	//   SUFFIX (per-question)
	//     - LEARNED TOOL PATTERNS (from similar past lessons)
	//     - BRAIN CONTEXT (entity pages, routing wisdom for THIS query)
	//     - RESOLVED ENTITIES (this query's parsed/resolved entities)
	// ---------------------------------------------------------------
	var stablePrompt strings.Builder
	stablePrompt.WriteString(agentSystemPrompt)

	if soulCtx != "" {
		fmt.Fprintf(&stablePrompt, "\nUSER CONTEXT:\n%s\n", soulCtx)
	}

	// Environment context so the agent knows about the user's home
	// directory and active profile.
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	fmt.Fprintf(&stablePrompt, "\nENVIRONMENT:\n")
	fmt.Fprintf(&stablePrompt, "- Home directory: %s\n", home)
	fmt.Fprintf(&stablePrompt, "- Current working directory: %s\n", cwd)
	fmt.Fprintf(&stablePrompt, "- Profile: %s\n", profileName)

	// Source paths so the agent knows what projects are available.
	var sourcePaths []string
	for name, src := range srcs {
		if src.Path != "" {
			sourcePaths = append(sourcePaths, fmt.Sprintf("- %s: %s", name, src.Path))
		}
	}
	if len(sourcePaths) > 0 {
		stablePrompt.WriteString("\nAVAILABLE SOURCE PATHS:\n")
		for _, sp := range sourcePaths {
			fmt.Fprintln(&stablePrompt, sp)
		}
	}

	// Library load-time issues - sources rejected at config load
	// time (raw {query} passthroughs, etc.). Surfacing these as
	// agent context lets the agent mention "you might want to fix
	// source X" when its capabilities would have been relevant.
	// Format follows OpenAI's harness-engineering pattern: each
	// rejection is a problem + concrete fix, written as remediation
	// guidance rather than a description of the bug.
	if len(libIssues) > 0 {
		stablePrompt.WriteString("\nLIBRARY ISSUES (sources currently disabled - mention to user only when relevant to the question):\n")
		for _, iss := range libIssues {
			stablePrompt.WriteString(iss.AgentContext())
		}
	}

	// L1 progressive-disclosure index - Action #1b. List every available
	// library layer with its 1-line description. The agent decides which
	// (if any) L2 body to pull via the get_layer tool.
	if libRegistry != nil {
		if l1 := library.L1IndexString(libRegistry.LayerSummaries()); l1 != "" {
			stablePrompt.WriteString("\nLIBRARY LAYERS (call get_layer with the name to fetch a layer's full body):\n")
			stablePrompt.WriteString(l1)
		}
	}

	// (No "AVAILABLE TOOLS:" prose section. The API's tools[] array
	// already carries each tool's name + description + schema; listing
	// them again in the system prompt is a 50× tokens-per-tool double
	// count. Removed as part of Action #6 alongside lazy MCP loading.)

	// Variable suffix - per-question content. Kept compact so the
	// stable prefix stays the dominant token contribution.
	var variablePrompt strings.Builder

	// Inject learned tool patterns from high-quality past lessons.
	// pastLessons is question-driven, so this varies per call.
	if toolPatterns := buildAgentToolPatterns(pastLessons); toolPatterns != "" {
		variablePrompt.WriteString("\n")
		variablePrompt.WriteString(toolPatterns)
	}

	// Brain context: similar past lessons + entity pages for the
	// CURRENT question. Captured once here so the agent loop runs
	// against a consistent snapshot for the rest of this aida call.
	if brn != nil {
		brainEntities := append([]string(nil), intent.RawEntities...)
		sc, err := brn.Search(ctx, question, brainEntities, 5)
		if err == nil && sc != nil {
			brainCtx := sc.FormatContextForRouter()
			if brainCtx != "" {
				fmt.Fprintf(&variablePrompt, "\nBRAIN CONTEXT (past lessons and entity knowledge):\n%s\n", brainCtx)
			}
		}

		// Multi-channel retrieval (Action #1d). Surfaces typed memory
		// (facts/instructions) keyed on parsed entities, plus FTS5 +
		// substring channel hits, RRF-fused. Treated as additive to
		// the lessons-shaped Search above - the format is different
		// (per-doc snippets) so we tag the section distinctly.
		multi, err := brn.SearchMulti(ctx, question, brainEntities, 5)
		if err == nil && len(multi) > 0 {
			variablePrompt.WriteString("\nRELEVANT MEMORY (multi-channel retrieval; channel tags indicate why each was surfaced):\n")
			for _, r := range multi {
				channels := make([]string, 0, len(r.Channels))
				for c := range r.Channels {
					channels = append(channels, string(c))
				}
				snippet := r.Body
				if len(snippet) > 280 {
					snippet = snippet[:280] + "…"
				}
				fmt.Fprintf(&variablePrompt, "- [%s; %s] %s\n", r.DocType, strings.Join(channels, "+"), snippet)
			}
		}

		// Standing instructions (autonomous-loop Phase 2): always surface
		// active instruction-type memory (e.g. "build-after-changes")
		// regardless of whether it matched the question's entities, so
		// durable directives are never silently dropped by relevance
		// filtering.
		if instrs, err := brn.DB.ListMemory(brain.MemoryListOpts{
			Types:   []brain.MemoryType{brain.MemoryInstruction},
			Profile: profileName,
			Limit:   10,
		}); err == nil && len(instrs) > 0 {
			variablePrompt.WriteString("\nSTANDING INSTRUCTIONS (durable directives - follow unless the user overrides):\n")
			for _, m := range instrs {
				line := strings.TrimSpace(m.Body)
				if line == "" {
					continue
				}
				if len(line) > 280 {
					line = line[:280] + "…"
				}
				fmt.Fprintf(&variablePrompt, "- %s\n", line)
			}
		}
	}

	if resolved != nil {
		values := resolved.GetResolvedValues()
		if len(values) > 0 {
			variablePrompt.WriteString("\nRESOLVED ENTITIES:\n")
			for k, v := range values {
				fmt.Fprintf(&variablePrompt, "- %s: %s\n", k, v)
			}
		}
	}

	// ---------------------------------------------------------------
	// Run agent loop via orchestrator
	// ---------------------------------------------------------------
	agentModel := model
	if cfg.Agent.Model != "" {
		agentModel = cfg.Agent.Model
	}

	llmAdapter := engine.NewLLMAdapter(apiKey, agentModel)
	orchTools := engine.AdaptTools(tools)

	agent := &orchestrator.Agent{
		Name:               "aida-agent",
		InstructionsPrefix: stablePrompt.String(),
		Instructions:       variablePrompt.String(),
		Model:              llmAdapter,
		Tools:              orchTools,
	}

	// Bounds. orchestrator.Agent.MaxTurns is no longer honored by the
	// Runner (see its doc comment); a wall-clock deadline and a cost
	// ceiling replace the old turn cap instead. Every run gets a deadline, so
	// a voice/web-spawned job that previously relied on the turn cap
	// as its only bound is never unbounded; the cost ceiling only
	// applies once the operator has actually set one, matching
	// MaxBudgetUSD's existing "0 = unbounded" contract.
	runOpts := []orchestrator.RunOption{
		orchestrator.WithDeadline(time.Now().Add(cfg.AgentMaxWallClock())),
	}
	if cfg.Agent.MaxBudgetUSD > 0 {
		// costFuncFor reuses the exact pricing path costUSD is computed
		// from below (engine.CostFromUsage), so the enforced ceiling
		// and the reported cost never disagree.
		runOpts = append(runOpts, orchestrator.WithCostLimit(cfg.Agent.MaxBudgetUSD, costFuncFor(agentModel)))
	}
	if agentStream {
		// Streaming mode: print tokens live, suppress spinner during generation.
		runOpts = append(runOpts, orchestrator.WithStreamSink(makeAgentStreamSink()))
	} else if runDirSink != nil {
		// Run-dir mode: emit live tool_call_start events into
		// events.ndjson so an SSE-tailing UI sees mid-run progress
		// rather than a single dump at the end. Text deltas are
		// dropped - final answer goes to output.md.
		runOpts = append(runOpts, orchestrator.WithStreamSink(makeRunDirStreamSink(runDirSink)))
	}

	if agentStream {
		fmt.Println() // blank line before streaming output
	} else {
		spinner.Start("Agent reasoning...")
	}

	var result *orchestrator.RunResult
	var err error
	if agentGraph {
		// Opt-in multi-agent path: triage -> parallel specialists ->
		// evaluator-optimizer synthesis. Reuses the same model adapter,
		// tool palette, AND grounding (stable + variable system prompts,
		// which carry resolved entities / partner IDs / context docs) as
		// the single agent, so graph answers are grounded identically. The
		// default path (flag off) is unchanged. runOpts is passed through
		// so a --run-dir background job still emits live events; on an
		// interactive TTY the parallel specialists may interleave tokens.
		grounding := stablePrompt.String()
		if v := variablePrompt.String(); v != "" {
			grounding += "\n\n" + v
		}
		graph := engine.BuildInvestigateGraph(llmAdapter, orchTools, engine.InvestigateConfig{
			SystemContext: grounding,
		})
		result, err = graph.Run(ctx, question, runOpts...)
	} else {
		result, err = (&orchestrator.Runner{}).Run(ctx, agent, question, runOpts...)
	}

	if agentStream {
		fmt.Println() // newline after streamed output
	} else {
		spinner.Stop(fmt.Sprintf("%s Agent complete", ui.SuccessIcon))
	}

	if err != nil {
		// Best-effort: mark the plan abandoned so it doesn't
		// pollute the active list. The decision log captures the
		// failure for later inspection.
		if planActive && brn != nil {
			_ = brain.AppendDecisionLog(brn.Path, runID, "agent",
				fmt.Sprintf("agent loop failed: %s", err.Error()))
			_ = brain.TransitionExecPlan(brn.Path, runID, brain.ExecPlanStatusAbandoned)
		}
		if runDirSink != nil {
			runDirSink.emitError(err.Error())
			// Also write a small output.md so callers reading
			// /api/runs/<id>/output get a useful message rather
			// than 404. Format mirrors the failed-decision-log
			// shape used by autoSolveIngestedTasks.
			_ = runDirSink.writeOutput("aida --agent failed: " + err.Error() + "\n")
		}
		// Close out our own jobs row - the spawner only fires cmd.Wait,
		// so without this a failed run sat in "queued"/"running" forever
		// and the notifier never announced the failure. Sticky terminal
		// states make this a no-op if a cancel already failed the row.
		if agentJobsStore != nil {
			_ = agentJobsStore.Fail(jobRunID, err.Error())
		}
		return fmt.Errorf("agent mode failed: %w", err)
	}

	costUSD := engine.CostFromUsage(result.Usage, agentModel)
	totalDuration := time.Since(totalStart)

	// ---------------------------------------------------------------
	// Display results
	// ---------------------------------------------------------------
	if runDirSink != nil {
		// Run-dir mode: emit one tool_call event per completed
		// tool invocation (these are the canonical batched events,
		// independent of any stream-sink emissions during the run),
		// then write output.md, then the complete event.
		for _, tc := range result.ToolCalls {
			runDirSink.emitToolCall(tc.Turn, tc.Tool, tc.Input, tc.Output, tc.Error)
		}
		if err := runDirSink.writeOutput(result.FinalOutput); err != nil {
			runDirSink.emitVerbose("output.md", "write error: "+err.Error())
		}
		runDirSink.emitComplete(result.Turns, costUSD, len(result.FinalOutput))
		// output.md is on disk, so it's safe to transition the row now.
		// Unlike the old unconditional Complete, Finalize looks at how
		// the run actually ended (result.Termination, result.Result) and
		// only lands on done when there's a verified achieved outcome
		// behind it, see jobs.FinalizeState. A run that burned its bound
		// (wall clock / cost) or ended with free text and no
		// submit_result call lands on incomplete instead of a false
		// "done", which is the whole point: an agent that never
		// verified its own work must never be reported as having
		// finished it.
		if agentJobsStore != nil {
			if finalizeErr := agentJobsStore.Finalize(jobRunID, finalizeParamsFromResult(result)); finalizeErr != nil {
				ui.PrintVerbose("jobs", "Finalize: "+finalizeErr.Error())
			}
		}
	} else if !agentStream {
		// TTY mode, non-streaming: print the result block.
		// Streaming already printed text; skip the repeat.
		ui.PrintResult(result.FinalOutput)
	}
	ui.PrintVerbose("Agent turns", fmt.Sprintf("%d", result.Turns))
	ui.PrintVerbose("Agent tool calls", fmt.Sprintf("%d", len(result.ToolCalls)))
	ui.PrintVerbose("Agent cost", fmt.Sprintf("$%.4f", costUSD))

	if verbose {
		for _, tc := range result.ToolCalls {
			status := "ok"
			if tc.Error != "" {
				status = "error: " + tc.Error
			}
			ui.PrintVerbose(fmt.Sprintf("  Turn %d", tc.Turn), fmt.Sprintf("%s → %s", tc.Tool, status))
		}
	}

	// ---------------------------------------------------------------
	// Append decision-log entries to the exec-plan and mark
	// it completed. One log line per tool call so the plan
	// reads as a chronological narrative of what the agent did,
	// not just a final answer dump.
	// ---------------------------------------------------------------
	if planActive && brn != nil {
		for _, tc := range result.ToolCalls {
			status := "ok"
			if tc.Error != "" {
				status = "error: " + truncate(tc.Error, 200)
			}
			_ = brain.AppendDecisionLog(brn.Path, runID, "agent",
				fmt.Sprintf("turn %d: %s → %s", tc.Turn, tc.Tool, status))
		}
		_ = brain.AppendDecisionLog(brn.Path, runID, "agent",
			fmt.Sprintf("completed in %d turns, $%.4f", result.Turns, costUSD))
		if err := brain.TransitionExecPlan(brn.Path, runID, brain.ExecPlanStatusCompleted); err != nil {
			ui.PrintVerbose("Exec-plan", "transition error: "+err.Error())
		} else {
			ui.PrintVerbose("Exec-plan", fmt.Sprintf("%s → completed", runID))
		}
	}

	// ---------------------------------------------------------------
	// Record run and lesson
	// ---------------------------------------------------------------
	run := &runs.Run{
		ID:        runID,
		StartedAt: totalStart,
		TotalMs:   totalDuration.Milliseconds(),
		Question:  question,
		Cwd:       cwd,
		Profile:   profileName,
		Action:    "agent",
		Strategy:  "agent",
		Entities:  intent.RawEntities,
		Answer:    result.FinalOutput,
	}

	for _, tc := range result.ToolCalls {
		run.AgentToolCalls = append(run.AgentToolCalls, runs.AgentToolCallRun{
			Turn:   tc.Turn,
			Tool:   tc.Tool,
			Input:  tc.Input,
			Output: truncateRunSummary(tc.Output, 1500),
			Error:  tc.Error,
		})
	}

	if path, err := runs.Save(run); err != nil {
		ui.PrintVerbose("Run log", "save error: "+err.Error())
	} else {
		ui.PrintVerbose("Run log", path)
	}

	// Record lesson for future routing.
	snippet := result.FinalOutput
	if len(snippet) > 500 {
		snippet = snippet[:500] + "..."
	}

	// Collect tool names used.
	toolsSeen := make(map[string]bool)
	var toolNames []string
	for _, tc := range result.ToolCalls {
		if !toolsSeen[tc.Tool] {
			toolsSeen[tc.Tool] = true
			toolNames = append(toolNames, tc.Tool)
		}
	}

	// Capture successful tool call inputs as executed queries so future
	// similar questions can reuse the known-good commands.
	executedQueries := make(map[string]string)
	for _, tc := range result.ToolCalls {
		if tc.Error == "" && tc.Output != "" {
			executedQueries[tc.Tool] = tc.Input
		}
	}
	var eqField map[string]string
	if len(executedQueries) > 0 {
		eqField = executedQueries
	}

	lesson := &lessons.Lesson{
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		RunID:           run.ID,
		Question:        strings.ToLower(strings.TrimSpace(question)),
		Cwd:             cwd,
		Action:          "agent",
		Strategy:        "agent",
		Sources:         toolNames,
		ArtifactCount:   len(result.ToolCalls),
		AnswerSnippet:   snippet,
		ExecutedQueries: eqField,
	}

	if brn != nil {
		if err := brn.RecordLesson(ctx, lesson); err != nil {
			ui.PrintVerbose("Brain write", "error: "+err.Error())
		}
	}

	return nil
}

// buildAgentToolPatterns extracts tool selection patterns from high-quality
// past lessons that have ExecutedQueries. This gives the agent loop a
// "memory" of which tools worked well for similar questions, following
// the same pattern as buildExecutorFeedback in executor.go.
func buildAgentToolPatterns(pastLessons []lessons.SimilarLesson) string {
	var patterns []string
	for _, ls := range pastLessons {
		l := ls.Lesson
		if l.ExecutedQueries == nil {
			continue
		}
		// Only include high-quality or explicitly confirmed lessons.
		if l.Quality < 4 && l.Feedback != lessons.FeedbackThumbsUp {
			continue
		}
		quality := l.Quality
		if quality == 0 && l.Feedback == lessons.FeedbackThumbsUp {
			quality = 5 // thumbs-up with no scorer = treat as excellent
		}
		for tool := range l.ExecutedQueries {
			patterns = append(patterns, fmt.Sprintf("- For questions like %q → use %q tool (quality %d/5)", l.Question, tool, quality))
		}
	}
	if len(patterns) == 0 {
		return ""
	}
	return "## Learned tool patterns from prior successful queries:\n" + strings.Join(patterns, "\n") + "\n"
}

// buildGetLayerTool returns the agent's L2 fetcher for the L1/L2/L3
// progressive-disclosure flow (Action #1b). The system prompt lists
// every available layer's name + 1-line description as L1; this tool
// returns the full body of one layer when the agent decides it's worth
// reading. Calling with an unknown name returns an error string the
// agent can recover from (typo, layer disabled by missing tool, etc.).
func buildGetLayerTool(reg *library.Registry) engine.AgentTool {
	return engine.AgentTool{
		Name:        "get_layer",
		Description: "Fetch the full body of a library layer by name. Layer names + 1-line descriptions are listed under LIBRARY LAYERS in the system prompt - call this when you decide a layer is worth reading in full. Returns the layer's markdown content.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "Exact layer name from the LIBRARY LAYERS index.",
				},
			},
			"required": []interface{}{"name"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			args.Name = strings.TrimSpace(args.Name)
			if args.Name == "" {
				return "", fmt.Errorf("name is required")
			}
			layer, ok := reg.Layers[args.Name]
			if !ok {
				return "", fmt.Errorf("layer %q not found", args.Name)
			}
			if !layer.Available {
				return "", fmt.Errorf("layer %q is unavailable on this machine (missing tools: %v)", args.Name, layer.MissingTools)
			}
			bundle := reg.MaterializeLayers([]string{args.Name})
			if bundle.IsEmpty() {
				return "", fmt.Errorf("layer %q failed to materialize", args.Name)
			}
			return bundle.String(), nil
		},
	}
}

// buildCloudInvestigateTool creates a tool that delegates deep
// investigation to an Anthropic Managed Agent session. Returns nil if
// the agent ID is empty (cloud not provisioned yet).
func buildCloudInvestigateTool(cloudClient *cloud.Client, agentID, envID string, vaultIDs []string) *engine.AgentTool {
	if cloudClient == nil || agentID == "" {
		return nil
	}
	managed := &cloud.ManagedRunnable{
		Name_:    "cloud-investigator",
		Client:   cloudClient,
		AgentID:  agentID,
		EnvID:    envID,
		VaultIDs: vaultIDs,
	}

	return &engine.AgentTool{
		Name:        "cloud_investigate",
		Description: "Delegate a deep investigation to a cloud-hosted Claude agent with sandbox access. Use this for complex questions that require sustained multi-step research, running code, or accessing cloud-only resources. The cloud agent has its own tool suite and can run for many turns independently.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"brief": map[string]interface{}{
					"type":        "string",
					"description": "A detailed investigation brief: what to investigate, what data to look for, what success looks like. Include any entity IDs, time ranges, or context the investigator needs.",
				},
			},
			"required": []interface{}{"brief"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Brief string `json:"brief"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if args.Brief == "" {
				return "", fmt.Errorf("investigation brief is required")
			}

			result, err := managed.Run(ctx, args.Brief)
			if err != nil {
				return "", fmt.Errorf("cloud investigation failed: %w", err)
			}
			if result.FinalOutput == "" {
				return "(investigation produced no output)", nil
			}
			return result.FinalOutput, nil
		},
	}
}

// buildAskUserTool returns the engine-side ask_user tool used by
// long-running background agents to yield control back to the user.
// The tool transitions the job's SQL/manifest state to awaiting_input,
// emits an awaiting_input run-dir event, and then blocks polling
// <run-dir>/input.txt every 2s. When the file appears, it is read,
// deleted (so a second ask_user call doesn't pick up a stale reply),
// the job state is flipped back to running, an input_received event
// is emitted, and the user's reply is returned to the agent loop as
// the tool result.
//
// Only registered for daemon-managed runs (runDirSink != nil). Not
// exposed to the Jarvis-side voice LLM - that's the top-level agent;
// recursion is prevented per feedback_no_mcp_query_tool.
func buildAskUserTool(sink *runDirSink, store *jobs.Store, runID, runDir string) engine.AgentTool {
	return engine.AgentTool{
		Name: "ask_user",
		Description: "Pause the agent run and ask the user a clarifying question. " +
			"USE THIS when a decision is required that you genuinely cannot make on the " +
			"user's behalf (e.g. 'should I rebase onto main or merge?', 'which of three " +
			"identical-looking refunds should I issue?'). The tool blocks until the user " +
			"replies via the daemon's job_send_input voice tool or the tasks-web input " +
			"endpoint. The user's reply is returned to you as the tool result; continue " +
			"the run from there. Do NOT use this for questions you can answer by reading " +
			"the code or running another tool - prefer doing the work over interrupting.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"prompt": map[string]interface{}{
					"type":        "string",
					"description": "the question to ask the user, phrased naturally; spoken aloud by Jarvis on next wake",
				},
			},
			"required": []interface{}{"prompt"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Prompt string `json:"prompt"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			prompt := strings.TrimSpace(args.Prompt)
			if prompt == "" {
				return "", fmt.Errorf("prompt is required")
			}

			// Best-effort: write the new state before announcing it.
			// If Pause fails (DB lock, disk full, …) we still emit
			// the awaiting_input event so the UI sees the question.
			// The store transition is the canonical signal for the
			// daemon's notifier; without it the user will only see
			// the prompt in the run-dir / tasks-web.
			if err := store.Pause(runID, prompt); err != nil {
				if sink != nil {
					sink.emitVerbose("ask_user", "store.Pause failed: "+err.Error())
				}
			}
			if sink != nil {
				sink.emitAwaitingInput(prompt)
			}

			inputPath := filepath.Join(runDir, "input.txt")
			// Defensive: remove any stale reply that pre-dates this
			// call (e.g. left over from a previous run-dir reuse, or
			// a race where the user fired job_send_input before the
			// agent invoked ask_user). Treat missing-file as fine.
			_ = os.Remove(inputPath)

			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-ticker.C:
				}
				data, err := os.ReadFile(inputPath)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					// Surface real read errors immediately - the
					// agent loop can treat this like any other tool
					// failure and proceed without the reply.
					return "", fmt.Errorf("read input.txt: %w", err)
				}
				reply := strings.TrimSpace(string(data))
				// Best-effort cleanup; if removal fails the next
				// poll will short-circuit on the same file.
				_ = os.Remove(inputPath)

				if err := store.Resume(runID); err != nil && sink != nil {
					sink.emitVerbose("ask_user", "store.Resume failed: "+err.Error())
				}
				if sink != nil {
					sink.emitInputReceived(reply)
				}
				if reply == "" {
					// Empty file = user replied with empty string;
					// return a placeholder so the agent doesn't
					// receive a bare empty result that's easy to
					// hallucinate around.
					return "(user replied with an empty message)", nil
				}
				return reply, nil
			}
		},
	}
}

// buildRequestApprovalTool returns the engine-side request_approval tool: the
// agent calls it immediately BEFORE any irreversible action (merging a PR,
// sending Slack/email, writing back to Notion). It transitions the job to
// awaiting_approval, announces it, and blocks polling <run-dir>/approval.txt
// for the user's verdict (written by the approve_job/reject_job voice tools or
// `aida jobs approve|reject`). "approve" => store.Approve + return "approved" so
// the agent proceeds; anything else => the run is failed and the tool returns
// an error so the agent STOPS without acting. A missing/empty file keeps
// waiting - fail-safe (the gate never opens on its own).
//
// Like ask_user this is ENGINE-side (never exposed to the Jarvis voice LLM);
// the recursion guard (feedback_no_mcp_query_tool) is about aida_query.
// upsertTool replaces the palette entry sharing t's name, or appends when
// absent. The Anthropic API rejects a request outright when two tools share
// a name, so any tool that specializes a generically-registered one (the
// run-dir ask_user replacing the engine's stdin ask_user) must go through
// this instead of append.
func upsertTool(tools []engine.AgentTool, t engine.AgentTool) []engine.AgentTool {
	for i := range tools {
		if tools[i].Name == t.Name {
			tools[i] = t
			return tools
		}
	}
	return append(tools, t)
}

func buildRequestApprovalTool(sink *runDirSink, store *jobs.Store, runID, runDir string) engine.AgentTool {
	return engine.AgentTool{
		Name: "request_approval",
		Description: "Pause and request HUMAN APPROVAL before an IRREVERSIBLE action " +
			"(merging a PR to main, sending a Slack/email message, writing back to Notion). " +
			"You MUST call this and receive approval BEFORE performing any such action. Provide " +
			"`action` (merge|send|writeback) and `payload` (a one-line description of exactly " +
			"what will happen). Blocks until the user approves or rejects by voice (\"approve " +
			"PR 583\") or `aida jobs approve <ref>`. On approval the tool returns \"approved\" and " +
			"you proceed; on rejection the run fails and you must STOP.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action": map[string]interface{}{
					"type":        "string",
					"enum":        []interface{}{"merge", "send", "writeback"},
					"description": "the class of irreversible action being requested",
				},
				"payload": map[string]interface{}{
					"type":        "string",
					"description": "one-line description of exactly what happens on approval, e.g. 'merge PR #83 into main'",
				},
			},
			"required": []interface{}{"action", "payload"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Action  string `json:"action"`
				Payload string `json:"payload"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			action := strings.TrimSpace(args.Action)
			payload := strings.TrimSpace(args.Payload)
			if action == "" || payload == "" {
				return "", fmt.Errorf("action and payload are required")
			}

			if err := store.RequestApproval(runID, action, payload); err != nil {
				if sink != nil {
					sink.emitVerbose("request_approval", "store.RequestApproval failed: "+err.Error())
				}
			}
			if sink != nil {
				sink.emit(EventKindAwaitingApproval, map[string]any{"action": action, "payload": payload})
			}

			approvalPath := filepath.Join(runDir, "approval.txt")
			_ = os.Remove(approvalPath) // clear any stale verdict

			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-ticker.C:
				}
				data, err := os.ReadFile(approvalPath)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return "", fmt.Errorf("read approval.txt: %w", err)
				}
				verdict := strings.TrimSpace(string(data))
				_ = os.Remove(approvalPath)

				if strings.HasPrefix(strings.ToLower(verdict), "approve") {
					if err := store.Approve(runID); err != nil && sink != nil {
						sink.emitVerbose("request_approval", "store.Approve failed: "+err.Error())
					}
					if sink != nil {
						sink.emit(EventKindApprovalGranted, map[string]any{"action": action})
					}
					return "approved", nil
				}
				// Anything that isn't an explicit approval is a rejection
				// (fail-safe). Strip a leading "reject"/"reject:" for the reason.
				reason := verdict
				if low := strings.ToLower(verdict); strings.HasPrefix(low, "reject") {
					reason = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(verdict[len("reject"):], ":"), " "))
				}
				if reason == "" {
					reason = "rejected by user"
				}
				_ = store.RejectApproval(runID, reason)
				return "", fmt.Errorf("approval REJECTED: %s", reason)
			}
		},
	}
}

// finalizeParamsFromResult translates an orchestrator RunResult into
// jobs.FinalizeParams, the shape Store.Finalize expects. Isolated as
// its own pure function (rather than inlined at the call site), so the
// translation, in particular that a result with no structured
// SubmitResult must leave HasResult false and never invent an Outcome,
// is table-driven-testable without spinning up a whole agent run.
// See jobs.FinalizeState for how these fields earn a terminal state.
func finalizeParamsFromResult(result *orchestrator.RunResult) jobs.FinalizeParams {
	params := jobs.FinalizeParams{
		Termination: string(result.Termination),
	}
	if result.Result != nil {
		params.HasResult = true
		params.Outcome = string(result.Result.Outcome)
		params.Summary = result.Result.Summary
		params.FailureCode = result.Result.FailureCode
	}
	return params
}

// costFuncFor builds an orchestrator.CostFunc that prices a
// completion's token usage using the same table engine.CostFromUsage
// draws on for the costUSD figure runAgentMode reports at the end of a
// run, so the enforced WithCostLimit ceiling and that reported number
// are never computed two different ways. Defined at package scope
// (rather than inline in runAgentMode) because runAgentMode's own
// `model` parameter, the agent's model id, shadows the
// orchestrator/model package import for the whole function body.
func costFuncFor(modelID string) func(model.Usage) float64 {
	return func(u model.Usage) float64 {
		return engine.CostFromUsage(u, modelID)
	}
}

// makeAgentStreamSink returns a stream event handler suitable for live
// terminal output in agent mode. Text deltas are printed directly; tool
// calls emit a bracketed status line when the block completes.
func makeAgentStreamSink() func(model.StreamEvent) {
	return func(ev model.StreamEvent) {
		switch ev.Kind {
		case model.StreamTextDelta:
			fmt.Print(ev.Text)
		case model.StreamBlockDone:
			if ev.Block != nil && ev.Block.Type == model.BlockToolUse {
				fmt.Printf("\n%s [tool: %s]\n", ui.SpinIcon, ev.ToolName)
			}
		}
	}
}
