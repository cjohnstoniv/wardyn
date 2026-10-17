#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-lane-preflight.sh — the cheap merge gates, exercised through stubs.
#
# lane-preflight.sh's contract is WHICH commands it runs and with what
# arguments: it stops at the first failing step (so a later gate never buries
# the one that failed), scopes `go test` and `vitest related` to what the lane
# actually changed against BASE plus the working tree, and drops a Go package
# whose files all sit behind a build tag. None of that is visible in a shell
# transcript or from outside, and each change that widens it is silent: a lane
# whose preflight passed over a red gate, or one that died on a package that
# cannot build under default tags.
#
# Daemon-free, network-free and heavy-command-free: `make`, `go` and `pnpm` are
# recording stubs on a private PATH, and the repository under test is a
# throwaway one, so no real lint, typecheck or test ever runs.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

WORK="$TMP/repo"
BIN="$TMP/bin"
LOG="$TMP/calls.log"
mkdir -p "$WORK/scripts/lib" "$BIN" "$WORK/ui/src/app"
cp "$ROOT/scripts/lane-preflight.sh" "$WORK/scripts/"
cp "$ROOT/scripts/lib/common.sh" "$WORK/scripts/lib/"

# Each stub records "<name> <args...>" and fails when the command it was asked
# for is the one PREFLIGHT_<NAME>_FAIL names (never when unset), so a case can
# make exactly one gate fail and watch what runs after it.
cat >"$BIN/make" <<SHIM
#!/usr/bin/env bash
printf 'make %s\n' "\$*" >>"$LOG"
[ "\${1:-}" = "\${PREFLIGHT_MAKE_FAIL:-}" ] && exit 1
exit 0
SHIM
cat >"$BIN/pnpm" <<SHIM
#!/usr/bin/env bash
printf 'pnpm %s\n' "\$*" >>"$LOG"
exit "\${PREFLIGHT_PNPM_FAIL:-0}"
SHIM
# `go list -e -f …` reports which changed packages have a buildable file under
# default tags; GO_UNBUILDABLE names the one that reports none, the way a
# build-tagged package does.
cat >"$BIN/go" <<'SHIM'
#!/usr/bin/env bash
printf 'go %s\n' "$*" >>"$CALLS"
if [ "${1:-}" = test ]; then exit "${PREFLIGHT_GO_FAIL:-0}"; fi
for pkg in "$@"; do
  case "$pkg" in ./*) dir="${pkg#./}" ;; *) continue ;; esac
  [ "$dir" = "${GO_UNBUILDABLE:-}" ] || printf 'example.test/%s\n' "$dir"
done
SHIM
sed -i "s|\"\$CALLS\"|\"$LOG\"|" "$BIN/go"
chmod +x "$BIN/make" "$BIN/go" "$BIN/pnpm"

seed() { mkdir -p "$(dirname "$WORK/$1")"; printf '%s\n' "${2:-seed}" >"$WORK/$1"; }
commit_base() {
  git -C "$WORK" add -A
  git -C "$WORK" -c user.name=test -c user.email=test@example.com commit --quiet -m base
  git -C "$WORK" update-ref refs/remotes/origin/main HEAD
}

git init --quiet -b main "$WORK"
seed internal/untouched/untouched.go "package untouched"
seed internal/changed/changed.go "package changed"
seed internal/tagged/tagged.go "package tagged"
seed ui/src/app/untouched.tsx "export const Untouched = 1;"
seed ui/src/app/changed.tsx "export const Changed = 1;"
seed ui/src/app/changed.css ".changed {}"
seed ui/e2e/notes.md "not a UI source"
seed scripts/changed.sh "echo changed"
commit_base

run_preflight() { # [env assignments...]
  : >"$LOG"
  RC=0
  (cd "$WORK" && env "$@" PATH="$BIN:$PATH" GO_UNBUILDABLE=internal/tagged \
    ./scripts/lane-preflight.sh) >"$TMP/out" 2>"$TMP/err" || RC=$?
}
calls() { cat "$LOG"; }
assert() { local label=$1; shift; "$@" || { calls; cat "$TMP/err" >&2; die "$label"; }; echo "ok  $label"; }

# ── the happy path: every gate once, in order, scoped to what changed ───────
seed internal/changed/changed.go "package changed // edited"
seed internal/tagged/tagged.go "package tagged // edited"
seed ui/src/app/changed.tsx "export const Changed = 2;"
rm -rf "$WORK/internal/untouched" # a deletion, which --diff-filter=d excludes
seed internal/brandnew/brandnew.go "package brandnew" # untracked: never committed
seed ui/src/app/brandnew.tsx "export const BrandNew = 1;"
run_preflight BASE=origin/main
assert "preflight passed on a green stub run" test "${RC}" -eq 0
want="make lint
make staticcheck
make ui-typecheck
go list -e -f {{if or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}} ./internal/brandnew ./internal/changed ./internal/tagged
go test -p 4 -count=1 example.test/internal/brandnew example.test/internal/changed
pnpm -C ui exec vitest related --run --maxWorkers=4 src/app/brandnew.tsx src/app/changed.tsx"
assert "every gate ran once, in order, scoped to the changed files" \
  test "$(calls)" = "${want}"
grep -q 'preflight passed (not a substitute for hosted CI or the coverage floor)' "$TMP/out" \
  || die "preflight passed silently"
echo "ok  the passing run says what it is not"

# ── it stops at the FIRST failing gate ─────────────────────────────────────
# The make stub fails on exactly the target PREFLIGHT_MAKE_FAIL names, so the
# recorded list pins where preflight stopped: past the gate that failed is a
# green run over a red gate, short of it is a gate that never ran.
order=(lint staticcheck ui-typecheck)
for i in 0 1 2; do
  gate="${order[$i]}"
  run_preflight BASE=origin/main PREFLIGHT_MAKE_FAIL="${gate}"
  [[ ${RC} -ne 0 ]] || die "preflight passed with make ${gate} failing"
  ran="$(calls | sed -n 's/^make //p' | tr '\n' ' ')"
  assert "make ${gate} failing stops preflight there" \
    test "${ran}" = "${order[*]:0:$((i + 1))} "
done
# A failing gate is not the only failure preflight must carry: the scoped steps
# come after the three make gates and stop each other the same way.
run_preflight BASE=origin/main PREFLIGHT_GO_FAIL=1
[[ ${RC} -ne 0 ]] || die "preflight passed with go test failing"
assert "a failing go test stops preflight before vitest" \
  test "$(calls | grep -c '^pnpm ')" -eq 0
run_preflight BASE=origin/main PREFLIGHT_PNPM_FAIL=1
[[ ${RC} -ne 0 ]] || die "preflight passed with vitest failing"
echo "ok  a failing gate stops preflight before the next one"

# ── a changed Go package behind a build tag never reaches `go test` ─────────
run_preflight BASE=origin/main
assert "go test ran only the buildable changed packages" \
  test "$(calls | grep -c '^go test -p 4 -count=1 example.test/internal/brandnew example.test/internal/changed$')" -eq 1
assert "the unbuildable package is gone from go test" \
  test "$(calls | grep '^go test' | grep -c 'tagged')" -eq 0

# Every changed package unbuildable: the step is skipped, never run with an
# empty package list (which would test the current directory instead).
rm -rf "$WORK/internal/changed" "$WORK/internal/brandnew"
run_preflight BASE=origin/main
assert "go test is skipped when nothing is buildable" test "$(calls | grep -c '^go test ')" -eq 0
grep -q 'no changed Go packages buildable under default tags' "$TMP/out" \
  || die "the skipped go test said nothing"
echo "ok  an all-unbuildable change skips go test and says so"

# ── a base with no merge base is refused before any gate runs ──────────────
run_preflight BASE=origin/nope
[[ ${RC} -ne 0 ]] || die "preflight passed with no merge base"
grep -q 'no merge base with origin/nope' "$TMP/err" || die "the missing merge base was not named"
assert "no gate ran without a merge base" test ! -s "$LOG"
echo "ok  a base with no merge base is refused before any work"

# ── a lane with no Go or UI change skips those steps ───────────────────────
git -C "$WORK" checkout --quiet -- .
rm -rf "$WORK/ui/src/app/brandnew.tsx"
seed scripts/added.sh "echo added" # one untracked file: neither Go nor UI source
run_preflight BASE=origin/main
assert "a scripts-only lane is green" test "${RC}" -eq 0
assert "go test and vitest were skipped" test "$(calls | grep -cE '^(go test|pnpm) ')" -eq 0
grep -q 'no changed Go packages buildable' "$TMP/out" || die "the skipped go test said nothing"
grep -q 'no changed UI sources, skipped' "$TMP/out" || die "the skipped vitest said nothing"
echo "ok  a lane with no Go or UI change skips those steps and says so"

log "lane preflight: PASS"