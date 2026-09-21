# Aida Harness Evolution Plan

> Synthesized from Martin Fowler, OpenAI, Stripe, Anthropic, LangChain, Garry Tan's GBrain, and 2026 industry trends.
> Created: 2026-04-11
> Historical roadmap: kept as-is for lineage. Superseded in places by later decisions (the partners registry it references was removed 2026-09; the SQLite driver is mattn/go-sqlite3, not modernc) - HISTORY.md carries the current timeline.

---

## Table of Contents

1. [Industry Landscape: What 2026 Says](#1-industry-landscape)
2. [Gap Analysis: Where Aida Stands](#2-gap-analysis)
3. [The One-Shot vs Chat Question](#3-one-shot-vs-chat)
4. [The Shared Memory / Brain Architecture](#4-shared-memory)
5. [Implementation Roadmap](#5-implementation-roadmap)
6. [Sources](#6-sources)

---

## 1. Industry Landscape

### The Three Evolutions of Developer-AI Interaction

| Era | Focus | Key Question |
|-----|-------|--------------|
| 2023 | **Prompt Engineering** | "How do I ask the model better?" |
| 2024-25 | **Context Engineering** | "What should the model see and when?" |
| 2026 | **Harness Engineering** | "What system of constraints, feedback loops, and automation wraps the model?" |

The defining equation: **Agent = Model + Harness**. The model is no longer the bottleneck - the harness is. LangChain proved this empirically: changing nothing about the model, only the harness, jumped their agent from 52.8% to 66.5% on Terminal Bench 2.0 (Top 30 → Top 5).

### Six Key Patterns from Industry Leaders

#### Pattern 1: Guides + Sensors (Martin Fowler)

Control theory applied to agents:

- **Guides (feedforward)**: Steer the agent *before* it acts - CLAUDE.md, AGENTS.md, architectural constraints, rule files
- **Sensors (feedback)**: Observe *after* the agent acts and enable self-correction - linters, tests, type checkers, evaluator agents

Both can be **computational** (deterministic, fast, cheap) or **inferential** (LLM-powered, slower, expensive). Prefer computational where possible; use inferential for semantic judgment.

Aida equivalent: The deterministic middle (classify → resolve → plan) is a guide. Lessons + quality scoring is a sensor. But Aida lacks inferential sensors (evaluator agents) and computational sensors (linting/validation of generated queries).

#### Pattern 2: Blueprints - Deterministic + Agentic Nodes (Stripe)

Stripe's Minions use "blueprints" - state machines that mix deterministic nodes (run configured linters, no LLM) with agentic nodes (implement task, LLM reasoning). Benefits:
- Guarantees specific subtasks complete deterministically
- Saves tokens at scale
- Reduces agent failure surface
- Enables team-specific customization

Aida equivalent: The 6-step pipeline IS a blueprint. Steps 2-4 are deterministic nodes, steps 1/5/6 are agentic nodes. This is architecturally validated by Stripe's approach. But Aida could add more deterministic sensor nodes (query validation, result verification).

#### Pattern 3: Scoped Context, Not Global Bloat (Everyone)

OpenAI's key lesson: "Give a map, not a 1,000-page instruction manual." Stripe's rule files are scoped to directories/patterns, not unconditionally global. GBrain's CLAUDE.md was 20,000 lines before Garry Tan realized 200 lines of pointers works far better.

Aida equivalent: Source-specific context loading is already right. The library system with route-based activation is spot-on. This is a strength.

#### Pattern 4: Shifting Feedback Left (Stripe)

Automated checks run earliest and closest to development:
1. Pre-push local linting (<1 second, cached)
2. Local blueprint linting node
3. First CI iteration (autofixes applied)
4. Second chance (one more agent fix attempt)
5. Human handoff (diminishing returns beyond 2 iterations)

Aida equivalent: The quality scoring (PRM-lite) is a feedback sensor, but it runs AFTER synthesis. There's no pre-execution validation of generated queries or post-execution verification of results.

#### Pattern 5: Multi-Agent Specialization (Anthropic)

Three-agent architecture for long-running apps:
- **Planner**: Converts prompts into detailed specs
- **Generator**: Implements iteratively
- **Evaluator**: Independent quality assessment

Key insight: "Separating the agent doing the work from the agent judging it is a strong lever." Agents consistently overestimate their own work quality.

Aida equivalent: The parser, executor, and synthesizer are separate concerns but all use the same model without independent evaluation. Adding a verifier step between execution and synthesis would catch bad source results before they poison the answer.

#### Pattern 6: Compiled Truth + Knowledge Compounding (GBrain)

Every brain page has two layers:
- **Above the HR**: Compiled truth - current best understanding, rewritten when evidence arrives
- **Below the HR**: Timeline - append-only, reverse-chronological evidence trail

This is NOT RAG (which re-derives from scratch each query). It's pre-synthesized knowledge that any agent can trust without re-processing. The agent runs enrichment cron jobs overnight (the "dream cycle").

Aida equivalent: `lessons.jsonl` is append-only evidence (like the timeline), but there's no compiled truth layer. Aida never synthesizes "what I've learned about routing queries about partner X" into a pre-computed artifact.

---

## 2. Gap Analysis

### Where Aida is AHEAD or ON PAR

| Capability | Aida Implementation | Industry Validation |
|-----------|----------------------|-------------------|
| LLM at edges, deterministic middle | 6-step pipeline, steps 2-4 are pure Go | Stripe blueprints, OpenAI constraints |
| Config-driven routing | sources.yaml + routes.yaml + planner scoring | Stripe MCP tool curation, OpenAI layer enforcement |
| Parallel execution | errgroup.SetLimit(5) with phase dependencies | Universal pattern (errgroup/Promise.all/asyncio) |
| Scoped context loading | Source context files, library layers, route activation | Stripe rule files, OpenAI map-not-manual |
| Learning from execution | lessons.jsonl + LLM router with K=5 similar | Unique advantage - most tools don't have this |
| Library system | Multi-root, layers, skills, sources, routes | Ahead of most CLI tools |
| Profile auto-detection | Tool availability checks | Standard practice |

### Where Aida is BEHIND

| Gap | What Industry Does | Aida Today | Priority |
|-----|-------------------|-------------|----------|
| **Shared memory across instances** | GBrain: git-based brain repo + remote MCP; Mem0/Zep/Letta frameworks | lessons.jsonl is local only | **P0** |
| **Result verification** | Anthropic: independent evaluator agent; Stripe: 2-attempt bounded iteration | Quality scoring after synthesis only | **P1** |
| **Confidence scoring** | Stripe: per-source quality metrics; Anthropic: calibrated evaluators with scoring breakdowns | No confidence on routing, results, or answers - only post-hoc Quality 1-5 | **P1** |
| **Compiled knowledge** | GBrain: compiled truth pages, rewritten on new evidence | Raw lessons only, never synthesized | **P1** |
| **Observability / tracing** | LangChain: traces for every agent run; OpenAI: telemetry-driven development | Run logs exist but no structured tracing | **P2** |
| **MCP server exposure** | LangChain Deep Agents: expose as MCP tool; GBrain: 37 operations via MCP | Cannot be called by other agents | **P2** |
| **Garbage collection agents** | OpenAI: background Codex tasks scan for deviations | No periodic maintenance | **P2** |
| **Skill framework** | GBrain: markdown procedures + executables; LangChain: agent skills standard | Library skills exist but are passive context only | **P3** |
| **Multi-agent coordination** | A2A protocol, orchestrator-subagent patterns | Orchestrator package built (`internal/engine/orchestrator/`): Agent + Runner + Sequential/Parallel/Loop composites + handoffs + guardrails + A2A bridge. Not yet wired into `aida investigate` - parity only, migration is a follow-up. | **P3 (foundation done, migration open)** |
| **A/B testing / bandits** | Stripe: minion variants; LangChain: reasoning sandwiches | No routing experimentation | **P3** |
| **Context resets / compaction** | Anthropic: structured handoffs; Claude Code: context compaction | N/A (one-shot, no long context issue) | **P4** |

---

## 3. The One-Shot vs Chat Question

**TL;DR: No, Aida does not need to become a chat app.**

### Evidence That One-Shot Wins at Scale

Stripe's Minions are the strongest proof point. They are explicitly one-shot, and they're the most successful agent system at scale: **1,300 PRs merged weekly, zero human-written code**. Their reasoning:

- **Error compounding math**: A five-step chain at 95% per-step accuracy yields only 77% end-to-end. One-shot eliminates this compounding.
- **Autonomy by design**: "The most unique aspect of minions is the absence of a supervisory human."
- **Bounded iteration**: Not truly single-attempt - they get 2 CI cycles before human handoff. But there's no chat loop.

### What Chat Mode Actually Provides (and How to Get It Without Chat)

| Chat Mode Benefit | Aida Alternative |
|------------------|-------------------|
| Clarification on ambiguous queries | Better parsing + `--dry-run` showing the plan before execution |
| Iterative refinement of results | `aida thumbs-down --because "should have used X"` → re-query with learned routing |
| Deep investigation with follow-ups | `aida investigate` (cloud agent, already exists) |
| Building context over a session | Compiled knowledge pages (see shared memory below) |
| Learning user preferences | `soul.yaml` + feedback reasons propagating to synthesizer |

### The Hybrid Model (Recommended)

Keep `aida` as one-shot for the 90% case. Add a lightweight **`aida session`** mode for the 10% case:

```
aida session "investigate checkout failures for pine-hollow"
```

This would:
1. Run the normal pipeline
2. Show the answer
3. Offer a prompt: `Follow up? (or Enter to exit)`
4. If the user follows up, carry forward the prior context (results, entities, sources used)
5. Each follow-up is another pipeline run with accumulated context - NOT a chat with a model
6. Session context is ephemeral (not persisted to lessons unless explicitly thumbs-up/down)

This gives you investigation depth without the architectural complexity of a chat app. The core pipeline stays one-shot. Sessions are just chained one-shots with carried context.

---

## 4. The Shared Memory / Brain Architecture

This is the biggest gap and the most exciting opportunity. You're running Claude Code across work/home, multiple projects - and none of that knowledge compounds.

### The Problem

```
┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐
│  Claude Code     │  │  Claude Code     │  │  Claude Code     │
│  Work Project A  │  │  Home Project B  │  │  Work Project C  │
│                  │  │                  │  │                  │
│  ~/.claude/      │  │  ~/.claude/      │  │  ~/.claude/      │
│  memory/         │  │  memory/         │  │  memory/         │
│  (isolated)      │  │  (isolated)      │  │  (isolated)      │
└─────────────────┘  └─────────────────┘  └─────────────────┘
         ↕                    ↕                    ↕
    lessons.jsonl        (nothing)            (nothing)
    (aida only)
```

Each Claude Code instance has project-scoped memory. Aida has lessons.jsonl but it's local. Nothing connects them.

### The Architecture (Inspired by GBrain, Adapted for Aida)

**Design principle**: Markdown files are the source of truth (human-readable, version-controlled, Claude Code reads directly). SQLite + sqlite-vec is the index/retrieval layer (semantic search, structured queries, fast similarity matching). The database is **derived from** the files - you can always nuke the DB and rebuild from markdown.

```
┌──────────────────────────────────────────────────────────────┐
│                  BRAIN REPO - Source of Truth (git)           │
│                  ~/.aida/brain/ (markdown files)            │
│                                                               │
│  entities/            knowledge/          meta/               │
│  ├── partners/        ├── routing/        ├── soul.yaml       │
│  │   ├── pine-hollow.md   │   └── compiled.md │  └── sync.yaml    │
│  │   └── shopify.md   ├── patterns/       │                   │
│  ├── tools/           │   └── checkout    │                   │
│  │   ├── snow.md      │       -debug.md   │                   │
│  │   └── chrono.md    └── domains/        │                   │
│  └── people/              └── payments.md │                   │
│      └── ryan.md      lessons/            │                   │
│                        ├── compiled.md    │                   │
│                        └── raw.jsonl      │                   │
├──────────────────────────────────────────────────────────────┤
│                  INDEX LAYER - SQLite + sqlite-vec            │
│                  ~/.aida/brain.db (embedded, derived)       │
│                                                               │
│  ┌─────────────┐  ┌───────────────┐  ┌──────────────┐        │
│  │   lessons    │  │ routing_rules │  │   entities   │        │
│  │ + embeddings │  │ + embeddings  │  │ + embeddings │        │
│  └─────────────┘  └───────────────┘  └──────────────┘        │
│                                                               │
│  Semantic search · Structured queries · Aggregation           │
│  Rebuilt from markdown via `aida brain index`                   │
├──────────────────────────────────────────────────────────────┤
│                  SYNC LAYER                                   │
│                                                               │
│  Git sync ─── Separate private repo (always works)            │
│  MCP server ── `aida brain serve` (cross-agent access)          │
└───────────────────────┬──────────────────────────────────────┘
                        │
             ┌──────────┼──────────┐
             │          │          │
             ▼          ▼          ▼
      ┌──────────┐ ┌──────────┐ ┌──────────┐
      │  aida CLI  │ │Claude    │ │Claude    │
      │ (aida) │ │Code @    │ │Code @    │
      │          │ │work      │ │home      │
      └──────────┘ └──────────┘ └──────────┘
```

### Four Layers

#### Layer 1: Brain Repo (Source of Truth)

A **separate, private git repository** - not inside the Aida codebase. The brain has a different lifecycle (auto-commits on every query) and different sharing model (private, contains work entities) than the Aida source code. Keeping them separate prevents commit pollution and keeps `git log` on Aida meaningful.

```yaml
# ~/.aida/config.yaml
brain:
  path: ~/.aida/brain                              # local checkout
  remote: git@github.com:ryanlitalien/aida-brain.git      # private repo
  auto_sync: true                                     # pull/push on every query
```

Human-readable, version-controlled, editable. This is what Claude Code reads directly via file access. This is what you can open in an editor and make sense of.

**Directory structure** (MECE - Mutually Exclusive, Collectively Exhaustive):

```
~/.aida/brain/                    # Separate private git repo
├── .gitignore                      # brain.db, *.embedding.cache
├── RESOLVER.md                     # Decision tree: "where does this info go?"
├── entities/
│   ├── partners/                   # One page per partner
│   │   ├── pine-hollow.md
│   │   └── shopify.md
│   ├── tools/                      # One page per tool/source
│   │   ├── snowflake.md
│   │   └── chronosphere.md
│   └── people/                     # People you work with
│       └── ryan.md
├── knowledge/
│   ├── routing/                    # Compiled routing wisdom
│   │   └── compiled.md             # "When X type of question, use Y source because Z"
│   ├── patterns/                   # Recurring investigation patterns
│   │   └── checkout-debugging.md
│   └── domains/                    # Domain knowledge
│       └── payments.md
├── lessons/
│   ├── compiled.md                 # Synthesized routing lessons (THE key file)
│   ├── work/                       # Profile-partitioned lesson files
│   │   ├── 2026-04-11T14-32-00-a3f2.json
│   │   └── 2026-04-11T15-01-00-b7c1.json
│   ├── home/
│   │   ├── 2026-04-11T19-45-00-c8d3.json
│   │   └── 2026-04-12T09-12-00-e4f5.json
│   └── mobile/                     # Future profiles auto-created
│       └── ...
└── meta/
    ├── soul.yaml                   # User preferences, identity
    └── sync.yaml                   # Sync configuration
```

**Why one file per lesson, split by profile:**
- **Zero merge conflicts**: Two machines never create the same filename (timestamp + hash). Git handles concurrent file additions cleanly.
- **Profile separation**: Work lessons stay in `lessons/work/`, home in `lessons/home/`. Easy to filter, audit, or archive by context.
- **No JSONL conflicts**: The old `raw.jsonl` approach would conflict when two machines append to the same file. One file per lesson eliminates this entirely.
- **Auto-organized**: Profile directory is created automatically from the active Aida profile. No manual setup.

**Lesson file format** (one JSON file per query):

```json
{
  "id": "2026-04-11T14-32-00-a3f2",
  "ts": "2026-04-11T14:32:00Z",
  "profile": "work",
  "question": "why are checkout failures spiking for pine-hollow?",
  "action": "investigate",
  "strategy": "investigate",
  "sources": ["chronosphere", "snowflake"],
  "per_source_status": {"chronosphere": "success", "snowflake": "success"},
  "artifact_count": 8,
  "quality": 4,
  "quality_reason": "Good root cause analysis with supporting log evidence",
  "routing_confidence": {"chronosphere": 0.91, "snowflake": 0.65},
  "result_confidence": {"chronosphere": 0.88, "snowflake": 0.42},
  "answer_confidence": 0.82,
  "grounding": "partially",
  "source_contribs": {"chronosphere": 0.75, "snowflake": 0.20},
  "answer_snippet": "Checkout failures increased 3x due to...",
  "feedback": "",
  "feedback_reason": ""
}
```

**Page format** (following GBrain's compiled truth pattern):

```markdown
# Snowflake (Tool)

Snowflake is the primary data warehouse for a former employer's payments stack. Best used for
merchant-level queries, transaction lookups, and financial data.
Typical query latency: 5-30 seconds. Requires `snow` CLI on PATH.

**Routing guidance**: Prefer over Chronosphere for anything involving
amounts, partner IDs, or transaction counts. Avoid for real-time
log analysis.

**Common pitfalls**: Date formats must be YYYY-MM-DD. Always filter
by partition key for performance.

---

## Evidence Trail

- 2026-04-11: Routed checkout volume query → Snowflake → quality 5/5, 12 results
- 2026-04-10: Routed error rate query → Snowflake → quality 2/5, wrong source (should have been Chrono)
- 2026-04-09: User thumbs-down: "snowflake too slow for real-time logs"
```

#### Layer 2: SQLite Index (Retrieval Layer)

The brain.db is a **derived index** - built from the markdown files, never the other way around. It exists to answer questions that grep can't: "find the 5 most semantically similar past queries" or "which sources have quality < 3 for investigate-strategy queries?"

**Schema:**

```sql
-- Lessons with embeddings and confidence scores
CREATE TABLE lessons (
    id TEXT PRIMARY KEY,
    timestamp TEXT NOT NULL,
    question TEXT NOT NULL,
    question_embedding BLOB,      -- sqlite-vec vector
    action TEXT,                   -- investigate, query, lookup, etc.
    strategy TEXT,
    sources_used TEXT,             -- JSON array
    per_source_status TEXT,        -- JSON object {source: status}
    quality INTEGER,               -- 1-5 PRM score
    feedback TEXT,                 -- thumbs-up/thumbs-down/null
    feedback_reason TEXT,
    answer_snippet TEXT,
    brain_page_refs TEXT,          -- JSON array of related brain pages
    -- Confidence scoring (Phase 2)
    routing_confidence TEXT,       -- JSON: {"snowflake": 0.87, "chrono": 0.65}
    result_confidence TEXT,        -- JSON: {"snowflake": 0.85, "chrono": 0.4}
    answer_confidence REAL,        -- 0.0-1.0 overall answer confidence
    grounding TEXT,                -- "fully", "partially", "weakly", "ungrounded"
    source_contribs TEXT           -- JSON: {"snowflake": 0.8, "chrono": 0.15}
);

-- Compiled routing rules (synthesized from lessons)
CREATE TABLE routing_rules (
    id TEXT PRIMARY KEY,
    pattern TEXT,                  -- "checkout errors for partner X"
    pattern_embedding BLOB,
    recommended_sources TEXT,      -- JSON array
    avoid_sources TEXT,            -- JSON array
    confidence REAL,               -- 0.0-1.0
    evidence_count INTEGER,
    last_compiled TEXT
);

-- Entity registry (indexed from partner/tool markdown pages)
CREATE TABLE entities (
    slug TEXT PRIMARY KEY,
    type TEXT NOT NULL,            -- partner, tool, person
    name TEXT NOT NULL,
    aliases TEXT,                  -- JSON array
    page_path TEXT,                -- path to markdown source of truth
    summary TEXT,                  -- first paragraph of compiled truth
    summary_embedding BLOB,
    last_indexed TEXT
);

-- Virtual table for vector similarity search
CREATE VIRTUAL TABLE lessons_vec USING vec0(
    id TEXT PRIMARY KEY,
    question_embedding FLOAT[1024]
);
```

**Why SQLite + sqlite-vec (not Postgres):**

| Factor | SQLite | Postgres/Supabase |
|--------|--------|-------------------|
| Infrastructure | Zero - it's a file | Needs a server or hosted service |
| Offline | Always works | Needs network |
| Startup latency | ~0ms (embedded) | Connection handshake |
| Go integration | `modernc.org/sqlite` (pure Go, no CGO) | `pgx` (needs network) |
| Deployment | Ships with `aida` binary | External dependency |
| Vector search | `sqlite-vec` extension | pgvector |
| Cloud sync | Turso (optional) | Built-in with Supabase |

**What each memory type actually needs:**

| Memory Type | Best Store | Why |
|-------------|-----------|-----|
| Routing wisdom (similar past queries) | **Database** - vector similarity search | Jaccard → vector is a strict upgrade |
| Execution history (lessons) | **Database** - structured queries, aggregation | "All queries where source X scored < 3" |
| Entity knowledge (partners, tools) | **Files** - long-form context for LLM prompts | These are context documents, not records |
| Domain knowledge (patterns, guides) | **Files** - same reason | Claude Code reads directly |
| User preferences (soul) | **File** - simple, rarely changes | soul.yaml is fine |

#### Layer 3: Aida Integration (Read/Write Loop)

Aida reads from the brain at query time and writes back after each execution:

```
aida "why are checkout failures spiking for pine-hollow?"
  │
  ├─ 1. SEMANTIC SEARCH (brain.db)
  │    SELECT * FROM lessons l
  │    JOIN lessons_vec v ON l.id = v.id
  │    WHERE vec_distance(v.question_embedding, ?) < 0.3
  │    ORDER BY l.quality DESC, l.timestamp DESC
  │    LIMIT 5
  │    → Finds similar past queries + what worked
  │
  ├─ 2. ENTITY LOOKUP (brain.db)
  │    SELECT * FROM entities
  │    WHERE 'pine-hollow' IN (SELECT value FROM json_each(aliases))
  │    → Finds pine-hollow.md brain page
  │
  ├─ 3. LOAD CONTEXT (markdown files)
  │    Read brain/entities/partners/pine-hollow.md  (compiled truth)
  │    Read brain/knowledge/routing/compiled.md    (routing wisdom)
  │    → Rich context feeds LLM router (step 4.5)
  │
  ├─ 4. RUN PIPELINE (existing 6-step)
  │    Parse → Classify → Resolve → Plan → Route → Execute → Synthesize
  │
  └─ 5. WRITE BACK
       INSERT INTO lessons (...) → brain.db
       Append evidence line → brain/entities/partners/pine-hollow.md
       Append evidence line → brain/entities/tools/snowflake.md
       (git commit + sync in background, non-blocking)
```

**Implementation**: A new `internal/brain/` package that:
- Opens/creates `brain.db` on startup (SQLite, embedded, no external deps)
- Reads brain.db for semantic search + entity lookup at query time
- Loads matched markdown pages as LLM context
- Writes lesson records + evidence trails after each query
- Generates embeddings via Haiku (cheap, fast) or local model (offline)

**Compile triggers** (re-synthesize compiled truth from raw evidence):
- Every N queries (e.g., every 10)
- On explicit command: `aida brain compile`
- Via cron/scheduled task
- When evidence count exceeds threshold since last compile

**Reindex** (rebuild brain.db from markdown):
- `aida brain index` - full rebuild from all markdown files
- Runs automatically if brain.db is missing or stale
- Incremental: only re-embeds pages modified since last index

#### Layer 4: Sync & Cross-Agent Access

**Git sync (automatic, background):**

The brain repo is a separate private git repo. Sync is fully automatic - you never run git commands manually:

```
aida "question"
  ├─ START: git pull --rebase (background, non-blocking)
  │         If brain.db is stale → incremental re-index
  ├─ ... pipeline runs ...
  └─ END:   git add lessons/work/... entities/... 
            git commit -m "auto: work 2026-04-11T14:32:00"
            git push (background, non-blocking)
```

- One file per lesson, profile-partitioned → zero merge conflicts
- Entity page evidence lines are append-only → conflicts rare, union-merge safe
- brain.db is `.gitignore`'d → rebuilt locally, never synced
- Push failures silently queue → retried on next invocation
- Commit messages are prefixed `auto:` so they're filterable in `git log`

**MCP server (cross-agent access):**
- `aida brain serve` starts a local MCP server exposing brain operations
- Claude Code connects via MCP config
- Operations: `brain_search`, `brain_get_page`, `brain_put_page`, `brain_add_evidence`
- Wraps both SQLite queries and markdown file operations

### How This Connects Claude Code Instances

```
# In your global ~/.claude/CLAUDE.md, add:
# When working on projects, check ~/.aida/brain/ for context on:
# - Partners: ~/.aida/brain/entities/partners/
# - Tools: ~/.aida/brain/entities/tools/
# - Routing wisdom: ~/.aida/brain/knowledge/routing/compiled.md
# - Investigation patterns: ~/.aida/brain/knowledge/patterns/
```

Now every Claude Code instance - work, home, project A/B/C - reads from the same brain. When Aida learns that "Snowflake is bad for real-time logs," that knowledge is available to Claude Code when you're writing a CLAUDE.md for a new project.

For richer access, Claude Code can connect to `aida brain serve` via MCP and run semantic searches against brain.db - not just file reads but actual similarity queries.

### Confidence Scoring Architecture

Aida currently has no confidence signals. The `Quality` field (1-5) is a post-hoc grade on the final answer - it doesn't tell you *why* it was bad or *where* the pipeline lost confidence. Confidence scoring adds three measurement points across the pipeline, each feeding into the next and ultimately into the brain's compiled knowledge.

#### The Three Confidence Points

```
User Query
    │
    ▼
[PARSE]
    │
    ▼
[CLASSIFY + RESOLVE]
    │
    ▼
[PLAN + ROUTE]  ◄─── (1) ROUTING CONFIDENCE
    │                  "How sure am I these are the right sources?"
    │                  Per-source: 0.0-1.0
    ▼
[EXECUTE]
    │
    ▼
[VERIFY]        ◄─── (2) RESULT CONFIDENCE
    │                  "Did each source actually answer the question?"
    │                  Per-source: 0.0-1.0 + reason
    ▼
[SYNTHESIZE]
    │
    ▼
[SCORE]         ◄─── (3) ANSWER CONFIDENCE
                       "How grounded is this answer in the evidence?"
                       Overall: 0.0-1.0 + grounding breakdown
```

#### (1) Routing Confidence

**Where**: LLM router (step 4.5) - `internal/engine/router.go`

**What changes**: The LLM router currently returns `LLMRouteResult{Sources, Reason}`. Extend it to return a confidence per source:

```go
type LLMRouteResult struct {
    Sources []RoutedSource `json:"sources"`
    Reason  string         `json:"reason"`
}

type RoutedSource struct {
    Name       string  `json:"name"`
    Confidence float64 `json:"confidence"` // 0.0-1.0
    Rationale  string  `json:"rationale"`  // one-line why
}
```

**How it's computed**: The router prompt already sees past lessons. Add an instruction:

> For each source you pick, rate your confidence 0.0-1.0:
> - 1.0 = strong past evidence this source answers this exact type of question
> - 0.7 = good fit based on capabilities, but limited evidence
> - 0.4 = plausible but uncertain - might not have the right data
> - 0.1 = speculative, including as a backup

**How it's used**:
- Sources with confidence < 0.3 are flagged as speculative in verbose output
- If ALL sources have confidence < 0.4, the synthesizer gets a hint: "routing was uncertain - hedge the answer"
- Confidence is stored on the lesson record for brain learning

#### (2) Result Confidence

**Where**: New VERIFY step between EXECUTE and SYNTHESIZE - `internal/engine/verifier.go`

**What changes**: After execution, each `SourceResult` gets a confidence assessment:

```go
type VerifiedResult struct {
    Source          sources.SourceResult
    Confidence      float64 `json:"confidence"`       // 0.0-1.0
    Relevance       string  `json:"relevance"`        // "direct", "partial", "tangential", "irrelevant"
    ConfidenceReason string `json:"confidence_reason"` // one-line explanation
    ShouldRetry     bool   `json:"should_retry"`      // verifier thinks a retry might help
}
```

**How it's computed**: Two tiers - 

*Computational (fast, free, always runs):*
- Empty/error results → confidence 0.0
- Result contains entity IDs from the question → confidence boost +0.2
- Result contains keywords from the question → confidence boost +0.1
- Result is very short (<50 chars) for a query-type question → confidence penalty -0.3
- Artifact count: 0 artifacts → confidence 0.1, 1-3 → 0.5 baseline, 4+ → 0.7 baseline

*Inferential (LLM, optional, runs when computational confidence is ambiguous 0.3-0.7):*
- Lightweight Haiku call: "Given the question '{Q}' and this result, rate 0.0-1.0 how well the result answers it. Reply with JSON: {confidence, relevance, reason}"
- Only runs for results in the ambiguous zone - saves tokens on clear pass/fail

**How it's used**:
- Results with confidence < 0.2 are dropped from synthesizer context (don't waste tokens on junk)
- Results with confidence 0.2-0.5 are included but flagged: "low-confidence result from {source}"
- Results with `ShouldRetry=true` trigger one bounded retry with a modified query
- All confidences stored on lesson for brain learning

#### (3) Answer Confidence

**Where**: Enhanced quality scorer - `internal/engine/synthesizer.go`

**What changes**: The existing PRM-lite `scoreAnswer` call already produces `Quality` (1-5) and `QualityReason`. Extend it:

```go
type AnswerScore struct {
    Quality          int     `json:"quality"`           // 1-5 (existing)
    QualityReason    string  `json:"quality_reason"`    // existing
    Confidence       float64 `json:"confidence"`        // 0.0-1.0 (NEW)
    Grounding        string  `json:"grounding"`         // "fully", "partially", "weakly", "ungrounded"
    SourceContribs   map[string]float64 `json:"source_contribs"` // per-source contribution to answer
}
```

**How it's computed**: Add to the scorer prompt:

> Also assess:
> - confidence (0.0-1.0): how likely is this answer correct given the evidence?
> - grounding: is every claim supported by a source result? (fully/partially/weakly/ungrounded)
> - source_contribs: what fraction of the answer came from each source? (should sum to ~1.0)

**How it's used**:
- Low-confidence answers get a visible caveat in output: "⚠ Low confidence (0.3) - consider verifying"
- `source_contribs` feeds brain learning: if Source A contributed 0.8 and Source B contributed 0.05, Source B was a wasted query for this type of question
- Grounding level feeds compiled routing rules: "ungrounded" answers from a source pattern → avoid that route

#### How Confidence Compounds in the Brain

All three confidence scores are stored on each lesson record:

```sql
-- Extended lessons table
ALTER TABLE lessons ADD COLUMN routing_confidence TEXT;  -- JSON: {source: confidence}
ALTER TABLE lessons ADD COLUMN result_confidence TEXT;   -- JSON: {source: confidence}
ALTER TABLE lessons ADD COLUMN answer_confidence REAL;   -- 0.0-1.0
ALTER TABLE lessons ADD COLUMN grounding TEXT;           -- fully/partially/weakly/ungrounded
ALTER TABLE lessons ADD COLUMN source_contribs TEXT;     -- JSON: {source: fraction}
```

When `aida brain compile` runs, it aggregates these into **routing rules with evidence-backed confidence**:

```markdown
# Routing Wisdom (Compiled)

## Snowflake
- **Query strategy** (confidence: 0.87, n=34): Strong for transaction lookups,
  partner volume queries, financial aggregations
- **Investigate strategy** (confidence: 0.42, n=8): Weak for real-time debugging - 
  results often tangential, low grounding scores
- **Avoid for**: real-time logs (3 thumbs-down), error rate spikes (result confidence avg 0.2)

## Chronosphere
- **Investigate strategy** (confidence: 0.91, n=22): Primary source for error spikes,
  latency analysis, log pattern matching
- **Query strategy** (confidence: 0.55, n=12): Decent for time-series questions,
  but Snowflake usually has richer data
```

This compiled wisdom then feeds back into the router prompt on the next query - a closed loop where confidence scores from past runs directly improve future routing decisions.

#### Evidence Trail Format (Updated)

Brain page evidence lines now include confidence:

```markdown
---

## Evidence Trail

- 2026-04-11: checkout volume query → Snowflake → route:0.9 result:0.85 answer:0.92 [quality 5/5, 12 artifacts]
- 2026-04-10: error rate query → Snowflake → route:0.6 result:0.2 answer:0.3 [quality 2/5, wrong source]
- 2026-04-09: thumbs-down: "snowflake too slow for real-time logs" → intended:chrono
```

---

## 5. Implementation Roadmap

### Phase 1: Brain Foundation (P0 - Shared Memory)

**Goal**: Get the brain repo + SQLite index working so knowledge compounds across sessions and machines.

**1a. Brain repo structure**
   - Initialize `~/.aida/brain/` with RESOLVER.md and directory skeleton
   - No migration needed - old `lessons.jsonl` can be discarded (clean start)
   - Move `soul.yaml` → `brain/meta/soul.yaml`
   - Seed entity pages from existing `partners.yaml` and `sources.yaml`

**1b. SQLite index (`internal/brain/` package)**
   - Add `modernc.org/sqlite` (pure Go, no CGO) - no sqlite-vec, cosine similarity computed in Go
   - Create `brain.db` schema: `lessons`, `routing_rules`, `entities` with vector BLOBs
   - Embedding generation: Voyage AI (`voyage-3-lite`, 512 dims) - see Appendix A for details
   - `aida brain index` - full rebuild of brain.db from all lesson JSON files + markdown pages
   - Auto-rebuild if brain.db missing on startup

**1c. Brain read at query time**
   - Semantic search brain.db for K=5 similar past lessons (replaces current Jaccard similarity)
   - Entity lookup in brain.db to find matching brain pages
   - Load matched markdown pages (compiled truth sections) into LLM router context
   - Load matching pattern pages when strategy is `investigate`

**1d. Brain write after query**
   - Write lesson JSON file to `brain/lessons/{profile}/{timestamp}-{hash}.json`
   - INSERT lesson record into brain.db (with embedding)
   - Append evidence line to relevant entity/tool markdown pages
   - All writes are local-first, git sync is async (background goroutine)

**1e. Brain compile command**
   - `aida brain compile` - LLM reads raw evidence, rewrites compiled truth pages
   - Focus on `knowledge/routing/compiled.md` first (highest impact)
   - Recompile entity pages where evidence count exceeds threshold
   - Rebuild `routing_rules` table from compiled markdown
   - Run as background task or explicit command

**1f. Sync (fully automatic)**
   - `aida brain init` - clone or create `ryanlitalien/aida-brain` (private) with remote
   - Auto-pull on every `aida` invocation (background goroutine, non-blocking)
   - Auto-commit + push after every query (background goroutine, non-blocking)
   - Commit messages prefixed `auto:` with profile and timestamp
   - Push failures queued silently, retried on next invocation
   - brain.db in `.gitignore` - always rebuilt locally
   - `aida brain sync` - manual force sync if needed (rarely used)

### Phase 2: Confidence Scoring & Result Verification (P1)

**Goal**: Add confidence measurement at three pipeline points, catch bad results before synthesis, and close the learning loop so confidence compounds in the brain.

**2a. Routing confidence (`internal/engine/router.go`)**
   - Extend `LLMRouteResult` with per-source `Confidence float64` + `Rationale string`
   - Update router prompt to request confidence 0.0-1.0 per picked source
   - Store on lesson record as `routing_confidence` JSON field
   - Display in `--verbose` output: `"Source: snowflake (confidence: 0.87)"`

**2b. Result verifier (`internal/engine/verifier.go` - new file)**
   - Computational tier (always runs, ~0ms):
     - Empty/error → 0.0
     - Entity/keyword overlap with question → boost
     - Artifact count heuristic → baseline
   - Inferential tier (Haiku, runs when computational confidence is 0.3-0.7):
     - "Does this result answer the question?" → `{confidence, relevance, reason}`
   - Produces `VerifiedResult` per source with confidence + relevance + retry flag
   - Results with confidence < 0.2 dropped from synthesizer context
   - Results with 0.2-0.5 included but flagged as low-confidence
   - Store on lesson record as `result_confidence` JSON field

**2c. Bounded retry for failed/irrelevant sources**
   - If verifier sets `ShouldRetry=true`, retry ONCE with modified query
   - Modification strategy: include verifier's reason in re-prompt ("prior query returned irrelevant results about X, need Y instead")
   - After second failure, mark source as failed and proceed
   - Record both attempts in brain evidence

**2d. Answer confidence (extend `internal/engine/synthesizer.go`)**
   - Extend existing `scoreAnswer` to also return `Confidence float64`, `Grounding string`, `SourceContribs map[string]float64`
   - Low-confidence answers (<0.4) get visible caveat in output
   - `source_contribs` identifies which sources actually contributed vs. were wasted
   - Store all on lesson record

**2e. Query validation before execution**
   - Deterministic check: does the generated query look valid for this source type?
   - SQL syntax check for Snowflake queries
   - Command safety check (already exists, extend)
   - Invalid queries get confidence 0.0 without executing (save tokens + latency)

**2f. Brain integration**
   - All three confidence scores persisted on every lesson in brain.db
   - `aida brain compile` aggregates confidence into routing rules:
     - Per-source, per-strategy average confidence with sample count
     - Auto-generated "avoid" rules when confidence is consistently < 0.3
   - Evidence trail lines updated to include confidence triplet: `route:X result:Y answer:Z`

### Phase 3: Observability & MCP (P2)

**Goal**: Make Aida observable and callable by other agents.

1. **Structured tracing**
   - OpenTelemetry-compatible spans for each pipeline step
   - Token counts, latency, model used per LLM call
   - Source execution timing and result sizes
   - Export to local JSON or OTLP endpoint

2. **MCP server mode**
   - `aida serve` - expose Aida as an MCP tool server
   - Tools: `aida_query`, `aida_investigate`, `aida_brain_search`
   - Claude Code or other agents can call Aida via MCP
   - Brain operations also exposed: `brain_get`, `brain_search`, `brain_put`

3. **Brain garbage collection**
   - `aida brain gc` - periodic cleanup agent
   - Deduplicate entity pages
   - Archive old evidence (>90 days)
   - Flag stale compiled truth (evidence has diverged)
   - Can run as cron job

### Phase 4: Session Mode & Skills (P3)

**Goal**: Support deeper investigations and reusable procedures.

1. **`aida session` mode**
   - Chained one-shots with carried context
   - Each follow-up runs the full pipeline with accumulated results
   - Session context auto-saved to brain if user gives feedback

2. **Active skills framework**
   - Skills as markdown procedures + executable scripts (following GBrain pattern)
   - `aida skill run checkout-debug --entity ABC123`
   - Skills can chain multiple `aida` queries with deterministic logic between them

3. **A/B routing experiments**
   - Fork routing decisions: run both LLM-routed and deterministic-routed in parallel
   - Compare quality scores
   - Auto-promote winning routing strategy to compiled truth

### Phase 5: Multi-Agent & Federation (P3-P4)

**Goal**: Aida as part of a larger agent ecosystem.

**Status:** foundation shipped in PR #29. `internal/engine/orchestrator/`
provides a provider-agnostic multi-agent coordination layer (ADK /
OpenAI Agents SDK / Claude Agent SDK ideas ported into Go):

- `Agent` / `Runner` / `Session` / `Handoff` / `Guardrail` primitives.
- `SequentialAgent`, `ParallelAgent`, `LoopAgent` workflow composites.
- A2A client + server via `trpc-a2a-go`: any Runnable can be served
  over A2A; any remote A2A endpoint can be plugged in as a Runnable.
- OpenTelemetry tracing at run / turn / tool span levels.

See `docs/plan-agent-orchestrator-research.md` for design rationale,
port decisions, OSS-extraction plan, and naming (argos).

**Remaining work in this phase:**

1. **A2A protocol support** - ✅ done (client + server wrapper under
   `orchestrator/a2a/`). Real two-binary roundtrip still needs a
   deployment verification pass.
2. **Federated brain** - unchanged; orchestrator doesn't touch brain.
3. **Agent delegation** - ✅ primitives done. **Migration open:**
   `aida investigate` still calls the legacy `agent_loop.go` and the
   Anthropic Managed Agents API rather than an orchestrator graph.
   Follow-up PR will either (a) build a `ParallelAgent` of specialist
   Agents for the decomposition case, or (b) treat the managed-agent
   session as a RemoteAgent. Both paths keep the existing UX.
4. **Cross-project routing** - unchanged; still lives in
   `internal/engine/crossproject.go`.
5. **Streaming** - deferred to #30. Current `model.LLM` is one-shot
   `Complete`; streaming tokens/tool_use deltas need a real caller
   (likely `aida ask --stream`) before the event-union design is
   finalized.

---

## 6. Sources

### Articles Digested

- [Martin Fowler: Harness Engineering for Coding Agent Users](https://martinfowler.com/articles/harness-engineering.html) - Guides vs Sensors framework, three harness types, working "on" vs "in" the loop
- [OpenAI: Harness Engineering](https://openai.com/index/harness-engineering/) - 1M lines with zero human code, layered architecture constraints, garbage collection agents
- [Stripe: Minions Part 1](https://stripe.dev/blog/minions-stripes-one-shot-end-to-end-coding-agents) - One-shot agents at scale, blueprint architecture, 1,300 PRs/week
- [Stripe: Minions Part 2](https://stripe.dev/blog/minions-stripes-one-shot-end-to-end-coding-agents-part-2) - MCP Toolshed (500 tools), scoped rule files, shifting feedback left, bounded iteration
- [Stripe: Selective Test Execution](https://stripe.dev/blog/selective-test-execution-at-stripe-fast-ci-for-a-50m-line-ruby-monorepo) - Run ~5% of tests per change, three-tier feedback, statistical test dependency inference
- [Anthropic: Harness Design for Long-Running Apps](https://www.anthropic.com/engineering/harness-design-long-running-apps) - Multi-agent specialization (Planner/Generator/Evaluator), context resets vs compaction, sprint contracts, "every harness component encodes what the model can't do"
- [LangChain: Deep Agents](https://blog.langchain.com/deep-agents-deploy-an-open-alternative-to-claude-managed-agents/) - Model-agnostic harness, memory as primary lock-in vector, AGENTS.md as open standard, skills framework
- [Garry Tan: GBrain](https://github.com/garrytan/gbrain) - Three-layer brain architecture, compiled truth + timeline, MECE directory schema, "thin harness fat skills", 37 operations via CLI/MCP, multi-device via remote MCP

### Key Supplementary Sources

- [LangChain: Improving Deep Agents with Harness Engineering](https://blog.langchain.com/improving-deep-agents-with-harness-engineering/) - 52.8% → 66.5% on TerminalBench by changing harness only
- [12 Agentic Harness Patterns from Claude Code](https://generativeprogrammer.com/p/12-agentic-harness-patterns-from) - Tiered memory, context compaction, parallel subagents in worktrees, hooks
- [Anthropic: Multi-Agent Coordination Patterns](https://claude.com/blog/multi-agent-coordination-patterns) - Generator-Verifier, Orchestrator-Subagent, Agent Teams, Message Bus, Shared State
- [Mem0: State of AI Agent Memory 2026](https://mem0.ai/blog/state-of-ai-agent-memory-2026) - Memory architecture tiers, contextual memory surpassing RAG
- [Addy Osmani: The Code Agent Orchestra](https://addyosmani.com/blog/code-agent-orchestra/) - Multi-agent orchestration patterns for coding

---

## Appendix A: Implementation Spec

Decisions and details needed to one-shot implement Phase 1 and Phase 2.

### A.1 Embeddings: Voyage AI

**What embeddings are**: A function that converts text into a fixed-length vector of numbers (e.g., 512 floats) where semantic meaning is preserved as geometric distance. Similar meanings → close vectors. This replaces Jaccard word-overlap with true semantic similarity.

**Why Voyage AI**: Anthropic's embedding partner. Simple REST API, cheap, purpose-built for retrieval. Claude models don't have a native embedding endpoint.

**Model**: `voyage-3-lite`
- Dimensions: 512
- Max input: 16,000 tokens
- Cost: ~$0.05 per million tokens (negligible for lesson volumes)
- API: `https://api.voyageai.com/v1/embeddings`
- Auth: separate API key (`VOYAGE_API_KEY` env var), or can be configured per-profile

**Usage pattern**:
```
POST https://api.voyageai.com/v1/embeddings
{
  "model": "voyage-3-lite",
  "input": ["why are checkout failures spiking for pine-hollow?"],
  "input_type": "query"    // or "document" for stored lessons
}
→ { "data": [{ "embedding": [0.23, -0.11, 0.87, ...] }] }
```

**Batching**: Voyage supports batch embedding (up to 128 texts per call). Use this during `aida brain index` to embed all lessons efficiently. During normal queries, embed single questions on-the-fly (~100ms latency).

**Caching**: Store embeddings alongside lessons in brain.db. Only re-embed when content changes. During `aida brain index`, skip lessons that already have embeddings.

**Offline fallback**: If no Voyage API key is configured or network is unavailable, fall back to Jaccard similarity (current behavior). The brain still works - just with lower-quality similarity matching.

**Config**:
```yaml
# ~/.aida/config.yaml
brain:
  path: ~/.aida/brain
  remote: git@github.com:ryanlitalien/aida-brain.git
  auto_sync: true
  embeddings:
    provider: voyage          # only option for now
    model: voyage-3-lite
    api_key_env: VOYAGE_API_KEY
    dimensions: 512
```

### A.2 Vector Search Without sqlite-vec

Since sqlite-vec requires CGO/C extensions, vectors are stored as BLOBs and cosine similarity is computed in Go.

**Storage**: Vectors stored as `[]byte` in SQLite BLOB columns. Each `float32` is 4 bytes, so a 512-dim vector = 2KB. Encoding/decoding via `math.Float32frombits` / `math.Float32bits`.

**Search**: Load candidate vectors from SQLite, compute cosine similarity in Go:

```go
func cosineSimilarity(a, b []float32) float64 {
    var dot, normA, normB float64
    for i := range a {
        dot += float64(a[i]) * float64(b[i])
        normA += float64(a[i]) * float64(a[i])
        normB += float64(b[i]) * float64(b[i])
    }
    return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
```

**Performance**: For thousands of lessons (realistic volume), brute-force cosine similarity over 512-dim vectors takes <10ms. No index needed until you hit tens of thousands of lessons, which would take years of heavy use.

**Query pattern**: Load all lesson embeddings into memory on startup (one SQLite query), hold in a `[]LessonVector` slice. On each query, compute cosine similarity against the query embedding, return top-K. This is simpler and faster than any index for this scale.

### A.3 Integration Architecture

The brain integrates into the existing pipeline at four points. No restructuring needed - it hooks into `runQuery()` and the existing packages.

**New package**: `internal/brain/`

Responsibilities:
- Open/create brain.db (SQLite)
- Read: semantic search for similar lessons, entity lookup, load markdown pages
- Write: save lesson JSON file, insert into brain.db, append evidence to markdown
- Sync: git pull/push in background
- Index: rebuild brain.db from lesson files + markdown
- Compile: re-synthesize compiled truth from evidence

**Integration into the query pipeline (`internal/cli/query.go`)**:

```
runQuery() {
    // EXISTING: Load config, profile, sources, partners, LLM client

    // NEW - Brain startup
    //   Open brain.db (create if missing)
    //   Background git pull (non-blocking)
    //   If brain.db is stale → incremental re-index

    // EXISTING: Parse → Classify → Resolve

    // CHANGED - Brain read (replaces lessons.FindSimilar)
    //   Embed the question via Voyage AI
    //   Semantic search brain.db for K=5 similar lessons
    //   Entity lookup in brain.db → load matching brain pages
    //   Load knowledge/routing/compiled.md
    //   All of this becomes context for LLM router

    // EXISTING: Plan → LLM Route (now with richer brain context)
    // EXISTING: Execute

    // NEW - Verify (Phase 2, between Execute and Synthesize)
    //   Computational confidence scoring per result
    //   Optional inferential verification for ambiguous results
    //   Drop junk results, flag low-confidence ones

    // EXISTING: Synthesize → Print answer

    // CHANGED - Brain write (replaces lessons.Append)
    //   Write lesson JSON to brain/lessons/{profile}/{ts}-{hash}.json
    //   INSERT into brain.db with embedding
    //   Append evidence to relevant entity/tool brain pages
    //   Background: git add + commit + push (fire-and-forget subprocess)

    // EXISTING: Print timing, links
}
```

**What changes in existing packages**:
- `internal/lessons/` - `FindSimilar()` replaced by brain semantic search. `Append()` replaced by brain write. The package can be deprecated once brain is stable, or kept as a thin wrapper.
- `internal/engine/router.go` - `LLMRoute()` receives brain context (compiled routing wisdom + entity pages) in addition to past lessons. The prompt gets a new section with compiled knowledge.
- `internal/engine/synthesizer.go` - `scoreAnswer()` extended to return confidence + grounding + source_contribs (Phase 2).
- `internal/cli/query.go` - `runQuery()` calls brain read before routing and brain write after scoring. The flow is the same, just with brain hooks added.

**What doesn't change**: Parse, Classify, Resolve, Plan, Execute, the LLM client, the config system, the UI. The brain is additive - it enriches context and replaces the similarity search, but the pipeline shape is identical.

### A.4 Git Sync Lifecycle

**Problem**: `aida` is a CLI that exits after each query. Background goroutines die with the process. How does git push complete?

**Solution**: Fire-and-forget subprocess. The git push runs as a detached child process that outlives the parent:

```go
// After writing lesson file + brain.db:
cmd := exec.Command("git", "-C", brainPath, "add", "-A")
cmd.Run()  // fast, local, ~50ms

cmd = exec.Command("git", "-C", brainPath, "commit", "-m", 
    fmt.Sprintf("auto: %s %s", profile, timestamp))
cmd.Run()  // fast, local, ~100ms

// Push as detached process - parent can exit
pushCmd := exec.Command("git", "-C", brainPath, "push")
pushCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
pushCmd.Start()  // don't Wait() - process outlives parent
```

**Pull on startup**: At the beginning of `runQuery()`, do a non-blocking pull:

```go
go func() {
    exec.Command("git", "-C", brainPath, "pull", "--rebase").Run()
    // If new files arrived, mark brain.db as stale
}()
```

The pull runs concurrently with Parse (step 1). By the time the pipeline reaches the router (step 4.5), the pull has completed and any new lessons are available.

**Failure handling**:
- Push fails (offline, auth error): silent. Next invocation's pull+push will sync.
- Pull has conflicts: shouldn't happen with one-file-per-lesson. If it does, `--rebase` handles it.
- brain.db is stale after pull: re-index incrementally (only new files since last index timestamp).

### A.5 Brain Compile Prompt

`aida brain compile` reads raw evidence and rewrites compiled truth pages. This is the "dream cycle" - periodic synthesis that pre-computes routing wisdom.

**When it runs**:
- Explicitly: `aida brain compile`
- Auto-trigger: every 20 queries (configurable), checked at end of `runQuery()`
- The auto-trigger just prints "Brain compile available - run `aida brain compile` or pass --compile" (non-blocking suggestion, not forced)

**What it compiles** (priority order):

1. **`knowledge/routing/compiled.md`** - the highest-impact file. Aggregates all lessons into per-source routing guidance.

2. **`entities/tools/*.md`** - for each tool/source page, rewrite the compiled truth section (above the HR) from the evidence trail (below the HR).

3. **`routing_rules` table** - structured version of compiled.md, queryable from brain.db.

**Prompt template for routing compilation**:

```
You are compiling routing wisdom for an LLM-powered query orchestrator called Aida.
Below are lessons from past queries - each records which sources were used, whether they
worked, confidence scores, and any user feedback.

Analyze the patterns and write a routing guide in markdown. For each source, describe:
- What query types/strategies it's strong for (with confidence and sample count)
- What it's weak for or should be avoided for
- Any user feedback patterns (thumbs-down reasons, intended alternatives)

Be specific and evidence-based. Only include sources with >= 3 lessons.
Do NOT include raw evidence - this is the compiled summary.

## Lessons

{lessons_json}
```

**Prompt template for entity page compilation**:

```
You are updating a brain page for {entity_type} "{entity_name}" in a knowledge system.
Below is the current compiled truth (if any) and new evidence since the last compilation.

Rewrite ONLY the section above the horizontal rule with an updated summary that
incorporates the new evidence. Keep it concise - this will be loaded into an LLM
context window during queries. Focus on routing-relevant facts: what works, what
doesn't, known issues, key identifiers.

Do NOT modify anything below the horizontal rule (the evidence trail).

## Current Page

{current_page_content}

## New Evidence Since Last Compile

{new_evidence_lines}
```

### A.6 New CLI Commands

```
aida brain init              # Set up brain repo, clone or create remote
aida brain sync              # Manual git pull + push
aida brain index             # Rebuild brain.db from lesson files + markdown
aida brain compile           # Re-synthesize compiled truth from evidence
aida brain search "query"    # Semantic search against brain.db (debugging)
aida brain stats             # Show brain size, lesson count, last compile, sync status
aida brain serve             # Start MCP server (Phase 3)
aida brain gc                # Garbage collection (Phase 3)
```

### A.7 Dependency Summary

| Package | Purpose | Phase |
|---------|---------|-------|
| `modernc.org/sqlite` | Pure Go SQLite driver (no CGO) | 1 |
| Voyage AI API | Text embeddings (HTTP calls, no SDK needed) | 1 |
| `syscall` (stdlib) | Detached subprocess for git push | 1 |
| No new deps for vector search | Cosine similarity in ~10 lines of Go | 1 |
