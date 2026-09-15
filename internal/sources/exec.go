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

const execTimeout = 30 * time.Second

// ExecAdapter is a generic shell-out adapter for sources with custom exec commands.
type ExecAdapter struct{}

// Execute runs the source's exec template with variable substitution.
func (a *ExecAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	result := SourceResult{
		Source: "exec",
		Status: "success",
	}

	// Issue #14 Bug E: when no template matches, the LLM sometimes
	// returns the literal placeholder "NO_MATCHING_TEMPLATE". Treat
	// that as a structured "no template" outcome rather than passing
	// it to the shell, which would attempt to execute it as a command.
	if isNoTemplatePlaceholder(command) {
		result.Status = "empty"
		result.Summary = "no exec template matched this question (LLM returned NO_MATCHING_TEMPLATE)"
		return result, nil
	}

	// Find the appropriate exec template
	cmdStr := a.resolveTemplate(command, src)
	if cmdStr == "" {
		result.Status = "error"
		result.Summary = "No exec template found for source"
		return result, nil
	}

	// Apply variable substitutions
	cmdStr = a.substituteVars(cmdStr, command, src)
	// Also reject if substitution result is just the placeholder.
	if isNoTemplatePlaceholder(cmdStr) {
		result.Status = "empty"
		result.Summary = "no exec template matched this question (LLM returned NO_MATCHING_TEMPLATE)"
		return result, nil
	}

	opts := execx.RunOpts{Timeout: execTimeout}
	if src.Path != "" {
		opts.Dir = config.ExpandPath(src.Path)
	}

	res, err := execx.RunShell(ctx, cmdStr, opts)
	if res.TimedOut {
		result.Status = "timeout"
		result.Summary = fmt.Sprintf("Command timed out after %s", execTimeout)
		// Unlike the other branches below, this returns a real error: a
		// nil error here reads as success to any caller that checks
		// err != nil (see agent_tools.go's buildSourceTool), letting a
		// timeout be recorded and synthesized as if it were a clean result.
		return result, fmt.Errorf("%s", result.Summary)
	}
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("exec error: %s: %s", err.Error(), string(res.Stderr))
		return result, nil
	}

	raw := res.Stdout
	if len(bytes.TrimSpace(raw)) == 0 {
		result.Status = "empty"
		result.Summary = "Command produced no output"
		return result, nil
	}

	artifacts, err := a.ParseOutput(raw, src)
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("Failed to parse output: %s", err.Error())
		return result, nil
	}

	result.Artifacts = artifacts

	// Try to store as JSON, fall back to string
	if json.Valid(raw) {
		result.Data = json.RawMessage(raw)
	} else {
		jsonData, _ := json.Marshal(string(raw))
		result.Data = json.RawMessage(jsonData)
	}

	result.Summary = fmt.Sprintf("Command completed with %d artifacts", len(artifacts))
	return result, nil
}

// resolveTemplate selects the best exec template for the command.
func (a *ExecAdapter) resolveTemplate(command string, src config.Source) string {
	if src.Exec == nil {
		return ""
	}
	// Try "query" template first (most common)
	if tmpl, ok := src.Exec["query"]; ok {
		return tmpl
	}
	// Try "search" template
	if tmpl, ok := src.Exec["search"]; ok {
		return tmpl
	}
	// Try "run" template
	if tmpl, ok := src.Exec["run"]; ok {
		return tmpl
	}
	// Fall back to first available template
	for _, tmpl := range src.Exec {
		return tmpl
	}
	return ""
}

// substituteVars replaces template variables in the command string.
func (a *ExecAdapter) substituteVars(cmdStr, command string, src config.Source) string {
	replacements := map[string]string{
		"{query}": command,
		"{path}":  config.ExpandPath(src.Path),
	}

	// Add any asset paths as variables
	for key, val := range src.Assets {
		replacements[fmt.Sprintf("{%s}", key)] = config.ExpandPath(val)
	}

	for placeholder, value := range replacements {
		cmdStr = strings.ReplaceAll(cmdStr, placeholder, value)
	}
	return cmdStr
}

