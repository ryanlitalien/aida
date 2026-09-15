package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

// MCPToolCaller is the interface satisfied by mcp.MCPDiscovery. Defined here
// to avoid an import cycle (sources ← mcp via server.go → brain → ui → sources).
type MCPToolCaller interface {
	CallTool(ctx context.Context, server, name string, args map[string]any) (string, error)
}

// MCPToolAdapter calls MCP tools via the shared MCPDiscovery instance.
type MCPToolAdapter struct {
	Discovery MCPToolCaller
}

// Execute parses the LLM-generated command as JSON arguments and calls the
// MCP tool specified in the source's exec config (keys "server" and "tool").
func (a *MCPToolAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	result := SourceResult{
		Source:  "mcp-tool",
		Status:  "success",
		Command: command,
	}

	// Extract server and tool name from the source exec config.
	serverName := src.Exec["server"]
	toolName := src.Exec["tool"]
	if serverName == "" || toolName == "" {
		result.Status = "error"
		result.Summary = "MCP tool source missing 'server' or 'tool' in exec config"
		return result, nil
	}

	// Parse the LLM-generated command as JSON arguments.
	var args map[string]any
	if command != "" && command != "{}" {
		if err := json.Unmarshal([]byte(command), &args); err != nil {
			// If it doesn't parse as JSON, wrap the raw string as a "query" arg.
			args = map[string]any{"query": command}
		}
	}
	if args == nil {
		args = make(map[string]any)
	}

	start := time.Now()
	text, err := a.Discovery.CallTool(ctx, serverName, toolName, args)
	result.Duration = time.Since(start)

	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("MCP tool call failed: %v", err)
		return result, nil
	}

	if text == "" {
		result.Status = "empty"
		result.Summary = "MCP tool returned no content"
		return result, nil
	}

	artifacts, err := a.ParseOutput([]byte(text), src)
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("Failed to parse MCP output: %v", err)
		return result, nil
	}

	result.Artifacts = artifacts

	// Store as JSON if valid, otherwise as a JSON string.
	if json.Valid([]byte(text)) {
		result.Data = json.RawMessage(text)
	} else {
		jsonData, _ := json.Marshal(text)
		result.Data = json.RawMessage(jsonData)
	}

	result.Summary = fmt.Sprintf("MCP tool %s/%s returned %d artifacts", serverName, toolName, len(artifacts))
	return result, nil
}

// ParseOutput converts MCP tool output into artifacts. It tries JSON first,
// then falls back to raw text.
func (a *MCPToolAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	text := string(raw)
	if text == "" {
		return nil, fmt.Errorf("empty output")
	}

	// Try JSON array.
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

	// Try JSON object.
	var jsonObj map[string]interface{}
	if err := json.Unmarshal(raw, &jsonObj); err == nil {
		snippet, _ := json.Marshal(jsonObj)
		return []Artifact{
			{
				Type:    "row",
				ID:      "result",
				Snippet: string(snippet),
			},
		}, nil
	}

	// Raw text fallback.
	return []Artifact{
		{
			Type:    "page",
			ID:      "mcp-result",
			Snippet: text,
		},
	}, nil
}
