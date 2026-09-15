# Agent Orchestrator - Research & Direction

**Status:** all six phases built under `internal/engine/orchestrator/`; awaiting review.
**Branch:** `claude/research-agent-orchestrator-2AQvw`
**Last updated:** 2026-04-18

## Problem framing

Aida needs a multi-agent coordination layer (tracked in
`docs/harness-evolution-plan.md` row "Multi-agent coordination", priority P3).
Today `aida investigate` delegates work but does not coordinate sub-agents.
The goal is a supervisor that can fan out parallel sub-agents, support an
A2A-style protocol, and keep Aida's existing Anthropic + OpenAI providers
(Gemini is out of scope - we do not want to start paying for it).

## Direction

Build our own Go orchestrator in `internal/engine/` that ports the useful
ideas from Google ADK and OpenAI's Python Agents SDK, with pluggable LLM
provider adapters. Adopt the A2A spec via `trpc-a2a-go` rather than
reimplementing it. Emit OpenTelemetry traces for debugging instead of
cloning `adk-web`. Design the package boundary so we can eventually spin
it out as a standalone open-source Go library.

Why not just use ADK Go: it's hard-coupled to `google.golang.org/genai` and
has no community path to Anthropic or OpenAI we'd want to depend on. See
research below.

Why not just use OpenAI's Python Agents SDK: it's Python, we want a single
static Go binary, and the ideas are portable. A community port exists
(`nlpodyssey/openai-agents-go`) that we can reference.

Why not assemble from existing Go agent frameworks: none of LangChainGo,
Eino, Genkit, or SwarmGo has the full shape we want (multi-provider +
A2A + handoffs + parallel + guardrails). Eino is closest architecturally;
SwarmGo is closest in surface area. We borrow ideas, don't depend.

## Scope of the port

Primitives we're taking from ADK:

- `LlmAgent` - LLM-driven routing via a `transfer` tool
- `SequentialAgent`, `ParallelAgent`, `LoopAgent` - workflow composition
- Session abstraction - resumable, serializable, addressable runs
- Sub-agent hierarchy - root agent delegates to named sub-agents

Primitives we're taking from OpenAI Agents SDK:

- `Agent` - instructions + tools + handoffs + guardrails
- `Runner` - loop until final output or stop condition
- `Handoffs` - agent-as-tool delegation (same shape as ADK `transfer` but
  with better typing)
- `Guardrails` - parallel input/output validators that fail fast
- `Sessions` - persistent context memory (keyed store, not just in-memory)

Primitives we're taking from the Anthropic Claude Agent SDK (Python/TS):

- Built-in tool palette shape (Read/Write/Edit/Bash/Glob/Grep/WebFetch/
  WebSearch/Agent) - we already have analogs; this confirms the set
- Context compaction primitive - we already have a deterministic one,
  keep it
- MCP integration shape - we already have it in `internal/mcp/`

Primitives we're adopting as dependencies, not porting:

- A2A protocol - use `github.com/trpc-group/trpc-a2a-go` (Tencent,
  actively maintained, v1.0 JSON-RPC + gRPC, JWT+JWKS auth, streaming)
- OpenTelemetry tracing - standard spans; users can point at Jaeger /
  Tempo / Langfuse. Skip cloning `adk-web`.

## LLM provider adapter layer

Single `model.LLM` interface with concrete adapters:

- `anthropic.Model` over `github.com/anthropics/anthropic-sdk-go`
- `openai.Model` over `github.com/openai/openai-go`
- Interface has to translate tool schemas both ways (Anthropic `tool_use`
  and OpenAI function-calling are similar but not identical on streaming
  deltas and tool_result encoding)
- Streaming events unified into a single event stream type
- Usage/token accounting normalized so the supervisor can budget
- Explicitly **no `genai` dependency**, so no Gemini coupling

Adding a third provider later (Bedrock, Vertex direct, Ollama) is a
matter of implementing `model.LLM`. That's the extensibility story for
the eventual OSS pitch.

## Open-source angle

If the package boundary is clean from day one, we can extract
`internal/engine/orchestrator` → `github.com/ryanlitalien/aida-agent`
(or similar) as an independent Go library later. Rules of thumb to keep
the door open:

- No imports from `internal/brain`, `internal/sources`, `internal/cli`
  inside the orchestrator package
- Supervisor takes tools as an interface, not a concrete `agent_tools.go`
  reference
- Sessions and traces are pluggable (default implementations in the
  library; Aida supplies its own storage-backed impls)
- Config via code + a thin YAML loader; no coupling to Aida's config
  shape
- Examples directory with a "build your own Claude Code" demo would make
  it adoptable

The positioning wedge if we ever ship it: "ADK Go, but with Anthropic and
OpenAI first-class, plus OpenAI-Agents-SDK ergonomics, plus A2A, plus
OTel - no Gemini dependency." Today there is no Go library that hits all
of those.

