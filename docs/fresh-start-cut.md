# The fresh-start cut runbook

One-shot, mostly-irreversible operation that turns the private development
repo `ryanlitalien/aida` into a renamed `ryanlitalien/aida-private` (still
writable, not archived, per the 2026-09-14 decision below) and a fresh
public `ryanlitalien/aida`, seeded from one scrubbed initial commit.
Background and rationale: `docs/plan-open-source.md` (Checklist, "Gated on
Ryan"; Part 7's docs/plans disposition table) and
`docs/diagrams/three-repo-cut.md`. Read both before running this.

This document is preparation only. Nothing in it has been run against the
real GitHub repos. Every command below is meant to be copy-pasted by Ryan,
in order, on the day of the cut.

## Conventions used below

- Each step is marked **[reversible]** or **[irreversible]**, with a
  verification command directly under it.
- `PRIVATE=ryanlitalien/aida-private` and `PUBLIC=ryanlitalien/aida` are
  used as shorthand; the private repo is `ryanlitalien/aida` until step (b)
  renames it.
- Commands assume the private checkout at `~/dev/aida` on edith (this
  machine) unless noted.
- Nothing here pushes on this session's behalf and nothing here runs the
  cut - `scripts/fresh-start-cut.sh` (deliverable 2) only builds the local
  export tree and touches no GitHub state.

---

## a. Preconditions

Run all of these before touching GitHub. Every one should pass; if any
fails, stop and fix it first.

1. **Main is green.**
   ```bash
   cd ~/dev/aida
   git fetch origin
   git log --oneline origin/main -1
   gh run list --repo ryanlitalien/aida --branch main --limit 1
   ```
   Verify: the latest `main` run's `STATUS`/`CONCLUSION` is `completed` /
   `success`.

2. **No open PRs you want left behind.** The cut freezes the private repo's
   working history and starts the public repo from one commit; any PR still
   open on `ryanlitalien/aida` either merges first or is consciously
   abandoned (it stays reachable in `aida-private`, just not in the public
   repo's history - and, since `aida-private` stays writable, it can also
   just keep going there after the cut).
   ```bash
   gh pr list --repo ryanlitalien/aida --state open
   ```
   Verify: manually review the list and either merge or explicitly decide
   to abandon each one. As of 2026-09-14 this list was PR #188 (this
   runbook's own tracking PR, expected to still be open at cut time), #186,
   #172, and draft #76 - confirm this list again on the actual cut day, it
   will have changed.

3. **`scripts/oss-scan.sh` is clean.**
   ```bash
   cd ~/dev/aida && bash scripts/oss-scan.sh
   ```
   Verify: prints `oss-scan: clean.` and exits 0.

4. **Working tree clean on all three machines.**
   ```bash
   # edith (this machine)
   cd ~/dev/aida && git status --porcelain
   # minty
   ssh minty 'cd ~/dev/aida && git status --porcelain'
   # photon
   ssh photon 'cd ~/dev/aida && git status --porcelain'
   ```
   Verify: all three print nothing. (`minty` and `photon` are resolved from
   `~/.aida/config.yaml`'s `devices:` block, which lists both with
   `probe: ssh` and no explicit `ssh_host` override, i.e. the tailnet/SSH
   config alias is the device name itself - confirm `ssh minty true` and
   `ssh photon true` succeed before relying on this.)

5. **Blog part 4 ready to publish the same day.** Per the checklist, part 4
   "ends with the announcement" and is meant to land alongside the cut.
   Verify by hand: the draft in `~/dev/aida-content/` is in its final,
   Ryan-reviewed state and Melissa's SEO/social pass is done. No command for
   this - it's a go/no-go read, not a machine check.

---

## b. Rename the private repo

**[irreversible in effect, technically reversible - see rollback]**

Per the 2026-09-14 decision, the private repo is renamed but stays
writable, not archived: open PR branches (e.g. this runbook's own tracking
PR) need to stay pushable after the cut.

GitHub redirects the old repo name (`ryanlitalien/aida`) to the new one
(`ryanlitalien/aida-private`) until some other repo claims the old name.
The public repo created in step (e) claims `ryanlitalien/aida`, which kills
that redirect at that moment. So this step must fully complete, and
anything depending on the old name (clone URLs, CI badges, bookmarks) must
be updated, **before** step (e) runs.

```bash
gh repo rename aida-private --repo ryanlitalien/aida
```

Verify:
```bash
gh repo view ryanlitalien/aida-private --json isArchived,name
```
Expect `"isArchived": false` and `"name": "aida-private"`.

Rollback: `gh repo rename aida --repo ryanlitalien/aida-private` undoes
this cleanly **as long as step (e) has not yet claimed the
`ryanlitalien/aida` name** - once the public repo exists at that name, the
rename-back would collide with it.

---

## c. Build the public tree

**[reversible - entirely local, no GitHub calls]**

Use `scripts/fresh-start-cut.sh` (deliverable 2 of this task, already in
this repo). It exports the tracked tree at `main`, removes
`scripts/oss-scan.sh`, removes that script's step from
`.github/workflows/ci.yml` (edits the file, does not delete it), and adds
`.github/workflows/secrets-scan.yml` from the template in
`scripts/fresh-start-cut/`.

```bash
cd ~/dev/aida
./scripts/fresh-start-cut.sh --apply /tmp/aida-public-export
```

Verify: the script's own final section already checks all of this and
exits non-zero on any failure -
- no file named `oss-scan.sh` anywhere in the export
- no `bash scripts/oss-scan.sh` line in the export's `ci.yml`
- `.github/workflows/secrets-scan.yml` present
- `~/dev/aida/scripts/oss-scan.sh` (the ORIGINAL, not copied into the
  export) run against the export tree reports `oss-scan: clean.`

Additionally, by hand:
```bash
cd /tmp/aida-public-export
go build ./... && go vet ./... && go test ./...
```
Verify: all three exit 0. (Confirmed clean during this session's dry run
into a scratch directory - see the Deliverables report for that
transcript.)

### What else stays private - findings

Per `docs/plan-open-source.md` Part 7, the docs/plans inventory and its
disposition (delete / trim / move-to-aida-config / keep) was already
executed in PR #185 (merged 2026-09-14, the commit before this branch was
cut). As of this session, `docs/` and `plans/` in `main` contain none of
the files Part 7 marked `delete` or `move-to-aida-config`
(`docs/agent-architecture-diagram.md`, `docs/aida-dispatcher-roster-design.md`,
`docs/aida-personal-roster-plan.md`, `docs/aida-vault-design.md`,
`docs/local-agent-structure-solutions.txt`, `docs/pipeline-diagram.png`,
`docs/work-machine-rename-checklist.md` - all absent, verified by `ls`).
The only remaining file the plan says must not reach the public tree is
`scripts/oss-scan.sh` itself (spotted 2026-09-13, per the checklist), which
`fresh-start-cut.sh` strips. **Re-run the Part 7 table against `main` at
cut time** in case something new landed between now and then - this
finding is current as of 2026-09-14, not a permanent guarantee.

