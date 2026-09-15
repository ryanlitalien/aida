# Implementation Plan: Aida Agent Mode

**Date:** 2026-04-15
**Status:** Proposed
**Prerequisite reading:** `docs/gap-analysis-2026-04.md`

---

## Design Principle

Aida's core strength - deterministic routing with "LLM at edges" - stays unchanged for structured queries. Agent mode is a **new execution track** alongside the existing pipeline, not a replacement. The classifier decides which track to use.

```
User Query
    │
    ▼
[PARSE] → [CLASSIFY]
    │           │
    │     ┌─────┴──────┐
    │     │            │
    ▼     ▼            ▼
  PIPELINE          AGENT LOOP
  (existing)        (new)
  3 LLM calls       N tool calls
  deterministic     LLM-driven
  read-only         read+write
  fast, cheap       flexible, powerful
```

---

## Phase 1: MCP Client

### New dependency
```
go get github.com/modelcontextprotocol/go-sdk@v1.5.0
```

### New files

**`internal/mcp/client.go`** - MCP client manager
- `type MCPClient struct` - manages multiple MCP server connections
- `func NewMCPClient(configPaths []string) *MCPClient` - reads `.mcp.json` files, starts server processes
- `func (c *MCPClient) DiscoverTools() []MCPTool` - calls `session.Tools()` on each server
- `func (c *MCPClient) CallTool(server, name string, args map[string]any) (*mcp.CallToolResult, error)`
- `func (c *MCPClient) Close()` - shuts down server processes
- Excludes `aida` server from discovery (recursion prevention)

**`internal/mcp/config.go`** - `.mcp.json` parser
- `type MCPConfig struct` - mirrors `.mcp.json` format
- `func LoadMCPConfigs() (*MCPConfig, error)` - reads `~/.claude/.mcp.json` + project-level `.mcp.json`
- Merges configs, project-level overrides global

**`internal/sources/mcp_tool.go`** - MCP tool adapter
- Implements existing `Source` adapter interface
- `Execute()` calls `MCPClient.CallTool()`
- LLM constructs tool arguments from user query + tool's JSON schema
- Maps MCP tool annotations to aida capabilities:
  - `readOnlyHint: true` → read capabilities
  - `destructiveHint: true` → write capabilities, require confirmation
- Returns tool output as artifacts

### Modified files

**`internal/engine/planner.go`** - Register MCP tools as routable sources
- After loading library sources, append discovered MCP tools as additional sources
- Each MCP tool gets capabilities inferred from its annotations + description

**`internal/cli/root.go`** - Initialize MCP client at startup (lazy, only when needed)

### Safety

- Read-only MCP tools: auto-approved
- Destructive MCP tools: require user confirmation via stdin prompt
- All MCP calls logged to run record
- Timeout: 30 seconds per MCP tool call
- Aida's own MCP server excluded from discovery

---

## Phase 2: Agent Loop

### New files

**`internal/engine/agent_loop.go`** - The core agent loop
```go
type AgentLoop struct {
    client      *llm.Client
    mcpClient   *mcp.MCPClient
    adapters    map[string]sources.Adapter
    brain       *brain.Brain
    maxTurns    int
    maxBudget   float64
    verbose     bool
    confirmFn   func(action string) bool  // human-in-the-loop
}

func (a *AgentLoop) Run(ctx context.Context, query string) (*AgentResult, error)
```

The loop:
1. Build system prompt with: brain context, entity pages, routing wisdom, available tools (adapters + MCP tools as JSON schemas)
2. Call LLM with user query
3. If response has tool calls → execute each tool, append results to message history, go to 2
4. If response is text only → return as final answer
5. If turns >= maxTurns or cost >= maxBudget → synthesize best answer from accumulated results, return with warning

Tool definitions exposed to the LLM:
- Each aida adapter becomes a tool: `snowflake_query(sql)`, `chrono_query(query, timerange)`, `grep_search(pattern, path)`, `git_log(repo, args)`
- Each MCP tool exposed with its native schema
- Meta-tools: `ask_user(question)` for clarification, `delegate_to_claude_code(directory, task)` for code tasks

**`internal/engine/agent_tools.go`** - Adapter-to-tool wrappers
- Wraps each aida adapter as an LLM-callable tool with JSON schema
- Handles argument parsing, execution, result formatting

