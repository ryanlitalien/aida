package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine"
	"github.com/ryanlitalien/aida/internal/eval"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/runs"
	"github.com/ryanlitalien/aida/internal/sources"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

// sessionState holds accumulated context across queries in a session.
type sessionState struct {
	ID           string
	Profile      string
	StartedAt    time.Time
	QueryCount   int
	TotalCost    float64
	History      []sessionQuery
	PriorResults []sources.SourceResult
}

type sessionQuery struct {
	Question   string   `json:"question"`
	Answer     string   `json:"answer"`
	Sources    []string `json:"sources"`
	CostUSD    float64  `json:"cost_usd"`
	DurationMs int64    `json:"duration_ms"`
}

func newSessionCmd() *cobra.Command {
	var resumeID string

	cmd := &cobra.Command{
		Use:   "session [initial question]",
		Short: "Interactive session with carried context across queries",
		Long: `Starts a session where each follow-up query carries forward context
from prior queries. Results accumulate, and the LLM sees the full
conversation history. Each query still runs the full pipeline.

Type 'exit' or 'quit' to end the session. Press Ctrl+C to abort.
Type 'history' to see past queries in this session.
Type 'cost' to see accumulated costs.

Use --resume <id> to resume a previously saved session.
Use 'aida session list' to see saved sessions.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSession(cmd, args, resumeID)
		},
	}

	cmd.Flags().StringVar(&resumeID, "resume", "", "resume a saved session by ID")

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List saved sessions",
		RunE: func(_ *cobra.Command, _ []string) error {
			return listSessions()
		},
	})

	return cmd
}

func runSession(_ *cobra.Command, args []string, resumeID string) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	srcs, srcOrigin, err := resolveSources()
	if err != nil {
		return err
	}
	_ = srcOrigin

	_, profileName := cfg.ActiveProfileConfig()

	// Set up LLM client
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

	soul, _ := config.LoadSoul()
	soulCtx := soul.ForPrompt()

	// Open brain
	brn, _ := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if brn != nil {
		defer brn.Close()
	}

	// Library
	libRegistry, _ := library.LoadRegistry(config.Dir())
	cwd, _ := os.Getwd()

	var state *sessionState

	// Resume a previously saved session if --resume was provided
	if resumeID != "" {
		loaded, loadErr := loadSession(resumeID)
		if loadErr != nil {
			return fmt.Errorf("failed to resume session: %w", loadErr)
		}
		state = loaded
		fmt.Printf("Resuming session %s (%d prior queries, $%.4f spent)\n", state.ID, state.QueryCount, state.TotalCost)
		if len(state.History) > 0 {
			last := state.History[len(state.History)-1]
			fmt.Printf("Last question: %s\n", last.Question)
		}
	} else {
		state = &sessionState{
			ID:        fmt.Sprintf("session-%s", time.Now().UTC().Format("20060102-150405")),
			Profile:   profileName,
			StartedAt: time.Now(),
		}
		fmt.Printf("Session started (%s, profile: %s)\n", state.ID, profileName)
	}

	fmt.Println("Type your questions. 'exit' to quit, 'history' to review, 'cost' for spending.")
	fmt.Println()

	// If initial question provided, run it first
	if len(args) > 0 {
		question := strings.Join(args, " ")
		runSessionQuery(context.Background(), state, question, cfg, client, brn, srcs, libRegistry, soulCtx, cwd, profileName, isOffline)
	}

	// REPL
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Printf("\n[%d] aida> ", state.QueryCount+1)
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		switch strings.ToLower(input) {
		case "exit", "quit", "q":
			fmt.Printf("\nSession ended. %d queries, $%.4f total cost.\n", state.QueryCount, state.TotalCost)
			saveSession(state)
			return nil
		case "history":
			printSessionHistory(state)
			continue
		case "cost":
			fmt.Printf("Session cost: $%.4f (%d queries)\n", state.TotalCost, state.QueryCount)
			continue
		case "help":
			fmt.Println("Commands: exit, history, cost, help")
			fmt.Println("Everything else is treated as a query.")
			continue
		}

		runSessionQuery(context.Background(), state, input, cfg, client, brn, srcs, libRegistry, soulCtx, cwd, profileName, isOffline)
	}

	fmt.Printf("\nSession ended. %d queries, $%.4f total cost.\n", state.QueryCount, state.TotalCost)
	saveSession(state)
	return nil
}

func runSessionQuery(
	ctx context.Context,
	state *sessionState,
	question string,
	cfg *config.Config,
	client *llm.Client,
	brn *brain.Brain,
	srcs config.Sources,
	libRegistry *library.Registry,
	soulCtx, cwd, profileName string,
	isOffline bool,
) {
	queryStart := time.Now()
	spinner := ui.NewSpinner()

	// Check for task intent first
	if HandleTaskShortcut(question, cfg, profileName) {
		return
	}

	// Parse
	spinner.Start("Parsing...")
	intent, err := engine.Parse(ctx, client, question, soulCtx, nil)
	spinner.Stop(fmt.Sprintf("%s Parsed", ui.SuccessIcon))
	if err != nil {
		fmt.Printf("Parse error: %s\n", err)
		return
	}

	// Promote `#N` literals to action=task / task_action=lookup before the
	// task intercept fires. Parser drift safety net; see PromoteTaskIntent.
	engine.PromoteTaskIntent(intent)

	// Task intercept -- honor prior feedback before short-circuiting.
	if intent.Action == "task" && !taskIntentOverriddenByFeedback(ctx, brn, question) {
		HandleTaskIntent(intent, cfg, profileName)
		return
	}

	// Classify + Resolve
	classified := engine.Classify(intent)
	resolved := engine.Resolve(classified)

	// Library routes
	entityValues := append([]string(nil), intent.RawEntities...)
	for _, e := range classified.Entities {
		entityValues = append(entityValues, e.Raw)
	}
	routeResolution, _ := libRegistry.Resolve(cwd, entityValues)
	if routeResolution == nil {
		routeResolution = &library.Resolution{}
	}
	var libBundle *library.LayerBundle
	if len(routeResolution.Layers) > 0 {
		libBundle = libRegistry.MaterializeLayers(routeResolution.Layers)
	} else {
		libBundle = libRegistry.MaterializeAllAvailable()
	}
	libContext := libBundle.String()

	// Plan
	profile, _ := cfg.ActiveProfileConfig()
	plan := engine.Plan(classified, resolved, srcs, profile, routeResolution.Sources)

	// Route
	pastLessons, _ := lessons.FindSimilar(question, 5)

	// Add session context: prior answers become part of the soul context
	sessionSoulCtx := soulCtx
	if len(state.History) > 0 {
		var sessionCtx strings.Builder
		sessionCtx.WriteString("\n\n## Session Context (prior queries this session)\n\n")
		for _, sq := range state.History {
			sessionCtx.WriteString(fmt.Sprintf("Q: %s\nA: %s\n\n", sq.Question, truncateForContext(sq.Answer, 300)))
		}
		sessionSoulCtx += sessionCtx.String()

		// Enrich the raw query with session context so sub-agents
		// (claude-project) can resolve follow-up references like
		// "is that merged?" → "is ButterStack issue #486 merged?"
		last := state.History[len(state.History)-1]
		intent.RawQuery = question + "\n\nContext from prior conversation:\n" +
			fmt.Sprintf("Prior question: %s\nPrior answer: %s",
				last.Question, truncateForContext(last.Answer, 500))
	}

	// Brain context
	if brn != nil {
		brainEntities := append([]string(nil), intent.RawEntities...)
		sc, _ := brn.Search(ctx, question, brainEntities, 5)
		if sc != nil {
			brainCtx := sc.FormatContextForRouter()
			if brainCtx != "" {
				sessionSoulCtx += "\n\n" + brainCtx
			}
		}
	}

	candidatesForLLM := flattenCandidates(plan)
	var domainKW map[string][]string
	if brn != nil {
		domainKW = brain.LoadDomainKeywords(brn.Path)
	}
	llmRoute, refinedSources, _ := engine.LLMRoute(ctx, client, question, intent, candidatesForLLM, srcs, pastLessons, sessionSoulCtx, domainKW, nil)

	if llmRoute != nil {
		ui.PrintVerbose("Router", fmt.Sprintf("picked %v", llmRoute.Sources))
	}
	if len(refinedSources) > 0 {
		plan = rebuildPlanWithRefinedSources(plan, refinedSources)
	}

	totalPlanned := 0
	for _, ph := range plan.Phases {
		totalPlanned += len(ph.Sources)
	}
	if totalPlanned == 0 {
		fmt.Printf("%s No sources matched this query.\n", ui.WarnIcon)
		return
	}

	// Execute
	execSources := collectPlanSourceNames(plan)
	spinner.Start(fmt.Sprintf("Querying %s...", strings.Join(execSources, ", ")))
	execResult, err := engine.Execute(ctx, client, plan, intent, resolved, libContext, config.BuildKnownGitHubReposHint(srcs), config.BuildGHRepoAllowlist(srcs), nil)
	spinner.Stop(fmt.Sprintf("%s Executed", ui.SuccessIcon))
	if err != nil {
		fmt.Printf("Execute error: %s\n", err)
		return
	}

	// Verify
	if !isOffline {
		verifiedResults := engine.Verify(ctx, client, question, intent, execResult.AllResults)
		var filtered []sources.SourceResult
		for _, vr := range verifiedResults {
			if vr.Confidence >= 0.2 {
				filtered = append(filtered, vr.Result)
			}
		}
		if len(filtered) > 0 {
			execResult.AllResults = filtered
		}
	}

	// Synthesize - include prior results for context
	allResults := execResult.AllResults
	if len(state.PriorResults) > 0 {
		allResults = append(state.PriorResults, allResults...)
	}

	// Build re-execution callback for the validation loop (disabled in offline mode)
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
			reExecResult, err := engine.Execute(ctx, client, plan, guidedIntent, resolved, libContext, config.BuildKnownGitHubReposHint(srcs), config.BuildGHRepoAllowlist(srcs), nil)
			if err != nil {
				return nil, err
			}
			return reExecResult.AllResults, nil
		}
	}

	spinner.Start("Synthesizing...")
	answer, qualityResult, reviewRecords, err := engine.SynthesizeWithValidation(
		ctx, client, question, sessionSoulCtx, libContext, allResults, pastLessons,
		reExecuteFn, 0.05, // tighter budget in session mode
	)

	spinner.Stop(fmt.Sprintf("%s Synthesized", ui.SuccessIcon))
	if err != nil {
		fmt.Printf("Synthesis error: %s\n", err)
		return
	}

	// Persist reviewer records the same way cli/query.go does.
	// Session mode reuses the run-id scheme so eval-runs sync
	// across machines via brain repo and feed the router boost
	// on subsequent queries (in this session or any other).
	// Best-effort - broken eval storage must not regress session
	// answer display.
	if brn != nil && len(reviewRecords) > 0 {
		runID := runs.NewID(queryStart, question)
		evalRun := &brain.EvalRun{
			RunID:            runID,
			Question:         question,
			Model:            client.Model(),
			Profile:          profileName,
			AggregateVerdict: string(eval.AggregateVerdict(reviewRecords)),
			Reviewers:        reviewRecords,
		}
		if err := brain.WriteEvalRun(brn.Path, evalRun); err == nil {
			ui.PrintVerbose("Eval run", fmt.Sprintf("%s (%d reviewers, aggregate=%s)",
				runID, len(reviewRecords), evalRun.AggregateVerdict))
		}
	}

	queryDuration := time.Since(queryStart)
	queryCost := client.TotalCost() - state.TotalCost

	ui.PrintResult(answer)
	fmt.Printf("  (%s, $%.4f)\n", queryDuration.Round(time.Millisecond), queryCost)

	// Update state
	var srcNames []string
	for _, r := range execResult.AllResults {
		srcNames = append(srcNames, r.Source)
	}
	state.History = append(state.History, sessionQuery{
		Question:   question,
		Answer:     answer,
		Sources:    srcNames,
		CostUSD:    queryCost,
		DurationMs: queryDuration.Milliseconds(),
	})
	state.QueryCount++
	state.TotalCost = client.TotalCost()
	state.PriorResults = execResult.AllResults

	// Record to brain
	if brn != nil {
		lesson := &lessons.Lesson{
			Question:        strings.ToLower(strings.TrimSpace(question)),
			Action:          intent.Action,
			Strategy:        string(classified.Strategy),
			Sources:         srcNames,
			PerSourceStatus: make(map[string]lessons.Status),
			AnswerSnippet:   truncateForContext(answer, 500),
		}
		for _, r := range execResult.AllResults {
			lesson.PerSourceStatus[r.Source] = lessons.Status(r.Status)
		}
		if qualityResult != nil {
			lesson.Quality = qualityResult.Quality
			lesson.HasData = qualityResult.HasData
			lesson.QualityReason = qualityResult.Reason
			if qualityResult.Quality <= 2 {
				lesson.RecoverableError = true
			}
		} else if !isOffline {
			scoreAnswer(ctx, client, question, answer, lesson)
		}
		_ = brn.RecordLesson(ctx, lesson)
	}
}

