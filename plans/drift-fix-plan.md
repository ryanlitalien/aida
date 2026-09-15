# Aida documentation-drift fix plan

## Problem statement

`~/dev/aida/CLAUDE.md` has drifted significantly from the live `aida` CLI surface: 18 of 27 subcommands (`daily, session, vault, lessons, runs, replay, tune, seed, golden, note, thumbs-up, partners, profile, sources, library, skill, init, setup`) are not mentioned anywhere, internal contradictions exist (e.g. "exactly 3 LLM calls" vs. "LLM router (4th LLM call)"), and at least one stale claim (`tasks_list` filters) was caught only when authoring this plan. This matters because `~/.aida/library/sources/aida.yaml` declares `context: CLAUDE.md`, so when `aida` is asked about itself it loads only this doc - never the Go source. The drift bug surfaced today: `aida` could not answer "what task statuses does `aida tasks` support?" even though commit `b1dc5d3` shipped `in-progress / hold / deferred / closed` weeks ago. Recurrence risk is high: CLAUDE.md was last updated three commits ago (`a055a60`) while the repo has 159 total commits and ships features regularly, with no automation to keep the two in sync.

## Phase 1 - One-time CLAUDE.md catch-up

**Goal:** restore CLAUDE.md as an accurate snapshot of the live CLI / MCP surface and fix internal contradictions, so the next phase's drift-prevention has a known-good baseline.

**Steps** (line numbers refer to `~/dev/aida/CLAUDE.md` at HEAD plus the unstaged `tasks_list` patch already in the working tree):

1. **Add a complete subcommand inventory section.** The current "Useful subcommands beyond `aida <query>`" block (lines 31–40) lists only `--explain`, `--dry-run`, `lint`, `index --generate`. Rename the section to "Top-line CLI flags" and insert a new section **"Subcommand inventory"** immediately after it that documents all 27 subcommands grouped by purpose. Suggested grouping (verified against `aida --help`):
   - *Query / interaction*: `aida <query>`, `session`, `daily`
   - *Brain & tasks*: `brain`, `tasks`, `lessons`, `note`, `thumbs-up`, `thumbs-down`
   - *Library & routing*: `library`, `sources`, `partners`, `skill`, `lint`, `index`
   - *Recording / replay*: `runs`, `replay`, `tune`, `golden`, `seed`
   - *MCP / setup / profile*: `serve`, `setup`, `init`, `profile`, `vault`
   - One-line description per command, copied/condensed from `aida <cmd> --help` short blurb. Put this section between "Build & Development Commands" (ends ~line 29) and "MCP server surface" (currently line 42).
2. **Resolve the LLM-call-count contradiction.** Line 90 says "Each query makes exactly 3 LLM calls". Line 202 (Routing contract step 6) says "LLM router (4th LLM call)". The router is real (cli/query.go `flattenCandidates` → LLM router) and runs before execution. Fix line 90 to read "Each query makes 3–4 LLM calls (router + parse + execute + synthesize, with the router skipped on single-candidate routing)" or similar - verify the exact count by reading `internal/cli/query.go` and the engine entry point. Update the ASCII diagram to add a `[0. ROUTE]` step or annotate that step 4 (PLAN) is followed by an LLM router before EXECUTE.
3. **Demote / rename the misleading "Useful subcommands" subsection** as part of step 1 - its current name implies completeness. Either rename to "Common flags & one-liners" or fold it into the new inventory.
4. **Verify and fix the "Notion via Claude CLI" claim** at line 177. `internal/sources/notion.go` still uses `claude --print --allowedTools "mcp__notion__..."` (verified), so the claim is still accurate for the standard query path. **However**, agent-mode (commit `c96e387`) added an in-process MCP client (`internal/llm/tools.go`, `internal/engine/agent_loop.go`). Update the bullet to: *"Notion via Claude CLI (default) or in-process MCP client (`--agent` mode). The default executor shells out to `claude --print` to reuse Notion MCP. Agent mode talks MCP directly - see `internal/llm/tools.go`."*
5. **Reconcile `aida-implementation-plan.md` reference** at line 9. The doc is called the "authoritative design document" but is 1001 lines long and predates Phase 6+. Either (a) demote the reference to "historical design doc - see `docs/` for current per-feature plans" or (b) replace with a pointer to the most recent shipped-feature plans in `docs/` (e.g., `docs/implementation-plan-agent-mode.md`, `docs/gap-analysis-2026-04.md`). (Superseded by the open-source scrub: the file was deleted outright and CLAUDE.md now points at `docs/plan-open-source.md` Part 2 and `HISTORY.md` instead.)
6. **Stage the working-tree `tasks_list` fix** that's already present (`git diff CLAUDE.md`) along with these edits in the same commit.

