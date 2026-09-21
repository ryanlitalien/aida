# Claude Code <-> Aida Memory Bridge - design

Status: draft / branch `design/claude-memory-bridge`
Created: 2026-07-13
Author: Ryan (with Claude)

> **Status update (2026-07-18): shipped.** Merged to main via PR #91 (merge commit
> `4ff31d5`). Capture (`aida brain capture-hook` / `aida brain remember`), identity
> projection (`aida brain project-soul`), and recall (`aida brain recall`, the
> `brain_recall` MCP tool, the `claude_memory_recall` Jarvis voice tool) are all
> live - see `claude-memory-bridge-capture.md` for the concrete wiring. One
> deviation from the architecture below: the bridge is one-directional mirroring
> (the capture hook copies Claude's memory into the brain) rather than the
> symlink/subtree of `~/.claude/memory` this doc originally proposed - that reverse
> projection, plus transcript-to-event consolidation, remain future work. See
> "Next steps" and "Open decisions" below for what's resolved.

Concept only. No tooling in this doc - it captures the architecture, the mapping,
and the decisions to settle before anyone writes code.

## Goal

Make Claude Code's memory (`~/.claude`) durable, learnable, and portable by adopting
**Aida's brain as the system of record** instead of building a second memory system.
Three outcomes:

1. **Don't lose anything that matters** - personality, preferences, distilled context,
   skills - even if the laptop dies or the folder is deleted.
2. **Learn from it** - feed raw Claude sessions into Aida's `consolidate` loop so the
   keepers become durable facts/instructions and the noise evaporates.
3. **Portability** - boot Claude onto a new machine as "me" with a quick sync-down, not
   a manual rebuild.

## Problem / why now

Audited 2026-07-13. `~/.claude` (413 MB) is **not backed up anywhere**: not a git repo,
not symlinked into a synced folder, no dotfiles repo, no iCloud, no Time Machine
destination configured. It exists only on one Mac's local disk. Meanwhile Aida already
has everything a durable memory needs and `~/.claude` is the one island that uses none
of it.

The irreplaceable, hand-authored part of `~/.claude` is tiny (~50 KB): `CLAUDE.md`,
`memory/`, `skills/`, `commands/`, `hooks/`, portable bits of `settings.json`, and
per-project `.claude/agents`. The other ~360 MB is regenerable cruft (session
transcripts, caches, image-cache, file-history, plugins).

## Key realization: adopt, don't build

`aida brain --help` describes the brain as "a git repo of markdown files indexed by
SQLite with vector embeddings for semantic search." It already provides:

- `aida brain sync` - pull + commit + push (git-based sync).
- `brain/memory/{instruction,fact,event}/` - a purpose-built memory tree, **currently
  empty**, i.e. a home waiting to be populated.
- `aida brain consolidate` - LLM-powered promotion of raw `event` records into durable
  `fact`/`instruction`.
- `aida brain index` - rebuild `brain.db` + embeddings from the markdown (proves the DB
  is a derived cache, not the source of truth).
- `aida brain garden` / `gc` - staleness auditing and garbage collection.
- `soul.yaml` - an authored persona layer ("Who you are").
- `~/.aida` is already git-backed with a sync remote on another machine.

So the task is not "build a brain." It is "bridge the Claude edge into the Aida core and
decide who is authoritative."

## Architecture: one store, two indexes

The clean target is not a sync *pipeline* between two stores. It is collapsing them into
**one store with two indexes over it**.

```
        Aida brain repo (git, remote-backed)  <- SYSTEM OF RECORD
        |__ memory/
             |__ instruction/   <- rules, personality, "how to work with me"
             |__ fact/          <- project + reference knowledge
             |__ event/         <- raw session takeaways (transient)
                    |
        +-----------+------------+
   brain.db + embeddings     ~/.claude/memory  (symlink / subtree of the brain repo)
   (Aida's semantic index)   MEMORY.md         (Claude's flat index)
        derived, rebuildable    derived, rebuildable
```

If `~/.claude/memory` becomes a **subtree of, or symlink into, the brain repo**, then:

- Claude reads/writes the *same files* Aida indexes. No transform layer, no divergence.
- "Sync" and "backup" are just `aida brain sync`. It already exists.
- `MEMORY.md` (Claude's index) and `brain.db` (Aida's index) are both *derived* and
  regenerable, so neither needs syncing. Each machine rebuilds its own.

The one genuine bridge problem is **frontmatter dialect**:

| Claude Code (`metadata.type`) | Aida brain (`memory/<type>`) |
|---|---|
| `user`, `feedback`           | `instruction`                |
| `project`, `reference`       | `fact`                       |
| (transient session context)  | `event`                      |

Reconcile these into one superset frontmatter schema both indexers can read. That mapping
is the whole tie-in.

## Three tiers of `~/.claude`

Sort the 413 MB into three buckets. Getting the third row right is what keeps the synced
brain safe to land on a fresh machine.

| Tier | What | Fate |
|---|---|---|
| **Identity** (~50 KB) | `CLAUDE.md`, `memory/`, `skills/`, `commands/`, `hooks/`, portable `settings.json`, project `.claude/agents` | Into the brain. This *is* "boot Claude as me." |
| **Cruft** (~360 MB) | `projects/` transcripts, `cache/`, `image-cache/`, `file-history/`, `plugins/`, `*-cache.json` | Disposable. Regenerates. Never synced. |
| **Secrets / machine-local** | auth tokens (OAuth refresh tokens, API keys), `settings.local.json`, absolute paths, derived ports | **Never** in the portable set. Keychain + a per-machine `.local` layer. |

Rule of thumb: **portability = identity + knowledge, never credentials.**

## The "learn from" loop

This is the part that is actually novel, and it answers "responses/context I don't want
to lose" without hoarding gigabytes.

- Claude session transcripts (~360 MB) are an **event stream**, not an archive.
- Feed the *takeaways* in as `event` records -> `aida brain consolidate` promotes the
  durable ones into `fact`/`instruction` -> **drop the raw transcript**.
- The keepers survive as distilled, searchable memory; the noise evaporates. You back up
  ~50 KB of wisdom, not 360 MB of chatter.
- Guardrail: consolidation is LLM-powered, so it needs a **review gate** (Aida already
  has `thumbs-up/down`, `note`, `garden`). Directly-given rules (like the feedback rules
  Ryan gives Claude) should enter as high-confidence `instruction`, not be inferred from
  events.

## Portability / new-machine bootstrap

A fresh laptop becomes "me" in roughly this sequence, all off primitives that already
exist:

1. Install Claude Code + the `aida` CLI.
2. `aida brain sync` - git-pull the brain (markdown only; no DB, no embeddings).
3. Materialize the identity tier: symlink/checkout `~/.claude/{CLAUDE.md,memory,skills,
   commands,hooks}` from the brain subtree; apply the machine-local `.local` layer.
4. `aida brain index` - rebuild `brain.db` + embeddings locally from the markdown.
5. Re-auth credentials per machine (keychain). These are deliberately not in the sync.

The heavy artifacts (index, embeddings) are derived, and the identity is tiny, so this is
a fast sync-down rather than a rebuild.

## Open decisions (settle before tooling)

1. **Direction of truth.** Recommend brain = system of record, `~/.claude` = projection +
   capture-inbox for newly authored memories. Two independent sources of record guarantee
   drift.
   **Resolved (shipped):** brain is the system of record; `~/.claude` is the capture-inbox
   + projection edge, exactly as recommended.
2. **Backup vs sync are different.** The existing remote is a *sync
   peer* - a delete-and-push deletes everywhere. Real backup needs a versioned, off-LAN
   copy. Mirror the `aida-wiki` pattern (`github.com/ryanlitalien/aida-wiki`): add a
   **private GitHub remote** for the brain so there is history plus an off-machine copy.
   **Resolved (shipped):** the brain repo's `origin` is a private GitHub repo
   (`git@github.com:ryanlitalien/aida-brain.git`), auto-committed and pushed. New-machine
   restore is clone + `aida brain index`.
3. **Two personas, one soul.** `soul.yaml` and `CLAUDE.md` are both "who you are." Pick one
   authored source (likely `soul.yaml` / brain `instruction` pages) and let the `CLAUDE.md`
   preference block be a *generated projection* of it, so personality is not maintained in
   two files that drift. (They already differ: `soul.yaml` centers a former employer's
   day-job identity; `CLAUDE.md` is Acme Widgets-scoped.)
   **Resolved (shipped):** `soul.yaml` is the authored source; `aida brain project-soul`
   splices a generated, managed block into `~/.claude/CLAUDE.md`.
4. **Skills duplication.** Aida has `library`/`skill`; Claude Code has `skills/`. Decide
   unify vs parallel-with-bridge before both grow.
   **Still open** - not addressed by the shipped work.
5. **Frontmatter superset.** Agree the one schema both indexers read. Everything above
   depends on this.
   **Not resolved as scoped** - no schema unification happened. Capture instead reads both
   dialects directly (top-level `type:` and nested `metadata.type:`), which turned out to be
   enough to unblock everything else. Revisit if that dual-read approach starts to strain.

## Net

Not building a brain - adopting Aida's as the substrate and making `~/.claude` a thin,
projectable edge of it. That yields durability, learning (`consolidate`), sync
(`brain sync`), and fast portability essentially for free, plus the off-machine GitHub
backup remote (now added).

## Next steps

- [x] Decide direction-of-truth (open decision 1). **Done** - brain is system of record,
  `~/.claude` is capture-inbox + projection.
- [ ] Define the superset frontmatter schema (open decision 5) - unblocks everything.
  **Not done as scoped** - capture reads both frontmatter dialects instead of unifying
  them; revisit if that stops being enough.
- [x] Add a private GitHub backup remote to the brain repo (open decision 2). **Done** -
  verified: `origin` is `git@github.com:ryanlitalien/aida-brain.git` (private), auto-
  committed and pushed.
- [x] Prototype the identity-tier manifest (the exact glob set that equals "me"). **Done**
  - the capture hook's path matching (`~/.claude/memory/*.md`, `~/.claude/CLAUDE.md`,
  `~/.claude/projects/*/memory/*.md`) is that manifest.
- [x] Tooling for projection + capture. **Done** - capture (`aida brain capture-hook` /
  `aida brain remember`) and identity projection (`aida brain project-soul`) both shipped.
  **Still future:** the reverse projection that materializes `~/.claude/memory` itself
  from the brain (symlink/subtree), and transcript-to-event ingestion.
