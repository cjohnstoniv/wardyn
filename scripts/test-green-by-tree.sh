#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Pins scripts/green-by-tree.sh: a PATH-shim `gh` serves canned JSON per API
# endpoint, and the real script runs against it. The cases:
#   1. green: exit 0, stdout holds exactly the four id lines
#   2. a marker from another repo id                  -> 1
#   3. conclusion failure, and a cancelled run that still carries the marker -> 1
#   4. a marker for T on a run whose head tree is not T -> 1
#   5. a required context missing from the jobs       -> 1, naming it
#   6. a watched nightly job missing                  -> 1, naming it
#   7. no `multi-arch build (` rows                   -> 1
#   8. only a scheduled nightly is green, NEED_STAGING 0 and 1 -> 1, and the
#      nightly query asks for workflow_dispatch runs only
#   9. gh exits non-zero                              -> 2
#  10. the script text has no `|| true` and no `|| :`
# plus: an expired marker, a wrong workflow path, a newer bad candidate in front
# of an older good one, a nightly on another tree or from a fork, and usage errors.
# Daemon-free, network-free.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/scripts/green-by-tree.sh"
[ -x "$SCRIPT" ] || { echo "FAIL: $SCRIPT is missing or not executable"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"

# The shim: `gh api [-X GET] [--paginate] <endpoint> [-f k=v ...]` prints
# $FIX/<endpoint with / as _>.json, and appends its arguments to $FIX/calls.log.
cat >"$WORK/bin/gh" <<'SHIM'
#!/usr/bin/env bash
[ "${GH_SHIM_FAIL:-0}" = 1 ] && { echo "gh: HTTP 500" >&2; exit 1; }
printf '%s\n' "$*" >>"$FIX/calls.log"
ep=""
shift # api
while [ $# -gt 0 ]; do
  case "$1" in
    -X|-f|-F) shift 2 ;;
    --*) shift ;;
    *) ep="$1"; shift ;;
  esac
done
f="$FIX/$(printf '%s' "$ep" | tr '/' '_').json"
[ -f "$f" ] || { echo "gh: no fixture for $ep (HTTP 404)" >&2; exit 1; }
cat "$f"
SHIM
chmod +x "$WORK/bin/gh"

R=acme/wardyn
SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
T=1111111111111111111111111111111111111111
OTHER=2222222222222222222222222222222222222222
REPO_ID=4242
WATCHED="w1 w2"

fail=0
ok()  { echo "ok:   $1"; }
bad() { echo "FAIL: $1"; fail=1; }

fixture() { printf '%s/%s.json' "$FIX" "$(printf '%s' "$1" | tr '/' '_')"; }
put() { printf '%s\n' "$2" >"$(fixture "$1")"; }

jobs_json() {  # names... -> {"jobs":[{"name":n,"conclusion":"success"}...]}
  local out="" n
  for n in "$@"; do out+="{\"name\":\"$n\",\"conclusion\":\"success\"},"; done
  printf '{"jobs":[%s]}' "${out%,}"
}

baseline() {  # baseline <label>: a fresh fixture dir with one green CI run (500) and one green nightly (700)
  FIX="$WORK/fix.$1"; export FIX
  mkdir -p "$FIX"
  put "repos/$R/git/commits/$SHA" "{\"tree\":{\"sha\":\"$T\"}}"
  put "repos/$R" "{\"id\":$REPO_ID}"
  put "repos/$R/branches/main" '{"protection":{"required_status_checks":{"contexts":["build","dco"]}}}'
  put "repos/$R/actions/artifacts" "{\"artifacts\":[{\"name\":\"ci-full-tree-$T\",\"expired\":false,\"created_at\":\"2030-01-02T00:00:00Z\",\"workflow_run\":{\"id\":500,\"head_repository_id\":$REPO_ID}}]}"
  put "repos/$R/actions/runs/500" "{\"id\":500,\"path\":\".github/workflows/ci.yml\",\"status\":\"completed\",\"conclusion\":\"success\",\"head_commit\":{\"tree_id\":\"$T\"}}"
  put "repos/$R/actions/runs/500/jobs" "$(jobs_json build dco other)"
  put "repos/$R/actions/workflows/nightly.yml/runs" "{\"workflow_runs\":[{\"id\":700,\"event\":\"workflow_dispatch\",\"status\":\"completed\",\"created_at\":\"2030-01-02T01:00:00Z\",\"head_sha\":\"$OTHER\",\"head_repository\":{\"full_name\":\"$R\"},\"head_commit\":{\"tree_id\":\"$T\"}}]}"
  put "repos/$R/actions/runs/700/jobs" "$(jobs_json w1 w2 'multi-arch build (wardynd)' 'multi-arch build (agent-base)')"
}

