# Loop lifecycle: task → agent → gate → PR (never merge)

One task's path through `aida loop`. Full internals in
`docs/notes/loop-internals.md`.

```mermaid
flowchart TD
    T["task picked<br/>highest priority in the tagged set<br/>→ marked in-progress"]:::step
    T --> WT["worktree (optional)<br/>branch auto/#id-slug off origin/main<br/>± docker sandbox"]:::step
    WT --> RC["brain recall<br/>top-k similar lessons<br/>seeded into the prompt"]:::step
    RC --> AG["fresh-context agent<br/>one subprocess, one task,<br/>per-task timeout + budget metering"]:::step
    AG -- "every --check must exit 0" --> G{"all checks green?"}:::step

    G -- "fail (≤ max-fix attempts)" --> FX["continuation agent<br/>same worktree, prompt =<br/>the exact failing output"]:::step
    FX --> G
    G -- "still red / agent error" --> H(["parked on hold<br/>visible, never silently re-spun"]):::term

    G -- "green" --> D{"outcome?"}:::step
    D -- "default" --> DONE(["task done<br/>+ learning distilled into the brain"]):::term
    D -- "--pr" --> RP["adversarial review panel<br/>(optional) N reviewers must<br/>majority-approve"]:::step
    RP -- "rejected" --> H
    RP -- "approved" --> PR(["PR opened, reviewer tagged<br/>task on hold, tagged pr-open<br/>+ learning distilled"]):::term

    PR -. "human reviews and merges:<br/>the loop NEVER merges" .-> HU(["you"]):::term
    DONE -. "next similar task<br/>recalls the learning" .-> RC

    classDef step stroke-width:2.5px;
    classDef term stroke-dasharray: 5 4;
```

Three properties the shape encodes:

- **The gate is a loop, not a verdict.** A red check re-spawns a continuation
  agent with the failure text as its prompt, up to `--max-fix-iterations`
  times: iterate-until-green in the same worktree, then park on `hold` if it
  never greens. Failure is a visible state, not a retry storm.
- **The human sits at exactly two points**: reviewing the PR and merging it.
  Everything before that is autonomous but bounded (budget cap, consecutive-
  failure circuit breaker, per-task timeout, round cap).
- **The dashed return edge is the compounding loop**: each passing task's
  `## Learnings` section becomes an embedded brain lesson that the recall step
  feeds to the next similar task's fresh agent.
