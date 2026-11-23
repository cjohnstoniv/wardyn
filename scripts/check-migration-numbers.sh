#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-migration-numbers.sh — a migration file added on THIS branch must take its number from the band of
# the branch it targets (origin/main, or GITHUB_BASE_REF on a CI pull-request build), above every prefix of
# that band already there; or be a byte-for-byte copy of a file its own release line already carries (a
# forward-port keeps its number). internal/db/migration_bands.tsv holds the bands.
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
# Then the table-overlap pass (scripts/lib/migration-overlap.py) over the band files this branch adds: band
# files on two lines reach a database in either order, so a new file may not touch a table a newer band
# altered, nor alter one an older band's not-yet-forwarded file touches.
#
# Run via `make lint`.
set -euo pipefail
cd "$(dirname "$0")/.."

MIGRATIONS_DIR="internal/db/migrations"
BANDS="internal/db/migration_bands.tsv"

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

[ -f "$BANDS" ] || { echo "check-migration-numbers: $BANDS is missing" >&2; exit 1; }
band_rows="$(grep -v -e '^#' -e '^$' "$BANDS" || true)"

# The open band whose branches name the base.
open_first="" open_last=""
while IFS=$'\t' read -r first last open branches _follows _tags; do
  [ "$open" = yes ] || continue
  IFS=, read -ra globs <<<"$branches"
  for g in "${globs[@]}"; do
    # shellcheck disable=SC2053 # $g is a glob on purpose
    if [[ $base == $g ]]; then
      [ -z "$open_first" ] || { echo "check-migration-numbers: $base matches two open bands in $BANDS" >&2; exit 1; }
      open_first=$((10#$first)) open_last=$((10#$last))
      break
    fi
  done
done <<<"$band_rows"
[ -n "$open_first" ] || {
  echo "check-migration-numbers: no open band in $BANDS takes base branch $base; give its line's row this branch" >&2
  exit 1
}
want=$(printf '%04d-%04d' "$open_first" "$open_last")

# band_of PREFIX: "first last tags branches" of the band holding PREFIX; returns 1 for a pre-band prefix.
band_of() {
  local first last open branches follows tags
  while IFS=$'\t' read -r first last open branches follows tags; do
    if (($1 >= 10#$first && $1 <= 10#$last)); then
      printf '%s %s %s %s\n' "$first" "$last" "$tags" "$branches"
      return 0
    fi
  done <<<"$band_rows"
  return 1
}

# forwarded FILE TAGGLOB BRANCHGLOBS: the first tag matching TAGGLOB (newest first), else the first
# origin/<branch> matching one of the comma-separated BRANCHGLOBS, that holds FILE byte for byte. Returns 1
# when none does.
forwarded() {
  local blob c b g gs
  blob=$(git hash-object -- "$1")
  while IFS= read -r c; do
    if [ -n "$c" ] && [ "$(git rev-parse -q --verify "$c:$1" 2>/dev/null)" = "$blob" ]; then
      echo "$c"
      return 0
    fi
  done < <(git tag -l --sort=-v:refname "$2")
  [ -n "$3" ] || return 1
  IFS=, read -ra gs <<<"$3"
  while IFS= read -r c; do
    b=${c#refs/remotes/origin/}
    for g in "${gs[@]}"; do
      # shellcheck disable=SC2053 # $g is a glob on purpose
      if [[ $b == $g ]] && [ "$(git rev-parse -q --verify "$c:$1" 2>/dev/null)" = "$blob" ]; then
        echo "origin/$b"
        return 0
      fi
    done
  done < <(git for-each-ref --format='%(refname)' refs/remotes/origin/)
  return 1
}

# The highest prefix of the open band already on the base. A command substitution, so a failed ls-tree
# stops the gate instead of reading as "no migrations".
base_max=$((open_first - 1))
base_names="$(git ls-tree --name-only -r "$ref" -- "$MIGRATIONS_DIR")"
while IFS= read -r name; do
  [[ "$(basename "$name")" =~ ^([0-9]{4})_ ]] || continue
  prefix=$((10#${BASH_REMATCH[1]}))
  ((prefix >= open_first && prefix <= open_last && prefix > base_max)) && base_max=$prefix
done <<<"$base_names"

status=0
band_new=()
while IFS= read -r -d '' f; do
  name=$(basename "$f")
  [[ "$name" =~ ^([0-9]{4})_ ]] || continue
  # Already on the base: not a migration this branch adds. It must still be the base's file byte for byte:
  # Migrate never re-runs a recorded name, so an edit would reach fresh installs only.
  if base_blob=$(git rev-parse -q --verify "$ref:$f" 2>/dev/null); then
    if [ "$base_blob" != "$(git hash-object -- "$f")" ]; then
      printf 'check-migration-numbers: %s differs from the copy on origin/%s. A migration the base has is never edited; add a new one.\n' "$f" "$base" >&2
      status=1
    fi
    continue
  fi
  digits=${BASH_REMATCH[1]}
  prefix=$((10#$digits))
  if ! read -r bfirst blast btags bbranches < <(band_of "$prefix"); then
    if hit=$(forwarded "$f" 'v*' ''); then
      echo "check-migration-numbers: $f is the pre-band file at $hit, byte for byte (a forward-port); its number stands" >&2
    else
      printf 'check-migration-numbers: %s uses prefix %s, which is below every band in %s; only a byte-for-byte copy of a released file may. origin/%s takes new migrations in %s.\n' \
        "$f" "$digits" "$BANDS" "$base" "$want" >&2
      status=1
    fi
    continue
  fi
  band_new+=("$f")
  if ((prefix >= open_first && prefix <= open_last)); then
    if ((prefix <= base_max)); then
      printf 'check-migration-numbers: %s uses prefix %s, which does not exceed the highest prefix of band %s on origin/%s (%04d). Renumber it to %04d or higher.\n' \
        "$f" "$digits" "$want" "$base" "$base_max" "$((base_max + 1))" >&2
      status=1
    fi
  elif [ "$btags" = - ]; then
    printf 'check-migration-numbers: %s is in band %s-%s, which origin/%s does not take (it takes %s), and no release line owns that band. Renumber it into %s.\n' \
      "$f" "$bfirst" "$blast" "$base" "$want" "$want" >&2
    status=1
  elif hit=$(forwarded "$f" "$btags" "$bbranches"); then
    echo "check-migration-numbers: $f is band $bfirst-$blast's file at $hit, byte for byte (a forward-port); its number stands" >&2
  else
    printf 'check-migration-numbers: %s is in band %s-%s, which origin/%s does not take (it takes %s). It passes only as a byte-for-byte copy of that file at a %s tag or on a %s branch, and none holds it (fetch the tags, or renumber it into %s).\n' \
      "$f" "$bfirst" "$blast" "$base" "$want" "$btags" "$bbranches" "$want" >&2
    status=1
  fi
done < <(find "$MIGRATIONS_DIR" -maxdepth 1 -name '*.sql' -print0)

# The table-overlap pass, against the base and each other line's own branch (each row's first branch) as this
# checkout has it. CI's checkout carries every branch; a line this checkout lacks is not read, and says so.
if ((${#band_new[@]})); then
  lines=()
  while IFS=$'\t' read -r _first _last _open branches _follows _tags; do
    line=${branches%%,*}
    [ "$line" != "$base" ] || continue
    case " ${lines[*]-} " in *" --line=refs/remotes/origin/$line "*) continue ;; esac
    if git rev-parse --verify -q "refs/remotes/origin/$line" >/dev/null; then
      lines+=("--line=refs/remotes/origin/$line")
    else
      echo "check-migration-numbers: origin/$line is not in this checkout; the overlap pass does not read that line" >&2
    fi
  done <<<"$band_rows"
  python3 -I scripts/lib/migration-overlap.py "$BANDS" "$ref" "${lines[@]+"${lines[@]}"}" -- "${band_new[@]}" || status=1
fi

if ((status == 0)); then
  printf 'check-migration-numbers: OK (origin/%s takes band %s, max %04d)\n' "$base" "$want" "$base_max"
fi
exit $status
