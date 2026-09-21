# Aida Gap Analysis: Industry Agent Architecture vs Current State

**Date:** 2026-04-15
**Sources researched:**
- Anthropic: "Building Effective Agents", Agent SDK docs, "Writing Effective Tools", "Advanced Tool Use", "Code Execution with MCP", "Managed Agents", "Effective Context Engineering"
- OpenAI: "A Practical Guide to Building Agents" (34-page guide), Agents SDK, Swarm, "Orchestrating Agents: Routines and Handoffs"
- Google: Agent Bake-Off 5 tips, ADK safety architecture
- MCP: Official spec (2025-11-25), Go SDK v1.5.0, tool annotations, 2026 roadmap
- Letta/MemGPT: Context Constitution, memory blocks, agent loop rearchitecture
- LangChain: "Your Harness, Your Memory"
- Phil Schmid: "Agent Harness 2026"
- Sarah Wooders (Letta): memory-as-harness thesis

---

## Executive Summary

Aida nails the two things the industry considers foundational: **"LLMs reason, deterministic code executes"** and **open, portable, local-first architecture**. Anthropic's "Building Effective Agents" guide explicitly validates aida's approach - it maps directly to their "routing" + "orchestrator-workers" workflow patterns and their recommendation to "find the simplest solution possible, and only increase complexity when needed." OpenAI's 34-page agent guide describes the same core principle. Google's Bake-Off winners won with this pattern. Aida is not behind on architecture; it's behind on capability surface area.