---

## d. One initial commit

**[reversible until pushed in step (e) - local commit only]**

`fresh-start-cut.sh` already leaves the export directory `git init`'d with
everything staged (`git add -A`), so this step is just the commit itself.
Author it as the repo's own normal git identity, not an invented one:

```bash
cd ~/dev/aida
git log -1 --format='%an <%ae>' main
```
This printed `Ryan L'Italien <1479756+ryanlitalien@users.noreply.github.com>`
during this session - re-check at cut time in case it has changed, and use
whatever it prints, not the value hardcoded here.

```bash
cd /tmp/aida-public-export
cat <<'EOF' > /tmp/aida-initial-commit-msg.md
Initial public release

See HISTORY.md for the development timeline. This repo starts from a
single scrubbed commit; the original development history (600+ commits,
48 PRs between 2026-02-27 and this cut) stays private in the renamed
aida-private repo. Every PR number referenced in this repo's docs points
at that renamed private repo, not this repo - issue and PR numbering starts over here.
EOF
git -c user.name="Ryan L'Italien" \
    -c user.email="1479756+ryanlitalien@users.noreply.github.com" \
    commit -F /tmp/aida-initial-commit-msg.md
```

No `Co-Authored-By` trailer on this commit - the public repo carries no AI
attribution (per the repo's public-repo rule).

Verify:
```bash
git -C /tmp/aida-public-export log -1 --format='%an <%ae>%n%s%n%b'
git -C /tmp/aida-public-export log --oneline
```
Expect exactly one commit, authored as above, with no
`Co-Authored-By` line in the body.

---

## e. Create the public repo

**[irreversible once pushed and depended on - see rollback]**

```bash
cd /tmp/aida-public-export
gh repo create ryanlitalien/aida --public --source=. --remote=origin
git push origin main
gh repo edit ryanlitalien/aida --default-branch main
```

Copy over description and topics from the private (now renamed) repo.
Read them first and use the exact values - the description contains
characters (an em dash) that should be reproduced byte-for-byte from the
API, not retyped:

```bash
gh repo view ryanlitalien/aida-private --json description,repositoryTopics
```

As of this session that printed:
- `description`: a 297-character string starting `A CLI-first "agent of
  agents"` (fetch it live with the command above rather than trusting a
  copy pasted into this doc - retyping it risks silently changing the
  punctuation it contains)
- `repositoryTopics`: `null` (no topics currently set) - if none exist at
  cut time either, skip the topics copy step entirely.

```bash
gh repo edit ryanlitalien/aida --description "$(gh repo view ryanlitalien/aida-private --json description -q .description)"
# only if repositoryTopics is non-empty at cut time:
# gh repo edit ryanlitalien/aida --add-topic <topic1> --add-topic <topic2> ...
```

Branch protection: checked 2026-09-14, `main` on the private repo had
**no branch protection configured** (`gh api
repos/ryanlitalien/aida/branches/main/protection` returned 404 "Branch not
protected"). Re-check at cut time:
```bash
gh api repos/ryanlitalien/aida-private/branches/main/protection
```
If that still 404s, there is nothing to copy and this sub-step is a no-op.
If it now returns a protection object, translate its settings onto the
public repo with `gh api -X PUT repos/ryanlitalien/aida/branches/main/protection ...`
using the same required-checks/review settings, and record what was copied
by editing this line in the actual runbook run (not this template).

Verify:
```bash
gh repo view ryanlitalien/aida --json isPrivate,defaultBranchRef,description,repositoryTopics
```
Expect `isPrivate: false`, `defaultBranchRef.name: main`, description
matching the private repo's, and CI green on the just-pushed commit:
```bash
gh run list --repo ryanlitalien/aida --branch main --limit 1
```

Rollback: within minutes, if something leaked, `gh repo delete
ryanlitalien/aida --yes` removes the public repo outright (this also
reopens the option to rename `aida-private` back to `aida`, per step (b)'s
rollback note). Once anyone outside this session has cloned or starred it,
treat that as no longer a clean rollback - the leak already happened
regardless of whether the repo is deleted afterward.

---

## f. Re-point `origin` on all three machines

**[reversible - local git config only, no GitHub calls]**

Each machine's existing local checkout keeps its full private history
(unaffected by any of the above) but its `origin` remote now points at a
repo (`ryanlitalien/aida-private`) that holds only the old history. New work
must branch off the NEW public repo's `main`, which shares no commit
history with the old local `main`. The pattern on each machine: add a
`private` remote pointing at that renamed repo (so the old history stays
reachable for reference), re-point `origin` at the public repo, fetch, then
reset the local `main` to track the public repo's `main` (this discards
nothing - the old commits stay reachable via the `private` remote and via
reflog).

### edith (this machine)

```bash
cd ~/dev/aida
git remote rename origin private
git remote set-url private git@github.com:ryanlitalien/aida-private.git
git remote add origin git@github.com:ryanlitalien/aida.git
git fetch origin
git checkout -B main origin/main
git branch --set-upstream-to=origin/main main
```
Verify:
```bash
git remote -v
git rev-parse main
git rev-parse origin/main
```
Expect `origin` pointing at `ryanlitalien/aida.git`, `private` pointing at
`ryanlitalien/aida-private.git`, and `main` at the same SHA as
`origin/main` (the single initial commit from step d, or whatever `main`
has advanced to by cut time).

### minty

Reached via `ssh minty` (device `minty` in `~/.aida/config.yaml`, role
`bifrost-host`, `probe: ssh`, no `ssh_host` override - the SSH alias is the
bare device name). Confirm this resolves (`ssh minty true`) before running
the real thing; if it doesn't, minty's actual SSH host alias needs to be
looked up from `~/.ssh/config` on edith or from the Tailscale admin console
and substituted below.

```bash
ssh minty 'cd ~/dev/aida && \
  git remote rename origin private && \
  git remote set-url private git@github.com:ryanlitalien/aida-private.git && \
  git remote add origin git@github.com:ryanlitalien/aida.git && \
  git fetch origin && \
  git checkout -B main origin/main && \
  git branch --set-upstream-to=origin/main main && \
  git remote -v'
```
Verify: same as edith, remotely - the final `git remote -v` in the command
above should show `origin` at the public repo and `private` at `aida-private`.

### photon

Reached via `ssh photon` (device `photon` in `~/.aida/config.yaml`, role
`satellite`, `probe: ssh`, no `ssh_host` override - same alias convention
as minty). Same caveat: confirm `ssh photon true` resolves first.

```bash
ssh photon 'cd ~/dev/aida && \
  git remote rename origin private && \
  git remote set-url private git@github.com:ryanlitalien/aida-private.git && \
  git remote add origin git@github.com:ryanlitalien/aida.git && \
  git fetch origin && \
  git checkout -B main origin/main && \
  git branch --set-upstream-to=origin/main main && \
  git remote -v'
```
Verify: same pattern as minty.

**Note:** this session could not independently confirm `minty` and
`photon` resolve as literal SSH host aliases (no `~/.ssh/config` read was
in scope) - it is inferred from `~/.aida/config.yaml`'s `devices:` block
and the fact that `probe: ssh` entries with no `ssh_host` override use the
bare device name elsewhere in this codebase (contrast with `beast-wsl`,
which does set an explicit `ssh_host: beast-wsl`). Confirm on the day with
`ssh minty true` / `ssh photon true` before running the real re-point
commands - if either fails, this is a placeholder and the real alias needs
to be substituted.

---

## g. Post-cut

**[reversible - task/doc bookkeeping only]**

1. Close the tracking task. Per this task's instructions, it mirrors to
   GitHub Issues issue #390, titled "Make aida public (open source)", with
   a different local `#N` id per machine. **This session could not verify
   issue #390 exists** - `gh issue view 390 --repo ryanlitalien/aida` and
   `gh api repos/ryanlitalien/aida/issues/390` both 404'd, and a title
   search (`gh issue list --search "Make aida public"` / `--search
   "public"`) found nothing matching on 2026-09-14. Two live possibilities:
   the issue lives in a different repo than `ryanlitalien/aida`, or it was
   already closed and is no longer showing under the searches tried. Ryan
   should confirm the actual issue location/number before relying on this
   step - flagged in the Deliverables report too.
   ```bash
   aida tasks done '<slug-or-#N-for-this-machine>'
   ```
   Verify: `aida tasks show '<ref>'` shows `status: done`.

2. Update the plan doc status line. `docs/plan-open-source.md`'s header
   currently reads `**Status**: chunks A/B/C merged (PR #185, 2026-09-14);
   see Checklist`. Change it to reflect the cut, e.g. `**Status**: public
   repo live at github.com/ryanlitalien/aida (cut YYYY-MM-DD); see
   Checklist`, and check the two "Gated on Ryan" checklist boxes for the
   fresh-start cut and the oss-scan.sh stripping.

3. Publish blog part 4. Per the checklist this "ends with the
   announcement" - publish alongside the cut, not before (the announcement
   should link a repo that already exists and is green).

