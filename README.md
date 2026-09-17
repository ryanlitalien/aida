# A.I.D.A. (`aida`)

A.I.D.A. - Artificial Intelligent Digital Assistant - is a CLI-first
"agent of agents" that parses a natural-language question, figures out
which of your own registered sources can answer it, queries them in
parallel, and synthesizes one grounded answer with citations.

## The problem it's solving

A working person's knowledge is scattered across a dozen surfaces: local
codebases, wikis, Notion, Slack, SaaS dashboards, data warehouses,
observability tools, personal notes, and the heads of past selves.
Answering a real question means first knowing *where* to look, then
knowing *how* to ask each tool, then stitching the answers together
yourself. LLM chat helps with the stitching but knows nothing about your
sources; agent frameworks know your sources but hand the routing
decision to a model that hallucinates it.

Aida's answer, and the thing worth reading the code for: **LLM at the
edges, deterministic middle**. The model parses language in and prose
out; the routing decision in between is config you write and can read - 
YAML files, not a prompt the model might ignore. Exactly three core LLM
calls per query (parse, execute, synthesize), plus one bounded router
call. No chain of agents deciding among themselves where to look.

## A worked example

Say you've registered a codebase and a log-query tool for a side
project, `wakanda-api`:

```yaml
# ~/.aida/library/sources/wakanda-api.yaml
type: codebase
entities: [wakanda, vibranium-checkout]

# ~/.aida/library/sources/ops-logs.yaml
type: tool
description: query the error/latency logs for the home services
capabilities: [log-query, error-investigation, api-monitoring]
exec:
  query: 'logtool query "{query}"'
```

```
$ aida "any 500 errors in wakanda-api last night?" --explain

Planner scoring (6 candidates, 2 picked)
  score  source           name  topic  cap   kw  type  route  picked
  -----  ---------------  ----  -----  ---  ---  ----  -----  --------
  130    wakanda-api        30      0    0    0     0    100  → diagnose
  120    ops-logs            0      0   15    2     3    100  → diagnose
  ---  zero-score (considered, cut)  ---
  0      web-search          0      0    0    0     0      0
  0      recipe-box          0      0    0    0     0      0
  0      family-calendar     0      0    0    0     0      0
  0      workshop-notes      0      0    0    0     0      0

Answer
────────────────────────────────────────────────────────────
3 requests 500'd in wakanda-api between 11pm and midnight, all
in the vibranium-checkout handler.
(ops-logs: last-night error query; wakanda-api: handler source)
```

Nothing here is magic: the question named `wakanda-api` (a +30 name
match), a `match_entity: wakanda` route in `routes.yaml` boosted both
relevant sources by +100 and excluded the other four, and `ops-logs`
picked up capability points for advertising `log-query` /
`error-investigation` / `api-monitoring`. Every number in that table
is deterministic and reproducible - the full annotated walkthrough,
including what happens when a past correction changes the score, is in
[`docs/diagrams/routing-walkthrough.md`](docs/diagrams/routing-walkthrough.md).

## Quickstart

```bash
git clone https://github.com/ryanlitalien/aida.git
cd aida
make install       # go install ./cmd/aida, code-signed, symlinked onto PATH
export PATH="$HOME/bin:$PATH"   # or $(go env GOPATH)/bin
aida init          # bootstrap ~/.aida/ from nothing - config, example sources, an empty local-git brain
aida "hello"        # first query
```

Neither install path is on a stock shell's PATH by default - add that
`export` line to your shell rc file (`~/.zshrc`, `~/.bashrc`, etc) so it
persists across terminal sessions.

`aida init`'s closing diagnostic tells you exactly which environment
variables are set, which features that unlocks, and the one-line fix
for each gap - building needs only Go, everything past that is a
runtime key or tool for one specific feature. Full dependency matrix,
platform support, and the honest key chain: [`INSTALL.md`](INSTALL.md).

## How it works

Each query runs a six-step pipeline. Three steps spend LLM tokens; three
are plain deterministic Go:

```
query -> [1 PARSE llm] -> [2 CLASSIFY det] -> [3 RESOLVE det] -> [4 PLAN det] -> [5 EXECUTE llm] -> [6 SYNTHESIZE llm]
```

