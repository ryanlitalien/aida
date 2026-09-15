package engine

// Lazy MCP tool loading + source discovery - Action #6 of the harness
// roadmap. Replaces eager registration of every discovered MCP tool
// with two meta-tools (find_mcp_tool, call_mcp_tool) that load tool
// definitions on demand. Source tools stay eager because there are
// only a handful per profile and they have type-specific input schemas
// the agent benefits from seeing up front.
//
// Why: TDS's "Save on Tokens" piece measures 55K-134K tokens of
// always-on tool definitions in setups with many MCP servers. Each MCP
// tool's name + description + JSON schema is sent on every API turn
// even when 90% of them are irrelevant to the current question. Two
// meta-tools cost ~300 tokens; the agent pulls only the definitions
// it needs.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/mcp"
)

// BuildAgentToolsLazy builds the agent's tool palette with MCP tools
// loaded lazily. Source tools and meta-tools are still registered
// eagerly. Pass nil mcpTools / mcpDiscovery to skip MCP entirely.
//
// claudeConfigDir is a trailing variadic (see BuildAgentTools) so
// existing call sites keep compiling unchanged; pass at most one
// value, the CLAUDE_CONFIG_DIR delegate_to_claude_code should set on
// its child process.
func BuildAgentToolsLazy(
	srcs config.Sources,
	mcpTools []mcp.DiscoveredTool,
	mcpDiscovery *mcp.MCPDiscovery,
	confirmFn func(action string) bool,
	confirmPatterns []string,
	claudeConfigDir ...string,
) []AgentTool {
	var tools []AgentTool

	// Source adapters - kept eager. Small in number per profile and
	// the per-source tool description carries type-specific hints
	// (SQL vs grep vs git args).
	for name, src := range srcs {
		tools = append(tools, buildSourceTool(name, src))
	}

	// Source-discovery meta-tool - useful when the agent's parser
	// suggested an entity it doesn't immediately recognize. Registered
	// even when MCP is empty.
	if len(srcs) > 0 {
		tools = append(tools, buildFindSourceTool(srcs))
	}

	// MCP - register two meta-tools instead of one tool per discovered
	// MCP tool. Skipped when there are no MCP tools at all.
	if len(mcpTools) > 0 && mcpDiscovery != nil {
		tools = append(tools, buildFindMCPTool(mcpTools))
		tools = append(tools, buildCallMCPTool(mcpTools, mcpDiscovery, confirmFn, confirmPatterns))
	}

	// Meta-tools.
	tools = append(tools, buildDelegateToClaudeCodeTool(firstOrEmpty(claudeConfigDir)))
	tools = append(tools, buildAskUserTool())

	// Terminal tool.
	tools = append(tools, buildSubmitResultTool())

	return tools
}

// buildFindSourceTool returns a tool that searches the source registry
// by keyword (substring match on name + description + capabilities +
// entities). Returns matching source names + 1-line descriptions in
// rank order.
func buildFindSourceTool(srcs config.Sources) AgentTool {
	return AgentTool{
		Name:        "find_source",
		Description: "Search aida data sources by keyword. Use this when you're not sure which source covers a topic - returns matching source names with short descriptions. After picking a source, call it directly by name.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Keyword(s) to search source names, descriptions, capabilities, and entities.",
				},
			},
			"required": []interface{}{"query"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			query := strings.ToLower(strings.TrimSpace(args.Query))
			if query == "" {
				return "", fmt.Errorf("query is required")
			}
			tokens := strings.Fields(query)

			type ranked struct {
				name string
				src  *config.Source
				hits int
			}
			var matches []ranked
			for name, src := range srcs {
				blob := strings.ToLower(name + " " + src.Description + " " +
					strings.Join(src.Capabilities, " ") + " " +
					strings.Join(src.Entities, " "))
				hits := 0
				for _, tok := range tokens {
					if strings.Contains(blob, tok) {
						hits++
					}
				}
				if hits > 0 {
					matches = append(matches, ranked{name, src, hits})
				}
			}
			sort.SliceStable(matches, func(i, j int) bool {
				if matches[i].hits != matches[j].hits {
					return matches[i].hits > matches[j].hits
				}
				return matches[i].name < matches[j].name
			})
			if len(matches) == 0 {
				return fmt.Sprintf("No sources matched %q. Available sources can be listed by calling list_sources or by reviewing the L1 LIBRARY LAYERS index in your system prompt.", args.Query), nil
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "Sources matching %q:\n", args.Query)
			for _, m := range matches {
				desc := m.src.Description
				if desc == "" {
					desc = "(no description; type=" + m.src.Type + ")"
				}
				fmt.Fprintf(&sb, "- %s (%s): %s\n", m.name, m.src.Type, desc)
			}
			return sb.String(), nil
		},
	}
}

