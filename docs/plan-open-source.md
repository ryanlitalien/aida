# Plan: Open-sourcing Aida

**Branch**: `main` · **Status**: public repo live at github.com/ryanlitalien/aida (cut 2026-09-15); see Checklist · **Deliverables**: (1) a public `aida` repo a stranger can clone, build, and run; (2) a blog series about building it and the projects it spawned.

This is the umbrella plan. It covers what Aida is (the narrative the README and the blog both need), what actually blocks publication (a concrete audit of this repo as of 2026-08-21), what happens to each satellite repo (aida-config, aida-brain, aida-wiki, aida-android, aida-agents, life-log), and the phased workplan to get there.

---

## Checklist (consolidated 2026-09-11)

The single to-do list for this plan and `docs/plan-oss-launch.md`. Phase 0 (the scrub) merged in PR #168 on 2026-09-08 and is not repeated here. The remaining work is grouped into three chunks sized for one autonomous agent each, chosen so no two chunks edit the same files: chunk A is Go code under `internal/` plus `examples/`, chunk B is prose in the repo root and `docs/`, chunk C is new engineering notes and diagrams under `docs/`. Each chunk is a branch and one PR; the loop never merges. A fourth list holds what only Ryan can do, and a fifth holds non-blocking "coming soon" work announced at launch but not gating it. Tick items here as they land; the phase text in Part 5 stays as the rationale.

Ground rules for any agent taking a chunk: work on a branch off `main` in its own worktree, granular commits, never squash, `go build ./... && go vet ./... && go test ./... && scripts/oss-scan.sh` green before the PR, no employer names, no real partner or family data, fictional seeds only (MCU flavor is fine). Read Part 4 and Part 6 of this document first.

### Chunk A - Runnable by a stranger (code)

Goal: `git clone && make install && aida init && aida "hello"` works on a machine that has never seen `~/.aida/`. Owner: an agent. Files: `internal/cli/init*.go`, `internal/config/`, `internal/brain/`, `internal/ui/output.go`, `internal/jarvis/` (minecraft tool), `internal/cli/fleet*.go`, `examples/`, `scripts/run-goldens.sh`, `cmd/golden-log/`.

