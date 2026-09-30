# The arbiter: one plan

**Status**: consolidated 2026-09-17 from aida tasks #481, #497, #498, the model-usage research and burn-down design on PR #172 (`docs/research/model-usage-burndown/`), and the 2026-09-17 design conversation. Private until the code lands; the public repo gets the docs with the implementation. Owner: Ryan. First-pass verification and wave briefs: Fitz.

## 1. What it is

The arbiter grants scarce model capacity to one requestor at a time by policy, in the bus-arbitration sense (PR #172 named it deliberately: not a router, not a proxy, not a gateway). It answers two questions every time a task wants to run: which lane has headroom for it, and is now the time. It runs on minty, always on, inside `aida serve`, and it is host-agnostic: nothing about it depends on edith being awake.

Non-goals, carried forward from #172: it is not a session router for interactive work; it never merges; it never changes infrastructure unattended; it never puts personal or ButterStack data on a lane whose data class forbids it; and it never chooses new paid spend on its own. Spend is a dial Ryan turns; the arbiter suggests levers, it does not pull them.

## 2. What exists

| Piece | Where | State |
|---|---|---|
| Provider usage probes, nicknames, `aida models`, dashboard Models panel | `internal/models` | shipped |
| Burn pace per window, "empty in X" verdicts | `internal/models/pace.go` | shipped |
| Capacity floors, headroom, drain-to-zero, `aida burndown capacity`, `~/.aida/burndown.yaml` | `internal/burndown` | shipped |
| Dispatcher loop: pick, recall, run, `--check`, retry, hold, `--pr`, worktrees, concurrency, budget, breaker, daemon | `aida loop`, `aida serve --loop` | shipped |
| Runners: lane 4 herdr on minty (`aida fleet start`), docker agents on LiteLLM keys, the bake-off phase launcher | `aida-agents`, `~/bin/launch-funding.sh` on minty | shipped, ad hoc |
| Tasks as the queue, six statuses, MCP tools | `aida tasks`, `internal/mcp` | shipped |
| Jobs store, Jarvis notifier, dashboard | `internal/jobs`, `/dashboard` | shipped |
| Lane roster, data class gate, picker, routing eval, signals, ledger, runners | `internal/arbiter`, `~/.aida/lanes.yaml` (`examples/lanes.yaml`) | shipped 2026-09-24 |
| STATE.json, acceptance-as-done, HANDOFF.md with staleness check, claim with lease | `internal/taskstate` | shipped 2026-09-24 |
| Brain read-side fetch and fast-forward before a pick (#498) | `internal/brain/sync_ff.go` | shipped 2026-09-24 |
| Lane enforcement in the loop (`aida loop --arbiter`, `aida serve --loop-arbiter`) | `internal/cli/loop_arbiter.go` | shipped 2026-09-24, off by default (ADR-0002) |
| `aida arbiter lanes|plan|signal|signals|handoff|ledger` | `internal/cli/arbiter.go` | shipped 2026-09-24 |

The sensing half is done. Of the deciding half, sections 3, 5 (the signal ingest side), 6 and 7 are built; section 4 (the scheduler in `aida serve --arbiter`), the hooks themselves, lanes 3 to 5 end to end, and section 8 steps 0, 4, 5, 7 and 8 are not. Decisions that were open when this was written are recorded under `docs/adr/`.

## 3. Lanes, cheapest first (Ryan, 2026-09-17)

| # | Lane | Runner | Credential | Window |
|---|---|---|---|---|
| 1 | Claude Max, personal | `claude` on minty under herdr | personal login config dir | 5-hour rolling, 7-day (Thu 2pm ET) |
| 2 | Claude Pro, ButterStack | `claude-bs` (own config dir) | hello@ login | 5-hour, 7-day (4am) |
| 3 | Gemini via Antigravity | `agy -i` (never `-p`, it has a 5-minute cap) | Google login | 5-hour, 7-day |
| 4 | Codex Plus | `codex exec` with stdin closed | ChatGPT login | 5-hour, 7-day; banked resets are manual levers |
| 5 | qwen on EC2 | `claude` with `ANTHROPIC_BASE_URL` at LiteLLM, model `qwen` | AWS credits; IAM scoped to start/stop/describe one instance | wake, run, stop |
| 6 | LiteLLM budget keys (Anthropic API, Bedrock, cheap providers) | docker agents or `claude` via the proxy | `op.env` keys with hard budgets | real dollars, last |

Lane 5 detail: a g6e.xlarge (L40S 48 GB, about $1.86 per hour) or a g6.xlarge (L4 24 GB, about $0.80 per hour) on the Deep Learning AMI, Ollama or vLLM serving a 4-bit 27B-class Qwen from a persistent EBS volume, Tailscale on the instance, security group closed to the internet. LiteLLM on minty is the door, so no client config changes. The lifecycle script starts the instance, polls health, runs the job, stops the instance; a cron on minty stops it after 20 idle minutes; a per-wake hour cap and a monthly hour budget are hard limits. Nothing else lives on the instance: no repo, no state, no third sync spoke.

Data class gate (from #172, unchanged): `project:finances` and `personal` tags can only match personal-ceiling lanes; ButterStack coding burns the hello@ seat to its floor before any company-money lane; credentials never cross lanes.

## 4. Overnight scheduling

Most windows are five hours, so midnight to 8am holds about two full windows per subscription, plus the ButterStack Pro weekly reset at 4am. The scheduler fills windows as they refill: it wakes on a plan (23:00 nightly, plus the last 24 hours before each weekly reset), builds a wave from tasks that carry a machine verifier, dispatches to the cheapest lane with headroom above its floor, and re-plans whenever a lane reports empty or a window resets. Floors keep Ryan's daytime quota; overnight floors drop to zero only for windows that reset before he wakes.

## 5. Signals in, decisions out

Hooks and wrappers only signal; the arbiter decides.

- A Claude Code `Stop` hook on any host reads the last assistant message; if it is the usage-limit refusal, it posts "lane empty" to the arbiter. Open question, must be tested first: whether `Stop` fires on a limit refusal or only on a normal turn end. The test is to exhaust a 5-hour window on purpose and watch the hook log.
- The phase launcher and `aida fleet` wrappers do the same from the exit code and output of `claude -p`, `codex exec`, and `agy`.
- A `UserPromptSubmit` hook warns (or blocks) when the weekly window is past 95 percent, so an interactive session can start on another lane before the cliff.
- On "lane empty" the arbiter re-queues the task with `retry_after` and `last_lane`, and hands it to the next lane with "continue from the recorded state".

## 6. State and hand-off (built 2026-09-24; see ADR-0001 and ADR-0004)

These mechanics came out of the 2026-09-17 conversation. Ryan settled the three contested points on 2026-09-24: a task resumes on the new lane from HANDOFF.md, never restarts; an ambiguous rate-limit signal fails open (the lane is treated as empty and every such mark is logged so the false-positive rate is measurable); lease contention is resolved by detecting the collision and backing off with jitter, for any number of machines. The lease is a git ref compare-and-swap rather than a task-file commit (ADR-0001); the task file mirrors `claimed_by` and `lease_until` for humans.

- Per task, a structured `STATE.json` written by the harness (a `PostToolUse` or `Stop` hook), never by the model: phase, step, lane, started, updated, next_action. `LOG.md` stays the human narrative; nothing decides from it.
- Done means acceptance: the named deliverables exist and the `--check` command passes. A `DONE` string is a claim, not a fact.
- Claim with lease on the task file: commit `status: in-progress, claimed_by: <lane>@<host>, lease_until: <ts>` and push; a rejected push means someone else holds it. One arbiter process is the only consumer.
- Git as the ledger: each step commits; a resumer reads `git log` plus `STATE.json`; steps are re-runnable so the last one can be redone rather than guessed.
- No broker yet. Revisit (SQS on credits, or NATS or Redis Streams on minty) only when there is more than one dispatcher, sub-second dispatch, or streaming progress.
- The brain's read side must fetch and fast-forward (or refuse to pick when the remote is ahead) before listing or claiming, the twin of the pull that `AddTask` already does (#498).

The pattern that made hand-offs work in the 2026-09-15 bake-off is the template: a brief file per phase, a folder per worker, and "read the brief and the log, continue from the last line" as the only instruction any lane needs.

## 7. Ledger and morning summary (from #172)

Every run appends one JSON line: task, wave id, correlation id, lane, model, effort, window percent before and after per bar, tokens, wall time, verifier result, PR URL, cost for metered lanes. Success metric: percent of each window consumed at reset, per account, before and after the arbiter. One utterance and one dashboard card per wave in the morning.

## 8. Build order

0. Calibrate: five tasks by hand through `aida loop --pr --check "make test"` on minty on Sonnet 5 medium; record window deltas with `aida models --fresh` before and after. Count how many tasks carry a machine verifier; if few, backlog composition is the bottleneck. (Partly done informally by the 2026-09-15 bake-off; needs the numbers written down.)
1. Verification pass (Fitz): section 6 against the loop and brain code; the `Stop` hook test; the Codex headless-login question.
2. Picker plus `aida burndown plan` (dry run: print the wave and the lane per task, run nothing). Lane config validated at load time: a subscription-login lane may only run through the vendor's own CLI. Done 2026-09-24 as `aida arbiter plan` and `aida arbiter lanes --lint`.
3. Claim with lease, `STATE.json`, acceptance-as-done, the brain read-side pull. `aida serve --loop` and the arbiter must never dispatch over the same task at once. Done 2026-09-24 (`internal/taskstate`, `aida loop --arbiter`); the lease ref is what keeps two dispatchers off one task.
4. Scheduler in `aida serve --arbiter` on minty, ledger, morning summary, preflight of lane 1 before a wave (speak an alert if the OAuth session expired).
5. Signals: the `Stop` and `UserPromptSubmit` hooks, the launcher wrappers. The ingest side (`aida arbiter signal`, fail open, logged) is done 2026-09-24; wiring the hooks into `~/.claude/settings.json` and the wrappers is not.
6. Lanes 3 and 4 wired through the same runner contract.
7. Lane 5: the EC2 instance, the LiteLLM route, the lifecycle script, the idle cron, the caps.
8. Remote trigger (`aida burndown start` over ssh, MCP tool, dashboard card, phone page) and the weekly data refresh.

## 9. Worked examples that motivated this

- 2026-09-13: Fable burned on chunk A/C code sweeps despite `models.yaml` saying Sonnet.
- 2026-09-15/16: the funding bake-off lost the ButterStack Pro lane to its weekly cap mid-phase and the agy lane to the 5-minute `-p` cap; both recovered by hand, one on the personal Max plan, one by switching agy to interactive mode.
- 2026-09-11: a `life-log` fundraising file reference leaked into a butter_stack run, the incident behind the context-plane gate.

## 10. Tracking

aida task #481 is the parent (mirror: aida-brain issue #492); #497 (lanes and scheduling) and #498 (brain read-side pull) are its children. PR #172 stays as the research record. This file is the plan of record; when the implementation ships to the public repo, this doc goes with it.
