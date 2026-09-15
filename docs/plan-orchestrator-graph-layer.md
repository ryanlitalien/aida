# Orchestrator: graph layer + ADK Go 2.0 provider verdict

**Status: IMPLEMENTED (phases 1–4)** on branch `worktree-adk-graph-sketch`.
**Last updated:** 2026-07-02
**Context:** follow-up to `docs/plan-agent-orchestrator-research.md` (April 2026), which built
our provider-agnostic orchestrator and evaluated Google `adk-go` v1.1.0. Google shipped
**ADK Go 2.0** (graph engine + durable HITL). This doc verified whether 2.0 changes our
build-vs-adopt call (it does not - still Gemini-only), and the graph layer it sketched has
since been built out.

## Implemented

Validated against LangGraph (the market-leading graph agent framework) - ADK 2.0's shape is
convergent with it, so we adopted the *pattern*, not the Gemini-locked framework. All under
`internal/engine/orchestrator/` unless noted; every step tested under `-race`.

- **Phase 1 - graph engine** (`graph.go`, `state.go` excluded): nodes wrap any `Runnable`; routed
  edges (`Route`/`When`/`DefaultRoute`); fan-out; fan-in `Join`; **bounded cycles** via per-node
  `MaxVisits` (the evaluator-optimizer loop shape); per-node `Retry`/`Timeout`; `graph.node` OTel
  spans.
- **Phase 2 - shared typed state** (`state.go`): LangGraph-style `State` + per-key `Reducer`s,
  opt-in via `StatefulRunnable`; `Node.RouteState` and `Graph.OutputKey`. Fully additive.
- **Phase 3 - durable checkpointing** (`checkpoint.go`): `RunDurable`/`Resume`, `Checkpointer`
  (file-backed atomic write mirroring `internal/jobs`), `RequestInput` pause sentinel + handoff-mode
  resume across a process restart.
- **Phase 4 - real wire-in** (`internal/engine/graph_investigate.go`, `internal/cli`): the
  `BuildInvestigateGraph` supervisor (triage → parallel specialists → evaluator-optimizer),
  behind `aida --agent --graph`. Default agent path unchanged. Verified end-to-end with a live run.

The sections below are the original research/design record.

## 1. Provider verdict - does ADK Go 2.0 unlock Anthropic/OpenAI?

**No. It is still Gemini-only, and the model interface is still `genai`-typed end to end.**
Our April thesis ("hard-coupled to `google.golang.org/genai`, no community path to
Anthropic/OpenAI") holds unchanged in 2.0.

Evidence, from a clone of `google/adk-go` at tag `v2.0.0` (module `google.golang.org/adk/v2`, go 1.25):

- **`model/` ships only two backends:** `model/gemini` and `model/apigee`. `apigee` is not a
  third provider - it's Gemini/Vertex behind an Apigee proxy (`GOOGLE_GENAI_USE_VERTEXAI`,
  model names `apigee/gemini/*` and `apigee/vertex_ai/*`).
- **Grep for `anthropic|openai|litellm|bedrock|ollama|claude|gpt` across all non-test `.go`
  files returns nothing.** No LiteLLM-equivalent exists in Go (Python/Java route non-Gemini
  via LiteLLM; Go has no analog).
- **The model interface is single-method but genai-typed:**
  ```go
  // model/llm.go
  type LLM interface {
      GenerateContent(ctx context.Context, req *LLMRequest, stream bool) iter.Seq2[*LLMResponse, error]
  }
  type LLMRequest  struct { Contents []*genai.Content; Config *genai.GenerateContentConfig }
  type LLMResponse struct {
      Content           *genai.Content
      CitationMetadata  *genai.CitationMetadata
      GroundingMetadata *genai.GroundingMetadata
      UsageMetadata     *genai.GenerateContentResponseUsageMetadata
      FinishReason      genai.FinishReason
      // ...
  }
  ```
  A third-party Anthropic adapter is *technically* implementable (one method) but would have to
  marshal Claude's wire format to/from `genai.Content`, `genai.FinishReason`, grounding/citation
  metadata, transcriptions, logprobs. That is exactly the "locked to genai's type surface" tax
  the April research predicted - unchanged in 2.0.