**Acceptance criteria:**
- `aida --help` and the new "Subcommand inventory" section have the same set of 27 names (diff verifiable with a small shell pipeline).
- No occurrence of "exactly 3 LLM calls" anywhere in CLAUDE.md unless it is followed by a clarifying note.
- `grep -n "claude --print" internal/sources/notion.go` still returns a hit (confirms the claim being reworded is still grounded).
- File still under ~300 lines (currently 215; target keeps it skimmable).

**Effort:** 60–90 minutes (most of the work is writing the one-line subcommand blurbs).

## Phase 2 - Drift prevention

**Goal:** make the inventory section in CLAUDE.md self-healing so the next shipped subcommand cannot silently drift, and ensure aida-the-binary can introspect its own current state.

**Three options, with tradeoffs:**

### Option A - Generated subcommand inventory via `make docs` + markers

- Add `scripts/gen-subcommand-inventory.sh` that runs `bin/aida --help` (and optionally `bin/aida <cmd> --help` per subcommand for one-line descriptions), then formats a markdown table.
- Add markers in CLAUDE.md:
  ```
  <!-- HM_SUBCOMMANDS_BEGIN -->
  ...generated table...
  <!-- HM_SUBCOMMANDS_END -->
  ```
- Add a `make docs` target that builds the binary, runs the script, and rewrites the section between markers. Optionally add `make docs-check` (used in CI / pre-commit) that runs the same logic and `git diff --exit-code CLAUDE.md`.
- **Pros:** deterministic, fast, no extra LLM calls per query, surfaces drift as a CI red light.
- **Cons:** only catches subcommand-name drift - not behaviour drift, not flag drift inside a subcommand, and not stale prose claims like the LLM-call count.

### Option B - Broaden the `aida` source `context:` so the LLM sees current code

- Edit `~/.aida/library/sources/aida.yaml` to replace `context: CLAUDE.md` with one of:
  - A list (if the loader supports it): `context: [CLAUDE.md, README.md, docs/gap-analysis-2026-04.md]`.
  - A generated `library/layers/sources/aida.md` (the layer file already exists at `~/.aida/library/layers/sources/aida.md` - repurpose it) that concatenates CLAUDE.md, the most recent `docs/*.md` plans, and `aida --help` output.
  - A `search:` block (Phase 1.1 grep mode) so questions about aida do live grep over `*.go` headers / `cmd/aida/*` instead of loading prose.
- **Pros:** addresses the root cause directly - even if CLAUDE.md drifts, the LLM has the source to fall back on. No CI gate needed.
- **Cons:** larger token bill per aida-routed query; needs a quick check that the loader handles a list-typed `context` (verify `internal/library/sources.go`); may surface lower-quality answers if the additional context is noisy.

### Option C - Pre-commit / CI drift check

- Add a Git hook or CI job (`.github/workflows/docs-drift.yml`) that builds `aida`, runs `aida --help`, extracts subcommand names, greps CLAUDE.md for each, and fails on missing names.
- **Pros:** stops drift at the boundary.
- **Cons:** annoying friction on every commit if not scoped tightly; only catches the subcommand-name dimension; same scope limit as Option A.

### Recommendation: **Option A as primary, Option B as a follow-up.**

Option A is one small shell script + a Makefile target + 4 lines of markers, and it converts subcommand drift from "manual diligence" to "build break". Option B is the structural fix for the original bug (aida not knowing its own features) - do it in the same week but ship it separately because it requires verifying the loader's list-typed context behaviour. Option C overlaps Option A and is worth adding only if `make docs-check` proves insufficient in practice.

**Concrete steps (Option A):**

1. Add `scripts/gen-subcommand-inventory.sh` that emits a markdown table from `aida --help`. Group by the same categories defined in Phase 1 step 1 (encode the grouping in the script via a small awk/case map).
2. Insert `<!-- HM_SUBCOMMANDS_BEGIN -->` / `<!-- HM_SUBCOMMANDS_END -->` markers into the inventory section landed in Phase 1.
3. Add `Makefile` target:
   ```
   docs: build
   \t./scripts/gen-subcommand-inventory.sh > /tmp/aida-inv.md && \
   \tpython3 scripts/replace-between-markers.py CLAUDE.md HM_SUBCOMMANDS /tmp/aida-inv.md
   docs-check: docs
   \tgit diff --exit-code CLAUDE.md
   ```
4. Document the workflow in CLAUDE.md "Build & Development Commands" section: "Run `make docs` after adding or renaming a subcommand."
5. (Optional) Add a `pre-push` Git hook in `scripts/hooks/` that runs `make docs-check`.

**Concrete steps (Option B, as follow-up):**

