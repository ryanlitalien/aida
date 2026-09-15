package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/runs"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

func newSeedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Generate and evaluate golden seed questions for building the brain",
	}
	cmd.AddCommand(newSeedGenerateCmd())
	cmd.AddCommand(newSeedEvalCmd())
	return cmd
}

// ---------- seed generate ----------

const seedGenSystemPrompt = `You are generating golden seed questions for an LLM-powered query router called Aida.

Given a source's name, type, description, and documentation, generate questions that:
1. Test routing - a human reading the question should immediately know THIS source answers it
2. Test answer extraction - the answer MUST be findable in the provided documentation
3. Are specific and concrete, not generic (good: "What 3D file formats does 3d-viewer support?" bad: "Tell me about this project")
4. Cover different aspects of the source (architecture, features, configuration, dependencies)

For each question, also provide the answer extracted from the documentation, and a confidence marker:
- "stable" - answer comes from code/schema/config, unlikely to change
- "semi-stable" - changes occasionally (counts, versions)
- "dynamic" - must be queried live, changes frequently

Return a JSON array. Example:
[
  {"question": "What 3D file formats does 3d-viewer support?", "answer": "STL, OBJ, USDZ, PLY, DAE", "confidence": "stable"},
  {"question": "Does 3d-viewer use SceneKit or RealityKit?", "answer": "SceneKit with Model I/O for loading", "confidence": "stable"}
]

IMPORTANT: Only ask questions whose answers appear in the documentation provided. Do NOT invent facts.`

const seedGenCrossCuttingSystemPrompt = `You are generating cross-cutting golden seed questions for an LLM-powered query router called Aida.

Given a list of all available sources with their descriptions, generate questions that:
1. Span multiple sources (e.g., "Which projects use Docker?")
2. Are deliberately ambiguous to test disambiguation (e.g., "What's that menu bar thing?")
3. Require analytical synthesis across projects (e.g., "What databases do I use?")

For each question, provide the expected answer (what a correct response should say) and mark confidence as "routing-test".

Return a JSON array. Example:
[
  {"question": "Which of my projects use Docker?", "answer": "butter-stack (Docker Compose), perforce-docker (containerized P4)", "confidence": "routing-test"}
]`

type seedQA struct {
	Question   string `json:"question"`
	Answer     string `json:"answer"`
	Confidence string `json:"confidence"`
}

func newSeedGenerateCmd() *cobra.Command {
	var questionsPerSource int
	var crossCutting int
	var outputDir string
	var profileFlag string
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate golden seed questions by crawling all sources",
		Long: "Reads every library source's context file (CLAUDE.md, README.md),\n" +
			"generates routing-distinctive questions via LLM, and writes\n" +
			"golden-seed-questions-{profile}.md and golden-seed-answers-{profile}.md.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runSeedGenerate(questionsPerSource, crossCutting, outputDir, profileFlag)
		},
	}
	cmd.Flags().IntVar(&questionsPerSource, "questions-per-source", 3, "questions to generate per source")
	cmd.Flags().IntVar(&crossCutting, "cross-cutting", 10, "cross-cutting/adversarial questions")
	cmd.Flags().StringVar(&outputDir, "output-dir", "docs", "output directory for seed files")
	cmd.Flags().StringVar(&profileFlag, "profile", "", "profile name for seed files (default: active profile)")
	return cmd
}

