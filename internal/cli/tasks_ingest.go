package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/adapters"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/ui"
)

// sourceContextLimit caps the inline excerpt we prepend to each
// auto-solve agent prompt. ~3K chars ≈ ~750 tokens - modest cost per
// task, large enough to carry pre-extracted action items and a
// discussion summary, small enough that the full transcript stays
// in the typed-memory event for retrieval rather than the prompt.
const sourceContextLimit = 3000

// ingestSourceHash returns a short, stable identifier for a
// (source-ref, prose) pair. Used as the dedupe key on every
// ingested task tag and as the typed-memory event key for the
// source content. Same prose from the same source → same hash.
// Edited prose → fresh hash → fresh ingest.
func ingestSourceHash(srcRef, prose string) string {
	h := sha256.Sum256([]byte(srcRef + "\x00" + prose))
	return hex.EncodeToString(h[:6])
}

// sourceTagPrefix is the tag applied to every ingested task carrying
// its source-hash. Visible to humans (`aida tasks --tag source-hash:abc`)
// and read by the idempotency check before re-ingest.
const sourceTagPrefix = "source-hash:"

// excerptForAutoSolve picks the first sourceContextLimit chars of
// prose as the inline context for an auto-solve agent prompt. When
// truncation happens, appends a pointer to the typed-memory event
// id so the agent (or a curious human reading the run log) can fetch
// the rest. Empty input returns empty string.
func excerptForAutoSolve(prose, memoryID string) string {
	prose = strings.TrimSpace(prose)
	if prose == "" {
		return ""
	}
	if len(prose) <= sourceContextLimit {
		return prose
	}
	cut := prose[:sourceContextLimit]
	// Don't break mid-line; back up to the last newline if one is
	// reasonably close.
	if i := strings.LastIndex(cut, "\n"); i > sourceContextLimit-300 {
		cut = cut[:i]
	}
	if memoryID != "" {
		cut += fmt.Sprintf("\n\n[truncated - full source in typed-memory event %q; call aida-brain to read]", memoryID)
	} else {
		cut += "\n\n[truncated]"
	}
	return cut
}

// buildAutoSolveQuestion produces the agent prompt for one task,
// composing the source context (when present) with the task body.
// Empty context falls back to body-only for backward compat.
//
// Header wording: avoid "Task:" - aida's parser classifies queries
// starting with task-shaped verbs as action=task and (without the
// --agent bypass shipped alongside this) short-circuits the agent
// loop. Use "Action item:" instead - neutral enough that the
// classifier routes the prompt into investigation/lookup, plus the
// surrounding "Investigate as an agent:" framing biases toward the
// research path.
func buildAutoSolveQuestion(srcRef, sourceExcerpt, taskTitle, taskBody string) string {
	body := strings.TrimSpace(taskBody)
	if body == "" {
		body = taskTitle
	}
	if strings.TrimSpace(sourceExcerpt) == "" {
		// Even body-only mode prepends the "investigate" framing so
		// the agent doesn't treat the request as a one-line lookup.
		return "Investigate as an agent and produce a draft response:\n" + body
	}
	return fmt.Sprintf(
		"Investigate as an agent and produce a draft response. "+
			"Context from %s follows; the action item is at the end.\n\n"+
			"Context:\n%s\n\nAction item:\n%s",
		srcRef, strings.TrimSpace(sourceExcerpt), body,
	)
}