### Modified files

**`internal/engine/classifier.go`** - Add `StrategyAgent` 
- Triggered by action verbs: create, send, draft, fix, deploy, open, close, update, delete
- Triggered by multi-step phrasing: "do X then Y", "find X and use it to Z"
- Triggered by `--agent` CLI flag (manual override)

**`internal/cli/root.go`** - Add `--agent` flag

**`internal/engine/engine.go`** (or equivalent entry point)
- After classify, if strategy == agent → enter agent loop instead of pipeline
- Pass brain context, entity resolution results, and MCP client to agent loop

### Configuration

In `config.yaml`:
```yaml
agent:
  max_turns: 10
  max_budget_usd: 1.00
  model: claude-sonnet-4-6  # agent mode uses smarter model
  confirm_destructive: true
```

---

## Phase 3: Claude Code Delegation

### Modified files

**`internal/sources/claude_project.go`** - Enhanced delegation
- Accept structured task context (not just raw query)
- Pass allowed_tools list
- Capture structured output (files changed, PR URL, etc.)
- Cost budget forwarding

**`internal/engine/agent_tools.go`** - Add delegation tool
```go
// Tool: delegate_to_claude_code
// Description: Delegate a code generation, file editing, or PR creation task to Claude Code
// Parameters:
//   directory: string - project directory to work in
//   task: string - detailed task description
//   context: string - relevant aida context (entities, lessons)
//   allowed_tools: []string - tools the sub-agent can use
```

The agent loop (Phase 2) can invoke this tool when it determines a task requires code generation or file editing.

---

## Phase 4: Web Search

### Option A (preferred): MCP-based
If a web search MCP server is installed (Brave Search, Tavily, etc.), Phase 1's MCP client discovers it automatically. The planner routes web queries to it. Zero additional code.

### Option B: Exec adapter fallback
Add to `~/.aida/library/sources/`:
```yaml
# web-search.yaml
type: exec
description: Web search via Brave Search API
capabilities: [web-search, current-events, documentation-lookup]
exec:
  search: "curl -s 'https://api.search.brave.com/res/v1/web/search?q={query}' -H 'X-Subscription-Token: $BRAVE_API_KEY' | jq '.web.results[:5]'"
```

---

## Phase 5: Memory Self-Optimization

### Modified files

**`internal/brain/brain.go`** - Auto-compile trigger
- Track `last_compile_at` in brain metadata
- After each query, check if 25+ lessons recorded since last compile
- If so, trigger async compile (don't block query response)

**`internal/lessons/lessons.go`** - Lesson promotion
- After recording a lesson, check if similar pattern exists 3+ times with quality >= 4
- If so, generate a brain knowledge page from the pattern
- Write to `brain/knowledge/patterns/<topic>.md`

**`internal/engine/synthesizer.go`** - Routing hints
- After successful synthesis (quality >= 4), generate a one-line routing hint
- Store in lesson record: `hint: "route to [snowflake], filter by partner_ari"`
- Future similar queries see this hint in the lesson context

---

## Execution Order

```
Week 1-2:  Phase 1 (MCP Client) + Phase 5 (Memory, independent)
Week 3-4:  Phase 2 (Agent Loop)
Week 5:    Phase 3 (Claude Code Delegation) + Phase 4 (Web Search)
Week 6:    Integration testing, seed eval with agent mode queries
```

---

## Success Criteria

| Query | Expected Behavior | Phase Required |
|-------|-------------------|----------------|
| `aida "checkout errors yesterday"` | Existing pipeline, unchanged | Already works |
| `aida "search gmail for camp-butz invoice"` | MCP Gmail tool, returns email content | Phase 1 |
| `aida "what's the latest Go version?"` | Web search, returns current answer | Phase 1 or 4 |
| `aida --agent "find errors for camp-butz, check chrono, summarize"` | Agent loop, 3-step investigation | Phase 2 |
| `aida --agent "draft a Slack message about the outage to #incidents"` | Agent loop → Slack MCP tool, confirm before send | Phase 1 + 2 |
| `aida --agent "fix the timeout bug in butterstack and open a PR"` | Agent loop → delegate to Claude Code → PR URL | Phase 2 + 3 |
| `aida "checkout errors"` (30th time) | Auto-compiled routing wisdom makes it faster | Phase 5 |