// buildFindMCPTool returns a tool that searches the discovered MCP
// tool list by keyword and returns matching tool definitions (full
// name + description). The agent calls call_mcp_tool with the chosen
// name + an input object that satisfies the tool's input schema.
func buildFindMCPTool(mcpTools []mcp.DiscoveredTool) AgentTool {
	servers := make(map[string]struct{})
	for _, t := range mcpTools {
		servers[t.Server] = struct{}{}
	}
	serverNames := make([]string, 0, len(servers))
	for s := range servers {
		serverNames = append(serverNames, s)
	}
	sort.Strings(serverNames)

	desc := "Search MCP server tools by keyword. Returns matching tools' full names and descriptions; pair with call_mcp_tool to invoke."
	if len(serverNames) > 0 {
		desc += " Connected MCP servers: " + strings.Join(serverNames, ", ") + "."
	}

	return AgentTool{
		Name:        "find_mcp_tool",
		Description: desc,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Keyword(s) to search MCP tool names + descriptions.",
				},
			},
			"required": []interface{}{"query"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			query := strings.ToLower(strings.TrimSpace(args.Query))
			if query == "" {
				return "", fmt.Errorf("query is required")
			}
			tokens := strings.Fields(query)

			type ranked struct {
				tool mcp.DiscoveredTool
				hits int
			}
			var matches []ranked
			for _, t := range mcpTools {
				blob := strings.ToLower(t.Server + " " + t.Name + " " + t.Description)
				hits := 0
				for _, tok := range tokens {
					if strings.Contains(blob, tok) {
						hits++
					}
				}
				if hits > 0 {
					matches = append(matches, ranked{t, hits})
				}
			}
			sort.SliceStable(matches, func(i, j int) bool {
				if matches[i].hits != matches[j].hits {
					return matches[i].hits > matches[j].hits
				}
				return matches[i].tool.Name < matches[j].tool.Name
			})
			if len(matches) == 0 {
				return fmt.Sprintf("No MCP tools matched %q. Connected servers: %s.", args.Query, strings.Join(serverNames, ", ")), nil
			}
			cap := 12
			if len(matches) > cap {
				matches = matches[:cap]
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "MCP tools matching %q (call via call_mcp_tool with the full name):\n", args.Query)
			for _, m := range matches {
				fmt.Fprintf(&sb, "- mcp__%s__%s - %s\n", m.tool.Server, m.tool.Name, m.tool.Description)
			}
			return sb.String(), nil
		},
	}
}

// buildCallMCPTool returns a tool that invokes a discovered MCP tool by
// full namespaced name (mcp__<server>__<name>). Confirmation handling
// (destructive flag, confirmPatterns) mirrors the eager-MCP path.
func buildCallMCPTool(
	mcpTools []mcp.DiscoveredTool,
	discovery *mcp.MCPDiscovery,
	confirmFn func(action string) bool,
	confirmPatterns []string,
) AgentTool {
	// Index by full name for O(1) lookup at call time.
	byName := make(map[string]mcp.DiscoveredTool, len(mcpTools))
	for _, t := range mcpTools {
		full := fmt.Sprintf("mcp__%s__%s", t.Server, t.Name)
		byName[full] = t
	}

	return AgentTool{
		Name:        "call_mcp_tool",
		Description: "Invoke an MCP tool by full name (e.g. mcp__notion__notion-search). Use find_mcp_tool first to discover tool names. The input object must conform to the chosen tool's schema; pass {} when the tool takes no input.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "Full namespaced MCP tool name from find_mcp_tool, e.g. mcp__slack__slack_search_public.",
				},
				"input": map[string]interface{}{
					"type":        "object",
					"description": "Input object matching the tool's JSON schema. Pass {} for tools with no input.",
				},
			},
			"required": []interface{}{"name", "input"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			args.Name = strings.TrimSpace(args.Name)
			if args.Name == "" {
				return "", fmt.Errorf("name is required")
			}
			mt, ok := byName[args.Name]
			if !ok {
				return "", fmt.Errorf("unknown MCP tool %q (call find_mcp_tool to list available)", args.Name)
			}

			// Confirmation: destructive flag from MCP server OR
			// matches a profile-level guardrail pattern.
			needsConfirm := mt.Destructive
			for _, pattern := range confirmPatterns {
				if strings.Contains(mt.Name, pattern) || strings.Contains(args.Name, pattern) {
					needsConfirm = true
					break
				}
			}
			if needsConfirm {
				if confirmFn == nil {
					return "", fmt.Errorf("tool %q requires confirmation but no confirmation function is available", mt.Name)
				}
				if !confirmFn(fmt.Sprintf("Allow MCP tool %s/%s?", mt.Server, mt.Name)) {
					return "", fmt.Errorf("user declined MCP tool %s/%s", mt.Server, mt.Name)
				}
			}

			// Decode tool input into a generic map for the MCP caller.
			var toolArgs map[string]interface{}
			if len(args.Input) > 0 {
				if err := json.Unmarshal(args.Input, &toolArgs); err != nil {
					return "", fmt.Errorf("invalid tool input: %w", err)
				}
			}
			if toolArgs == nil {
				toolArgs = map[string]interface{}{}
			}

			result, err := discovery.CallTool(ctx, mt.Server, mt.Name, toolArgs)
			if err != nil {
				return "", err
			}
			if result == "" {
				return "(no output)", nil
			}
			return result, nil
		},
	}
}