1. Read `internal/library/sources.go` to confirm whether `context:` accepts a list. If not, file a small task to extend the loader.
2. Update `~/.aida/library/sources/aida.yaml` `context:` to include `CLAUDE.md`, `README.md`, and the latest `docs/*plan*.md`. Keep the list short to control token usage.
3. Verify with the verification commands below.

**Acceptance criteria:**
- `make docs` is idempotent on a clean tree (running it twice produces no diff).
- `make docs-check` returns non-zero after a hypothetical new subcommand is added without running `make docs`.
- (Option B) `aida "what subcommands does aida have?"` returns the full list rather than just the 9 historically documented ones.

**Effort:** Option A: 1–2 hours including the Python helper. Option B: 2–4 hours depending on loader changes.

## Phase 3 (optional) - Split CLAUDE.md into per-feature layers

**Goal:** reduce CLAUDE.md to a thin index and move feature-specific docs next to their owning code or into per-feature layer files, so the doc surface scales with the code without reintroducing drift in a single 215-line file.

**Why it's worth considering:** `~/.aida/library/layers/sources/aida.md` already exists as a designated layer doc location. The layer system was built for exactly this - pulling structured context into the LLM only when relevant. Today CLAUDE.md is doing two jobs (build instructions + feature reference) and it's losing at the second.

**Sketch:**

- Keep `CLAUDE.md` as: project overview, build commands, top-line architecture diagram, generated subcommand inventory, pointers to layer docs.
- Move feature-specific sections to layer files:
  - "MCP server surface" → `internal/mcp/MCP.md` (read by both CLAUDE.md and `library/layers/sources/aida.md`).
  - "Task CLI surface" + "Six task statuses" → `internal/brain/TASKS.md`.
  - "Routing contract" + "Feedback loop" → `internal/engine/ROUTING.md`.
- Update `~/.aida/library/sources/aida.yaml` `context:` to list those files, so aida self-queries can pull the right doc by topic.
- Drift prevention from Phase 2 still applies to each split file (one marker block per feature in its own doc).

**Decision criteria:** ship Phase 3 only if Phase 2 Option B isn't enough - i.e., loading more context still gives mediocre answers because the prose is too dense. Skip it if Option A + Option B fix the felt pain.

**Effort:** 3–4 hours including updating the layer file references and verifying layer activation routes.

## Out of scope / explicit non-goals

- **Not** auditing every claim in CLAUDE.md against the source (e.g., the `errgroup` semaphore limit of 5, `partners.yaml` schema details). Stop at structural drift + the contradictions called out above.
- **Not** rewriting `aida-implementation-plan.md`. Reference fixes only - it remained a historical artifact until the open-source scrub deleted it outright (it was the former-employer-era design doc).
- **Not** building a generic doc-generation framework. The scripts in Phase 2 are deliberately scoped to subcommand inventory.
- **Not** changing the `aida.yaml` `context:` field as part of Phase 1 - that's Phase 2 Option B specifically, with its own verification.
- **Not** documenting flag-level details (`--explain`, `--dry-run`, etc.) for every subcommand. The inventory is one line per command; deep details stay in `aida <cmd> --help`.

## Verification commands

Run these after each phase to confirm completion.

**After Phase 1:**

```bash
# Subcommand-name parity
diff <(aida --help | awk '/^Available Commands:/,/^Flags:/' | awk 'NF==2 {print $1}' | sort -u) \
     <(awk '/<!-- HM_SUBCOMMANDS_BEGIN -->/,/<!-- HM_SUBCOMMANDS_END -->/' \
         ~/dev/aida/CLAUDE.md \
       | grep -oE '`aida [a-z-]+`' | awk '{print $2}' | tr -d '`' | sort -u)
# (Markers won't exist yet in Phase 1 - run a manual grep instead)

grep -n "exactly 3 LLM calls" ~/dev/aida/CLAUDE.md   # expect: no match
grep -n "claude --print" ~/dev/aida/internal/sources/notion.go   # expect: still present
```

**After Phase 2 (Option A):**

```bash
cd ~/dev/aida && make docs && git diff --exit-code CLAUDE.md   # expect: clean
make docs-check                                                                      # expect: exit 0
```

**After Phase 2 (Option B):**

```bash
aida "what task statuses does aida tasks support now?"
# Expect the answer to include all six: open, in-progress, hold, deferred, done, closed.
# Currently this query returns only open + done (the bug that triggered this work).

aida "list every aida subcommand"
# Expect ~27 names rather than ~9.
```

**After Phase 3 (if shipped):**

```bash
aida "where do I find docs on the MCP task surface?"
# Expect a pointer to internal/mcp/MCP.md (or wherever the split lands).

# Layer activation - confirm aida.yaml context list resolves to all layer files
aida sources --explain | grep -A3 aida
```