**Consequence:** our differentiator ("Anthropic + OpenAI first-class, no genai coupling, plus
A2A, plus OTel") is intact. ADK Go 2.0 does **not** obsolete our orchestrator. Keep building on
`internal/engine/orchestrator`.

## 2. What ADK Go 2.0 actually added (the parts worth studying)

The headline is the `workflow/` package - a graph engine. Public surface:

| ADK 2.0 concept | API |
|---|---|
| Node | `type Node interface { Name/Description/Config/InputSchema/OutputSchema/ValidateInput/ValidateOutput; Run(ctx, input) iter.Seq2[*session.Event, error] }` |
| Node kinds | `NewFunctionNode[IN,OUT]`, `NewAgentNode(agent.Agent)`, `NewToolNode(tool.Tool)`, `NewJoinNode`, `NewDynamicNode[IN,OUT]`, `NewWorkflowNode` (subgraph), `NewEmittingFunctionNode`, `NewFunctionNodeFromState` |
| Edge | `type Edge struct { From, To Node; Route Route }`; sentinel `Start Node` |
| Routing | `Route` iface; `StringRoute`, `IntRoute`, `BoolRoute`, `MultiRoute[T]`, `Default`. A node emits `event.Routes`; edges match. |
| Build | `EdgeBuilder{Add, AddRoute, AddFanOut, AddFanIn, AddRoutes}`, plus `Chain(nodes...)` / `Concat(...)` |
| Construct/run | `New(name, edges, opts...) (*Workflow, error)`; `Workflow.Run(ctx) iter.Seq2[*session.Event, error]`; `WithMaxConcurrency` |
| Durability | `resume.go` + `persistence.go` + `request_input.go` - a node pauses, RunState persists in `session.State`, run resumes across process restarts |
| Resilience | per-node `retry.go` (exp backoff+jitter), per-node timeouts, graph-wide concurrency cap |
| Unified runtime | even a standalone agent runs through the graph via a synthetic single-node wrapper (`WithRootWrapper`) |

Two ideas are genuinely ahead of our composites:
1. **Graph > tree.** Their DAG-with-routing expresses conditional branching and fan-in that our
   `Sequential`/`Parallel`/`Loop` tree can't without hand-rolling it inside one Agent.
2. **Durable HITL.** Pause → persist → resume-across-restart. Our orchestrator package has none
   (MemorySession only) - though Aida *as a whole* already has this in `internal/jobs`.

## 3. The graph layer sketch (`graph.go`)

A directed-graph Runnable that maps ADK's model onto our types, without the genai tax.

**Mapping to ADK:**

| ADK 2.0 | Ours | Note |
|---|---|---|
| `Node` interface (genai events) | `Node{Name, Runnable, Route, Join, Merge}` | node wraps any `Runnable` - Agent, composite, nested Graph, or `RunnableFunc` |
| `NewAgentNode(a)` | `Node{Runnable: agent}` | our Agent is already a Runnable; zero adapter |
| `NewFunctionNode` / dynamic node | `RunnableFunc` | plain Go func as a node → data-dependent branching |
| `NewWorkflowNode` (subgraph) | `Graph` is a `Runnable` | graphs nest in composites and vice versa |
| `StringRoute` + `Default` | `Node.Route func(*RunResult) string` + `Edge.When` / `DefaultRoute` | node emits label, edge matches; same shape, synchronous |
| `JoinNode` | `Node.Join` + `Node.Merge` | fan-in barrier |
| `EdgeBuilder.AddFanOut/AddFanIn` | multiple `Edge`s | fan-out runs successors concurrently |
| `WithMaxConcurrency` | `Graph.MaxConcurrency` | semaphore in the scheduler |
| `Start` sentinel | `StartNode` const | entry edges |

**Design choices that differ from ADK on purpose:**
- **String in, `*RunResult` out** - same contract as our existing `Runnable`, so every Agent and
  composite is a node for free and telemetry rolls up through the existing `mergeInto`. No
  `genai.Content`, no `iter.Seq2` event plumbing.
- **A `Graph` is a `Runnable`** - the "unified runtime" without a synthetic wrapper: composition
  is just interface nesting.
- **Deterministic final output** - terminal node outputs joined in name order (single-terminal
  graphs, the common case, yield exactly that node's output).

**Worked example - the supervisor shape** (an LLM router fanning to specialists, then a join),
expressed with real orchestrator Agents as nodes:

```go
g := &orchestrator.Graph{
    Name: "support",
    Nodes: []orchestrator.Node{
        {Name: "triage", Runnable: triageAgent, Route: func(r *orchestrator.RunResult) string {
            return classify(r.FinalOutput) // "billing" | "tech"
        }},
        {Name: "billing", Runnable: billingAgent},
        {Name: "tech",    Runnable: techAgent},
        {Name: "reply",   Runnable: replyAgent, Join: true},
    },
    Edges: []orchestrator.Edge{
        {From: orchestrator.StartNode, To: "triage"},
        {From: "triage", To: "billing", When: "billing"},
        {From: "triage", To: "tech",    When: "tech"},
        {From: "billing", To: "reply"},
        {From: "tech",    To: "reply"},
    },
}
res, err := g.Run(ctx, userMessage)
```

**Tested** (`graph_test.go`, all passing under `-race`): conditional routing (only the routed
branch runs), fan-out→fan-in join (both branches merged), and the cycle guard
(`MaxActivations` bounds a mis-specified cyclic graph instead of hanging).

## 4. Deferred to later phases (called out honestly)

- **Cycles / loops.** The scheduler is DAG-oriented; `MaxActivations` is a backstop, not loop
  semantics. Iteration still goes through `LoopAgent` for now. A faithful port would model ADK's
  node re-trigger with a per-node iteration budget.
- **Durable HITL.** ADK 2.0's flagship. The seam here: a `RequestInput` sentinel a node returns to
  pause, plus a `Session` that persists per-node outputs so `Run` can resume. Aida already has
  the durable machinery in `internal/jobs` (ask_user → `awaiting_input` → `input.txt` polling →
  `NotifiedAt` idempotency across daemon restarts) - the work is to expose it as an orchestrator
  `Session` implementation so the resumable-pause capability lives in the *extractable* package,
  not just in the Aida product.
- **Per-node retry/timeout.** ADK has per-node exponential-backoff retry and timeouts. Ours has a
  run-level circuit breaker (3 tool failures) + MaxTurns. Per-node policies would be a `Node.Retry`
  / `Node.Timeout` field wrapping `Runnable`.

## 5. Recommendation

1. **Provider:** no change - keep building on our orchestrator; ADK Go 2.0 stays Gemini-locked.
2. **Adopt the graph layer** (this sketch) as a real phase 7 of the orchestrator: promote
   `graph.go`, add OTel spans per node (reuse the existing `run→turn→tool` tracer, add a
   `graph.node` level), and wire `Graph` into the planner where conditional multi-agent routing
   is wanted.
3. **Then** lift the `internal/jobs` durability into an orchestrator `Session` so a graph node can
   pause/resume durably - matching ADK 2.0's headline while keeping the package extractable
   (`argos`).
