# Aida Autonomous Loop - Overview

Status: shipped, merged to main via PR #81 / PR #82 (2026-06-23). High-level by intent,
for the per-phase types, states, flags, config keys, swarm protocol, sandbox contract,
and verification matrix, see [`SPEC.md`](./SPEC.md). This document supersedes the
earlier sketch in [`../autonomous-loop-design.md`](../autonomous-loop-design.md)
(kept for the Ralph-pattern rationale it captures).

> **Implementation status (2026-06-18, branch `feat/autoloop-build`, PR #82).**
> All seven phases plus follow-ons are built, gated (`go build/vet/test`), and on
> the PR. Deviations from this spec, all intentional:
>
> - **Seatbelt tier dropped.** The `sandbox-exec` fallback this doc describes was
>   removed in favor of the stronger sbx microVM. Tiers are now just `none |
>   docker`; `docker` is driven by the real `sbx` CLI (not a `docker sandbox`
>   subcommand, which false-positives on OrbStack). `none` is the only
>   zero-setup tier.
> - **Real `aida` runs confined.** An sbx sandbox is a Linux microVM, so the
>   macOS `aida` can't run in it directly. `Policy.ProvisionBin` copies a
>   linux/arm64 `aida` (auto-built `CGO_ENABLED=0`, pure-Go sqlite, or
>   `--provision-aida <path>`) into the VM and runs it in place of the host
>   binary. Live-validated: `aida --version` executes inside the microVM.
> - **`aida serve --loop`** hosts the dispatcher as a daemon goroutine (`--loop-*`
>   knobs mirror the high-value `aida loop` flags).
> - **Per-job governance** (`SpendCapUSD`/`TimeoutSec`/`ToolAllowlist`/
>   `SandboxTier`) round-trips on the manifest (v4) + jobs.db via
>   `Store.SetGovernance`; the loop stamps the tier + timeout each job runs
>   under. `ToolAllowlist` is persisted for audit; agent-loop enforcement is the
>   remaining open item.
> - **No network egress control was built.** The host-side proxy + secret-header-
>   injection design in §5 below (and SPEC.md's Phase 6) was never implemented,
>   there is no `sandbox.egress_allowlist` config, no proxy, no auth-header
>   injection anywhere in the repo. The `sbx` microVM gives filesystem and
>   process isolation only (its own kernel, the worktree mounted read-write).
>   The "network-deny" third layer described for Gate #2 in §3 does not exist
>   today; only the first two layers (`request_approval` state machine,
>   `confirm_always` fail-closed) hold, and `confirm_always` has no `gh pr
>   merge`/send patterns configured by default either.
> - **The loop's own `--pr` path does not use the approval gate.** `aida loop
>   --pr` commits, pushes, opens the PR, and parks the task on `hold` (tagged
>   `pr-open`) for a fully manual `gh pr merge`, no voice "approve PR N" round
>   trip is required or consulted. `request_approval`/`StateAwaitingApproval`/
>   `aida jobs approve|reject` are real, shipped, general-purpose HITL
>   infrastructure (`internal/jobs`, `internal/cli/agent.go`,
>   `internal/jarvis/tools/approval.go`) that any `--agent --run-dir` task can
>   pause on, the loop's own merge step just isn't wired to call it.
> - **`aida brain analyze --propose` is read-only**, exactly as shipped: it
>   prints proposals (and can write them to a file via `--out`), but there is no
>   `--pr` flag on that command; Phase 7's "opens its own gated PR" idea did
>   not ship. Turning a proposal into a merged change is still a manual copy-
>   paste-and-PR by a human today.

## Contents

1. [Vision and motivation](#1-vision-and-motivation)
2. [The end-to-end pipeline](#2-the-end-to-end-pipeline)
3. [The two human gates](#3-the-two-human-gates)
4. [The swarming model](#4-the-swarming-model)
5. [Why Docker Sandboxes](#5-why-docker-sandboxes)
6. [How it maps onto what already exists](#6-how-it-maps-onto-what-already-exists)
7. [Phase roadmap](#7-phase-roadmap)

## 1. Vision and motivation

The goal is one sentence:

> Get off a Notion call, say *"turn that call into tasks,"* and have an agent pull
> each task, work it with a real eval loop, open a PR **with tests**, and tag you
> for review - with a human in the loop **only** before "merge to main" and "press
> send."

Everything reversible is automated. Everything irreversible stops at a gate.

### Why now: the 2026 "agent harness engineering" frontier

The reading cluster that motivated this converged on one claim: **the harness, not
the model, is the lever.** The same model wrapped in a better harness - fresh
context per unit of work, hard feedback loops, adversarial verification, sandboxed
execution, trace-mined self-improvement - wins decisively over a raw model in a
thin wrapper. Roughly **two-thirds of enterprise agent failures are harness
defects**, not model defects; the ordering is **Frameworks < Runtimes < Harnesses**.
The 2026 frontier (LangSmith Engine, Galileo Signals, GEPA - autonomous
*trace-mining*) closes the loop further: the harness reads its own execution traces
and proposes its own improvements.

Aida already lives on the "harness" end of that spectrum - config-driven routing,
a semantic brain with feedback→router boosts, the `internal/eval` reviewer sensors,
a jobs queue with worktrees. The one frontier it had not crossed is **making the
improvement loop autonomous**: today a human still drives every step. This system
crosses it. The self-improvement meta-loop - `aida brain analyze` mining
`eval-runs/*.json` to propose its own golden tests and routing boosts (Phase 7) - 
is aida's version of autonomous trace-mining.

## 2. The end-to-end pipeline

```
  VOICE CAPTURE                                          ┌─ human gate? NO ─┐
  "Jarvis, turn that call into tasks"                    │ everything here  │
        │                                                │ is reversible    │
        ▼                                                └──────────────────┘
  ┌─────────────────────┐   ingest_notion_tasks (Jarvis-only voice tool)
  │ 1. INGEST           │   shells: aida tasks ingest --from-notion <ref>
  │   NotionAdapter     │   --dry-run → "Would create N tasks" → "yes" →
  │   claude --print    │   --plan --yes --tag auto
  └─────────────────────┘
        │  tasks tagged `auto` (handoff token)
        ▼
  ┌─────────────────────┐   aida loop --tag auto --pr --concurrency N --daemon
  │ 2. DISPATCH (swarm) │   pickNextTask → p1>p2>p3, oldest-first
  │   extended aida loop  │   fans out N tasks HORIZONTALLY, each → its own
  │                     │   worktree off origin/main → its own Docker Sandbox
  └─────────────────────┘
        │  one sandboxed worktree per task
        ▼
  ┌─────────────────────┐   docker sandbox run -d --name aida-<runID> --memory <n>
  │ 3. SANDBOX (sbx)    │   docker exec … "cd /work && aida --agent --run-dir …"
  │   microVM/workspace │   host-side egress proxy injects secrets; VM never
  │                     │   holds raw keys; writes confined to /work
  └─────────────────────┘
        │
        ▼
  ┌─────────────────────┐   aida --agent inner loop (fresh context per iteration)
  │ 4. WORK + EVAL LOOP │   CodeReviewer runs build/test/lint → Fail record
  │   iterate-to-green  │   re-spawn with failure Issue.Messages until green
  │   (Ralph Wiggum)    │   or --max-fix-iterations / budget; persist eval-runs
  └─────────────────────┘
        │  green
        ▼
  ┌─────────────────────┐   N reviewer sub-agents, distinct lenses
  │ 5. ADVERSARIAL PANEL│   (correctness / security / test-coverage)
  │   (vertical swarm)  │   majority-pass required, else the loop iterates
  └─────────────────────┘
        │  panel approves
        ▼
  ┌─────────────────────┐   branch auto/<taskID>-<slug> → commit (w/ test) →
  │ 6. OPEN PR          │   push -u origin → gh pr create --fill
  │   internal/cli/pr.go│   --reviewer <user> --base main → SetArtifactURL
  └─────────────────────┘
        │  PR opened, you are tagged
        ▼
  ┌─────────────────────┐  ╔═══════════════════════════════════════════════╗
  │ 7. PARK             │  ║  HUMAN GATE #1 - merge to main                ║
  │   awaiting_approval │  ║  request_approval → state=awaiting_approval   ║
  │                     │  ║  Jarvis speaks on next wake                   ║
  └─────────────────────┘  ╚═══════════════════════════════════════════════╝
        │  "Jarvis, approve PR 583"  (or `aida jobs approve <ref>`)
        ▼
  ┌─────────────────────┐   gh pr merge (only now unblocked) → state=done
  │ 8. MERGE + CLEANUP  │   sandbox removed (sbx rm) + worktree removed
  │                     │   (git worktree remove --force)
  └─────────────────────┘
        ┊
        ┊  (periodic, separate)        ╔═══════════════════════════════════════╗
        ▼                              ║  HUMAN GATE #2 - send / writeback     ║
  aida brain analyze --propose --pr      ║  any send (Slack/Gmail/Notion-update) ║
  mines eval-runs → golden + boost     ║  also routes through request_approval ║
  diffs → opens a gated PR (Phase 7)   ╚═══════════════════════════════════════╝
```

The whole left column is reversible and runs unattended. The two double-boxed gates
are the only places a human is required.

## 3. The two human gates

There are exactly two, and they sit at the two irreversible edges:

| Gate | Where | Mechanism | Enforced by (defense in depth) |
|------|-------|-----------|--------------------------------|
| **#1 - merge to main** | After the PR is opened and the panel approves | Job parks in a new non-terminal `awaiting_approval` state; Jarvis speaks on next wake; you say *"approve PR 583"* or run `aida jobs approve` | (1) `request_approval` agent tool blocks polling `approval.txt`; (2) `confirm_always` pattern for `gh pr merge` - `confirmFn` reads stdin and **fails closed** in a detached `--run-dir` job; (3) sandbox egress policy |
| **#2 - send / writeback** | Before any outbound send or external mutation | Same `request_approval` path | Same triple: tool gate + `confirm_always` for `slack_send_message` / `notion-update-page` / Gmail send (all fail-closed in detached jobs) + sandbox egress allowlist |

The key invariant: **merge and send never happen on the agent's own authority.**
Three independent layers each block them - the explicit `request_approval` tool, the
`GuardrailsConfig.ConfirmAlways` confirm-on-stdin gate (which can never succeed in a
detached background job, so it fails *closed*), and the sandbox network policy that
won't let the request leave the microVM unless it's allowlisted. Any one layer
holding is sufficient; all three must be defeated to bypass a gate.

This is why ingest today already hard-codes a forbid: `appendDestinationInstructions`
in `internal/cli/tasks_ingest.go` literally instructs the auto-solve agent *"Do NOT
run `gh pr create` or push a branch"* and gates full-auto behind a future
`auto_create: true` flag. The autonomous loop relaxes that forbid **only inside the
gated pipeline**, where the merge step is itself behind Gate #1.

> **As shipped, Gate #1 for the loop's own PRs is simpler than the table above.**
> `aida loop --pr` never calls `request_approval` and never runs `gh pr merge`
> itself: once the PR is open it parks the task on `hold` (tagged `pr-open`) and
> a human merges it directly (`gh pr merge` or the GitHub UI), no voice
> "approve" round trip. The `request_approval`/`awaiting_approval` machinery in
> the table is real and shipped (`internal/jobs`, `internal/cli/agent.go`,
> `internal/jarvis/tools/approval.go`), and any `--agent --run-dir` task can
> pause on it for a human sign-off, it just isn't the mechanism the loop uses
> for its own merge step today. The sandbox egress-policy layer named in both
> gates was not built at all (see the implementation-status note at the top of
> this document and §5 below); only the `request_approval` state machine and
> `confirm_always` (whose default patterns don't include `gh pr merge`/send
> verbs) exist.

## 4. The swarming model

Fan-out is first-class at three levels, all built on primitives that already exist
(`delegate_to_claude_code` worktree-isolated sub-agents, the jobs queue, Docker Agent
teams):

1. **Horizontal - across the queue (Phase 4).** `aida loop --concurrency N` works N
   `auto` tasks at once. Each task gets its own worktree off `origin/main`, which
   becomes its own Docker Sandbox - this is exactly what Docker's
   *one-sandbox-per-workspace* rule wants, and it's what makes concurrency safe.
   Bounded by `--max-budget-usd` and `--max-consecutive-failures`.
2. **Vertical - within a task (Phases 2–3).**
   - *Generator/judge tournament (optional):* fan out 2–3 implementation attempts
     with different approaches; keep the one that passes `CodeReviewer` and scores
     best. This mirrors the synthesizer's existing PRM-lite re-exec
     (`SynthesizeWithValidation`).
   - *Adversarial review panel:* before a PR opens, N reviewer sub-agents with
     distinct lenses (correctness / security / test-coverage) must majority-approve,
     else the loop iterates. This is the harness research's two-wave adversarial
     verify, and it extends the deliberately-deferred "Ralph Wiggum" iterate-until-
     clean variant noted at `internal/eval/reviewer.go:102-104`.
3. **Build-time.** This system is itself built by a Workflow-orchestrated swarm:
   independent phases fan out in parallel, per-file diffs are verified by adversarial
   code-review agents before commit, and this very document was produced by a
   documentation swarm (one agent per section against cited code, an editor agent
   assembling and de-duping).

The eval layer makes this safe to run unattended: `eval.AggregateVerdict` is
**fail-loud** (any `Fail` wins), and `CodeReviewer` turns a non-zero build/test/lint
exit into a `Fail` `ReviewRecord` that feeds the next fix iteration.

## 5. Why Docker Sandboxes

Autonomous agents editing a repo and reaching the network need real isolation, not
just good intentions. The decision is **Docker Sandboxes (`sbx`)** - a microVM per
workspace (own kernel), the workspace mounted read-write, and a host-side network
proxy that enforces egress policy *and injects auth headers so the VM never holds raw
secrets*. macOS arm64, Docker Desktop not required.

| Option | Isolation | Verdict |
|--------|-----------|---------|
| **Docker Sandboxes (`sbx`)** | microVM per workspace, own kernel; host-side egress proxy; secret injection | **Chosen.** Per-task worktree maps cleanly to one sandbox; host proxy gives the network-deny layer of Gate #2; macOS-native, no Docker Desktop. Caveats: perf overhead, in-sandbox commit-signing unsolved (sign on host rebase), `sbx` is experimental → pin the version. |
| Apple `container` | per-container VM (macOS 26) | Rejected primary. Apple-native VM is comparable isolation but heavier image plumbing and no built-in secret-injecting egress proxy. |
| Seatbelt (`sandbox-exec`) | process-level profile, no VM | **Kept as fallback** when Docker/`sbx` is unavailable, so the loop still runs confined (degraded). This is what Codex/Claude Code use locally. |
| NVIDIA OpenShell | Linux pod / enterprise | Rejected - Linux/enterprise-shaped, wrong fit for a macOS-native single-operator harness. |

`aida --agent` is a custom binary, not a built-in `sbx run` agent, so the integration
uses the documented custom path:
`docker sandbox run -d --name aida-<runID> --memory <n> <workspace>` then
`docker exec aida-<runID> sh -c "cd /work && aida --agent --run-dir …"`, torn down on
terminal state by the existing worktree-cleanup goroutine in `serve.go`. The egress
allowlist covers Anthropic, GitHub, Voyage, and the Notion host; everything else is
denied - completing the third defense layer for both human gates.

> **As shipped, this section describes the original design intent, not the
> current mechanism.** `internal/sandbox/sandbox.go` drives the tier with the
> real `sbx` CLI (`sbx create`/`sbx exec`/`sbx rm`), not `docker sandbox
> run`/`docker exec`, that alias only shows up as the tier's shorthand name.
> Teardown is a `defer cleanup()` around each spawn in `internal/cli/loop.go`,
> not a worktree-cleanup goroutine in `serve.go`. The Seatbelt fallback row was
> dropped (see the top-of-document note); tiers are `none`/`docker` only, and
> `none` is the fallback when `sbx` isn't installed. There is no host-side
> egress proxy, no allowlist, and no secret-header injection anywhere in the
> repo: `sbx create`/`sbx exec` gives filesystem and process isolation via the
> microVM only. `aida --agent` running confined is real (a cross-compiled
> linux/arm64 build is provisioned into the VM via `Policy.ProvisionBin`, since
> the macOS binary can't execute there), but the network-deny layer this
> section describes does not exist.

## 6. How it maps onto what already exists

This was the gap analysis that motivated building the system (originally framed as
"~70% built" against the substrate that existed before any of the seven phases
landed). Every row's gap has since been closed; see the "Gap the loop closes"
column below for what was actually shipped, and the implementation-status note at
the top of this document for the handful of deliberate deviations from the original
design:

| Capability | Already in `aida` (real path) | Gap the loop closed |
|------------|------------------------------|---------------------|
| Task list + priorities | `internal/brain/tasks.go`, `status.go` (6 statuses, `PriorityTag` p1/p2/p3) | tag `auto` as the handoff token, shipped |
| Fresh-context agent | `runAgentMode` in `internal/cli/agent.go` (`ask_user` pause→`awaiting_input`, polls `input.txt`; `emitComplete` carries `costUSD`) | `MemoryInstruction` injection; `request_approval` tool; sandbox wrap, all shipped |
| Jobs queue + worktrees | `internal/jobs/{manifest,store,db}.go`; `startPRWorkJob` worktree logic in `internal/jarvis/tools/jobs.go` | `awaiting_approval` state; governance fields (budget/timeout/allowlist/tier); shared `internal/worktree` helper, all shipped |
| Daemon watcher | `watchJobsForNotifications`/`scanJobsForNotifications` in `serve.go` (2s ticker, `MarkNotified` idempotency) | `case StateAwaitingApproval` added, shipped; the loop's own PR path doesn't route through it (see §3 caveat) |
| Eval / reviewers | `internal/eval/reviewer.go` (`Reviewer`, `Loop`, fail-loud `AggregateVerdict`, `DefaultReviewers`); `boosts.go`; `brain/eval_runs.go` | `CodeReviewer` (build/test/lint→Fail); iterate-to-green; adversarial panel, all shipped (panel's majority vote lives inline in `internal/cli/pr.go`, not a separate `eval.PanelVerdict`) |
| Notion ingest | `internal/adapters/notion.go` + `internal/cli/tasks_ingest.go` (`--from-notion --plan --auto-solve`, source-hash idempotency) | `ingest_notion_tasks` Jarvis voice tool, shipped (`internal/jarvis/tools/ingest.go`) |
| The driver | `aida loop` skeleton in `internal/cli/loop.go` (pick→recall→spawn→`--check` gate→commit→`distillLearning`) | per-iteration worktree isolation, cost/budget stop, `--daemon`, `--pr`, `--concurrency`, all shipped |
| Budget primitive | `cfg.Agent.MaxBudgetUSD` (`config.go:31`) | wired, read per-iteration `costUSD` from run-dir `events.ndjson` (`readRunCostUSD` in `loop.go`); shipped |
| Guardrails | `GuardrailsConfig.ConfirmAlways`; `confirmFn` fails closed in detached jobs (`agent_tools.go`) | `request_approval` tool shipped; no default merge/send patterns were added to `ConfirmAlways`, and the sandbox network layer was never built (see §3/§5 caveats) |
| Self-improvement | `aida brain analyze` read-only (`brain_analyze.go`); `golden.go` Judge | `--propose` (mine failures → print/`--out` a proposal) shipped; `--pr` (auto-open a gated PR) did **not** ship, turning a proposal into a PR is still manual |

## 7. Phase roadmap

One commit per phase, stacked on `feat/autonomous-loop`. Phases D and 0 (docs +
brain note) come first; 1 and 2 are independent; 3 needs a worktree; 5 needs 3; 6
follows 5 (network-deny completes the gate); 7 needs 3 + 5.

All phases below (D, 0, 1–7) plus three follow-ons shipped on `feat/autonomous-loop`
/ `feat/autoloop-build`, merged via PR #81 / PR #82. "New / changed surfaces" is
mostly accurate as a table of what landed; the **Status** column notes the handful
of deliberate deviations (detailed in the implementation-status note at the top of
this document and in SPEC.md).

| Phase | Title | Core deliverable | New / changed surfaces | Status |
|-------|-------|------------------|------------------------|--------|
| **D** | Documentation first | This `OVERVIEW.md` + `SPEC.md`, generated by a doc swarm | `docs/autonomous-loop/{OVERVIEW,SPEC}.md` | shipped |
| **0** | Fold harness specifics into brain note | 2026 frontier numbers + sandbox landscape into the knowledge base | `~/.aida/brain/knowledge/patterns/agent-harness-engineering.md` | shipped |
| **1** | Voice-triggered Notion ingest | `ingest_notion_tasks` voice tool; confirm-before-create; `--tag auto` | `internal/jarvis/tools/ingest.go`, `registry.go` | shipped |
| **2** | Code-correctness eval loop | `CodeReviewer` (build/test/lint→Fail); iterate-until-green; `MemoryInstruction` injection | `internal/eval/code.go`; `--max-fix-iterations`, repeatable `--check` | shipped; `agent.checks` config key did not ship (CLI-only) |
| **3** | Autonomous PR + review swarm | Un-gate `gh pr create` *inside the gated pipeline*; adversarial review panel | `internal/cli/pr.go`; `aida loop --pr --reviewer --base --review-panel N` | shipped; majority-vote lives inline in `pr.go`, no separate `eval.PanelVerdict` |
| **4** | `aida loop` → dispatcher | Per-iteration worktree off `origin/main`; `--daemon` continuous; horizontal `--concurrency`; wire `MaxBudgetUSD` | shared `internal/worktree`; `aida serve --loop`; `--max-budget-usd`, `--max-consecutive-failures` | shipped |
| **5** | HITL approval gate | `awaiting_approval` state; `request_approval` tool; voice `approve_job`/`reject_job` | `internal/jobs/*` (manifest bumped to v4, alongside Phase 6's fields); `agent.go`; `internal/jarvis/tools/approval.go`; `serve.go` switch | shipped as general-purpose infra; the loop's own `--pr` path parks on `hold` instead of routing through it (see §3 caveat) |
| **6** | Docker Sandboxes | `Wrap(cmd, policy)` via `sbx create`/`exec`/`rm`; per-job governance | `internal/sandbox/sandbox.go`; `--sandbox`, `--sandbox-memory`, `--provision-aida` | shipped with deviations: Seatbelt fallback dropped (tiers are `none`/`docker` only), no egress allowlist or secret injection (see §5 caveat) |
| **7** | `brain analyze` auto-proposals (follow-on) | `--propose` drafts golden tests + routing-boost diffs | `internal/cli/brain_analyze.go` | shipped; `--pr` (auto-open a gated PR) did not ship, `--propose`/`--out` print/write only |

See [`SPEC.md`](./SPEC.md) for the per-phase types, state machine, swarm protocol,
sandbox integration contract, and end-to-end verification matrix.