func runSeedGenerate(questionsPerSource, crossCutting int, outputDir, profileFlag string) error {
	srcs, _, err := resolveSources()
	if err != nil {
		return err
	}

	// Resolve profile name for file scoping.
	profileName := profileFlag
	if profileName == "" {
		cfg, err := config.LoadConfig()
		if err != nil {
			return err
		}
		_, profileName = cfg.ActiveProfileConfig()
	}

	// Sort source names for deterministic output
	var names []string
	for name := range srcs {
		names = append(names, name)
	}
	sort.Strings(names)

	// Check for existing seed files - we append to them, never overwrite.
	questionsFile := filepath.Join(outputDir, fmt.Sprintf("golden-seed-questions-%s.md", profileName))
	answersFile := filepath.Join(outputDir, fmt.Sprintf("golden-seed-answers-%s.md", profileName))
	existingSources, highestNum := parseSeedSources(questionsFile)

	// Dry run: just list sources
	if dryRun {
		fmt.Printf("Sources: %d\n", len(names))
		fmt.Printf("Questions per source: %d\n", questionsPerSource)
		fmt.Printf("Cross-cutting: %d\n", crossCutting)
		fmt.Printf("Estimated total: %d\n\n", len(names)*questionsPerSource+crossCutting)
		if len(existingSources) > 0 {
			fmt.Printf("Existing sources (will skip): %d\n", len(existingSources))
			fmt.Printf("Existing questions: %d (will continue numbering from %d)\n\n", highestNum, highestNum+1)
		}

		for _, name := range names {
			src := srcs[name]
			ctx := "(no context file)"
			if src.Context != "" {
				doc, _ := src.LoadContextFile()
				if doc != "" {
					ctx = fmt.Sprintf("%s (%d chars)", src.Context, len(doc))
				} else {
					ctx = fmt.Sprintf("%s (file missing)", src.Context)
				}
			}
			skip := ""
			if existingSources[name] {
				skip = " [exists]"
			}
			fmt.Printf("  %-35s %-15s %s%s\n", name, src.Type, ctx, skip)
		}
		return nil
	}

	// Set up LLM client
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}
	apiKey := cfg.GetAPIKey()
	if apiKey == "" {
		return fmt.Errorf("no API key (set ANTHROPIC_API_KEY)")
	}
	client := llm.NewClient(apiKey, cfg.Model.Primary, cfg.Model.OfflineMode)
	if cfg.Model.Stages != nil {
		client.SetStageModels(cfg.Model.Stages)
	}
	ctx := context.Background()

	// Generate per-source questions - skip sources already in seed files.
	allQA := make(map[string][]seedQA)
	qNum := 0
	skipped := 0
	schema := seedQASchema(questionsPerSource)

	for _, name := range names {
		if existingSources[name] {
			skipped++
			continue
		}

		src := srcs[name]
		contextDoc, _ := src.LoadContextFile()
		if contextDoc == "" {
			// Try folder structure inference for sources without docs
			contextDoc = brain.InferFolderStructure(src.Path)
			if contextDoc == "(no path configured)" {
				ui.PrintVerbose("Seed generate", fmt.Sprintf("skipping %s (no context)", name))
				continue
			}
		}

		userPrompt := fmt.Sprintf(
			"Source name: %s\nType: %s\nDescription: %s\nEntities: %s\nCapabilities: %s\n\nDocumentation:\n%s\n\nGenerate exactly %d questions.",
			name, src.Type, src.Description,
			strings.Join(src.Entities, ", "),
			strings.Join(src.Capabilities, ", "),
			truncateForPrompt(contextDoc, 6000),
			questionsPerSource,
		)

		raw, err := client.CompleteJSON(ctx, seedGenSystemPrompt, userPrompt, schema)
		if err != nil {
			ui.PrintVerbose("Seed generate", fmt.Sprintf("LLM error for %s: %s", name, err))
			continue
		}

		var qas []seedQA
		if err := json.Unmarshal([]byte(raw), &qas); err != nil {
			ui.PrintVerbose("Seed generate", fmt.Sprintf("parse error for %s: %s", name, err))
			continue
		}

		allQA[name] = qas
		qNum += len(qas)
		fmt.Printf("%s %s (%d questions)\n", ui.SuccessIcon, name, len(qas))
	}

	// Generate cross-cutting questions only when there are no existing
	// ones (i.e. this is the first run or the file was empty).
	var crossQA []seedQA
	if crossCutting > 0 && !existingSources["cross-cutting"] {
		var sourceList strings.Builder
		for _, name := range names {
			src := srcs[name]
			fmt.Fprintf(&sourceList, "- %s (%s): %s\n", name, src.Type, src.Description)
		}

		crossSchema := seedQASchema(crossCutting)
		userPrompt := fmt.Sprintf(
			"Available sources:\n%s\nGenerate exactly %d cross-cutting questions.",
			sourceList.String(), crossCutting,
		)

		raw, err := client.CompleteJSON(ctx, seedGenCrossCuttingSystemPrompt, userPrompt, crossSchema)
		if err != nil {
			ui.PrintVerbose("Seed generate", "cross-cutting LLM error: "+err.Error())
		} else {
			if err := json.Unmarshal([]byte(raw), &crossQA); err != nil {
				ui.PrintVerbose("Seed generate", "cross-cutting parse error: "+err.Error())
			} else {
				qNum += len(crossQA)
				fmt.Printf("%s Cross-cutting (%d questions)\n", ui.SuccessIcon, len(crossQA))
			}
		}
	}

	if qNum == 0 {
		fmt.Printf("\n%s No new questions to generate (%d sources already seeded)\n", ui.SuccessIcon, skipped)
		return nil
	}

	// Build new content to append.
	var qBuf, aBuf strings.Builder
	num := highestNum + 1

	for _, name := range names {
		qas, ok := allQA[name]
		if !ok || len(qas) == 0 {
			continue
		}
		qBuf.WriteString(fmt.Sprintf("### %s\n\n", name))
		aBuf.WriteString(fmt.Sprintf("### %s\n\n", name))
		for _, qa := range qas {
			qBuf.WriteString(fmt.Sprintf("%d. %s\n", num, qa.Question))
			aBuf.WriteString(fmt.Sprintf("%d. `[%s]` %s\n\n", num, qa.Confidence, qa.Answer))
			num++
		}
		qBuf.WriteString("\n")
	}

	if len(crossQA) > 0 {
		qBuf.WriteString("### Cross-cutting / Multi-source / Adversarial\n\n")
		aBuf.WriteString("### Cross-cutting / Multi-source / Adversarial\n\n")
		for _, qa := range crossQA {
			qBuf.WriteString(fmt.Sprintf("%d. %s\n", num, qa.Question))
			aBuf.WriteString(fmt.Sprintf("%d. `[%s]` %s\n\n", num, qa.Confidence, qa.Answer))
			num++
		}
	}

	// Write output - append to existing files or create new ones.
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return err
	}

	if highestNum > 0 {
		// Append to existing files.
		appendSection := fmt.Sprintf("\n---\n\n## Generated on %s (%d new questions)\n\n",
			time.Now().Format("2006-01-02"), qNum)

		existingQ, _ := os.ReadFile(questionsFile)
		if err := os.WriteFile(questionsFile, []byte(string(existingQ)+appendSection+qBuf.String()), 0644); err != nil {
			return err
		}
		existingA, _ := os.ReadFile(answersFile)
		if err := os.WriteFile(answersFile, []byte(string(existingA)+appendSection+aBuf.String()), 0644); err != nil {
			return err
		}
	} else {
		// Fresh files - write with header.
		var fullQ, fullA strings.Builder
		fullQ.WriteString("# Golden Seed Questions\n\n")
		fullQ.WriteString(fmt.Sprintf("%d questions generated by `aida seed generate` on %s.\n\n---\n\n",
			qNum, time.Now().Format("2006-01-02")))
		fullQ.WriteString(qBuf.String())

		fullA.WriteString("# Golden Seed Answers\n\n")
		fullA.WriteString("Answers to golden-seed-questions.md. Numbered to match.\n\n")
		fullA.WriteString("## Confidence markers\n\n")
		fullA.WriteString("- `[stable]` - from code/schema/config, rarely changes.\n")
		fullA.WriteString("- `[semi-stable]` - changes occasionally.\n")
		fullA.WriteString("- `[dynamic]` - must be queried live.\n")
		fullA.WriteString("- `[routing-test]` - adversarial/ambiguous.\n\n---\n\n")
		fullA.WriteString(aBuf.String())

		if err := os.WriteFile(questionsFile, []byte(fullQ.String()), 0644); err != nil {
			return err
		}
		if err := os.WriteFile(answersFile, []byte(fullA.String()), 0644); err != nil {
			return err
		}
	}

	fmt.Printf("\n%s Generated %d new questions (skipped %d existing sources, continuing from Q%d)\n",
		ui.SuccessIcon, qNum, skipped, highestNum+1)
	fmt.Printf("  Questions: %s\n", questionsFile)
	fmt.Printf("  Answers:   %s\n", answersFile)
	return nil
}

