# Claude Code <-> Aida memory bridge - capture + recall hooks (prototype)

Status: sketch / branch `design/claude-memory-bridge`
Companion to `claude-memory-bridge-design.md` (the concept). This doc is the
concrete wiring: the two hooks that make the bridge live, plus the two thin CLI
verbs they depend on.

> **Status update (2026-07-18): shipped.** Merged to main via PR #91 (merge commit
> `4ff31d5`). Both directions below are live: capture via `aida brain capture-hook`
> (stdin) + `aida brain remember --file`, installed by `aida setup` as a detached
> `PostToolUse(Write|Edit)` hook (174 memories backfilled); recall via
> `aida brain recall` (CLI), the `brain_recall` MCP tool, and the
> `claude_memory_recall` Jarvis voice tool. This doc's "recall path 2" shipped as a
> new `brain_recall` tool (not a modified `brain_search`) - see the corrections in
> "Recall path 2", "Scope filtering", "What it takes to go live", and "Open
> questions" below for what changed and what's now done.

## The two directions

```
  Claude WRITES a memory  ── PostToolUse(Write|Edit) ─▶  aida brain remember   (CAPTURE, detached)
  Claude needs context    ── static soul projection + warm mcp__aida__brain_search  (RECALL)
```

- **Capture** mirrors every hand-authored Claude memory into Aida's brain as a
  durable, embedded, git-synced record - scoped correctly (global vs project).
  It runs **detached**, so its latency never touches the turn.
- **Recall** is deliberately **not** a per-prompt or per-session hook (see the
  latency section - a cold `aida` spawn costs ~430-830 ms, far too much to pay
  every turn). Instead it splits into two zero/low-cost paths: a **static soul
  projection** loaded free at session start (identity: kids, projects, people),
  and the **already-warm `mcp__aida__brain_search`** tool the model calls
  on-demand when it touches a memory.

## Ground truth this sketch is built on (verified 2026-07-17)

- Claude memories are **project-scoped**: this repo's live at
  `~/.claude/projects/-Users-ryan-dev-aida/memory/*.md`. A **global**
  `~/.claude/memory/` also exists separately. Both matter; `projects/` is not
  all-cruft.
- Memory writes go through the **Write/Edit tools**, so **PostToolUse** (and
  PreToolUse) fire. There is no memory-specific hook event. Matchers filter by
  tool name only, so path filtering happens **inside the script**.
- Aida's typed memory is `brain/memory/{fact,event,instruction}/<id>.json`
  (JSON, not markdown). `brain.WriteMemory()` embeds + supersedes-by-key +
  inserts into `brain.db`. **No CLI exposes it yet** - this sketch adds one.
- Recall needs its own path. `aida brain search` / MCP `brain_search` call
  `Brain.Search`, which returns lessons/entities/routing/tasks and **does not
  read memory records**. Only `SearchMulti` (the agent path) covers
  `memory:fact/event/instruction`, and even there retrieval is FTS/substring/
  exact-key - **nothing reads `memory_records.body_embedding`**, and
  `Brain.Index` does not reindex the memory JSON. Durable, semantic recall
  therefore requires two fixes (a memory reindex in Index; a memory vector
  channel) before it works end to end (**both shipped** - see the status banner
  above). **`soul.yaml` is not indexed either** -
  identity recall reaches it via a separate projection. See the implementation
  plan for the corrected, phased design.

## Scope model: global vs project (the part that has to be right)

Every captured record carries its origin scope so recall can filter and
supersession stays contained. Scope is derived from the write path + hook `cwd`,
then stored across three existing `MemoryRecord` fields (no schema change):

| Claude source path | Scope | `Key` (supersession namespace) | `Tags` | `Source` |
|---|---|---|---|---|
| `~/.claude/memory/foo.md` | `global` | `claude:global:foo` | `claude-code`, `scope:global` | `claude-code:~/.claude/memory/foo.md` |
| `~/.claude/CLAUDE.md` | `global` | `claude:global:CLAUDE` | `claude-code`, `scope:global` | `claude-code:~/.claude/CLAUDE.md` |
| `.../projects/<slug>/memory/bar.md` | `project:<proj>` | `claude:project:<proj>:bar` | `claude-code`, `scope:project`, `project:<proj>` | `claude-code:<abs path>` |
| project-local `./CLAUDE.md` | `project:<proj>` | `claude:project:<proj>:CLAUDE` | `claude-code`, `scope:project`, `project:<proj>` | `claude-code:<abs path>` |

Decisions baked in:

