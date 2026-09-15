// Package orchestrator is a provider-agnostic multi-agent coordination
// layer for Go. It ports ideas from Google ADK (LlmAgent, Sequential /
// Parallel / Loop composites, handoff via transfer), the OpenAI Agents
// SDK (Agent + Runner + Handoff + Guardrail), and Anthropic's Claude
// Agent SDK (tool-use loop with circuit breaker and context
// compression) into a single cohesive Go package.
//
// Design principles:
//
//   - One LLM abstraction. The model subpackage defines a minimal
//     model.LLM interface and ships adapters for Anthropic and OpenAI.
//     Callers program against model.LLM; adding a third provider means
//     implementing that interface.
//   - Agents are plain values. An Agent holds instructions, a model, a
//     tool palette, optional handoffs, and optional guardrails. The
//     Runner drives it and does not mutate it.
//   - Handoffs are tool calls. When the model calls a handoff tool,
//     the Runner transfers control to the target agent rather than
//     executing the call. This is how ADK transfer works and how the
//     OpenAI Agents SDK Handoff works.
//   - Guardrails run in parallel with the main work and fail fast.
//     Input guardrails execute before any LLM call; output guardrails
//     execute after the final text is produced.
//   - Sessions are the unit of resumable state. MemorySession is the
//     default; storage-backed Sessions are trivial to add.
//   - Observability is a seam. The Tracer interface lets callers plug
//     OpenTelemetry without dragging in the SDK from the core package.
//
// This package is designed to be extractable as a standalone library.
// It has no imports from the rest of Aida, only the stdlib and the
// two provider SDKs.
package orchestrator
