#!/usr/bin/env bash
#
# fresh-start-cut.sh - builds the public export tree (step c of the
# fresh-start cut runbook, kept in the private repo) from the current HEAD
# of this repo's main branch. Safe by default: never touches GitHub, never
# pushes, never commits. Default mode is a dry run that only prints what
# it would do.
#
# Usage:
#   scripts/fresh-start-cut.sh              # dry run (no filesystem writes)
#   scripts/fresh-start-cut.sh --apply DIR  # build the export tree into DIR
#
# What it does (--apply):
#   1. Exports the tracked tree at the given ref (default: main) into DIR,
#      via `git archive` - no .git, no untracked/gitignored files.
#   2. Removes scripts/oss-scan.sh from the export.
#   3. Removes the "bash scripts/oss-scan.sh" step from the export's
#      .github/workflows/ci.yml (the file itself is kept and edited, not
#      deleted).
#   4. Drops scripts/fresh-start-cut/secrets-scan.yml into the export's
#      .github/workflows/secrets-scan.yml.
#   5. git-inits the export directory and stages everything (git add -A)
#      so the fresh-start cut runbook's step (d) can commit directly from
#      it - this script does NOT create that commit itself.
#   6. Verifies: no file named oss-scan.sh anywhere in the export, and
#      this repo's own scripts/oss-scan.sh (invoked against the export
#      tree, not copied there) finds nothing. The private patterns it
#      scans for are never duplicated in this script - see the note in
#      run_oss_scan_against_export() below.
#
# Never touches GitHub. Never runs `git push`. Never runs `git commit`.

set -euo pipefail

REF="${FRESH_CUT_REF:-main}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && git rev-parse --show-toplevel)"
TEMPLATE_DIR="$REPO_ROOT/scripts/fresh-start-cut"
SECRETS_SCAN_TEMPLATE="$TEMPLATE_DIR/secrets-scan.yml"
OSS_SCAN_SCRIPT="$REPO_ROOT/scripts/oss-scan.sh"
CI_WORKFLOW_REL=".github/workflows/ci.yml"
OSS_SCAN_STEP_PATTERN='^\s*-\s*run:\s*bash scripts/oss-scan\.sh\s*$'

mode="dry-run"
target_dir=""

while [ $# -gt 0 ]; do
  case "$1" in
    --apply)
      mode="apply"
      target_dir="${2:-}"
      if [ -z "$target_dir" ]; then
        echo "fresh-start-cut: --apply requires a target directory" >&2
        exit 1
      fi
      shift 2
      ;;
    -h|--help)
      sed -n '2,25p' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *)
      echo "fresh-start-cut: unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

if [ ! -f "$SECRETS_SCAN_TEMPLATE" ]; then
  echo "fresh-start-cut: missing template $SECRETS_SCAN_TEMPLATE" >&2
  exit 1
fi
if [ ! -x "$OSS_SCAN_SCRIPT" ] && [ ! -f "$OSS_SCAN_SCRIPT" ]; then
  echo "fresh-start-cut: missing $OSS_SCAN_SCRIPT" >&2
  exit 1
fi

if ! git -C "$REPO_ROOT" rev-parse --verify "$REF" >/dev/null 2>&1; then
  echo "fresh-start-cut: ref '$REF' not found in $REPO_ROOT" >&2
  exit 1
fi

# file_list <ref> - every tracked file at <ref>, one per line.
file_list() {
  git -C "$REPO_ROOT" ls-tree -r --name-only "$1"
}

# print_plan - shared summary used by both dry-run and apply, so the two
# modes can never silently drift apart.
print_plan() {
  echo "fresh-start-cut: plan for ref '$REF' (HEAD: $(git -C "$REPO_ROOT" rev-parse --short "$REF"))"
  echo
  echo "Remove from export:"
  echo "  - scripts/oss-scan.sh"
  echo
  echo "Edit in export:"
  echo "  - $CI_WORKFLOW_REL: drop the 'bash scripts/oss-scan.sh' step (file kept)"
  echo
  echo "Add to export:"
  echo "  - .github/workflows/secrets-scan.yml (from scripts/fresh-start-cut/secrets-scan.yml)"
  echo
}

