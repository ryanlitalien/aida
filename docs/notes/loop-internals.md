# Engineering note: `aida loop` internals

How the autonomous loop actually works, from the code (`internal/cli/loop.go`,
`internal/cli/pr.go`) and the original design doc (`docs/autonomous-loop-design.md`).
This is the deep-dive companion to the blog series; the flag reference lives in
CLAUDE.md's "Autonomous loop surface" section.

## The shape: a deterministic outer loop around a stateless inner agent

`aida loop` is a Ralph-style driver (credit: Geoffrey Huntley's Ralph pattern and
Ryan Carson's snarktank/ralph - see `docs/notes/inspiration.md`). Ralph is ~120
lines of bash that repeatedly spawns a *fresh-context* coding agent, each instance
completing one story from `prd.json`, appending what it learned to `progress.txt`,
and exiting. Three ideas carry it:

1. **Externalized state** - memory between iterations is git history plus two flat files.
2. **Fresh context per unit of work** - a long-lived agent context degrades
   ("context rot"), so reset it every story.
3. **Hard feedback loops** - typecheck/test must stay green or broken code
   compounds across iterations.

Aida adopts the control structure and replaces the files with substrate it already
had: `aida tasks` (six statuses, stable `#N` IDs, tags, priorities) replaces
`prd.json`, and the brain's embedded lessons replace `progress.txt`.

## One round, step by step

`runLoopCtx` runs rounds until the set drains, a bound trips, or the context is
cancelled (when `aida serve --loop` hosts the dispatcher in-process, the daemon's
SIGINT/SIGTERM context stops the loop alongside the HTTP server).

1. **Pick a batch** (`pickBatch`): up to `--concurrency` non-terminal tasks
   matching every `--tag`, sorted priority `p1 > p2 > p3` (lexical compare on the
   priority tag) then oldest-first. Both `open` and `in-progress` are eligible - an
   in-progress task is one a prior round started but didn't finish. Empty set:
   non-daemon mode prints `<promise>COMPLETE</promise>` (Ralph's own completion
   sentinel, kept as a tip of the hat) and exits; `--daemon` sleeps `--poll` and
   re-checks.
2. **Mark in-progress**, one task per goroutine (`processTask`), with a
   `sync.WaitGroup` barrier between rounds so task selection is race-free:
   every batch task reaches done-or-hold before the next pick happens.
3. **Isolate** (`setupTaskWorktree`), when `--worktree` (or anything implying it:
   `--pr`, `--concurrency > 1`, `--sandbox docker`): a git worktree off
   `origin/main` on branch `auto/<id>-<slug>`, stale state from a crashed prior
   run cleared first. Teardown is a `defer` so every return path cleans up.
4. **Recall** (`loopRecall`): embed the task title + description, pull the top
   `--recall-k` (default 3) semantically similar brain lessons, and format them as
   a "Relevant past learnings" preamble. This is the reason the loop lives on aida
   instead of being a bash script: where Ralph greps a flat `progress.txt`, the
   fresh agent starts *already knowing* the relevant gotchas via vector recall.
5. **Spawn** (`spawnLoopAgent`): a fresh `aida --agent --run-dir <dir>` subprocess -
   clean context, one task, prompt = recall preamble + task body + gate
   instruction (`buildLoopPrompt`). The prompt ends by demanding a `## Learnings`
   section: 2–4 reusable bullets. The subprocess is enqueued on the jobs store, so
   governance (timeout, sandbox tier) is auditable per run.
6. **Gate** (`runCodeGate`): every `--check` command must exit 0. On failure the
   loop does not park the task immediately - it re-spawns a *continuation* agent
   with the exact failing output fed back (`buildFixPrompt`: "the working tree
   still has that attempt's changes; fix these failures and nothing else"), up to
   `--max-fix-iterations` (default 3) attempts. Iterate-until-green, not
   retry-from-scratch: same worktree, so each attempt builds on the last.
   No `--check` configured = pass with a warning, because a loop without a
   feedback loop is how broken code compounds.
7. **Land it**: on green, either `CompleteTask` directly, `--commit` the tree, or - 
   with `--pr` - commit on the worktree branch, run the adversarial
   `--review-panel` (N reviewer agents with correctness/security/test-coverage
   briefs that must majority-approve), push, open a PR tagging `--reviewer`, and
   park the task on `hold` tagged `pr-open`. **The loop never merges.** The human
   stays at exactly two points: reviewing the PR and merging it.
8. **Distill** (`distillLearning`): read the run's `output.md`, extract the
   `## Learnings` section (capped at 800 chars; no section = record nothing rather
   than pollute the lessons table with a full answer), and write it as a
   first-class brain lesson with an embedding, keyed on the task title. The next
   similar task's `loopRecall` surfaces it. That's the compounding loop closed.
9. **Fail safe**: an agent error or a gate that never goes green parks the task on
   `hold` - visible, non-terminal, never silently re-spun.

## Runtime bounds

`loopState` is the mutex-guarded accounting shared across concurrent workers:

- **Budget**: each finished run's `cost_usd` is parsed from its `events.ndjson`
  (`readRunCostUSD`; best-effort, informational). `--max-budget-usd` (falling back
  to config `agent.max_budget_usd`) stops the loop once accumulated spend crosses
  the line - checked between rounds and between fix attempts.
- **Circuit breaker**: `--max-consecutive-failures` stops after N consecutive
  tasks parked on hold. A pass resets the counter.
- **Safety bound**: `--max-iterations` counts *rounds*, not tasks (like Ralph's
  loop counter).
- **Per-task timeout**: `--per-task-timeout` (default 7m30s) bounds each spawned
  agent via `context.WithTimeout`.

## Sandboxing

`--sandbox docker` confines each spawned agent to a Linux microVM. The host
(macOS) binary can't run in it, so the loop cross-compiles a `CGO_ENABLED=0`
linux/arm64 `aida` once per run (`buildLinuxAida` - pure-Go SQLite driver makes
this dependency-free) and provisions it into each sandbox; `--provision-aida`
supplies a prebuilt one when the loop is working tasks in some other repo. The
build output path is keyed by PID so two concurrent loop processes built from
different code can't silently overwrite each other's provisioned binary. The
policy mounts exactly two things: the task's worktree and the run directory
(so the confined agent can write its `events.ndjson`/`output.md` back).
A Seatbelt (macOS sandbox-exec) tier was tried during development and dropped;
Docker won on having an actually-enforceable write boundary.

## Driving from a goal

```
aida loop plan "add priority to tasks: schema, badge, edit selector, filter" --tag prio
aida loop --tag prio --check "make test"
```

`aida loop plan` decomposes a plain-English goal into tagged tasks by reusing the
generic LLM task-extraction path, assigning priority by document order (first two
tasks `p1`, next three `p2`, rest `p3`) as a coarse dependency stand-in. This is
the loop's honest weak spot: a loop-specific decomposition prompt enforcing
dependency ordering, one-iteration sizing, and verifiable acceptance criteria is
still TODO (see the `SKETCH` comment in `newLoopPlanCmd`).

## Design decisions worth stealing

- **Tasks are the task list; brain is the memory.** No parallel state files to
  drift. The loop reads and writes the same task store the CLI, voice layer, MCP
  server, and web UI use.
- **Safe by default.** `--commit` and `--pr` are off; `--dry-run` prints the pick
  and exits; failures park on `hold`. Ralph runs with permissions checks disabled;
  aida instead keeps the human at PR review and merge.
- **Fresh context is the point, not a limitation.** One subprocess per task means
  no context rot, and it makes the quality gate meaningful - the gate judges a
  bounded, attributable change.
- **The gate output is a prompt.** Feeding the failing check output verbatim into
  a continuation agent turns "CI is red" into a actionable fix instruction with
  zero human translation.
- **Composable into the daemon.** `aida serve --loop` hosts the same dispatcher as
  a goroutine, so tasks captured by voice get worked continuously; flag validation
  runs up front so a bad combination fails `aida serve` outright instead of dying
  silently in the goroutine.