// appendDestinationInstructions wraps the base auto-solve prompt with
// destination-specific guidance for the agent. The agent's final
// output writes `artifact_url` to the manifest via `aida jobs set-artifact-url`
// when a typed deliverable was produced. Empty/unknown destination
// types leave the prompt unchanged (universal Copy/Save fallback).
//
// v1 (issue #58) is half-auto for github-pr - the agent produces a
// title + body block; the modal opens GitHub's new-PR page pre-filled.
// Full-auto (gh pr create from a worktree) is gated behind a future
// `auto_create: true` flag on the destination. Callers currently always
// pass a nil dest: the partner registry that used to resolve a task's
// tags to a typed destination was removed 2026-09-12 with no replacement
// (docs/plan-remove-partners.md), so this switch is dead in production
// until a registry-free resolution mechanism exists; it's kept (and
// tested) because *dest is still a valid, directly-constructible type.
func appendDestinationInstructions(question string, runID string, dest *jobs.Destination) string {
	if dest == nil {
		return question
	}
	switch dest.Type {
	case jobs.DestinationTypeGitHubPR:
		repo := dest.Repo
		if repo == "" {
			repo = "<repo>"
		}
		return question + fmt.Sprintf(`

DESTINATION: github-pr in %s

Produce a draft PR description in this exact shape so the web modal
can render an "Open PR" link that pre-fills GitHub's new-PR page:

  Title: <one-line PR title>

  <PR body - markdown, ~3-8 paragraphs covering problem, change,
   test plan>

Do NOT run `+"`gh pr create`"+` or push a branch. The user reviews the
draft in the modal and clicks "Open PR" to open GitHub's new-PR page
with title + body pre-filled (half-auto v1).

If you do produce a real PR URL via some other channel (e.g. the user
asked for full-auto explicitly in the task body), run:

  aida jobs set-artifact-url --run-id %s --url <url>

so the modal renders it as the primary action instead of the GitHub
new-PR link.`, repo, runID)
	case jobs.DestinationTypeSlackMessage:
		channel := dest.Channel
		channelHint := ""
		if channel != "" {
			channelHint = fmt.Sprintf(" (target channel: %s)", channel)
		}
		return question + fmt.Sprintf(`

DESTINATION: slack-message%s

Produce a draft Slack message - plain text, no markdown formatting,
no headings. Slack will not render `+"`*bold*`"+` or `+"`# heading`"+`
the way the markdown copy path expects. Keep it short (1-4 short
paragraphs), conversational, and ready to paste into Slack as-is.`, channelHint)
	}
	// Unknown type: leave the prompt untouched.
	return question
}

// newTasksIngestCmd builds `aida tasks ingest` - generic task
// ingestion from any IngestSource adapter (stdin, file, future
// notion/slack/github-issue/cron). The command is intentionally
// thin over the adapters package so adding a new origin is a
// new adapter, not new ingestion plumbing.
//
// Behavior:
//  1. Pick an adapter (stdin or --from-file).
//  2. Read prose via adapter.Read(ctx).
//  3. LLM-extract a task list using llm.TaskExtractionPrompt.
//  4. For each task, brain.AddTask + (when --plan) brain.NewExecPlan.
//
// Why not a "meeting" subcommand? Per the platform-not-use-case
// memory, aida meeting becomes a thin wrapper that calls ingest
// with the Notion adapter. Same for aida slack, aida gh-issue, etc.
func newTasksIngestCmd() *cobra.Command {
	var fromFile string
	var fromStdin bool
	var fromNotion string
	var dryRun bool
	var withPlans bool
	var autoSolve bool
	var assumeYes bool
	var force bool
	var tags []string

	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Extract tasks from a prose blob via an ingestion adapter",
		Long: "Reads prose from an IngestSource and runs an LLM extraction pass\n" +
			"to produce a flat task list, then creates each task in the brain.\n" +
			"Use --dry-run to preview without writing.\n\n" +
			"Adapters today: stdin, file, notion (via claude MCP). Future\n" +
			"adapters (slack, github-issue, email) are new files in\n" +
			"internal/adapters/, not changes to this command.\n\n" +
			"Idempotent re-runs: every ingestion writes a `source-hash:<short>`\n" +
			"tag on its tasks. Re-running with the same source content (same\n" +
			"file bytes / same Notion page revision) is detected up front and\n" +
			"refuses to create duplicate tasks. Pass --force to ingest anyway.\n\n" +
			"With --auto-solve and --plan, each extracted task is handed to\n" +
			"`aida --agent` and the draft answer is appended to the task's\n" +
			"exec-plan as a decision-log entry - the meeting → tasks →\n" +
			"drafts → review loop. Use --yes to skip the confirmation\n" +
			"prompt before auto-solve starts.\n\n" +
			"Examples:\n" +
			"  aida tasks ingest --from-file standup-2026-05-05.md --plan\n" +
			"  cat thread.txt | aida tasks ingest --from-stdin --tag triage\n" +
			"  aida tasks ingest --from-notion 'Weekly partner meeting' --plan --auto-solve",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTasksIngest(cmd.Context(), tasksIngestOpts{
				FromFile:   fromFile,
				FromStdin:  fromStdin,
				FromNotion: fromNotion,
				DryRun:     dryRun,
				WithPlans:  withPlans,
				AutoSolve:  autoSolve,
				AssumeYes:  assumeYes,
				Force:      force,
				ExtraTags:  tags,
			})
		},
	}
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read prose from this file path")
	cmd.Flags().BoolVar(&fromStdin, "from-stdin", false, "read prose from stdin (e.g. piped from another command)")
	cmd.Flags().StringVar(&fromNotion, "from-notion", "", "Notion page reference (URL, page id, or title) - fetched via claude MCP")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "extract + print tasks without creating them in brain")
	cmd.Flags().BoolVar(&withPlans, "plan", false, "also create an exec-plan for each extracted task")
	cmd.Flags().BoolVar(&autoSolve, "auto-solve", false, "after creating tasks, run `aida --agent` on each and append the draft answer to the exec-plan (requires --plan)")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip the auto-solve confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false, "ingest even when the source-hash matches a prior ingestion (creates duplicate tasks)")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "tag to add to every extracted task (repeatable)")
	return cmd
}