---

## h. Rollback

Summary of what is undoable at each step, gathered from the notes above:

| Step | Undoable? | How |
|---|---|---|
| (a) Preconditions | n/a | nothing was changed |
| (b) Rename | Yes, until (e) claims the `aida` name | `gh repo rename aida --repo ryanlitalien/aida-private` |
| (c) Build public tree | Yes, always | it's a local `/tmp` directory; `rm -rf` it and re-run the script |
| (d) Initial commit | Yes, until pushed in (e) | it's an uncommitted-to-GitHub local commit; discard the export directory |
| (e) Create + push public repo | Only within minutes, before anyone external sees it | `gh repo delete ryanlitalien/aida --yes`; if that's done, (b)'s rollback becomes available again too |
| (f) Re-point origin (all 3 machines) | Yes, always | `git remote remove origin; git remote rename private origin` restores the pre-cut state exactly, since nothing was deleted, only renamed/re-pointed |
| (g) Post-cut bookkeeping | Yes, always | reopen the task, revert the plan-doc status line edit |

The one point of no real return is the moment external users clone, star,
or fork the public repo, or the moment blog part 4 links it publicly -
before that, steps (b) through (f) can all be unwound in sequence (f, then
e, then b, in reverse order) with no data loss, because nothing destructive
happens to the private repo's content at any point - it is renamed,
never deleted.
