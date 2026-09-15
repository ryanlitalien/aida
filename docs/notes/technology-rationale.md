# Engineering note: technology rationale (why X over Y)

Ten decisions, each with the alternative considered and the one reason that
decided it. Written from the actual code and git history, not from memory; no
entry here lacks a real decision behind it. Commit references point at the
private development history (see the note at the top of `HISTORY.md`).

## 1. Go over Python/TypeScript

The alternative was the default agent-framework stack: Python. What decided it
was deployment: a single static binary that runs on satellite machines with no
runtime environment at all. `CGO_ENABLED=0` plus the pure-Go SQLite driver
(`modernc.org/sqlite`) means `make build-all` cross-compiles darwin-arm64,
darwin-amd64, linux-arm64, and linux-amd64 in seconds, and the autonomous loop
exploits it directly - it cross-compiles a linux/arm64 `aida` on the fly to
provision into each Docker sandbox. Goroutines giving the parallel fan-out for
free was the bonus, not the reason; the voice layer even replaced its one
Python dependency with a native Piper binary ("no Python at runtime",
2026-05-10) to keep the property.

## 2. SQLite + files-as-truth over a vector database

The alternative was a dedicated vector store (Pinecone/Chroma/pgvector-style).
What decided it: the brain had to be a git repository - portable, diffable,
recoverable - and a vector database can't be one. So markdown files are the
source of truth and `brain.db` is a *derived index* (the schema comments say
exactly that) holding embeddings as BLOB columns; staleness detection
(`IsStale`: 3+ files newer than the db) triggers a detached background rebuild
rather than blocking a query. Losing the database costs a re-index, never
data, and a new machine is a `git clone`.

## 3. Voyage embeddings over OpenAI or local models

The alternative was OpenAI embeddings (the obvious default) or a local model.
What decided it was retrieval quality per dollar at personal-corpus scale:
`voyage-4-lite` at 512 output dimensions (a Matryoshka pin chosen to stay
comparable with the previous model's vectors) costs ~$0.02/M tokens. The
decision was then stress-tested for real: the voyage-3-lite → voyage-4-lite
migration (2026-07-19) shipped alongside an `aida brain reembed` command that
re-embeds the full corpus in one command - proving the vendor is a config
swap, not an architecture commitment.

## 4. whisper.cpp local STT over cloud STT

The alternative was any cloud STT API, which would transcribe faster than a
laptop on some days. What decided it: this microphone is *always on* in a
family home, and that audio never leaves the box, full stop. whisper.cpp with
Metal acceleration makes local transcription fast enough (hundreds of ms on
`small.en`); the accuracy gap on jargon is closed with a decode-bias prompt of
recurring proper nouns rather than a bigger model. The default moved from
`tiny.en` to `small.en` (2026-07) when measurement showed the ~100–300 ms cost
disappeared under the ack phrase anyway.

## 5. Piper TTS baseline + ElevenLabs streaming upgrade

The alternative framings were "local only" (free, robotic) or "cloud only"
(better voice, dies offline, costs per word). What decided the hybrid: the
voice layer must always be able to speak, so the MIT-licensed Piper voice is
the floor and ElevenLabs is an upgrade the code treats as optional
(`Synthesizer` is the interface; `StreamingSynthesizer` is a capability, not a
requirement). The vendor's model choice was decided by an A/B harness, not the
vendor's guidance: 26 measured requests showed `turbo_v2_5` at 309 ms
time-to-first-byte with σ31 ms beating the recommended `flash_v2_5` (379 ms,
σ162 ms) - the jitter, not the mean, was disqualifying for spoken replies.
Streaming playback was added purely to cut time-to-first-sound to
first-chunk latency.

## 6. Anthropic primary + multi-provider fallback

The alternative was binding to one vendor's SDK everywhere. What decided the
`Provider` interface (Anthropic, OpenAI-compatible, Gemini via its
OpenAI-compat endpoint, Ollama for offline): the whole architecture bet is
"LLM at the edges, deterministic middle", and that bet is only honest if the
edges are swappable - the deterministic middle must not care whose model
parses and synthesizes. In practice Claude Haiku is pinned as the primary
because it cut wall-clock 5–10x on the pipeline's three calls, and offline
resolution falls through to a local Ollama model; the dispatch pattern credits
pi-mono's `pi-ai` provider dispatch in the code comments.

## 7. MCP in both directions over bespoke plugins

The alternative was a proprietary plugin system for tools in and an ad-hoc API
for other agents to reach the brain. What decided MCP both ways: one protocol
gets an entire existing ecosystem in each direction - as a *server*, aida
exposes brain and task tools to any MCP client; as a *client*, it discovers
whatever servers the user's own configs already declare, with zero
aida-specific integration work per tool. The one wrinkle is recursion: the
discovery layer hard-skips the `aida` server so an agent asking aida can't
route back into aida, a guard important enough to be recorded as standing
feedback (`feedback_no_mcp_query_tool`).

## 8. Tailscale-only bind + bearer token for the phone surface

The alternative was widening the existing loopback daemon's bind (or a public
endpoint behind TLS + auth). What decided the separate `:1218` listener: the
`:1610` surface is unauthenticated *by design* - loopback is its auth - so the
phone had to get its own listener with its own mux, bound to the resolved
Tailscale CGNAT address and never `0.0.0.0` (no tailnet: the listener doesn't
start, rather than falling back to a wildcard bind). The bearer token is
Crockford base32 with input normalization and a constant-time compare because
it gets typed from a terminal into a phone exactly once. Full reasoning in
`docs/notes/lmd-protocol.md`.

## 9. Cobra + lipgloss for the CLI; errgroup/semaphore over a queue system

The CLI alternative was hand-rolled flag parsing; Cobra decided itself on
subcommand scale (the binary grew past twenty subcommands) and lipgloss's
adaptive colors handle light/dark terminals without a theming layer. The more
interesting refusal is parallelism: the alternative was a real job-queue
system, and what decided against it is that a bounded in-process semaphore is
the entire requirement - the original executor used `errgroup` with
`SetLimit(5)`, and its orchestrator successor uses a channel semaphore with
the same shape. Every unit of work is request-scoped; durable queueing exists
only where it earns its keep (the background jobs store), not in the query
path.

## 10. `claude --print` subprocess reuse over direct API integrations

The alternative was writing first-party API clients for Notion and every
similar service. What decided the subprocess pattern: the Claude CLI already
holds the MCP connections *and the auth* - shelling out to `claude --print`
with a scoped `--allowedTools` list reuses both, so a new integration costs a
config line instead of a client library. The tradeoffs are real and the code
carries their scars: subprocess creds must be scrubbed so the CLI uses its
subscription OAuth instead of billing an inherited API key, argument parsing
needed a `--` sentinel to stop `--allowedTools` swallowing the query, and a
one-hop depth guard (`AIDA_DISPATCH_DEPTH`) keeps agents-spawning-agents from
recursing. Accepted, because the pattern turns "integrate a service" into
"name a tool".
