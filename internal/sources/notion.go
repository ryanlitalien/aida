package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/execx"
)

const (
	notionTimeout = 30 * time.Second
	// The "--" terminates --allowedTools variadic arg collection so the
	// prompt string after it is treated as the positional prompt, not
	// another allowed-tool name. Without --, claude --print errors with
	// "Input must be provided either through stdin or as a prompt
	// argument when using --print" because --allowedTools greedily
	// consumes every non-flag arg that follows it.
	notionDefault = `claude --print --allowedTools "mcp__notion__notion-search,mcp__notion__notion-fetch" -- "{query}"`
)

// NotionAdapter handles Notion queries via the Claude CLI with MCP tools.
type NotionAdapter struct{}

// Execute runs a Notion query using Claude CLI with MCP integration.
func (a *NotionAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	result := SourceResult{
		Source: "notion",
		Status: "success",
	}

	// Build the command string from exec template or default
	cmdStr := notionDefault
	if tmpl, ok := src.Exec["query"]; ok {
		cmdStr = tmpl
	}
	cmdStr = strings.ReplaceAll(cmdStr, "{query}", command)

	res, err := execx.RunShell(ctx, cmdStr, execx.RunOpts{Timeout: notionTimeout})
	if res.TimedOut {
		result.Status = "timeout"
		result.Summary = "Notion query timed out after 30 seconds"
		return result, nil
	}
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("claude CLI error: %s: %s", err.Error(), string(res.Stderr))
		return result, nil
	}

	raw := res.Stdout
	if len(bytes.TrimSpace(raw)) == 0 {
		result.Status = "empty"
		result.Summary = "No results from Notion"
		return result, nil
	}

	artifacts, err := a.ParseOutput(raw, src)
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("Failed to parse Notion output: %s", err.Error())
		return result, nil
	}

	result.Artifacts = artifacts
	// Store raw text as JSON string
	jsonData, _ := json.Marshal(string(raw))
	result.Data = json.RawMessage(jsonData)
	result.Summary = fmt.Sprintf("Retrieved Notion content (%d bytes)", len(raw))
	return result, nil
}

// ParseOutput wraps the Claude CLI text response as a single page artifact.
func (a *NotionAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, fmt.Errorf("empty Notion response")
	}

	artifacts := []Artifact{
		{
			Type:    "page",
			ID:      "notion-response",
			Snippet: text,
		},
	}
	return artifacts, nil
}
