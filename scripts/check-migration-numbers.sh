#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-migration-numbers.sh — a migration file added on THIS branch must use
# a numeric prefix GREATER than every prefix already on the branch it targets
# (origin/main, or GITHUB_BASE_REF on a CI pull-request build).
#
# Why: two branches independently taking "the next number" (both picking
# 0069, say) each compile and pass their own PR's gate in isolation — the
# collision only appears at merge, when whichever lands second silently
# shadows or duplicates the first's number. Comparing against the base's
# ACTUAL max, rather than trusting the author's arithmetic, catches a branch
# that forked before a sibling merged and never rebased its number.
#
# It cannot see a SIBLING PR open at the same time that also claims the next
# free number and has not landed yet — both still pass this gate, since each
# only sees its base as it existed when it ran. That cross-PR collision is
# the job nightly.yml's migration-merge-check exists for
# (scripts/nightly-migration-merge-check.sh), not this one.
#
# Run via `make lint`.
set -euo pipefail
cd "$(dirname "$0")/.."

MIGRATIONS_DIR="internal/db/migrations"

# The branch to compare against. A CI pull-request build compares against the
# branch it targets (a patch PR to release/** takes the next number THERE, not
# above main's). A CI push build of any branch but main has no target to
# compare against — its migrations were checked on the PRs that brought them
# in — so it skips. Everything else (a local run, a push to main) compares
# against main; a local release-branch lane sets GITHUB_BASE_REF itself.
if [ -n "${GITHUB_BASE_REF:-}" ]; then
  base="$GITHUB_BASE_REF"
elif [ -n "${GITHUB_REF_NAME:-}" ] && [ "$GITHUB_REF_NAME" != main ]; then
  echo "check-migration-numbers: push build of $GITHUB_REF_NAME has no base branch to compare against; skipping" >&2
  exit 0
else
  base=main
fi
ref="refs/remotes/origin/$base"

# Best-effort: this gate needs the base branch to compare against. Offline, or
# a checkout with no 'origin' remote, degrades to a skip rather than a hard
# failure — it is a merge-collision early warning, not a source of truth no
# other gate already is, and `make lint` must still work with no network. The
# fetch names its destination, so a checkout with no default fetch refspec
# still ends up with the ref.
if ! git rev-parse --verify -q "$ref" >/dev/null; then
  if ! git remote get-url origin >/dev/null 2>&1; then
    echo "check-migration-numbers: no 'origin' remote; skipping" >&2
    exit 0
  fi
  if ! git fetch --quiet origin "+refs/heads/$base:$ref" 2>/dev/null; then
    echo "check-migration-numbers: could not fetch origin/$base; skipping" >&2
    exit 0
  fi
fi

# Every 4-digit prefix already on the base. A command substitution, so a
# failed ls-tree stops the gate instead of reading as "no migrations".
base_max=0
base_names="$(git ls-tree --name-only -r "$ref" -- "$MIGRATIONS_DIR")"
while IFS= read -r name; do
  [[ "$(basename "$name")" =~ ^([0-9]{4})_ ]] || continue
  prefix=$((10#${BASH_REMATCH[1]}))
  (( prefix > base_max )) && base_max=$prefix
done <<<"$base_names"

status=0
while IFS= read -r -d '' f; do
  name=$(basename "$f")
  [[ "$name" =~ ^([0-9]{4})_ ]] || continue
  # Already on the base (an unchanged pre-existing file) — not a new
  # migration this branch is adding, so it is not this gate's business.
  if git cat-file -e "$ref:$f" 2>/dev/null; then
    continue
  fi
  prefix=$((10#${BASH_REMATCH[1]}))
  if (( prefix <= base_max )); then
    printf 'check-migration-numbers: %s uses prefix %s, which does not exceed origin/%s'"'"'s current max (%04d). Renumber it to %04d or higher.\n' \
      "$f" "${BASH_REMATCH[1]}" "$base" "$base_max" "$((base_max + 1))" >&2
    status=1
  fi
done < <(find "$MIGRATIONS_DIR" -maxdepth 1 -name '*.sql' -print0)

if (( status == 0 )); then
  echo "check-migration-numbers: OK (origin/$base max = $(printf '%04d' "$base_max"))"
fi
exit $status