func printSessionHistory(state *sessionState) {
	if len(state.History) == 0 {
		fmt.Println("No queries yet.")
		return
	}
	fmt.Printf("Session: %s (%d queries, $%.4f)\n\n", state.ID, state.QueryCount, state.TotalCost)
	for i, sq := range state.History {
		fmt.Printf("[%d] Q: %s\n    A: %s\n    Sources: %s | Cost: $%.4f\n\n",
			i+1, sq.Question, truncateForContext(sq.Answer, 100), strings.Join(sq.Sources, ", "), sq.CostUSD)
	}
}

func saveSession(state *sessionState) {
	if state.QueryCount == 0 {
		return
	}
	dir := filepath.Join(config.Dir(), "sessions")
	os.MkdirAll(dir, 0755)
	data, _ := json.MarshalIndent(state, "", "  ")
	path := filepath.Join(dir, state.ID+".json")
	os.WriteFile(path, data, 0644)
	ui.PrintVerbose("Session", "saved to "+path)
}

func truncateForContext(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// loadSession reads a saved session JSON from ~/.aida/sessions/<id>.json.
func loadSession(id string) (*sessionState, error) {
	dir := filepath.Join(config.Dir(), "sessions")
	path := filepath.Join(dir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session file not found: %s", path)
	}
	var state sessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("invalid session file: %w", err)
	}
	return &state, nil
}

