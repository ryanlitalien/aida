# Arbiter: how to kick it off

Status: written 2026-09-25 alongside the implementation on this branch. Companion to `docs/arbiter-plan.md` (the design) and `docs/adr/` (the decisions).

## What the phrases map to

Nothing maps plain English to the arbiter yet. Jarvis and `aida <query>` have no arbiter intent, so each phrase below is something you say to a Claude session (or type as the command it stands for). Wiring a voice intent is a follow-up.

The arbiter picks the lane, meaning which credential and which CLI a task runs on. It does not pick a persona. Mack, Teddy, Phil and the BSG producer are roster personas; today the loop runs a generic agent inside the chosen lane. "Send it to Mack" therefore means "tag it so it lands on Mack's lane". A persona per tag is also a follow-up.

Lane enforcement is on by default now (2026-09-25), so `aida loop` and `aida serve --loop` already run through the arbiter without naming `--arbiter` on the command line; the examples below leave it off for that reason. `--arbiter=false` (or `--loop-arbiter=false` on `aida serve`) reverts a single invocation to the old always-metered `aida --agent` path.

## Personal work (the aida repo, Mack's health backlog)

"What would the arbiter do with my aida tasks tonight?"

```
aida arbiter plan --tag project:aida
```

"Dry-run the overnight loop on the arbiter, tests as the gate."

```
aida loop --tag project:aida --check "make test" --worktree --dry-run
```

"Run the loop for real on the cheapest lane, one task at a time."

```
aida loop --tag project:aida --check "make test" --worktree --pr
```

## Company and games work (ButterStack, Butter Smooth Games)

"Plan tonight's ButterStack wave on the company seat."

```
aida arbiter plan --tag project:butterstack
```

"Dry-run Mack's health backlog on my personal plan."

```
aida loop --tag health --check "make test" --worktree --dry-run
```

"Run the Pilot Light tasks wherever is cheapest."

```
aida loop --tag project:plt --check "godot --headless --export-pack ..." --worktree --pr
```

## The dry-run flow

`aida arbiter plan` runs nothing, ever. `aida loop --dry-run` stops before any claim, so it never pushes a lease ref, never writes STATE.json, never spawns an agent.

```
aida arbiter plan --tag project:aida            (runs nothing, ever)
  |
  |-- load ~/.aida/lanes.yaml  (missing -> error naming the path; copy examples/lanes.yaml there)
  |-- probe capacity: models.yaml + burndown.yaml -> headroom per window
  |-- read ~/.aida/arbiter/lane-state.json  (lanes marked empty)
  |-- list open + in-progress tasks with the tag, p1 first
  |
  v
TASK  CLASS     LANE          MODEL            NEAR-EXHAUSTION  REASON
#481  personal  claude-max    claude-sonnet-5  no               eligible
#497  personal  codex-plus    gpt-5.6-terra    no               claude-max: 5-hour headroom 3.0% < min 5.0%
#498  personal  none          -                -                claude-max: signalled empty until ...; codex-plus: ...
  |
  v
footer: 1 on claude-max, 1 on codex-plus, 1 with no lane


aida loop ... --dry-run                          (stops before any claim)
  |
  |-- fetch + fast-forward brain repo   (remote ahead -> refuse this round)
  |-- pick candidates (3 x concurrency), skip ones leased elsewhere
  |
  v
  [dry-run] work tasks via the arbiter: 3 candidate task(s)
  [dry-run] would claim + spawn on the cheapest eligible lane + run the code gate
  (exit; no lease pushed, no STATE.json, no agent)
```

What a real run adds after that line: claim `refs/leases/<slug>`, STATE.json claimed, pick a lane per attempt, run on that lane's CLI, and if the run says "usage limit" the lane is marked empty and the next lane resumes from HANDOFF.md. The gate is `--check` plus any named deliverables. On pass a PR opens, the task goes on hold, and a ledger line is written.

## One pass with all three classes

```
aida arbiter plan --tag project:butterstack --tag health --tag project:plt
  |
  |-- lanes.yaml -> 6 lanes cheapest first
  |-- probe: Max 5h/7d, Pro(BS) 5h/7d, Codex 5h/7d, LiteLLM $ row
  |-- tag -> data class (ADR-0003):
  |      project:butterstack -> butterstack     (company data)
  |      health              -> personal        (Mack; untagged also = personal)
  |      project:plt         -> games           (Butter Smooth Games)
  |
  v
TASK  CLASS        LANE           MODEL            NEAR-EXH  REASON
#512  butterstack  claude-pro-bs  claude-sonnet-5  no        claude-max: data class "butterstack" not allowed (allows personal, games, public)
#530  personal     claude-max     claude-sonnet-5  yes(91%)  eligible  -> HANDOFF.md armed on this lane
#544  games        codex-plus     gpt-5.6-terra    no        claude-max: 5-hour headroom 2.0% < min 5.0%
#545  games        litellm        claude-sonnet-5  no        claude-max, codex-plus: signalled empty; games may use litellm
#513  butterstack  none           -                -         claude-pro-bs: 7-day headroom 0% (floor 15); no other lane allows butterstack with headroom
  |
  v
footer: claude-pro-bs 1, claude-max 1, codex-plus 1, litellm 1, no lane 1
```

Note: `--tag` on `aida arbiter plan` and `aida loop` means a task must carry ALL listed tags, so the one-pass example above is illustrative; run one `plan` per tag to see each class, or omit `--tag` to see everything.

## What each class can never do

ButterStack never touches the personal Max or Codex login. Personal (Mack, the aida repo, finances) never touches the hello@ seat, LiteLLM, or EC2. Games may use any lane, so they drain the cheap ones first.

## Before the first real run

1. `aida arbiter lanes --lint` to confirm the roster loads and validates.
2. `aida arbiter plan` with no tag and check the CLASS column against the tags; a ButterStack row showing `claude-max` in LANE means a task is missing `project:butterstack`.
3. `aida loop ... --dry-run` once, then drop `--dry-run`.