// parseSeedSources reads an existing golden-seed-questions.md file and
// returns the set of source names that already have questions (extracted
// from ### headers) and the highest question number found. Returns an
// empty map and 0 if the file doesn't exist or can't be parsed.
func parseSeedSources(questionsFile string) (map[string]bool, int) {
	data, err := os.ReadFile(questionsFile)
	if err != nil {
		return map[string]bool{}, 0
	}
	content := string(data)

	// Extract source names from ### headers. Handles both formats:
	//   ### source-name
	//   ### source-name (Q1–20)
	sources := map[string]bool{}
	headerRe := regexp.MustCompile(`(?m)^###\s+(.+?)(?:\s+\(Q\d+|$)`)
	for _, match := range headerRe.FindAllStringSubmatch(content, -1) {
		name := strings.TrimSpace(match[1])
		// Normalize: cross-cutting sections get a canonical key.
		lower := strings.ToLower(name)
		if strings.Contains(lower, "cross-cutting") || strings.Contains(lower, "adversarial") || strings.Contains(lower, "analytical") {
			sources["cross-cutting"] = true
		} else {
			sources[name] = true
		}
	}

	// Find the highest numbered question.
	highest := 0
	numRe := regexp.MustCompile(`(?m)^(\d+)\.\s+`)
	for _, match := range numRe.FindAllStringSubmatch(content, -1) {
		n, _ := strconv.Atoi(match[1])
		if n > highest {
			highest = n
		}
	}

	return sources, highest
}

