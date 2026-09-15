#!/usr/bin/env bash
# Computes the next release version and prints it to stdout.
#
# Rule: the latest release tag is the highest `v*` tag whose major version is
# >= 1 (v0.x tags are ignored entirely - versioning starts at v1.0.0). If no
# such tag exists, the next version is v1.0.0. Otherwise it's
# vMAJOR.(MINOR+1).0.
#
# Requires the full tag list to be present (e.g. `actions/checkout` with
# `fetch-depth: 0` and `fetch-tags: true`). Used by both the `changelog` and
# `release` jobs in .github/workflows/ci.yml so the two never drift.
set -euo pipefail

latest=$(git tag -l 'v*' | { grep -E '^v[1-9][0-9]*\.[0-9]+\.[0-9]+$' || true; } | sort -V | tail -1)

if [ -z "$latest" ]; then
  echo "v1.0.0"
else
  major=$(echo "$latest" | sed -E 's/^v([0-9]+)\..*/\1/')
  minor=$(echo "$latest" | sed -E 's/^v[0-9]+\.([0-9]+)\..*/\1/')
  echo "v${major}.$((minor + 1)).0"
fi