# diff_vs_main - file list diff: what's removed/added relative to a
# straight export of $REF, independent of dry-run vs apply.
diff_vs_main() {
  local removed=("scripts/oss-scan.sh")
  local added=(".github/workflows/secrets-scan.yml")
  echo "File list diff vs a plain export of '$REF':"
  for f in "${removed[@]}"; do
    echo "  - $f"
  done
  for f in "${added[@]}"; do
    echo "  + $f"
  done
  echo
  echo "Total tracked files at $REF: $(file_list "$REF" | wc -l | tr -d ' ')"
}

# run_oss_scan_against_export <dir> - invokes THIS REPO's own
# scripts/oss-scan.sh, unmodified, with the export directory as its
# working tree. oss-scan.sh does `cd "$(git rev-parse --show-toplevel)"`
# as its first move and then `git ls-files` from there, so the export
# directory must itself be a git repo with the files staged/tracked
# (done in build_export() below) for this to see anything. This function
# never inlines any of oss-scan.sh's patterns - it only shells out to the
# private script itself, so the patterns it guards are never duplicated
# here (this script is meant to ship in the public tree; oss-scan.sh
# itself is stripped from that same tree in the same run).
run_oss_scan_against_export() {
  local dir="$1"
  echo "fresh-start-cut: running $OSS_SCAN_SCRIPT against $dir"
  ( cd "$dir" && bash "$OSS_SCAN_SCRIPT" )
}

# build_export <dir> - the --apply path.
build_export() {
  local dir="$1"

  if [ -e "$dir" ] && [ -n "$(ls -A "$dir" 2>/dev/null)" ]; then
    echo "fresh-start-cut: target '$dir' already exists and is not empty" >&2
    exit 1
  fi
  mkdir -p "$dir"

  echo "fresh-start-cut: exporting $REF into $dir"
  git -C "$REPO_ROOT" archive --format=tar "$REF" | tar -x -C "$dir"

  echo "fresh-start-cut: removing scripts/oss-scan.sh"
  rm -f "$dir/scripts/oss-scan.sh"

  local ci_file="$dir/$CI_WORKFLOW_REL"
  if [ -f "$ci_file" ]; then
    echo "fresh-start-cut: dropping the oss-scan step from $CI_WORKFLOW_REL"
    grep -vE "$OSS_SCAN_STEP_PATTERN" "$ci_file" > "$ci_file.tmp"
    mv "$ci_file.tmp" "$ci_file"
  else
    echo "fresh-start-cut: WARNING: $CI_WORKFLOW_REL not found in export, nothing to edit" >&2
  fi

  echo "fresh-start-cut: adding .github/workflows/secrets-scan.yml"
  mkdir -p "$dir/.github/workflows"
  cp "$SECRETS_SCAN_TEMPLATE" "$dir/.github/workflows/secrets-scan.yml"

  echo "fresh-start-cut: git-init'ing the export and staging everything"
  ( cd "$dir" && git init -q && git add -A )

  echo
  echo "fresh-start-cut: verifying"

  local stray
  stray="$(find "$dir" -name 'oss-scan.sh' 2>/dev/null || true)"
  if [ -n "$stray" ]; then
    echo "fresh-start-cut: FAIL: oss-scan.sh still present in export:" >&2
    echo "$stray" >&2
    exit 1
  fi
  echo "  - no file named oss-scan.sh in export: OK"

  if grep -RqE 'bash scripts/oss-scan\.sh' "$ci_file" 2>/dev/null; then
    echo "fresh-start-cut: FAIL: oss-scan step still present in $CI_WORKFLOW_REL" >&2
    exit 1
  fi
  echo "  - oss-scan step removed from $CI_WORKFLOW_REL: OK"

  if [ ! -f "$dir/.github/workflows/secrets-scan.yml" ]; then
    echo "fresh-start-cut: FAIL: secrets-scan.yml missing from export" >&2
    exit 1
  fi
  echo "  - secrets-scan.yml present: OK"

  if run_oss_scan_against_export "$dir"; then
    echo "  - private-pattern scan against export: OK (clean)"
  else
    echo "fresh-start-cut: FAIL: private-pattern scan found matches in the export (see above)" >&2
    exit 1
  fi

  echo
  echo "fresh-start-cut: export built at $dir (git-initialized, all files staged, not committed)."
  echo "fresh-start-cut: next step is the runbook's step (d): the initial commit."
}

case "$mode" in
  dry-run)
    print_plan
    diff_vs_main
    echo
    echo "fresh-start-cut: dry run only - nothing written. Re-run with --apply <target-dir> to build the export."
    ;;
  apply)
    print_plan
    build_export "$target_dir"
    ;;
esac
