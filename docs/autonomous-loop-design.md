# Autonomous Loop (`aida loop`) design

Status: shipped, merged to main via PR #81 (2026-06-23). Implementation:
`internal/cli/loop.go` and `internal/cli/pr.go`. Originally drafted as part of the
context-gathering initiative explored on the (unmerged) `feat/context-lake` branch; the
loop itself shipped independently through `feat/autonomous-loop` and `feat/autoloop-build`.

This document is the original design rationale and Ralph-pattern comparison that motivated
`aida loop`, updated to reflect what actually shipped. For the full phase-by-phase spec
(voice ingest, swarmed dispatch, gated PRs, sandboxing) see `docs/autonomous-loop/OVERVIEW.md`
and `SPEC.md`. For the exact flag reference, see CLAUDE.md's "Autonomous loop surface"
section.

## Goal

Give aida a **Ralph-style autonomous driver**: a deterministic outer loop that repeatedly
spawns a fresh-context `aida --agent` instance, each one completing the single highest-priority
open task, until the set is drained or a budget is hit. We adopt Ralph's *control structure*,
not its files, because aida already has better substrate (semantic brain recall, a real task
system, a jobs queue with worktrees) than Ralph's flat `progress.txt` + `prd.json`.

Reference: snarktank/ralph (Ryan Carson) and Geoffrey Huntley's Ralph pattern. Notes below.

## What Ralph is (and why it works)

Ralph is ~120 lines of bash wrapping a stateless coding agent:

```
for i in 1..N:
    spawn FRESH agent (clean context)
      -> read prd.json + progress.txt
      -> pick highest-priority story where passes:false
      -> implement ONE story
      -> run quality checks (typecheck/test)
      -> commit, set passes:true, append learnings
    if all stories pass -> <promise>COMPLETE</promise>, exit
```

Three ideas carry the whole thing:
1. **Externalized state.** Memory between iterations is git history + `prd.json` (task list) +
   `progress.txt` (append-only learnings, with a curated "Codebase Patterns" header).
2. **Fresh context per unit of work.** Each iteration is a new instance with clean context.
   Deliberate anti-context-rot: a long-lived context degrades, so reset it every story.
3. **Hard feedback loops.** Typecheck/test/CI must stay green or broken code compounds across
   iterations. Right-sized tasks ("completable in one context window") keep each step in budget.

The two skills are the on-ramp: `/prd` (feature idea -> PRD via Q&A) and `/ralph` (PRD ->
`prd.json` with small, dependency-ordered, verifiably-gated stories).

## Where aida stands

This was the gap analysis that motivated building the driver. Verdicts below reflect what
has shipped since:

| Ralph pillar | aida equivalent | Verdict |
|---|---|---|
| Task list (`prd.json`, `passes`) | `aida tasks` (6 statuses, stable IDs, tags, MCP, web UI) | aida richer |
| Learnings (`progress.txt`) | brain lessons + `jarvis_lessons`, embeddings + recall + feedback->router boosts | aida well ahead |
| Fresh-context agent spawning | `internal/jobs/` + `pr_work` worktrees + `ask_user` pause/resume | shipped via `aida loop` |
| Iterate-until-done driver | `aida loop` (`internal/cli/loop.go`) | shipped |
| Decomposition discipline (small, verifiable, ordered) | `aida loop plan` (generic LLM extraction) | partial, see Outstanding work |
| Per-iteration quality gate | `--check` + iterate-until-green (`internal/eval`) | shipped |

aida had the substrate; it lacked the *driver*, the *decomposition discipline*, and an
*enforced quality gate*. `aida loop` now supplies the driver and the gate. The decomposition
discipline is still partial: `aida loop plan` reuses the generic task-extraction prompt
rather than one enforcing dependency order, one-iteration sizing, and verifiable acceptance
criteria (see Outstanding work, below).

## The killer synergy: recall replaces `progress.txt`

Ralph re-reads a flat text file each iteration and greps for patterns. aida can do strictly
better: at the start of each iteration, embed the task and pull the top-k semantically-similar
brain lessons into the fresh agent's prompt (`loopRecall`). The fresh agent starts
*already knowing* the relevant gotchas. On completion we distill the iteration's learning back
into the brain as a first-class lesson (with an embedding), so the next similar task recalls it.
That is the compounding loop Atlan describes, and it is why this belongs on aida, not in bash.

## `aida loop` command surface

The exact flag reference (every flag, default, and one-line meaning) lives in CLAUDE.md's
"Autonomous loop surface" section, kept current there because Claude Code loads CLAUDE.md
automatically. Repeating it here would just be a second copy to drift out of sync. Three
groups of flags landed after this sketch, each closing one of the gaps in the table above:

