package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ─── ingest_notion_tasks ───────────────────────────────────────────────────────
//
// Phase 1 of the autonomous loop: voice-triggered capture. Turns a Notion
// meeting/call page into `aida tasks` so the rest of the loop (dispatch → work →
// PR → approve) has something to chew on.
//
// Like aida_query, this shells out to `aida` (and to `claude --print` for the
// recent-page heuristic) rather than calling the engine in-process, so profile
// resolution, library routing, and source-hash idempotency all use the binary's
// own logic. It is Jarvis-only - never exposed as an MCP tool.
//
// Two-phase confirm flow (the tool itself is stateless; the Jarvis LLM threads
// page_ref across the two calls):
//  1. confirm=false → run `aida tasks ingest --from-notion <ref> --plan --dry-run`
//     and return the preview. The LLM reads the task count back and asks the user.
//  2. confirm=true  → run the real ingest with `--yes --tag auto` so the cohort
//     enters the autonomous loop's `auto` queue.

type ingestNotionInput struct {
	PageRef string `json:"page_ref"`
	Confirm bool   `json:"confirm"`
}

// ingestANSI strips spinner/escape codes from `aida`/`claude` output so the tool
// result Claude sees is clean text. Mirrors the stripper in aidaQueryTool.
var ingestANSI = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*(\x07|\x1b\\)`)

func ingestNotionTasksTool() Tool {
	return Tool{
		Name: "ingest_notion_tasks",
		Description: "Capture action items from a Notion meeting/call page into the user's aida task " +
			"tracker. Use when the user says \"turn that call into tasks\", \"capture the meeting " +
			"notes as tasks\", \"make tasks from the partner sync\", etc. If the user NAMES a page " +
			"(title or URL), pass it as page_ref; if they don't (\"that call\"), leave page_ref " +
			"empty and the tool auto-detects the most recently edited Notion meeting page. " +
			"FIRST call with confirm=false (the default): returns a DRY-RUN preview of the tasks " +
			"that WOULD be created - read the count and titles back to the user and ASK them to " +
			"confirm. ONLY after the user explicitly says yes, call again with the SAME page_ref " +
			"and confirm=true to actually create them (tagged `auto` so the autonomous loop can " +
			"work them). NEVER set confirm=true without the user's explicit go-ahead.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"page_ref": map[string]interface{}{
					"type": "string",
					"description": "Notion page URL, page id, or title. Leave empty to auto-detect " +
						"the most recently edited meeting/call notes page.",
				},
				"confirm": map[string]interface{}{
					"type": "boolean",
					"description": "false (default) = dry-run preview only; true = actually create the " +
						"tasks. Only set true after the user has confirmed the preview.",
				},
			},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in ingestNotionInput
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
			}
			// Generous budget: claude --print page fetch (up to 90s) + the
			// task-extraction LLM call. Matches aida_query's long-tail tolerance.
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
			defer cancel()

			ref := strings.TrimSpace(in.PageRef)
			if ref == "" {
				resolved, err := resolveRecentNotionPage(ctx)
				if err != nil {
					return "", fmt.Errorf("ingest_notion_tasks: no page_ref given and could not "+
						"auto-detect a recent Notion meeting page (%v) - ask the user which page", err)
				}
				ref = resolved
			}

			if !in.Confirm {
				out, err := runHMIngest(ctx, "tasks", "ingest", "--from-notion", ref, "--plan", "--dry-run")
				if err != nil {
					return out, fmt.Errorf("ingest_notion_tasks dry-run: %w", err)
				}
				return fmt.Sprintf("Notion page: %s\n\n%s\n\nTo create these, call ingest_notion_tasks "+
					"again with page_ref=%q and confirm=true (they'll be tagged `auto` for the autonomous "+
					"loop). Read the count back to the user and confirm before doing so.", ref, out, ref), nil
			}

			out, err := runHMIngest(ctx, "tasks", "ingest", "--from-notion", ref, "--plan", "--yes", "--tag", "auto")
			if err != nil {
				return out, fmt.Errorf("ingest_notion_tasks: %w", err)
			}
			return fmt.Sprintf("Created tasks from %s (tagged `auto`):\n%s", ref, out), nil
		},
	}
}

// runHMIngest runs `aida <args...>` and returns its cleaned, ANSI-stripped output.
// Args are passed as discrete argv elements (no shell), so page refs and titles
// can't shell-inject.
func runHMIngest(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "aida", args...).CombinedOutput()
	clean := strings.TrimSpace(ingestANSI.ReplaceAllString(string(out), ""))
	return clean, err
}

// resolveRecentNotionPage shells `claude --print` (which has Notion MCP loaded
// in the user's environment, same as NotionAdapter) to find the most recently
// edited meeting/call notes page, returning a ref `aida tasks ingest --from-notion`
// accepts (a URL when possible). Used when the user says "that call" without
// naming a page.
func resolveRecentNotionPage(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return "", fmt.Errorf("claude not on PATH")
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	prompt := "Using Notion, find the user's MOST RECENTLY EDITED meeting notes or call notes page. " +
		"Return ONLY its page URL on a single line - no preamble, no other text. " +
		"If you cannot find one, return exactly NONE."
	out, err := exec.CommandContext(cctx, "claude", "--print", prompt).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("claude --print (recent notion page): %w", err)
	}
	clean := ingestANSI.ReplaceAllString(string(out), "")
	ref := pickNotionRef(clean)
	if ref == "" || strings.EqualFold(ref, "NONE") {
		return "", fmt.Errorf("no recent Notion meeting page found")
	}
	return ref, nil
}

// pickNotionRef extracts a usable Notion reference from claude's output: prefer a
// line that looks like a URL, else the first non-empty line.
func pickNotionRef(s string) string {
	lines := strings.Split(s, "\n")
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if strings.Contains(ln, "notion.so") || strings.HasPrefix(ln, "http") {
			return ln
		}
	}
	for _, ln := range lines {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return ""
}