run() {  # run [NEED_STAGING]; sets OUT (stdout), ERR (stderr), RC
  local need="${1:-0}"
  OUT="$(PATH="$WORK/bin:$PATH" GITHUB_REPOSITORY=$R GH_TOKEN=x WATCHED="$WATCHED" NEED_STAGING="$need" "$SCRIPT" "$SHA" 2>"$WORK/err")"; RC=$?
  ERR="$(cat "$WORK/err")"
}
want() {  # want <label> <rc> [stderr-substring]
  if [ "$RC" != "$2" ]; then bad "$1: exit $RC, want $2 (stderr: $ERR)"; return; fi
  if [ -n "${3:-}" ] && ! grep -qF -- "$3" <<<"$ERR"; then bad "$1: stderr does not name '$3' (stderr: $ERR)"; return; fi
  ok "$1"
}
mut() {  # mut <endpoint> <jq filter>: rewrite one fixture
  local f; f="$(fixture "$1")"
  jq -c "$2" "$f" >"$f.new" && mv "$f.new" "$f"
}

# ── 1. green ────────────────────────────────────────────────────────────────
baseline 1; run
want "1 green exits 0" 0
want_out="tree=$T
ci_run=500
nightly_run=700
nightly_sha=$OTHER"
[ "$OUT" = "$want_out" ] && ok "1 stdout is exactly the four id lines" || bad "1 stdout was: $OUT"
[ -z "$ERR" ] && ok "1 nothing on stderr when green" || bad "1 stderr was: $ERR"
grep -qF -- "-f name=ci-full-tree-$T" "$FIX/calls.log" && ok "1 artifacts are asked for by marker name" || bad "1 no name= filter in: $(cat "$FIX/calls.log")"

# ── 2. a marker from another repo id ────────────────────────────────────────
baseline 2; mut "repos/$R/actions/artifacts" '.artifacts[0].workflow_run.head_repository_id = 999'
run; want "2 marker from another repo id" 1

# ── 3. a failed run, and a cancelled one that still carries the marker ──────
baseline 3; mut "repos/$R/actions/runs/500" '.conclusion = "failure"'
run; want "3 conclusion failure" 1
baseline 3b; mut "repos/$R/actions/runs/500" '.conclusion = "cancelled"'
run; want "3 conclusion cancelled (the marker uploads before the tests run)" 1

# ── 4. a marker for T on a run whose head tree is not T ─────────────────────
baseline 4; mut "repos/$R/actions/runs/500" ".head_commit.tree_id = \"$OTHER\""
run; want "4 head tree is not T" 1

# ── 5. a required context missing from the jobs ─────────────────────────────
baseline 5; put "repos/$R/actions/runs/500/jobs" "$(jobs_json build other)"
run; want "5 required context missing" 1 "dco"
baseline 5b; mut "repos/$R/actions/runs/500/jobs" '.jobs[0].conclusion = "skipped"'
run; want "5 required context skipped" 1 "build"

# ── 6. a watched nightly job missing ────────────────────────────────────────
baseline 6; put "repos/$R/actions/runs/700/jobs" "$(jobs_json w1 'multi-arch build (wardynd)')"
run; want "6 watched nightly job missing" 1 "w2"

# ── 7. no multi-arch build rows, and one red row ────────────────────────────
baseline 7; put "repos/$R/actions/runs/700/jobs" "$(jobs_json w1 w2)"
run; want "7 no multi-arch build rows" 1 "multi-arch build"
baseline 7b; mut "repos/$R/actions/runs/700/jobs" '.jobs[2].conclusion = "failure"'
run; want "7 a red multi-arch build row" 1 "multi-arch build (wardynd)"

