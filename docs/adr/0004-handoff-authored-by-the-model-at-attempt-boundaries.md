# ADR-0004: HANDOFF.md is written by the model on harness demand; STATE.json only by the harness

Status: accepted 2026-09-24. Reversible: partially; the trigger threshold and max age are flags.

## Context

Ryan's decision 1: a task resumes on the new model, it does not restart, and the hand-off note is written while the original model still has full context. The aida loop's inner agent is a fresh process per attempt, so "full context" exists only inside an attempt. The harness cannot write what was tried and rejected; only the model knows that. The harness does know the task, the acceptance criteria, the lane, the attempt number and the worktree head.

## Decision

Split the file by who knows what. STATE.json is harness-only (plan section 6) and is what staleness is measured against. HANDOFF.md carries a small header the harness pre-fills (task, slug, lane, attempt, written, worktree head) plus Task and Acceptance sections the harness renders from the task itself; the model fills Done, Left, and Tried and rejected.

The harness arms the hand-off when the chosen lane is at or past 90 percent used on any of its windows, and then every attempt prompt on that lane opens with "write or refresh HANDOFF.md first, then continue". After each attempt the harness runs `taskstate.Check`: missing, malformed, written for an earlier attempt than STATE.json records, written against a different worktree head, or older than the max age are all errors that are logged and change what happens next. They are never ignored. On a lane change the next model gets the hand-off inlined as its brief (`ResumePrompt`); if the hand-off is missing or stale the next model gets `ReconstructPrompt` instead, which says to rebuild HANDOFF.md from `git log`, `git diff` and STATE.json before doing anything else, with the staleness reason verbatim.

## Consequences

A hand-off is only as good as the model's last refresh, which is why the check runs every attempt while armed, not once. The 90 percent threshold is per lane (`handoff_at_pct`) and the max age is a loop flag, so both can be tuned from the ledger without a code change.
