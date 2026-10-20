#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-preflight-mutations.sh — proves scripts/test-lane-preflight.sh goes red when lane-preflight.sh
# --candidate stops catching one of the two kinds of defect it exists to find before hosted CI does:
#   1. a whole-tree guard (a test that reads docs, ui/, .github or walks the source) broken by a change
#      in another package;
#   2. a Postgres-only failure in a changed package, or in internal/api when the store under it changed.
# (Playwright is left to hosted ui-e2e.)
# Each mutant reopens one of them in a copy of lane-preflight.sh or lib/candidate-tests.py; the harness
# must fail on every mutant and pass on the unmutated copy. Daemon-free, like the harness it drives.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# harness SRC: run the preflight harness against the copies in SRC; its exit status.
harness() { PREFLIGHT_SRC="$1" bash "$ROOT/scripts/test-lane-preflight.sh" >"$TMP/harness.log" 2>&1; }

fresh() {
  rm -rf "$TMP/src"
  mkdir -p "$TMP/src/lib"
  cp "$ROOT/scripts/lane-preflight.sh" "$TMP/src/"
  cp "$ROOT/scripts/lib/common.sh" "$ROOT/scripts/lib/candidate-tests.py" "$TMP/src/lib/"
}

harness_ok() { fresh; harness "$TMP/src" || { tail -20 "$TMP/harness.log"; die "the harness fails on the unmutated copy"; }; }
harness_ok
echo "ok  the harness passes on the unmutated preflight"

# mutant LABEL FILE OLD NEW: replace exactly one OLD with NEW in the copy of FILE, then the harness must fail.
mutant() {
  local label=$1 file=$2 old=$3 new=$4
  fresh
  python3 -I - "$TMP/src/$file" "$old" "$new" <<'PY' || die "mutant '${label}' no longer applies: ${file} changed, update the mutant"
import sys
path, old, new = sys.argv[1:]
text = open(path).read()
if text.count(old) != 1:
    sys.exit(1)
open(path, "w").write(text.replace(old, new))
PY
  if harness "$TMP/src"; then die "mutant survived: ${label} (the harness stayed green)"; fi
  echo "ok  killed: ${label} ($(grep -m1 -E 'error|FAIL|die' "$TMP/harness.log" | head -c 160))"
}

# 1. whole-tree guards
mutant "1 guard list drops tests that walk the source" lib/candidate-tests.py \
  'if CI_GUARD.search(text) or TREE_READ.search(text):' 'if CI_GUARD.search(text):'
mutant "1 guards run only in the packages the change touched" lane-preflight.sh \
  '    IFS=$'"'"'\t'"'"' read -r dir names <<<"${line}"' \
  '    IFS=$'"'"'\t'"'"' read -r dir names <<<"${line}"; [[ "${changed}" == *"${dir}/"* ]] || continue'
mutant "1 a failing guard is ignored" lane-preflight.sh \
  'go test -count=1 -run "^(${names})\$" "./${dir}"' \
  'sh -c '"'"'go "$@" || true'"'"' sh test -count=1 -run "^(${names})\$" "./${dir}"'

# 2. Postgres-only failures
mutant "2 a store change no longer runs internal/api's Postgres tests" lane-preflight.sh \
  '[ -z "${api_src}" ] || pg_pkgs+=("./internal/api/...")' ':'
mutant "2 a broken selector is read as no Postgres tests" lane-preflight.sh \
  "elif grep -q 'no PG tests found' \"\${sel_err}\"; then" 'elif true; then'
mutant "2 the Postgres step runs no test" lane-preflight.sh \
  'go test -p 1 -count=1 -timeout 30m -run "${pg_re}" "${pg_pkgs[@]}"' 'true'

log "preflight mutations: PASS (every mutant killed)"