func seedQASchema(_ int) map[string]interface{} {
	return map[string]interface{}{
		"type": "array",
		"items": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"question":   map[string]interface{}{"type": "string"},
				"answer":     map[string]interface{}{"type": "string"},
				"confidence": map[string]interface{}{"type": "string", "enum": []string{"stable", "semi-stable", "dynamic", "routing-test"}},
			},
			"required":             []string{"question", "answer", "confidence"},
			"additionalProperties": false,
		},
	}
}

func truncateForPrompt(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-13] + "\n[truncated]"
}

// ---------- seed eval ----------

func newSeedEvalCmd() *cobra.Command {
	var only int
	var inputDir string
	var batch int
	var profileFlag string
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Evaluate aida against golden seed questions and record feedback",
		Long: "Runs each golden seed question through the full aida pipeline,\n" +
			"compares the answer to the expected answer via LLM-as-judge,\n" +
			"and records thumbs-up/down/note feedback to build the brain.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runSeedEval(only, inputDir, batch, profileFlag)
		},
	}
	cmd.Flags().IntVar(&only, "only", 0, "run only question number N")
	cmd.Flags().StringVar(&inputDir, "input-dir", "docs", "directory containing seed files")
	cmd.Flags().IntVar(&batch, "batch", 0, "run N questions then stop (0 = all)")
	cmd.Flags().StringVar(&profileFlag, "profile", "", "profile name for seed files (default: active profile)")
	return cmd
}

// seedPair holds a parsed question/answer pair from the seed files.
type seedPair struct {
	Num        int
	Question   string
	Answer     string
	Confidence string // stable, semi-stable, dynamic, routing-test
}

