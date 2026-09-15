#!/bin/bash
# Runs every question in ~/.aida/golden-questions.txt through `aida` and
# writes a markdown report to ~/.aida/golden.md. Each entry records the
# question, the routing line, the run id, and the cleaned-up answer text.
# Used to audit routing accuracy across many questions at once --
# complementary to `aida golden run` which validates routing+answer
# assertions for a small fixed suite.
#
# The questions file (and this script's report) are personal seed data,
# not part of this repo: `aida init` seeds ~/.aida/golden-questions.txt
# from the fictional examples/golden/questions.txt set so this runs out of
# the box; replace it with your own real questions once you have real
# sources configured.
#
# Usage:
#   ./scripts/run-goldens.sh                              # default paths
#   AIDA=/path/to/aida ./scripts/run-goldens.sh           # override aida binary
#   QUESTIONS=other.txt ./scripts/run-goldens.sh          # override question file
#   OUT=/tmp/foo.md ./scripts/run-goldens.sh              # override output file
#
# This script does NOT use GNU `timeout` because macOS doesn't ship it
# by default. It relies on aida's own per-adapter timeouts (sqlite=30s,
# chrono=60s, claude-project=120s, etc.).
set +e

# Resolve aida binary: env override -> ~/go/bin/aida -> ~/bin/aida -> PATH.
if [ -z "${AIDA:-}" ]; then
  if [ -x "$HOME/go/bin/aida" ]; then
    AIDA="$HOME/go/bin/aida"
  elif [ -x "$HOME/bin/aida" ]; then
    AIDA="$HOME/bin/aida"
  elif command -v aida >/dev/null 2>&1; then
    AIDA="$(command -v aida)"
  else
    echo "ERROR: aida binary not found. Set AIDA=/path/to/aida or add it to PATH." >&2
    exit 1
  fi
fi

QUESTIONS="${QUESTIONS:-$HOME/.aida/golden-questions.txt}"
OUT="${OUT:-$HOME/.aida/golden.md}"

if [ ! -f "$QUESTIONS" ]; then
  echo "ERROR: questions file not found: $QUESTIONS" >&2
  exit 1
fi

# Header
{
  echo "# Aida Golden Question Run"
  echo ""
  echo "Generated: $(date '+%Y-%m-%d %H:%M:%S %Z')"
  printf 'Library state: '
  "$AIDA" library list 2>&1 | grep -oE 'layers:[0-9]+ +skills:[0-9]+ +sources:[0-9]+' | head -1
  echo ""
  echo "Each entry below records what the LLM router actually picked, what"
  echo "aida synthesized, and a run id. Inspect any run with: \`aida tune <run-id>\`"
  echo ""
  echo "---"
  echo ""
} > "$OUT"

i=0
total=$(grep -c '' "$QUESTIONS")
while IFS= read -r question || [ -n "$question" ]; do
  i=$((i + 1))
  [ -z "$question" ] && continue
  echo "[$i/$total] $question" >&2

  raw=$("$AIDA" "$question" 2>&1)
  ec=$?

  # Strip ANSI escape codes (spinner output) for clean parsing.
  clean=$(printf '%s' "$raw" | sed -E 's/\x1b\[[^m]*m//g; s/\x1b\[K//g')

  routing=$(printf '%s' "$clean" | grep -E '^🔀 Routing|^⚠️|^📂 Profile' | head -3 | tr '\n' ' | ')
  if [ -z "$routing" ]; then
    routing="(no routing line found)"
  fi

  # Pull the answer block: lines after the first "Answer" header until "Timing".
  answer=$(printf '%s' "$clean" | awk '
    /^Answer$/ { in_answer=1; next }
    /^Timing:/ { in_answer=0 }
    in_answer && !/^─/ { print }
  ' | sed '/^$/N;/^\n$/D')

  if [ -z "$answer" ]; then
    answer="(no answer block produced)"
  fi

  # Find the latest run id (best-effort -- this race-walks ~/.aida/runs/
  # and grabs the file with the newest mtime, which is almost always the
  # one we just created).
  run_id=$(ls -t "$HOME/.aida/runs/"*.json 2>/dev/null | head -1 | xargs basename 2>/dev/null | sed 's/\.json$//')

  {
    echo "## $i. $question"
    echo ""
    echo "**Routing:** \`$routing\`"
    echo ""
    if [ -n "$run_id" ]; then
      echo "**Run id:** \`$run_id\`"
      echo ""
    fi
    if [ "$ec" -eq 124 ]; then
      echo "**Status:** ⏱ TIMEOUT"
      echo ""
    elif [ "$ec" -ne 0 ]; then
      echo "**Status:** ⚠ exit $ec"
      echo ""
    fi
    echo "**Answer:**"
    echo ""
    echo "$answer" | sed 's/^/> /'
    echo ""
    echo "---"
    echo ""
  } >> "$OUT"
done < "$QUESTIONS"

echo "Done. Wrote $OUT" >&2