- [x] Drop the partner registry concept entirely (decided 2026-09-12, superseding the earlier rename-to-entities decision): it was an employer-tenure feature with no value to anyone else. Remove `partners.yaml`, `partners/<profile>.yaml`, `checkout_token_patterns`, the resolver's registry lookups (`FindByAlias`, `FindByCheckoutToken`), the classifier's checkout-token entity type, the planner's `+50` registry boost, and the `aida partners` CLI. Routing keeps working off what already exists without the registry: sources' `entities:` token lists and `routes.yaml` `match_entity`. Sweep Part 2 of this doc and CLAUDE.md prose in the same PR. Step-by-step execution order: `docs/plan-remove-partners.md`
- [x] `aida init` bootstraps `~/.aida/` from nothing: `config.yaml`, `library.yaml` with one root, empty `library/sources/`, `library/routes.yaml`, a brain directory initialized as a local git repo (`brain.InitRepo`), and copies of the `examples/` sources so the first query has something to route to
- [x] `aida init --brain-remote <url>` and `--config-remote <url>` set a private backup remote at bootstrap (wiring only: `brain.InitRepo` already takes a remote and `CommitAndPush` already skips the push when no `origin` exists)
- [x] `aida init` is idempotent: re-running on an existing `~/.aida/` reports what is present and changes nothing without `--force`
- [x] `examples/` holds worked files: two or three source YAMLs (a codebase, a docs dir with `search.mode: grep`, an exec tool with a real command prefix) with fictional `entities:` tokens, and `roster.yaml` promoted from `docs/roster.yaml.example` (delete the old location, update references)
- [x] `examples/README.md` explains each file and how `aida init` uses them
- [x] Fake-key example env files: `examples/env.example` (every env var the binary reads - `ANTHROPIC_API_KEY`, `VOYAGE_API_KEY`, optional ElevenLabs/LiteLLM - with obviously-fake values like `sk-ant-EXAMPLE-not-a-real-key`) and `examples/op.env.example` (the 1Password service-account token shape, fake value, comment pointing at 1P service-account docs); `aida init` copies both into `~/.aida/` as `.example` files, never as live config
- [x] `aida init` ends with a key diagnostic: which env vars are set, which features that unlocks (engine / brain embeddings / voice), and the one-line fix for each gap - so a stranger's first run tells them what to export instead of failing later somewhere deep
- [x] Config-driven link templates: move the observability org URL and vendor header names hardcoded in `internal/ui/output.go` into a `links:` block on the source YAML; the engine renders citations from the template; no hardcoded vendor strings remain in non-test Go (resolved differently in Phase 0/PR #168, commit 1b5e32c, 2026-09-07: the Chronosphere-specific deep-link builder - `PrintLinks`/`buildChronoURL`/`detectStatusCodeFilter`, hardcoded to `acme-pay.chronosphere.example` and `x-acme-*` headers - was deleted outright along with the Snowflake/Chronosphere adapters rather than generalized into a config-driven template, since both adapters were former-employer-specific dead weight the generic exec adapter already covers; no `links:`-block mechanism exists or is needed. Verified 2026-09-13: no hardcoded vendor strings remain in non-test Go)
- [x] `internal/config/ari_extractor.go` generalized to configurable ID patterns in `config.yaml` so the employer identifier scheme is no longer a named concept in code (the registry's `checkout_token_patterns` is gone per the item above; this replaces both with one generic mechanism)
- [x] `gws` shell-outs in `internal/dailybriefing/` gated behind config and documented in a comment as an example of a private adapter
- [x] Golden-seed split: `aida golden` reads questions and expectations from the library (`~/.aida/`) instead of `scripts/`; `scripts/golden-questions.txt`, `scripts/golden-expectations.yaml`, and `golden.md` move to aida-config (out of this repo); a small fictional seed set ships under `examples/golden/` so the harness runs out of the box (`scripts/golden-questions.txt`/`golden-expectations.yaml` deleted here per the task's instruction, with this note that they move to aida-config -- no copy performed by this session, that's Ryan's to do in that repo; `cmd/golden-log` and `scripts/run-goldens.sh` now default to `~/.aida/golden-questions.txt` / `~/.aida/golden-expectations.yaml` / `~/.aida/golden.md`; `aida init` seeds the first two from the new fictional `examples/golden/{questions.txt,expectations.yaml}`, which exercise the three `examples/sources/*.yaml` worked sources)
- [x] In-code golden fixtures (`routing_golden_test.go`, `tier1_golden_test.go`) use the fictional seed set (renamed camp-butz/first-chair/thrive/butterstack/cred-checker/ryanlitalien-* to camp-alder/star-pass/novacore/cakestack/credbot/exampleuser-*, preserving the exact routing-score assertions -- these are fictional labels on the same deterministic scoring fixture, not a rewrite of the test's logic. `testdata/golden-queries.yaml` was also considered; its placeholder person name used a real surname, not a fictional one, so the surname was removed 2026-09-14 (see internal/config/soul_test.go, which uses the same "Kevin" convention))
- [x] Test fixtures with `/Users/ryan/...` or `ryan.litalien` paths (24 files per the 2026-08-21 audit; re-count) replaced with neutral paths (re-counted 2026-09-13: 9 `.go` files matched in `internal/` -- the original 24 either predates Phase 0's scrub or included `docs/`/`plans/`/`scripts/jarvis/` files outside this chunk's ownership, left for chunk B or a separate pass. All 9 `.go` files, plus one doc-comment example in `internal/brain/harvest.go`, now use `/Users/fakehome/...`, matching the convention already used in `internal/cli/brain_capture_test.go`. `internal/engine/routing_golden_test.go`'s entity names (camp-butz/first-chair/thrive/butter_stack/cred-checker) and GitHub org names are left as-is here -- that fictionalization is items 16/17's job, not a path issue)
- [x] `minecraft_ask` and `aida fleet start --host` read the `devices:` config block like bifrost does, instead of defaulting to a literal machine name (PR #168 follow-up) (`minecraft_ask` was already fully config-driven via `minecraft.ssh_host` in `config.go`/`minecraft.go` -- no hardcoded host found; `aida fleet start --host` did still default to the literal `"minty"`, fixed by resolving the `devices:` entry with `herdr: true` the same way `newBifrostClients` does for `/bifrost`, erroring out with a pointer to the fix when none is configured instead of guessing a hostname)
- [x] `scripts/oss-scan.sh` runs in CI (`.github/workflows/`) and fails the build on a regression

### Chunk B - Public-quality docs (prose)

Goal: a stranger landing on the repo understands what Aida is, what it needs, and how to run it, with nothing personal left in the tree. Owner: an agent. Files: `README.md`, `INSTALL.md` (new), `CLAUDE.md`, `HISTORY.md`, `docs/*.md` except `docs/diagrams/` and `docs/notes/` (chunk C), `plans/`, `.mcp.json`, `.claude/`.

- [x] `INSTALL.md`: dependency matrix per feature (engine / brain / voice / android), platform support table (voice is macOS-only: `afplay`, `osascript`, Metal whisper; engine cross-compiles linux-amd64), the honest key chain (Anthropic or Ollama offline, Voyage for embeddings, whisper.cpp model plus Piper voice or ElevenLabs plus ffmpeg for voice), and a "Backing up your config and memory" section that says plainly the brain holds everything the user ever told Aida and must never point at a public repo
- [x] README rewrite: thesis from Part 1, a synthetic home-profile demo query with `--explain` output, architecture from Part 2 (link the pipeline diagram from chunk C when it exists), quickstart from `INSTALL.md`, MCP server and voice sections trimmed to what a stranger can use; from "my system" to "a system"
- [x] `docs/` inventory: a table in this document (new Part 7) listing every file in `docs/` and `plans/` with a disposition of keep / trim / move-to-aida-config / delete, then executed; files that name private repos or people are trimmed or moved
- [x] `docs/aida-vault-design.md` disposition executed (move to aida-config or trim to the public-safe architecture) (PR #168 follow-up)
- [x] Trimmed contributor-facing `CLAUDE.md`: build commands, commit policy, architecture pointers, the `make install` rule; personal operational detail (family, menu, habits, employer context, device names) removed
- [x] `.mcp.json` personal absolute path and `.claude/research/` reading list pruned (`.mcp.json` already carried no personal path when this chunk started - verified, not re-touched; `.claude/research/2026-05-03-ai-reading-list.md` deleted)
- [x] `HISTORY.md` brought current through the Phase 0 merge and this checklist, still with no employer names
- [x] `CONTRIBUTING.md` (short): how to run the tests and goldens, the granular-commit rule, where user data lives
- [x] Every remaining `docs/` reference to PR numbers notes that they point at the archived private repo after the cut (one line at the top of `HISTORY.md` is enough)

### Chunk C - Engineering notes and diagrams (launch content)

Goal: the material the blog sidebars link to and the README embeds, all living in the repo so the public repo and the blog share one source. Owner: an agent, with a Dana critique pass after. Files: `docs/notes/` (new), `docs/diagrams/`, `docs/pipeline-diagram.mmd`. Rules from `docs/plan-oss-launch.md` apply: credit primary sources, every rationale entry states the alternative and the one reason that decided it, zero examples of AI-generated game content.

- [x] `docs/notes/technology-rationale.md`: the ten entries from launch plan Section 5, three to five sentences each, written from the actual code and history
- [x] `docs/notes/inspiration.md`: the credited prior-art list from launch plan Section 6, linking primary sources (all URLs verified live 2026-09-12; unverifiable ones cited by name); part 1's "What I Borrowed" links back to it once it exists (blog edit = Ryan)
- [x] `docs/notes/failure-stories.md`: the ten stories from launch plan Section 7 as symptom, wrong hypothesis, actual cause, fix, each verified against git history (corrections applied where the telling disagreed with git: the 10-second tax ran 4 days not weeks; the hallucination filter shipped with the audit log and the real story is its bare-word blind spot; the Nest-cast merge and revert were both 2026-07-06; the cred-scrub fix was a three-wave arc)
- [x] `docs/notes/loop-internals.md`, `docs/notes/voice-pipeline.md`, `docs/notes/lmd-protocol.md`, `docs/notes/feedback-mechanics.md`: the four deep dives from the earlier eight-article sketch, as engineering notes
- [x] Diagram: ecosystem map (aida, config, brain, wiki, android, agents) - `docs/diagrams/ecosystem-map.md`
- [x] Diagram: six-step pipeline with the LLM-vs-deterministic split explicit (redesign of `docs/pipeline-diagram.mmd`, moved to `docs/diagrams/pipeline.md`)
- [x] Diagram: routing walkthrough of one fictional query as annotated `--explain` output - `docs/diagrams/routing-walkthrough.md`
- [x] Diagram: memory levels L0-L4 as a layered stack - `docs/diagrams/memory-levels.md`
- [x] Diagram: voice turn sequence with the latency budget - `docs/diagrams/voice-turn.md`
- [x] Diagram: loop lifecycle (task, agent, gate, PR, never merge) - `docs/diagrams/loop-lifecycle.md`
- [x] Diagram: two rosters, one seam (finish the ASCII draft in `docs/diagrams/team-seam.md`) - mermaid rendering added, reworked into an 8-node diagram after the Dana critique pass
- [x] Diagram: three-repo and fresh-cut picture (what is public, what stays private) - `docs/diagrams/three-repo-cut.md`
- [x] Every diagram is mermaid or SVG in `docs/diagrams/`, reads at blog-column width, and survives light and dark themes (all eight are mermaid in `docs/diagrams/`, vertical layouts, default theme with stroke-weight/dash class distinctions only - no hardcoded fills)

### Gated on Ryan (not for agents)

- [x] Close task #200 (done, PR #168 merged 2026-09-08; closed 2026-09-13) - the tracking task is the one mirrored to tasks issue #390 ("Make aida public / open source"); its local `#N` id differs per machine (#216 on minty, #379 on edith), so refer to it by title or issue number
- [x] Stand up the content workspace `~/dev/aida-content/` (private) for drafts, scripts, and Melissa/Dana outputs (up since 2026-09-12: drafts, outlines, SEO diffs, Dana critique, review packets, Jekyll-ready staging copies)
- [x] Dana critique pass on the chunk C diagrams; fixes applied 2026-09-13 across all eight `docs/diagrams/*.md` files
- [x] Em-dash sweep of the whole repo (decided 2026-09-14: repo docs, scripts, web UI strings, and Go comments/strings all follow the house rule, not just blog copy; done on `oss` the same day, tests and scan green)
- [ ] Blog part 2 "How it decides": outline, Fable technical pass, Melissa SEO and social pass, Ryan rewrite, publish (prerequisite found 2026-09-14: the site has no mermaid support, and parts 2-4 all embed diagrams; a verified patch and the publish checklist are in `aida-content/website-staging/`; 2026-09-14 the patch is applied on the local website branch `feat/mermaid-support` and a `jekyll build --drafts` renders all seven fences across the three staged drafts, part 1 untouched; Ryan pushes and merges)
- [ ] Blog part 3 "How it remembers": same loop
- [ ] Blog part 4 "What it spawned": same loop, ends with the announcement
- [ ] Video pilot (four screencasts, demo profile with fictional seeds only, never the real brain); part 1 video can be backfilled; task #478 (Manim) informs the format
- [x] Decide: do the personal pages (`/menu`, `/habits`) and their `~/dev/health/...` defaults ship in public aida, or get extracted? Decided 2026-09-14: ship as-is. They are config-overridable, degrade quietly when the paths don't exist, and carry no personal data; extraction gates nothing
- [x] The fresh-start cut, on Ryan's go, alongside part 4: rename this repo `aida-private` (stays writable, not archived, per the 2026-09-14 decision), create public `ryanlitalien/aida` seeded with one initial commit of the scrubbed tree, re-point `origin` on this machine, edith, and photon, then close #216 (done 2026-09-15: renamed, public repo seeded from one commit, origins re-pointed on edith and minty; photon pending)
- [x] At the cut, strip `scripts/oss-scan.sh` and its `ci.yml` step from the public initial commit (spotted 2026-09-13): the scan quotes every private pattern it guards - employer name and ticker, the real merchant list, the work username - so publishing it publishes the secret map. It stays in `aida-private` (and CI there keeps running it) as the pre-publish gate; the public repo optionally gets a generic secrets check (gitleaks-style) with no personal patterns

### Coming soon (non-blocking)

Announced as "coming soon" at launch; none of these gate the fresh-start cut or the part 4 publish. Each becomes its own plan when picked up.

- [ ] aida-android scrub and publish (the LMD protocol note from chunk C becomes the shared contract)
- [ ] aida-agents scrub and publish
- [ ] Optional wiki-template extraction from aida-wiki
- [ ] Grocery Chrome extension (Instacart helper): not in any aida repo yet - bring the source into the fold (own repo or alongside aida-android in the second wave), then scrub and decide its publish lane; ships as code, never through the Chrome Web Store
- [ ] Model roster + usage gauges (`aida models`): productize the `~/.aida/models.yaml` roster - nickname resolution, live usage probes, the dashboard Models panel - so a stranger points it at their own providers. Conceptually the arbiter's capacity view (the fuel gauges and race clock, not the crew chief - see the pit-wall metaphor in the model-usage-burndown research); it needs no separate name unless it's ever extracted into its own repo/web app, and it gets named at that repo boundary

---

## Part 1 - What Aida is, and why it exists

This section is the thesis. It becomes the top of the public README and the spine of blog part 1.

### The problem

A working engineer's knowledge is scattered across a dozen surfaces: local codebases, wikis, Notion, Slack, SaaS dashboards, data warehouses, observability tools, personal notes, and the heads of past selves. Answering "what's the refund status on this loan" or "how does our tenant scoping work" means first knowing *where* to look, then knowing *how* to ask each tool, then stitching the answers together yourself. LLM chat helps with the stitching but knows nothing about your sources, and agent frameworks know your sources but hand routing decisions to a model that hallucinates them.

### The answer

Aida is a CLI-first "agent of agents" written in Go. You ask it a question in plain English; it figures out which of your registered sources can answer, queries them in parallel, and synthesizes one grounded answer with citations. The core design bet, and the thing worth blogging about, is **LLM at the edges, deterministic middle**: the LLM parses language in and prose out, but the routing decisions in between are config-driven, testable, and reproducible. Exactly 3 core LLM calls per query (parse, execute, synthesize), plus one bounded router call. No chain-of-agents deciding among themselves where to look.

### Why it helps (the personal pitch)

- One query surface across work and home. `aida "camp-butz revenue last week"` and `aida "what engine does thrive use"` route to entirely different source sets without me switching tools, because profiles partition the library.
- Memory that compounds. Every thumbs-down teaches the router. Every Claude Code / Codex / Gemini session gets harvested into recallable memory. Knowledge stops evaporating when a terminal closes.
- It is the substrate other projects grow on. The autonomous loop pattern became Pilot Light's Producer agent. The LLM-analysis-of-failures pattern became ButterStack's build failure analysis. The ops automation instincts fed Camp Butz and FirstChair (firstchair.ski, the ski-pass comparison site). Aida is where the patterns get proved before they get redeployed.

### The lineage (honest attribution, also blog material)

Aida is deliberately an amalgamation of public ideas rather than an invention: Ralph-style deterministic outer loops around fresh-context agents, memory-consolidation research (Elastic Atlas style episodic-to-semantic promotion, decay scoring), Karpathy-style LLM-maintained wikis, LangChain-era orchestration reduced to its useful skeleton (parallel fan-out, structured outputs) with the LLM-decides-everything part removed, vector recall via Voyage embeddings + SQLite, and MCP as the interop layer in both directions (Aida is an MCP server to other agents and an MCP client to everything else). The blog series should name all of these; the value added is the integration and the deterministic-middle discipline, not any single idea.

---

## Part 2 - Architecture (README material, blog parts 2-4 sidebars, engineering notes)

Everything here already exists and works; this section is the canonical outline for explaining it publicly.

### The six-step engine pipeline

```
query -> [1 PARSE llm] -> [2 CLASSIFY det] -> [3 RESOLVE det] -> [4 PLAN det] -> [5 EXECUTE llm] -> [6 SYNTHESIZE llm]
```

- **Parse** (LLM #1): natural language to an `Intent` struct via structured output.
- **Classify** (deterministic): regex typing of entities (IDs, tokens, URLs) and strategy selection (`lookup`, `query`, `investigate`, `record`, `execute`, `search`).
- **Resolve** (deterministic): types each entity and flattens its extracted identifiers for the planner and prompt builders (no registry lookup - removed 2026-09-12, see the Checklist).
- **Plan** (deterministic): capability matching and source ranking, including a `routes.yaml` `match_entity` route boost (+100) for a source tied to the resolved entity. `--explain` prints the scoring table; `--dry-run` shows the plan without executing.
- **Execute** (LLM #2): generate source-specific queries, run them in parallel (`errgroup`, semaphore of 5).
- **Synthesize** (LLM #3): aggregate results into one cited answer.

### Source files as the knowledge registry

Sources are one YAML file each under `~/.aida/library/sources/`, declaring type (`codebase`, `docs`, `tool`, `data-source`, `app`, `claude-project`, `git`), capabilities, entity tokens, an optional context doc fed to the LLM, an optional grep search mode, and an optional `exec` command template. A load-time validator rejects bare `{query}` passthroughs (shell-injection guard), and `aida lint` surfaces misconfigurations. `aida index --generate` scans the filesystem and LLM-classifies new sources, merging without clobbering manual edits. Routing is therefore data, not code: adding a knowledge source is writing a YAML file.

### Profiles: one binary, partitioned lives

Sources can be scoped to named profiles (`work`, `home`). Profile auto-detection keys off tool availability (e.g. a work-only CLI on PATH selects the work profile), with `AIDA_PROFILE` as an override. The same binary answers ButterStack questions at home and employer questions at work without either library contaminating the other. This is a headline feature for the blog: most agent frameworks assume one context; real people have at least two.

### The feedback loop (thumbs up/down)

`aida thumbs-down --because "..."` runs a polarity-tracking directive extractor (negation-aware tokenizer) that pulls `intendedSources` and `excludedSources` out of the free-text reason. Those persist as lessons with embeddings; the next similar query's router applies deterministic +50/-50 adjustments from the top-3 most-similar lessons *before* prompting. Corrections change behavior mechanically, not as prose hints the model may ignore. The voice layer mirrors this with `jarvis_thumbs_up/down` and a standing-directive tool, dual-writing to voice lessons and, when the turn used the engine, passing through to the engine's lesson store via a run-id sentinel.

### Memory levels

Aida's memory is tiered, and the tiers are worth naming explicitly in both docs and blog:

| Level | Store | Written by | Recalled by |
|---|---|---|---|
| L0 ephemeral | run records, `--explain` traces | every query | `aida runs`, debugging |
| L1 feedback lessons | engine `lessons` + `jarvis_lessons` (SQLite + embeddings) | thumbs up/down, notes | router boosts, voice-turn prompt injection |
| L2 captured agent memories | brain memory profiles: `claude` (hook-mirrored), `codex` / `gemini` (LLM-distilled harvest of session transcripts, watermarked, hook + periodic sweep) | coding-agent sessions | `brain_recall` (MCP), `aida brain recall`, voice `claude_memory_recall` |
| L3 curated knowledge | entity pages, `knowledge/domains/*.md`, per-source layer docs | me + LLM compile passes | `brain_search`, source context injection |
| L4 consolidated wiki | aida-wiki (OKF bundle): consolidation, decay scoring, supersession | `aida brain consolidate` (planned, see plan-super-ryan-wiki.md) | wiki commands, brain |
| Tasks | markdown files with frontmatter, six statuses, stable `#N` IDs | CLI, voice, MCP, web UI | loop driver, briefings, mirrors to GitHub Issues |

Files are the source of truth; `brain.db` is a derived index that rebuilds detached when stale. That single rule is what makes the whole thing portable and git-syncable.

### Tier movement: automated hooks, not manual curation

The tiers only earn their keep if records move between them without a human shepherding each one. Name the movement mechanisms explicitly in docs and blog - what's automated today, and what the roadmap automates next:

**Automated today (hooks and sweeps):**
- Session -> L2: the Claude capture hook mirrors memory files on every Write/Edit (`aida brain capture-hook`); Codex/Gemini Stop/AfterAgent hooks plus the `aida serve --harvest-sweep` timer run the LLM-distillation harvest; Meetily rides the sweep alone. Watermarks + quiet windows make re-runs free.
- L0 -> L1: thumbs up/down promotes a run record into an embedded lesson at the moment of feedback; the voice layer dual-writes and passes through to the engine store via the run-id sentinel.
- In-place supersession: re-capturing the same memory key replaces the prior record (no duplicates), and stale `brain.db` triggers a detached rebuild - both automatic.

**Roadmap (each is a promotion hook to build, and blog material):**
- L2 -> L3 promotion pass: a periodic compile step (harvest-sweep-style timer or post-capture hook) that clusters hot L2 memories by entity/domain and proposes merges into entity pages and `knowledge/domains/*.md` - human-approved at first, threshold-automated later. Signals: recall frequency, cluster density, age.
- L3 -> L4: `aida brain consolidate` (plan-super-ryan-wiki.md) - consolidation, decay scoring, supersession into the wiki. Decay scoring is also the designed *demotion* path: tiers move down by losing weight, not by deletion.
- Write-time tiered summaries: an L0/L1/L2-style abstract-overview-full layering per record at capture time (the OpenViking-validated idea from `docs/research-openviking.md`) so recall loads only as deep as needed.

The through-line for the blog: feedback and hooks promote; decay demotes; humans curate exceptions, not the pipeline.

### Voice, jobs, and the autonomous loop

- **Jarvis/Aida voice layer**: ffmpeg mic capture, amplitude VAD, whisper.cpp `tiny.en` on Metal, Haiku with in-process tool dispatch, Piper/ElevenLabs TTS. All Go, no Python runtime. Two personas over one brain via `NewTwin`.
- **Background jobs**: per-profile SQLite-backed queue, agents that can pause on `ask_user` and resume from a voice reply, speak-on-next-wake notifications.
- **`aida loop`**: the Ralph-style driver. Deterministic outer loop spawns fresh-context agents against the task queue, quality-gates with `--check`, isolates in worktrees, optionally sandboxes in Docker, opens PRs with an adversarial review panel, never merges. Budget caps and a consecutive-failure circuit breaker. `aida loop plan <goal>` decomposes a goal into tagged tasks. This subsystem alone is an engineering note (and the part 4 sidebar).
- **Dispatcher + roster**: named agent call-signs (`~/.aida/roster.yaml`) mapping to Claude subagents, pinned sources, MCP tools, or background jobs; live discovery of `.claude/agents` personas; 0 LLM calls when a call-sign is named, 1 to select otherwise.

### The `--lmd` service (portability to the phone)

A second HTTP listener on `:1218`, bound to the Tailscale IP only (never `0.0.0.0`), bearer-token auth (`LMD-` prefixed Crockford base32, ~125 bits, constant-time compare, `aida lmd token --rotate`), serving `/lmd/v1/*` to the A.I.D.A. Android client. The loopback `:1610` daemon (tasks web UI, Jarvis HTTP surface, MCP call dispatch) stays unauthenticated but unreachable off-box. The split is a nice, teachable security story: the naming (Earth-1610 / Earth-1218) is flavor, the bind discipline is the substance.

### Portability: the three-repo split

Code (`aida`), config (`aida-config` -> `~/.aida/`), and memory (`aida-brain` -> `~/.aida/brain/`, auto-committed every query) are three git repos. A new machine is two clones and an `.env`. This split is exactly what makes open-sourcing tractable: the public repo is the code; the private repos are me.

---

## Part 3 - Repo-by-repo disposition

| Repo / asset | Disposition | Notes |
|---|---|---|
| `aida` (this repo) | **Open source, via a fresh public repo** | The deliverable. Published as a new repo from the scrubbed tree; this repo is archived, never flipped (history rationale in Part 4 item 7). Blockers in Part 4. |
| `aida-config` | **Stays private**; replaced publicly by bootstrap | Ship `aida init` that generates a working `~/.aida/` skeleton plus an `examples/library/` in this repo. Never publish the real one (it encodes employers, partners, machines). |
| `aida-brain` | **Stays private**; ship empty-brain init | `aida init` creates an empty brain (git-initialized locally, no remote). Document the "point it at your own private repo" pattern. |
| `aida-wiki` | **Stays private** (it *is* the personal archive) | Consider a second-wave `okf-wiki-template` repo: conventions, lint taxonomy, triage workflows, no content. Blog article regardless. |
| `aida-android` | **Open source, second wave** | Already standalone at ~/dev/aida-android. Needs its own scrub pass (keystore paths, package naming decision, default host). LMD protocol doc (`docs/lmd-protocol.md`) moves to shared/public since it is the wire contract. |
| `aida-agents` | **Open source candidate, second wave** | The detached Docker agent runner is generic. Scrub minty/host specifics into config. |
| `life-log` | **Never** | Personal archive. Mentioned in blog only in the abstract ("a private life-log repo"). |
| MCP connectors | **Document, don't ship** | Aida discovers whatever MCP servers the user's own configs declare. Public docs list examples; no credentials or personal server lists. `.mcp.json` in this repo needs the personal `mermaid-mcp` absolute path removed. |
| Memory levels / brain code | **Open sources with `aida`** | It is `internal/brain`; nothing to split. The *contents* are private by the config split above. |

---

## Part 4 - Publication blockers in this repo (audit of 2026-08-21)

Verified against the working tree and full git history (606 commits). Good news first: **no secrets have ever been committed** (history-wide scan for key shapes came back clean), non-test Go code has zero hardcoded machine names, and `go build` / `go vet` / `go test ./...` are green in CI.

### Hard blockers

1. **No LICENSE file.** Default is all-rights-reserved; nothing is open source until this exists. **Decided: MIT** (matches the Piper/jarvis voice lineage and maximizes the blog's "go use it" call to action). No per-file license headers (Go convention: single top-level LICENSE).
2. **README headline example was real former-employer production data.** Two real loan IDs with dollar amounts and dispute outcomes, plus a live observability deep link. Same ID shapes in `golden.md`, `test-runs/*.md`, `internal/engine/classifier_test.go`, `internal/engine/resolver_test.go`. Urgent independent of open-sourcing given post-separation posture. Replace with synthetic IDs everywhere; rewrite the README demo around a home-profile query.
3. **`docs/golden-seed-{questions,answers}-work.md`** name former-employer internal tooling. Delete, don't redact; they have no public value.
4. **`docs/golden-seed-answers-home.md`** is a full ButterStack architecture dump (tenancy, auth, queues, plans, webhook parameters, credit system). Delete from the public repo; move to aida-config or butter_stack if the golden-seed harness still needs it.
5. **`docs/reports/household-survey-2026-07-19.html`**: home LAN topology and Tailscale IPs. Delete.
6. **`internal/config/soul_test.go`**: Kevin's personal gmail in a fixture. Replace with `partner@example.com`; someone else's PII needs their ok even in a scrubbed form.
7. **History: publish a fresh repo; do not bring the old history.** (Recommended after research; final sign-off tabled.) GitHub's own sensitive-data guidance is decisive here: force-pushing rewritten history does not actually remove the old commits from GitHub. They stay directly reachable by SHA URL, in cached views, and through pull request refs, and only a GitHub Support ticket purges them. This repo has ~139 merged PRs, so `refs/pull/*` alone would preserve essentially every original commit even after a perfect `git filter-repo` pass; flipping *this* repo public can never be made safe. Fresh-start mechanics: rename this repo (e.g. `aida-private`) and archive it read-only as the historical record; create a new public `ryanlitalien/aida` seeded with a single "initial public release" commit of the scrubbed tree; make it the canonical origin going forward (granular, no-squash history accrues publicly from that point); re-point `origin` on this machine, edith, and photon (no force-push coordination needed). Accepted side effects: issue/PR numbering resets, and historical PR references in docs (#81, #139, ...) point at the archive.

### Soft blockers (fix before or shortly after publishing)

- `internal/ui/output.go` hardcoded the former employer's observability org URL and vendor header names; move to source config (this is also just correct design: link templates belong to the source YAML).
- `gws` shell-outs (`internal/dailybriefing/`) depend on an employer-internal CLI; gate behind config and document as an example of a private adapter.
- `internal/config/ari_extractor.go` encodes an employer identifier scheme as a named concept; generalize to "configurable ID patterns" (the partner YAML already has `checkout_token_patterns`, extend that idea).
- Test fixtures with `/Users/ryan/...` and `ryan.litalien` paths (24 files): mechanical replace with neutral paths.
- `docs/tts-model-ab/clips/*.mp3` (~3.3MB ElevenLabs output): drop from the public tree; ElevenLabs redistribution terms are not worth the review.
- Tracked PDFs (`original-plan.pdf`) and the two large OpenAI PDFs in the tree (untracked but present): confirm none are tracked going forward; `.gitignore` already covers `*.pdf` except the explicitly-added one.
- `.mcp.json` personal absolute path (mermaid); `.claude/research/` personal reading list: prune.
- CLAUDE.md itself is written for me, references employers, family context lives upstream: the public repo keeps a trimmed contributor-facing CLAUDE.md (build commands, commit policy, architecture pointers) and drops the personal operational detail.
- Docs pruning pass over `docs/`: several plan docs reference private repos and people; each either survives (architecture value), gets trimmed, or moves to aida-config. Inventory in Phase 2.

### Golden seeds: harness public, seeds private (decided)

The golden machinery is two separable things, and the split resolves the "does this go public" question. The **harness** is generic eval tooling: `aida golden`, `cmd/golden-log`, `scripts/run-goldens.sh`, the in-code routing goldens (`routing_golden_test.go`, `tier1_golden_test.go`), and the run-report format. The **seeds** are personal data: `scripts/golden-questions.txt`, `scripts/golden-expectations.yaml`, `golden.md`, and the `docs/golden-seed-*` files, which encode my sources, a former employer, and ButterStack internals.

- The harness ships publicly. Eval-driven routing tuning against your own library is one of Aida's genuinely distinctive features, and it is strong blog-sidebar material.
- Seed files move out of this repo into `~/.aida/` (aida-config), where user data belongs anyway; `aida golden` learns to read questions/expectations from the library instead of `scripts/`.
- A small fictional seed set ships in `examples/` so the harness is demonstrable out of the box. MCU-flavored fictional partners are fine per the branding decision.
- The in-code golden fixtures get the same fictional treatment (they currently carry corp usernames and employer URLs; already listed in the scrub items above).

### The real adoption gap (not a scrub item)

The README's setup path is `git clone` of two private repos. A stranger currently cannot run Aida at all. **Phase 1 is therefore design work, not cleanup**: `aida init` must bootstrap a working `~/.aida/` from nothing (config, empty library, empty brain, example sources), and the docs must state the honest dependency chain: Anthropic key (or Ollama offline mode), Voyage key for embeddings, and for voice only: whisper.cpp model, Piper voice or ElevenLabs key, ffmpeg. Voice layer is macOS-only (`afplay`, `osascript`, Metal whisper); the engine itself is portable and `make build-all` already cross-compiles linux-amd64.

---

## Part 5 - Phased workplan

Each phase is a PR-sized unit or a small series of granular commits; the loop can drive the mechanical ones (`aida loop --tag oss`).

**Phase 0 - Safe (target: 1-2 days of work)**
1. Add LICENSE (MIT) + copyright line.
2. Purge blockers 2-6: synthetic IDs, delete work golden seeds + ButterStack dump + household survey, scrub soul_test fixture, drop mp3s, fix `.mcp.json`.
3. Maintain `HISTORY.md` as scrub work proceeds (it ships in the fresh repo as the public history). The fresh cut itself (new public repo, scrubbed tree as one initial commit, re-point `origin` on all three machines, archive this repo read-only) is approved but DEFERRED to the Phase 2 publish step - do not execute it during Phase 0.
4. Re-run the secret/PII scans as a CI-able script (`scripts/oss-scan.sh`) so regressions fail CI.

**Phase 1 - Runnable by a stranger (target: ~1 week)**
1. `aida init` full bootstrap: `~/.aida/` skeleton, empty brain (local git), example library with 2-3 generic sources (a codebase, a docs dir, an exec tool). Accept `--brain-remote <url>` (and `--config-remote <url>`) so a private backup remote can be set at bootstrap instead of by hand; `brain.InitRepo` already takes an optional remote and `CommitAndPush` already skips the push when no `origin` exists, so this is wiring, not new behavior.
2. `examples/` directory: worked source YAMLs, a partners.yaml with fictional entities, a roster.yaml (exists as docs/roster.yaml.example, promote it).
3. Config-driven link templates (kills the output.go hardcoding).
4. Honest INSTALL doc: dependency matrix per feature (engine / brain / voice / android), platform support table. Include a "Backing up your config and memory" section: `~/.aida/` and `~/.aida/brain/` are plain files that never leave the machine; the brain is already a local git repo that commits itself after every query; to back up or sync across machines, add a **private** remote to each and push, dotfiles-style. Say plainly that the brain holds everything the user has ever told Aida and must never point at a public repo.
5. Golden-seed split: `aida golden` reads seeds from the library; real seeds move to aida-config; fictional example seeds land in `examples/`.

**Phase 2 - Public-quality docs**
1. README rewrite: thesis from Part 1, synthetic demo, architecture from Part 2, quickstart from Phase 1. From "my system" to "a system".
2. Docs inventory: keep / trim / move for every file in `docs/` and `plans/`.
3. Trimmed public CLAUDE.md.
4. Publish: the fresh public repo goes live alongside blog part 1.

**Phase 3 - Second wave**
1. aida-android scrub + publish; LMD protocol doc becomes the shared contract.
2. aida-agents scrub + publish.
3. Optional `okf-wiki-template` extraction from aida-wiki.

**Phase 4 - Blog series** (part 1 shipped 2026-09-03, ahead of the repo flip; the repo now goes public alongside part 4, which ends with the announcement, or sooner if Phase 2 lands first)

Format decision: a **4-part main series** for a broad audience (techy, non-techy, nerds, my dad), one part per week for four consecutive weeks. The dad test (revised 2026-09-03): every part must read start to finish without needing the code, but code is welcome where it shows rather than tells: shell commands, YAML source files, routing/`--explain` output, diagrams, and short Go snippets when they make a mechanism concrete (task filtering, tool routing, confidence scoring). No Go function dumps. Deeper technical material lives in short "for the nerds" sidebars that link into the repo's `docs/`. The deep-dive material from the earlier 8-article sketch (loop internals, voice pipeline, LMD protocol, feedback mechanics) becomes engineering notes shipped in the repo instead of blog posts, free to graduate into follow-up posts later if any of them finds an audience.

| # | Part | For everyone | "For the nerds" sidebar |
|---|---|---|---|
| 1 | "Introducing Aida - An agent of agents for one person" (**shipped 2026-09-03**: [post](https://www.ryanlitalien.com/posts/an-agent-of-agents-for-one-person/), [series page](https://www.ryanlitalien.com/series/aida/)) | Shareware-games nostalgia into "technology as magic", then the pivot to owning your own memory. Aida and Jarvis introduce themselves in their own words with spoken clips. The Snowflake line as the hinge, the Tax ID problem, one design bet stated not proven, the Cape Cod query with its self-flagged 28-vs-27 caveat, work/home profiles as the human argument, "What I Borrowed" under a T.S. Eliot epigraph with primary-source links, and what the project is not. Passes the dad test: zero code blocks. | The "I built my own Jarvis" day-in-the-life demo did not make it in and belongs to part 2 or a video; the three-repo code/config/memory split waits for the repo flip |
| 2 | "How it decides" | The switchboard-operator metaphor: the AI understands the question, but a rulebook you can read decides where to look. And corrections that actually stick. | LLM at edges / deterministic middle; the six-step pipeline; thumbs-down router boosts; the golden-seed eval harness |
| 3 | "How it remembers" | Memory that compounds instead of evaporating when a chat window closes; every AI session distilled into one recallable brain. | Memory levels L0-L4; the Claude/Codex/Gemini harvest bridge; files as truth, db as index; the tier-movement hooks (capture hooks, harvest sweeps, thumbs-promotion, planned L2->L3 compile and L4 consolidate/decay); ByteDance's OpenViking as the convergent-evolution foil (`docs/research-openviking.md`) |
| 4 | "What it spawned" | How the same patterns built a game-studio SaaS (ButterStack), a ski-pass site (FirstChair), a campground's operations (Camp Butz), and a tireless game producer (Pilot Light). Ends with the open-source announcement and an invitation to run your own. | `aida loop` and the Producer; the two-roster seam (`docs/diagrams/team-seam.md`); the quickstart |

Cross-cutting blog rule: every employer reference stays generic ("a payments company's data warehouse"); ButterStack references are fine at the architecture-pattern level, not the golden-seed level.

---

## Part 6 - Decision log

Decisions recorded 2026-08-21:

1. **License: MIT.** Decided.
2. **History: fresh public repo; the old history never goes public.** Research and mechanics in Part 4 item 7, reflected throughout the plan. **Approach approved 2026-08-23; execution explicitly deferred - do NOT perform the cut yet.** The cut happens only at the Phase 2 publish step, on Ryan's go. In the meantime, `HISTORY.md` (repo root) is the curated, sanitized public stand-in for the private history: the dated development timeline the fresh repo ships with and the blog draws on.
3. **Branding: the MCU flavor stays** (A.I.D.A., the Jarvis persona and voice, Earth-1610/1218 ports). The Piper `en_GB-jarvis-high` voice is MIT-licensed (via jgkawell/jarvis), so shipping it is clean. Ryan flagged wanting to relocate/rework the Jarvis voice at some point; tracked as post-publication work, not a blocker.
4. **Golden seeds: harness public, seeds private**, with fictional example seeds in `examples/`. Details in Part 4.
5. **Carlos: out of scope for this plan.** Ryan brings him in when the time is right; no phase depends on him.
6. **Blog: 4-part weekly broad-audience series** per Phase 4, repo public alongside part 1.
7. **Venue: ryanlitalien.com** (decided 2026-09-03 by shipping). Jekyll on GitHub Pages; the site grew a series mechanism (`series:` + `series_part:` front matter, `_data/series.yml`, a "Part n of N" line and parts list on every post, and a landing page at `/series/aida/`) that teases the unpublished parts by working title. Part 1 went out **before** the repo flip, so decision 6's "repo public alongside part 1" is superseded: the flip now rides with part 4's announcement.

Remaining open: the go signal for executing the (already approved) fresh-start cut, now targeted at the part 4 publish.

Launch execution (content production, Melissa/Dana involvement, diagrams, failure stories, YouTube) is planned separately in `docs/plan-oss-launch.md`.

---

## Part 7 - docs/ and plans/ inventory (2026-09-13, chunk B)

Every file in `docs/` and `plans/` as of chunk B, with a disposition and
the reason, executed in the same PR this table lands in. `docs/notes/`
and `docs/diagrams/` (chunk C's output) are out of scope here - they are
public by design and untouched except for the link fixes noted below.
"move-to-aida-config" means the file is deleted from this repo with a
commit message noting it; no copy is performed by this session - moving
the private copy into aida-config is Ryan's action, not this repo's.

| File | Disposition | Reason |
|---|---|---|
| `docs/agent-architecture-diagram.md` | delete | ButterStack's own internal Rails/Action-Cable agent architecture, not Aida's - off-topic for this repo regardless of privacy |
| `docs/aida-dispatcher-roster-design.md` | move-to-aida-config | Real design rationale, but the running example is `~/dev/butter_stack` paths and org-chart files throughout 684 lines; the public architecture summary already lives in `CLAUDE.md`'s "Aida dispatcher + agent roster" section, so nothing public is lost |
| `docs/aida-personal-roster-plan.md` | move-to-aida-config | Entirely personal: real machine names (minty/edith/heimdall/beast/friday/photon), a health+finance agent roster, a personal email address |
| `docs/aida-vault-design.md` | delete | Real 1Password vault/item ids, two real service-account names, a home street reference, and a former-employer vault entry - pure personal ops with no architecture payoff (checklist item, PR #168 follow-up) |
| `docs/autonomous-loop-design.md` | keep | Pure architecture/design rationale for `aida loop`, already generic |
| `docs/autonomous-loop/OVERVIEW.md`, `docs/autonomous-loop/SPEC.md` | keep | Same - implementation spec, no personal data |
| `docs/claude-memory-bridge-capture.md`, `docs/claude-memory-bridge-design.md` | keep | Architecture docs for the L2 memory-capture hook, generic |
| `docs/gap-analysis-2026-04.md` | keep | Historical self-audit of the engine, architecture-level |
| `docs/harness-evolution-plan.md` | keep | Architecture/roadmap doc, generic |
| `docs/implementation-plan-agent-mode.md` | keep | Design doc for `--agent` mode, generic |
| `docs/JARVIS.md` | trim | Excellent public reference doc; one real home-location example replaced with `"Boston, MA"` |
| `docs/lmd-protocol.md` | keep | The actual wire-protocol spec ~20 Go files cite by section name (`### Token`, `## Personas`, etc.); `docs/notes/lmd-protocol.md` (chunk C) is a separate narrative engineering-note treatment of the same subsystem for blog/README linking, not a replacement - no personal data in either |
| `docs/local-agent-structure-solutions.txt` | delete | ButterStack-platform research note that mentions Aida only as one comparison point; not this repo's story |
| `docs/memory-bridge-multi-agent.md` | keep | Architecture doc for the Codex/Gemini/Meetily harvest bridge, generic |
| `docs/pipeline-diagram.png` | delete | Superseded by `docs/diagrams/pipeline.md` (chunk C); nothing references the old PNG anymore |
| `docs/plan-agent-orchestrator-research.md` | keep | Architecture research, generic |
| `docs/plan-cost-ledger.md` | keep | Design doc, generic |
| `docs/plan-dashboard-bifrost.md` | keep | Design doc, generic |
| `docs/plan-investigate.md` | keep | Design doc; references the now-removed partner-registry concept as dated historical context, harmless |
| `docs/plan-mcp-task-edit.md` | keep | Design doc, generic |
| `docs/plan-open-source.md` | keep | This plan |
| `docs/plan-orchestrator-graph-layer.md` | keep | Architecture doc, generic |
| `docs/plan-oss-launch.md` | keep | The companion launch plan |
| `docs/plan-remove-partners.md` | keep | Execution plan for the already-completed partner-registry removal; historical record |
| `docs/plan-super-ryan-wiki.md` | keep | Design doc for the planned L3→L4 consolidation, generic |
| `docs/reports/README.md`, `docs/reports/aida-system-map-2026-07-20.html` | keep | Dated architecture snapshot; the home-LAN survey report this README once accompanied was already removed in Phase 0 |
| `docs/research-opengym.md`, `docs/research-openviking.md` | keep | External prior-art research notes, generic |
| `docs/retry-resilience-plan.md` | keep | Design doc, generic |
| `docs/tts-model-ab/README.md`, `report.html`, `run-ab.sh`, `latency.csv`, `clips.csv` | trim | ElevenLabs TTS A/B results, generic and worth keeping - but `clips.csv` had six real `/Users/ryan/Desktop/...` paths (missed by `scripts/oss-scan.sh`, which has no check for this pattern despite the Part 4 audit calling it out); replaced with `/Users/fakehome/Desktop/...` |
| `docs/work-machine-rename-checklist.md` | delete | Personal machine-migration checklist naming a real former coworker and a former employer's internal username |
| `plans/drift-fix-plan.md` | trim | Design doc, generic - but had five literal `/Users/ryan.litalien/dev/aida/...` paths (same oss-scan gap as `clips.csv` above); replaced with the `~/dev/aida/...` form already used elsewhere in the same file |
| `plans/openclaw-plan.md` | keep | Design doc, generic |
| `plans/orphan-safe-subprocess-exec.md` | keep | Design doc, generic |

Net: 3 delete for pure personal-ops content with no architecture value
(`aida-vault-design.md`, `work-machine-rename-checklist.md`,
`local-agent-structure-solutions.txt`), 1 delete for off-topic content
(`agent-architecture-diagram.md`), 1 delete for a superseded binary
(`pipeline-diagram.png`), 2 move-to-aida-config (`aida-dispatcher-roster-design.md`,
`aida-personal-roster-plan.md`), 3 trim (`JARVIS.md`, `docs/tts-model-ab/clips.csv`,
`plans/drift-fix-plan.md`), and the remaining ~23 files kept as-is - 
pure architecture and plan documents were the large majority, matching
the "be conservative with engineering notes and
plan docs, aggressive with anything personal" instruction.