## Research findings (April 2026)

### 1. ADK Go community adapters

- Upstream `google/adk-go` v1.1.0 ships only Gemini and Apigee via
  `google.golang.org/genai`.
- `go-a2a/adk-go` is a parallel community reimplementation (alpha,
  ~101 stars). Claims Gemini + Anthropic via a registry pattern. Not a
  Google fork.
- `jiatianzhao/adk-go-openai` is a small fork adding an OpenAI-compatible
  adapter (~3 stars, last release Dec 2025). Reference-grade at best.
- No merged PRs to upstream adding Anthropic or OpenAI. ADK Python and
  Java route non-Gemini via LiteLLM; ADK Java also got LangChain4j. No
  Go equivalent.
- **Verdict:** no first-class community path. If we want ADK-shaped
  ergonomics in Go with Anthropic/OpenAI, we write it.

### 2. A2A protocol

- **Spec v1.0.0 released March 12, 2026.** Production grade: signed
  Agent Cards, multi-tenancy, JSON-RPC + gRPC bindings, v0.3 → v1.0
  negotiation.
- Governance moved to Linux Foundation. 150+ orgs, 22k+ stars on the
  spec repo.
- `github.com/trpc-group/trpc-a2a-go` is a usable Go implementation - 
  JSON-RPC types, client/server, JWT+JWKS auth, streaming, in-memory
  task manager. Tencent-backed, actively maintained.
- **Verdict:** spec is stable enough to adopt. Use `trpc-a2a-go` rather
  than writing A2A ourselves.

### 3. adk-web debugger

- `github.com/google/adk-web` is an Angular/TypeScript local dev UI
  (localhost:4200) against an ADK API server (localhost:8000). Chat
  playground + event/trace waterfall + eval result viewer.
- Tied to ADK Python / Java API server contracts. **Not wired to ADK
  Go.** Go agents emit traces separately via Cloud Trace / OTel.
- **Verdict:** not worth cloning. Emit OTel spans and point users at
  Jaeger / Tempo / Langfuse. We already generate `test-runs/` artifacts
  that cover a lot of this locally.

### 4. `google.golang.org/genai` stability

- v1 stable since 2025; current v1.54.0 (April 13, 2026). Releases every
  5–7 days, additive. No documented breakings recently.
- **Verdict:** stable - but moot for us because we're not using it.
  Writing ADK-compatible model adapters would have locked us to its
  type surface; going DIY avoids that tax entirely.

### 5. Anthropic Agent SDK

- No Go Agent SDK. Official `anthropic-sdk-go` is raw API (messages,
  tools, streaming).
- Python: `anthropics/claude-agent-sdk-python`. TypeScript:
  `anthropics/claude-agent-sdk-typescript`. Both ship the Claude Code
  harness: agent loop, ~10 built-in tools (Read/Write/Edit/Bash/Glob/
  Grep/WebSearch/WebFetch/Monitor/Agent), context compaction, MCP. The
  "Claude Agent SDK" is the rename of the older "Claude Code SDK."
- Managed Agents (server-side loop) in public beta since April 8, 2026 - 
  an alternative to hosting your own loop.
- **Verdict:** for Aida we port the TS/Python harness shape onto
  `anthropic-sdk-go` calls. The official SDK doesn't give it to us in
  Go - porting is the only path.

### 6. OpenAI Agents SDK (Python) → Go

- Core abstractions: Agent, Runner, Handoffs, Guardrails, Tracing,
  Sessions.
- No official Go port. Community:
  - `nlpodyssey/openai-agents-go` - most faithful port (~249 stars,
    v0.1.0 Oct 2025). All core abstractions present. Best reference.
  - `MitulShah1/openai-agents-go`, `pontus-devoteam/agent-sdk-go`,
    `Ingenimax/agent-sdk-go` - smaller, varying completeness.
- **Port priority:** Agent + Runner + Handoffs first (the core value),
  then Guardrails, then Tracing (map to OTel), Sessions last (easy to
  re-invent on our own storage).

### 7. Go agent framework landscape

- **LangChainGo** - 20+ providers, single-agent focus, no A2A, no native
  multi-agent orchestration.
- **Eino (CloudWeGo / ByteDance)** - statically-typed graph/chain
  orchestration, production-scale, multi-agent graphs + parallel
  branches, no first-class A2A. Strongest Go option architecturally.
- **Genkit Go (Firebase)** - plugin-based, fast prototyping, single-
  agent + flows, no multi-agent primitives, no A2A.
- **SwarmGo** - OpenAI-Swarm-inspired, Agents + handoffs +
  ConcurrentSwarm (parallel agents) + memory, OpenAI + Anthropic, no
  A2A. Closest shape to what we want, but small community.