- **Isolation and parallelism**: `--worktree`, `--concurrency`, `--sandbox` (`none`/`docker`),
  `--sandbox-memory`, `--provision-aida`. Each task can run in its own git worktree branched
  off `origin/main`; `--concurrency` fans out several tasks per round, each in its own
  worktree; `--sandbox docker` confines the spawned agent to an sbx microVM running a
  provisioned linux/arm64 `aida` in place of the host binary.
- **PR workflow**: `--pr`, `--reviewer`, `--base`, `--review-panel`. Instead of completing
  the task directly, the loop commits on the worktree branch, optionally runs an adversarial
  review panel, then opens a PR and tags a reviewer. The loop never merges.
- **Runtime bounds and daemon mode**: `--max-budget-usd`, `--max-consecutive-failures`,
  `--daemon`, `--poll`, `--max-fix-iterations`. A circuit breaker stops the loop after too
  many consecutive tasks park on `hold`; a budget cap stops it once accumulated agent spend
  crosses a threshold; `--daemon` keeps the dispatcher alive, polling for new tasks instead
  of exiting when the set drains.

The flags this sketch originally proposed, `--tag`, `--max-iterations`, `--check` (now
repeatable), `--commit`, `--recall-k`, `--per-task-timeout`, and `--dry-run`, are all still
present with the same meaning.

### Per-iteration flow (as implemented)

1. **Pick a batch.** Each round selects up to `--concurrency` highest-priority non-terminal
   tasks in the set (`open` or `in-progress`); priority p1>p2>p3, then oldest-first. An empty
   set emits `<promise>COMPLETE</promise>` and exits, or in `--daemon` mode sleeps `--poll` and
   checks again.
2. Mark each picked task `in-progress`.
3. **Recall**: `brn.Search(task)` -> format the top `--recall-k` lessons as a preamble
   (`loopRecall`).
4. **Isolate**, if `--worktree` (or anything that implies it: `--pr`, `--concurrency` > 1,
   `--sandbox docker`): create a git worktree off `origin/main` on branch `auto/<id>-<slug>`.
5. **Spawn** a fresh `aida --agent --run-dir <dir>` subprocess (clean context), confined per
   `--sandbox`, scoped to that one task; prompt = recall preamble + task body + gate
   instruction. Reuses the exact enqueue -> run -> complete/fail pattern from
   `tasks ingest --auto-solve`.
6. **Quality gate**: run every `--check` command. On failure, re-spawn a continuation agent
   with the failing output fed back as a fix prompt, up to `--max-fix-iterations` attempts
   ("iterate-until-green"). No gate configured = pass with a warning; a loop without a
   feedback loop is how broken code compounds.
7. On a green gate: either `--commit` directly, or, with `--pr`, commit on the worktree
   branch, run `--review-panel` adversarial reviewers, push, and open a PR tagging
   `--reviewer` (never merges; parks the task on `hold` tagged `pr-open` for human review).
   Otherwise `CompleteTask`. Either way, **distill a learning** back into the brain. On agent
   error, or a gate that never goes green within `--max-fix-iterations`: park the task on
   `hold` (do not silently re-spin).
8. After each task, check the budget (`--max-budget-usd`) and circuit breaker
   (`--max-consecutive-failures`); either can stop the loop between rounds. Otherwise repeat
   until the set drains or `--max-iterations` rounds have run.

### What shipped, and what's still outstanding

| Piece | State |
|---|---|
| pick / recall / spawn / gate / mark-done control flow | shipped |
| commit-on-pass | shipped, gated behind `--commit` (default off) |
| `--dry-run` (pick + print, no mutation/spawn) | shipped |
| learning write-back to brain (`distillLearning`) | shipped, parses the run's `## Learnings` section from `output.md` and records a `lessons.Lesson` (embedded) so `loopRecall` surfaces it next time |
| per-iteration worktree isolation (`--worktree`) | shipped, `internal/worktree`, branch `auto/<id>-<slug>` off `origin/main` |
| iterate-until-green retries (`--max-fix-iterations`) | shipped, failing `--check` output feeds a fix prompt to a continuation agent |
| budget stop (`--max-budget-usd`) and circuit breaker (`--max-consecutive-failures`) | shipped, `loopState` in `internal/cli/loop.go` |
| parallel dispatch (`--concurrency`) | shipped, round-barrier batch model, requires `--worktree` |
| sandboxing (`--sandbox docker`) | shipped, sbx microVM confinement, requires `--worktree`; a provisioned linux/arm64 `aida` runs in place of the host binary |
| autonomous PR creation + adversarial review panel (`--pr`, `--review-panel`) | shipped, `internal/cli/pr.go`; commits, optionally runs N adversarial reviewer agents that must majority-approve, then opens a PR and tags a reviewer. The loop never merges |
| `--daemon` / `--poll` | shipped, dispatcher waits for new tasks instead of exiting once the set drains |
| `aida loop plan "<goal>"` decomposition | partial, reuses the generic `llm.TaskExtraction*` path to turn a goal into tagged tasks; a loop-specific prompt (dependency order, one-iteration sizing, verifiable acceptance criteria) is still TODO, see the `SKETCH` comment in `newLoopPlanCmd` |