// listSessions reads all saved session files and displays them sorted
// by most recent first.
func listSessions() error {
	dir := filepath.Join(config.Dir(), "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No saved sessions.")
			return nil
		}
		return err
	}

	type sessionInfo struct {
		ID         string
		StartedAt  time.Time
		QueryCount int
		TotalCost  float64
	}

	var sessions []sessionInfo
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s sessionState
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		sessions = append(sessions, sessionInfo{
			ID:         s.ID,
			StartedAt:  s.StartedAt,
			QueryCount: s.QueryCount,
			TotalCost:  s.TotalCost,
		})
	}

	if len(sessions) == 0 {
		fmt.Println("No saved sessions.")
		return nil
	}

	// Sort by most recent first
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt.After(sessions[j].StartedAt)
	})

	fmt.Printf("%-35s  %-20s  %8s  %8s\n", "ID", "Started", "Queries", "Cost")
	fmt.Printf("%-35s  %-20s  %8s  %8s\n", strings.Repeat("-", 35), strings.Repeat("-", 20), strings.Repeat("-", 8), strings.Repeat("-", 8))
	for _, s := range sessions {
		fmt.Printf("%-35s  %-20s  %8d  $%.4f\n",
			s.ID,
			s.StartedAt.Local().Format("2006-01-02 15:04:05"),
			s.QueryCount,
			s.TotalCost,
		)
	}
	fmt.Printf("\nResume with: aida session --resume <id>\n")
	return nil
}
