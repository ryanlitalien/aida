# HISTORY

Aida's public repo starts from a fresh initial commit; the original development history stays private. This file is the curated record of that history: what was built, when, in what order, distilled from 600+ commits across 48 PRs between 2026-02-27 and the public cut. Identifying details are omitted; the engineering is not. Every PR number in this file (and elsewhere in this repo's docs) refers to the archived private repo, not this one - issue and PR numbering starts over here.

## 2026-02-27 - Hermes, the one-day v0 (8 commits)

The whole first version landed in a single day as `hm`, the "Hermes" CLI: a natural-language question in, parallel queries out to a data warehouse (Snowflake) and an observability stack, one synthesized answer back.

- LLM-extracted SQL with a 10-minute timeout and multi-statement parsing fixes, ID-pattern recognition, query templates
- Deep links into the observability UI, per-stage timing, a copy-pasteable "shareable summary"
- Auto-release CI on push to main from day one

## 2026-04 - The engine takes shape (163 commits)

A five-week gap, then the second burst turned a demo into an architecture.

- 04-07: the agentic library (config-driven source registry), the LLM router, and the learning loop land together
- 04-08: golden questions arrive - a 40-question routing audit that grows into a 148-question seed set with graded answers; run-goldens harness
- 04-09: feedback becomes a surface: `+1`/`-1` aliases, directive extraction from free-text reasons, lesson weighting, re-query detection; `soul.yaml` (user identity + routing preferences); the LLM layer refactors into a Provider interface with multi-provider dispatch and cost tracking; Ollama + OpenAI-compatible backends
- 04-11: the brain: shared memory with embeddings and confidence scoring, then brain-native tasks the same day (natural-language creation, stable IDs, a one-way GitHub Issues mirror)
- 04-12 to 04-16: observability, MCP server + client, sessions (`-c` continue), skills, delegation; web search adapter (Brave + DuckDuckGo fallback); agent mode with tool loops; reliability pass (circuit breaker, audit trail, guardrails); a fix for the brain index rebuilding on every query (a 10-second tax nobody had noticed)
- 04-17: `execx`, orphan-safe subprocess execution, replaces every raw exec call site
- 04-18: the orchestrator port - six phases in one day: provider-agnostic model layer, Agent/Runner/Session/Handoff/Guardrail, Sequential/Parallel/Loop composites, A2A client + server, OpenTelemetry tracing. The engine's execute step is then rewired through it over the following days
- 04-23 to 04-28: task statuses grow to six, MCP task tools, partner-aware routing, the daily briefing command, a local tasks web UI

## 2026-05 - The voice month (91 commits)

- 05-04: a SessionStart hook makes every coding session pull + rebuild on a clean tree
- 05-06: the "full closed loop" harness PR: sensors, layered/typed memory, lazy tools, durable artifacts; the per-profile background jobs queue
- 05-10: Jarvis is born in a single-day burst of ~30 commits: ffmpeg mic capture, amplitude VAD, whisper.cpp STT, wake-phrase regexes, in-process voice tools, Piper TTS as a native binary (no Python at runtime), an NDJSON audit log, LaunchAgent autostart, a "One moment, sir" ack to mask engine latency
- 05-11: voice task tools, MCP discovery from the client side (find-then-call), background jobs get `awaiting_input` + an `ask_user` tool - the assistant can now pause an agent, ask you a question by voice, and resume it with your answer
- 05-21 to 05-27: ElevenLabs TTS with Piper fallback, then streamed playback for latency; voice feedback tools writing to a lessons table with embedding recall; casting replies to room speakers (later reverted - the latency never justified the complexity)

## 2026-06 - Autonomy (42 commits)

- 06-04: `loop`, the Ralph-style autonomous driver, is sketched with its design doc: a deterministic outer loop spawning fresh-context agents against the task queue
- 06-17: the loop builds out in one day: per-task git worktree isolation, budget stops, `--concurrency` fan-out, daemon mode, autonomous PR creation behind an adversarial review panel, human-in-the-loop approval gates, Docker microVM sandboxing (a Seatbelt fallback was tried and dropped)
- 06-18: the serve daemon learns to host the loop dispatcher in-process; a hardening pass driven by adversarial review of the loop's own output
- 06-22: `swarm`, a natural-language front door to the loop; a `claude -p` connector fallback for questions the engine can't answer itself
- 06-23: the repo adopts granular-commits-never-squash as written policy
- 06-24: the echo saga: the always-on mic was hearing the assistant's own TTS and re-answering itself; fixed by flushing self-audio after speaking. Same day: daemon port takeover, durable voice thread memory, pronounceable job handles ("phantom job" grounding guard included)