// extractEnvelopeRows recognises common collection-wrapper shapes:
//
//	{ "results": [ … ] }                     // NYT top-stories / popular / wire / books / archive
//	{ "response": { "docs": [ … ] } }        // NYT search
//	{ "data":    [ … ] }                     // generic
//	{ "items":   [ … ] }                     // generic
//
// When the wrapper contains a non-empty array of objects, it returns those
// objects so the caller can split them into per-row artifacts. Returns
// (nil, false) when the shape doesn't match (single-object responses
// continue to be treated as one "result" artifact).
func extractEnvelopeRows(obj map[string]interface{}) ([]map[string]interface{}, bool) {
	candidates := []string{"results", "data", "items", "rows"}
	for _, key := range candidates {
		if v, ok := obj[key]; ok {
			if rows, ok := coerceObjectArray(v); ok {
				return rows, true
			}
		}
	}
	// Two-level: { "response": { "docs": [...] } } (NYT search) or similar.
	if resp, ok := obj["response"].(map[string]interface{}); ok {
		for _, key := range []string{"docs", "results", "items", "data"} {
			if v, ok := resp[key]; ok {
				if rows, ok := coerceObjectArray(v); ok {
					return rows, true
				}
			}
		}
	}
	return nil, false
}

// coerceObjectArray returns the slice of object maps when v is a non-empty
// []interface{} of map[string]interface{}; otherwise (nil, false).
func coerceObjectArray(v interface{}) ([]map[string]interface{}, bool) {
	arr, ok := v.([]interface{})
	if !ok || len(arr) == 0 {
		return nil, false
	}
	rows := make([]map[string]interface{}, 0, len(arr))
	for _, el := range arr {
		m, ok := el.(map[string]interface{})
		if !ok {
			return nil, false
		}
		rows = append(rows, m)
	}
	return rows, true
}

// ParseOutput tries to parse output as JSON first, falls back to raw text.
func (a *ExecAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, fmt.Errorf("empty output")
	}

	// Try to parse as JSON array
	var jsonArray []map[string]interface{}
	if err := json.Unmarshal(raw, &jsonArray); err == nil {
		var keys []string
		if len(jsonArray) > 0 {
			keys = sortedKeys(jsonArray[0])
		}
		fallbackCol := pickIDColumnFromMaps(keys, jsonArray)
		artifacts := make([]Artifact, 0, len(jsonArray))
		for i, item := range jsonArray {
			snippet, _ := json.Marshal(item)
			artifacts = append(artifacts, Artifact{
				Type:    "row",
				ID:      jsonRowID(item, fallbackCol, i),
				Snippet: string(snippet),
			})
		}
		ensureUniqueIDs(artifacts)
		return artifacts, nil
	}

	// Try to parse as single JSON object. Type "result" (not "row")
	// distinguishes whole-response envelopes (NYT API, single-record
	// tool calls) from per-element rows of a JSON array; the
	// synthesizer gives "result" a wider truncation budget.
	var jsonObj map[string]interface{}
	if err := json.Unmarshal(raw, &jsonObj); err == nil {
		// If the envelope wraps a collection (NYT shape:
		// {status, results: [...]} or search shape:
		// {response: {docs: [...]}}), explode the inner array into
		// per-row artifacts so the synthesizer can cite individual
		// articles by id and render per-article links. Without this,
		// every NYT response collapses to a single (source: result)
		// citation regardless of how many articles came back.
		if rows, ok := extractEnvelopeRows(jsonObj); ok {
			keys := sortedKeys(rows[0])
			fallbackCol := pickIDColumnFromMaps(keys, rows)
			artifacts := make([]Artifact, 0, len(rows))
			for i, item := range rows {
				snippet, _ := json.Marshal(item)
				artifacts = append(artifacts, Artifact{
					Type:    "row",
					ID:      jsonRowID(item, fallbackCol, i),
					Snippet: string(snippet),
				})
			}
			ensureUniqueIDs(artifacts)
			return artifacts, nil
		}
		snippet, _ := json.Marshal(jsonObj)
		return []Artifact{
			{
				Type:    "result",
				ID:      "result",
				Snippet: string(snippet),
			},
		}, nil
	}

	// Fall back to raw text as single artifact
	return []Artifact{
		{
			Type:    "file",
			ID:      "output",
			Snippet: text,
		},
	}, nil
}
