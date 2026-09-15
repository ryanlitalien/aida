# Plan: Aida launch - content, blog, video

**Status**: draft · **Companion to**: `docs/plan-open-source.md` (repo publication) · **Scope**: everything around the publication - the blog series production, diagrams, technology rationale, failure stories, SEO, the YouTube question, and positioning. This plan deliberately does not write any content; it defines who produces what, in what order, through which gates.

---

## 1. Voice and authorship

The series publishes in Ryan's voice, first person, under his name. Agents draft, critique, and optimize; Ryan is the author of record and the final edit on every artifact. Nothing publishes without his explicit action (merge, send, upload). This mirrors the ButterStack agent hard lines and is non-negotiable: the story is "I built this with AI leverage," and the writing itself has to demonstrate the same honest human-AI split it describes.

## 2. The team

| Who | Role here | How engaged |
|---|---|---|
| Ryan | Author, narrator, final edit, all publishing actions | (himself) |
| Melissa (ButterStack marketing agent) | Blog drafting support, SEO pass, social derivatives, campaign calendar for the 4-part rollout | `aida ask melissa "..."` via the roster, or directly in butter_stack context |
| Dana (ButterStack designer agent) | Critique of diagrams, article layout, and any video thumbnails/slides; design score before anything ships | `aida ask dana "..."` / designer subagent |
| Claude (Fable) | Orchestration, engineering notes, HISTORY.md upkeep, technical accuracy review of drafts, diagram first-drafts | main-loop sessions in this repo |

Working arrangement: Melissa and Dana are ButterStack agents with ButterStack hard lines (draft-only, never publish, never commit). For this personal-brand project they work against a content workspace, not `blog/_posts/` in butter_stack - proposed: `~/dev/aida-content/` (private repo, becomes the staging ground for posts, scripts, diagrams). Their outputs are inputs to Ryan's edit, not finished posts.

SEO standing rule (recorded feedback): minimum-effective set only. Titles, descriptions, slugs, one keyword focus per part, internal links. No schema spam, no keyword stuffing, nothing "icky." Melissa's SEO pass is shown as a diff before it is applied.

## 3. Content inventory (what must exist before a part publishes)