- **Verdict:** Eino's typed-graph core and SwarmGo's handoff shape are
  both worth reading. Neither is a drop-in. Borrow design; don't depend.

## Relevant existing code

- `internal/engine/agent_loop.go` - current single-turn agent loop
- `internal/engine/delegate.go` - delegation primitive; reconcile with
  new supervisor interface before touching either
- `internal/engine/planner.go` - planner that would plausibly call the
  supervisor
- `internal/cloud/agents.go` - cloud-side agent registry to promote
  behind the uniform interface
- `internal/mcp/` - existing MCP integration; keep
- `docs/harness-evolution-plan.md` lines 120, 847 - A2A and multi-agent
  tracking

## Package layout

```
internal/engine/orchestrator/
  doc.go              // package overview
  agent.go            // Agent type + helper methods
  runner.go           // single-agent tool-use loop + handoff dispatch
  composite.go        // SequentialAgent, ParallelAgent, LoopAgent + Runnable
  session.go          // Session interface + MemorySession
  tool.go             // Tool interface + FuncTool adapter
  handoff.go          // Handoff type + handoff tool-shape definition
  guardrail.go        // Guardrail interface + parallel fail-fast runner
  tracing.go          // Tracer interface + NoopTracer
  otel.go             // OTelTracer implementing Tracer over OpenTelemetry
  runner_test.go / composite_test.go / otel_test.go
  model/
    model.go          // LLM interface + Message/Block/ToolDef/Response
    model_test.go
    anthropic/        // anthropic-sdk-go adapter + tests
    openai/           // openai-go adapter + httptest roundtrip
  a2a/
    server.go         // Serve(Runnable, AgentCard) → *A2AServer
    client.go         // RemoteAgent implementing orchestrator.Runnable
    a2a_test.go       // end-to-end httptest roundtrip
```

Aida-specific concrete types (tools, storage-backed sessions,
brain-backed memory) live outside this package, in `internal/engine/`
proper. The orchestrator package imports nothing from the rest of
Aida - this is what makes future OSS extraction cheap.

## Implementation status

All six phases are built under `internal/engine/orchestrator/`. Each
phase is its own commit on the feature branch, reviewable in isolation.

1. **Done.** `model.LLM` interface and provider-agnostic types
   (`Message`, `Block`, `ToolDef`, `Request`, `Response`, `Usage`,
   `StopReason`). Anthropic adapter over `anthropic-sdk-go` handles
   text / tool_use / tool_result blocks with normalized stop reasons.
   OpenAI adapter over `openai-go` splits user messages carrying
   tool_result blocks into role-tool messages, translates tool_calls
   both directions, and preserves cached-token counts. 16 adapter
   tests including an httptest round-trip for OpenAI.
2. **Done.** `Agent` / `Runner` / `Session` / `Handoff` / `Guardrail`
   primitives. Runner reproduces `agent_loop.go`'s core behavior:
   `MaxTurns=10`, end on end_turn or empty tool_calls, 3-failure
   circuit breaker, deterministic context compression at turn>=8 &&
   messages>15. Extensions for handoff dispatch and parallel
   fail-fast guardrails. 14 tests including circuit-breaker,
   max-turns fallback, input/output guardrails, handoff plumbing, and
   compression parity.
3. **Done.** `SequentialAgent`, `ParallelAgent`, `LoopAgent` over a
   shared `Runnable` interface. Composites nest arbitrarily deep;
   `mergeInto` rolls child usage/turns/tool-calls into the aggregate.
   Timing test confirms `ParallelAgent` actually fans out (3×50ms
   children finish in <130ms).