- **`<proj>` = git-toplevel basename** of the hook `cwd` (falls back to `cwd`
  basename), e.g. `aida`. Stable and human-readable; the exact path is preserved
  in `Source` for provenance.
- **Scope lives in `Key` + `Tags`, not `Profile`.** `Profile` (work/personal) is
  an orthogonal Aida axis - left at its default. Keying by scope means a global
  instruction supersedes only globals, a project fact only within its project.
  No cross-project clobbering.
- **`MEMORY.md` is skipped.** It is a derived index (Claude's, like `brain.db`
  is Aida's). Ingesting it would duplicate every entry as one blob.

### Type mapping (frontmatter -> brain type)

Read `metadata.type` from the memory file's frontmatter and map per the design
doc, confirmed against `memory_types.go`:

| Claude `metadata.type` | Aida brain type | Semantics |
|---|---|---|
| `user`, `feedback` | `instruction` | supersede-by-key, confidence 1.0 |
| `project`, `reference` | `fact` | supersede-by-key, confidence 1.0 |
| (bare `CLAUDE.md`, no frontmatter) | `fact` | keyed on `CLAUDE` |
| (transcript takeaway - separate pipeline) | `event` | append-only, feeds `consolidate` |

Hand-authored memories enter as **high-confidence instruction/fact directly** -
never as events needing consolidation. This is the design doc's rule: "directly
given rules enter as instruction, not inferred from events." Events are reserved
for the later transcript->event->consolidate loop, which is out of scope here.

## Capture hook (write side)

### `~/.claude/hooks/capture-memory.sh`

Thin, fire-and-forget, never blocks the turn, never fails the tool. It only
decides *what* and *scope*; all real work (parse, embed, supersede, index) is in
`aida brain remember`, run detached.

```bash
#!/usr/bin/env bash
# PostToolUse(Write|Edit): mirror a Claude memory write into Aida's brain.
set -euo pipefail
input=$(cat)

# Write/Edit both carry a file_path; anything else is not a file write.
file=$(printf '%s' "$input" | jq -r '.tool_input.file_path // empty')
[ -n "$file" ] || exit 0

# Classify by path. MEMORY.md is a derived index - never ingest it.
case "$file" in
  */MEMORY.md)                        exit 0 ;;
  */.claude/memory/*.md)              scope="global" ;;
  */.claude/projects/*/memory/*.md)   scope="project" ;;
  */CLAUDE.md)                        scope="detect" ;;   # global vs project by path
  *)                                  exit 0 ;;
esac

# Resolve a stable project key from cwd (git toplevel basename).
cwd=$(printf '%s' "$input" | jq -r '.cwd // empty')
proj=$(cd "$cwd" 2>/dev/null && { git rev-parse --show-toplevel 2>/dev/null \
        | xargs -r basename || basename "$cwd"; })

# A CLAUDE.md under $HOME/.claude is global; anywhere else is project-local.
if [ "$scope" = "detect" ]; then
  case "$file" in "$HOME"/.claude/CLAUDE.md) scope="global" ;; *) scope="project" ;; esac
fi

scope_arg="$scope"
[ "$scope" = "project" ] && scope_arg="project:${proj:-unknown}"

mkdir -p "$HOME/.aida/logs"
# Detached: WriteMemory embeds (Voyage) + supersedes + indexes; do NOT do it inline.
nohup aida brain remember \
        --file "$file" \
        --scope "$scope_arg" \
        --source "claude-code" \
        >> "$HOME/.aida/logs/claude-capture.log" 2>&1 &
exit 0
```

### settings.json wiring (add alongside the existing hooks)

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Write|Edit",
        "hooks": [
          { "type": "command", "command": "$HOME/.claude/hooks/capture-memory.sh" }
        ]
      }
    ]
  }
}
```

(There is already a `PostToolUse` `matcher:"*"` third-party notify hook; this is an
additional entry, not a replacement. Hooks with different matchers coexist.)

### New CLI verb: `aida brain remember`

The one piece of Go this needs. It is a thin wrapper over the existing
`brain.WriteMemory`.

```
aida brain remember --file <path>
                    [--scope global|project:<key>]   # default: global
                    [--type fact|instruction|event]  # default: from frontmatter
                    [--key <k>] [--source <s>]
                    [--dry-run]
