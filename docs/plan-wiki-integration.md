# Plan: wiki integration (consolidation, decay scoring, wiki commands)

**Status**: shipped. All three phases landed - `aida brain consolidate` (`internal/brain/consolidate.go`), decay + use-count scoring (`internal/brain/search_multi.go`), and the `aida wiki` command group with its recall channel (`internal/cli/wiki.go`, `internal/brain/wiki_index.go`). Kept as a record of the design and its sources.

## Context

A personal archive folded into an LLM-maintained wiki (an OKF v0.1 bundle, located by `wiki.path`, default `~/dev/aida-wiki`) with aida on top. This plan covers the Go side - three phases distilled from a review of Elastic Atlas (consolidation/decay/supersession), the Obsidian Karpathy-wiki plugin (lint taxonomy, aliases), and Google OKF (format). The wiki-side conventions and triage/synthesis workflows live in the wiki repo itself, not here.

Design constraints honored: files are the source of truth, brain.db is derived; LLM at edges, deterministic middle; granular commits, never squash.

## Phase A - Typed-memory consolidation (`aida brain consolidate`)

Promote accumulated `event` records (46 today, mostly Jarvis voice turns) into durable `fact`/`instruction` records with provenance - the Atlas episodic→semantic/procedural loop, cloned from the existing compile pattern.

**New files**: `internal/brain/consolidate.go`, `internal/cli/brain_consolidate.go`

1. **Schema**: add `Provenance []string` and `Confidence float64` to `MemoryRecord` (memory_types.go:66-77); persist in JSON files and `memory_records` via idempotent `ALTER TABLE ... ADD COLUMN` (migration pattern: db.go:178).
2. **`(*DB).SupersedeByID(oldID, newID string) error`** mirroring `SupersedePriorMemory` (memory_types.go:224): set `superseded_by`, drop from FTS corpus. Enables contradiction-driven supersession of arbitrary records (today only same-`(type,key,profile)` supersedes).
3. **`(*Brain).Consolidate(ctx, client *llm.Client) error`**:
   - Load active events: `ListMemory({Types: [MemoryEvent]})` (memory_types.go:298), newest N since last run.
   - Load existing active facts/instructions (dedup context, Atlas "do not duplicate" instruction).
   - One `client.CompleteJSON` call (llm/client.go:104) with schema:
     ```json
     {"facts":[{"body":"","key":"","supporting_event_ids":[]}],
      "instructions":[{"body":"","key":"","supporting_event_ids":[]}],
      "supersedes":[{"old_id":"","new_body":"","new_key":"","contradiction":"harsh|natural"}]}
     ```
   - Write each via `WriteMemory` (memory_types.go:112) - embedding + by-key supersession come free; set `Provenance` from `supporting_event_ids`; `Confidence` 1.0, or penalized (0.7) for `harsh` contradictions (Atlas rule). For `supersedes` entries, call `SupersedeByID` after the new write.
   - Require non-empty `supporting_event_ids` per output; drop anything without provenance (Atlas guard).
4. **Trigger state**: `meta/last_consolidate.json` mirroring `autoCompileMeta` / `ShouldAutoCompile` / `MarkCompiled` (brain.go:424-467), threshold = 15 new events.
5. **Auto-trigger**: goroutine after queries beside the autocompile driver (cli/query.go:800-815), same offline guard. Manual: `aida brain consolidate [--dry-run]` (dry-run prints the proposed facts/instructions without writing).
6. **Tests**: fixture events → assert facts written with provenance, superseded chains intact, dry-run writes nothing; extend memory_types_test.go patterns.

## Phase B - Decay + use-count recall scoring

Recency/frequency signals in multi-channel recall (Atlas pattern).

**Files**: `internal/brain/db.go`, `internal/brain/search_multi.go`