### Closing the compounding loop

`buildLoopPrompt` now asks each agent to end with a `## Learnings` section (2-4 reusable
bullets). On a passing iteration `distillLearning` extracts that section and records it as a
brain lesson keyed on the task title, with an embedding. The next time a similar task comes up,
`loopRecall` pulls it into the fresh agent's prompt. That is the full Atlan-style compounding
loop: each iteration leaves the brain a little smarter, so later iterations terminate faster and
repeat fewer mistakes. No `progress.txt`, no grep.

### Driving from a plain-English goal

```
aida loop plan "add priority levels to tasks: schema, badge, edit selector, filter" --tag prio
aida loop --tag prio --check "make test"
```

`plan` decomposes the goal into tagged tasks (priority by document order as a coarse
dependency stand-in); `loop` then drains that set one fresh-context agent at a time.

## Design decisions

- **Tasks are the task list.** No `prd.json`; `aida tasks` filtered by `--tag` is the durable set,
  and it is richer than Ralph's JSON (statuses, stable IDs, web UI, MCP).
- **Brain is the memory.** No `progress.txt`; lessons + recall replace it and compound.
- **Fresh context per task is the point.** One subprocess per task, clean context every time,
  the same context-rot rationale explored in the (unmerged) context-lake work.
- **Safe by default.** `--commit` is off, `--dry-run` is honored, and failures park on `hold`
  rather than retrying forever. `--pr` adds a second gate: the loop commits on the worktree
  branch, optionally runs an adversarial `--review-panel`, and opens a PR tagging `--reviewer`,
  but it never merges. Ralph runs fully autonomous (`--dangerously-skip-permissions`); aida
  instead keeps a human at exactly two points, opening the PR and merging it, and can
  additionally offer a human-in-the-loop variant via the existing `ask_user` + Jarvis notify
  path.
- **Quality gate is mandatory in spirit.** The command runs without `--check`, but warns: the
  feedback loop is what keeps an autonomous loop from compounding errors. When a gate is
  configured, a failure doesn't park the task immediately; it retries with the failure fed
  back in, up to `--max-fix-iterations` times, before giving up.
- **Composable into `aida serve`.** `aida serve --loop` hosts the same dispatcher as a
  background goroutine (`--loop-tag`, `--loop-worktree`, `--loop-pr`, and friends mirror the
  standalone flags), so tasks ingested from voice (Jarvis) get worked continuously without a
  separate `aida loop` process.

## Definition of done for any CLI change

Documenting the command in the agent-facing instruction files is part of done, not an
afterthought. For every new/changed `aida` command, flag, or subcommand:
- update the aida repo `CLAUDE.md` (auto-loaded by Claude Code in this repo) with the command
  and a flag table;
- if it is worth reaching for from any cwd, add a row to the "Subcommands to reach for first"
  table in the global `~/.claude/CLAUDE.md`;
- alongside the usual build / vet / gofmt / test gates.

The point: Ryan should never have to remember arguments, and Claude/agents should be able to run
the command from what is already in context.

## Outstanding work

Everything this sketch originally deferred has shipped except one:

- **Decomposition front-end discipline.** `aida loop plan "<goal>"` decomposes a goal into
  tasks today, but through the same generic extraction prompt `aida tasks ingest` uses. A
  loop-specific prompt, one that enforces dependency ordering, one-iteration sizing, and
  verifiable acceptance criteria written into each task body, is still TODO. See the `SKETCH`
  comment in `newLoopPlanCmd` (`internal/cli/loop.go`).

Learning write-back, per-task worktree isolation, budget and circuit-breaker stop conditions,
and `job kind: "loop"` integration with `aida jobs` are all done; see "What shipped, and
what's still outstanding" above.

## References
- snarktank/ralph: https://github.com/snarktank/ralph
- Geoffrey Huntley, "Ralph": https://ghuntley.com/ralph/
- Context-rot / fresh-context rationale: Anthropic's context-engineering writing on
  compaction and why a long-lived context degrades. The fuller writeup lives in the
  context-lake exploration on the (unmerged) `feat/context-lake` branch.