4. **Done.** `a2a/Serve` wraps any Runnable as an A2A server via
   `trpc-a2a-go`'s `MessageProcessor` + in-memory TaskManager; `a2a/
   RemoteAgent` consumes remote A2A endpoints as local Runnables.
   End-to-end roundtrip test via `httptest` + a well-known
   agent-card endpoint check. Cross-process validation (two actual
   Aida binaries talking) is the one piece still on the user to
   verify in a real deployment.
5. **Done.** `OTelTracer` emits spans at three nested levels:
   `orchestrator.run` → `orchestrator.turn` → `orchestrator.tool`,
   with snake_case dotted attributes (agent.name, turn, tool.name,
   tool.use.id) so Jaeger / Tempo / Langfuse render cleanly. Tested
   via `tracetest.SpanRecorder` against a 2-tool scripted run.
6. **Done** - OSS decision and naming captured below.

## OSS extraction decision

**Recommendation: extract when there's demand, not yet.** The package
is already coupling-clean enough to lift into its own repo:

- Zero imports from `internal/brain`, `internal/sources`,
  `internal/cli`, `internal/config`, or any other Aida package.
- Only external deps are the three SDKs the orchestrator wraps
  (anthropic-sdk-go, openai-go, trpc-a2a-go) plus OTel.
- Every abstraction Aida would supply - concrete Tool implementations,
  storage-backed Sessions, brain-backed memory - plugs in via the
  orchestrator's existing interfaces. None of it needs to live inside
  the orchestrator package.

So the door is open. The question is *when to walk through it*: today
there are zero external users, so an external repo adds release/version
overhead without benefit. Extract once there's a real second consumer
(another of our projects, a partner team, or a real community ask).

When we do extract, the migration is mechanical: `git filter-branch`
or `git subtree split` the `internal/engine/orchestrator/` subdirectory
into its own repo, flip its module path, and change Aida's imports
to the new path. No code changes.

## Naming

Three candidates in order of preference:

1. **`argos`** - named for the many-eyed giant of Greek myth; fits the
   Aida family (messengers and watchers), conveys "many agents, one
   purpose" without being literal about it. Short, pronounceable,
   dockable as `github.com/ryanlitalien/argos` if extracted. Minor
   risk: there's a Greek letter library and a few unrelated projects
   named Argos in other ecosystems, but no dominant Go library.
2. **`agentry`** - evokes "agentry" as a craft/collective noun for
   agents. Unique on Go package search. Slightly less self-evident
   than Argos.
3. **`relay`** - communicates the handoff idea directly. Generic
   enough that collisions are likely; kept as a fallback.

**Recommendation:** go with `argos` when extracting. The name slots
naturally alongside Aida in a thematic family, is memorable, and
doesn't mislead anyone about provider or protocol lock-in.

## Resolved questions

The original research doc listed eight open questions. All are now
resolved - decisions captured below so the trail is visible.

1. **Package split - extract as OSS or keep internal?** Designed for
   extractability (zero imports from the rest of Aida; only stdlib +
   wrapped SDKs + OTel) but not extracting now. Cost stays low until
   there's a second consumer. See the "OSS extraction decision"
   section above.
2. **Unify `delegate.go` with the new supervisor?** Done - `delegate.go`
   deleted in commit `14e5252`. The decomposition types + function
   never executed anything (they only appended sub-task markdown to
   the investigation brief behind an opt-in `--decompose` flag). The
   new orchestrator composites (`SequentialAgent`, `ParallelAgent`,
   `LoopAgent`) supply the decomposition role with real execution
   when we want it. Unrelated `CrossProjectSource` helpers from the
   same file were moved to `internal/engine/crossproject.go`.
3. **Claude Agent SDK harness (compaction, tool palette) - reimplement
   or keep?** Keep Aida's own. The orchestrator's Runner reproduces
   the existing `agent_loop.go` compression heuristic (keep first +
   last 6, deterministic summary of middle; threshold at
   turn>=8 && messages>15) rather than porting Anthropic's TS
   compactor. Parity confirmed by `TestTruncateForLog` plus the
   `TestRunnerContextCompression` test asserting compressed summary
   appears in history.
4. **Guardrail design - parallel fail-fast?** Yes, ported as described.
   `runGuardrailsParallel` spawns goroutines against a shared
   cancellable context; first violation cancels the rest via `cancel()`
   and returns a `GuardrailViolation` wrapping the underlying error.
   Input guardrails run before any LLM call; output guardrails run
   against the final text.
5. **Session storage - in-memory + SQLite, or piggyback on
   `investigations/`?** In-memory only in this PR. `MemorySession`
   satisfies the `Session` interface; storage-backed implementations
   (SQLite, investigations-directory, brain) are trivial to add as
   separate types because `Session` is interface-typed all the way
   through. Deferred until a real persistence need arises.
6. **Managed Agents as a fourth provider adapter - in scope?** Out of
   scope. Managed Agents run the loop on Anthropic's side; the
   orchestrator is the loop. The right integration is to treat a
   managed-agent session as a `RemoteAgent`-style endpoint (Runnable
   that delegates to an external session) - that's a follow-up if we
   ever want it; it is not a model.LLM adapter.
7. **Tool schema / streaming delta translation - dedicated spike?**
   Deferred to #30 (streaming). Today `model.LLM` only exposes
   one-shot `Complete`, and tool_use schemas translate cleanly at the
   static-schema layer (JSON Schema both sides accept). Streaming is
   where Anthropic's `content_block_delta` vs. OpenAI's
   `choice.delta.tool_calls[*].function.arguments` diverge enough to
   need a unified event type, and that belongs in the streaming issue
   with a real consumer informing the design.
8. **Naming - what do we call it if extracted?** **`argos`**. The
   many-eyed watcher of Greek myth; thematic pair with Aida; not
   dominant in the Go ecosystem. See the "Naming" section above for
   the runner-up candidates.