The structural ceiling: **aida is a read-only query router (a "workflow" in Anthropic's taxonomy), not an action-taking agent.** The goal - "answer comes back correct, whether that's a budget, a PR, or a Slack draft" - requires aida to cross the read/write boundary. That's the central gap.

The good news: aida doesn't need to become an agent framework. The industry consensus (Anthropic, OpenAI, Google) is clear - the orchestrator pattern (smart router + specialized workers) is the winning architecture. Aida already IS the smart router. It just needs better workers and the ability to let them act.

---

## Where Aida Already Leads

### 1. "LLMs Reason, Deterministic Code Executes"

Anthropic's "Building Effective Agents" draws a sharp line between **workflows** (deterministic pipelines) and **agents** (autonomous loops), recommending workflows for predictable tasks. Aida's 6-step pipeline - LLM at edges, deterministic middle - is a textbook implementation of their "routing" + "orchestrator-workers" patterns. Google's Bake-Off tip #5 says the same: "reserve language models for reasoning and intent extraction only... hand variables to deterministic code for actual execution." OpenAI's guide: "agents replace the human connector - they make decisions in ambiguous contexts." Aida correctly uses LLMs for ambiguity (parse, route, synthesize) and deterministic code for everything else.

**Industry validation: this is the winning pattern. Don't change it.**

### 2. Open Harness / Own Your Memory

LangChain warns against three escalating lock-in risks: API state storage, closed harnesses, and memory behind APIs. Wooders' thesis: "managing context, and therefore memory, is a core capability and responsibility of the agent harness." Letta's Context Constitution principle #4: "Model Independence - agent identity is separate from the underlying foundation model."

Aida is the model of what they're advocating: lessons in JSONL, brain in SQLite + git-backed markdown, config in YAML, all local. Model-switchable (Anthropic → OpenAI → Ollama) without losing memory. No provider lock-in.

### 3. Modular "Build to Delete" Architecture

Google tip #2 and Schmid both emphasize building for impermanence. Anthropic's framework caution: "frameworks often create extra layers of abstraction that can obscure the underlying prompts." Aida's adapter interface is clean Go - each source is a self-contained file, routing is YAML-driven, no framework magic. When models improve and an adapter becomes unnecessary, you delete one file.

### 4. Harness as Dataset

Schmid: "competitive advantage shifts from prompts to captured execution trajectories." Aida logs every run to `~/.aida/runs/`, records lessons with quality scores, tracks per-source outcomes, and has a full seed/eval pipeline (`aida seed eval` + `aida golden`). This is sophisticated for a personal agent.

### 5. Multi-Source Fan-Out

Google tip #1: "break the monolith into specialized sub-agents." Anthropic's "parallelization" workflow pattern. Aida does this natively - Snowflake, Chrono, grep, Notion run in parallel with errgroup (limit 5 concurrent). The router acts as Anthropic's "orchestrator."

### 6. Tool Design Alignment

Anthropic's "Writing Effective Tools" gives 7 principles. Aida already follows several:
- **Intentional selection**: sources have typed capabilities, not generic "do anything"
- **Token efficiency**: context files loaded only for selected sources
- **Meaningful returns**: artifacts tagged with source names and structured metadata
- **Iterative refinement**: the `aida tune` + `aida thumbs-up/down` feedback loop

---

## The Gaps (Revised, Industry-Grounded)

### GAP 1: No MCP Client / Dynamic Tool Discovery (Critical - Prerequisite for Everything)

**Industry consensus:** Anthropic's Agent SDK has MCP as a first-class integration. OpenAI's Agents SDK has `HostedMCPTool()`. Google tip #4: "mastering protocols like MCP separates fragile prototypes from scalable systems." The MCP spec (2025-11-25) defines `tools/list` for discovery and `tools/call` for invocation. The official Go SDK (`github.com/modelcontextprotocol/go-sdk` v1.5.0) provides `CommandTransport` for stdio servers, `session.Tools()` for discovery, and `session.CallTool()` for invocation.

**Where aida is:** MCP server only. Fixed adapter registry in `adapter.go:init()`. Cannot discover or call external tools. Meanwhile, the user's environment has 40+ MCP tools (Gmail, Slack, Calendar, Notion, Chrome DevTools) already configured.

**What this unlocks:** Everything. MCP client is the prerequisite for action execution, web search (via MCP), communication (Slack/Gmail MCP), and future tool integrations. A single implementation gives aida access to every MCP tool in the ecosystem.

**Key implementation detail from the Go SDK:**
```go
client := mcp.NewClient(&mcp.Implementation{Name: "aida", Version: "v1.0.0"}, nil)
transport := &mcp.CommandTransport{Command: exec.Command("aida", "serve")}
session, _ := client.Connect(ctx, transport, nil)
for tool := range session.Tools(ctx, nil) {
    // tool.Name, tool.Description, tool.InputSchema, tool.Annotations
}
result, _ := session.CallTool(ctx, &mcp.CallToolParams{Name: "slack_send_message", Arguments: args})
```

**Safety from MCP spec:** Tool annotations (`readOnlyHint`, `destructiveHint`, `idempotentHint`) inform approval decisions. Auto-approve read-only tools from trusted servers; require confirmation for destructive actions. This aligns with Anthropic, OpenAI, and Google safety guidance.

---

### GAP 2: No Agentic Reasoning Loop (Critical)

**Industry consensus:** Both Anthropic and OpenAI describe the identical core pattern:

```
while true:
    response = llm(tools + context + history)
    if response.has_tool_calls:
        results = execute(response.tool_calls)
        history.append(results)
    else:
        return response.text  // final answer
```

Anthropic: "Agents are typically just LLMs using tools based on environmental feedback in a loop." Claude Code's only state is "a message array." OpenAI: "The rule for whether the LLM output is a final output is that it produces text output with the desired type, and there are no tool calls."

Both recommend this ONLY for "open-ended problems where it's difficult or impossible to predict the required number of steps." Simple queries should use workflows (deterministic pipelines).

**Where aida is:** A static 6-step pipeline. No loop. The plan is computed upfront. The only dynamism is phase chaining (investigate strategy) and one quality re-execution cycle.

**The right design for aida:** Two-track architecture:
- **Fast track (existing pipeline):** For 80% of queries - structured data lookups, entity queries, source routing. Deterministic, fast, cheap. 3 LLM calls.
- **Agent track (new):** For 20% of queries - multi-step investigations, action-requiring tasks, complex workflows. The LLM drives tool selection in a loop. Capped by max_turns and cost budget.

The classifier already determines strategy (lookup, query, investigate, record, execute, search). Adding an "agent" strategy that enters the loop is a natural extension.

**Key design decisions from industry:**
- Anthropic: sub-agents get fresh context windows (prevents bloat). Only summaries return to parent.
- OpenAI: max_turns cap, max_budget_usd cap, graceful fallback on MaxTurnsExceeded.
- Both: human-in-the-loop for destructive actions. Always.

---

### GAP 3: No Action Execution (Critical - Solved by Gap 1 + Gap 2)

**Industry consensus:** Agents must act, not just answer. Anthropic's Agent SDK built-in tools include Read, Edit, Write, Bash, WebSearch, WebFetch. OpenAI's Agents SDK has ShellTool, CodeInterpreterTool, WebSearchTool. Codex runs in sandboxed containers with filesystem write access.

**Where aida is:** Read-only except CSV append.

**Why this gap is solved by MCP Client + Agent Loop:** Once aida can call MCP tools (Gap 1) and reason about which tools to call in a loop (Gap 2), it gains write capabilities:
- **Slack messages:** `mcp__claude_ai_Slack__slack_send_message`
- **Email drafts:** `mcp__claude_ai_Gmail__gmail_create_draft`
- **Calendar events:** `mcp__claude_ai_Google_Calendar__gcal_create_event`
- **Notion pages:** `mcp__claude_ai_Notion__notion-create-pages`
- **Code editing:** Delegate to Claude Code sub-agent (existing claude-project adapter)
- **PR creation:** Delegate to Claude Code sub-agent with `gh pr create`

Aida doesn't need bespoke adapters for each action. MCP is the universal adapter.

---

### GAP 4: No Claude Code Delegation for Complex Actions (High)

**Industry consensus:** Anthropic's "orchestrator-workers" pattern: central LLM dynamically decomposes tasks and delegates to worker LLMs. OpenAI's "agents as tools" manager pattern: orchestrator calls specialist agents via `Agent.as_tool()`. Both recommend context isolation - sub-agents get fresh context, only summaries return.

**Where aida is:** The `claude-project` adapter exists but is primitive. It shells out to `claude --print` in a directory with its own `.mcp.json`. It's not a first-class delegation mechanism - aida can't control what the sub-agent does, can't pass structured context, and can't validate results before accepting them.

**What this should become:** For tasks requiring code generation, file editing, or PR creation, aida should delegate to Claude Code (via the Agent SDK or `claude` CLI) with:
- Structured task description from aida's decomposition
- Relevant context from brain/lessons/entity pages
- Specific tool permissions (allowed/disallowed tools)
- Result validation before returning to user
- Cost budget cap

This is where aida's unique value shines: it provides the context (entity resolution, partner data, routing history) that a raw Claude Code session doesn't have.

---

### GAP 5: Memory Is Good But Not Self-Optimizing (Medium)

**Industry consensus:**
- Letta: "sleep-time compute" - background subagents conduct memory refinement during idle periods. Agents can use "rethink" tools to rewrite their own context blocks.
- Anthropic: "structured note-taking - agents write persistent notes outside the context window, retrievable later." Claude Code's Memory tool enables file-based knowledge bases.
- Wooders: "Can the agent modify its own system instructions? What survives compaction?"

**Where aida is:** Solid passive memory (lessons + brain + runs), but no active self-optimization:
- Brain `compile` exists but runs manually
- Lessons accumulate but are never pruned or promoted
- The agent can't update its own prompts or routing rules
- No background processing

**Path forward (inspired by Letta's sleep-time compute):**
1. **Auto-compile:** Trigger brain compilation every N queries or on idle
2. **Lesson promotion:** High-signal lessons (repeated patterns, consistent 5/5 quality) auto-promote to brain entity pages or routing wisdom
3. **Prompt self-modification:** After a successful query, the synthesizer writes a "note to future self" that gets injected into similar future queries - not raw run metadata, but refined instructions
4. **Lesson pruning:** Merge duplicate lessons, archive old low-signal ones

---

### GAP 6: No Web Search (Medium - Low Effort)

**Industry consensus:** Claude Code has WebSearch + WebFetch as built-in tools. OpenAI has WebSearchTool and FileSearchTool. Codex has network access during setup phase.

**Where aida is:** No web access.

**Path forward:** Two options, both simple:
- **Option A (MCP):** If an MCP web search server is available (Brave Search MCP, etc.), it's automatically discovered via Gap 1's MCP client. Zero additional code.
- **Option B (exec adapter):** Define a web search source in sources.yaml with an exec template that calls a search CLI. The LLM constructs the search query, exec runs it, results synthesized.

---

## What Aida Should NOT Change

These are validated by industry consensus across all sources:

1. **"LLM at edges, deterministic middle"** - Anthropic, OpenAI, and Google all validate this. Don't let LLMs make routing decisions for structured queries.
2. **Local-first, open architecture** - LangChain and Letta explicitly warn against provider lock-in. Aida owns its memory. Keep it.
3. **Config-driven routing for simple queries** - The 6-step pipeline should remain the default. The agent loop is an addition, not a replacement.
4. **The adapter interface** - Clean, modular, deletable. This is "build to delete" done right.
5. **The feedback/learning loop** - lessons + brain + quality scoring. Expand it, don't replace it.

---

## Implementation Plan

### Phase 1: MCP Client - The Universal Adapter

**Goal:** Aida discovers and calls MCP tools from `.mcp.json` configs.
**Unlocks:** Slack, Gmail, Calendar, Notion, web search, and any future MCP tools - both read AND write.

**Implementation:**
1. Add `github.com/modelcontextprotocol/go-sdk` dependency
2. New package `internal/mcp/client.go`:
   - Read `~/.claude/.mcp.json` and project-level `.mcp.json`
   - Start MCP server processes via `CommandTransport`
   - Discover tools via `session.Tools()`
   - Map tool annotations to aida capabilities (readOnlyHint → read capability, destructiveHint → write capability)
   - Expose `CallTool(name, args)` interface
3. New adapter `internal/sources/mcp_tool.go`:
   - Implements the Source adapter interface
   - Uses the MCP client to call discovered tools
   - LLM constructs tool arguments from user query + tool schema
4. Register MCP tools in the planner as sources with inferred capabilities
5. Safety layer:
   - Auto-approve tools with `readOnlyHint: true` from trusted servers
   - Require user confirmation (stdin prompt) for `destructiveHint: true` tools
   - Skip aida's own MCP server (no recursion - see memory note)
   - Log all MCP tool calls to runs

**Excludes from MCP discovery:** The `aida` server itself (aida already has direct Go access to its own brain/tasks).

**Test:** `aida "search my gmail for emails from pine-hollow"` → routes to Gmail MCP tool → returns results.

---

### Phase 2: Agent Loop Mode - Multi-Step Reasoning

**Goal:** For complex queries, aida enters an LLM-driven tool loop instead of the static pipeline.

**Implementation:**
1. New strategy in classifier: `agent` - triggered when:
   - Query requires actions ("create", "send", "draft", "fix", "deploy")
   - Query is multi-step ("do X then Y", "find X and use it to Z")
   - Query mentions tools that require write access
   - Manual override: `aida --agent "complex task"`
2. New package `internal/engine/agent_loop.go`:
   - Implements the standard agent loop pattern (shared by Anthropic and OpenAI):
     ```
     while turns < max_turns && cost < budget:
         response = llm(system_prompt + tools + history)
         if no tool_calls: return response.text
         for each tool_call: execute, append result to history
     ```
   - Available tools = aida adapters (snowflake, chrono, grep, etc.) + MCP tools (from Phase 1)
   - Each aida adapter exposed as a tool with JSON schema
   - System prompt includes: brain context, entity pages, routing wisdom, lessons
3. Safety guardrails:
   - `max_turns` (default 10, configurable)
   - `max_budget_usd` (default $1.00, configurable)
   - Human confirmation for destructive MCP tools
   - Dry-run mode: `aida --agent --dry-run` shows plan without executing
4. Integration with existing pipeline:
   - Fast track: classifier picks lookup/query/search → existing 6-step pipeline (unchanged)
   - Agent track: classifier picks agent → agent loop with full tool access
   - Hybrid: agent loop can call aida's own pipeline as a tool ("run a structured query against Snowflake")

**Test:** `aida --agent "find checkout errors for pine-hollow in the last hour, check if there was a recent deploy, and draft a Slack message to the team"` → multi-step execution with intermediate results visible in verbose mode.

---

### Phase 3: Claude Code Delegation - Code Generation and PRs

**Goal:** For tasks requiring code generation, file editing, or PR creation, aida delegates to Claude Code with structured context.

**Implementation:**
1. Enhance `internal/sources/claude_project.go`:
   - Accept structured task descriptions (not just raw queries)
   - Pass aida context: relevant brain entities, partner data, lessons
   - Support allowed/disallowed tool lists
   - Support cost budget cap
   - Capture structured output (files changed, PR URL, etc.)
2. New CLI flag: `aida --agent --delegate` for explicit delegation mode
3. The agent loop (Phase 2) can invoke delegation as a tool:
   - Tool: `delegate_to_claude_code(directory, task, context, allowed_tools)`
   - Returns: structured result (success/failure, artifacts, URLs)
4. Integration with Agent SDK (optional, future):
   - If Claude Agent SDK gets a Go binding, use it directly
   - Until then, shell out to `claude --print` with structured prompts

**Test:** `aida --agent "add a dark mode toggle to acme-widgets"` → aida decomposes → delegates to Claude Code in `~/dev/acme-widgets/` → returns diff + PR URL.

---

### Phase 4: Web Search

**Goal:** Aida can answer questions requiring current information from the web.

**Implementation:**
- **If MCP web search is available** (e.g., Brave Search MCP installed): automatic via Phase 1's MCP client. The planner routes web queries to the web search MCP tool. Zero code.
- **Fallback:** Add a web search exec source in `sources.yaml` with an exec template calling a search CLI tool. Define capabilities: `["web-search", "current-events", "documentation-lookup"]`.

**Test:** `aida "what's the latest MCP spec version?"` → routes to web search → returns current answer.

---

### Phase 5: Memory Self-Optimization

**Goal:** Aida gets smarter without manual intervention.

**Implementation:**
1. **Auto-compile brain:** After every 25 queries, trigger `brain compile` to extract routing wisdom from recent lessons. Track last compile timestamp in brain metadata.
2. **Lesson promotion:** When a lesson pattern repeats 3+ times with quality >= 4, auto-promote to a brain knowledge page (`brain/knowledge/patterns/`).
3. **Synthesizer notes:** After each successful query (quality >= 4), the synthesizer generates a one-line "routing hint" stored in the lesson. Future similar queries see: "Hint from prior success: route to [source] and filter by [field]."
4. **Lesson pruning:** Monthly, archive lessons older than 90 days with quality < 3 and no explicit feedback. Keep all thumbs-up/down lessons indefinitely.

**Test:** Run 30 queries over a week. Verify brain auto-compiles. Verify that query #31 for a similar topic benefits from compiled routing wisdom.

---

## Phase Dependency Graph

```
Phase 1: MCP Client ──────────────────────┐
    │                                      │
    ▼                                      ▼
Phase 2: Agent Loop ──► Phase 3: Claude Code Delegation
    │
    ▼
Phase 4: Web Search (may be free via MCP)
    
Phase 5: Memory Self-Optimization (independent, can start anytime)
```

---

## Updated Priority Matrix

| Phase | Gap | Effort | Unlocks | Depends On |
|-------|-----|--------|---------|------------|
| 1 | MCP Client | Medium (1-2 weeks) | Tool discovery, read+write actions, web search | Nothing |
| 2 | Agent Loop | High (2-3 weeks) | Multi-step reasoning, action orchestration | Phase 1 |
| 3 | Claude Code Delegation | Medium (1 week) | Code generation, PRs, file editing | Phase 2 |
| 4 | Web Search | Low (1 day or free) | Current information access | Phase 1 |
| 5 | Memory Self-Optimization | Medium (1 week) | Self-improving system | Nothing |

---

## The Vision, Restated

**Today (35% there):**
```
aida "what happened with checkout errors yesterday?"
→ 6-step pipeline → synthesized answer with citations ✅
```

**After Phase 1+2 (~60%):**
```
aida "search my gmail for the pine-hollow invoice and add it to my expenses"
→ agent loop → Gmail MCP search → CSV append → "Done. Added $4,200 invoice from Pine Hollow Campground to April expenses."
```

**After Phase 3 (~70%):**
```
aida --agent "fix the checkout timeout bug in acme-widgets and open a PR"
→ agent loop → investigate (Snowflake + Chrono) → delegate to Claude Code → PR created → "PR #42 opened: fixes checkout timeout by increasing retry window."
```

**After Phase 5 (~75%):**
```
aida "checkout errors for pine-hollow"
→ routing hint from compiled wisdom: "pine-hollow checkout errors → route to [snowflake, chrono], filter by partner_ari"
→ faster, more accurate, zero manual tuning
```

The last 25% is reliability at scale, edge case handling, and trust - earned through real-world usage and the feedback loop aida already has.