```

Behavior:
1. Read `--file`. Split YAML frontmatter from body (whole file if no frontmatter).
2. Pick brain type: `--type` if given, else map `metadata.type` (table above),
   else `fact`.
3. Derive `Key` = `claude:<scope>:<basename-without-ext>` unless `--key` given.
4. Build the record:
   `Tags = ["claude-code", "scope:<...>", ("project:<key>")]`,
   `Source = "<--source>:<abs path>"`, `Confidence = 1.0`, `Body = <body>`.
5. Call `brain.WriteMemory(ctx, rec)` - it embeds, supersedes prior same-key
   records, writes the JSON mirror, and inserts the `brain.db` row.
6. `--dry-run` prints the record it would write and exits.

Because instruction/fact supersede by key, re-editing the same memory file just
supersedes the prior version - no duplicate accumulation, no dedup logic needed.

## Latency: why recall is NOT a per-prompt/per-session hook

Measured on this machine (2026-07-17), cold `aida` binary spawn:

| Operation | Cost |
|---|---|
| Cold binary + open `brain.db` (10 MB + 4 MB WAL), **no search** | **~430 ms** (floor) |
| `brain search` **with** Voyage embedding (network) | **~620-830 ms** |

The ~430 ms is a **floor** - just spawning the process and opening the DB, before
any query. An FTS-only "fast" path can shave the embed (~200-400 ms) but never
gets under the floor while it cold-starts a fresh binary. So any hook that fires
per prompt (`UserPromptSubmit`) or per session (`SessionStart`) would add
~0.4-0.8 s to *every* turn/session. Rejected.

Two facts change the calculus:

- **The `aida` MCP server is already warm.** A Claude Code session holds a
  persistent stdio connection to `aida serve` (MCP mode). `mcp__aida__brain_search`
  / `brain_get_page` / `brain_stats` reuse that process's already-open `brain.db`,
  so they cost ~tens of ms - no cold-start. (Confirmed: 5 `aida serve` MCP
  processes running; the HTTP daemon on :1610 is a separate, optional mode.)
- **Identity is static, not query-shaped.** "Who are my kids / what am I working
  on" wants to be *always present*, which is the opposite of a sometimes-firing
  hook.

So recall becomes two low/zero-cost paths, neither a per-turn hook.

### Recall path 1 - static soul projection (identity, zero latency)

Project `soul.yaml` into the always-loaded `CLAUDE.md` (global) as a small
generated block: `name`, `family.kids`, active projects parsed from `context`,
the `people:` roster. It loads for free at session start (no process, no
network), so Claude always knows the identity facts. Regenerate it on
`aida brain sync` or a cron - not per session.

This is design open-decision 3 ("two personas, one soul") resolved in the
capture direction: one authored source (`soul.yaml`), `CLAUDE.md`'s identity
block a generated projection of it. Even cleaner long-term: index soul into the
brain as `fact` records (one per `family.kids`, `people:<name>`, project) so it
also flows through `brain_search`; then the CLAUDE.md block is just a rendering.

### Recall path 2 - warm MCP, on-demand (dynamic, ~tens of ms)

**Shipped, as a new tool rather than a modified `brain_search`.** The proposal
below was a CLAUDE.md instruction pointing at `mcp__aida__brain_search`; what
actually shipped is a dedicated `mcp__aida__brain_recall` tool (plus
`aida brain recall` on the CLI and `claude_memory_recall` for Jarvis voice), and
`~/.claude/CLAUDE.md` now carries a line telling Claude Code to call
`mcp__aida__brain_recall` for context about a person/project/preference.
`brain_search` itself was left untouched. Original text kept below for the
reasoning that motivated it:

No hook. A CLAUDE.md instruction: *"When you read or edit a memory, or need
context about a person/project/preference, call `mcp__aida__brain_search`
(scope-filter to global + the current project)."* It fires exactly when useful,
on the warm connection, at ~tens of ms. Zero new infrastructure - `brain_search`
already exists and is already connected.

Optional automatic variant, if you want it deterministic rather than
model-driven: a `PostToolUse(Read)` hook matched to memory paths **only** (fires
a handful of times per session, and only after Claude already paid Read latency).
To keep even that off the cold-start floor, add a tiny `GET /brain/recall` route
to the HTTP daemon and hit it with `curl` instead of spawning the binary. **Still
deferred** - path 1 + path 2 have proven sufficient so far.

### Scope filtering (needed for path 2)

**Shipped**, in `FindSimilarMemories` (`internal/brain/memory_recall.go`) rather
than as a `brain_search` change: `memoryScopeMatch` filters to global OR
`project:<slug>` *before* the top-k cut (a project scope includes globals), and
`aida brain recall` / `brain_recall` both accept an explicit `--scope`. Original
proposal kept below:

`mcp__aida__brain_search` should accept a scope filter so a project session sees
`scope:global` + `scope:project:<current>` and not other projects' memories -
a tag filter on `search_multi`, or a post-filter on the result set. This is the
one search enhancement the recall side needs.

## Safety / correctness notes

- **No loop.** Capture writes to `~/.aida/brain/memory/*.json`; the hook only
  matches `~/.claude/**`. Aida's own writes never re-trigger it.
- **Never blocks.** Embedding is a ~200-400 ms network call; the capture hook is
  detached (`nohup &`) so it never adds to the turn. Recall never runs from a
  per-turn hook at all (see the latency section).
- **Never fails the tool.** The capture hook `exit 0`s on every branch; a broken
  brain degrades to "no capture," not a blocked write.
- **Secrets stay out.** Only `~/.claude/memory` + `CLAUDE.md` are captured, never
  `settings.local.json`, tokens, or caches (design doc tier 3).
- **Supersession = no churn.** Same-key instruction/fact writes supersede; the
  git repo sees one active record per memory, not a growing pile.
- **jq dependency.** These use `jq` for clarity; the existing third-party hooks use
  awk to avoid it. If jq is not guaranteed on every machine, port the field
  extraction to the same awk helper.

## What it takes to go live

**All done - shipped via PR #91 (2026-07-18).**

Capture (the only hook):
1. [x] `aida brain remember` - thin wrapper over `WriteMemory` (~1 command file).
2. [x] Drop in `capture-memory.sh` + the `PostToolUse(Write|Edit)` settings entry.
   Shipped as `aida brain capture-hook` (reads stdin directly, no separate shell
   script) installed by `aida setup`. 174 memories backfilled.

Recall (no hooks):
3. [x] Generate the soul->CLAUDE.md identity block (path 1); wire regeneration into
   `aida brain sync`. Shipped as `aida brain project-soul`, which splices a managed
   `<!-- BEGIN/END aida-soul -->` block into `~/.claude/CLAUDE.md` and runs after
   `brain sync`.
4. [x] Add a scope tag-filter to `mcp__aida__brain_search` (path 2). Shipped, but as
   `memoryScopeMatch` inside the new `brain_recall` path rather than a `brain_search`
   change - see "Scope filtering" above.
5. [x] Add the CLAUDE.md instruction telling Claude to call `brain_search` on memory
   read / context need. Shipped, pointing at `mcp__aida__brain_recall` instead (the
   tool that actually landed) - see "Recall path 2" above.

Step 1 was the real work; the rest was small, as predicted. Nothing here changed the
brain schema - it rides entirely on existing `MemoryRecord` fields. The optional
`PostToolUse(Read)` recall hook + `GET /brain/recall` daemon route remain deferred
(not needed - path 1 + path 2 cover it).

## Open questions

- **Recall settled: not a per-turn hook. Shipped.** Measured cold spawn is
  ~430-830 ms, too much per turn. Recall = static soul projection (path 1, shipped
  as `aida brain project-soul`) + warm on-demand recall (path 2, shipped as
  `mcp__aida__brain_recall`, `aida brain recall`, and `claude_memory_recall` for
  Jarvis voice - not a `brain_search` change). The automatic `PostToolUse(Read)`
  variant stays deferred behind a warm daemon route; not needed so far.
- **Recall exclusions (found during live testing, shipped).** Two record types
  are captured and backed up but excluded from recall results via a
  `recallEligible` filter applied before top-k: the `...:MEMORY` index blobs (24
  of 174 - Claude's own derived index, not an episodic memory) and the captured
  global `claude:global:CLAUDE` record (standing instructions already present in
  every session's context; reciting it back truncates badly).
- **Recency needed real timestamps (found during live testing, shipped).**
  Backfill capture had stamped all 174 records with capture-time, collapsing them
  onto one instant and breaking recency-based recall. Capture now stamps a
  record's `Created` from the source file's mtime instead; `RetimeMemory`
  corrected the already-stored records in place (updates `brain.db` + the JSON
  mirror, no supersede churn). All 174 re-timed.
- **Stale `aida serve` processes.** 5 were running during this audit with nothing
  on :1610 - likely leaked stdio MCP instances from past sessions. Unrelated to
  the bridge, but worth a cleanup pass.
- **Project key stability.** git-toplevel basename collides across same-named
  repos in different paths. Fine for now (Source keeps the full path); revisit
  if collisions bite. Still open - not addressed by the shipped work.
- **Edit vs Write content.** Capture re-reads the file from disk (canonical
  post-write state) rather than trusting `tool_input` - so partial `Edit`s
  capture the whole updated memory, not just the diff. (Handled in `remember`.)
