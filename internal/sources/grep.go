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
	grepTimeoutDefault = 30 * time.Second
	grepTimeoutMax     = 5 * time.Minute
)

// GrepAdapter handles local file searches using grep.
type GrepAdapter struct{}

// Execute runs a grep search against the source's file path.
func (a *GrepAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	result := SourceResult{
		Source: "grep",
		Status: "success",
	}

	searchPath := config.ExpandPath(src.Path)
	if searchPath == "" {
		searchPath = "."
	}

	// Per-source timeout override via search.timeout_seconds. Large
	// codebases (the monorepo at ~300k files) blow past 30s even with
	// aggressive include filters, so we let the source config declare
	// a longer budget. Capped at 5 min to prevent runaway shells.
	timeout := grepTimeoutDefault
	if src.Search != nil && src.Search.TimeoutSeconds > 0 {
		timeout = time.Duration(src.Search.TimeoutSeconds) * time.Second
		if timeout > grepTimeoutMax {
			timeout = grepTimeoutMax
		}
	}

	// First attempt: build the grep command in default mode (BRE).
	// If the LLM-generated query contains regex metacharacters that
	// don't parse, we retry with grep -F (fixed strings) below.
	cmdStr := a.buildCommand(command, src, searchPath, false)

	opts := execx.RunOpts{Timeout: timeout}
	res, err := execx.RunShell(ctx, cmdStr, opts)
	if err != nil && isGrepRegexError(string(res.Stderr)) {
		// Retry with -F (fixed-string) so unescaped parens, +, ?, {n}
		// in the query are treated as literal characters. This is the
		// fallback for issue #14 Bug C: the LLM sometimes generates
		// regex patterns the BRE parser rejects.
		cmdStr = a.buildCommand(command, src, searchPath, true)
		res, err = execx.RunShell(ctx, cmdStr, opts)
	}

	if res.TimedOut {
		result.Status = "timeout"
		result.Summary = fmt.Sprintf("Search timed out after %s", timeout.Round(time.Second))
		return result, nil
	}
	// grep returns exit code 1 when no matches found - that is not an error
	if err != nil {
		if res.ExitCode == 1 {
			result.Status = "empty"
			result.Summary = "No matches found"
			return result, nil
		}
		result.Status = "error"
		result.Summary = fmt.Sprintf("grep error: %s: %s", err.Error(), string(res.Stderr))
		return result, nil
	}

	raw := res.Stdout
	if len(bytes.TrimSpace(raw)) == 0 {
		result.Status = "empty"
		result.Summary = "No matches found"
		return result, nil
	}

	artifacts, err := a.ParseOutput(raw, src)
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("Failed to parse grep output: %s", err.Error())
		return result, nil
	}

	// Enforce max results from search config
	if src.Search != nil && src.Search.MaxResults > 0 && len(artifacts) > src.Search.MaxResults {
		artifacts = artifacts[:src.Search.MaxResults]
	}

	result.Artifacts = artifacts
	result.Data = json.RawMessage(fmt.Sprintf(`{"match_count":%d}`, len(artifacts)))
	result.Summary = fmt.Sprintf("Found %d matches", len(artifacts))
	return result, nil
}

// buildCommand constructs the grep command string with include/exclude
// patterns. When fixedString is true, -F is added so the query is
// treated as a literal string (issue #14 Bug C fallback).
func (a *GrepAdapter) buildCommand(query string, src config.Source, searchPath string, fixedString bool) string {
	var parts []string
	if fixedString {
		parts = append(parts, "grep", "-rnF")
	} else {
		parts = append(parts, "grep", "-rn")
	}

	// Add include patterns from search config
	if src.Search != nil {
		for _, pattern := range src.Search.Include {
			parts = append(parts, fmt.Sprintf("--include=%q", pattern))
		}
		for _, pattern := range src.Search.Exclude {
			parts = append(parts, fmt.Sprintf("--exclude=%q", pattern))
			parts = append(parts, fmt.Sprintf("--exclude-dir=%q", pattern))
		}
	}

	parts = append(parts, fmt.Sprintf("%q", query))
	parts = append(parts, searchPath)

	return strings.Join(parts, " ")
}

// isGrepRegexError detects the BSD/GNU grep error messages produced
// when the pattern contains unescaped regex metacharacters. The
// substrings cover both BSD grep ("repetition-operator operand invalid")
// and GNU grep ("Unmatched", "Invalid").
func isGrepRegexError(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, marker := range []string{
		"repetition-operator",
		"unmatched",
		"invalid back reference",
		"trailing backslash",
		"invalid character class",
		"invalid range end",
		"invalid content of",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// ParseOutput parses grep output lines (file:line:content) into artifacts.
func (a *GrepAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	lines := bytes.Split(raw, []byte("\n"))
	artifacts := make([]Artifact, 0, len(lines))

	for i, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		lineStr := string(line)

		// Parse grep output format: file:line:content
		var file, content string
		var lineNum string
		parts := strings.SplitN(lineStr, ":", 3)
		switch len(parts) {
		case 3:
			file = parts[0]
			lineNum = parts[1]
			content = parts[2]
		case 2:
			file = parts[0]
			content = parts[1]
		default:
			content = lineStr
		}

		id := file
		if lineNum != "" {
			id = fmt.Sprintf("%s:%s", file, lineNum)
		}

		artifacts = append(artifacts, Artifact{
			Type:    "code",
			ID:      id,
			Snippet: strings.TrimSpace(content),
		})

		// Safety limit to avoid massive result sets
		if i > 1000 {
			break
		}
	}
	return artifacts, nil
}
