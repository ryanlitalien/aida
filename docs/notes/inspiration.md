# Engineering note: inspiration and prior art

Aida is deliberately an amalgamation of public ideas rather than an invention;
the value added is the integration and the deterministic-middle discipline,
not any single idea. This is the credited list. Every URL below was verified
live before inclusion; where a primary source could not be verified, the work
is named without a link rather than given a guessed one.

Blog part 1's "What I Borrowed" section credits the core of this list; this
note is the extended register.

## The control loop

- **The Ralph pattern** - Geoffrey Huntley,
  ["Ralph Wiggum as a 'software engineer'"](https://ghuntley.com/ralph/), and
  Ryan Carson's reference implementation
  [snarktank/ralph](https://github.com/snarktank/ralph). The deterministic
  outer loop around fresh-context agents, the "externalized state + hard
  feedback loops" trinity, and the `<promise>COMPLETE</promise>` sentinel.
  `aida loop` adopts the control structure and replaces the flat files with
  tasks and semantic recall (`docs/notes/loop-internals.md`).
- **Anthropic, ["Building Effective Agents"](https://www.anthropic.com/engineering/building-effective-agents)**.
  The workflows-versus-agents discipline behind "LLM at the edges,
  deterministic middle": use plain code where the path is knowable, spend
  model autonomy only where it isn't.
- **Harness engineering essays** - Martin Fowler,
  ["Harness engineering for coding agent users"](https://martinfowler.com/articles/harness-engineering.html)
  (the guides-vs-sensors framing); OpenAI,
  ["Harness engineering: leveraging Codex in an agent-first world"](https://openai.com/index/harness-engineering/);
  LangChain,
  ["Improving Deep Agents with Harness Engineering"](https://blog.langchain.com/improving-deep-agents-with-harness-engineering/).
  These shaped the loop's gate/retry design and the eval-runs-as-sensors idea.
  Further pieces informed the harness plan and are credited by name pending
  link verification: Stripe's "Minions" series and "Selective Test Execution",
  Anthropic's "Harness Design for Long-Running Apps", and Addy Osmani's
  "The Code Agent Orchestra".

## Memory

- **Elastic's Atlas** - Noam Schwartz, Elasticsearch Labs,
  ["How we built a persistent agent memory layer on Elasticsearch"](https://www.elastic.co/search-labs/blog/agent-memory-elasticsearch).
  Tiered memory with episodic-to-semantic promotion, decay scoring, and
  supersession - the direct model for the brain's L0–L4 levels and the
  planned consolidate/decay pass.
- **Andrej Karpathy's LLM-maintained wiki idea** - 
  [the "llm-wiki" gist](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f).
  The seed of aida-wiki and the brain's entity pages: a knowledge base the
  LLM keeps current instead of a human.
- **Open Knowledge Format (OKF)** - Google Cloud's v0.1 spec formalizing the
  markdown-bundle-with-frontmatter knowledge pattern:
  [GoogleCloudPlatform/knowledge-catalog](https://github.com/GoogleCloudPlatform/knowledge-catalog)
  and the
  [announcement post](https://cloud.google.com/blog/products/data-analytics/how-the-open-knowledge-format-can-improve-data-sharing).
  aida-wiki is an OKF v0.1 bundle.
- **Karpathy LLM Wiki Obsidian plugin** - GD4AI,
  [obsidian-llm-wiki](https://github.com/GD4AI/obsidian-llm-wiki). Its lint
  taxonomy and alias conventions informed the wiki's own.
- **Letta, ["Context Constitution"](https://www.letta.com/blog/context-constitution/)**
  (companion repo:
  [letta-ai/context-constitution](https://github.com/letta-ai/context-constitution)).
  The memory-first harness argument: agents should own and constitute their
  own context.
- **LangChain, ["Your harness, your memory"](https://blog.langchain.com/your-harness-your-memory/)**.
  The own-your-memory argument - knowledge shouldn't evaporate when a vendor's
  chat window closes - which is half of aida's founding pitch.
- **OpenViking** - ByteDance / Volcano Engine,
  [volcengine/OpenViking](https://github.com/volcengine/OpenViking), a
  "self-evolving context database for AI agents". Convergent evolution rather
  than an input - its L0/L1/L2 progressive loading and session-to-memory
  distillation validated the same designs independently
  (`docs/research-openviking.md`).
- **Voyage AI** - [voyageai.com](https://www.voyageai.com/). The embeddings
  behind every recall path (`docs/notes/technology-rationale.md` §3).

## Orchestration and interop

- **LangChain** - 
  [Deep Agents](https://blog.langchain.com/deep-agents-deploy-an-open-alternative-to-claude-managed-agents/)
  and the broader LangChain era. Aida keeps the useful skeleton (parallel
  fan-out, structured outputs) and removes the LLM-decides-everything part.
- **OpenAI Agents SDK** - 
  [openai/openai-agents-python](https://github.com/openai/openai-agents-python).
  The Agent/Runner/Session/Handoff/Guardrail shapes ported into the internal
  orchestrator (the 2026-04-18 "orchestrator port" in `HISTORY.md`).
- **Model Context Protocol** - Anthropic,
  [modelcontextprotocol.io](https://modelcontextprotocol.io). The interop
  layer in both directions: aida is an MCP server to other agents and an MCP
  client to everything the user already runs.
- **A2A protocol** - Google, now Linux Foundation,
  [a2aproject/A2A](https://github.com/a2aproject/A2A). Agent-to-agent interop;
  the orchestrator carries an A2A client and server.
- **pi-mono's `pi-ai`** - the provider-dispatch pattern credited in
  `internal/llm/provider.go`'s comments (cited by name; no verified canonical
  URL).

## The voice stack

- **whisper.cpp** - Georgi Gerganov / ggml,
  [ggml-org/whisper.cpp](https://github.com/ggerganov/whisper.cpp). Local,
  Metal-accelerated STT; the reason an always-on home microphone never sends
  audio off the box.
- **Piper TTS** - [rhasspy/piper](https://github.com/rhasspy/piper). The
  offline-capable synthesis floor.
- **The Jarvis Piper voice** - jgkawell,
  [huggingface.co/jgkawell/jarvis](https://huggingface.co/jgkawell/jarvis)
  (MIT). The `en_GB-jarvis-high.onnx` voice that gives the butler persona its
  sound; see `internal/jarvis/ATTRIBUTION.md`.
- **wttr.in** - Igor Chubin,
  [chubin/wttr.in](https://github.com/chubin/wttr.in). The free, keyless
  weather backend behind the voice `weather` tool.

## The rule this list exists to honor

Credit generously and link primary sources. The project's credibility rests on
honest lineage: none of these ideas are claimed as original, and the blog
series names them all. What Aida adds is the combination - a deterministic
router over config-as-data, memory that compounds across every surface, and
one person's insistence that the whole thing stay ownable.
