#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-migration-numbers.sh — a migration file added on THIS branch must use
# a numeric prefix GREATER than every prefix already on origin/main.
#
# Why: two branches independently taking "the next number" (both picking
# 0069, say) each compile and pass their own PR's gate in isolation — the
# collision only appears at merge, when whichever lands second silently
# shadows or duplicates the first's number. Comparing against origin/main's
# ACTUAL max, rather than trusting the author's arithmetic, catches a branch
# that forked before a sibling merged and never rebased its number.
#
# It cannot see a SIBLING PR open at the same time that also claims the next
# free number and has not landed yet — both still pass this gate, since each
# only sees origin/main as it existed when it ran. That cross-PR collision is
# the job nightly.yml's octopus merge check exists for
# (scripts/nightly-migration-merge-check.sh), not this one.
#
# Run via `make lint`.
set -euo pipefail
cd "$(dirname "$0")/.."

MIGRATIONS_DIR="internal/db/migrations"

# Best-effort: this gate needs an origin/main to compare against. Offline, or
# a checkout with no 'origin' remote, degrades to a skip rather than a hard
# failure — it is a merge-collision early warning, not a source of truth no
# other gate already is, and `make lint` must still work with no network.
if ! git rev-parse --verify -q origin/main >/dev/null; then
  if ! git remote get-url origin >/dev/null 2>&1; then
    echo "check-migration-numbers: no 'origin' remote; skipping" >&2
    exit 0
  fi
  if ! git fetch --quiet origin main 2>/dev/null; then
    echo "check-migration-numbers: could not fetch origin/main; skipping" >&2
    exit 0
  fi
fi

# Every 4-digit prefix already on origin/main.
main_max=0
while IFS= read -r name; do
  [[ "$name" =~ ^([0-9]{4})_ ]] || continue
  prefix=$((10#${BASH_REMATCH[1]}))
  (( prefix > main_max )) && main_max=$prefix
done < <(git ls-tree --name-only -r origin/main -- "$MIGRATIONS_DIR" 2>/dev/null | xargs -r -n1 basename)

status=0
while IFS= read -r -d '' f; do
  base=$(basename "$f")
  [[ "$base" =~ ^([0-9]{4})_ ]] || continue
  # Already on origin/main (an unchanged pre-existing file) — not a new
  # migration this branch is adding, so it is not this gate's business.
  if git cat-file -e "origin/main:$f" 2>/dev/null; then
    continue
  fi
  prefix=$((10#${BASH_REMATCH[1]}))
  if (( prefix <= main_max )); then
    printf 'check-migration-numbers: %s uses prefix %s, which does not exceed origin/main'"'"'s current max (%04d). Renumber it to %04d or higher.\n' \
      "$f" "${BASH_REMATCH[1]}" "$main_max" "$((main_max + 1))" >&2
    status=1
  fi
done < <(find "$MIGRATIONS_DIR" -maxdepth 1 -name '*.sql' -print0)

if (( status == 0 )); then
  echo "check-migration-numbers: OK (origin/main max = $(printf '%04d' "$main_max"))"
fi
exit $status