# ── 8. only a scheduled nightly is green ────────────────────────────────────
# Only a workflow_dispatch nightly qualifies a tree: a scheduled run skips the
# staging matrix, which GitHub may report as one row literally named
# `multi-arch build (${{ matrix.name }})`. So the script asks for dispatch runs
# with or without NEED_STAGING, and rejects a scheduled run the API lets through.
baseline 8; mut "repos/$R/actions/workflows/nightly.yml/runs" '.workflow_runs[0].event = "schedule"'
run 0; want "8 scheduled nightly only, NEED_STAGING=0" 1 "700"
run 1; want "8 scheduled nightly only, NEED_STAGING=1" 1 "700"
grep -qF -- "event=workflow_dispatch" "$FIX/calls.log" && ok "8 the nightly query asks for workflow_dispatch runs" || bad "8 no event=workflow_dispatch in: $(cat "$FIX/calls.log")"

# ── 9. gh exits non-zero ────────────────────────────────────────────────────
baseline 9
OUT="$(GH_SHIM_FAIL=1 PATH="$WORK/bin:$PATH" GITHUB_REPOSITORY=$R GH_TOKEN=x WATCHED="$WATCHED" "$SCRIPT" "$SHA" 2>"$WORK/err")"; RC=$?; ERR="$(cat "$WORK/err")"
want "9 gh exits non-zero" 2
baseline 9b; mv "$(fixture "repos/$R/actions/runs/500/jobs")" "$WORK/gone.json"
run; want "9 a failing jobs call mid-way" 2

# ── 10. no error-swallowing fallbacks ───────────────────────────────────────
if grep -nE '\|\| *(true|:)([[:space:]]|$)' "$SCRIPT"; then bad "10 script text contains '|| true' or '|| :'"; else ok "10 no '|| true' and no '|| :'"; fi

# ── more: candidates and inputs ─────────────────────────────────────────────
baseline 11; mut "repos/$R/actions/artifacts" '.artifacts[0].expired = true'
run; want "11 an expired marker" 1
baseline 12; mut "repos/$R/actions/runs/500" '.path = ".github/workflows/evil.yml"'
run; want "12 a run of another workflow file" 1
baseline 13
put "repos/$R/actions/artifacts" "{\"artifacts\":[{\"name\":\"ci-full-tree-$T\",\"expired\":false,\"created_at\":\"2030-01-03T00:00:00Z\",\"workflow_run\":{\"id\":501,\"head_repository_id\":$REPO_ID}},{\"name\":\"ci-full-tree-$T\",\"expired\":false,\"created_at\":\"2030-01-02T00:00:00Z\",\"workflow_run\":{\"id\":500,\"head_repository_id\":$REPO_ID}}]}"
put "repos/$R/actions/runs/501" "{\"id\":501,\"path\":\".github/workflows/ci.yml\",\"status\":\"completed\",\"conclusion\":\"cancelled\",\"head_commit\":{\"tree_id\":\"$T\"}}"
run; want "13 a newer bad run does not hide an older good one" 0
grep -qx "ci_run=500" <<<"$OUT" && ok "13 the older good run is the one reported" || bad "13 stdout was: $OUT"
baseline 14; mut "repos/$R/actions/workflows/nightly.yml/runs" ".workflow_runs[0].head_commit.tree_id = \"$OTHER\""
run; want "14 a nightly on another tree (an ancestor never counts)" 1
baseline 15; mut "repos/$R/actions/workflows/nightly.yml/runs" '.workflow_runs[0].head_repository.full_name = "fork/wardyn"'
run; want "15 a nightly from a fork" 1

baseline 16
OUT="$(PATH="$WORK/bin:$PATH" GITHUB_REPOSITORY=$R GH_TOKEN=x WATCHED="$WATCHED" "$SCRIPT" not-a-sha 2>/dev/null)"; RC=$?
want "16 a bad sha is a usage error" 2
OUT="$(PATH="$WORK/bin:$PATH" GITHUB_REPOSITORY=$R GH_TOKEN=x WATCHED="" "$SCRIPT" "$SHA" 2>/dev/null)"; RC=$?
want "16 an empty WATCHED is a usage error" 2
OUT="$(PATH="$WORK/bin:$PATH" GITHUB_REPOSITORY=$R GH_TOKEN=x WATCHED="$WATCHED" NEED_STAGING=2 "$SCRIPT" "$SHA" 2>/dev/null)"; RC=$?
want "16 NEED_STAGING=2 is a usage error" 2

if [ "$fail" = 0 ]; then echo "All green-by-tree cases behaved as expected."; else exit 1; fi