1. **Parse** (LLM) - natural language into a structured `Intent`
   (action, entities, keywords, timeframe).
2. **Classify** (deterministic) - regex-types the entities (IDs,
   tokens, URLs) and picks a strategy: `lookup`, `query`, `investigate`,
   `record`, `execute`, or `search`.
3. **Resolve** (deterministic) - matches entities against sources'
   `entities:` tokens and `routes.yaml`'s `match_entity` / `match_cwd`.
4. **Plan** (deterministic) - scores every candidate source (name,
   topic, capability, keyword, route buckets) and produces an execution
   plan. `--explain` prints the table above; `--dry-run` shows the plan
   without executing.
5. **Execute** (LLM) - generates each source's actual query and runs
   the adapters in parallel (`errgroup`, bounded fan-out).
6. **Synthesize** (LLM) - aggregates results into one answer where
   every claim cites a specific source.

One bounded router call sits between plan and execute: it picks 1–3
sources from the ranked list, and a past thumbs-down correction applies
a deterministic ±100 to specific sources *before* that prompt is even
built - a correction changes behavior mechanically, not as a prose hint
the model might ignore.

Full diagram: [`docs/diagrams/pipeline.md`](docs/diagrams/pipeline.md).
Architecture pointers for contributors: [`CLAUDE.md`](CLAUDE.md).

## Sources are data, not code

Each knowledge source is one YAML file under `~/.aida/library/sources/`:
type (`codebase`, `docs`, `tool`, `data-source`, `app`, `claude-project`,
`git`), capabilities, `entities:` topic tokens, an optional context doc
fed to the LLM, an optional grep search mode, and an optional `exec`
command template (a load-time validator rejects a bare `{query}`
passthrough - a real command prefix is required, or it's a
shell-injection risk). `aida lint` flags misconfigurations;
`aida index --generate` scans the filesystem and LLM-classifies new
sources, merging without clobbering manual edits. Adding a knowledge
source to Aida is writing a YAML file, not writing Go.

`~/.aida/library/routes.yaml` adds `match_cwd` / `match_entity` routes
that activate layers and sources without ever touching the target
project's own files - the read-only-folder solution.

Worked examples of all three source shapes ship in
[`examples/`](examples/) and `aida init` copies them into a fresh
install so the first query has something to route to.

## Profiles: one binary, partitioned contexts

Sources can be scoped to named profiles (e.g. `work`, `home`), with
auto-detection keyed off tool availability (a work-only CLI on `PATH`
selects the work profile) and `AIDA_PROFILE` as an override. The same
binary answers project-A questions and project-B questions without
either library contaminating the other - most agent frameworks assume
one context; real people have at least two.

## Feedback that changes behavior mechanically

```bash
aida thumbs-down <run-id> --because "no need for web-search, should have used ops-logs"
```

A polarity-tracking directive extractor pulls `intendedSources` and
`excludedSources` out of that free-text reason (handling negation:
"no need for", "do not use", "instead of"). Those persist as an
embedded lesson; the next similar query's router applies a deterministic
±50/±100 from the top-3 most-similar past lessons before the prompt is
built. See
[`docs/notes/feedback-mechanics.md`](docs/notes/feedback-mechanics.md).

## Memory that compounds

Aida's memory is a tiered stack - L0 ephemeral run records up through
L4 consolidated knowledge - with automated hooks doing the promotion
(thumbs feedback, coding-agent session harvests, periodic sweeps)
instead of manual curation. Files are always the source of truth;
`brain.db` is a derived index that rebuilds itself, detached, when
stale.

```
~/.aida/runs/              L0  every query's record
~/.aida/brain/lessons/     L1  thumbs feedback
~/.aida/brain/memory/      L2  captured agent memories
~/.aida/brain/knowledge/   L3  entity + domain pages
~/dev/aida-wiki/           L4  consolidated wiki (separate repo, optional)
```

