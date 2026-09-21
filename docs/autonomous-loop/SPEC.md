# Aida Autonomous Loop - Technical Spec

Status: shipped, merged to main via PR #81 / PR #82 (2026-06-23). The normative,
code-grounded companion to [`OVERVIEW.md`](./OVERVIEW.md); supersedes the earlier sketch in
[`../autonomous-loop-design.md`](../autonomous-loop-design.md). Every cited path,
symbol, and line number was verified against the repo **at the time this spec was
drafted, pre-implementation**: it invents no APIs, and where a name is new it is
marked **NEW**. The rest of this document is that pre-implementation draft, kept as
the design record. It reads correctly for control flow, phase ordering, and
rationale; the box below is the one place to check before trusting a specific type,
flag default, command, or config key against what actually shipped.

> **Implementation deviations from this draft (verified against the repo).** All
> seven phases plus follow-ons shipped (see `git log --oneline | grep -i loop`
> for the commit-per-phase history). Where the shipped code differs from what
> follows:
>
> - **Sandbox tier and commands.** `internal/sandbox/sandbox.go` is a single file
>   (not `sandbox.go`/`docker.go`/`seatbelt.go`), tiers are `none`/`docker` only
>   (the Seatbelt fallback was dropped), and the docker tier is driven by the
>   real `sbx` CLI (`sbx create`/`sbx exec`/`sbx rm`), not `docker sandbox
>   run`/`docker exec` as drafted below. `Policy` in the shipped code is
>   `{WorkDir, ExtraMounts, MemoryLimit, Env, ProvisionBin}`, narrower than the
>   drafted `{Tier, Workspace, RunID, MemoryMB, EgressAllow, InjectSecrets}`.
> - **No network egress control was built.** `SandboxConfig`, `egress_allowlist`,
>   and `InjectSecrets`/secret-header injection do not exist anywhere in the
>   repo (grep confirms zero hits). The sandbox gives filesystem + process
>   isolation via the `sbx` microVM only; the "third gate layer" this draft
>   describes throughout Phases 5-6 and the verification walkthrough was never
>   implemented. Only two of the three described defense layers exist: the
>   `request_approval` state machine, and `confirm_always` (which also has no
>   default `gh pr merge`/send patterns configured).
> - **No config-file surface shipped beyond `agent.max_budget_usd`** (which
>   already existed and is now read). `agent.checks`, `agent.pr.*`, and the
>   whole `sandbox:` block described in "New config keys" below do not exist in
>   `internal/config/config.go`; every corresponding loop flag (`--check`,
>   `--reviewer`, `--base`, `--sandbox`, etc.) is CLI-only with a hardcoded
>   default, not config-resolved.
> - **`manifestVersion` is `4`**, not the `2→3` bump this draft describes:
>   Phase 5's approval fields and Phase 6's governance fields landed in the same
>   increment as originally planned, just numbered one higher.
> - **The review panel's majority vote lives inline** in
>   `internal/cli/pr.go`'s `runReviewPanel` (`approvals*2 > n`), not as a
>   separate `eval.PanelVerdict` / `internal/eval/panel.go`.
> - **The "tests-missing" deterministic warn reviewer (Phase 3's third
>   enforcement layer) was not built.** Only the prompt directive (layer 1,
>   `buildLoopPrompt`) and `CodeReviewer`'s hard `make test` gate (layer 2)
>   exist.
> - **The loop's own `--pr` path does not call `request_approval` or `gh pr
>   merge`.** It commits, pushes, opens the PR, and parks the task on `hold`
>   (tagged `pr-open`) for a fully manual merge by a human. The generic
>   `request_approval`/`StateAwaitingApproval`/`aida jobs approve|reject`
>   machinery described in Phase 5 is real and shipped, any `--agent
>   --run-dir` task can pause on it, it just isn't wired into the loop's own
>   merge step. The elaborate "approve, then agent runs `gh pr merge`" walkthrough
>   in the last section of this document describes the original intent, not
>   what `aida loop --pr` does today.
> - **`aida jobs reject` takes `--reason`, not `--because`** as drafted below.
> - **`aida brain analyze --propose` shipped read-only.** Real flags are
>   `--propose`, `--out <path>`, `--min-failures` (default `2`), `--limit`,
>   `--source`, `--ndjson`. There is no `--pr`, `--dry-run`, `--reviewer`, or
>   `--base` flag; turning a proposal into a PR is still a manual step.
> - **`--worktree`, `--sandbox`, `--sandbox-memory`, and `--provision-aida`**
>   are real `aida loop` flags this draft doesn't mention (worktree isolation
>   is opt-in, not automatic per iteration as §4.2 assumes); **`--poll`** is the
>   shipped name for what this draft calls `--poll-interval`, defaulting to
>   `5s` not `30s`. See the corrected flag table in "Reference" below.
> - **A bonus surface this draft doesn't cover:** `internal/cli/swarm.go`, a
>   natural-language front door ("swarm on PR 456" / "work through my prio
>   tasks") that recognizes an autonomous-work request and hands off to the
>   same `runLoopCtx` machinery, so a user never has to type a loop flag at all.

## Contents

- [Architecture & end-to-end pipeline](#architecture--end-to-end-pipeline)
- [Agent swarming / fan-out model](#agent-swarming--fan-out-model)
- [Phase 1 - Voice-triggered Notion ingest](#phase-1--voice-triggered-notion-ingest)
- [Phase 2 - Code-correctness eval loop](#phase-2--code-correctness-eval-loop)
- [Phase 3 - Autonomous PR creation + adversarial review swarm](#phase-3--autonomous-pr-creation--adversarial-review-swarm)
- [Phase 4 - Dispatcher (daemon, per-iteration worktree, budget, concurrency)](#phase-4--dispatcher-daemon-per-iteration-worktree-budget-concurrency)
- [Phase 5 - HITL approval gate (`awaiting_approval`)](#phase-5--hitl-approval-gate-awaiting_approval)
- [Phase 6 - Docker Sandboxes integration](#phase-6--docker-sandboxes-integration)
- [Phase 7 - brain analyze auto-proposals (follow-on)](#phase-7--brain-analyze-auto-proposals-follow-on)
- [Reference - new CLI flags, config keys, job states, types](#reference--new-cli-flags-config-keys-job-states-types)
- [Verification matrix & end-to-end walkthrough](#verification-matrix--end-to-end-walkthrough)

---

## Architecture & end-to-end pipeline

The autonomous loop is not a new subsystem; it is the wiring of five primitives that already exist in Aida into one continuous, gated assembly line. This section names each component, the data that flows between them, and exactly where capture → dispatch → eval → review → PR → approval → merge is realized in code.

### Components

**1. Jobs queue + manifest + run-dir** (`internal/jobs/`)

Every unit of background work is a `jobs.Manifest` (`internal/jobs/manifest.go:48`) - the on-disk source of truth at `~/.aida/jobs/<profile>/runs/<run_id>/manifest.json`; the SQLite row in `jobs.db` is a derived index (`Reindex` rebuilds it from manifests). The `jobs.Store` (`internal/jobs/store.go`) is the mutation API: `Enqueue / Claim / Complete / Fail / Pause / Resume / SetWorktreePath / SetArtifactURL / MarkNotified`. States today are `queued / running / awaiting_input / done / failed` (`manifest.go:13-17`), gated by `IsValidState` / `IsTerminalState`. `manifestVersion = 2` (`manifest.go:42`).

Each job owns a **run-dir** - the contract between the detached agent subprocess and everything observing it:

| File | Writer | Reader |
|------|--------|--------|
| `events.ndjson` | agent (`runDirSink` in `agent.go`) | tasks-web SSE tail; cost read-back (Phase 4) |
| `output.md` | agent final answer (`runDirSink.writeOutput`, `agent.go:491`) | `distillLearning` (`loop.go:333` reads `jobs.OutputPath`) |
| `input.txt` | `job_send_input` / `aida jobs ...` (atomic rename) | `buildAskUserTool` poll (`agent.go:809`) |

The `emitComplete` event (`agent.go:494`) carries `result.Turns` + `costUSD` (`engine.CostFromUsage`). **Verification note:** that `costUSD` is *only* written to `events.ndjson` inside the subprocess - `cfg.Agent.MaxBudgetUSD` (`config.go:31`) exists in config but is never read by any caller. Phase 4's budget stop must parse cost back out of `events.ndjson`; until then there is no cost ceiling.

**2. The `aida --agent` inner loop** (`internal/cli/agent.go runAgentMode`)

A stateless, single-task worker. `--run-dir` mode opens the `runDirSink` (`agent.go:106-113`), builds an exec-plan via `brain.NewExecPlan` (`agent.go:135`), builds tools with `engine.BuildAgentToolsLazy` (`agent.go:212`), and runs to a turn cap: default 15, overridable by `cfg.Agent.MaxTurns` or env `AIDA_AGENT_MAX_TURNS` (`agent.go:401-408`; `pr_work` sets it to 50). `buildAskUserTool` (`agent.go:761`) is registered only for daemon-managed runs (`runDirSink != nil`): it calls `store.Pause(runID, prompt)` → `awaiting_input`, then blocks polling `<run-dir>/input.txt` every 2s, and `store.Resume(runID)` on reply. This is the existing single human-interrupt mechanism the approval gate (Phase 5) generalizes.

**3. The worktree model** (`internal/jarvis/tools/jobs.go startPRWorkJob`)

Isolation today exists only for `kind=pr_work`: `git -C <repo> worktree add --detach` into `~/.aida/worktrees/<profile>/pr-<num>-<runID>/` (`jobs.go:150-166`), then `gh pr checkout` with `cmd.Dir = worktreeDir`, spawn detached with `Setpgid=true`. The path is stashed via `store.SetWorktreePath`. `removeWorktree` (`jobs.go:239`) is the cleanup helper. The plan promotes this into a shared `internal/worktree` package (`CreateWorktree` / `RemoveWorktree`) so `loop.go` and `serve.go` use it without importing voice-tools, and makes **per-task worktree = per-task Docker Sandbox workspace** (Phase 6) - satisfying Docker's one-sandbox-per-workspace rule and enabling Phase 4 concurrency.

**4. The daemon watcher** (`internal/cli/serve.go scanJobsForNotifications`)

A 2s ticker (`watchJobsForNotifications`, `serve.go:250`) that scans `store.List` and reacts to state transitions, using `NotifiedAt` for idempotency across restarts (`serve.go:277`). The switch (`serve.go:281-290`):

```go
switch j.State {
case jobs.StateAwaitingInput: msg = formatAwaitingNotice(&j)
case jobs.StateDone:          msg = formatDoneNotice(&j)
case jobs.StateFailed:        msg = formatFailedNotice(&j)
default:                      continue   // <-- silently drops unknown states
}
...
if jobs.IsTerminalState(j.State) && j.WorktreePath != "" {
    go cleanupWorktreePath(j.WorktreePath)   // worktree teardown on terminal
}
```

**Verification note (highest risk):** that `default: continue` (`serve.go:288`) silently swallows any state not in the switch. Adding `StateAwaitingApproval` to the enum without adding `case jobs.StateAwaitingApproval:` here means the user is *never told* a PR is waiting - the gate becomes a silent hang. Phase 5 must extend this switch in lockstep with the enum.

**5. The dispatcher - `aida loop`** (`internal/cli/loop.go`, branch `feat/autonomous-loop`)

The deterministic outer loop. `runLoop` (`loop.go:77`) iterates up to `--max-iterations` (default 10): `pickNextTask(brn, tags)` (priority `p1>p2>p3`, then oldest) → `SetTaskStatus(StatusInProgress)` → `loopRecall` (top-k brain lessons seeded into the prompt) → `spawnLoopAgent` (enqueues `kind="loop"`, runs `aida --agent --run-dir` **foreground in cwd**, `loop.go:263`) → `runQualityGate(--check)` (single shell pass/fail, `loop.go:294`) → `loopCommit` (gated behind `--commit`, default off) → `CompleteTask` → `distillLearning` (parses `## Learnings` from `output.md` into a brain lesson). Flags: `--tag --max-iterations --check --commit --recall-k(3) --per-task-timeout(7m30s) --dry-run`. Documented TODOs (`loop.go:33-34, 310`): per-iteration worktree isolation and a token/cost budget stop - both closed by Phase 4.

### End-to-end data flow

```
                          ┌─── HUMAN GATE #1 (before create) ──┐
 [Notion call]            v                                    │
     │   "turn that call into tasks"                           │
     ▼                                                         │
 CAPTURE  ── voice tool ingest_notion_tasks (Phase 1, Jarvis-only)
     │      aida tasks ingest --from-notion <ref> --dry-run  ◄───┘ "yes"
     │      → aida tasks ingest --plan --yes --tag auto
     ▼
 TASKS  tagged `auto`  (~/.aida/brain/tasks/<slug>.md + SQLite)
     │
     ▼
 DISPATCH  aida loop --tag auto --concurrency N        (loop.go runLoop)
     │      pickNextTask → SetTaskStatus(in-progress) → loopRecall
     │      per task: CreateWorktree(off origin/main) = 1 Docker Sandbox
     │                jobs.Enqueue(kind="loop") → run-dir
     ▼
 ┌────────────── per task, inside sandboxed worktree ──────────────┐
 │ AGENT   aida --agent --run-dir  (runAgentMode, maxTurns)          │
 │   │     events.ndjson · output.md · input.txt                   │
 │   ▼                                                             │
 │ EVAL    CodeReviewer (Phase 2): build/test/lint in WorkDir      │
 │   │     non-zero → Fail ReviewRecord → re-spawn with fix prompt │
 │   │     (iterate-until-green, --max-fix-iterations) ──┐         │
 │   │            ▲────────────────────────────────────┘ each     │
 │   ▼            attempt → brain.WriteEvalRun(eval-runs/<id>.json)│
 │ REVIEW  adversarial panel (Phase 3): N reviewer sub-agents      │
 │   │     (correctness / security / test-coverage) majority-pass  │
 │   │     else iterate                                            │
 │   ▼                                                             │
 │ PR      branch auto/<taskID>-<slug> · commit w/ test · push     │
 │   │     gh pr create --fill --reviewer <user> --base main       │
 │   │     SetArtifactURL(pr_url)                                  │
 │   ▼                                                             │
 │ request_approval  → store state = awaiting_approval (Phase 5)   │
 │         blocks polling approval.txt                             │
 └─────────────────────────┬───────────────────────────────────────┘
                           ▼
 daemon watcher (serve.go scanJobsForNotifications, +new case)
   case StateAwaitingApproval: Notifier.Enqueue → DrainOnNextWake
                           │
              ┌─── HUMAN GATE #2 (before merge/send) ───┐
              ▼   "Jarvis, approve PR 583"              │
 APPROVE  approve_job / aida jobs approve  → approval.txt = approved
              │   (reject → reject_job, loop iterates or parks)
              ▼
 MERGE    agent unblocks → gh pr merge  →  store.Complete → done
              │
              ▼
 CLEANUP  daemon: cleanupWorktreePath + sbx rm (terminal transition)
              │
              ▼
 DISTILL  distillLearning: output.md "## Learnings" → brain lesson
          (feeds loopRecall on next similar task)
```