func runSeedEval(only int, inputDir string, batch int, profileFlag string) error {
	// Resolve profile name for file scoping.
	profileName := profileFlag
	if profileName == "" {
		cfg, err := config.LoadConfig()
		if err != nil {
			return err
		}
		_, profileName = cfg.ActiveProfileConfig()
	}

	pairs, err := parseSeedFilesForProfile(inputDir, profileName)
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Printf("Seed files: %s/ (profile: %s)\n", inputDir, profileName)
		fmt.Printf("Questions: %d\n", len(pairs))

		// Count by confidence
		counts := map[string]int{}
		for _, p := range pairs {
			counts[p.Confidence]++
		}
		fmt.Println("\nBy confidence:")
		for _, c := range []string{"stable", "semi-stable", "dynamic", "routing-test"} {
			if n := counts[c]; n > 0 {
				fmt.Printf("  [%s]: %d\n", c, n)
			}
		}

		// Check source availability
		srcs, _, _ := resolveSources()
		fmt.Printf("\nAvailable sources: %d\n", len(srcs))
		return nil
	}

	// Set up pipeline components
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}
	srcs, _, err := resolveSources()
	if err != nil {
		return err
	}
	apiKey := cfg.GetAPIKey()
	if apiKey == "" {
		return fmt.Errorf("no API key (set ANTHROPIC_API_KEY)")
	}
	client := llm.NewClient(apiKey, cfg.Model.Primary, cfg.Model.OfflineMode)
	if cfg.Model.Stages != nil {
		client.SetStageModels(cfg.Model.Stages)
	}
	profile, _ := cfg.ActiveProfileConfig()
	libRegistry, _ := library.LoadRegistry(config.Dir())
	domainKW := brain.LoadDomainKeywords(cfg.BrainPath())

	ctx := context.Background()
	thumbsUp, thumbsDown, notes, skipped := 0, 0, 0, 0

	for _, pair := range pairs {
		if only > 0 && pair.Num != only {
			continue
		}
		if batch > 0 && (thumbsUp+thumbsDown+notes+skipped) >= batch {
			break
		}

		fmt.Printf("Q%d: %s\n", pair.Num, pair.Question)

		// Run full pipeline
		answer, run, picked, err := runSeedPipeline(ctx, cfg, client, srcs, profile, libRegistry, domainKW, pair.Question)
		if err != nil {
			fmt.Printf("  %s error: %s\n", ui.ErrorIcon, err)
			skipped++
			continue
		}

		// LLM-as-judge
		verdict, reason := seedJudge(ctx, client, pair.Question, pair.Answer, answer)
		fmt.Printf("  Sources: %s\n", strings.Join(picked, ", "))

		switch verdict {
		case "YES":
			_ = appendFeedback(run, lessons.FeedbackThumbsUp, reason)
			thumbsUp++
		case "NO":
			_ = appendFeedback(run, lessons.FeedbackThumbsDown, reason)
			thumbsDown++
		case "PARTIAL":
			_ = appendFeedback(run, lessons.FeedbackNote, reason)
			notes++
		default:
			fmt.Printf("  %s unknown verdict %q, skipping feedback\n", ui.WarnIcon, verdict)
			skipped++
		}

		// Checkpoint every 20 questions
		total := thumbsUp + thumbsDown + notes + skipped
		if total > 0 && total%20 == 0 {
			fmt.Printf("\n--- Checkpoint at %d questions ---\n", total)
			fmt.Printf("  %s %d thumbs-up  %s %d thumbs-down  %s %d notes  %d skipped\n",
				ui.SuccessIcon, thumbsUp, ui.ErrorIcon, thumbsDown, ui.WarnIcon, notes, skipped)
			pct := float64(thumbsUp) / float64(total) * 100
			fmt.Printf("  Pass rate: %.0f%%\n", pct)
			fmt.Println("---")
			fmt.Println()
		}
	}

	// Final summary
	total := thumbsUp + thumbsDown + notes + skipped
	fmt.Printf("\n%s Eval complete: %d questions\n", ui.SuccessIcon, total)
	fmt.Printf("  %s %d thumbs-up (%.0f%%)\n", ui.SuccessIcon, thumbsUp, pct(thumbsUp, total))
	fmt.Printf("  %s %d thumbs-down (%.0f%%)\n", ui.ErrorIcon, thumbsDown, pct(thumbsDown, total))
	fmt.Printf("  %s %d notes (%.0f%%)\n", ui.WarnIcon, notes, pct(notes, total))
	if skipped > 0 {
		fmt.Printf("  %d skipped\n", skipped)
	}

	_, pName := cfg.ActiveProfileConfig()
	brn, brainErr := brain.Open(cfg.BrainPath(), pName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if brainErr == nil {
		defer brn.Close()
		stats := brn.GetStats()
		fmt.Printf("\nBrain: %d total lessons\n", stats.LessonCount)
	}

	return nil
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

// runSeedPipeline runs the full aida pipeline for a single question and
// returns the synthesized answer, the saved run, picked source names, and any error.
func runSeedPipeline(
	ctx context.Context,
	cfg *config.Config,
	client *llm.Client,
	srcs config.Sources,
	profile *config.Profile,
	libRegistry *library.Registry,
	domainKW brain.DomainKeywords,
	question string,
) (answer string, run *runs.Run, picked []string, err error) {
	startedAt := time.Now()

	intent, err := engine.Parse(ctx, client, question, "", nil)
	if err != nil {
		return "", nil, nil, fmt.Errorf("parse: %w", err)
	}
	classified := engine.Classify(intent)
	resolved := engine.Resolve(classified)

	cwd, _ := os.Getwd()
	entityValues := append([]string(nil), intent.RawEntities...)
	for _, e := range classified.Entities {
		entityValues = append(entityValues, e.Raw)
	}
	routeRes, _ := libRegistry.Resolve(cwd, entityValues)
	var libBundle *library.LayerBundle
	if routeRes != nil && len(routeRes.Layers) > 0 {
		libBundle = libRegistry.MaterializeLayers(routeRes.Layers)
	} else {
		libBundle = libRegistry.MaterializeAllAvailable()
	}
	libContext := libBundle.String()

	var routedSources []string
	if routeRes != nil {
		routedSources = routeRes.Sources
	}
	plan := engine.Plan(classified, resolved, srcs, profile, routedSources)

	pastLessons, _ := lessons.FindSimilar(question, 5)
	candidatesForLLM := flattenCandidates(plan)
	_, refinedSources, llmErr := engine.LLMRoute(ctx, client, question, intent, candidatesForLLM, srcs, pastLessons, "", domainKW, nil)
	if llmErr == nil && len(refinedSources) > 0 {
		plan = rebuildPlanWithRefinedSources(plan, refinedSources)
	}

	picked = pickedSourceNames(plan)

	execCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	execResult, err := engine.Execute(execCtx, client, plan, intent, resolved, libContext, config.BuildKnownGitHubReposHint(srcs), config.BuildGHRepoAllowlist(srcs), nil)
	if err != nil {
		return "", nil, picked, fmt.Errorf("execute: %w", err)
	}

	answer, err = engine.Synthesize(ctx, client, question, "", libContext, execResult.AllResults, nil)
	if err != nil {
		return "", nil, picked, fmt.Errorf("synthesize: %w", err)
	}

	totalDuration := time.Since(startedAt)
	_, profileName := cfg.ActiveProfileConfig()

	// Build and save run log
	run = buildSeedRun(question, cwd, profileName, intent, classified, resolved, routeRes, libBundle, plan, execResult, answer, startedAt, totalDuration)
	if _, saveErr := runs.Save(run); saveErr != nil {
		ui.PrintVerbose("Seed eval", "run save error: "+saveErr.Error())
	}

	return answer, run, picked, nil
}

// buildSeedRun constructs a Run record from pipeline results.
func buildSeedRun(
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

	for _, pr := range execResult.PhaseResults {
		ph := runs.PhaseRun{Name: pr.Phase}
		for _, sr := range pr.Results {
			ph.Sources = append(ph.Sources, runs.SourceRun{
				Name:          sr.Source,
				Command:       truncateForPrompt(sr.Command, 500),
				Status:        sr.Status,
				Summary:       truncateForPrompt(sr.Summary, 1500),
				ArtifactCount: len(sr.Artifacts),
				DurationMs:    sr.Duration.Milliseconds(),
			})
		}
		r.Phases = append(r.Phases, ph)
	}
	return r
}

// seedJudge uses an LLM to compare aida's answer against the expected answer.
// Returns verdict (YES/NO/PARTIAL) and reason.
func seedJudge(ctx context.Context, client *llm.Client, question, expectedAnswer, actualAnswer string) (string, string) {
	const systemPrompt = `You are evaluating whether an AI system's answer matches an expected answer for a golden seed test.

Compare the actual answer against the expected answer. Focus on factual correctness, not phrasing.

Return JSON with:
- verdict: "YES" if the key facts match (doesn't need word-for-word), "NO" if factually wrong or completely missed, "PARTIAL" if some facts are right but important ones are missing
- reason: one sentence explaining your verdict

Be lenient on phrasing differences but strict on factual accuracy.`

	userPrompt := fmt.Sprintf(
		"Question: %s\n\nExpected answer: %s\n\nActual answer: %s\n\nDoes the actual answer match the expected answer?",
		question, expectedAnswer, truncateForPrompt(actualAnswer, 2000),
	)
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"verdict": map[string]interface{}{
				"type": "string",
				"enum": []string{"YES", "NO", "PARTIAL"},
			},
			"reason": map[string]interface{}{"type": "string"},
		},
		"required":             []string{"verdict", "reason"},
		"additionalProperties": false,
	}

	raw, err := client.CompleteJSON(ctx, systemPrompt, userPrompt, schema)
	if err != nil {
		return "NO", "judge LLM call failed: " + err.Error()
	}
	var v struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "NO", "judge response not valid JSON: " + err.Error()
	}
	return v.Verdict, v.Reason
}