## 2026-07 - Identity, memory, surfaces (282 commits, the peak)

- 07-02: the orchestrator gains a directed graph engine (typed shared state, reducers, durable checkpointing, resumable graphs) and a multi-agent investigate graph
- 07-05: wiki integration: consolidation (event-to-fact promotion with provenance), decay + use-count recall scoring, wiki lint/index/recall
- 07-05/06: the great renames. Hermes becomes K.E.V.I.N. in a 13-commit sweep; A.I.D.A. replaces it within a day in a second full sweep (module path, binary, config dir, env prefix, launchd label, run-id sentinels). Rename checklists for the satellite machines follow
- 07-07: the time-routing PR: a zero-LLM fast path for time/date, refusal-marker classification, `none_viable` router escape, lesson supersession + retraction, volatile-data guards in prompts
- 07-08: Aida becomes a voice persona - a cosmetic twin of Jarvis sharing one brain
- 07-13 to 07-20: push-to-talk on a physical USB button via IOKit HID; the hard-won lesson that adhoc codesigning silently voids the macOS Input Monitoring grant on every rebuild (fix: sign with a stable Developer ID); explicit TCC state checks that warn loudly instead of failing silently
- 07-17: the coding-agent memory bridge: a capture hook mirrors memory files into the brain, with scope-aware semantic + recency recall exposed via CLI, MCP, and a voice tool
- 07-19: Tier-1 answer cache (mine past runs, short-circuit repeat questions before the pipeline) - followed within two days by admission and TTL guards after it served stale answers to time-sensitive questions; embedding model migration with a full-corpus `reembed` command; a large voice-tool bugfix batch
- 07-20 to 07-23: fleet dashboard pages on the daemon; the dispatcher + roster (named agent call-signs over subagent/source/MCP/job backends, live persona discovery); the L.M.D. listener - a tailnet-only authenticated HTTP surface for the Android client, deliberately separate from the loopback daemon
- 07-21: a TTS model A/B harness with recorded results decides each persona's voice model

## 2026-08 - Many models, many agents (22 commits so far)

- 08-14/15: a Google/Gemini provider joins Anthropic/OpenAI/Ollama; offline-mode provider resolution; the model pricing catalog refreshes
- 08-18: the multi-agent memory bridge: session-transcript harvesters for two more coding agents (LLM-distilled, watermarked, incremental), wired into setup hooks plus a periodic sweep in the daemon
- 08-21: the open-source plan lands - the audit, decisions, and phased path that produced the repo you are reading

## 2026-09 - The open-source cut

- 09-03: blog part 1 ships on the author's personal site, ahead of the repo flip
- 09-08: Phase 0 merges: the MIT license, a scrub of personal/former-employer/merchant data across code and docs, and a CI-enforced regression scanner for the patterns that scrub removed
- 09-09 to 09-12: the remaining work splits into three parallel chunks, each its own branch and PR, sized for one autonomous agent apiece. Chunk A (code) drops the partner-registry concept entirely - routing keeps working off sources' `entities:` tokens and `routes.yaml` route boosts alone - bootstraps `aida init` to build a working `~/.aida/` from nothing, generalizes the employer-specific ID-pattern extractor into configurable patterns, and ships a fictional `examples/` tree the golden-eval harness runs against out of the box. Chunk C writes ten engineering notes (technology rationale, failure stories, inspiration, and four subsystem deep dives) and eight mermaid diagrams as the shared source the public repo and the blog both draw from
- 09-13: chunk B - the public-quality docs pass: this file's own update, a rewritten README and CLAUDE.md, a new INSTALL.md and CONTRIBUTING.md, and a docs/plans inventory that trimmed or deleted every file naming a private repo, a real coworker, or personal operational detail

## Reading the arc

Three months in, the shape is consistent: bursts of building (a subsystem lands in a day), followed by days of hardening driven by real failures - the echo misfire, the silent TCC revocation, the stale cache, the orphaned agents. The failure-and-fix pairs are documented deliberately; they are the most transferable part of the history.