The two human gates are the *only* synchronous blocks: (#1) before `auto` tasks are created (the `--dry-run` confirm in `ingest_notion_tasks`), and (#2) before any irreversible action - `request_approval` parks the job in `awaiting_approval` until the user says "approve". Belt-and-suspenders, merge/send tools are *also* listed in `config.GuardrailsConfig.ConfirmAlways` (`config.go:113`), and in a detached run-dir job `confirmFn` reads stdin and **fails closed** (`internal/engine/agent_tools.go buildMCPTool`) - so even an agent that skips `request_approval` cannot merge unattended.

### What exists vs. what this adds

| Capability | Exists today | This system adds |
|---|---|---|
| Background job lifecycle | `jobs.Store` Enqueue/Claim/Complete/Fail/Pause/Resume; manifest + run-dir | `RequestApproval`/`Approve`/`RejectApproval`; `StateAwaitingApproval`; manifest v2→3; `SpendCapUSD`/`TimeoutSec`/`ToolAllowlist`/`SandboxTier` fields |
| Single-task agent | `runAgentMode`, `ask_user`/`buildAskUserTool`, turn caps | `buildRequestApprovalTool`; `MemoryInstruction` injection (e.g. `build-after-changes`); sandbox-wrapped exec |
| Worktree isolation | `pr_work` only (`startPRWorkJob` / `removeWorktree`) | shared `internal/worktree`; per-iteration worktree off `origin/main` for every loop task |
| Daemon notifications | `scanJobsForNotifications` for awaiting_input / done / failed | `case StateAwaitingApproval`; speak "PR N ready, waiting on approval"; sandbox teardown alongside worktree cleanup |
| Quality gate | `runQualityGate` single shell pass/fail | `CodeReviewer` (`internal/eval/code.go`) build/test/lint → `ReviewRecord`; iterate-until-green w/ `--max-fix-iterations`; `WriteEvalRun` per attempt |
| Review | `eval.Loop` one-pass, observational (`reviewer.go`); the "Ralph Wiggum" iterate variant explicitly deferred at `reviewer.go:102-104` | adversarial review panel: N reviewer sub-agents (correctness/security/coverage) via `delegate_to_claude_code`, majority-pass before PR |
| PR creation | forbidden - `appendDestinationInstructions` blocks `gh pr create` (full-auto gated behind `auto_create:true`) | `internal/cli/pr.go`: branch/commit/push/`gh pr create --reviewer`; `SetArtifactURL`; un-gated via `auto_create:true` |
| Dispatcher | `aida loop` single-pass over a task set, manual invocation | `--daemon` continuous mode hosted in `runHTTPDaemon` (`aida serve --loop`); `--concurrency N` horizontal fan-out; `--max-budget-usd`/`--max-consecutive-failures` (wires `cfg.Agent.MaxBudgetUSD` via `events.ndjson` cost read-back) |
| Sandbox | none - `aida --agent` runs unconfined; `delegate_to_claude_code` uses /tmp worktree isolation only | `internal/sandbox/docker.go Wrap(cmd, policy)`: `docker sandbox run` microVM per worktree + egress allowlist; Seatbelt fallback (`seatbelt.go`) |
| Capture | `aida tasks ingest --from-notion` CLI + `NotionAdapter` | voice tool `ingest_notion_tasks` (`internal/jarvis/tools/ingest.go`), confirm-before-create, tags `auto` |
| Approval voice/CLI | `job_send_input` (answers ask_user only) | `approve_job`/`reject_job` voice tools + `aida jobs approve|reject <ref>` |

**Net:** the spine - queue, manifest, run-dir, detached agent, worktree, 2s watcher, eval reviewers, brain recall/distill - already ships. The autonomous loop adds (a) a voice capture front door, (b) an iterate-until-green code-eval inner loop plus an adversarial review panel, (c) PR creation, (d) one new non-terminal state `awaiting_approval` with its watcher case and approve/reject surfaces, (e) per-task Docker Sandbox confinement with egress policy, and (f) `aida loop` upgraded from a single-pass driver to a continuous, concurrent, budget-bounded dispatcher hosted inside `aida serve`.

---

## Agent swarming / fan-out model

Swarming in the autonomous loop is **two orthogonal runtime axes** built on primitives that already exist in the repo - the jobs queue (`internal/jobs/`), the `delegate_to_claude_code` meta-tool (`internal/engine/agent_tools.go:219`), the per-task worktree (`internal/jarvis/tools/jobs.go` `startPRWorkJob`), and the observational reviewer panel (`internal/eval/reviewer.go`). Nothing here invents a new orchestrator: it composes `aida loop` (`internal/cli/loop.go`) over those parts. A third, **build-time** axis (the Workflow tool) is how this spec is *implemented*, not a runtime component.

```
                 aida loop --concurrency N           ← horizontal (across the auto queue)
        ┌────────────────┬────────────────┬────────────────┐
        ▼                ▼                ▼
   task #A (sbx)     task #B (sbx)    task #C (sbx)         ← one Docker Sandbox + worktree each
        │
        ├─ generate attempt 1 ┐
        ├─ generate attempt 2 ├─ judge → pick passing+best   ← vertical: generator/judge tournament
        └─ generate attempt 3 ┘
        │
        └─ adversarial review panel (correctness/security/test-coverage)
              majority pass → PR ;  else iterate                ← vertical: review-before-PR gate
```

### Axis 1 - Horizontal: `--concurrency N` across the `auto` queue

`runLoop` (`loop.go:77`) is today a strictly serial `for i := 1; i <= o.maxIterations` loop calling `pickNextTask(brn, o.tags)`. Horizontal fan-out adds a worker pool: drain up to `N` non-terminal `auto`-tagged tasks at once, each claimed exactly once and each pinned to **its own worktree = its own Docker Sandbox**. Per-task worktrees are precisely what satisfies Docker's one-sandbox-per-workspace rule, so concurrency and sandbox isolation are the same mechanism. The implementation details (claim-once mutex, `errgroup` + semaphore, worktree creation, cleanup) live in [Phase 4](#phase-4--dispatcher-daemon-per-iteration-worktree-budget-concurrency); the new `loopOpts` fields are:

```go
type loopOpts struct {
    // ...existing: tags, maxIterations, check, commit, recallK, perTask
    concurrency            int     // --concurrency N  (default 1 = today's serial behavior)
    maxBudgetUSD           float64 // --max-budget-usd; wires cfg.Agent.MaxBudgetUSD (config.go:31, never read)
    maxConsecutiveFailures int     // --max-consecutive-failures; circuit-breaker
}
```

### Concurrency caps × budget interplay

`N` is the upper bound on *parallelism*; the budget is the upper bound on *spend*, and the budget can throttle effective concurrency below `N`. The mechanics:

- **Per-iteration cost is trapped in the subprocess.** `spawnLoopAgent` (`loop.go:263`) runs `aida --agent --run-dir <dir>` with `cmd.Stdout = nil`. The agent emits cost only into the run-dir: `runDirSink.emitComplete(result.Turns, costUSD, ...)` (`agent.go:494`) where `costUSD := engine.CostFromUsage(...)` (`agent.go:477`). So the loop must **read `costUSD` back from `events.ndjson`** after each `spawnLoopAgent` returns (see Phase 4 §4.5 `readRunCost`), accumulate into a mutex-guarded running total, and stop scheduling new tasks once `total >= o.maxBudgetUSD`. `cfg.Agent.MaxBudgetUSD` (`config.go:31`) exists but is **never read today** - this is its first consumer.
- **In-flight budget.** A worker about to spawn checks `total + estimate < cap`; if the cap is already exhausted, the pool drains in-flight work and stops claiming new tasks rather than killing running agents mid-iteration.
- **Failure circuit-breaker.** `--max-consecutive-failures` counts quality-gate failures (the existing `runQualityGate` → `StatusHold` path at `loop.go:153-158`); tripping it halts the whole pool so a systematically broken `auto` cohort can't burn the budget across all `N` lanes.
- **Per-job governance (sandbox phase).** `SpendCapUSD` and `TimeoutSec` also land on the `Manifest` (`internal/jobs/manifest.go`, [Phase 6](#phase-6--docker-sandboxes-integration)) so each lane is independently bounded: the dispatcher wraps each `spawnLoopAgent` in `context.WithTimeout(ctx, o.perTask)` (already done at `loop.go:270`) and the new `SpendCapUSD` short-circuits a runaway single agent before the global cap.

> **Verification note.** `cfg.Agent.MaxBudgetUSD` and `costUSD`/`emitComplete` are confirmed real (`config.go:31`, `agent.go:477/494`). But `spawnLoopAgent` currently discards stdout/stderr and never reads the run-dir back, so the cost read-back from `events.ndjson` is **net-new wiring** and must be built *before* `--max-budget-usd` can be trusted (a locked risk in the plan).

### Axis 2 - Vertical: generator/judge tournament + adversarial review panel (within one task)

Both vertical fan-outs run *inside* a single task's worktree/sandbox and both reuse `delegate_to_claude_code` sub-agents (worktree-isolated, `isolate=true`) - confirmed at `internal/engine/agent_tools.go:219-265`, where `isolate=true` "runs in a discarded git worktree and return[s] the diff inline (preview-only - does not modify the user's working tree)". That is exactly the property a tournament needs: N candidate diffs that don't collide.

**(a) Generator/judge tournament (optional, before committing an implementation).**
- Fan out 2–3 `delegate_to_claude_code` calls with `isolate=true`, each prompted toward a *different approach* to the same task. Each returns a candidate diff inline.
- A **judge** scores them. Two reuse paths exist: (1) the eval layer - run the (Phase-2) `CodeReviewer` over each candidate's worktree (`make build`/`test`/`vet`) and take `eval.AggregateVerdict` (`reviewer.go:126`, fail-loud: any `VerdictFail` loses); (2) for ties among passing candidates, the `GoldenQuery`/`GoldenExpected` + `Judge` model from `internal/cli/golden.go` picks best-on-quality. Winner's diff is applied to the task's real worktree; losers are discarded with their throwaway worktrees.
- This is opt-in (e.g. `--attempts K`, default 1) because it multiplies per-task cost by K and therefore interacts directly with the budget cap above.

**(b) Adversarial review panel (mandatory gate before a PR is opened).**
- After the implementation is green, fan out **N reviewer sub-agents with distinct lenses** - correctness / security / test-coverage - again via `delegate_to_claude_code` (or a Docker Agent team, below). This is the harness research's two-wave adversarial verify and the runtime instantiation of `eval.Loop` over a custom reviewer slice.
- Each reviewer returns an `eval.ReviewRecord{Verdict, Score, Issues, Rationale}` (`reviewer.go:61`). The panel reduces via **majority-pass** - a *new* `eval.PanelVerdict` reducer (see [Phase 3](#phase-3--autonomous-pr-creation--adversarial-review-swarm)), stricter than the engine's default fail-loud `AggregateVerdict` because here a single warn lens shouldn't block but a majority must approve. On sub-majority, the loop **iterates** - re-spawning the implementation agent with the failing `Issue.Message`s embedded (Phase 2's iterate-until-green mechanism; the existing `eval.Loop` at `reviewer.go:105` is explicitly one-pass and *defers* the "Ralph Wiggum" iterate-until-clean variant per its own doc comment).
- Each panel run persists via `brain.WriteEvalRun` → `~/.aida/brain/eval-runs/<id>.json` so `ListRecentEvalRuns` / `RecentFailedReviewsForEntities` (and [Phase 7](#phase-7--brain-analyze-auto-proposals-follow-on)'s `aida brain analyze`) can mine recurring reviewer failures into routing boosts (`internal/eval/boosts.go` `FailureBoosts`).

> **Verification note.** `delegate_to_claude_code` with `isolate=true` is real and worktree-isolated (`agent_tools.go:219`), and `eval.Loop`/`AggregateVerdict`/`ReviewRecord`/`DefaultReviewers` are real (`reviewer.go:105/126/61/148`). But `DefaultReviewers()` ships only *citation/completeness/scope* (`reviewer.go:148`) - the correctness/security/test-coverage **code** reviewers do not exist yet; the panel needs the new `CodeReviewers(...)` slice (Phase 2's `internal/eval/code.go`) plus the sub-agent lenses. `AggregateVerdict` is fail-loud, not majority - the panel needs its own `PanelVerdict` reducer rather than reusing it verbatim.

### Optional: Docker Agent teams as an alternative panel backend

Where `delegate_to_claude_code` shells one Claude Code sub-agent per lens, the **Docker Agent team** YAML (root + `sub_agents`, toolsets `filesystem`/`shell`/`think`/`todo`/`memory`/`mcp`) can run the whole review panel as a single declarative team inside the task's existing sandbox - one `docker sandbox`/`sbx` invocation produces all N lenses. This is an implementation detail of the panel, not a separate axis: the loop still consumes `ReviewRecord`s and applies the same majority gate. Use it when the panel is large enough that per-sub-agent subprocess spawn overhead dominates; otherwise the `delegate_to_claude_code` path is simpler and already wired.

### Axis 3 - Build-time swarming (the Workflow tool)

Distinct from runtime: this spec and its sibling phases are *built* by a Workflow-orchestrated swarm - independent phases (e.g. Phase 1 voice-ingest and Phase 2 eval-loop have no shared files) and per-file changes fan out in parallel, with adversarial code-review agents verifying each diff before commit, and the `docs/autonomous-loop/` docs produced by a documentation swarm (one drafter per section against cited code, an editor assembling). This axis leaves no runtime artifact; it is named here only so the swarming model is complete and to mirror the runtime generator/judge + review-panel structure at authoring time.

### Recursion guards (apply to every axis)

The vertical fan-out sub-agents are spawned *by* an `aida --agent` process, so the standing guards hold: `aida_query` is never exposed as an MCP tool (`feedback_no_mcp_query_tool`), and the ingest/approval voice tools stay Jarvis-only - never registered in `engine.BuildAgentToolsLazy`. A swarm sub-agent must not be able to re-enter `aida loop` or `aida tasks ingest --auto-solve`, or the fan-out becomes unbounded.

---

## Phase 1 - Voice-triggered Notion ingest

> **Status:** shipped (`internal/jarvis/tools/ingest.go`, the `ingest_notion_tasks`
> tool registered in `registry.go`). The schema (`page_ref`/`confirm`) and the
> dry-run → confirm flow described below match what shipped.

**Goal.** Close the one missing edge in the capture path: today there is no voice route into ingestion. `aida tasks ingest --from-notion` (`internal/cli/tasks_ingest.go`) and `adapters.NotionAdapter` (`internal/adapters/notion.go`) already do all the work - resolve a Notion page, LLM-extract a flat task list, write tasks + exec-plans, dedupe by source-hash. Phase 1 adds a single Jarvis voice tool, `ingest_notion_tasks`, that shells `aida` (mirroring `aidaQueryTool`) and lands the cohort tagged `auto` - the handoff that Phase 4's dispatcher (`aida loop --tag auto`) consumes.

**No new CLI surface.** Phase 1 reuses the existing `runTasksIngest` flags verbatim (`--from-notion`, `--dry-run`, `--plan`, `--tag` repeatable, `--yes`, `--force`). The only new code is the voice tool + its registration.

### New file: `internal/jarvis/tools/ingest.go`

A single constructor `ingestNotionTasksTool() Tool`, following the established `Tool` shape in `registry.go` (`Name`/`Description`/`Schema`/`Run func(json.RawMessage)(string,error)`).

**Schema** (two optional fields - both can be empty for the bare "that call" case):

```go
type ingestNotionInput struct {
    PageRef string `json:"page_ref"` // spoken title / URL / page-id; empty => recent-page heuristic
    Confirm bool   `json:"confirm"`  // false => --dry-run preview; true => real ingest
}
```

```jsonc
{
  "type": "object",
  "properties": {
    "page_ref": { "type": "string",
      "description": "Notion page title, URL, or page id the user named. OMIT for vague references like \"that call\" / \"the meeting\" - the tool will resolve the most recently edited meeting-notes page and read its title back for confirmation." },
    "confirm":  { "type": "boolean",
      "description": "false (default) = dry-run preview: extract + speak \"Would create N tasks\" without writing. true = the user has confirmed; create the tasks tagged `auto`." }
  }
}
```

Description steers the model: *"Turn a Notion meeting/call into tasks. Use when the user says 'turn that call into tasks', 'make tasks from the partner sync', etc. ALWAYS dry-run first (confirm=false), speak the count back, and only re-call with confirm=true after the user says yes."*

### Page resolution: spoken ref vs. recent-page heuristic

Two branches inside `Run`:

1. **Explicit ref** (`page_ref != ""`): pass straight through as `aida tasks ingest --from-notion <ref>`. `NotionAdapter.Read` already resolves URL / page-id / title via `claude --print` (90s `notionFetchTimeout`), so no extra resolution is needed here.

2. **Bare "that call"** (`page_ref == ""`): run a short `claude --print` resolver to find the page, then feed its title to `--from-notion`. Reuse the binary-shell pattern, not a new adapter:

```go
const recentNotesPrompt = "Using your Notion tools, find the single most " +
    "recently edited meeting-notes / call-notes page. Return ONLY its exact " +
    "page title on one line - no commentary. If none, return 'NONE'."
```

   On `NONE` (or `claude` missing on PATH), return a spoken error: *"I couldn't find a recent meeting page, sir - name the page and I'll try again."* On success, the resolved title becomes the `--from-notion <title>` ref. The spoken dry-run reply leads with the title so the user can catch a wrong-page pick (*"From 'Q3 partner sync' I'd create 6 tasks. Create them?"*).

### Dry-run → confirm flow (rides the existing bare-wake "arm" loop)

The two-step is driven entirely by the `confirm` field plus the listener's existing arm-next-utterance behavior (`internal/jarvis/listener/listener.go`, `armed` flag, lines ~163/290/341) - no new state machine.

- **First call** (`confirm=false`): shell `aida tasks ingest --from-notion <ref> --dry-run`. `runTasksIngest` prints `Would create N task(s) from <src> (source-hash=<hash>): …` (tasks_ingest.go:432-448) without writing. Parse the count, speak it back. Critically, `--dry-run` also surfaces the idempotency note (`source-hash … matches M existing task(s); a real ingest would refuse without --force`, line 401) - so a repeat call is caught before any write.
- **User says "yes"** → the model re-invokes with `confirm=true`.
- **Second call** (`confirm=true`): shell the real ingest:

```
aida tasks ingest --from-notion <ref> --plan --yes --tag auto
```

  `--tag auto` is the Phase 4 handoff (every created task gets the literal `auto` tag, merged in at tasks_ingest.go:490-498). `--plan` creates the per-task exec-plan that Phases 2-3 append eval-runs / drafts to. `--yes` suppresses the interactive `[y/N]` auto-solve prompt (which would otherwise read `os.Stdin` and **fail closed** in a non-TTY voice context). Deliberately **no `--auto-solve`**: Jarvis only captures and tags in Phase 1; solving is the dispatcher's job, keeping the human-gate model intact.

Shell-out is identical in shape to `aidaQueryTool` (registry.go:388): `exec.CommandContext` + ANSI strip + `CombinedOutput`, but with a longer ceiling - the `claude --print` Notion fetch alone can take up to its 90s `notionFetchTimeout`, so budget **180s** (matching `aida_query`) and fire the shared `progressAck` ("One moment, sir.") at ~90s so the user isn't left in silence.

### Registration in `registry.go`

`ingest_notion_tasks` is a write-capable, jobs-adjacent capture tool, so register it inside the **`if jobsStore != nil`** block of `New(...)` (registry.go:104-110), alongside `job_start`/`job_status`/etc. Push-to-talk CLI invocations (`aida jarvis ask`) don't pass a `jobsStore` and correctly omit it:

```go
if jobsStore != nil {
    r.register(jobStartTool(jobsStore))
    // … existing job_* …
    r.register(ingestNotionTasksTool()) // NEW - Phase 1
}
```

It needs no `jobsStore` argument itself (it shells `aida`), but gating on the same `jobsStore != nil` keeps it to the always-on daemon (`aida serve`) where the Phase 4 dispatcher also lives.

### Jarvis-only recursion guard

`ingest_notion_tasks` MUST live **only** in `internal/jarvis/tools/registry.go` and **never** in `engine.BuildAgentToolsLazy` / `BuildAgentTools` (`internal/engine/agent_tools.go`). Verified: those builders register exactly source adapters (`buildSourceTool`), discovered MCP tools (`buildMCPTool`), and the two meta-tools `delegate_to_claude_code` + `ask_user` (agent_tools.go:41-57) - no path pulls in the voice registry. This is the same guard that keeps `aida_query` out of the agent palette (memory `feedback_no_mcp_query_tool`): the tool shells `aida tasks ingest`, whose `--auto-solve` itself spawns `aida --agent` subprocesses (tasks_ingest.go:707). Exposing it to a `--run-dir` agent would let a sub-agent trigger ingestion → auto-solve → more sub-agents, an unbounded recursion. Jarvis is the top-level agent, so subprocess fan-out from there is bounded and safe.

### Verification

End-to-end: with `aida serve` running, say *"Jarvis, turn that call into tasks."* Expected: recent-notes resolver speaks a title back; *"yes"* fires the real ingest; then `aida tasks --tag auto` shows the new cohort (each task also carries the `source-hash:<hash>` dedupe tag from tasks_ingest.go:498). Negative path: a second *"yes"* on the same page hits the source-hash idempotency check and is refused without `--force` - confirming captures aren't duplicated. Recursion guard: a quick grep asserting `ingest_notion_tasks` appears in `internal/jarvis/tools/` but in no `internal/engine/agent_tools*.go` file (extend the existing `TestBuildAgentToolsLazy_*` cases to assert the name is absent from the returned palette).

---

## Phase 2 - Code-correctness eval loop

> **Status:** shipped (`internal/eval/code.go` `CodeReviewer`; the iterate-until-
> green attempt loop in `internal/cli/loop.go` `processTask`). `agent.checks` as
> a config-file default did not ship; `--check` is CLI-only, repeatable.
>
> **Goal.** Turn `aida loop`'s single pass/fail `--check` into an *iterate-until-green* code-eval loop: a deterministic `CodeReviewer` runs build/test/lint, emits structured `eval.Issue`s on failure, and the loop re-spawns a fresh `aida --agent` with those failure messages folded into the prompt - repeating up to `--max-fix-iterations`, persisting every attempt as an `eval-runs/<id>.json`. This is the realization of the "Ralph Wiggum" iterate-until-clean variant that `internal/eval/reviewer.go:102-104` explicitly deferred.

### 2.1 New reviewer: `internal/eval/code.go`

The existing reviewers (`internal/eval/{citation,completeness,scope}.go`) grade a *synthesized answer string*. The `CodeReviewer` instead grades a *worktree* by shelling out commands and inspecting exit codes - but it implements the **same** `eval.Reviewer` interface (`Name() string`; `Review(ctx, ReviewInput) (*ReviewRecord, error)`), so it plugs into `eval.Loop` and `eval.AggregateVerdict` unchanged.

**`ReviewInput` extension (`internal/eval/reviewer.go`).** Today `ReviewInput` is `{Question, Answer string; Results []sources.SourceResult}`. Add two optional fields; per the type's existing contract ("reviewers must tolerate zero values for any field they do not use") the answer-grading reviewers ignore them:

```go
type ReviewInput struct {
    Question string
    Answer   string
    Results  []sources.SourceResult

    // Phase 2: code-correctness reviewers. Zero value = not a code review.
    WorkDir  string         // worktree the agent just edited; cmd.Dir for each check
    Commands []CodeCheck    // ordered build/test/lint commands; empty => CodeReviewer skips (returns nil)
}

type CodeCheck struct {
    Name string // "build" | "test" | "lint" - maps to Issue.Type "<name>-failure"
    Cmd  string // shell command, run via sh -c, cmd.Dir = WorkDir
}
```

**`CodeReviewer` type.** Mirrors `CitationReviewer`'s stateless shape (`NewCodeReviewer(checks []CodeCheck) *CodeReviewer`; `Name() string { return "code" }`). `Review` runs each `CodeCheck` in order with `cmd.Dir = in.WorkDir`, capturing combined stdout+stderr:

- **Empty `in.Commands` (or its own `checks`) → returns `(nil, nil)`** - `eval.Loop` treats a nil record as "skipped, no opinion", so adding `CodeReviewer` to a list is a no-op when no checks are configured.
- **Each non-zero exit → one `eval.Issue`**: `Issue{Type: "<name>-failure", Severity: "error", Message: <tail of combined output>, Anchor: <Cmd>}` where `Type` is `build-failure` / `test-failure` / `lint-failure`. The `Message` tail (last ~40 lines, capped like `citation.go`'s match handling) is what the loop feeds back to the next agent attempt - so it must be the *actual compiler/test diagnostic*, not a generic "build failed".
- **Verdict**: any failing check ⇒ `VerdictFail` with `Score = passed/total`; all pass ⇒ `VerdictPass`, `Score: 1.0`, `Rationale: "N/N checks green"`. This rides `AggregateVerdict`'s existing fail-loud reduction (`reviewer.go:126`) for free.

A `Review` whose *command* fails to launch (e.g. `make` not on PATH) is distinct from a non-zero exit - surface it as `Issue{Type:"check-unrunnable", Severity:"error"}` rather than swallowing, so a misconfigured `--check` doesn't masquerade as clean code.

**Constructor: `CodeReviewers(checks []CodeCheck) []Reviewer`.** Returns `[]Reviewer{NewCodeReviewer(checks)}` (a slice so the loop can later append the Phase 3 `tests-missing` warn reviewer to the same list). **`DefaultReviewers()` is left untouched** - the answer-grading triad stays the engine-synthesis default (`internal/engine/synthesizer.go:182`); code reviewers are loop-only and never bleed into `aida <query>` synthesis.

> **Verify (2.1).** `go test ./internal/eval/...`: a unit test points `CodeReviewer` at a temp dir with a deliberately broken `go build` and asserts `Verdict == VerdictFail`, one `Issue{Type:"build-failure"}`, and that `Issue.Message` contains the real compiler error substring.

### 2.2 Iterate-until-green loop in `internal/cli/loop.go`

Replace the single `runQualityGate(o.check)` boolean gate (currently `loop.go:153-159`, which parks `hold` on first failure) with a fix loop. The current flow spawns one agent then runs one gate; the new flow wraps spawn+gate in an attempt loop:

```
for attempt := 1; attempt <= o.maxFixIterations; attempt++ {
    prompt := buildLoopPrompt(task, preamble, o.checks)          // attempt 1
    if attempt > 1 { prompt = buildFixPrompt(task, lastIssues) } // re-spawn with failures
    runID := spawnLoopAgent(ctx, store, hmBin, profileName, task, prompt, o.perTask)

    in := eval.ReviewInput{WorkDir: worktreePath, Commands: o.checks}   // worktreePath from Phase 4; cwd until then
    records, _ := eval.Loop(ctx, eval.CodeReviewers(o.checks), in)
    verdict := eval.AggregateVerdict(records)

    // persist EVERY attempt - feeds Phase 7 brain analyze
    _ = brain.WriteEvalRun(cfg.BrainPath(), &brain.EvalRun{
        RunID: runID, Question: task.Title, Profile: profileName,
        AggregateVerdict: string(verdict), Reviewers: records,
    })

    if verdict == eval.VerdictPass { break }      // green → proceed to commit/complete
    lastIssues = collectIssues(records)           // flatten Issue.Message for the next prompt
}
if verdict != eval.VerdictPass {                  // exhausted budget still red
    brn.SetTaskStatus(task.Slug, brain.StatusHold)
    continue
}
```

Key points, all grounded in existing code:

- **Re-spawn with failure messages.** Each retry calls a new `buildFixPrompt(task, issues)` that embeds the prior attempt's `Issue.Message`s verbatim under a "## The previous attempt failed these checks" heading, instructing the fresh-context agent to fix exactly those. This reuses the existing `spawnLoopAgent` (which already does `store.Enqueue("loop", …)` → `exec.CommandContext` with `--agent --run-dir` → `Complete`/`Fail`), so each attempt is its own `kind=loop` job with its own run-dir and its own `RunID` - no shared state between attempts beyond the worktree and the brain.
- **`WriteEvalRun` per attempt, not just on the final verdict.** `brain.WriteEvalRun` keys on `RunID` (`eval_runs.go:48`, errors on empty `RunID`), and each `spawnLoopAgent` returns a distinct run id, so attempts never collide. The accumulated failed eval-runs are exactly what `brain.RecentFailedReviewsForEntities` + `eval.FailureBoosts` (Phase 7) and `aida brain analyze` consume - so the fix loop is what *generates* the self-improvement signal.
- **Exhaustion → park `hold`.** Preserves the current "leave for a human, don't re-spin" semantics (`loop.go:154-157`), just after N attempts instead of one.
- **`runQualityGate` is superseded.** The free-standing `os/exec` gate is removed; the `CodeReviewer` now owns command execution (so failures become structured `Issue`s rather than a discarded exit code).

### 2.3 New CLI / config surfaces (`loop.go`)

| Flag / key | Default | Notes |
|---|---|---|
| `--check` | `""` | **Now repeatable** (`StringArrayVar`, not `StringVar`) - each becomes a `CodeCheck`. Order preserved: build → test → lint. Existing single-`--check` invocations still parse. |
| `--max-fix-iterations` | `3` | Per-task retry budget. `1` reproduces today's single-pass behavior. |
| `agent.checks` (config.yaml) | unset | Optional default check set when no `--check` is passed. Parsed into `[]eval.CodeCheck`. |

`o.checks []eval.CodeCheck` replaces the scalar `o.check string` in `loopOpts`. `buildLoopPrompt`'s signature changes from `(task, preamble, check string)` to `(task, preamble string, checks []eval.CodeCheck)`, joining the command strings for the "these commands must pass before you finish" line it already emits (`loop.go:252-254`).

### 2.4 Wire `MemoryInstruction` into `agent.go` `variablePrompt`

`brain.MemoryInstruction` (`internal/brain/memory_types.go:42`) - durable rules like "always run `make install` after Go changes" - is a defined memory type with full persistence (`WriteMemory`, `ActiveMemoryByKey`, `ListMemory`) but is **never surfaced into the agent's instructions**. The build-after-changes discipline the code-eval loop depends on currently lives only in `CLAUDE.md`/auto-memory, which the spawned `aida --agent` subprocess does not read.

In `internal/cli/agent.go`, the `variablePrompt strings.Builder` block (currently `agent.go:343-395`) already injects tool patterns, `BRAIN CONTEXT`, multi-channel `RELEVANT MEMORY`, and `RESOLVED ENTITIES`. Add a dedicated, *unconditional-for-the-profile* instruction block fetched via `ListMemory`:

```go
if brn != nil {
    instrs, _ := brn.DB.ListMemory(brain.MemoryListOpts{
        Types: []brain.MemoryType{brain.MemoryInstruction}, Profile: profileName,
    })
    if len(instrs) > 0 {
        variablePrompt.WriteString("\nSTANDING INSTRUCTIONS (durable rules - follow on every task):\n")
        for _, m := range instrs { fmt.Fprintf(&variablePrompt, "- %s\n", m.Body) }
    }
}
```

This is distinct from the existing `SearchMulti` block (which is *similarity-gated* per question, `agent.go:370`): standing instructions like `build-after-changes` must apply to **every** code task regardless of semantic match, so they are listed deterministically (active, non-superseded, profile-scoped) rather than retrieved by embedding. Seed the rule once via `brn.WriteMemory(ctx, MemoryRecord{Type: MemoryInstruction, Key: "build-after-changes", Body: "After any Go change run `make build && make test` and fix failures before finishing."})`; supersession-by-key (`memory_types.go:59`) keeps it single-valued.

### 2.5 End-to-end verification

1. `go test ./internal/eval/...` and `go test ./internal/cli/ -run TestLoop` pass; `make build && make vet && make fmt` clean.
2. Seed an "add function + test" `auto` task whose acceptance check is `go test ./...`. Inject a deliberate failure (agent's first attempt omits the test). Run `aida loop --tag auto --check "go build ./..." --check "go test ./..." --max-fix-iterations 3`.
3. Assert: a *second* `kind=loop` job is spawned with the test-failure `Issue.Message` in its prompt; two `~/.aida/brain/eval-runs/<id>.json` files exist - the first `aggregate_verdict:"fail"` carrying a `test-failure` Issue, the second `"pass"`; the task ends `done` (not `hold`).
4. Negative: set `--max-fix-iterations 1` on the same failing task ⇒ task parks `hold`, exactly one eval-run, verdict `fail`.
5. `MemoryInstruction` wiring: after seeding `build-after-changes`, run `aida --agent --run-dir <dir> "<trivial code task>"`; assert via a `variablePrompt` unit test that the instruction body appears in `Agent.Instructions`.

> **Note on cost/budget (deferred to Phase 4).** Per-attempt cost is already emitted to the run-dir as `emitComplete`'s `cost_usd` (`internal/cli/agent_events.go:113-120`), but it is trapped in the subprocess; `cfg.Agent.MaxBudgetUSD` (`config.go:31`) exists and is still never read. The fix loop's per-attempt structure is where a budget stop will hook in (read `events.ndjson` `complete.cost_usd`, sum across attempts, abort when over `--max-budget-usd`) - but wiring that read-back is Phase 4 scope, not Phase 2. Phase 2 bounds iteration solely by `--max-fix-iterations` and the existing `--per-task-timeout` (`context.WithTimeout` in `spawnLoopAgent`, `loop.go:270`).

---

## Phase 3 - Autonomous PR creation + adversarial review swarm

> **Status:** shipped (`internal/cli/pr.go`; the review-panel + PR tail of
> `processTask` in `internal/cli/loop.go`). The PR is the *terminal artifact* of
> a loop iteration, gated behind an adversarial review panel. Merge stays
> entirely manual: the loop parks the task on `hold` (tagged `pr-open`) once the
> PR opens and never calls `gh pr merge` itself, so it never reaches past `gh pr
> create`; see the implementation-deviations note at the top of this document
> for how that differs from the Phase 5 auto-merge-after-approval flow
> originally sketched below.

### Goal

Replace the half-auto draft path with a real, un-gated PR - but only after a fan-out review panel votes majority-pass. Concretely:

1. After Phase 2's `CodeReviewer` is green, fan out **N reviewer sub-agents** with distinct lenses (correctness / security / test-coverage). Majority-pass required or the loop iterates (Phase 2's fix loop).
2. On panel pass, create `auto/<taskID>-<slug>` **branched off `origin/main`**, commit, push, and `gh pr create --fill --reviewer <user> --base main`.
3. Capture the PR URL into the manifest via `jobs.SetArtifactURL`, and append the PR + panel verdict to the exec-plan decision log via `brain.AppendDecisionLog`.

### What this un-gates

Today `appendDestinationInstructions` (`internal/cli/tasks_ingest.go:154`) hard-forbids `gh pr create`:

> *"Do NOT run `gh pr create` or push a branch."* - and full-auto is "gated behind a future `auto_create: true` flag on the partner destination" (`tasks_ingest.go:152-153`).

Phase 3 honors that gate rather than removing it: PR creation fires only when **either** `aida loop --pr` is passed **or** the resolved `jobs.Destination` carries `auto_create: true`. The auto-solve draft path (`tasks_ingest.go`, modal "Open PR") is untouched for the non-loop case.

### New file: `internal/cli/pr.go`

A small, dependency-light helper package-local to `cli`, callable from `loop.go`. No new external deps - shells `git` and `gh` exactly like `loopCommit` (`loop.go:312`) and `startPRWorkJob` (`internal/jarvis/tools/jobs.go`) already do.

```go
// PROptions controls autonomous PR creation. Zero value = no PR.
type PROptions struct {
    Enabled  bool     // --pr (xor loopOpts.commit)
    Base     string   // --base, default "main"
    Reviewers []string // --reviewer (repeatable); tags the human gate
    WorkDir  string   // the per-iteration worktree (Phase 4 supplies; Phase 3 = repo root)
}

// branchName derives the deterministic auto/ branch for a task.
//   #142 "Wire up the cron job" -> "auto/142-wire-up-the-cron-job"
func branchName(task *brain.TaskRecord) string {
    return fmt.Sprintf("auto/%d-%s", task.TaskID, slugify(task.Title))
}
```

`slugify` reuses the brain's existing slug rules (`internal/brain/tasks.go` already slugs titles for `<slug>.md`; expose/reuse rather than re-implement so `auto/<id>-<slug>` matches the task file slug exactly).

#### `createPR` - the branch/commit/push/open sequence

Critical ordering (from **Risks**: *"branch off `origin/main` (fetch first), not daemon cwd HEAD"*):

```go
func createPR(ctx context.Context, opts PROptions, task *brain.TaskRecord,
    runID, profileName string) (prURL string, err error) {

    git := func(args ...string) *exec.Cmd { c := exec.CommandContext(ctx, "git", args...); c.Dir = opts.WorkDir; return c }

    // 1. Always branch off a fresh origin base, never the daemon's cwd HEAD.
    if out, e := git("fetch", "origin", opts.Base).CombinedOutput(); e != nil {
        return "", fmt.Errorf("git fetch origin %s: %w\n%s", opts.Base, e, out)
    }
    br := branchName(task) // auto/<id>-<slug>
    if out, e := git("checkout", "-B", br, "origin/"+opts.Base).CombinedOutput(); e != nil {
        return "", fmt.Errorf("checkout -B %s: %w\n%s", br, e, out)
    }

    // 2. Stage + commit (conventional-commit shape, mirrors loopCommit).
    if out, e := git("add", "-A").CombinedOutput(); e != nil { return "", ... }
    msg := commitMessage(task) // see below
    if out, e := git("commit", "-F", "-" /*msg via stdin*/ ).CombinedOutput(); e != nil { return "", ... }

    // 3. Push the branch.
    if out, e := git("push", "-u", "origin", br).CombinedOutput(); e != nil {
        return "", fmt.Errorf("git push: %w\n%s", e, out)
    }

    // 4. Open the PR, tagging the human reviewer(s) - this IS the handoff to Phase 5.
    args := []string{"pr", "create", "--fill", "--base", opts.Base}
    for _, r := range opts.Reviewers { args = append(args, "--reviewer", r) }
    gh := exec.CommandContext(ctx, "gh", args...); gh.Dir = opts.WorkDir
    urlBytes, e := gh.Output()
    if e != nil { return "", fmt.Errorf("gh pr create: %w", e) }
    prURL = strings.TrimSpace(string(urlBytes))

    // 5. Persist the deliverable URL to the job manifest.
    if e := jobs.SetArtifactURL(profileName, runID, prURL); e != nil {
        ui.PrintVerbose("loop", "set-artifact-url failed: "+e.Error())
    }
    return prURL, nil
}
```

Notes grounded in the codebase:

- **`git checkout -B` (not `-b`)** so a re-run of the same task (idempotent dispatcher restart) re-points an existing `auto/<id>-<slug>` instead of erroring "branch exists."
- **`gh pr create --fill`** seeds title/body from the commit (which itself is derived from the task). The agent's `output.md` `## Summary` can override `--fill` with an explicit `--title`/`--body-file` in a follow-up, but `--fill` is the safe default and avoids the shell-escaping landmines called out in the user's global `CLAUDE.md` (no inline `-m`/`--body`; write to a temp file + `--body-file` when a rich body is needed).
- **Commit-message construction** uses a temp file + `git commit -F` (NEVER inline `-m`) per the project rule - `commitMessage(task)` writes `feat: #<id> <title>` plus a trailer `Task: #<id>` to `/tmp/aida-loop-commit-<runID>.txt`. (`loopCommit` currently uses inline `-m`; Phase 3 fixes that to comply with the heredoc/`-F` rule and to support multi-line bodies.)
- **`SetArtifactURL(profile, runID, url)`** is the real helper at `internal/jobs/manifest.go:271,279`; it sets `Manifest.ArtifactURL` (`manifest.go:79`, json `artifact_url`) and no-ops if unchanged. There is also a CLI shim `aida jobs set-artifact-url --run-id --url` already referenced from the auto-solve prompt (`tasks_ingest.go:183`).

### Adversarial review panel (gate *before* the PR)

This is the harness research's two-wave adversarial verify, and the **vertical swarm** from the plan ("Adversarial review panel: N reviewer sub-agents must majority-approve, else iterate"). It runs **after** Phase 2's `CodeReviewer` is green (build/test/lint already pass) - the panel reviews *judgment*, not compilation.

#### Reuse the existing `eval.Reviewer` contract

The panel slots directly into `internal/eval/reviewer.go`'s `Reviewer` interface (`Name()`, `Review(ctx, ReviewInput) (*ReviewRecord, error)`) and the `Loop` / `AggregateVerdict` reducers. **But** `eval.AggregateVerdict` (`reviewer.go:126`) is fail-loud (any `Fail` ⇒ `Fail`); a panel needs **majority-pass**, so Phase 3 adds a sibling reducer rather than changing the existing one (which the synthesizer depends on):

```go
// internal/eval/panel.go
// PanelVerdict votes: pass iff strictly more than half the records are VerdictPass.
// Warn counts as not-pass for the gate (conservative). Empty = fail (no panel ran).
func PanelVerdict(records []ReviewRecord) Verdict {
    if len(records) == 0 { return VerdictFail }
    pass := 0
    for _, r := range records { if r.Verdict == VerdictPass { pass++ } }
    if pass*2 > len(records) { return VerdictPass }
    return VerdictFail
}
```

#### Three lenses, run as isolated sub-agents

Each reviewer is an LLM sub-agent spawned via `delegate_to_claude_code` (`internal/engine/worktree.go`) - already worktree-isolated (`spawnDelegateWorktree` → `git worktree add --detach`, `worktree.go:43`; cleanup at `:62`) so a reviewer agent cannot mutate the candidate worktree. Each gets the diff (via `captureWorktreeDiff(worktreeDir)`, `worktree.go:89`) as `ReviewInput`, with a lens-specific system prompt:

| Lens (`Reviewer.Name()`) | Concern | `Issue.Type` on fail |
|---|---|---|
| `panel-correctness` | Does the diff actually implement the task? Edge cases, error paths, regressions. | `correctness` |
| `panel-security` | Injection, secret handling, unsafe `exec`/`sh -c`, auth. Mirrors OWASP ASI lens. | `security` |
| `panel-test-coverage` | Are the changed code paths tested? See "tests if applicable" below. | `missing-test` |

The fan-out runs concurrently with an `errgroup` + semaphore (same pattern as the engine executor, limit 5), N controlled by `--review-panel` (default 3). Each sub-agent returns a `*ReviewRecord{Verdict, Score, Issues, Rationale}`. The slice is reduced by `PanelVerdict`.

```
            CodeReviewer green (Phase 2)
                      │
        ┌─────────────┼─────────────┐         (delegate_to_claude_code,
        ▼             ▼             ▼           worktree-isolated, concurrent)
  panel-correctness  panel-security  panel-test-coverage
        │             │             │
        └─────────────┼─────────────┘
                      ▼
              eval.Loop → []ReviewRecord
                      ▼
              eval.PanelVerdict
              ├── pass  → createPR()  ──► gh pr create --reviewer <human>
              └── fail  → re-spawn fix agent (Phase 2 loop) with the panel's
                          Issue.Messages, up to --max-fix-iterations, then park `hold`
```

#### Persist the panel verdict

Every panel run writes a `brain.EvalRun` via `brain.WriteEvalRun(brainPath, &EvalRun{RunID, Question: task.Title, AggregateVerdict: string(PanelVerdict(recs)), Reviewers: recs})` → `~/.aida/brain/eval-runs/<runID>.json` (`internal/brain/eval_runs.go:48`). This is the same store Phase 7 mines via `ListRecentEvalRuns` / `RecentFailedReviewsForEntities`, and that `eval.FailureBoosts` (`internal/eval/boosts.go:94`) turns into routing signal - so a recurring `panel-security` fail on a given entity compounds into the brain, closing the self-improvement loop.

### "Tests if applicable" enforcement

Three layers, weakest to strongest:

1. **Prompt directive** - `buildLoopPrompt` (`loop.go:239`) gains a line: *"If your change touches non-test `*.go` files, add or update a `*_test.go` covering the new behavior."*
2. **CodeReviewer (Phase 2) forces `make test` green** - the agent cannot finish with a broken or absent build. This is the hard floor.
3. **`tests-missing` warn reviewer** - a *deterministic* (non-LLM) `eval.Reviewer` added to the panel: scans `captureWorktreeDiff` output; if the diff touches a non-`_test.go` `*.go` file but adds **zero** `_test.go` lines, emits `ReviewRecord{Verdict: VerdictWarn, Issues: [{Type:"tests-missing", Severity:"warn"}]}`. Warn does not by itself fail `PanelVerdict` (it isn't a `Fail`), but it does prevent a unanimous pass and surfaces in the `EvalRun` audit. Promote to `VerdictFail` via config `agent.pr.require_tests: true`.

### New flags + config

On `aida loop` (`internal/cli/loop.go newLoopCmd`):

| Flag | Type / default | Behavior |
|---|---|---|
| `--pr` | bool, false | Enable autonomous PR creation. **XOR with `--commit`** (`loopOpts.commit`, `loop.go:70`) - `--pr` implies a commit + push + PR; setting both errors in `RunE`. |
| `--reviewer` | `StringSlice`, nil | Repeatable; each becomes a `gh pr create --reviewer <r>`. This is the only thing that tags the human for Phase 5. |
| `--base` | string, `"main"` | Branch-off + `gh pr create --base` target. |
| `--review-panel` | int, `3` | N reviewer sub-agents in the adversarial panel. `0` disables the panel (CodeReviewer-only gate). |

Config (read by `runLoop`, defaults overridden by flags):

```yaml
agent:
  pr:
    base: main
    reviewer: your-github-handle   # string or list
    require_tests: false          # promote tests-missing warn -> fail
```

And the destination un-gate - add to `internal/config/partners.go DestinationConfig` (`partners.go:38`) and mirror onto `internal/jobs/manifest.go Destination` (`manifest.go:104`, which today has only `Type/Repo/BranchPrefix/Channel`):

```go
// config.DestinationConfig
AutoCreate bool `yaml:"auto_create,omitempty"` // honor full-auto gh pr create
// jobs.Destination
AutoCreate bool `json:"auto_create,omitempty"`
```

`appendDestinationInstructions` (`tasks_ingest.go:154`) is updated so that when `dest.AutoCreate` is true it **stops** forbidding `gh pr create` and instead instructs the agent to defer PR creation to the loop's `createPR` (the agent still must not call `gh pr create` itself - single source of truth for branch naming + reviewer tagging stays in `pr.go`). The reused `Destination.Repo` (`manifest.go:106`) supplies `gh --repo` when the worktree's `origin` is not the target repo. `Destination.BranchPrefix` (`manifest.go:107`) overrides the `auto/` prefix when set.

### Integration into `runLoop`

Phase 3 changes the post-gate tail of `runLoop` (`loop.go:153-174`). The current shape is `runQualityGate → if --commit loopCommit → CompleteTask → distillLearning`. The new shape (with Phase 2's iterate-until-green already in place):

```
green (CodeReviewer) ─► reviewPanel(ctx, panelN, worktree)
                        │
   PanelVerdict fail ───┴─► Phase 2 fix loop (re-spawn w/ Issue.Messages)
                            exhausted ─► SetTaskStatus(StatusHold)   // park, do NOT PR
   PanelVerdict pass ─────► if opts.pr.Enabled || dest.AutoCreate:
                                createPR(...) ─► SetArtifactURL ─► AppendDecisionLog("[pr] <url>")
                            else if --commit: loopCommit  (legacy path, unchanged)
                            ─► CompleteTask ─► distillLearning
```

`AppendDecisionLog(brainPath, task.PlanID, "loop-pr", "<url> + panel verdict")` reuses the same writer the auto-solve path uses (`tasks_ingest.go:734,757`), so `aida tasks drafts` and the exec-plan show the PR + panel result.

> **Important:** Phase 3 deliberately stops at `gh pr create`. It never marks the task `done` as "merged" - `CompleteTask` here means "PR opened, awaiting human." The job does **not** transition to a merge state; that is Phase 5's `awaiting_approval` + `request_approval` gate. The reviewer tag on the PR is the cross-channel signal that Jarvis speaks ("PR 583 ready for review") on next wake.

### Verification

Throwaway repo (so a stray push can't hit a real remote):

```bash
aida loop --tag auto --pr \
  --reviewer your-github-handle \
  --base main \
  --review-panel 3 \
  --check "make test"          # Phase 2 gate

# assertions:
git -C <worktree> branch --show-current        # => auto/<id>-<slug>
gh pr view <n> --json reviewRequests           # => your-github-handle tagged
cat ~/.aida/jobs/<profile>/runs/<runID>/manifest.json | jq .artifact_url   # => the PR URL
ls ~/.aida/brain/eval-runs/<runID>.json      # => panel ReviewRecords persisted
gh pr view <n> --json mergedAt                 # => null (NEVER merged in Phase 3)
```

Negative tests:

- **Panel-fail does not PR.** Force one reviewer lens to `Fail` (e.g. inject a deliberate vuln); confirm no branch is pushed (`git ls-remote --heads origin 'auto/*'` empty), the task lands `hold`, and `events.ndjson` shows no `gh pr create`.
- **`--pr` + `--commit` together errors** in `RunE` before any agent spawns (XOR guard).
- **Branch base is `origin/main`, not cwd HEAD** - make a dirty commit on the daemon's checkout, run the loop, assert the new `auto/` branch's merge-base is `origin/main` (`git merge-base auto/<id>-<slug> origin/main` == `origin/main` tip), not the local HEAD.
- **`auto_create` gate** - with neither `--pr` nor `dest.AutoCreate`, confirm the legacy `appendDestinationInstructions` draft path runs and `gh pr create` is never invoked.

**Verified against source:** `jobs.SetArtifactURL`/`Manifest.ArtifactURL` (`internal/jobs/manifest.go:79,271`), `appendDestinationInstructions` forbid-clause (`internal/cli/tasks_ingest.go:154-186`), `auto_create:true` as the documented future gate (`tasks_ingest.go:152-153`), `eval.Reviewer`/`Loop`/`AggregateVerdict` (`internal/eval/reviewer.go:87,105,126`), `brain.WriteEvalRun` + `eval-runs/<id>.json` (`internal/brain/eval_runs.go:48`), `delegate_to_claude_code` worktree isolation + `captureWorktreeDiff` (`internal/engine/worktree.go:43,62,89`), `loopCommit`/`buildLoopPrompt`/`runQualityGate` tail of `runLoop` (`internal/cli/loop.go:153-174,239,294,312`), `DestinationConfig`/`Destination` field sets that **lack** `auto_create` today (`internal/config/partners.go:38`, `internal/jobs/manifest.go:104`). The `--commit` flag and `loopOpts.commit` that `--pr` is XOR'd against are at `loop.go:70`.

---

## Phase 4 - Dispatcher (daemon, per-iteration worktree, budget, concurrency)

> **Status:** shipped: shared `internal/worktree/` package; `--worktree`,
> `--concurrency`, `--max-budget-usd`, `--max-consecutive-failures`, `--daemon`,
> `--poll` on `internal/cli/loop.go`; `aida serve --loop` in
> `internal/cli/serve.go`. One difference from the draft below: per-iteration
> worktree isolation is opt-in via `--worktree` (or anything implying it), not
> automatic for every task as §4.2 assumes.

Phase 4 turns the one-shot `aida loop` (`internal/cli/loop.go`) into the system's **dispatcher**: a continuous, concurrent, budget-bounded driver that drains the `auto` task cohort, each task in its own throwaway worktree (and, with [Phase 6](#phase-6--docker-sandboxes-integration), its own Docker Sandbox). It closes the two `loop.go` TODOs verbatim - *"worktree isolation per iteration"* (`loop.go:33`) and *"token/cost budget stop"* - and reuses the worktree + cleanup machinery already proven by `pr_work`.

This phase ships in four pieces, all in `internal/cli/loop.go`, `internal/cli/serve.go`, and a new leaf package `internal/worktree`.

### 4.1 Shared `internal/worktree` package (promote out of voice tools)

Today the worktree logic lives in `internal/jarvis/tools/jobs.go` (`startPRWorkJob` for create, `removeWorktree` for teardown) and is duplicated, inlined, in `serve.go` (`cleanupWorktreePath`, `serve.go:317`). The duplication exists specifically so `serve.go` stays "free of voice-tool imports" (its own comment, `serve.go:325-326`). The dispatcher needs the same primitives from a *third* caller (`loop.go`), so we promote them into a leaf package both can import without pulling in `internal/jarvis`.

**Create `internal/worktree/worktree.go`:**

```go
package worktree

// CreateWorktree adds a detached git worktree at dir, branched off baseRef
// (e.g. "origin/main"), and returns dir. Caller fetches first. If baseRef
// is "", falls back to the repo HEAD (current --detach behavior).
func CreateWorktree(repoRoot, dir, baseRef string) (string, error)

// RemoveWorktree force-removes a worktree, locating the parent repo via
// `git -C <dir> rev-parse --git-common-dir`, with a 30s cap and an
// os.RemoveAll fallback - lifted verbatim from tools.removeWorktree
// (jobs.go:239-288), which already has the common-dir walk + timeout.
func RemoveWorktree(dir string) error

// RepoRoot returns the toplevel of the git repo containing cwd
// (promoted from tools.gitRepoRoot, jobs.go:226).
func RepoRoot(cwd string) (string, error)
```

Then:
- `internal/jarvis/tools/jobs.go` `startPRWorkJob` replaces its inline `git worktree add --detach` + unwind with `worktree.CreateWorktree(repoRoot, worktreeDir, "")` (PR work keeps HEAD-detach then `gh pr checkout`, so `baseRef=""`); `removeWorktree` becomes a thin alias to `worktree.RemoveWorktree`.
- `serve.go` `cleanupWorktreePath` (`serve.go:317`) collapses to `worktree.RemoveWorktree(path)`, deleting the inlined copy and the `exec`/`filepath`/`strings` walk it carries.

Standardize the base directory on `~/.aida/worktrees/<profile>/` - the existing `pr_work` path (`config.Dir()/worktrees/<profile>/...`, `jobs.go:150-153`). Loop worktrees use the naming `loop-<taskID>-<runID>` to mirror `pr-<num>-<runID>`.

**Verification note:** `CreateWorktree`/`RemoveWorktree` must be a behavior-preserving extraction - `go test ./internal/jarvis/tools/...` (the `extractPRNumber` test stays green) plus a new `internal/worktree` round-trip test (`CreateWorktree` then `RemoveWorktree` leaves `git worktree list` clean).

### 4.2 Per-iteration detached worktree off `origin/main`

`runLoop` (`loop.go:77`) currently spawns the agent in the dispatcher's own cwd (`spawnLoopAgent`, `loop.go:263`, sets no `cmd.Dir`), so every iteration mutates the live checkout and `loopCommit` (`loop.go:312`) commits onto whatever branch the daemon was launched on - the exact hazard the plan's "Worktree base branch" risk calls out (branch off `origin/main`, **not** daemon cwd HEAD).

Each iteration now gets its own isolated tree:

```
# inside runLoop, before spawnLoopAgent, per iteration:
git -C <repoRoot> fetch origin main           # freshest base
worktree.CreateWorktree(repoRoot, dir, "origin/main")   # detached @ origin/main
store.SetWorktreePath(runID, dir)             # daemon cleanup hook keys off this
# spawnLoopAgent: cmd.Dir = dir  (NEW - currently unset)
# on terminal: worktree.RemoveWorktree(dir)
```

Concretely in `loop.go`:
- Add a `worktreePath` to the per-iteration scope; create it after `pickNextTask` (`loop.go:109`) and before `spawnLoopAgent`.
- `spawnLoopAgent` gains a `workDir string` param and sets `cmd.Dir = workDir` (the one missing line vs. `pr_work`, which already does `cmd.Dir = worktreeDir`, `jobs.go:188`). It already `Enqueue`s a `kind="loop"` job (`loop.go:264`) and computes `runDir` - after enqueue, call `store.SetWorktreePath(job.RunID, workDir)` (`store.go:303`) so the manifest carries `WorktreePath` (`manifest.go:98`).
- Cleanup: in **daemon mode** the existing `scanJobsForNotifications` teardown fires (`serve.go:306-308`, `IsTerminalState(j.State) && j.WorktreePath != ""` → `go cleanupWorktreePath`). In **one-shot mode** (no daemon watcher running) `runLoop` must remove the worktree itself in a `defer`, because nothing else will. Guard with a `daemon bool` so we don't double-remove.
- `loopCommit` (`loop.go:312`) runs inside the worktree via `git -C <workDir>` instead of bare `git` (today it shells `git add -A` / `git commit` against cwd). With Phase 3 this becomes the PR push path; here it just needs to be worktree-scoped.

**Verification note:** `aida loop --tag auto --concurrency 2 --max-iterations 4` then `git worktree list` shows zero leftover `loop-*` trees on completion; each iteration's commit lands on a branch off `origin/main`, not the daemon's launch branch.

### 4.3 `--daemon` continuous mode, hosted behind `aida serve --loop`

`runLoop`'s bounded `for i := 1; i <= o.maxIterations` (`loop.go:108`) and its `pickNextTask` returning `nil` → `COMPLETE` (`loop.go:113-116`) are correct for a one-shot drain but wrong for a long-lived dispatcher. Continuous mode inverts the exit condition: when `pickNextTask` returns `nil`, **sleep and re-poll** instead of returning.

New field on `loopOpts` (`loop.go:40`) and flag:

```go
daemon       bool          // continuous: sleep on empty queue instead of exiting
pollInterval time.Duration // re-poll cadence when the auto queue is empty
```
```go
cmd.Flags().BoolVar(&o.daemon, "daemon", false, "run continuously: sleep when no matching task, re-poll the queue")
cmd.Flags().DurationVar(&o.pollInterval, "poll-interval", 30*time.Second, "queue re-poll cadence in --daemon mode")
```

In `runLoop`, when `o.daemon` and `task == nil`: `select { case <-ctx.Done(): return; case <-time.After(o.pollInterval): continue }` (and `--max-iterations` becomes advisory / `0 = unbounded` in daemon mode). `maxIterations`'s "stop after N" semantics are preserved for the one-shot path.

**Hosting in the daemon.** The dispatcher should run in the same process as Jarvis + the job watcher, so one `aida serve` instance is the whole agent-of-agents. Add to `newServeCmd` (`serve.go:32`):

```go
var enableLoop bool
cmd.Flags().BoolVar(&enableLoop, "loop", false, "also run the autonomous-loop dispatcher (drains --tag auto tasks)")
// optionally: --loop-tag (default "auto"), --loop-concurrency, --loop-max-budget-usd
```

Then in `runHTTPDaemon` (`serve.go:115`), alongside the existing `go watchJobsForNotifications(...)` (`serve.go:197`), wire:

```go
if enableLoop {
    go runLoop(ctx, loopOpts{tags: []string{"auto"}, daemon: true, pr: true, concurrency: loopConcurrency, ...})
}
```

This requires threading a `context.Context` into `runLoop` (today it builds its own `context.Background()`, `loop.go:78`) so daemon shutdown (`<-ctx.Done()`, `serve.go:232`) cancels the dispatcher cleanly - both the poll-sleep and the per-iteration `context.WithTimeout` (`loop.go:270`) become children of the daemon ctx. The dispatcher opens its own `jobs.Store` via `jobs.Open(profileName)` (`store.go:65`) or shares the daemon's `jobsStore` handle (`serve.go:122`); sharing avoids a second SQLite connection and is preferred. The same `store` powers `aida jobs list` showing `kind=loop` rows.

**Verification note:** `aida serve --loop` with one `auto` task → `aida jobs list` shows a `kind=loop` row transitioning queued→running→done; with an empty `auto` cohort the dispatcher idles (no busy-spin - confirm CPU near zero across a `poll-interval`).

### 4.4 `--concurrency N` horizontal fan-out

The loop today is strictly serial (one `task` per `i`, `loop.go:108-175`). Horizontal fan-out runs N tasks at once, **each in its own worktree** - which is exactly Docker Sandboxes' one-sandbox-per-workspace rule (Phase 6): per-task worktree = per-task workspace = one sandbox.

New flag/field:

```go
concurrency  int   // number of auto tasks worked in parallel
cmd.Flags().IntVar(&o.concurrency, "concurrency", 1, "work up to N matching tasks in parallel, each in its own worktree")
```

Implement with `golang.org/x/sync/errgroup` (already a project dep, used by the engine fan-out) + a semaphore of size `o.concurrency`. The critical correctness constraint: `pickNextTask` (`loop.go:184`) currently selects by status and immediately the caller flips it to `StatusInProgress` (`loop.go:129`). With concurrency, two goroutines must not claim the same task. Two options, in order of preference:

1. **Claim-on-pick under a mutex:** wrap `pickNextTask` + `SetTaskStatus(slug, StatusInProgress)` in a dispatcher-held `sync.Mutex` so the pick→claim is atomic. `pickNextTask` already filters to `StatusOpen + StatusInProgress` (`loop.go:185-186`), so a just-claimed task is excluded from the next concurrent pick only after the status write commits - the mutex closes that window.
2. Reuse the jobs queue's own `Claim` semantics (`store.go`) if task-level claiming proves racy under SQLite write contention.

Each worker goroutine owns the full per-iteration lifecycle from 4.2 (create worktree off `origin/main` → `SetWorktreePath` → spawn agent with `cmd.Dir` → quality gate → commit/PR → `CompleteTask` → `distillLearning`, `loop.go:135-174`). The `--per-task-timeout` (`loop.go:72`, default `450s`) already scopes each agent via `context.WithTimeout` (`loop.go:270`); under concurrency it bounds each goroutine independently.

**Sandbox coupling (forward ref to Phase 6):** the per-worktree spawn in `spawnLoopAgent` is the single wrap point - `cmd := exec.CommandContext(...)` becomes `cmd := sandbox.Wrap(exec.CommandContext(...), policy)` so each of the N concurrent agents runs in its own microVM bound to its own worktree workspace. No additional concurrency plumbing is needed: N worktrees → N sandboxes falls out for free.

**Verification note:** `aida loop --tag auto --pr --concurrency 2 --max-iterations 4` → two tasks `running` simultaneously (`aida jobs list` shows two `kind=loop` rows in `running`), each with a distinct `WorktreePath`; `git worktree list` shows two `loop-*` trees during the run, zero after.

### 4.5 Budget stop - wire `cfg.Agent.MaxBudgetUSD` via `events.ndjson` cost read-back

`config.AgentYAMLConfig.MaxBudgetUSD` (`config.go:31`, `yaml:"max_budget_usd"`) **exists but is never read** - `agent.go` reads `cfg.Agent.MaxTurns` (`agent.go:401`) and `cfg.Agent.Model` (`agent.go:415`) but never the budget. The cost itself is "trapped in the subprocess": the agent computes `costUSD := engine.CostFromUsage(...)` (`agent.go:477`) and writes it only to the run-dir as a `complete` event - `runDirSink.emitComplete` emits `{"type":"complete","data":{"turns":3,"cost_usd":0.012}}` (`agent_events.go:113-121`, key `cost_usd`). The dispatcher's parent process never sees it directly because `spawnLoopAgent` discards stdout/stderr (`cmd.Stdout = nil`, `loop.go:278-279`).

So the budget stop reads cost **back out of the run-dir** after each iteration:

```go
// internal/cli/loop.go - new helper
// readRunCost parses the trailing `complete` event from a run-dir's
// events.ndjson and returns data.cost_usd. Returns 0 if absent (run
// still in flight, or failed before emitComplete).
func readRunCost(profile, runID string) float64 {
    data, err := os.ReadFile(jobs.EventsPath(profile, runID)) // paths.go:57
    if err != nil { return 0 }
    var cost float64
    for _, line := range bytes.Split(data, []byte("\n")) {
        var ev struct {
            Type string `json:"type"`
            Data struct{ CostUSD float64 `json:"cost_usd"` } `json:"data"`
        }
        if json.Unmarshal(line, &ev) == nil && ev.Type == "complete" {
            cost = ev.Data.CostUSD // last complete wins
        }
    }
    return cost
}
```

`runLoop` keeps a running `spentUSD` accumulator. After each `spawnLoopAgent` returns (it returns the `runID`, `loop.go:263`), add `readRunCost(profileName, runID)`; before picking the next task, if `maxBudget > 0 && spentUSD >= maxBudget`, stop the dispatcher (one-shot: return; daemon: stop *spawning new* iterations but let in-flight ones drain, then idle). Resolution order: `--max-budget-usd` flag > `cfg.Agent.MaxBudgetUSD` > `0` (unbounded).

```go
maxBudget    float64  // loopOpts field
maxConsecFailures int
cmd.Flags().Float64Var(&o.maxBudget, "max-budget-usd", 0, "stop spawning new iterations once cumulative agent cost reaches this (0 = unbounded)")
```

Under `--concurrency`, the accumulator is mutex-guarded (same lock as the claim in 4.4); the check is best-effort post-hoc per the plan ("Cost trapped in subprocess - build the events.ndjson cost read-back before relying on --max-budget-usd"), so a small overshoot of up to `concurrency × per-task cost` is expected and acceptable.

**Verification note:** `agent.max_budget_usd: 0.01` in config (or `--max-budget-usd 0.01`) → the loop stops after the first iteration whose cumulative `cost_usd` crosses the cap; confirm via `aida jobs list` that no further `kind=loop` jobs are enqueued and the dispatcher logs the budget stop.

### 4.6 `--max-consecutive-failures` circuit breaker

The current loop, on agent failure or quality-gate failure, parks the task on `StatusHold` and `continue`s (`loop.go:144-159`) - so a systemically broken environment (e.g. `make test` failing for an unrelated reason) will silently churn through and park the *entire* `auto` cohort. Add a consecutive-failure counter:

```go
maxConsecFailures int // 0 = unbounded
cmd.Flags().IntVar(&o.maxConsecFailures, "max-consecutive-failures", 3, "abort the dispatcher after N consecutive failed/parked iterations (0 = unbounded)")
```

`runLoop` increments a `consecFailures` counter on each `runErr != nil` (`loop.go:140`) and each `!runQualityGate(...)` park (`loop.go:153`), and resets it to `0` after a `CompleteTask` (`loop.go:170`). When `maxConsecFailures > 0 && consecFailures >= maxConsecFailures`, abort: in one-shot mode return an error; in daemon mode log loudly, stop the dispatcher goroutine, and **leave the rest of `aida serve` running** so Jarvis can still report status. Under `--concurrency`, the counter is the same mutex-guarded shared state; a single completion resets it.

**Verification note:** seed three `auto` tasks against a forced-failing `--check` (e.g. `--check "exit 1"`) with `--max-consecutive-failures 2` → the dispatcher parks two tasks on `hold` then aborts before touching the third; `aida tasks list --tag auto` shows the third still `open`.

### Interaction with the daemon notification switch (cross-ref Phase 5)

Loop jobs are `kind="loop"` and flow through the **existing** `scanJobsForNotifications` switch (`serve.go:281-290`), which handles `StateAwaitingInput`/`StateDone`/`StateFailed` and **silently drops unknown states via `default: continue`** (`serve.go:288-289`). That is correct for Phase 4 (loop jobs only ever reach the known states), but it is the precise reason [Phase 5](#phase-5--hitl-approval-gate-awaiting_approval)'s `StateAwaitingApproval` must add an explicit `case` to this switch - flagged here so the dispatcher's job rows don't get a notification path the approval gate later assumes exists. The terminal-state worktree cleanup in that same switch (`serve.go:306-308`) already covers loop worktrees the moment `WorktreePath` is set in 4.2 - no new cleanup wiring needed in daemon mode.

### New surface summary

| Surface | Location | Default |
|---|---|---|
| `internal/worktree.CreateWorktree/RemoveWorktree/RepoRoot` | new `internal/worktree/worktree.go` | - |
| `aida loop --daemon` | `loop.go` `loopOpts.daemon` | `false` |
| `aida loop --poll-interval` | `loop.go` `loopOpts.pollInterval` | `30s` |
| `aida loop --concurrency N` | `loop.go` `loopOpts.concurrency` | `1` |
| `aida loop --max-budget-usd` | `loop.go` `loopOpts.maxBudget` (falls back to `cfg.Agent.MaxBudgetUSD`) | `0` (unbounded) |
| `aida loop --max-consecutive-failures` | `loop.go` `loopOpts.maxConsecFailures` | `3` |
| `aida serve --loop` (+ `--loop-tag`, `--loop-concurrency`) | `serve.go` `newServeCmd` / `runHTTPDaemon` | off |
| `spawnLoopAgent(... workDir, ...)` sets `cmd.Dir` | `loop.go:263` | - |
| `store.SetWorktreePath(runID, dir)` per iteration | `loop.go` (existing `store.go:303`) | - |
| `readRunCost(profile, runID)` (parses `events.ndjson` `complete.cost_usd`) | `loop.go` (reads `jobs.EventsPath`, `paths.go:57`) | - |

**Critical files touched:** `internal/cli/loop.go` (dispatcher logic), `internal/cli/serve.go` (`newServeCmd`, `runHTTPDaemon` host point at `serve.go:197`), new `internal/worktree/worktree.go` (extraction from `internal/jarvis/tools/jobs.go:226-288`). Reads: `internal/jobs/manifest.go` (`WorktreePath`), `internal/jobs/store.go` (`SetWorktreePath`/`Enqueue`), `internal/jobs/paths.go` (`EventsPath`/`RunDir`/`OutputPath`), `internal/config/config.go` (`AgentYAMLConfig.MaxBudgetUSD`), `internal/cli/agent_events.go` (`emitComplete` cost shape).

---

## Phase 5 - HITL approval gate (`awaiting_approval`)

> **Status:** shipped, as general-purpose infrastructure: `StateAwaitingApproval`
> and `RequestApproval`/`Approve`/`RejectApproval` in `internal/jobs/`,
> `buildRequestApprovalTool` in `internal/cli/agent.go`, voice `approve_job`/
> `reject_job` in `internal/jarvis/tools/approval.go`, and `aida jobs approve
> <ref>` / `aida jobs reject <ref> --reason <text>` (note: `--reason`, not
> `--because` as drafted below). Any `--agent --run-dir` task can pause on this
> gate. What did **not** ship: the autonomous loop's own `--pr` path calling
> it; see the implementation-deviations note at the top of this document.

### Goal

The pipeline must be able to **stop one step short of every irreversible action** - merge-to-`main`, `slack_send_message`, `notion-update-page`, Gmail send - and park the job in a new, *non-terminal* state until a human says go. This is the second of the two human gates (the first being ingest-confirm; there is no third). Today the only mid-run pause is `awaiting_input` (`internal/jobs/manifest.go:15`, driven by `buildAskUserTool` in `internal/cli/agent.go:761`), which is semantically "the agent needs information." We need a state that says "the agent is finished and is *requesting permission to act*" - a different verb, a different voice utterance, and a different set of CLI/voice tools (`approve`/`reject`, not free-text `job_send_input`).

We reuse `buildAskUserTool`'s polling pattern, `notify.Enqueue`/`DrainOnNextWake`, `resolveJobRef`/`extractPRNumber` (`internal/jarvis/tools/jobs.go:438`,`:97`), and the `confirm_always`/`confirmFn` machinery (`internal/engine/agent_tools.go:157-183`).

### 5.1 New non-terminal state - `StateAwaitingApproval`

`internal/jobs/manifest.go`:

```go
const (
    StateQueued           = "queued"
    StateRunning          = "running"
    StateAwaitingInput    = "awaiting_input"
    StateAwaitingApproval = "awaiting_approval" // NEW - Phase 5
    StateDone             = "done"
    StateFailed           = "failed"
)
```

`IsTerminalState` is **unchanged** - `awaiting_approval` is *not* terminal (`done`/`failed` only), so the worktree/sandbox cleanup in `scanJobsForNotifications` must not fire on it. `IsValidState` (`manifest.go:29`) **must** be extended or `List(ListOpts{State: "awaiting_approval"})` rejects the filter at `store.go:147` (`return nil, fmt.Errorf("List: invalid state %q", ...)`) - this is load-bearing because the daemon scan filters on it:

```go
func IsValidState(s string) bool {
    switch s {
    case StateQueued, StateRunning, StateAwaitingInput,
        StateAwaitingApproval, StateDone, StateFailed:
        return true
    }
    return false
}
```

> **Verification note:** add a table test in `internal/jobs/manifest_test.go` asserting `IsValidState("awaiting_approval") == true` **and** `IsTerminalState("awaiting_approval") == false`. The second assertion is the safety property - a regression there would auto-clean the worktree out from under an unmerged PR.

### 5.2 Manifest schema bump `2 → 3` (additive, zero-value safe)

`manifestVersion = 3` in `manifest.go:42`. The bump is purely documentary/additive - Go's `json.Unmarshal` defaults missing keys to zero values, so v2 manifests (and v1) still decode, exactly as the existing comment at `manifest.go:39-41` and the `manifestToJob` note at `:238-240` promise. Two new optional fields on `Manifest`:

```go
// ApprovalAction names the irreversible operation the agent is
// requesting permission to perform: "merge" | "send" | "writeback".
// Set by RequestApproval; consumed by the voice notice + the
// approve/reject tools. Empty unless State == StateAwaitingApproval
// (or a terminal state that passed through it).
ApprovalAction string `json:"approval_action,omitempty"`

// ApprovalPayload is a human-readable one-liner describing exactly
// what will happen on approval (e.g. "gh pr merge --squash 583 into
// main", "slack #partners: 'GMV report ready'"). Surfaced verbatim
// in the voice utterance and `aida jobs list`. Never a secret.
ApprovalPayload string `json:"approval_payload,omitempty"`
```

Mirror both onto `jobs.Job` (`internal/jobs/store.go:23`) and wire through `jobToManifest` (`manifest.go:207`) and `manifestToJob` (`:241`), exactly parallel to how `AwaitingPrompt`/`WorktreePath` already round-trip. **Coordination note:** [Phase 6](#phase-6--docker-sandboxes-integration) adds four more fields in the *same* v2→v3 bump (`SpendCapUSD`/`TimeoutSec`/`ToolAllowlist`/`SandboxTier`) - the two phases share the single version increment.

> **Dual-machine note (per plan risk):** additive only. `jobs.db` is per-machine; the manifest JSON is the source of truth and is what syncs. v3 manifests opened by a v2 binary lose only the new keys on rewrite - acceptable, and the brain-repo sync never carries `jobs.db`.

### 5.3 SQL columns - idempotent ALTERs

`internal/jobs/db.go`: add to the `CREATE TABLE` block (for fresh DBs) **and** to the idempotent ALTER loop (for existing DBs - the loop already swallows "duplicate column name" via `isDuplicateColumnErr`):

```go
// in CREATE TABLE:
approval_action  TEXT NOT NULL DEFAULT '',
approval_payload TEXT NOT NULL DEFAULT '',

// appended to the ALTER slice:
`ALTER TABLE jobs ADD COLUMN approval_action  TEXT NOT NULL DEFAULT ''`,
`ALTER TABLE jobs ADD COLUMN approval_payload TEXT NOT NULL DEFAULT ''`,
```

Extend the column lists in `Get` (`store.go:127`), `List` (`store.go:166`), `insertRow` (`store.go:350`), and the `scanJob` destinations (`store.go:375`) - all four must stay in lockstep or the `Scan` arity mismatches at runtime.

### 5.4 Store transitions - `RequestApproval` / `Approve` / `RejectApproval`

`internal/jobs/store.go`, modeled exactly on `Pause`/`Resume` (`store.go:251`,`:279`) with the same disk-first ordering (manifest write, then SQL row):

```go
// RequestApproval transitions a running job to awaiting_approval,
// records what the agent wants to do, and clears NotifiedAt so the
// daemon speaks a fresh "ready for approval" notice. The agent-side
// caller (the request_approval tool) blocks polling approval.txt
// after this returns. Mirrors Pause's ordering.
func (s *Store) RequestApproval(runID, action, payload string) error

// Approve flips awaiting_approval -> running so the agent unblocks and
// performs the irreversible action it was holding. Mirror of Resume;
// leaves ApprovalAction/ApprovalPayload set for the audit trail.
func (s *Store) Approve(runID string) error

// RejectApproval transitions awaiting_approval -> failed with a
// reason, so the agent's poll loop observes a terminal manifest and
// aborts WITHOUT acting. (Terminal -> worktree/sandbox cleanup fires
// naturally via scanJobsForNotifications.)
func (s *Store) RejectApproval(runID, reason string) error
```

`RequestApproval` clears `NotifiedAt = ""` on the *transition into* `awaiting_approval`, exactly as `Pause` does at `store.go:261`, so a job that requests approval twice in one run re-notifies each time. `RejectApproval` routes through the existing `Fail` path (`store.go:207`) so the manifest lands in `failed` with the reason in `Error` - the agent's poll loop (below) reads that as "abort."

### 5.5 `buildRequestApprovalTool` - the agent-side gate

`internal/cli/agent.go`, sibling to `buildAskUserTool` and registered in the same daemon-managed block (`agent.go:223-237`, only when `runDirSink != nil`). **Jarvis-only exclusion does NOT apply** - this is an *engine-side* agent tool (like `ask_user`), never exposed to the Jarvis voice LLM; the recursion guard (`feedback_no_mcp_query_tool`) is about `aida_query`, not this.

```go
func buildRequestApprovalTool(sink *runDirSink, store *jobs.Store, runID, runDir string) engine.AgentTool {
    return engine.AgentTool{
        Name: "request_approval",
        Description: "Pause and request HUMAN APPROVAL before an irreversible action " +
            "(merging a PR to main, sending a Slack/email message, writing back to Notion). " +
            "You MUST call this and receive an explicit approval before doing any such action. " +
            "Provide `action` (merge|send|writeback) and `payload` (a one-line description of " +
            "exactly what will happen). Blocks until the user approves or rejects via voice " +
            "(\"approve PR 583\") or `aida jobs approve <ref>`. On approval the tool returns " +
            "\"approved\" and you proceed; on rejection the run is failed and you must STOP.",
        InputSchema: /* {action: enum[merge,send,writeback], payload: string}, required both */,
        Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
            // 1. store.RequestApproval(runID, action, payload)  (best-effort, like ask_user)
            // 2. sink.emitAwaitingApproval(action, payload)     (new run-dir event)
            // 3. poll <run-dir>/approval.txt every 2s (same ticker shape as ask_user)
            //    - file body "approve"          -> store.Approve(runID); return "approved", nil
            //    - file body "reject: <reason>" -> return "", fmt.Errorf("approval REJECTED: %s", reason)
            //    - ctx.Done()                    -> return "", ctx.Err()
            //    delete approval.txt after read (anti-stale, like input.txt at agent.go:837)
        },
    }
}
```

The poll loop is a near-clone of `buildAskUserTool` (`agent.go:816-853`) but reads `approval.txt` instead of `input.txt`, and the **default outcome of a missing/empty file is to keep waiting, never to proceed** - fail-safe. A new event kind `awaiting_approval` is added to `internal/cli/agent_events.go` alongside the existing `EventKindInputReceived` (`agent_events.go:147`) so `events.ndjson` records the gate for the negative-path verification ("`gh pr merge` never appears before an `approval_granted` event").

### 5.6 The REQUIRED new case in `serve.go` - the highest-risk fix

`scanJobsForNotifications` (`internal/cli/serve.go:268`) switches on `j.State` and its **`default: continue` (`serve.go:288`) silently drops any unknown state** - so without this change a job parked in `awaiting_approval` would *never* be announced, and the human gate would hang forever. Add the case:

```go
switch j.State {
case jobs.StateAwaitingInput:
    msg = formatAwaitingNotice(&j)
case jobs.StateAwaitingApproval:           // NEW - Phase 5 (without this it is dropped silently)
    msg = formatApprovalNotice(&j)         // "Sir, PR 583 is ready for review. Approve to merge into main?"
case jobs.StateDone:
    msg = formatDoneNotice(&j)
case jobs.StateFailed:
    msg = formatFailedNotice(&j)
default:
    continue
}
```

`formatApprovalNotice` (new, beside `formatAwaitingNotice`) builds the utterance from `ApprovalAction` + `ApprovalPayload` + `ArtifactURL` (the PR link, set earlier via `SetArtifactURL`). The existing `NotifiedAt`-idempotency (`serve.go:277`) and `MarkNotified` stamp (`serve.go:299`) carry over unchanged, so the approval prompt is spoken exactly once per request across daemon restarts.

The terminal-cleanup hook at `serve.go:306` (`IsTerminalState(j.State) && j.WorktreePath != ""`) is **deliberately untouched**: because `awaiting_approval` is non-terminal it does *not* match, so the worktree/sandbox survives across the human gate and is only torn down once `Approve→done` or `RejectApproval→failed` lands it in a terminal state on a later tick.

> **Verification note:** add a `scanJobsForNotifications` unit test (the function was split out for exactly this - `serve.go:266-267`) seeding one `awaiting_approval` job and asserting (a) `notifier` received one utterance, (b) `MarkNotified` was stamped, (c) the worktree was **not** cleaned.

### 5.7 Human surfaces - voice `approve_job`/`reject_job` + `aida jobs approve|reject`

**Voice** - `internal/jarvis/tools/approval.go` (new), registered in `internal/jarvis/tools/registry.go` inside the existing `jobsStore != nil` block next to `job_send_input` (`tools/jobs.go:468`). These are Jarvis-only (top-level agent; safe). Built on `resolveJobRef` (`tools/jobs.go:438`) so "approve PR 583" resolves via `extractPRNumber`/substring just like `job_send_input` does:

| Tool | Behavior |
|------|----------|
| `approve_job {ref}` | `resolveJobRef`; assert `State == StateAwaitingApproval` (mirror the guard at `tools/jobs.go:506`); atomic-write `approve` to `<run-dir>/approval.txt` (same tmp-then-`Rename` pattern as `tools/jobs.go:510-517`); echo-confirm `"Approved. Merging now, sir."` |
| `reject_job {ref, reason}` | same resolve+guard; write `reject: <reason>`; reason **required** (matches the `jarvis_thumbs_down` reason-required convention) |

**CLI** - `internal/cli/jobs.go`: `newJobsApproveCmd()` / `newJobsRejectCmd()` added to the `newJobsCmd()` tree (`jobs.go:25-44`, beside `newJobsSetArtifactURLCmd`). `aida jobs approve <ref>` and `aida jobs reject <ref> --because <reason>` (note: `--because`, per `feedback_thumbs_because`, **not** `-m`). Both resolve the active profile via `cfg.ActiveProfileConfig` like every other subcommand and write `approval.txt` through the same atomic helper. These are the typed equivalent of the voice path for when Jarvis isn't listening.

> The agent's poll loop is the single consumer of `approval.txt`; the voice tool and the CLI are two producers writing the same sentinel file - exactly the `input.txt` topology (`tasks_web.go:729` web producer + `job_send_input` voice producer + `buildAskUserTool` consumer), so no new IPC surface is introduced.

### 5.8 Belt-and-suspenders: `confirm_always` fail-closed guarantee

`request_approval` is the *intended* gate, but a misbehaving agent could try to call `gh pr merge` / `slack_send_message` directly. Three independent layers must all be crossed for an irreversible action to fire - defense in depth:

1. **Application gate (this phase):** merge/send/writeback routes through `request_approval`; nothing else marks the job approvable.
2. **`confirm_always` fail-closed (this phase):** add `gh pr merge`, `slack_send_message`, `notion-update-page`, and Gmail-send patterns to `GuardrailsConfig.ConfirmAlways` (`config.go:113`). `buildMCPTool` gates `mt.Destructive || confirm-pattern` through `confirmFn` (`internal/engine/agent_tools.go:171-183`). In a **detached `--run-dir` job there is no interactive stdin**, so `confirmFn` reads EOF and **returns false → the tool is denied** ("FAILS CLOSED"). This is the property we *want*: an agent that skips `request_approval` and reaches for a destructive MCP tool is hard-denied by the guardrail, not silently allowed.
3. **Network egress (Phase 6, the third layer):** the Docker-Sandbox host-side proxy's Locked-down/Balanced egress allowlist means even a denied-tool bypass can't reach the merge/send endpoint.

> **Audit caveat (per plan risk):** the same fail-closed `confirmFn` must **not** block benign in-worktree work. Verify the `confirm_always` patterns match only `gh pr merge` / send / writeback verbs, not `gh pr create`/`gh pr view`/file edits - otherwise the loop deadlocks on every routine step. Phase 3 keeps `gh pr create` *un-gated*; only `gh pr merge` is in `confirm_always`.

### State-machine diagram

```
                    request_approval tool
   ┌──────────┐   (store.RequestApproval)   ┌────────────────────┐
   │ running  │ ──────────────────────────▶ │ awaiting_approval  │  (non-terminal)
   └──────────┘                             └─────────┬──────────┘
        ▲                                             │
        │ store.Approve                               │  daemon: scanJobsForNotifications
        │ (approval.txt = "approve")                  │  case StateAwaitingApproval ─▶ notify.Enqueue
        │                                             │  (DrainOnNextWake, MarkNotified once)
        │                                             │  WORKTREE/SANDBOX NOT cleaned (non-terminal)
        │                                             ▼
        │                                   ┌───────────────────┐
        └────────────────── approve_job ───│  human gate (HITL) │
                            aida jobs approve └─────────┬─────────┘
                                                      │ reject_job / aida jobs reject --because
                                                      │ (approval.txt = "reject: <reason>")
   ┌──────────┐  store.Approve ▶ running ─▶ performs   │  store.RejectApproval (-> Fail)
   │  done    │  irreversible action, then Complete    ▼
   └──────────┘                              ┌──────────┐
        ▲                                    │  failed  │  (terminal -> worktree/sandbox cleanup)
        └── merge/send actually happens      └──────────┘
            ONLY after approve unblocks the   action NEVER performed (agent poll sees
            agent's request_approval poll      terminal manifest -> aborts)
```

### Cross-references (verified real paths/symbols)

- State enum + `IsValidState`/`IsTerminalState` + `manifestVersion`: `internal/jobs/manifest.go:13-42`, WriteManifest, job↔manifest folds (`:207`/`:241`).
- Store transitions to clone: `Pause`/`Resume` `internal/jobs/store.go:251`/`:279`; `Fail` `:207`; column lists `:127`/`:166`/`:350`/`:375`.
- SQL DDL + idempotent ALTER loop: `internal/jobs/db.go` (`isDuplicateColumnErr`).
- Agent-side poll pattern to clone: `buildAskUserTool` `internal/cli/agent.go:761-856`; daemon-only registration `agent.go:223-237`.
- Daemon switch to extend: `scanJobsForNotifications` `internal/cli/serve.go:268-311` (the `default: continue` at `:288`; cleanup hook `:306`).
- Voice producers/consumer topology: `job_send_input` `internal/jarvis/tools/jobs.go:468-521`; `resolveJobRef` `:438`; web producer `internal/cli/tasks_web.go:729-783`.
- CLI command tree: `newJobsCmd` `internal/cli/jobs.go:25-44`.
- Guardrail fail-closed: `GuardrailsConfig.ConfirmAlways` `internal/config/config.go:113`; `buildMCPTool` confirm gate `internal/engine/agent_tools.go:157-183`.

---

## Phase 6 - Docker Sandboxes integration

> **Status:** shipped, with real deviations from the design below; see the
> implementation-deviations note at the top of this document for the full
> list. In short: `internal/sandbox/sandbox.go` (single file, not three), tiers
> `none`/`docker` only (Seatbelt dropped), driven by `sbx create`/`sbx
> exec`/`sbx rm` (not `docker sandbox run`/`docker exec`), and **no egress
> allowlist or secret injection was built**; the isolation is filesystem +
> process only. `Policy.ProvisionBin` (cross-compiling a linux/arm64 `aida` into
> the microVM, since the macOS binary can't run there) is real and is the one
> piece of Phase 6 that goes beyond what this draft anticipated.

**Goal.** Today `aida --agent` runs unconfined: `startGenericAgentJob`/`startPRWorkJob` (`internal/jarvis/tools/jobs.go`) and `spawnLoopAgent` (`internal/cli/loop.go:263`) all do `exec.Command(hmBin, "--agent", "--run-dir", runDir, prompt)` straight on the host with `cmd.Env = os.Environ()` - raw `ANTHROPIC_API_KEY`/`VOYAGE_API_KEY`/`gh` token in the agent's environment, full filesystem and network reach. Phase 6 wraps that spawn in a **Docker Sandbox** (microVM, own kernel, host-side egress proxy) so the network-deny layer completes the merge/send gate started in Phase 5, and so Phase 4's `--concurrency N` is genuinely isolated. **Decision:** Docker Sandboxes (`sbx`); Seatbelt `sandbox-exec` is a degraded fallback when Docker is unavailable; Apple `container` and NVIDIA OpenShell rejected.

This phase is **wrap-only**: it changes *how* the existing `aida --agent` subprocess is launched, not the agent loop itself. The agent inside the sandbox still writes `events.ndjson`/`output.md`/`manifest.json` to the same `--run-dir`, because that dir is on the worktree volume mounted RW into the sandbox.

### Why per-task worktree = per-workspace = one sandbox

Docker Sandboxes enforce **one sandbox per workspace**. Phase 4's per-iteration detached worktree under `~/.aida/worktrees/<profile>/...` (promoted into the shared `internal/worktree` helper) *is* the workspace, so the one-sandbox-per-workspace rule is satisfied for free and N concurrent loop tasks become N independent sandboxes with no extra bookkeeping. The sandbox lifetime is bound 1:1 to the worktree lifetime, so teardown hooks into the existing worktree-cleanup goroutine rather than introducing a parallel reaper.

### New package: `internal/sandbox/`

**`internal/sandbox/sandbox.go`** - the wrap contract, tier-agnostic:

```go
package sandbox

// Tier selects the confinement backend. Mirrors config.SandboxConfig.Tier
// and the per-job Manifest.SandboxTier field.
type Tier string

const (
    TierDocker   Tier = "docker"   // Docker Sandboxes (sbx), default
    TierSeatbelt Tier = "seatbelt" // macOS sandbox-exec fallback
    TierNone     Tier = "none"     // unconfined (current behavior; tests / no-Docker hosts)
)

// Policy is resolved per job from config defaults + manifest overrides.
type Policy struct {
    Tier           Tier
    Workspace      string   // abs path to the worktree, mounted RW at /work
    RunID          string   // names the sandbox: aida-<runID>
    MemoryMB       int      // --memory; 0 => sbx default (50% host)
    EgressAllow    []string // hostnames the proxy permits; empty => Locked-down
    InjectSecrets  []string // env var names the host proxy injects as headers
}

// Wrap rewraps cmd so it executes inside the sandbox described by p.
// It MUST be called before cmd.Start()/cmd.Run(). On TierNone it is a
// no-op (returns cmd unchanged) so existing call sites degrade cleanly.
// Returns a teardown func the caller defers / hooks into worktree cleanup.
func Wrap(cmd *exec.Cmd, p Policy) (wrapped *exec.Cmd, teardown func() error, err error)
```

**`internal/sandbox/docker.go`** - `Wrap` for `TierDocker`. Because `aida --agent` is a *custom binary*, not a built-in `sbx run <agent>`, it uses the documented custom path:

```
docker sandbox run -d --name aida-<runID> --memory <MemoryMB> <workspace>
docker exec aida-<runID> sh -c "cd /work && aida --agent --run-dir /work/.aida-run <prompt>"
```

Concretely, `Wrap` does NOT pass the original `hmBin` host path through; it rewrites `cmd` so `cmd.Path`/`cmd.Args` become the `docker exec ...` invocation, preserves the caller's `cmd.Stdout/Stderr` (still `nil` for detached jobs), and strips the secret env vars from `cmd.Env` (the host-side proxy injects them - the VM never holds raw secrets). The `-d` (detached) container is the long-lived workspace; teardown is `docker sandbox rm aida-<runID>` (equivalently `sbx rm`). Implementation notes:

- **Run-dir inside the sandbox.** The agent's `--run-dir` must resolve to a path on the mounted workspace so `events.ndjson`/`output.md`/`manifest.json` survive teardown and remain readable by the host cost read-back (below) and Phase 5's `input.txt`/`approval.txt` polling. Standardize on `<worktree>/.aida-run` mapped to `/work/.aida-run`; callers compute the run-dir from the worktree, not from `jobs.RunDir(...)` under `~/.aida/jobs/...` (that host path is not visible inside the VM).
- **One sandbox per workspace** is asserted: `Wrap` errors if a sandbox named `aida-<runID>` already exists for a different workspace.
- **Verification note:** there is no `sbx`/`docker sandbox` dependency in the repo today - this is net-new. Pin the CLI (`brew install docker/tap/sbx`; record the pinned version in this `SPEC.md` and assert it at startup) because `sbx` is experimental. Probe availability with `exec.LookPath("docker")` + `docker sandbox --help`; on failure fall through to the Seatbelt tier.

**`internal/sandbox/seatbelt.go`** - `Wrap` for `TierSeatbelt`. Rewraps `cmd` as `sandbox-exec -f <profile.sb> aida --agent ...` against a generated profile that allows read/write only under the worktree and `~/.aida/jobs/<profile>/runs/<runID>`, denies writes elsewhere, and (best-effort) restricts network. This is the same mechanism Codex/Claude Code use locally; it is *not* a microVM (shared kernel, no egress proxy, so secrets stay in-env) and is explicitly the **degraded** path used only when Docker/`sbx` is unavailable. The loop still runs confined rather than fully open.

### Egress policy (the third gate layer)

Phase 5 gives two belt-and-suspenders layers (route merge/send through `request_approval`; `confirm_always` patterns that fail closed on stdin in a detached job). The sandbox network policy is the **third**: even if the agent tries to call `gh pr merge` or a Slack/Gmail/Notion send before approval, the host-side egress proxy denies the connection.

Two profiles, configured via `sbx policy` + a checked-in policy file (referenced from `sandbox.egress_allowlist` in config):

| Profile | Use | Allowlist |
|---|---|---|
| **Balanced** | implementation iterations (build/test/lint, brain recall, PR push) | Anthropic API, GitHub (`api.github.com`, `github.com`, `*.githubusercontent.com`), Voyage (embeddings for `loopRecall`), Notion host (ingest only) |
| **Locked-down** | default for any step not yet approved; review-panel sub-agents | empty allowlist (deny all egress); the only outbound is the Anthropic header-injected proxy hop |

The host-side proxy injects auth headers (the env vars named in `Policy.InjectSecrets`, default `ANTHROPIC_API_KEY`/`VOYAGE_API_KEY`/GitHub token), so the microVM never holds raw secrets - this is *why* `docker.go`'s `Wrap` strips those names from `cmd.Env`. Note `gh pr create` (Phase 3) needs GitHub egress + token, so the Balanced allowlist must include GitHub; `gh pr merge` reaching the network is acceptable *only because* the Phase 5 approval gate prevents the agent from issuing it pre-approval - the allowlist is not the merge gate, the approval state is.

### Per-job governance on the manifest

`internal/jobs/{manifest,store,db}.go` - extend `Manifest` (additive, zero-value safe; this is the same v2→3 bump [Phase 5](#phase-5--hitl-approval-gate-awaiting_approval) makes, so the two phases coordinate the single version increment):

```go
// Manifest, new fields (all omitempty; v2 manifests decode as zero):
SpendCapUSD   float64  `json:"spend_cap_usd,omitempty"`   // 0 => fall back to config default
TimeoutSec    int      `json:"timeout_sec,omitempty"`     // 0 => config default
ToolAllowlist []string `json:"tool_allowlist,omitempty"`  // nil => all tools
SandboxTier   string   `json:"sandbox_tier,omitempty"`    // "docker"|"seatbelt"|"none"
```

Plumb through `jobToManifest`/`manifestToJob` (the `Job` struct gains the same four fields) and the SQL columns in `db.go` (new columns are nullable; `Reindex` repopulates from the JSON source of truth, so dual-machine `jobs.db` regeneration is safe). No new state and no new `Store` method is strictly required - these are set at `Enqueue` time by the dispatcher; a thin `Store.SetGovernance(runID, ...)` is optional sugar.

**Enforcement** (in `internal/cli/loop.go`, the Phase 4 dispatcher - *not* in the agent subprocess, which can't be trusted to enforce its own cap):

- **`ToolAllowlist`** - passed via env (`AIDA_TOOL_ALLOWLIST=...`) into the spawned agent; `agent.go` filters `engine.BuildAgentToolsLazy(...)` by the allowlist before constructing the exec plan. nil/empty = current behavior (all tools). This lets a Locked-down review-panel sub-agent be denied `delegate_to_claude_code`/MCP send tools at the *tool layer* in addition to the network layer.
- **`TimeoutSec`** - the dispatcher already wraps the spawn in `context.WithTimeout` (`runCtx, cancel := context.WithTimeout(ctx, perTask)` at `loop.go:270`); `TimeoutSec` overrides the `--per-task-timeout` default per job.
- **`SpendCapUSD`** - wires `cfg.Agent.MaxBudgetUSD` (exists at `config.go:31`, currently never read). Cost is trapped in the subprocess; read it back from the run-dir after the iteration: parse `events.ndjson` for the final `{"type":"complete","data":{"cost_usd":...}}` line (emitted by `runDirSink.emitComplete` → `agent_events.go:113`, field `cost_usd`). The dispatcher accumulates `cost_usd` across iterations and stops the loop when the running total exceeds `SpendCapUSD`/`MaxBudgetUSD`. (Phase 4 §4.5 already owns the per-iteration `readRunCost`; Phase 6 only consumes it as a cap.)

### Config surface

`internal/config/config.go` - new top-level `SandboxConfig` (peer of `AgentYAMLConfig`):

```go
type SandboxConfig struct {
    Tier              string   `yaml:"tier,omitempty"`                // "docker" (default) | "seatbelt" | "none"
    EgressAllowlist   []string `yaml:"egress_allowlist,omitempty"`    // hostnames for the Balanced profile
    MemoryMB          int      `yaml:"memory,omitempty"`              // --memory; 0 => sbx default
    DefaultSpendCapUSD float64 `yaml:"default_spend_cap_usd,omitempty"`
    DefaultTimeoutSec  int     `yaml:"default_timeout_sec,omitempty"`
}
```

added to `Config` as `Sandbox SandboxConfig `yaml:"sandbox,omitempty"``. The dispatcher resolves each job's effective `Policy` as manifest-override-then-config-default. Default tier is `docker`; an unset config block + a host without `sbx` resolves to `seatbelt`, and `none` only via explicit opt-out (tests).

### Teardown hooked into worktree cleanup

The daemon already removes pr_work worktrees on terminal transitions: `scanJobsForNotifications` (`serve.go:306-308`) fires `go cleanupWorktreePath(j.WorktreePath)` when `jobs.IsTerminalState(j.State) && j.WorktreePath != ""`. Phase 6 makes that hook tier-aware: before removing the worktree, tear down its sandbox.

```go
// serve.go scanJobsForNotifications, terminal branch:
if jobs.IsTerminalState(j.State) && j.WorktreePath != "" {
    go func(runID, tier, path string) {
        _ = sandbox.Teardown(tier, runID) // docker sandbox rm aida-<runID>; no-op for seatbelt/none
        cleanupWorktreePath(path)         // existing git worktree remove --force
    }(j.RunID, j.SandboxTier, j.WorktreePath)
}
```

`sandbox.Teardown` is best-effort and idempotent (matching `removeWorktree`'s "already gone = nil" contract in `jobs.go:243`): `docker sandbox rm aida-<runID>` for `TierDocker`, no-op otherwise. Because the sandbox is keyed on `RunID` and the worktree is the workspace, the two always tear down together - no orphaned microVMs.

**Verification note:** Phase 6 keys teardown off `IsTerminalState`, which covers `done`/`failed`, so a job that reaches `awaiting_approval` (Phase 5) correctly does **not** tear down its sandbox/worktree - the sandbox must persist while parked for approval so the eventual approved merge runs in the same confined workspace.

### Caveats (baked into the spec)

- **Perf overhead.** microVM startup adds latency (~seconds) per iteration on top of the 8–15s `aida_query`-class round-trips; acceptable for background `loop` jobs, not for interactive Jarvis turns. Sandbox wrapping is applied to `loop`/`pr_work`/`agent` job spawns only, never to the in-process voice tool dispatch.
- **Commit-signing inside the sandbox is unsolved.** The VM cannot reach the host's signing key/agent. Resolution: commit **unsigned** inside the sandbox (Phase 3's `loopCommit`/`pr.go` push), then **sign on host rebase** or **sign at merge** - i.e. the human-gated `gh pr merge` (Phase 5) runs on the host, so the signed commit is produced at the approval boundary, not inside the VM. Document this so a "why is the PR commit unsigned" question has an answer.
- **Experimental `sbx`.** Pin the version; assert it at startup; degrade to Seatbelt (and ultimately `TierNone` with a loud warning) rather than failing the loop when the pinned `sbx` is missing.
- **Recursion guard unaffected.** Sandboxing wraps the *engine-side* `aida --agent` subprocess; the Jarvis-only ingest/approval voice tools never enter `BuildAgentToolsLazy`, so the sandbox sees no `aida_query`/job-spawn tools that could recurse.

### Verification matrix

1. `sbx ls` (or `docker sandbox ls`) shows a `aida-<runID>` sandbox for each in-flight loop iteration during `aida loop --tag auto --pr --concurrency 2`.
2. Inside the sandbox: a write to `/work/...` (the worktree) succeeds; a write to `$HOME` or `/etc` outside `/work` is denied.
3. With the **Balanced** allowlist: a request to the Anthropic API succeeds; a request to a non-allowlisted host (e.g. `example.com`) is denied by the proxy.
4. Secrets: `printenv ANTHROPIC_API_KEY` inside the VM is empty, yet the agent's LLM calls succeed (proxy header injection works).
5. `agent.max_budget_usd: 0.01` (or manifest `spend_cap_usd`) stops the loop after the first iteration whose accumulated `events.ndjson` `cost_usd` exceeds the cap.
6. On terminal state the sandbox is removed (`sbx ls` clean) and `git worktree list` is clean - teardown and worktree cleanup fired together.
7. Docker-absent host: tier resolves to `seatbelt`, the loop still completes a task confined, `sbx ls` is never invoked.

---

## Phase 7 - brain analyze auto-proposals (follow-on)

> **Status:** shipped in part (`internal/cli/brain_analyze.go`). The mine →
> draft → print/`--out` path is real: `--propose`, `--out <path>`,
> `--min-failures` (default `2`), `--limit`, `--source`, `--ndjson`. The
> `--pr`/`--dry-run`/`--reviewer`/`--base` flags below, and the "opens its own
> gated PR through Phase 3 + Phase 5" path, did **not** ship; there is no `--pr`
> flag on this command today. Turning a proposal into a merged change is still
> a manual step for a human.

**The self-improvement meta-loop.** Phases 1–6 turn a call into a merged PR; Phase 7 turns the *exhaust* of that pipeline - the `eval-runs/<id>.json` records every synthesis writes - back into committed regression tests and routing rules. Where `aida loop` improves the *codebase*, `aida brain analyze --propose` improves *aida's own judgment* (its golden suite + its router priors), and ships those improvements through the exact same review-gated, human-before-merge machinery built in Phases 3 and 5.

This is explicitly a **follow-on** (it needs Phases 3 + 5). It does not block the end-to-end walkthrough; it runs *periodically* against accumulated eval signal (walkthrough step 9).

### Gap

`runBrainAnalyze` (`internal/cli/brain_analyze.go`) is **read-only** - it walks `brain.ListRecentEvalRuns`, groups via `eval.ParseFailures`, and prints a per-source summary table. It surfaces "snowflake has 5 missing-source failures over 50 runs" but a human still has to translate that into a `GoldenQuery` or a source-config edit by hand. Phase 7 closes that last manual hop.

### Surfaces

Extend the existing `newBrainAnalyzeCmd()` (do **not** add a sibling command - keep the read-only summary as the default `RunE`):

```
aida brain analyze --propose [--dry-run] [--pr]
                 [--reviewer <user>] [--base main]
                 [--min-failures N] [--limit N] [--source <name>]
```

| Flag | Default | Behavior |
|------|---------|----------|
| `--propose` | off | Switch from summary table to proposal mode. Without it, today's behavior is unchanged. |
| `--dry-run` | - | Print proposals (new golden YAML + routing diff) to stdout, write nothing. The natural first call. |
| `--pr` | off | Open a review-gated PR with the proposals (xor with `--dry-run`). Reuses the Phase 3 helper + Phase 5 gate. |
| `--min-failures` | 3 | Only propose for a `source × issue-type` bucket that recurs at least N times - single failures are noise (mirrors `FailureBoosts`' "single hint vs repeated signal" design, `boosts.go:11-13`). |
| `--reviewer`, `--base` | from `agent.pr.*` config | Passed straight through to the Phase 3 PR helper. |
| `--limit`, `--source` | reused as-is | Same scan-window / single-source filters already on the command. |

`--propose` is mutually exclusive with `--ndjson`; `--dry-run` xor `--pr` (return an error if both set, matching the `--commit` xor `--pr` pattern from Phase 3/4).

### Proposal generation (deterministic mine → LLM draft)

Two proposal *kinds*, both derived from the same scan loop `runBrainAnalyze` already runs (`ListRecentEvalRuns` → filter `AggregateVerdict == "fail"` → `eval.ParseFailures(r.Reviewers)`):

**1. Routing-boost diffs (deterministic, no LLM).** Aggregate failed `[]eval.ReviewRecord` across the scan window and run the *existing* `eval.FailureBoosts(records)` to get `map[source]int` adjustments (already clamped to `±MaxAdjustment = 50`). Each non-zero entry above the `--min-failures` threshold becomes a proposed source-config edit:

```
# proposed: snowflake recurs as missing-source 6x over last 50 runs (+25 router prior)
# source: ~/.aida/library/sources/snowflake.yaml
- entities: [pine-hollow, first-chair]
+ entities: [pine-hollow, first-chair, gmv]   # ← drafted from the failed questions
```

Routing boosts are a *deterministic* mine - `FailureBoosts` is already the production router path (`brain.RecentFailedReviewsForEntities` → `FailureBoosts` feeds the LLM router's `±50`). Phase 7 just makes the recurring ones durable as config rather than re-derived per query.

**2. Golden regression tests (one LLM call per bucket).** For each recurring failure bucket, pull the representative failed `EvalRun.Question` and `EvalRun.RunID`, and draft a `GoldenQuery` for `testdata/golden-queries.yaml` (the suite `newGoldenAddCmd` already appends to). The LLM call fills the assertion bundle from the *fix signal*:

- `missing-source` (boost +25) → the synth wanted a source the planner didn't route to → propose `SourcesMustInclude: [<source>]`.
- `missing-citation` (demote −25) → source had thin data for this question shape → propose `SourcesMustNotInclude: [<source>]`.
- `scope-mismatch` → propose `EntitiesMustInclude: [<dropped-token>]`.
- Always set `Judge` (the natural-language criterion) over the legacy `AnswerMustMatch` regex - matches `GoldenExpected`'s own guidance (`golden.go:55-62`).

```yaml
# proposed golden query (from failed run 20260615-091233-ab12)
- id: auto-snowflake-missing-source-ab12
  question: "what's pine-hollow's GMV last quarter"
  expected:
    sources_must_include: [snowflake]
    judge: "answer cites a dollar GMV figure grounded in snowflake source data"
```

Reuse `GoldenQuery` / `GoldenExpected` verbatim - never invent a parallel schema. The drafting LLM call mirrors `llmJudge`'s `client.CompleteJSON(ctx, system, user, schema)` shape (`golden.go:378-409`), with a JSON schema whose fields map 1:1 onto `GoldenExpected`.

### PR path (`--pr`): reuse Phase 3 + Phase 5, do not re-implement

`--pr` does **not** open its own git/gh flow. It:

1. Calls the shared Phase 3 helper in `internal/cli/pr.go` - `branchName` = `auto/brain-analyze-<timestamp>`, branch off `origin/main` (fetch first), `add -A` over the edited `testdata/golden-queries.yaml` + touched `library/sources/*.yaml`, commit, `push -u`, `gh pr create --fill --reviewer <user> --base main`, capture URL via `jobs.Store.SetArtifactURL`.
2. The PR body embeds the *evidence*: for each proposal, the failing `RunID`s and `Issue.Message` tails (so the human reviewer sees "this golden test was generated because runs X,Y,Z failed missing-source on snowflake"). Per global instruction + `feedback_plaintext_drafts`, write the body to a temp file and pass `--body-file`, never inline.
3. **Phase 5 gate is the merge guard, not a new mechanism.** Because Phase 7's PR edits `aida`'s own brain config, merge-to-main is the irreversible edge → the job lands in `awaiting_approval` exactly like a code PR. Jarvis speaks "self-improvement PR ready, N golden tests + M routing edits, waiting on approval" on next wake; `aida jobs approve <ref>` (or "Jarvis, approve") merges. No special-casing - `scanJobsForNotifications`' new `case StateAwaitingApproval:` (Phase 5) already handles it.

Crucially the **drafting itself is read-only / never auto-applied**: `--propose --dry-run` only prints; `--propose --pr` only opens a gated PR. There is no `--propose --apply` that writes to the working tree without review - that would violate the "human gate before writeback" invariant.

### Validation before proposing (close the loop on itself)

A freshly drafted `GoldenQuery` is only worth committing if it actually *passes* against current aida (else we'd commit a red test). Before adding it to the PR, run it through the existing `runGolden(ctx, q)` (`golden.go:234`) - the same end-to-end parse→classify→plan→`LLMRoute`→execute→synthesize→assert path the suite uses. Drop any drafted query whose routing assertions don't hold today (it describes a *future* fix, not current behavior); keep the ones that pass so `go test ./internal/engine -run TestGoldenRouting` stays green on merge. This reuses the Phase 5 *gate model* (`golden.go` / `Judge` as the code-eval judge) without new infrastructure.

### Files touched

- **Modify** `internal/cli/brain_analyze.go` - add `--propose/--pr/--dry-run/--min-failures/--reviewer/--base` flags; a `proposeFromEvalRuns(brainPath, opts)` that reuses the existing scan loop, calls `eval.FailureBoosts` for routing diffs and a per-bucket LLM draft for goldens, validates via `runGolden`, then either prints (`--dry-run`) or hands the file edits to the Phase 3 PR helper (`--pr`).
- **Reuse, no change** - `brain.ListRecentEvalRuns`, `brain.ReadEvalRun`, `eval.ParseFailures`, `eval.FailureBoosts`/`MaxAdjustment` (`internal/eval/boosts.go`); `GoldenQuery`/`GoldenExpected`/`loadGoldenSuiteOrEmpty`/`runGolden` (`internal/cli/golden.go`); `internal/cli/pr.go` (Phase 3); the `awaiting_approval` state + `scanJobsForNotifications` case (Phase 5).

### Verification

- `aida brain analyze --propose --dry-run --min-failures 2` against a seeded `~/.aida/brain/eval-runs/` (several failed runs sharing one `source × issue-type`) prints both a routing diff and a YAML golden block; with `--min-failures 99` it prints nothing (threshold gate works).
- `--propose --pr --reviewer your-github-handle` opens a PR: `gh pr view <n> --json reviewRequests` shows the user tagged, the job manifest carries `artifact_url`, and the job sits in `awaiting_approval` (not merged) until `aida jobs approve <ref>`.
- Negative: a drafted golden whose routing assertion fails `runGolden` is *excluded* from the PR - inspect `--dry-run` output to confirm only currently-passing goldens are proposed.
- `go test ./internal/cli -run TestGoldenRouting` (and `./internal/engine`) stays green after a proposed-PR merge, proving the meta-loop doesn't redden the suite it edits.

> **Verification note:** `eval.FailureBoosts` only attributes a source when `Issue.Anchor` parses as `(source: id)` (`boosts.go:81`, `extractSourceFromAnchor`) - `scope-mismatch` anchors the bare token, so it contributes to *golden* proposals (via `ParseFailures`, which keeps the `IssueType`) but **not** to routing-boost diffs (consistent with `boosts.go:62-68`). Phase 7 must not assume every parsed failure yields a routing edit.

---

## Reference - new CLI flags, config keys, job states, types

This section is the consolidated, normative reference for every surface the autonomous loop *adds*. Each table flags whether a symbol **exists today** (verified against the repo at the cited path) or is **new** to this spec. When in doubt, the manifest JSON on disk is the source of truth; `jobs.db` is a per-machine cache (see [manifest version history](#manifest-version-history)).

> **This section is the pre-implementation draft.** The flag names, defaults,
> and config keys below are what was originally planned. The table immediately
> following has been corrected against the shipped `internal/cli/loop.go` /
> `internal/cli/serve.go`; the `aida jobs`, `aida brain analyze`, and "New
> config keys" tables after it have **not** been rewritten line-by-line;
> follow the callouts inline and the top-of-document deviations note for what's
> actually real.

### `aida loop` flags (corrected against the shipped code)

Existing flags on `internal/cli/loop.go` `loopOpts` / `newLoopCmd` at the time this spec was drafted: `--tag`, `--max-iterations` (10), `--check`, `--commit`, `--recall-k` (3), `--per-task-timeout` (7m30s = `450*time.Second`), `--dry-run` (global). Everything below is the full, verified-shipped flag set, correcting the defaults and additions this draft originally proposed:

| Flag | Type | Default (shipped) | `loopOpts` field | Phase | Behavior |
|---|---|---|---|---|---|
| `--pr` | bool | `false` | `pr` | 3 | Open a PR on a passing iteration instead of committing in place. **XOR with `--commit`** (errors if both set); implies `--worktree`. Routes through `internal/cli/pr.go` (`openTaskPR`: push, then `gh pr create --reviewer <user> --base <base>`), captures URL via `jobs.SetArtifactURL`. Never calls `gh pr merge`; parks the task on `hold` (tagged `pr-open`) instead. |
| `--reviewer` | string | `""` | `reviewer` | 3 | GitHub login tagged on the PR (`gh pr create --reviewer`). **No config fallback exists** (`cfg.Agent.PR.Reviewer` was never built), CLI-only. |
| `--base` | string | `"main"` | `base` | 3 | Base branch for the PR; worktree is cut off `origin/<base>` after `git fetch`. **No config fallback exists**, CLI-only. |
| `--review-panel` | int | `0` | `reviewPanel` | 3 | Number of adversarial reviewer sub-agents (correctness/security/test-coverage lenses, cycling) fanned out via `delegate_to_claude_code`-style spawns before PR open. `0` = panel disabled. Majority-pass (`approvals*2 > n`, inline in `pr.go`) required or the task parks on `hold`. |
| `--max-fix-iterations` | int | `3` | `maxFix` | 2 | Inner code-eval retry bound. After `CodeReviewer` `Fail`, re-spawn a fresh agent with a fix prompt embedding the failing `Issue`s; on exhaustion park the task `hold`. Distinct from the outer `--max-iterations` (rounds). |
| `--max-budget-usd` | float64 | `0` (falls back to `cfg.Agent.MaxBudgetUSD`, else unlimited) | `maxBudgetUSD` | 4 | Cumulative USD stop. Read per-iteration from the run-dir `events.ndjson` `complete.cost_usd` (`readRunCostUSD`). |
| `--concurrency` | int | `1` | `concurrency` | 4 | Horizontal fan-out: N tasks worked at once per round, each in its own worktree (requires `--worktree`). Round-barrier batch model, not a continuously-refilled pool. |
| `--daemon` | bool | `false` | `daemon` | 4 | Continuous mode: sleep `--poll` when no task is pending instead of exiting. Also reachable via `aida serve --loop` (forces `daemon=true` regardless of this flag). |
| `--poll` | duration | `5s` | `poll` | 4 | In `--daemon` mode, sleep duration when the task set is empty. (This draft originally called it `--poll-interval`, default `30s`; the shipped name and default are both different.) |
| `--max-consecutive-failures` | int | `0` (unlimited) | `maxConsecFail` | 4 | Circuit breaker: stop after N consecutive tasks parked on `hold`. |
| `--worktree` | bool | `false` | `worktree` | 4 | Isolate each task in its own git worktree branched off `origin/main` (`auto/<id>-<slug>`). **Opt-in**, not automatic per iteration as this draft's §4.2 assumed. Implied by `--pr`, `--concurrency > 1`, and `--sandbox docker`. |
| `--sandbox` | string | `"none"` | `sandboxTier` | 6 | Confine spawned agents: `none` \| `docker` (via the real `sbx` CLI). Requires `--worktree`. No Seatbelt fallback exists. |
| `--sandbox-memory` | string | `""` | `sandboxMemory` | 6 | `sbx` memory limit, e.g. `8g` (docker tier only). |
| `--provision-aida` | string | `""` | `provisionAida` | 6 | Prebuilt linux/arm64 `aida` to copy into the docker sandbox; default auto-builds one from the aida checkout (`CGO_ENABLED=0 GOOS=linux GOARCH=arm64`). |

> **Verification:** `--check` and `--commit` already existed pre-Phase-2/3. `--max-budget-usd` wires `cfg.Agent.MaxBudgetUSD` (`internal/config/config.go`, `MaxBudgetUSD float64 yaml:"max_budget_usd"`), which existed but was unread before this system; now `runLoopCtx` reads it as the fallback when `--max-budget-usd` is `0`. The `--pr`/`--commit` XOR is enforced in `loopOpts.validate()`.

### `aida jobs approve | reject` - new subcommands

New command group registered alongside `aida jobs list` (handlers near `internal/cli/jobs.go`; voice mirror in `internal/jarvis/tools/approval.go`):

| Command | Args | Phase | Behavior |
|---|---|---|---|
| `aida jobs approve <run-id>` | run id | 5 | `store.Get(runID)`, assert `State == awaiting_approval`, write `approval.txt` = `approve` in the run-dir. Unblocks the agent's `request_approval` poll, merge/send proceeds. **Correction:** takes the exact run-id (`cobra.ExactArgs(1)`), not a fuzzy `<ref>` resolved via `resolveJobRef`/`extractPRNumber` as drafted below. |
| `aida jobs reject <run-id> --reason <text>` | run id + optional reason | 5 | Writes `approval.txt` = `reject: <reason>` (or bare `reject` if `--reason` omitted); job transitions to `failed` (terminal). **Shipped flag is `--reason`** (optional), not the required `--because` originally drafted. |

`<ref>` resolution reuses the existing `resolveJobRef` helper; voice equivalents are `approve_job` / `reject_job`, **Jarvis-only** (never in `BuildAgentToolsLazy` - recursion guard).

### `aida brain analyze --propose` flags (Phase 7, follow-on; corrected against the shipped code)

`internal/cli/brain_analyze.go` `runBrainAnalyze` shipped read-only proposal generation, no PR-opening path exists. The real flag set:

| Flag | Type | Default | Behavior |
|---|---|---|---|
| `--propose` | bool | `false` | Draft new `golden.GoldenQuery`/`GoldenExpected` cases + routing-boost diffs from `ParseFailures`/`FailureBoosts` over `ListRecentEvalRuns`. Prints proposals. |
| `--out` | string | `""` | Also write the `--propose` report to this file, for a human to hand-carry into a PR. |
| `--min-failures` | int | `2` | Minimum recurring failures in a `source × issue-type` bucket before it earns a proposal. |
| `--limit` | int | `0` | Scan only the most recent N eval-runs (`0` = all); pre-existing flag. |
| `--source` | string | `""` | Filter to a single source name; pre-existing flag. |
| `--ndjson` | bool | `false` | Print raw eval-runs as NDJSON instead of the summary table; pre-existing flag. |

**`--pr`, `--dry-run`, `--reviewer`, and `--base` described below and in earlier drafts of this phase were never added.** There is no auto-PR path for `brain analyze`; a human takes the `--propose --out <file>` report and opens the PR by hand if they agree with it.

### New config keys, NOT IMPLEMENTED (design record only)

> **None of the keys in this section exist in `internal/config/config.go`.**
> `AgentYAMLConfig` has only `MaxTurns` and `MaxBudgetUSD`, no `Checks`, no
> `PR` sub-struct, and there is no top-level `SandboxConfig`/`sandbox:` block
> at all. Every flag that this draft imagined would fall back to one of these
> keys (`--check`, `--reviewer`, `--base`, `--sandbox`, etc.) is CLI-only with
> a hardcoded default in the shipped code; see the corrected flag table
> above. Kept below verbatim as the original design record.

All under the existing `agent:` and a new `sandbox:` block in `~/.aida/config.yaml`. `AgentYAMLConfig` lives at `internal/config/config.go:30` (`MaxTurns`, `MaxBudgetUSD` already present):

```yaml
agent:
  max_turns: 15            # EXISTS - config.go:30
  max_budget_usd: 0.50     # EXISTS struct field (config.go:31), NEW: now read by --max-budget-usd
  checks:                  # NEW - repeatable code-eval commands (CodeReviewer)
    - "make build"
    - "make test"
    - "make vet"
  pr:                      # NEW - agent.pr.* (Phase 3)
    reviewer: your-github-handle
    base: main
    auto_create: false     # honors the tasks_ingest.go destination flag

sandbox:                   # NEW block (Phase 6)
  tier: docker             # docker | seatbelt | none  (default docker)
  egress_allowlist:        # hosts the host-side proxy permits
    - api.anthropic.com
    - github.com
    - api.voyageai.com
    - api.notion.com
  memory: "8g"             # --memory passed to docker sandbox run (default 50% host)
  default_spend_cap_usd: 1.00
  default_timeout_sec: 1800
```

| Key | Go type / field | Read by | Phase |
|---|---|---|---|
| `agent.max_budget_usd` | `AgentYAMLConfig.MaxBudgetUSD float64` (exists) | `aida loop` per-iteration cost stop | 4 |
| `agent.checks` | `[]string` (new) | `eval.CodeReviewers(checks)` → `eval.CodeReviewer` | 2 |
| `agent.pr.reviewer` | new `agent.pr` sub-struct | `pr.go` `gh pr create --reviewer` | 3 |
| `agent.pr.base` | new | worktree base branch / `--base` default | 3 |
| `agent.pr.auto_create` | new | gates un-gating of `gh pr create` in `tasks_ingest.go` `appendDestinationInstructions` (today hard-forbids it, tasks_ingest.go:154-176) | 3 |
| `sandbox.tier` | new `SandboxConfig` | `internal/sandbox` adapter selection | 6 |
| `sandbox.egress_allowlist` | `[]string` | `sbx policy` file / host proxy | 6 |
| `sandbox.memory` | string | `docker sandbox run --memory` | 6 |
| `sandbox.default_spend_cap_usd` | float64 | per-job `Manifest.SpendCapUSD` default | 6 |
| `sandbox.default_timeout_sec` | int | per-job `Manifest.TimeoutSec` default | 6 |

### Job states

Defined in `internal/jobs/manifest.go:13-17`. **One new non-terminal state** is added; the others are unchanged:

| State const | Value | Terminal? | Status |
|---|---|---|---|
| `StateQueued` | `queued` | no | exists |
| `StateRunning` | `running` | no | exists |
| `StateAwaitingInput` | `awaiting_input` | no | exists |
| **`StateAwaitingApproval`** | **`awaiting_approval`** | **no** | **NEW (Phase 5)** |
| `StateDone` | `done` | **yes** | exists |
| `StateFailed` | `failed` | **yes** | exists |

`IsValidState` (manifest.go:29) gains `StateAwaitingApproval` in its `switch`; `IsTerminalState` (manifest.go:22) is unchanged (approval is non-terminal).

> **Highest-risk change (call it out in review):** `internal/cli/serve.go` `scanJobsForNotifications` (serve.go:268) switches on state with `case StateAwaitingInput / StateDone / StateFailed` and a **`default: continue`** (serve.go:288) that *silently drops unknown states*. Phase 5 MUST add `case jobs.StateAwaitingApproval:` to speak "PR <n> ready, waiting on approval to merge" - otherwise the gate is invisible.

### Manifest / Store - new fields & methods

`Manifest` struct (`internal/jobs/manifest.go:48`). Existing fields: `RunID`, `Kind`, `State`, `WorktreePath`, `AwaitingPrompt`, `NotifiedAt`, `ArtifactURL`. New fields are **additive and zero-value safe**:

| Field | Type | Phase | Purpose |
|---|---|---|---|
| `ApprovalAction` | `string` | 5 | What the agent wants approved, e.g. `merge` / `send`. Set by `RequestApproval`. |
| `ApprovalPayload` | `string` | 5 | Context for the human gate, e.g. the PR URL / draft text. |
| `SpendCapUSD` | `float64` | 6 | Per-job USD cap; dispatcher enforces. Defaults from `sandbox.default_spend_cap_usd`. |
| `TimeoutSec` | `int` | 6 | Per-job wall-clock cap; dispatcher applies `context.WithTimeout`. |
| `ToolAllowlist` | `[]string` | 6 | Agent filters `engine.BuildAgentToolsLazy` output to this set. |
| `SandboxTier` | `string` | 6 | `docker` / `seatbelt` / `none` per job. |

New `Store` methods (`internal/jobs/store.go`; existing: `Enqueue`/`Claim`/`Complete`/`Fail`/`Pause`/`Resume`/`SetWorktreePath`/`SetArtifactURL`/`MarkNotified`):

| Method | Signature (sketch) | Phase | Transition |
|---|---|---|---|
| `RequestApproval` | `(runID, action, payload string) error` | 5 | `running` → `awaiting_approval`; sets `ApprovalAction`/`ApprovalPayload` |
| `Approve` | `(runID string) error` | 5 | `awaiting_approval` → `running` (agent resumes, performs merge/send) |
| `RejectApproval` | `(runID, reason string) error` | 5 | `awaiting_approval` → `failed` (terminal) |

`manifestVersion` bumps **2 → 3** (manifest.go:42). The bump is additive only - v2 manifests missing the new fields decode as zero values (same pattern as the v1→v2 note at manifest.go:238-240), no migration step. Phase 5 and Phase 6 fields land in the same single increment.

### Approval-gate state machine

```
running ──RequestApproval(action,payload)──▶ awaiting_approval
                                              │
   serve.go scan ──speak "ready for review"──┤  (NEW case)
                                              │
   approve_job / aida jobs approve ─────────────┤
   writes approval.txt=approved               ▼
                                            running ──(merge/send)──▶ done
   reject_job / aida jobs reject --because ─────┐
   writes approval.txt=rejected:<reason>      ▼
                                            failed
```

The agent-side counterpart is `buildRequestApprovalTool` in `internal/cli/agent.go` (mirrors `buildAskUserTool`, which pauses to `awaiting_input` and polls `input.txt`): it sets `awaiting_approval` and **blocks polling `approval.txt`** in the run-dir; merge/send code is unreachable until `approved`.

### New packages / files

| Path | New? | Contents | Phase |
|---|---|---|---|
| `internal/eval/code.go` | new | `CodeReviewer` (implements `eval.Reviewer`); `CodeReviewers(checks []CodeCheck)`; runs build/test/lint in `WorkDir`, non-zero exit → `Fail` `ReviewRecord` with `Issue{Type:"build\|test\|lint-failure", Severity:"error"}` | 2 |
| `internal/eval/reviewer.go` | modify | add `WorkDir string` + `Commands []CodeCheck` to `ReviewInput` (today only `Question`/`Answer`/`Results`). Leave `DefaultReviewers()` (reviewer.go:148) untouched | 2 |
| ~~`internal/eval/panel.go`~~ | **not built** | The majority-pass reducer shipped inline as `runReviewPanel` (`approvals*2 > n`) in `internal/cli/pr.go` instead of a standalone `eval.PanelVerdict`. | 3 |
| `internal/cli/pr.go` | new (shipped) | `openTaskPR`: push → `gh pr create --head --base --title --reviewer`; `runReviewPanel`; `SetArtifactURL` via caller | 3 |
| `internal/worktree/` | new (shipped) | shared `Create`/`Remove`/`RepoRoot`/`DeleteBranch` in `internal/worktree/worktree.go`, used by `loop.go`; standardize on `~/.aida/worktrees/<profile>/` | 4 |
| `internal/sandbox/sandbox.go` | new (shipped, single file, not split into `docker.go`/`seatbelt.go`) | `Wrap(ctx, tier, policy, name, args...)` → for `docker`: `sbx create --name <n> [-m <mem>] shell <workdir> [mounts...]` then `sbx exec -w <workdir> -e <env> <n> <cmd> <args>`; teardown via `sbx rm -f <n>`. No Seatbelt tier, no egress allowlist, no secret injection. | 6 |
| `internal/jarvis/tools/ingest.go` | new | voice tool `ingest_notion_tasks {page_ref?, confirm?}`; registered under the `jobsStore != nil` block in `registry.go`; shells `aida tasks ingest --from-notion` | 1 |
| `internal/jarvis/tools/approval.go` | new | voice tools `approve_job` / `reject_job` | 5 |
| `internal/cli/agent.go` | modify | `buildRequestApprovalTool`; inject `MemoryInstruction`s (e.g. `build-after-changes`) into `variablePrompt`; sandbox `Wrap` | 2,5,6 |
| `internal/cli/serve.go` | modify | add `case jobs.StateAwaitingApproval` to `scanJobsForNotifications` (serve.go:268); `--loop` host point; sandbox-aware teardown | 4,5,6 |
| `internal/cli/loop.go` | modify | iterate-until-green inner loop, per-iteration worktree, `--concurrency`, `--daemon`, budget read-back, PR path | 2,3,4 |
| `docs/autonomous-loop/{OVERVIEW,SPEC}.md` | new | extends/supersedes existing `docs/autonomous-loop-design.md` | D |

### `sbx` CLI surface used by `internal/sandbox/sandbox.go` (corrected against the shipped code)

This draft originally assumed a `docker sandbox run`/`docker exec` invocation shape. The shipped code drives the real `sbx` CLI directly:

| Command | Use |
|---|---|
| `sbx create --name <n> [-m <mem>] shell <workdir> [extra-mounts...]` | Start a microVM sandbox (the built-in `shell` base image) over the per-task worktree, plus any extra mounts (e.g. the run-dir) |
| `sbx cp <linux-aida> <n>:/usr/local/bin/aida` | Copy a cross-compiled linux/arm64 `aida` into the VM (`Policy.ProvisionBin`), needed because the macOS `aida` binary can't execute inside the Linux microVM; runs in place of the host binary |
| `sbx exec -w <workdir> -e K=V... <n> <cmd> <args...>` | Run the (possibly provisioned) command inside the sandbox |
| `sbx rm -f <n>` | Teardown, a `defer cleanup()` around each spawn in `internal/cli/loop.go`, not a `serve.go` goroutine |
| `exec.LookPath("sbx")` | Availability probe (`sandbox.Available`); absent falls back to `none` (unconfined) with a printed warning, not to a Seatbelt tier |

> **What did not ship:** no `sbx policy` / egress-allowlist / secret-injection layer; the sandbox is filesystem + process isolation only. No Seatbelt fallback (`sandbox.tier` degrades straight to `none`, not `seatbelt`, which doesn't exist as a tier). The commit-signing caveat below is still accurate: the loop commits unsigned in-sandbox-or-not (`internal/cli/loop.go` `loopCommit` uses inline `git commit -m`, not `-F`, contrary to the project's own commit-message convention, worth a follow-up).
>
> **Caveats baked into the original draft, still relevant:** `sbx` is experimental, pin the version if this becomes a reliability problem; one sandbox per workspace is satisfied by per-task worktrees, which is what enables `--concurrency`.

<a name="manifest-version-history"></a>
> **Manifest version history (corrected):** v1 (initial) → v2 (`AwaitingPrompt`, `NotifiedAt`) → v3 → **v4** (shipped: approval fields `ApprovalAction`/`ApprovalPayload` from Phase 5 + governance fields `SpendCapUSD`/`TimeoutSec`/`ToolAllowlist`/`SandboxTier` from Phase 6, one version higher than this draft's "v2→v3, both phases in one bump"; the two phases still shared a single increment, just numbered 4). `jobs.db` is a per-machine cache regenerated from the per-run manifest JSON, so version bumps need no cross-machine migration.

---

## Verification matrix & end-to-end walkthrough

This section gives a per-phase verification command for every stage of the autonomous loop, then a single continuous walkthrough from the voice trigger *"turn that call into tasks"* through *"approve PR 583"* and merge - including the load-bearing negative test that proves `gh pr merge` cannot run without an explicit human approval.

> Convention: voice phrases are spoken to a running `aida serve --loop` (HTTP `127.0.0.1:1610` + mic listener + dispatcher in one process). CLI verifications can be run independently in a second terminal. `<user>` = `your-github-handle`.

> **This table and the walkthrough below are the original pre-implementation
> design record.** Rows 1-4 hold up against the shipped code. Rows 5-7, and the
> negative test and walkthrough's later steps, describe an auto-merge-after-
> approval and gated-egress design that was **not** fully realized; see the
> corrected cells inline and the implementation-deviations note at the top of
> this document.

### Per-phase verification matrix

| Phase | What it proves | Verification command(s) | Pass condition / verification note |
|---|---|---|---|
| **0** Brain note | Harness-engineering numbers folded in | `grep -c "Endor Labs\|Docker Sandboxes\|OWASP ASI" ~/.aida/brain/knowledge/patterns/agent-harness-engineering.md` | ≥ 3 hits; `aida brain index` re-embeds (`brain.IsStale` flips); non-code. |
| **D** Docs | Spec + overview render and cite real paths | `ls docs/autonomous-loop/{OVERVIEW,SPEC}.md` then `grep -o "internal/[a-z/]*\.go" docs/autonomous-loop/SPEC.md \| sort -u \| while read p; do test -f "$p" \|\| echo "MISSING: $p"; done` | Both files exist; every cited path resolves (no `MISSING:` lines). |
| **1** Voice ingest | Voice → `auto`-tagged task cohort | Voice: *"Jarvis, turn that call into tasks"* → *"yes"*. Then `aida tasks list --tag auto` | Tool `ingest_notion_tasks` registered only in the `jobsStore != nil` block of `internal/jarvis/tools/registry.go` (next to `jobStartTool`); first call shells `aida tasks ingest --from-notion <ref> --dry-run` (speaks "Would create N tasks"), confirm shells the real `--plan --yes --tag auto`. **Note:** assert `ingest_notion_tasks` is NOT in `engine.BuildAgentToolsLazy` (recursion guard, same rule as `feedback_no_mcp_query_tool`). |
| **2** Code-eval loop | `CodeReviewer` iterates build/test/lint until green | `go test ./internal/eval/...` then seed an "add fn + failing test" task and `aida loop --tag auto --check "make test" --max-fix-iterations 3 --dry-run`; for real, drop `--dry-run` and `ls ~/.aida/brain/eval-runs/` | New `internal/eval/code.go` `CodeReviewer` returns a `*ReviewRecord{Verdict: VerdictFail}` with `Issue{Type:"test-failure", Severity:"error", Message:<tail>}` on non-zero exit; forced failure produces ≥ 2 attempts and an `eval-runs/<id>.json` per attempt (`brain.WriteEvalRun`). `eval.AggregateVerdict` fail-loud means any `Fail` re-spawns. **Note:** `DefaultReviewers()` must be unchanged - new behavior is behind `CodeReviewers(checks)`. |
| **3** PR creation + review swarm | Branch pushed, user tagged, panel gates | Throwaway repo: `aida loop --tag auto --pr --reviewer your-github-handle --base main --check "make test" --review-panel 3` then `gh pr view <n> --json reviewRequests,headRefName` and `aida jobs get <runID> --json` | New `internal/cli/pr.go` `branchName(task)` = `auto/<taskID>-<slug>`, branched off `origin/main` (fetch first, NOT daemon HEAD); `reviewRequests` contains `<user>`; manifest `ArtifactURL` (`SetArtifactURL`) holds the PR URL. Adversarial panel (N `delegate_to_claude_code` sub-agents, correctness/security/test-coverage lenses) must majority-pass (`eval.PanelVerdict`) before `gh pr create` runs. **Note:** `appendDestinationInstructions` currently forbids `gh pr create` - `aida loop --pr` is the un-gated path, distinct from `tasks ingest --auto-solve`. |
| **4** Dispatcher / fan-out / budget | Continuous, concurrent, budgeted, worktree-isolated | `aida loop --tag auto --pr --concurrency 2 --max-iterations 4 --max-budget-usd 5.00`; concurrently `git worktree list` and `aida jobs list`; budget test: `agent.max_budget_usd: 0.01` in config then re-run | 2 tasks run at once, each in its own `~/.aida/worktrees/<profile>/...` worktree off `origin/main`; `git worktree list` is clean after terminal (daemon `cleanupWorktreePath` via `git worktree remove --force`); `aida jobs list` shows `kind=loop`. Budget: `cfg.Agent.MaxBudgetUSD` (exists but unread today) is read back per-iteration from run-dir `events.ndjson` `{"type":"complete","data":{"cost_usd":…}}` (`internal/cli/agent_events.go:113` `emitComplete`) - loop stops early. |
| **5** Approval gate | New state, general-purpose HITL infra | `internal/jobs/manifest_test.go` asserts `IsValidState("awaiting_approval")`; drive any `--agent --run-dir` task through `request_approval` and `aida jobs approve/reject <ref>` | **Shipped, but not for the loop's own PRs.** `StateAwaitingApproval` (non-terminal), `manifestVersion` is `4` (not `2→3`); `serve.go` speaks on the new state. **Correction:** `aida loop --pr` never calls `request_approval` or `gh pr merge`; it parks the PR'd task on `hold` (tagged `pr-open`) for a fully manual human merge. The `approve_job`/`request_approval` round trip is real infrastructure for *other* agent tasks, not the loop's merge step. |
| **6** Docker Sandbox | Per-task microVM (filesystem/process isolation only) | During a loop iteration: `sbx ls`; inside, attempt a write outside the mounted worktree | `internal/sandbox/sandbox.go` `Wrap` runs `sbx create`/`sbx exec`/`sbx rm` (not `docker sandbox run`/`docker exec`); a per-task sandbox is listed during the iteration and removed via `defer cleanup()` in `loop.go` after. **Correction:** there is no egress allowlist, no host-side proxy, and no secret injection; a non-allowlisted-host curl is not denied, because no such policy exists. No Seatbelt fallback; absent `sbx` falls back to `none` (unconfined). |
| **7** Self-improvement | Read-only proposal generation | `aida brain analyze --propose --min-failures 2` then `--propose --out report.md` | `--propose` prints proposed `GoldenQuery`/routing-boost diffs (from `ParseFailures`/`FailureBoosts` over `ListRecentEvalRuns`). **Correction:** there is no `--dry-run` or `--pr` flag on this command, it never opens a PR itself; `--out` writes the report to a file for a human to act on by hand. |

### Negative test - no approval, no merge (the load-bearing guarantee)

This is the test that proves the only path to `main` is an explicit human approval. Run a full loop iteration to the point where the agent calls `request_approval`, **withhold approval**, and assert the merge never fired:

```bash
# 1. Drive a loop to a PR + approval gate, then DON'T approve.
aida loop --tag auto --pr --reviewer your-github-handle --check "make test"
aida jobs list            # the loop job sits in awaiting_approval, NOT done

# 2. Triple-layer assertion that gh pr merge never ran:

# (a) Run-dir event stream: no merge tool call was ever emitted.
RUNDIR=$(aida jobs get <runID> --json | jq -r .run_dir)   # ~/.aida/jobs/<profile>/runs/<runID>/
grep -c '"name":"request_approval"' "$RUNDIR/events.ndjson"   # >= 1  (gate was hit)
! grep -q 'gh pr merge' "$RUNDIR/events.ndjson"               # exit 0 == merge NEVER invoked

# (b) GitHub: the PR is still open and unmerged.
gh pr view <n> --json state,mergedAt   # {"state":"OPEN","mergedAt":null}

# (c) Job state: parked, never terminal.
aida jobs get <runID> --json | jq -r .state   # "awaiting_approval"
```

**Why this holds - three independent layers (belt + suspenders + network):**

1. **State machine.** `request_approval` (new `buildRequestApprovalTool` in `internal/cli/agent.go`, modeled on `buildAskUserTool` at `agent.go:761`) sets `StateAwaitingApproval` and blocks polling `<run-dir>/approval.txt` exactly as `buildAskUserTool` polls `input.txt`. With no `approve_job`/`aida jobs approve`, the file never appears, the goroutine never returns, and the `gh pr merge` statement that follows it is never reached.
2. **`confirm_always` fails closed.** Even if a future prompt tried to call `gh pr merge` directly, it routes through `internal/engine/agent_tools.go buildMCPTool`'s `confirmFn` (the merge/send patterns added to `config.GuardrailsConfig.ConfirmAlways`, `config.go:113`). In a detached `--run-dir` job there is no TTY; `confirmFn`'s stdin scanner hits EOF and returns false - the destructive call is denied, not silently allowed.
3. **Network egress (Phase 6).** The Docker Sandbox host-side proxy holds the GitHub token and only injects it for allowlisted, policy-permitted operations; the sandbox VM never holds the raw secret, so an in-sandbox merge attempt can't authenticate even if layers 1–2 were bypassed.

A regression in any single layer is caught by the other two; assertion (a) (`! grep -q 'gh pr merge'`) is the canonical CI check because it reads the agent's own immutable `events.ndjson` rather than trusting GitHub state.

### End-to-end walkthrough - "turn that call into tasks" → "approve PR 583"

```
                          HUMAN GATE #1 (create)        HUMAN GATE #2 (merge)
                                  │                              │
 voice ───► ingest ───► dispatch ─┴► sandbox+eval ─► review ─► PR ─┴► merge
 (P1)       (P1)        (P4)         (P6 + P2)        (P3)       (P5)     (P5)
```

1. **Capture (P1).** Off a Notion call, you say *"Jarvis, turn that call into tasks."* The listener (`internal/jarvis/listener/listener.go`) dispatches the new `ingest_notion_tasks` voice tool. With a bare reference it asks `claude --print` for the most-recently-edited meeting-notes page (via `NotionAdapter`, `internal/adapters/notion.go`), then shells `aida tasks ingest --from-notion <ref> --dry-run` and **speaks the page title + "Would create 6 tasks. Create them?"** - this is *human gate #1*, a confirm-before-create.

2. **Create (P1).** You say *"yes."* The tool shells the real `aida tasks ingest --from-notion <ref> --plan --yes --tag auto`. Six tasks land in `~/.aida/brain/tasks/<slug>.md` (+ SQLite), each tagged `auto` (the Phase-4 handoff token) and given a priority tag. `aida tasks list --tag auto` shows the cohort.

3. **Dispatch + fan-out (P4).** The always-on `aida serve --loop` dispatcher runs `pickNextTask(brn, ["auto"])` (`internal/cli/loop.go:184`, p1 → p2 → p3 then oldest-first). With `--concurrency 2` it claims two `auto` tasks at once, flips each to `in-progress` (`brn.SetTaskStatus(... StatusInProgress)`), and for each creates a detached worktree off `origin/main` under `~/.aida/worktrees/<profile>/loop-<id>-<runID>/` (`SetWorktreePath`), enqueuing a `kind=loop` job (`store.Enqueue`).

4. **Sandbox + code-eval loop (P6 + P2).** Each worktree becomes its own Docker Sandbox via `sandbox.Wrap` - `docker sandbox run -d --name aida-<runID> --memory <n> <worktree>` then `docker exec … aida --agent --run-dir …`. The agent (`runAgentMode`, `maxTurns` from `AIDA_AGENT_MAX_TURNS`/`cfg.Agent.MaxTurns`) implements the task with `MemoryInstruction`s like `build-after-changes` injected into the prompt. After each attempt `CodeReviewer` runs `make build && make test && make vet`; a non-zero exit yields a `VerdictFail` `ReviewRecord`, and the loop re-spawns a fresh agent with the failing `Issue.Message`s embedded, iterating until green or `--max-fix-iterations` (then parks `hold`). Every attempt is persisted via `brain.WriteEvalRun` → `eval-runs/<id>.json`.

5. **Adversarial review panel (P3).** On green, `--review-panel 3` fans out three worktree-isolated reviewer sub-agents (`delegate_to_claude_code`, correctness / security / test-coverage lenses). `eval.PanelVerdict` majority-pass is required; otherwise the loop iterates with their issues. This is the harness research's two-wave adversarial verify.

6. **PR with tests (P3) - approaching gate #2.** `internal/cli/pr.go` creates branch `auto/<taskID>-<slug>`, `git add -A` + commit (test included; the prompt mandated add/update a test, and CodeReviewer kept `make test` green), `git push -u origin`, then `gh pr create --fill --reviewer your-github-handle --base main`. The PR URL is captured to the manifest via `SetArtifactURL`. Say the cohort's PR is #583.

7. **Park at the approval gate (P5).** The agent's final step calls `request_approval` instead of merging: the job transitions to `StateAwaitingApproval` and blocks on `approval.txt`. The daemon's `scanJobsForNotifications` hits the new `case jobs.StateAwaitingApproval:`, enqueues a voice notice via `notify.Enqueue`, and `MarkNotified` stamps idempotency. On your next wake word Jarvis speaks: *"PR 583 is ready for review, waiting on your approval to merge."* (speak-on-next-wake, `DrainOnNextWake`).

8. **Approve + merge (P5) - gate #2 opens.** You say *"Jarvis, approve PR 583."* The `approve_job` voice tool resolves the job (`resolveJobRef` / `extractPRNumber`), writes `approval.txt`; the blocked `request_approval` poll returns, the agent runs `gh pr merge`, the job goes `done`, and the daemon's terminal-transition pass fires `cleanupWorktreePath` (`git worktree remove --force`) and `sbx rm` for the sandbox. `brn.CompleteTask` marks the `auto` task `done` and `distillLearning` records a brain lesson from the run's `## Learnings`.

9. **Self-improvement (P7, periodic).** A scheduled `aida brain analyze --propose --pr` mines `eval-runs/` for recurring failures and opens its own Phase-5-gated PR with new golden tests + routing boosts - the meta-loop, still ending at human gate #2.

**Net invariant verified end-to-end:** the system is fully autonomous *between* the two human gates (ingest-confirm and merge-approve), and the negative test above proves the merge gate is enforced at the state-machine, stdin-confirm, and network-egress layers simultaneously - no `gh pr merge` (and, by the same `confirm_always` + sandbox mechanics, no `slack_send_message` / Gmail send / `notion-update-page` writeback) can fire without an explicit `approve_job` / `aida jobs approve`.
