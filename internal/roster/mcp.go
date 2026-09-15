package roster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/mcp"
)

// mcpCallTimeout bounds a single CallTool invocation, matching
// internal/mcp's own toolCallTimeout so a stuck server can't wedge a
// dispatch indefinitely.
const mcpCallTimeout = 30 * time.Second

// mcpTaskArgKeys are the property names buildMCPArgs checks, in
// preference order, when guessing which schema field should carry the
// task's free text.
var mcpTaskArgKeys = []string{"query", "q", "input", "text", "question", "prompt"}

// toolCaller is the subset of *mcp.MCPDiscovery the mcp backend needs.
// Defining it here (rather than depending on the concrete type directly)
// lets tests substitute a fake instead of connecting to a real MCP server.
// *mcp.MCPDiscovery satisfies this interface.
type toolCaller interface {
	Tools() []mcp.DiscoveredTool
	CallTool(ctx context.Context, server, name string, args map[string]any) (string, error)
}

// mcpBackend delegates a Request to a discovered MCP tool. Tool and
// argument selection are both heuristic (no LLM call): see selectMCPTool
// and buildMCPArgs.
type mcpBackend struct {
	name   string
	server string
	tool   string // "" = pick the best tool on server at Ask time
	caller toolCaller
}

// newMCPBackend builds the mcpBackend for e, requiring a populated mcp:
// spec block and a connected discovery client in deps.
func newMCPBackend(e *Entry, deps Deps) (Backend, error) {
	if e.MCP == nil || e.MCP.Server == "" {
		return nil, fmt.Errorf("entry %q has kind %q but no mcp.server", e.Name, KindMCP)
	}
	if deps.Discovery == nil {
		return nil, fmt.Errorf("entry %q needs MCP discovery, but none was provided", e.Name)
	}
	return &mcpBackend{
		name:   e.Name,
		server: e.MCP.Server,
		tool:   e.MCP.Tool,
		caller: deps.Discovery,
	}, nil
}

// Kind implements Backend.
func (b *mcpBackend) Kind() string { return KindMCP }

// Ask implements Backend by resolving a tool on b.server (an explicit
// pin or the best heuristic match), building its arguments from the
// task, and calling it.
func (b *mcpBackend) Ask(ctx context.Context, req Request) (Result, error) {
	start := time.Now()
	tools := b.caller.Tools()

	toolName := b.tool
	var schema map[string]interface{}
	if toolName == "" {
		tool, ok := selectMCPTool(tools, b.server, req.Task)
		if !ok {
			return Result{
				Entry:  b.name,
				Status: StatusError,
				Text:   fmt.Sprintf("no MCP tool on server %s", b.server),
				TookMS: time.Since(start).Milliseconds(),
			}, nil
		}
		toolName = tool.Name
		schema = tool.InputSchema
	} else {
		// Explicit pin: still best-effort look up the schema (for the arg
		// heuristic below) among currently discovered tools, but don't
		// fail if discovery doesn't (yet) know about it.
		for _, t := range tools {
			if t.Server == b.server && t.Name == toolName {
				schema = t.InputSchema
				break
			}
		}
	}

	args := buildMCPArgs(schema, req.Task)

	callCtx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	text, err := b.caller.CallTool(callCtx, b.server, toolName, args)
	took := time.Since(start).Milliseconds()

	if err != nil {
		return Result{Entry: b.name, Status: StatusError, Text: err.Error(), TookMS: took}, nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Result{Entry: b.name, Status: StatusEmpty, Text: "MCP tool returned no output", TookMS: took}, nil
	}
	return Result{Entry: b.name, Status: StatusSuccess, Text: text, TookMS: took}, nil
}

// selectMCPTool picks the best tool on server for task, out of tools
// (which may span many servers). No LLM involved -- three tiers, in
// order, the first non-empty tier wins:
//
//  1. a tool whose Name or Description contains one of task's
//     (lowercased, length > 2) words -- a cheap relevance signal.
//  2. the first read-only tool on the server (safest default guess).
//  3. the first tool on the server, period.
//
// Returns ok=false if server has no tools at all.
func selectMCPTool(tools []mcp.DiscoveredTool, server, task string) (mcp.DiscoveredTool, bool) {
	var onServer []mcp.DiscoveredTool
	for _, t := range tools {
		if t.Server == server {
			onServer = append(onServer, t)
		}
	}
	if len(onServer) == 0 {
		return mcp.DiscoveredTool{}, false
	}

	for _, word := range taskWords(task) {
		for _, t := range onServer {
			if strings.Contains(strings.ToLower(t.Name), word) || strings.Contains(strings.ToLower(t.Description), word) {
				return t, true
			}
		}
	}

	for _, t := range onServer {
		if t.ReadOnly {
			return t, true
		}
	}

	return onServer[0], true
}

// taskWords lowercases and splits task on whitespace, trims surrounding
// punctuation, and drops words of length <= 3 (stop-word-ish noise like
// "a", "to", "on", "for", "the") so the relevance match in selectMCPTool
// isn't swamped by trivial matches.
func taskWords(task string) []string {
	fields := strings.Fields(strings.ToLower(task))
	words := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, ".,!?;:\"'()")
		if len(f) > 3 {
			words = append(words, f)
		}
	}
	return words
}

// buildMCPArgs guesses the argument map for tool's schema, heuristically
// (no LLM): if schema declares a property named one of mcpTaskArgKeys, the
// task's text goes there; else if schema declares exactly one required
// string property, the task's text goes there; else an empty map (which
// is exactly right for a no-arg tool).
func buildMCPArgs(schema map[string]interface{}, task string) map[string]any {
	args := map[string]any{}
	if schema == nil {
		return args
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return args
	}

	for _, key := range mcpTaskArgKeys {
		if _, ok := props[key]; ok {
			args[key] = task
			return args
		}
	}

	required := schemaRequired(schema)
	if len(required) == 1 {
		key := required[0]
		if propSpec, ok := props[key].(map[string]interface{}); ok {
			if t, _ := propSpec["type"].(string); t == "string" {
				args[key] = task
				return args
			}
		}
	}

	return args
}

// schemaRequired reads schema's "required" list, tolerating both the
// []interface{} shape a JSON-Schema map produces after json.Unmarshal and a
// plain []string (the shape a hand-built test fixture might use).
func schemaRequired(schema map[string]interface{}) []string {
	switch v := schema["required"].(type) {
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}
