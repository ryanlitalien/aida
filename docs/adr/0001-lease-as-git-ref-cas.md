# ADR-0001: The task lease is a git ref compare-and-swap, not a task-file commit

Status: accepted 2026-09-24. Reversible: no flag; the alternative is not implemented.

## Context

Plan section 6 words the claim as "commit `status: in-progress, claimed_by, lease_until` to the task file and push; a rejected push means someone else holds it." The pros and cons review (pitfall 3) expects two hosts ticking at once to collide on that push. Ryan's decision 3 says: let claimers collide, detect it, loser backs off and retries, and it must hold for N machines.

A push of the brain branch is a poor lock primitive. The branch carries every other change on the machine (auto-commits, lessons, memory captures), so a rejected push says "your branch is behind", not "someone holds this task". Recovering means a rebase of unrelated commits, which is exactly the silent divergence `CommitAndPush` already fights. And when two machines race, both can succeed in sequence: A pushes, B pulls, rebases its claim on top of A's, pushes, and now the file says B holds it.

## Decision

The lease is a dedicated ref `refs/leases/<slug>` on the brain repo's remote. A claim pushes a tiny commit (one `LEASE.json` blob, built with `hash-object`, `mktree`, `commit-tree`, no working tree or index involved) with `--force-with-lease=refs/leases/<slug>:<expected>`. An empty expected means the ref must not exist (fresh claim); the current lease commit means take over an expired lease or renew our own. The server evaluates that condition atomically, so N claimers produce exactly one winner. A rejected push is the collision signal; the loser backs off with jittered exponential backoff and retries or moves to another task. A live, unexpired lease held by someone else is not a collision and is not retried.

The task file still gets `claimed_by` and `lease_until` mirrored into its frontmatter after the lease is won, so the file view matches the plan's wording and a human reading `tasks/` sees who has it. The mirror is informational; the ref is the truth.

## Consequences

Any git server that accepts arbitrary refs works (Forgejo and GitHub both do). `aida brain` sync never sees the lease refs because they are outside `refs/heads`. Expired leases are reclaimable without a human because expiry is in the blob and takeover is a CAS from the stale commit. The cost is one round trip per claim and one per renewal, which for an overnight loop is nothing.

Implementation: `internal/taskstate/lease.go`. Contention test: `TestClaimContention`.
