package adapters

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// notionFetchTimeout caps how long we'll wait for claude to fetch
// a Notion page. Page fetches are usually <10s; this is a safety
// valve for runaway sessions or auth prompts that hang.
const notionFetchTimeout = 90 * time.Second

// NotionAdapter reads a Notion page's content as prose so it can
// feed `aida tasks ingest`. Implementation shells out to `claude
// --print` rather than the Notion REST API directly - claude has
// Notion MCP loaded by default in the user's environment, handles
// auth, and returns clean markdown. This mirrors the
// claude_project source type's pattern in internal/sources.
//
// The adapter is intentionally a thin wrapper. Any prompt-tuning
// (which fields to extract, depth of nested content, etc.) lives
// here and changes only this file - the ingestion / extraction
// pipeline downstream stays generic.
type NotionAdapter struct {
	// PageRef is the input the user supplied - a Notion URL,
	// page id, or human-readable page title. claude's Notion
	// MCP tooling handles all three.
	PageRef string

	// ClaudeBin lets tests override the binary. Empty defaults
	// to "claude" on PATH.
	ClaudeBin string
}

// NewNotion returns an IngestSource backed by Notion (via claude
// MCP). pageRef can be a URL, page id, or page title - claude's
// Notion tools resolve all three.
func NewNotion(pageRef string) *NotionAdapter {
	return &NotionAdapter{PageRef: pageRef}
}

// Name implements IngestSource. Format "notion:<ref>" so logs and
// exec-plan origins point at the actual source.
func (a NotionAdapter) Name() string {
	if strings.TrimSpace(a.PageRef) == "" {
		return "notion:(unset)"
	}
	return "notion:" + a.PageRef
}

// Read implements IngestSource. Shells out to claude with a
// constrained prompt that asks for ONLY the page contents as
// markdown - no commentary, no editorializing. claude uses its
// Notion MCP tools to fetch and returns the body text.
//
// The prompt is intentionally narrow: we tell claude this is a
// content-extraction task, not a question to answer. The
// downstream task-extraction LLM call expects raw prose, not
// claude's interpretation of it.
func (a NotionAdapter) Read(ctx context.Context) (string, error) {
	if strings.TrimSpace(a.PageRef) == "" {
		return "", fmt.Errorf("NotionAdapter: empty PageRef")
	}

	bin := a.ClaudeBin
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return "", fmt.Errorf("claude binary %q not found on PATH (NotionAdapter shells out to claude --print): %w", bin, err)
	}

	// Cap the call with a timeout independent of the parent ctx
	// so a stuck claude session doesn't hold up the whole
	// ingestion pipeline. Honor the parent ctx if it's tighter.
	fetchCtx, cancel := context.WithTimeout(ctx, notionFetchTimeout)
	defer cancel()

	prompt := fmt.Sprintf(
		"Fetch the contents of Notion page %q and return ONLY the page's text/content "+
			"as plain markdown. No commentary, no summary, no preamble - just the page body. "+
			"Include child page titles and bullet points as they appear; preserve the structure. "+
			"If the page can't be found or is empty, return the literal string 'EMPTY' and nothing else.",
		a.PageRef)

	cmd := exec.CommandContext(fetchCtx, bin, "--print", prompt)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("claude --print for %s failed: %w\noutput: %s",
			a.Name(), err, truncateForError(string(out), 500))
	}

	body := strings.TrimSpace(string(out))
	if body == "EMPTY" || body == "" {
		return "", fmt.Errorf("notion page %q returned no content", a.PageRef)
	}
	return body, nil
}

// truncateForError keeps Notion-fetch error messages skim-able
// when claude's output is large. Caller's error wrapping adds the
// adapter name so users can find the offending page.
func truncateForError(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}