// parseSeedFilesForProfile reads profile-scoped seed files (e.g.
// golden-seed-questions-work.md) and pairs them by number.
func parseSeedFilesForProfile(dir, profile string) ([]seedPair, error) {
	qPath := filepath.Join(dir, fmt.Sprintf("golden-seed-questions-%s.md", profile))
	aPath := filepath.Join(dir, fmt.Sprintf("golden-seed-answers-%s.md", profile))
	return parseSeedFilePair(qPath, aPath)
}

// parseSeedFiles reads golden-seed-questions.md and golden-seed-answers.md
// from the given directory and pairs them by number.
func parseSeedFiles(dir string) ([]seedPair, error) {
	qPath := filepath.Join(dir, "golden-seed-questions.md")
	aPath := filepath.Join(dir, "golden-seed-answers.md")
	return parseSeedFilePair(qPath, aPath)
}

// parseSeedFilePair reads a questions file and an answers file and pairs them by number.
func parseSeedFilePair(qPath, aPath string) ([]seedPair, error) {
	qData, err := os.ReadFile(qPath)
	if err != nil {
		return nil, fmt.Errorf("reading questions: %w", err)
	}
	aData, err := os.ReadFile(aPath)
	if err != nil {
		return nil, fmt.Errorf("reading answers: %w", err)
	}

	questions := parseNumberedLines(string(qData))
	answers := parseNumberedAnswers(string(aData))

	var pairs []seedPair
	for num, q := range questions {
		a, ok := answers[num]
		if !ok {
			continue
		}
		pairs = append(pairs, seedPair{
			Num:        num,
			Question:   q,
			Answer:     a.text,
			Confidence: a.confidence,
		})
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Num < pairs[j].Num })
	return pairs, nil
}