// tasksIngestOpts groups runTasksIngest arguments to keep the
// function signature scannable as more flags accrete.
type tasksIngestOpts struct {
	FromFile   string
	FromStdin  bool
	FromNotion string
	DryRun     bool
	WithPlans  bool
	AutoSolve  bool
	AssumeYes  bool
	Force      bool
	ExtraTags  []string
}

// extractedTask matches the shape llm.TaskExtractionSchema enforces.
type extractedTask struct {
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Owner string   `json:"owner,omitempty"`
	Tags  []string `json:"tags,omitempty"`
}

func runTasksIngest(ctx context.Context, opts tasksIngestOpts) error {
	// Pick exactly one adapter - explicit-or-error so users
	// don't accidentally pick the wrong source.
	pickedCount := 0
	if opts.FromFile != "" {
		pickedCount++
	}
	if opts.FromStdin {
		pickedCount++
	}
	if opts.FromNotion != "" {
		pickedCount++
	}
	switch pickedCount {
	case 0:
		return fmt.Errorf("specify a source: --from-file <path>, --from-stdin, or --from-notion <ref>")
	case 1: // ok
	default:
		return fmt.Errorf("specify exactly ONE source flag, not multiple")
	}

	if opts.AutoSolve && !opts.WithPlans {
		return fmt.Errorf("--auto-solve requires --plan (drafts are appended to the exec-plan's decision log)")
	}

	var src adapters.IngestSource
	switch {
	case opts.FromFile != "":
		src = adapters.NewFile(opts.FromFile)
	case opts.FromStdin:
		src = adapters.NewStdin()
	case opts.FromNotion != "":
		src = adapters.NewNotion(opts.FromNotion)
	}

	if ctx == nil {
		ctx = context.Background()
	}

	prose, err := src.Read(ctx)
	if err != nil {
		return fmt.Errorf("read source %s: %w", src.Name(), err)
	}
	prose = strings.TrimSpace(prose)
	if prose == "" {
		fmt.Println("Source returned empty prose. Nothing to extract.")
		return nil
	}

	// Load config (needed for both LLM extraction and brain open).
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Open brain early so we can run the idempotency check BEFORE
	// paying for the LLM extraction call. A duplicate-source ingest
	// should fail fast and cheap.
	profile, profileName := cfg.ActiveProfileConfig()
	_ = profile
	brn, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return fmt.Errorf("open brain: %w", err)
	}
	defer brn.Close()

	// Compute source-hash. Tagged on every ingested task, used as
	// the typed-memory event key for the source content, and
	// checked here for idempotent re-ingest detection.
	srcHash := ingestSourceHash(src.Name(), prose)
	srcTag := sourceTagPrefix + srcHash

	// Idempotency: if any task already carries this source-hash
	// tag, this exact source has been ingested before. Refuse to
	// create duplicates unless --force is set. Errors include the
	// matching task IDs and creation timestamps so the user can
	// see WHEN the prior run happened. showCompleted=true so we
	// also catch hashes from already-closed prior cohorts.
	prior, priorErr := brn.DB.ListTasks(true, []string{srcTag}, 0, 5, profileName, nil)
	if priorErr != nil {
		return fmt.Errorf("idempotency check failed: %w", priorErr)
	}
	if len(prior) > 0 && !opts.Force && !opts.DryRun {
		var lines []string
		for _, p := range prior {
			lines = append(lines, fmt.Sprintf("  #%d %q (created %s, status=%s)", p.TaskID, p.Title, p.Created, p.Status))
		}
		return fmt.Errorf(
			"this source was already ingested (source-hash=%s, %d matching task(s)):\n%s\n\n"+
				"Pass --force to ingest anyway (creates duplicate tasks). To re-process the\n"+
				"same source with new extraction prompts, edit the source first so the hash\n"+
				"changes; or run `aida tasks status #N closed` on the prior tasks before re-ingest.",
			srcHash, len(prior), strings.Join(lines, "\n"),
		)
	}
	if len(prior) > 0 && opts.Force {
		ui.PrintVerbose("Ingest", fmt.Sprintf("--force: re-ingesting despite %d prior task(s) with source-hash %s", len(prior), srcHash))
	}
	if len(prior) > 0 && opts.DryRun {
		fmt.Printf("(note: source-hash %s matches %d existing task(s); a real ingest would refuse without --force)\n\n", srcHash, len(prior))
	}

	// LLM extraction pass.
	apiKey := cfg.GetAPIKey()
	model := cfg.Model.Primary
	if apiKey == "" {
		return fmt.Errorf("no API key configured for ingestion (set %s)", cfg.API.AnthropicKeyEnv)
	}
	client := llm.NewClient(apiKey, model, false)

	raw, err := client.CompleteJSONWithStage(ctx, "ingest",
		llm.TaskExtractionSystemPrompt,
		llm.TaskExtractionUserPrompt(prose, src.Name()),
		llm.TaskExtractionSchema(),
	)
	if err != nil {
		return fmt.Errorf("task extraction: %w", err)
	}

	var resp struct {
		Tasks []extractedTask `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return fmt.Errorf("parse extracted tasks: %w", err)
	}
	if len(resp.Tasks) == 0 {
		fmt.Println("LLM extracted 0 tasks from the prose.")
		return nil
	}

	if opts.DryRun {
		fmt.Printf("Would create %d task(s) from %s (source-hash=%s):\n\n", len(resp.Tasks), src.Name(), srcHash)
		for i, t := range resp.Tasks {
			fmt.Printf("%d. %s\n", i+1, t.Title)
			if t.Body != "" {
				fmt.Printf("   %s\n", t.Body)
			}
			if t.Owner != "" {
				fmt.Printf("   owner: %s\n", t.Owner)
			}
			if len(t.Tags) > 0 {
				fmt.Printf("   tags:  %s\n", strings.Join(t.Tags, ", "))
			}
			fmt.Println()
		}
		return nil
	}

	// (Brain was opened, hash computed, and idempotency checked
	// before the LLM call above - no need to repeat here.)

	// Persist the source prose as a typed-memory event. Two reasons:
	// (1) auto-solve agents see it via the 5-channel retrieval
	//     surface alongside the inline excerpt - so even when the
	//     excerpt truncates, retrieval can pull the full content.
	// (2) it's a durable record of WHAT was ingested, not just the
	//     extracted tasks. If extraction is bad and we re-prompt
	//     later, we still have the original source.
	memEventID := ""
	memRec, memErr := brn.WriteMemory(ctx, brain.MemoryRecord{
		Type:   brain.MemoryEvent,
		Key:    "ingest-source:" + srcHash,
		Body:   prose,
		Tags:   []string{"ingest", srcTag, "source:" + src.Name()},
		Source: src.Name(),
	})
	if memErr != nil {
		ui.PrintVerbose("Ingest", "memory event write failed (continuing): "+memErr.Error())
	} else if memRec != nil {
		memEventID = memRec.ID
		ui.PrintVerbose("Ingest", fmt.Sprintf("source persisted as memory event %s (key=ingest-source:%s)", memEventID, srcHash))
	}

	// Pre-compute the excerpt once - every auto-solve task sees
	// the same excerpt, so don't recompute per task.
	excerpt := excerptForAutoSolve(prose, memEventID)

	// Track per-task ids so the optional auto-solve pass can
	// append drafts to the right exec-plan.
	var ingested []ingestedTask

	// Pull once before the whole batch, not per task - this can create
	// many tasks in one run (aida loop plan included, see loop.go) and
	// each one calls AddTask, whose own SyncTaskSeqFromDisk re-seed only
	// needs a fresh pull once at the start of the batch.
	if cfg.Brain.AutoSync {
		brain.PullAndWait(cfg.BrainPath())
	}

	created := 0
	planned := 0
	for _, t := range resp.Tasks {
		title := strings.TrimSpace(t.Title)
		if title == "" {
			continue
		}
		mergedTags := append([]string(nil), opts.ExtraTags...)
		mergedTags = append(mergedTags, t.Tags...)
		if t.Owner != "" {
			mergedTags = append(mergedTags, "owner:"+t.Owner)
		}
		// Always append the source-hash tag so re-ingest is
		// detectable and so `aida tasks --tag source-hash:abc` shows
		// the cohort from one ingestion run.
		mergedTags = append(mergedTags, srcTag)

		body := strings.TrimSpace(t.Body)
		if _, addErr := brn.AddTask(title, mergedTags, body); addErr != nil {
			ui.PrintVerbose("Ingest", fmt.Sprintf("AddTask failed for %q: %s", title, addErr.Error()))
			continue
		}
		created++

		var planID string
		if opts.WithPlans {
			// Find a free slug. Seq=0 first; bump if the id is
			// already taken (collision can happen when several
			// tasks normalize to the same 40-char slug AND land
			// in the same UTC second). Bounded retry; collision
			// is rare enough that 5 attempts is plenty.
			for seq := 0; seq < 5; seq++ {
				candidate := slugifyForPlan(title, seq)
				if _, err := brain.ReadExecPlan(brn.Path, candidate); err != nil {
					planID = candidate
					break
				}
			}
			if planID == "" {
				ui.PrintVerbose("Ingest", fmt.Sprintf("slug collision on %q after 5 attempts; skipping plan", title))
			} else {
				plan := brain.NewExecPlan(planID, title, brain.ExecPlanOrigin{
					Type: "ingest",
					Ref:  src.Name(),
				})
				plan.Goal = body
				plan.Tags = mergedTags
				if err := brain.WriteExecPlan(brn.Path, plan); err != nil {
					ui.PrintVerbose("Ingest", "exec-plan write failed: "+err.Error())
					planID = "" // mark unusable for auto-solve
				} else {
					planned++
				}
			}
		}
		ingested = append(ingested, ingestedTask{
			Title:         title,
			Body:          body,
			PlanID:        planID,
			SourceRef:     src.Name(),
			SourceExcerpt: excerpt,
			Tags:          append([]string(nil), mergedTags...),
		})
	}

	fmt.Printf("Ingested from %s: %d task(s) created", src.Name(), created)
	if opts.WithPlans {
		fmt.Printf(", %d exec-plan(s) created", planned)
	}
	fmt.Println(".")

	// Optional auto-solve pass: hand each extracted task to
	// `aida --agent` and append the draft answer to its exec-plan.
	// Closes the meeting → tasks → drafts → review loop.
	if opts.AutoSolve && len(ingested) > 0 {
		if err := autoSolveIngestedTasks(ctx, brn.Path, ingested, opts.AssumeYes); err != nil {
			return fmt.Errorf("auto-solve: %w", err)
		}
	}
	return nil
}

// ingestedTask records what was created so the optional
// auto-solve pass can append drafts back to the right exec-plan.
type ingestedTask struct {
	Title  string
	Body   string
	PlanID string // empty when --plan was not requested
	// SourceRef is the adapter's Name() (e.g. "notion:<page-id>",
	// "file:/path/to/x.md"). Surfaced in the auto-solve prompt so
	// the agent knows WHERE its context came from.
	SourceRef string
	// SourceExcerpt is the trimmed source prose (up to
	// sourceContextLimit chars) prepended to the auto-solve
	// prompt as inline context. Empty when source was empty or
	// when this struct is built outside an ingest run.
	SourceExcerpt string
	// Tags is the merged tag set on the brain task - issue #58 used
	// these to resolve a task to a typed destination via the partner
	// registry (removed 2026-09-12, see appendDestinationInstructions);
	// still includes user-supplied --tag values, the extractor's tags,
	// owner:<x>, and source-hash:<x>.
	Tags []string
}

// autoSolveIngestedTasks runs `aida --agent --run-dir <path>` for each
// task and appends the draft answer to its exec-plan as a decision-
// log entry. Sequential by design - concurrent agent runs on the same
// brain risk SQLite write contention, and per-task token costs add up
// faster when invisible. Surfaces a confirmation prompt unless
// assumeYes is true.
//
// Replaces the prior CombinedOutput-then-stripANSI pipeline with the
// jobs queue + --run-dir mode shipped earlier in the harness work.
// Drafts now flow through:
//
//	jobs.Enqueue → spawn `aida --agent --run-dir <path>` → cmd.Wait
//	  → read <run-dir>/output.md → AppendDecisionLog → jobs.Complete/Fail
//
// Net effect for callers and existing consumers (tasks_drafts.go etc.):
// nothing changed. Same decision-log format ("draft answer:\n..."),
// same exec-plan write, same sequential timing. The difference is
// internal: no subprocess stdout to ANSI-strip, and every draft now
// has a queryable jobs.db row with a recoverable manifest.json.
func autoSolveIngestedTasks(
	ctx context.Context,
	brainPath string,
	ingested []ingestedTask,
	assumeYes bool,
) error {
	if !assumeYes {
		fmt.Printf("\nAuto-solve will run `aida --agent` on %d task(s) sequentially. "+
			"Each may take 30s-2min and incur LLM cost.\nContinue? [y/N] ", len(ingested))
		var resp string
		_, _ = fmt.Scanln(&resp)
		if !strings.EqualFold(strings.TrimSpace(resp), "y") {
			fmt.Println("Skipped auto-solve. Tasks are still in brain; run `aida --agent` per task manually if needed.")
			return nil
		}
	}

	aidaBin, err := exec.LookPath("aida")
	if err != nil {
		return fmt.Errorf("`aida` binary not found on PATH: %w", err)
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	_, profileName := cfg.ActiveProfileConfig()
	if profileName == "" {
		return fmt.Errorf("no active profile resolved (set AIDA_PROFILE or run `aida profile use <name>`)")
	}

	store, err := jobs.Open(profileName)
	if err != nil {
		return fmt.Errorf("open jobs store: %w", err)
	}
	defer store.Close()

	for i, t := range ingested {
		fmt.Printf("\n[%d/%d] Auto-solving: %s\n", i+1, len(ingested), t.Title)

		// Typed destinations rode the removed partner registry
		// (issue #58); drafts now always use the universal flow.
		var dest *jobs.Destination

		// Compose the agent prompt from source context + task body.
		// When SourceExcerpt is empty (legacy path or empty source)
		// this collapses to the task body alone.
		baseQuestion := buildAutoSolveQuestion(t.SourceRef, t.SourceExcerpt, t.Title, t.Body)

		job, err := store.Enqueue("draft", t.Title, t.PlanID, baseQuestion, t.SourceRef)
		if err != nil {
			fmt.Printf("  ✗ enqueue failed: %s\n", err)
			continue
		}

		// Stash destination on the job so the web modal can pick the
		// right renderer when this draft is opened. Done after Enqueue
		// so we already have the run id; failure is non-fatal (the
		// draft still produces output.md and the universal Copy path
		// still works). Goes through the store (not a direct manifest
		// read/write) so Destination also lands on the SQL row and
		// survives every later manifest rewrite (Complete, Fail,
		// MarkNotified, ...) -- see Store.SetDestination.
		if dest != nil {
			if err := store.SetDestination(job.RunID, dest); err != nil {
				ui.PrintVerbose("Auto-solve", "destination write failed: "+err.Error())
			}
		}

		// Compose the question the subprocess actually sees - with
		// destination-specific instructions appended when applicable.
		question := appendDestinationInstructions(baseQuestion, job.RunID, dest)
		runDir := jobs.RunDir(profileName, job.RunID)

		// Per-task timeout - 7.5min, post-tuning value. Sized to let
		// deep multi-turn investigations finish (6-10min at the long
		// end) while capping batch wallclock when the agent loop
		// spirals. 15 tasks × 7.5min = ~1.9hr ceiling.
		runCtx, cancel := context.WithTimeout(ctx, 450*time.Second)

		// Spawn `aida --agent --run-dir <run-dir>`. The subprocess writes
		// events.ndjson + output.md directly to the run-dir; nothing
		// of substance comes back through stdout/stderr. We pin the
		// profile env so the subprocess writes manifests under the
		// same profile we just enqueued under (defense against a stale
		// AIDA_PROFILE in the parent's environment).
		cmd := exec.CommandContext(runCtx, aidaBin, "--agent", "--run-dir", runDir, question)
		cmd.Env = append(cmd.Environ(),
			"NO_COLOR=1", "CLICOLOR=0", "TERM=dumb",
			"AIDA_PROFILE="+profileName,
		)
		// Drop subprocess stdout/stderr - output.md and events.ndjson
		// are the canonical channels. /dev/null avoids an idle pipe
		// filling up if the agent ever did emit residual output.
		cmd.Stdout = nil
		cmd.Stderr = nil

		// Mark the job running with our pid; on success we'll Complete
		// it, on failure Fail.
		if _, err := store.Claim(hostnameForClaim(), os.Getpid()); err != nil && err != jobs.ErrNoJobsAvailable {
			// Soft-fail - the run can still proceed even if the
			// SQL row never transitioned. The output.md will land
			// either way and Reindex catches the manifest.
			fmt.Fprintf(os.Stderr, "  (claim failed: %s)\n", err)
		}

		runErr := cmd.Run()
		cancel()

		if runErr != nil {
			fmt.Printf("  ✗ failed: %s\n", runErr)
			_ = store.Fail(job.RunID, runErr.Error())
			if t.PlanID != "" {
				_ = brain.AppendDecisionLog(brainPath, t.PlanID, "auto-solve",
					fmt.Sprintf("aida --agent failed: %s", runErr))
			}
			continue
		}

		// Read output.md - the canonical draft text. ANSI-free by
		// construction; no stripANSI / dedupeSpinnerPhrases needed.
		outBytes, err := os.ReadFile(jobs.OutputPath(profileName, job.RunID))
		if err != nil {
			fmt.Printf("  ✗ read output.md failed: %s\n", err)
			_ = store.Fail(job.RunID, "read output.md: "+err.Error())
			continue
		}
		draft := strings.TrimRight(string(outBytes), "\n")

		preview := draft
		if len(preview) > 280 {
			preview = preview[:280] + "..."
		}
		fmt.Printf("  ✓ draft (%d chars): %s\n", len(draft), preview)

		if t.PlanID != "" {
			if err := brain.AppendDecisionLog(brainPath, t.PlanID, "auto-solve",
				"draft answer:\n"+draft); err != nil {
				ui.PrintVerbose("Auto-solve", "decision-log append failed: "+err.Error())
			}
		}
		if err := store.Complete(job.RunID); err != nil {
			ui.PrintVerbose("Auto-solve", "complete failed: "+err.Error())
		}
	}
	return nil
}

// hostnameForClaim returns os.Hostname() with a fallback so a Claim
// never fails for the silliest possible reason (hostname() error
// from a chroot/container). Empty hostname would trip Claim's
// validation; "localhost" is a safe default.
func hostnameForClaim() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	return h
}

// slugifyForPlan turns an extracted-task title into a stable plan
// id with a UTC date prefix so plans sort chronologically.
//
// Two correctness concerns:
//
//  1. Path-unsafe characters. normalizeName only strips a small set
//     of separators (-_. and space); it doesn't strip `/`, `\`, or
//     other path metacharacters. A title like "rake file/demo control
//     tool" produced a slug containing `/`, which made WriteExecPlan
//     try to create a subdirectory and silently lose the plan
//     (observed in the live butterstack v2 ingest: "15 tasks, 14
//     plans"). slugifyForPlan now keeps only [a-z0-9] from the
//     normalized slug so any character that looks like a path
//     separator OR a shell metacharacter is dropped.
//
//  2. Collisions. time.Now() is at second resolution. If two extracted
//     tasks finish slug normalization within the same second AND
//     produce identical 40-char-truncated slugs, they collide.
//     slugifyForPlan's caller (runTasksIngest) provides a per-task
//     index `seq` that gets appended to disambiguate when nonzero;
//     index 0 (first plan) keeps the original suffix-free shape so
//     existing tests don't break.
func slugifyForPlan(title string, seq int) string {
	const dateFmt = "20060102-150405"
	now := time.Now().UTC().Format(dateFmt)
	slug := slugSafeChars(normalizeName(title))
	if slug == "" {
		slug = "ingest"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	id := now + "-" + slug
	if seq > 0 {
		id += fmt.Sprintf("-%d", seq)
	}
	return id
}

// slugSafeChars filters a string down to lowercase alphanumerics
// only. Used as a defense against path-unsafe characters (`/`, `\`,
// `:`, etc.) that normalizeName doesn't catch but that would corrupt
// any filesystem-derived plan id.
func slugSafeChars(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		}
	}
	return string(out)
}
