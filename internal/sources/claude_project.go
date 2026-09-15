package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/execx"
	"github.com/ryanlitalien/aida/internal/refusal"
)

// claudeProjectTimeout gives the delegated Claude Code sub-agent enough
// time to do its own intent parsing, MCP tool calls, and synthesis. These
// folders often hit external APIs (trainingpeaks, garmin, etc.) and spin
// up Python venvs, so the ceiling is generous.
const claudeProjectTimeout = 120 * time.Second

// ClaudeProjectAdapter delegates a natural-language question to a Claude
// Code sub-agent running INSIDE the source folder. This lets aida query
// any folder that is itself a Claude Code project with its own .mcp.json,
// .claude/settings.json, and CLAUDE.md -- the sub-agent picks up all of
// that automatically, including any stdio MCP servers declared for the
// project. Aida is the router; the sub-agent is the specialist.
//
// The "command" passed in is the user's raw (or LLM-refined) question,
// NOT a shell command. The LLM Call #2 prompt for claude-project sources
// is explicitly told to return a natural-language query, because building
// shell commands here would bypass the sub-agent's own reasoning.
type ClaudeProjectAdapter struct{}

// Execute shells out to `claude --print "<question>"` with the source
// folder set as working directory. It uses execx so that any background
// processes the subagent spawns (e.g. a dev server) don't pin aida'
// output pipes open past the timeout.
func (a *ClaudeProjectAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	result := SourceResult{
		Source: "claude-project",
		Status: "success",
	}

	question := strings.TrimSpace(command)
	if question == "" {
		result.Status = "error"
		result.Summary = "empty question passed to claude-project adapter"
		return result, nil
	}

	if src.Path == "" {
		result.Status = "error"
		result.Summary = "claude-project source has no path (need the folder containing .mcp.json / CLAUDE.md)"
		return result, nil
	}

	dir := config.ExpandPath(src.Path)

	// --dangerously-skip-permissions keeps the sub-agent non-interactive
	// so it doesn't stall on MCP tool-permission prompts. Safe because
	// aida runs on the same machine with the same user identity.
	//
	// Scrub Anthropic creds: aida's LoadDotEnv puts ANTHROPIC_API_KEY in our
	// env for the engine's own Haiku calls, but if the delegated `claude`
	// inherits it, claude runs in API-key mode where the user's claude.ai
	// OAuth account connectors (Slack, Gmail, Calendar, Drive, Notion,
	// Airtable) are all unavailable - only the project's .mcp.json stdio/URL
	// servers load. Stripping the key makes the sub-agent use the OAuth/
	// subscription path, so it sees the same connectors as an interactive
	// `claude` in that folder. Mirrors the claude -p connector fallback.
	res, err := execx.Run(ctx, "claude",
		[]string{"--print", "--dangerously-skip-permissions", question},
		execx.RunOpts{
			Timeout: claudeProjectTimeout,
			Dir:     dir,
			Env:     config.ScrubAnthropicCreds(os.Environ()),
		})

	if res.TimedOut {
		result.Status = "timeout"
		if res.OrphansKilled > 0 {
			result.Summary = fmt.Sprintf("claude --print timed out after %s in %s (reaped background process group)", claudeProjectTimeout, dir)
		} else {
			result.Summary = fmt.Sprintf("claude --print timed out after %s in %s", claudeProjectTimeout, dir)
		}
		return result, nil
	}
	if err != nil {
		result.Status = "error"
		stderrText := strings.TrimSpace(string(res.Stderr))
		if stderrText == "" {
			stderrText = err.Error()
		}
		result.Summary = fmt.Sprintf("claude --print failed in %s: %s", dir, stderrText)
		return result, nil
	}

	raw := res.Stdout
	text := strings.TrimSpace(string(raw))
	if text == "" {
		result.Status = "empty"
		result.Summary = fmt.Sprintf("Sub-agent returned no output from %s", dir)
		return result, nil
	}

	// The sub-agent sometimes declines instead of answering (e.g. its own
	// MCP tools are misconfigured, or it lacks access inside its project
	// folder). That prose otherwise becomes a "success" artifact that the
	// synthesizer can cite as fact. Catch it here and land on "empty" with
	// no artifacts, matching how any other source reports "nothing found".
	if refused, isRefusal := subagentRefusal(text); isRefusal {
		return refused, nil
	}

	artifacts, _ := a.ParseOutput(raw, src)
	result.Artifacts = artifacts
	jsonData, _ := json.Marshal(string(raw))
	result.Data = json.RawMessage(jsonData)
	result.Summary = fmt.Sprintf("Delegated to claude sub-agent in %s (%d bytes)", dir, len(raw))
	return result, nil
}

// subagentRefusal builds the SourceResult for a claude-project sub-agent
// response that reads as a declined/refused answer rather than real output.
// Split out from Execute so the classification is unit-testable without
// shelling out to the claude CLI.
func subagentRefusal(text string) (SourceResult, bool) {
	if !refusal.LooksLikeRefusal(text) {
		return SourceResult{}, false
	}
	return SourceResult{
		Source:  "claude-project",
		Status:  "empty",
		Summary: "sub-agent declined: " + refusal.FirstLine(text),
	}, true
}

// ParseOutput wraps the sub-agent's text response as a single artifact.
// The sub-agent has already done its own citation work internally; aida'
// synthesizer will quote the whole response and attribute it to the
// sub-agent by folder name.
func (a *ClaudeProjectAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, fmt.Errorf("empty response from claude sub-agent")
	}
	// Use the source's display name (folder basename) as the artifact ID
	// so citations read "(workouts: subagent)" rather than the generic
	// "(source 1: claude-project)".
	id := src.Path
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if id == "" {
		id = "subagent"
	}

	return []Artifact{
		{
			Type:    "subagent",
			ID:      id,
			Snippet: text,
		},
	}, nil
}