// parseNumberedLines extracts "N. text" lines from markdown.
func parseNumberedLines(content string) map[int]string {
	re := regexp.MustCompile(`(?m)^(\d+)\.\s+(.+)$`)
	result := make(map[int]string)
	for _, match := range re.FindAllStringSubmatch(content, -1) {
		num, _ := strconv.Atoi(match[1])
		result[num] = strings.TrimSpace(match[2])
	}
	return result
}

type parsedAnswer struct {
	text       string
	confidence string
}

// parseNumberedAnswers extracts "N. `[marker]` text" lines from the answers file.
// Handles multi-line answers by collecting text until the next numbered line.
func parseNumberedAnswers(content string) map[int]parsedAnswer {
	result := make(map[int]parsedAnswer)
	lines := strings.Split(content, "\n")
	numRe := regexp.MustCompile(`^(\d+)\.\s+` + "`" + `\[([^\]]+)\]` + "`" + `\s*(.*)`)

	var currentNum int
	var currentConf string
	var currentLines []string

	flush := func() {
		if currentNum > 0 {
			result[currentNum] = parsedAnswer{
				text:       strings.TrimSpace(strings.Join(currentLines, " ")),
				confidence: currentConf,
			}
		}
	}

	for _, line := range lines {
		if m := numRe.FindStringSubmatch(line); m != nil {
			flush()
			currentNum, _ = strconv.Atoi(m[1])
			currentConf = m[2]
			currentLines = []string{m[3]}
		} else if currentNum > 0 {
			trimmed := strings.TrimSpace(line)
			// Stop at section headers or blank lines between entries
			if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "---") {
				flush()
				currentNum = 0
				currentLines = nil
			} else if trimmed != "" {
				currentLines = append(currentLines, trimmed)
			}
		}
	}
	flush()

	return result
}
