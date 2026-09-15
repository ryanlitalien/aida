package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/runs"
	"github.com/ryanlitalien/aida/internal/sources"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// goldenSuite is the in-repo regression suite.
type goldenSuite struct {
	Queries []GoldenQuery `yaml:"queries"`
}

// GoldenQuery is one regression test.
type GoldenQuery struct {
	ID       string         `yaml:"id"`
	Question string         `yaml:"question"`
	Profile  string         `yaml:"profile,omitempty"`
	Expected GoldenExpected `yaml:"expected"`
}

// GoldenExpected is the assertion bundle. All fields are optional; only the
// ones set are checked.
//
// Routing assertions (Strategy, *MustInclude) stay deterministic
// because routing is the part of the pipeline that SHOULD be predictable.
// Answer assertions use a natural-language judge (Judge field) evaluated
// by an extra LLM call -- regex matching against LLM-generated text was
// brittle and forced the test author to predict phrasing the model
// chooses for itself. The judge call returns YES or NO with one sentence
// of reasoning, which is printed verbatim on failure.
type GoldenExpected struct {
	Strategy              string   `yaml:"strategy,omitempty"`
	Timeframe             string   `yaml:"timeframe,omitempty"`
	EntitiesMustInclude   []string `yaml:"entities_must_include,omitempty"`
	SourcesMustInclude    []string `yaml:"sources_must_include,omitempty"`
	SourcesMustNotInclude []string `yaml:"sources_must_not_include,omitempty"`
	// AnswerMustMatch is the legacy regex assertion list. Still
	// supported for back-compat but new tests should use Judge.
	AnswerMustMatch []string `yaml:"answer_must_match,omitempty"`
	// Judge is a natural-language criterion the synthesized answer
	// must satisfy. Evaluated by an LLM call ("does this answer
	// satisfy the criterion? yes/no with reason"). Use this for any
	// assertion about the meaning of the answer.
	Judge string `yaml:"judge,omitempty"`
}

// summarizeArtifactsForJudge produces a compact, readable rendering of
// every source result so the judge LLM can verify whether the answer is
// grounded in actual source data. Caps total length to keep the prompt
// from blowing up on chatty sources.
func summarizeArtifactsForJudge(results []sources.SourceResult) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "[source: %s, status: %s] %s\n", r.Source, r.Status, truncateLineForJudge(r.Summary, 200))
		shown := 0
		for _, a := range r.Artifacts {
			fmt.Fprintf(&b, "  - %s %s: %s\n", a.Type, a.ID, truncateLineForJudge(a.Snippet, 300))
			shown++
			if shown >= 8 {
				fmt.Fprintf(&b, "  ... %d more artifact(s) hidden\n", len(r.Artifacts)-shown)
				break
			}
		}
		if b.Len() > 6000 {
			b.WriteString("\n[truncated for prompt length]\n")
			break
		}
	}
	if b.Len() == 0 {
		return "(no source results)"
	}
	return b.String()
}

func truncateLineForJudge(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func newGoldenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "golden",
		Short: "Run or capture golden query regression tests",
	}
	cmd.AddCommand(newGoldenRunCmd())
	cmd.AddCommand(newGoldenAddCmd())
	return cmd
}