Part 1 shipped 2026-09-03 ([post](https://www.ryanlitalien.com/posts/an-agent-of-agents-for-one-person/)) on a lighter loop than the one below: Ryan wrote the lede and the edits, Fable drafted the body and the two persona intros, Ryan did the rewrite pass, no Melissa/Dana pass, no diagrams, no video. Items 1, 4 and 7 happened; 2, 3, 5 and 6 did not. Treat the list as the target for parts 2-4, not a description of part 1.

Per blog part (the 4-part structure is defined in plan-open-source.md Phase 4):

1. Outline approved by Ryan (one page: thesis, beats, which sidebar links)
2. Diagrams for that part (Section 4)
3. Technology-rationale entries it cites (Section 5)
4. Inspiration links it credits (Section 6)
5. At least one failure story with its fix (Section 7)
6. Melissa SEO pass + social derivatives (drafted, gated)
7. Fable technical-accuracy pass against the actual code

Shared, one-time artifacts:

- `HISTORY.md` (done, ships with the fresh repo; the blog links to it instead of retelling the timeline)
- The public README (plan-open-source.md Phase 2)
- Engineering notes in `docs/` that sidebars link to (loop, voice pipeline, LMD, feedback mechanics)

## 4. Diagrams workstream

Required set, roughly one hero diagram per part plus supporting ones:

| Diagram | Used by | Notes |
|---|---|---|
| Ecosystem map (aida + config + brain + wiki + android + agents) | part 1, README | the "what did he actually build" picture |
| Six-step pipeline with the LLM/deterministic split visually explicit | part 2, README | the signature diagram; exists as `docs/pipeline-diagram.mmd`, needs a redesign pass |
| Routing walkthrough of one real query (scores, boosts, winner) | part 2 | annotated `--explain` output |
| Memory levels L0-L4 as a layered stack | part 3 | table exists in plan-open-source.md; needs visual form |
| Voice turn sequence (mic -> VAD -> STT -> tools -> TTS) with latency budget | part 1 or engineering note | |
| Loop lifecycle (task -> agent -> gate -> PR, never merge) | part 4 sidebar | |
| Two rosters, one seam (home roster <-> company roster: discover, one hop, depth guard, reports not files) | part 4, ButterStack section | ASCII first cut + notes in `docs/diagrams/team-seam.md` (2026-09-07); needs the Dana pass |
| Three-repo / fresh-cut picture (what is public, what stays private) | part 1, HISTORY.md context | doubles as the transparency statement |

Production loop: Fable drafts (mermaid or Excalidraw via the excalidraw MCP), Dana critiques with a design score, Ryan picks. Diagrams must read at blog-column width and survive both themes. Every diagram lives in the repo (`docs/diagrams/`) so the public repo and the blog share one source of truth.

## 5. Technology rationale register (why X over Y)

To be written as short entries (3-5 sentences each), collected in one engineering note, cited by parts 2-3. The register, from the actual decisions:

- Go over Python/TypeScript: single static binary, trivial cross-compile, no runtime env on satellite machines, goroutines for the parallel fan-out
- SQLite + files-as-truth over a vector database: the brain is a git repo; the DB is a rebuildable index, not a store
- Voyage embeddings over OpenAI/local: quality-per-cost for recall; the reembed migration proved the swap cost is one command
- whisper.cpp local STT over cloud STT: privacy (an always-on home mic never leaves the box), latency, Metal acceleration
- Piper TTS baseline + ElevenLabs streaming upgrade: offline-capable default, paid quality where latency budget allows; the A/B harness that decided it
- Anthropic primary + multi-provider fallback (OpenAI-compatible, Ollama, Gemini): LLM at the edges means the edges are swappable
- MCP both directions over bespoke plugins: server (other agents reach the brain/tasks) and client (discovery of the user's existing servers)
- Tailscale-only bind + bearer token for the phone surface over a public endpoint: the LMD listener design
- Cobra/lipgloss for CLI ergonomics; errgroup + semaphore over a queue system for parallelism
- claude subprocess reuse over direct API integrations where MCP servers already exist (the "shell out to claude -p" pattern and its tradeoffs)

Each entry states the alternative considered and the one reason that decided it. No entry without a real decision behind it.

## 6. Inspiration and prior-art register

Part 1's "What I Borrowed" section ([post](https://www.ryanlitalien.com/posts/an-agent-of-agents-for-one-person/)) already credits and links the core of this list: the Ralph loop (Huntley, snarktank/ralph), Elastic Atlas, the Karpathy LLM-wiki gist, LangChain, Voyage, and MCP. The engineering note extends it with what the post left out (OKF, the Obsidian karpathy-wiki plugin, Letta's Context Constitution, LangChain's "Your Harness, Your Memory", Anthropic's "Building Effective Agents", OpenViking) and the post links back to the note once it exists.

Collected as a credited list (one engineering note; part sidebars link into it): the Ralph loop pattern, Karpathy-style LLM-maintained wikis, memory-consolidation work (episodic-to-semantic promotion, decay scoring), the OpenAI agents SDK (the orchestrator port), A2A protocol, OKF, MCP, the jgkawell/jarvis Piper voice (MIT), wttr.in, and the harness-engineering essays that shaped the loop design. Rule: credit generously and link primary sources; the blog's credibility rests on honest lineage (plan-open-source.md Part 1 makes this a feature, not a confession).

## 7. Failure-story inventory (the "things that went wrong" spine)

Mined from the real history; each is a candidate sidebar or section, told as symptom -> wrong hypothesis -> actual cause -> fix:

1. The echo misfire: the always-on mic heard the assistant's own TTS and re-answered itself; fixed by flushing self-audio after speaking
2. The silent TCC death: adhoc codesigning changes the binary's identity every rebuild, silently voiding the macOS Input Monitoring grant; push-to-talk died with zero errors; fix was a stable Developer ID
3. The stale cache: the Tier-1 answer cache confidently served old answers to time-sensitive questions; fix was admission guards + TTL
4. The 10-second tax: the brain index rebuilt on every query for weeks before anyone noticed; fix was staleness detection + detached rebuild
5. Every daemon job 400'd on turn 1: an ask_user dedup bug in run-dir mode
6. The K.E.V.I.N. rename that lasted one day (naming is hard; the second full-sweep rename followed 24 hours later)
7. The shell-injection guard: sources with bare `{query}` exec templates meant the LLM's prose could reach `sh -c`; the library now rejects them at load
8. Whisper hallucinations ([BLANK_AUDIO], "(upbeat music)") polluting the audit log until filtered
9. The Nest-cast satellite feature that shipped and got reverted: latency never justified the complexity; kept as an honest "we deleted it" story
10. Cred-scrubbing vs OAuth: subprocess agents inheriting API-key env vars silently bypassed the subscription auth path

Rule: every part carries at least one of these. They are the most read and most trusted sections of engineering blogs, and they are what separates this from launch-copy.

## 8. YouTube: sketch and verdict

Sketch: a build-in-public devlog channel, not a tutorial channel. Format A (primary): one 8-15 minute screencast per blog part - the demo the article describes, performed live, imperfections included. Format B (optional later): short clips (voice demo, loop opening a PR by itself) cut from A. Production floor: screen capture + voiceover + minimal editing; no studio, no thumbnails arms race (Dana can make a clean template once).

Verdict: yes, but as a pilot, not a commitment. Do exactly four videos (one per part), published alongside each article, then assess. Rationale for: video is the only medium that makes the voice layer and the autonomous loop visceral; a text blog cannot convey "I said a sentence and a PR appeared." It compounds the consulting angle (Section 9) more than anything else - a prospect watching aida work is a sales call half-done. Rationale for caution: video production is a time sink with a consistency expectation; a stalled channel reads worse than no channel. The pilot-of-four bounds that risk.

Hard rule for recordings: never record against the real brain/config. Record against a demo profile with fictional data (the same fictional seed set from the golden-seed split). A screen recording leaks harder than a repo - one frame of a real task list or partner name is a permanent leak.

## 9. Positioning: consulting, ButterStack, and the gamer-AI line

- Consulting (the FDE/agentic-delivery angle, currently branded under ButterStack Consulting rather than ITS): the strongest beneficiary. The series is a public, verifiable demonstration of exactly the skill being sold - designing, shipping, and operating agentic systems with honest engineering judgment. Each part ends with a low-key one-line pointer to the consulting practice; no hard sell inside the content.
- ButterStack: benefits by association, on purpose, in part 4 only - the "patterns proved in aida, redeployed in ButterStack" case study (build-failure analysis, the Producer). The blog is Ryan's personal property, not ButterStack marketing; Melissa helps as staff, ButterStack branding appears only where the story genuinely involves it.
- The gamer-AI line (critical): the games audience is hostile to AI in games - generated art, generated content, AI replacing creative work. The safe and honest position, held everywhere in this content: AI builds the tooling that builds the games; humans build the games. Aida, the loop, the Producer, ButterStack's failure analysis are all developer-infrastructure stories. Zero examples involving generated game content, generated art, or AI replacing creative roles; do not even use such examples hypothetically. Every part gets a check against this line before publishing (Melissa flags, Ryan judges).

## 10. Sequencing

1. Now (parallel with Phase 0 scrub): approve this plan; stand up the content workspace; Fable drafts the diagram set + the two registers (Sections 5-6) from repo evidence
2. Phase 0 done: outlines for all four parts (Ryan + Fable), Melissa drafts campaign calendar; Dana critiques diagrams
3. Phase 1-2 (bootstrap + docs): parts drafted one at a time - Ryan outline, Fable technical pass, Melissa SEO/social pass, Ryan final edit; record the matching pilot video against the demo profile
4. Publish (revised 2026-09-03): part 1 shipped first, without the repo, video, or diagrams. Parts 2-4 follow on the weekly cadence with the full loop above; the fresh-start cut executes (per plan-open-source.md, on Ryan's go) and the repo goes public alongside part 4, which ends with the announcement. The video pilot starts with part 2 if it starts; a part 1 video can be backfilled.
5. After part 4: retro - decide YouTube continuation, engineering-note follow-up posts, and whether the series becomes a talk

## 11. Risks

| Risk | Mitigation |
|---|---|
| Screen recordings leaking real data | Demo profile only (Section 8 hard rule); pre-publish frame check |
| Former-employer references sliding into content | The plan-open-source.md cross-cutting rule applies to every artifact, video audio included |
| Gamer-AI backlash | Section 9 line, checked per part |
| SEO pass overreaching | Minimum-effective standing rule; diffs shown before applying |
| Agent-drafted voice reading as inauthentic | Ryan's final edit is a rewrite pass, not a proofread; the human-AI split is itself disclosed in part 1 |
| Scope creep (channel expectations, weekly-forever cadence) | Pilot-of-four; the series is finite by design |
