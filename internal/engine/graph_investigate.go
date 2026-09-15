package engine

import (
	"context"
	"strings"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

// This file assembles the orchestrator's graph primitives into one real,
// multi-agent workflow - the "investigate" supervisor - to exercise the
// graph engine (fan-out, fan-in join, bounded cycles, shared state) on a
// live task. It is the opt-in wire-in from the graph-layer plan: the
// default single-agent path is unchanged; `aida --agent --graph` runs this
// instead.
//
// Topology:
//
//	          ┌──> evidence ─┐
//	triage ──►├──> analysis ─┼──► collect(join) ──► synthesize ⇄ evaluate
//	          └──> risks ────┘                          ▲___________│ (revise)
//
// triage frames the question; three specialist agents investigate it in
// parallel through distinct lenses (each with the full tool palette);
// collect barriers their findings; synthesize drafts an answer; evaluate
// critiques it and either routes "revise" back to synthesize (the
// evaluator-optimizer loop) or ends, at which point the graph's OutputKey
// projects the final draft from shared state.

// InvestigateConfig tunes the investigate graph. Zero values pick sane
// defaults.
type InvestigateConfig struct {
	// SpecialistMaxTurns caps each specialist agent's tool-use loop.
	SpecialistMaxTurns int
	// MaxRevisions bounds the synthesize⇄evaluate loop. The evaluator
	// forces "done" once this many drafts have been produced.
	MaxRevisions int
	// SystemContext is the shared grounding - the resolved-entity /
	// context-doc prompt the single-agent path
	// injects. It becomes each agent's cacheable InstructionsPrefix so
	// graph mode is grounded exactly like the non-graph path, with the
	// per-role instruction layered on top. Empty is fine (agents just
	// get their role prompt).
	SystemContext string
}

// specialistLens is one investigative angle. Keeping them data-driven
// makes the panel easy to extend.
type specialistLens struct {
	name   string
	prompt string
}

var investigateLenses = []specialistLens{
	{"evidence", "You are the EVIDENCE specialist in an investigation. Use the available tools to gather concrete, verifiable facts relevant to the question: search code, logs, docs, and data. Report only what you can substantiate, each finding with its source. Do not speculate."},
	{"analysis", "You are the ANALYSIS specialist in an investigation. Reason about mechanisms, causes, and how the parts fit together. Use tools to confirm your reasoning where possible. Produce a tight causal explanation, flagging assumptions explicitly."},
	{"risks", "You are the RISKS specialist in an investigation. Surface edge cases, failure modes, counter-evidence, and what the obvious answer might miss. Argue the skeptical case. Use tools to check your concerns against reality."},
}

const (
	investigateTriagePrompt = "You are the TRIAGE lead of an investigation. Restate the question as a crisp investigation brief: what precisely must be determined, what a good answer looks like, and which facts would settle it. Keep it to a few sentences; the specialists read this brief."

	investigateSynthPrompt = "You are the SYNTHESIS lead. Combine the specialists' findings into one grounded, well-structured answer to the original question. Prefer substantiated evidence; note where the analysis and risks perspectives disagree. If a prior draft and an evaluator's critique are included in your input, revise the draft to address the critique."

	investigateEvalPrompt = "You are the EVALUATOR. Judge whether the draft answer is well-supported, addresses the question, and has no obvious gap. Reply with a short critique. End your reply with exactly one line: 'VERDICT: APPROVED' if the draft is good enough to ship, or 'VERDICT: REVISE' with the single most important fix needed."
)

// BuildInvestigateGraph assembles the investigate supervisor over the
// given provider-agnostic model and tool palette. The same graph works
// with any model.LLM (Anthropic or OpenAI) - no vendor coupling.
func BuildInvestigateGraph(llm model.LLM, tools []orchestrator.Tool, cfg InvestigateConfig) *orchestrator.Graph {
	if cfg.SpecialistMaxTurns <= 0 {
		cfg.SpecialistMaxTurns = 8
	}
	if cfg.MaxRevisions <= 0 {
		cfg.MaxRevisions = 2
	}

	nodes := []orchestrator.Node{
		{Name: "triage", Runnable: &orchestrator.Agent{
			Name: "triage", InstructionsPrefix: cfg.SystemContext, Instructions: investigateTriagePrompt, Model: llm, MaxTurns: 2,
		}},
	}
	edges := []orchestrator.Edge{{From: orchestrator.StartNode, To: "triage"}}

	// Specialists fan out from triage and fan in to collect.
	for _, l := range investigateLenses {
		nodes = append(nodes, orchestrator.Node{Name: l.name, Runnable: &orchestrator.Agent{
			Name: l.name, InstructionsPrefix: cfg.SystemContext, Instructions: l.prompt, Model: llm, Tools: tools, MaxTurns: cfg.SpecialistMaxTurns,
		}})
		edges = append(edges,
			orchestrator.Edge{From: "triage", To: l.name},
			orchestrator.Edge{From: l.name, To: "collect"},
		)
	}

	// collect is the fan-in barrier for the specialists only (no back-edge),
	// which keeps the join out of the synthesize⇄evaluate cycle.
	collect := orchestrator.RunnableFunc(func(ctx context.Context, in string, opts ...orchestrator.RunOption) (*orchestrator.RunResult, error) {
		return &orchestrator.RunResult{FinalOutput: in}, nil
	})
	nodes = append(nodes, orchestrator.Node{
		Name: "collect", Runnable: collect, Join: true,
		Merge: func(findings []string) string {
			var b strings.Builder
			for i, f := range findings {
				if i > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(f)
			}
			return b.String()
		},
	})

	// synthesize wraps the synthesis agent in a stateful node so each
	// draft is recorded in state (draft + revision count). It is a normal
	// node (not a join), so the evaluator's back-edge re-triggers it.
	synthAgent := &orchestrator.Agent{Name: "synthesize", InstructionsPrefix: cfg.SystemContext, Instructions: investigateSynthPrompt, Model: llm, MaxTurns: 3}
	synthesize := orchestrator.StateFunc(func(ctx context.Context, in string, st *orchestrator.State, opts ...orchestrator.RunOption) (*orchestrator.RunResult, error) {
		res, err := synthAgent.Run(ctx, in, opts...)
		if err != nil {
			return nil, err
		}
		st.Set("revisions", revisionCount(st)+1)
		st.Set("draft", res.FinalOutput)
		return res, nil
	})

	evalAgent := &orchestrator.Agent{Name: "evaluate", InstructionsPrefix: cfg.SystemContext, Instructions: investigateEvalPrompt, Model: llm, MaxTurns: 2}

	nodes = append(nodes,
		orchestrator.Node{Name: "synthesize", Runnable: synthesize, MaxVisits: cfg.MaxRevisions + 1},
		orchestrator.Node{Name: "evaluate", Runnable: evalAgent, MaxVisits: cfg.MaxRevisions + 1,
			RouteState: func(res *orchestrator.RunResult, st *orchestrator.State) string {
				// Force done at the revision bound; otherwise honor the
				// evaluator's VERDICT line. Match the verdict token, not a
				// bare "approved" substring, so critique prose like "not yet
				// approved" doesn't short-circuit the loop. REVISE wins ties.
				if revisionCount(st) >= cfg.MaxRevisions {
					return "done"
				}
				verdict := strings.ToUpper(res.FinalOutput)
				if strings.Contains(verdict, "VERDICT: REVISE") {
					return "revise"
				}
				if strings.Contains(verdict, "VERDICT: APPROVED") {
					return "done"
				}
				return "revise" // no clear verdict → take another pass
			}},
	)
	edges = append(edges,
		orchestrator.Edge{From: "collect", To: "synthesize"},
		orchestrator.Edge{From: "synthesize", To: "evaluate"},
		orchestrator.Edge{From: "evaluate", To: "synthesize", When: "revise"},
		// "done" has no edge: the path ends and OutputKey projects the draft.
	)

	return &orchestrator.Graph{
		Name:           "investigate",
		Nodes:          nodes,
		Edges:          edges,
		OutputKey:      "draft",
		MaxConcurrency: len(investigateLenses) + 1,
	}
}

// revisionCount reads the running revision counter from shared state.
// It tolerates float64 because a durable checkpoint's JSON round-trip
// decodes numbers as float64, not int.
func revisionCount(st *orchestrator.State) int {
	v, ok := st.Get("revisions")
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	default:
		return 0
	}
}