func newGoldenRunCmd() *cobra.Command {
	var path string
	var only string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute every golden query and assert routing + answer shape",
		RunE: func(_ *cobra.Command, _ []string) error {
			suite, err := loadGoldenSuite(path)
			if err != nil {
				return err
			}
			ctx := context.Background()
			pass, fail := 0, 0
			for _, q := range suite.Queries {
				if only != "" && q.ID != only {
					continue
				}
				ok, msg := runGolden(ctx, q)
				if ok {
					pass++
					fmt.Printf("%s %s\n", ui.SuccessIcon, q.ID)
				} else {
					fail++
					fmt.Printf("%s %s\n   %s\n", ui.ErrorIcon, q.ID, strings.ReplaceAll(msg, "\n", "\n   "))
				}
			}
			fmt.Printf("\n%d passed, %d failed\n", pass, fail)
			if fail > 0 {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "file", "testdata/golden-queries.yaml", "path to golden suite YAML")
	cmd.Flags().StringVar(&only, "only", "", "run a single golden query by id")
	return cmd
}

func newGoldenAddCmd() *cobra.Command {
	var id string
	var path string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Capture the most recent run as a new golden query",
		RunE: func(_ *cobra.Command, _ []string) error {
			r, err := runs.Latest()
			if err != nil {
				return err
			}
			if r == nil {
				return fmt.Errorf("no runs to capture; run a query first")
			}
			suite, err := loadGoldenSuiteOrEmpty(path)
			if err != nil {
				return err
			}
			if id == "" {
				id = r.ID
			}
			// Build sources_must_include from whichever sources actually
			// produced artifacts in the recent run.
			var sources []string
			for _, p := range r.Phases {
				for _, s := range p.Sources {
					if s.Status != "error" && s.ArtifactCount > 0 {
						sources = append(sources, s.Name)
					}
				}
			}
			suite.Queries = append(suite.Queries, GoldenQuery{
				ID:       id,
				Question: r.Question,
				Profile:  r.Profile,
				Expected: GoldenExpected{
					Strategy:           r.Strategy,
					SourcesMustInclude: sources,
				},
			})
			if dryRunGuard("add golden query", id+" to "+path) {
				return nil
			}
			data, err := yaml.Marshal(suite)
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, data, 0644); err != nil {
				return err
			}
			fmt.Printf("%s Added golden query %q to %s\n", ui.SuccessIcon, id, path)
			fmt.Println("  Edit the file to tighten assertions (answer_must_match, entities_must_include, etc).")
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "id for the new golden query (default: latest run id)")
	cmd.Flags().StringVar(&path, "file", "testdata/golden-queries.yaml", "path to golden suite YAML")
	return cmd
}

func loadGoldenSuite(path string) (*goldenSuite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var s goldenSuite
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &s, nil
}

func loadGoldenSuiteOrEmpty(path string) (*goldenSuite, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		return &goldenSuite{}, nil
	}
	return loadGoldenSuite(path)
}

// runGolden executes one golden query end-to-end and asserts the
// expectations. Returns (pass, message-on-failure).
func runGolden(ctx context.Context, q GoldenQuery) (bool, string) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return false, "config: " + err.Error()
	}
	srcs, _, err := resolveSources()
	if err != nil {
		return false, err.Error()
	}
	apiKey := cfg.GetAPIKey()
	if apiKey == "" && !cfg.Model.OfflineMode {
		return false, "no API key (set ANTHROPIC_API_KEY or run --offline)"
	}
	model := cfg.Model.Primary
	if cfg.Model.OfflineMode {
		model = cfg.Model.Fallback
	}
	client := llm.NewClient(apiKey, model, cfg.Model.OfflineMode)

	intent, err := engine.Parse(ctx, client, q.Question, "", nil)
	if err != nil {
		return false, "parse: " + err.Error()
	}
	classified := engine.Classify(intent)
	resolved := engine.Resolve(classified)
	profile, _ := cfg.ActiveProfileConfig()

	libRegistry, _ := library.LoadRegistry(config.Dir())
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

	// Apply the same LLM routing path the user gets from `aida`. Goldens
	// must validate the actual production pipeline, not a parallel one.
	pastLessons, _ := lessons.FindSimilar(q.Question, 5)
	candidatesForLLM := flattenCandidates(plan)
	_, refinedSources, llmErr := engine.LLMRoute(ctx, client, q.Question, intent, candidatesForLLM, srcs, pastLessons, "", nil, nil)
	if llmErr == nil && len(refinedSources) > 0 {
		plan = rebuildPlanWithRefinedSources(plan, refinedSources)
	}

	execCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	execResult, err := engine.Execute(execCtx, client, plan, intent, resolved, libContext, config.BuildKnownGitHubReposHint(srcs), config.BuildGHRepoAllowlist(srcs), nil)
	if err != nil {
		return false, "execute: " + err.Error()
	}
	answer, err := engine.Synthesize(ctx, client, q.Question, "", libContext, execResult.AllResults, nil)
	if err != nil {
		return false, "synthesize: " + err.Error()
	}

	// --- Assertions ---
	var failures []string

	if exp := q.Expected.Strategy; exp != "" && !strings.EqualFold(string(classified.Strategy), exp) {
		failures = append(failures, fmt.Sprintf("strategy: got %q, want %q", classified.Strategy, exp))
	}
	if exp := q.Expected.Timeframe; exp != "" && !strings.EqualFold(intent.Timeframe, exp) {
		failures = append(failures, fmt.Sprintf("timeframe: got %q, want %q", intent.Timeframe, exp))
	}

	picked := pickedSourceNames(plan)
	for _, want := range q.Expected.SourcesMustInclude {
		if !contains(picked, want) {
			failures = append(failures, fmt.Sprintf("missing source %q (picked: %v)", want, picked))
		}
	}
	for _, banned := range q.Expected.SourcesMustNotInclude {
		if contains(picked, banned) {
			failures = append(failures, fmt.Sprintf("forbidden source %q was picked", banned))
		}
	}

	for _, want := range q.Expected.EntitiesMustInclude {
		if !containsAny(intent.RawEntities, want) {
			failures = append(failures, fmt.Sprintf("entity %q not extracted", want))
		}
	}

	for _, pat := range q.Expected.AnswerMustMatch {
		re, err := regexp.Compile(pat)
		if err != nil {
			failures = append(failures, fmt.Sprintf("bad regex %q: %v", pat, err))
			continue
		}
		if !re.MatchString(answer) {
			snippet := strings.ReplaceAll(answer, "\n", " ")
			if len(snippet) > 240 {
				snippet = snippet[:240] + "..."
			}
			failures = append(failures, fmt.Sprintf("answer did not match /%s/\n     answer was: %s", pat, snippet))
		}
	}

	// LLM-as-judge: ask another LLM call whether the synthesized answer
	// satisfies the natural-language criterion. The judge gets the
	// raw artifacts from the source results too, so it can verify
	// groundedness (was a number actually returned by a source vs
	// invented by the synthesizer) instead of just checking the answer
	// text in isolation.
	if q.Expected.Judge != "" {
		artifactSummary := summarizeArtifactsForJudge(execResult.AllResults)
		ok, judgeReason := llmJudge(ctx, client, q.Question, answer, artifactSummary, q.Expected.Judge)
		if !ok {
			snippet := strings.ReplaceAll(answer, "\n", " ")
			if len(snippet) > 240 {
				snippet = snippet[:240] + "..."
			}
			failures = append(failures, fmt.Sprintf("judge said NO: %s\n     criterion: %s\n     answer was: %s", judgeReason, q.Expected.Judge, snippet))
		}
	}

	if len(failures) > 0 {
		return false, strings.Join(failures, "\n")
	}
	return true, ""
}