| Level | What | Written by |
|---|---|---|
| L0 | Run records, `--explain` traces | Every query |
| L1 | Feedback lessons (embedded) | Thumbs up/down, voice notes |
| L2 | Captured coding-agent memories (Claude Code, Codex, Gemini) | Capture hooks + LLM-distilled session harvests |
| L3 | Curated entity pages, domain knowledge | Compile passes |
| L4 | Consolidated wiki | `aida brain consolidate` + decay scoring |

L0 through L3 all live under `~/.aida/`, written by machines as things
happen: append-only, cheap, self-correcting through supersession. L4
sits outside that tree on purpose. A wiki holds two things the brain
should not - the raw archive corpora being folded in, and prose a human
has reviewed - and material only graduates into it once a chapter stops
changing, so a wiki page is a standing claim rather than an observation.
The format is an [Open Knowledge Format](https://github.com/google/open-knowledge-format)
v0.1 bundle: markdown plus YAML frontmatter, `index.md` for progressive
disclosure, paths as identity. Point aida at yours with `wiki.path` in
`~/.aida/config.yaml` (default `~/dev/aida-wiki`), then `aida wiki index`
to make it a recall channel. It is entirely optional - nothing else in
aida needs it to work.

Diagram: [`docs/diagrams/memory-levels.md`](docs/diagrams/memory-levels.md).

## MCP server

`aida serve` auto-detects its invocation and runs one of two modes: a
stdio JSON-RPC **MCP server** when stdin is a pipe (so Claude Code,
Cursor, and other MCP clients can spawn it directly), or an HTTP daemon
when stdin is a TTY. As an MCP server it exposes brain search/recall
and task CRUD to other agents - `brain_search`, `brain_recall`,
`tasks_list`, `tasks_add`, `tasks_done`, and more. As an MCP *client*
(a separate surface - `internal/mcp/discovery.go`), Aida discovers
whatever stdio/HTTP MCP servers your own Claude Code config already
declares, so it can use them too.

## Voice (macOS only)

`aida serve` also runs an always-on voice assistant: say *"Hey Jarvis,
what's the weather"* and hear a spoken reply. Fully local, no Python at
runtime: ffmpeg mic capture → amplitude VAD → whisper.cpp STT (Metal) →
Claude with in-process tool dispatch → Piper TTS (MIT-licensed voice,
bundled) or ElevenLabs. It's macOS-only by construction - `afplay`,
`osascript`, and Metal-accelerated whisper.cpp have no portable
equivalent here - but the engine itself has no such dependency and
cross-compiles cleanly to `linux-amd64`.

```bash
make jarvis-deps        # brew deps, build piper from source, download models
aida jarvis greet       # foreground; triggers the macOS mic permission prompt
aida serve               # always-on daemon: HTTP :1610 + voice listener
```

Full reference: [`docs/JARVIS.md`](docs/JARVIS.md). Voice pipeline deep
dive with the latency budget:
[`docs/notes/voice-pipeline.md`](docs/notes/voice-pipeline.md).

## The autonomous loop

`aida loop` is a Ralph-style deterministic outer loop: it repeatedly
spawns fresh-context agents against a task queue, one highest-priority
task at a time, running a quality gate (`--check "make test"`) after
each attempt and parking failures on `hold` instead of silently
re-spinning. `--worktree` isolates each task in its own git worktree;
`--pr` opens a PR per passing task behind an optional adversarial review
panel instead of completing it directly - the loop never merges.

```bash
aida loop plan "add priority to tasks: schema, badge, filter" --tag prio
aida loop --tag prio --check "make test"
```

Deep dive: [`docs/notes/loop-internals.md`](docs/notes/loop-internals.md).
Lifecycle diagram:
[`docs/diagrams/loop-lifecycle.md`](docs/diagrams/loop-lifecycle.md).

## Portability: code, config, and memory are three repos

```bash
aida                     # code (this repo) - public
~/.aida/                 # config: sources, routes, roster - private, yours
~/.aida/brain/           # memory: everything you've ever told it - private, yours
```

`aida init` bootstraps an empty `~/.aida/` and a local-git `brain/` so a
new machine is one clone plus one command. The config and brain
directories are never published - see INSTALL.md's "Backing up your
config and memory" section before pointing either at a remote of your
own. Full picture:
[`docs/diagrams/three-repo-cut.md`](docs/diagrams/three-repo-cut.md),
[`docs/diagrams/ecosystem-map.md`](docs/diagrams/ecosystem-map.md).

## Commands

```bash
# Queries
aida "your question"                    # run the full pipeline
aida --explain "your question"          # print the planner scoring table
aida --dry-run "your question"          # show the plan without executing
aida --offline "your question"          # use a local Ollama model instead of Anthropic
echo "your question" | aida             # piped input

# Tasks (brain-native, six statuses, optional GitHub Issues mirror)
aida tasks                              # list open + in-progress tasks
aida tasks add "title" --tag project:x  # create a task
aida tasks done <ref>                   # complete by slug, partial slug, or '#N'
aida tasks web                          # loopback web UI for browsing/filtering

# Brain (shared memory)
aida brain stats                        # lesson / entity / task counts, DB size
aida brain index                        # rebuild brain.db from files
aida brain search "query"                # semantic search over lessons + entities
aida brain recall "query"                 # recall captured coding-agent memories

# Feedback (the learning loop)
aida thumbs-up <run-id>
aida thumbs-down <run-id> --because "..."

# MCP server / always-on daemon (port 1610 = Earth-1610)
aida serve                              # HTTP daemon + voice listener (or stdio MCP, auto-detected)
aida serve --no-listen                  # HTTP only, no mic

# Voice (see the Voice section above)
aida jarvis ask "question"              # typed -> spoken reply, no mic
aida jarvis greet                       # TTS smoke test

# Autonomous loop
aida loop plan "<goal>" --tag <tag>     # decompose a goal into tasks
aida loop --tag <tag> --check "<cmd>"    # drive the loop with a quality gate

# Library
aida lint                                # validate library sources
aida index --generate                    # scan + classify sources into the library

# Wiki (optional L4 - a separate OKF repo you point aida at)
aida wiki index                          # index the wiki into brain.db for recall
aida wiki lint                           # audit it for dead links, orphans, drift

# Dispatcher + roster (named agent call-signs)
aida ask <who> "<task>"                  # dispatch to a named roster entry
aida roster list                         # show the configured roster
```

## Development

```bash
make build          # build to bin/aida (not on PATH - see CONTRIBUTING.md)
make install        # go install to $(go env GOPATH)/bin/aida, code-signed, symlinked
make test           # go test ./...
make build-all      # cross-compile darwin-arm64, darwin-amd64, linux-amd64
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the full test/golden-eval
workflow and the commit policy.

```
cmd/aida/           entry point
internal/
  cli/              cobra command handlers
  config/           ~/.aida/config.yaml loaders
  library/          multi-root library aggregation, routes, layers, manifest
  engine/           the six-step pipeline (parser, classifier, resolver, planner, executor, synthesizer)
  llm/              Anthropic SDK wrapper, prompt templates, JSON schemas
  sources/          adapters (sqlite, git, grep, notion, exec, csv)
  brain/            memory: lessons, entities, tasks, the multi-agent harvest bridge
  jarvis/           voice layer
  roster/, dispatch/  named agent call-signs and the dispatcher
docs/
  notes/            engineering deep dives (technology rationale, failure stories, inspiration, ...)
  diagrams/         mermaid diagrams (pipeline, memory levels, routing walkthrough, ...)
```

## Learn more

- [`docs/notes/technology-rationale.md`](docs/notes/technology-rationale.md) - why Go, why SQLite-as-index, why Voyage, and eight more X-over-Y decisions
- [`docs/notes/failure-stories.md`](docs/notes/failure-stories.md) - ten real failures and their fixes, verified against git history
- [`docs/notes/inspiration.md`](docs/notes/inspiration.md) - the credited prior art: Ralph-style loops, Elastic Atlas, Karpathy-style wikis, MCP, and more
- [`docs/notes/lmd-protocol.md`](docs/notes/lmd-protocol.md) - the phone client's bind-and-token story (full wire spec: [`docs/lmd-protocol.md`](docs/lmd-protocol.md))
- [`HISTORY.md`](HISTORY.md) - the curated development timeline this repo shipped from

## License

MIT