1. **Migration**: `last_used_at TEXT`, `use_count INTEGER NOT NULL DEFAULT 0` on `memory_records` (idempotent ALTER, db.go:178 pattern).
2. **Bump on recall**: after hydration in `SearchMulti` (search_multi.go:160-162), one batched `UPDATE memory_records SET last_used_at=?, use_count=use_count+1 WHERE id IN (...)` for returned `memory:` docs. Retrieval practice: recalling a fact keeps it fresh (relevance decay, not truth decay - truth is supersession's job).
3. **Scoring**: multiply `r.Score` post-hydration (`hydrateMultiResult` at :195 populates DocType; extend it to carry `created`/`last_used_at`/`use_count`):
   - `decay = gauss(age(last_used_at ?? created); offset=180d, scale=1825d, floor at decay=0.5)`
   - `boost = 1 + log10(1+use_count) * 0.2`
   - `r.Score *= decay * boost`
   - **Instructions exempt from decay** (recency ≠ effectiveness for standing rules - Atlas exempts procedural for the same reason). Lessons decay on `lessons.timestamp` (db.go:49-70), no bump (lesson recall isn't tracked yet).
4. **Constants** beside `rrfConst` (search_multi.go:38): `decayOffsetDays=180`, `decayScaleDays=1825`, `useCountWeight=0.2`.
5. **Tests**: table tests in search_multi_test.go - fresh vs 3-year-stale vs frequently-recalled ordering; instruction exemption.

## Phase C - `aida wiki` command group + wiki recall channel

**New files**: `internal/cli/wiki.go`, `internal/cli/wiki_lint.go`, `internal/brain/wiki_index.go`

1. **Config**: `Wiki WikiConfig` on `Config` (config/config.go:19-26), `WikiConfig{Path string}` mirroring `BrainConfig` (:37), `WikiPath()` resolver mirroring `BrainPath()` (:79). Default `~/dev/aida-wiki`.
2. **Command group**: `newWikiCmd()` in wiki.go, registered in root.go:121-148 beside `newBrainCmd()` (:137); subcommand-per-file convention like brain (brain.go:16, brain_garden.go).
3. **`aida wiki lint`**: clone the garden shape - pure `auditWiki(...)` returning `[]gardenFinding`-style records (brain_garden.go:53-57, :187), findings grouped by category, exit 0 (report, not gate; `--strict` for non-zero like cli/lint.go). Six categories over `wiki/` + kept source notes:
   - `dead-link` - markdown/`[[...]]` targets that don't resolve (casefold-aware)
   - `orphan-page` - no inbound links and absent from index.md
   - `empty-page` - body under threshold
   - `missing-aliases` - pages without ≥1 alias
   - `dup-slug` - **casefold-duplicate slugs** (APFS lesson: a case-insensitive filesystem silently collides two slugs)
   - `frontmatter` - missing `type`/`title`, invalid `wiki_status`, synthesized pages with no `sources` citations
4. **`aida wiki index`**: walk `wiki/*.md` + `triaged_keep` source notes → `EmbedDocuments` (embeddings.go:57, batches at 128) → new `wiki_pages` table (slug, title, path, body, embedding BLOB, indexed_at) + FTS rows (`upsertCorpusFTS("wiki:<slug>", "wiki:page", body)` per memory_types.go:210-214 pattern). Incremental: re-embed only files whose mtime > indexed_at.
5. **5th recall channel**: `ChannelWiki` in `SearchMulti` - vector + FTS hits over `wiki:` docs added to `rankings` before `rrfFuse` (:153). The file header (:16-20) documents this exact extension point ("no other call sites change"). `hydrateMultiResult` (:195) learns the `wiki:` prefix → agent's RELEVANT MEMORY block (cli/agent.go:394-408) starts surfacing wiki pages with zero prompt changes.
6. **Later** (explicitly out of scope): `aida wiki ingest` via a new `IngestSource` adapter (adapters/adapter.go:25 - the File/Stdin/Notion pattern; package doc states new origins are adapter-only changes).

## Ordering & sizing

- A and B both touch the `memory_records` migration → land the migration as its own first commit, then A and B independently on top.
- C is independent of A/B (depends only on a wiki repo existing).
- Rough sizes: A ≈ 400 LOC + tests · B ≈ 150 · C ≈ 500.

## Verification

- `make test` green after each phase; new table tests as listed per phase.
- A: seed events with `aida` voice/agent use → `aida brain consolidate --dry-run` proposes sane facts; real run → `aida brain stats` shows fact/instruction counts > 0; superseded records keep files but leave FTS.
- B: `aida "question that hits typed memory"` twice → second run's recalled fact has bumped `use_count`; stale unrecalled facts rank below fresh ones in `SearchMulti` output.
- C: `aida wiki lint` on the scaffolded repo reports cleanly; seed one wiki page, `aida wiki index`, then an `aida --agent` query about that topic surfaces the page in RELEVANT MEMORY.