// llmJudge asks the configured LLM to verdict an answer against a
// natural-language criterion. The judge sees the question, the answer,
// the raw source artifacts (so it can verify groundedness rather than
// just check answer text), and the criterion. Returns (passed, reason).
// On any LLM failure (network, parse, etc.) returns (false, errorMessage)
// so the golden test surfaces the problem rather than silently passing.
func llmJudge(ctx context.Context, client *llm.Client, question, answer, sourceData, criterion string) (bool, string) {
	const systemPrompt = `You are a strict eval judge for an "agent of agents" CLI. Given a user's question, the system's answer, the raw source data the system had access to, and a natural-language criterion, decide whether the answer satisfies the criterion. Be objective and concise. The source data is the GROUND TRUTH -- if the answer cites a number that appears in the source data, do NOT call it invented. Reply with valid JSON ONLY: {"verdict": "YES" or "NO", "reason": "one short sentence"}.`

	userPrompt := fmt.Sprintf(
		"Question: %s\n\nAnswer:\n%s\n\nSource data the system had access to:\n%s\n\nCriterion: %s\n\nDoes the answer satisfy the criterion? Reply YES or NO with a one-sentence reason.",
		question, answer, sourceData, criterion,
	)
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"verdict": map[string]interface{}{
				"type": "string",
				"enum": []string{"YES", "NO"},
			},
			"reason": map[string]interface{}{"type": "string"},
		},
		"required":             []string{"verdict", "reason"},
		"additionalProperties": false,
	}
	raw, err := client.CompleteJSON(ctx, systemPrompt, userPrompt, schema)
	if err != nil {
		return false, "judge LLM call failed: " + err.Error()
	}
	var v struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return false, "judge response not valid JSON: " + err.Error()
	}
	return v.Verdict == "YES", v.Reason
}

func pickedSourceNames(plan *engine.ExecutionPlan) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range plan.Phases {
		for _, s := range p.Sources {
			if !seen[s.Name] {
				seen[s.Name] = true
				out = append(out, s.Name)
			}
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func containsAny(list []string, want string) bool {
	for _, v := range list {
		if strings.Contains(v, want) {
			return true
		}
	}
	return false
}
