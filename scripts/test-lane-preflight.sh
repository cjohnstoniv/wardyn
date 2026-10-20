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
# PREFLIGHT_SRC: where lane-preflight.sh and its lib/ come from. scripts/test-preflight-mutations.sh points it
# at mutated copies to prove each case below goes red when the behaviour it pins is broken.
SRC="${PREFLIGHT_SRC:-$ROOT/scripts}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

WORK="$TMP/repo"
BIN="$TMP/bin"
LOG="$TMP/calls.log"
mkdir -p "$WORK/scripts/lib" "$BIN" "$WORK/ui/src/app"
cp "$SRC/lane-preflight.sh" "$WORK/scripts/"
cp "$SRC/lib/common.sh" "$WORK/scripts/lib/"

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
# docker: record the call; `logs -f` prints the two "ready" lines the wait step looks for, `port` an address.
cat >"$BIN/docker" <<'SHIM'
#!/usr/bin/env bash
printf 'docker %s\n' "$*" >>"$CALLS"
case "${1:-}" in
  logs) printf 'ready to accept connections\nready to accept connections\n' ;;
  port) echo '127.0.0.1:55432' ;;
esac
SHIM
sed -i "s|\"\$CALLS\"|\"$LOG\"|" "$BIN/docker"
chmod +x "$BIN/make" "$BIN/go" "$BIN/pnpm" "$BIN/docker"

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
# --candidate fixtures: a whole-tree guard test, the two vitest helpers that read outside ui/ and one
# reader test, the api/store packages the Postgres cut keys on, and a recording selector stub.
seed internal/guard/guard_test.go $'package guard\nfunc TestGuard(t *testing.T) { _ = ".github/workflows" }'
seed ui/src/app/lib/copy-doc-parity.ts 'import fs from "node:fs";'
seed ui/src/app/lib/test-fixtures.ts 'import fs from "node:fs";'
seed ui/src/app/reader.test.ts 'import fs from "node:fs";'
seed internal/api/api.go "package api"
seed internal/api/api_test.go "package api"
seed internal/store/store.go "package store"
seed internal/store/store_test.go "package store"
seed internal/other/other.go "package other"
seed internal/other/other_test.go "package other"
seed ui/e2e/flow.spec.ts "// spec"
cat >"$WORK/scripts/run-ui-e2e.sh" <<SHIM
#!/usr/bin/env bash
printf 'e2e skip_build=%s %s\n' "\${WARDYN_E2E_SKIP_BUILD:-0}" "\$*" >>"$LOG"
exit 0
SHIM
chmod +x "$WORK/scripts/run-ui-e2e.sh"
mkdir -p "$WORK/scripts/lib"
cp "$SRC/lib/candidate-tests.py" "$WORK/scripts/lib/"
# The selector stub records its patterns and answers the way the real one does: a regexp, or the
# "no PG tests found" failure for the package PG_NONE names.
cat >"$WORK/scripts/pg-test-select.sh" <<SHIM
#!/usr/bin/env bash
printf 'select %s\n' "\$*" >>"$LOG"
[ "\${PG_NONE:-}" = "\$*" ] && { echo "pg-test-select: no PG tests found in: \$*" >&2; exit 1; }
[ -z "\${PG_BROKEN:-}" ] || { echo "selector exploded" >&2; exit 2; }
echo '^(TestPG)\$'
SHIM
chmod +x "$WORK/scripts/pg-test-select.sh"
commit_base

ARGS=() # lane-preflight.sh's own arguments (--candidate), set by the case that wants them
run_preflight() { # [env assignments...]
  : >"$LOG"
  RC=0
  (cd "$WORK" && env "$@" PATH="$BIN:$PATH" GO_UNBUILDABLE=internal/tagged \
    ./scripts/lane-preflight.sh "${ARGS[@]}") >"$TMP/out" 2>"$TMP/err" || RC=$?
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
# ── --candidate: the release-candidate steps, through stubs ─────────────────
# No gate on the box is assumed: GATE=none names a file that does not exist, which is the path a box
# without the semaphore takes (commands run bare). A recording gate stands in for the real one.
git -C "$WORK" checkout --quiet -- .
git -C "$WORK" clean --quiet -fd
cat >"$BIN/gate" <<SHIM
#!/usr/bin/env bash
printf 'gate %s\n' "\$*" >>"$LOG"
exec "\$@"
SHIM
chmod +x "$BIN/gate"
ARGS=(--candidate)
nopg() { calls | grep -cE '^(select|docker) ' || true; }

seed internal/store/store.go "package store // edited"
seed internal/other/other.go "package other // edited"
run_preflight BASE=origin/main GATE=none
assert "a candidate run on a green stub is green" test "${RC}" -eq 0
want="make lint
make staticcheck
make ui-typecheck
go test -count=1 -run ^(TestGuard)\$ ./internal/guard
go list -e -f {{if or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}} ./internal/other ./internal/store
go test -p 4 -count=1 example.test/internal/other example.test/internal/store
select example.test/internal/other example.test/internal/store ./internal/api/...
docker run -d --name N
docker logs -f N
docker port N 5432/tcp
go test -p 1 -count=1 -timeout 30m -run ^(TestPG)\$ example.test/internal/other example.test/internal/store ./internal/api/...
pnpm -C ui exec vitest run --maxWorkers=4 src/app/reader.test.ts
docker rm -fv N"
assert "the steps ran in order: make gates, guards, changed packages, selector, PG, vitest, cleanup" \
  test "$(calls | sed -E 's/candidate-pg-[0-9]+/N/; s/^(docker run -d --name N).*/\1/')" = "${want}"
assert "GATE=none runs every command bare" test "$(calls | grep -c '^gate ')" -eq 0
assert "a non-test store change adds the internal/api PG set" \
  test "$(calls | grep -c '^go test -p 1 -count=1 -timeout 30m -run ^(TestPG)$ example.test/internal/other example.test/internal/store ./internal/api/...$')" -eq 1
assert "the PG run is the selector's regexp, one process, packages in the selector's order" test "$(calls | grep -c '^go test -p 1 ')" -eq 1
assert "vitest ran the files that read outside ui/" \
  test "$(calls | grep -c '^pnpm -C ui exec vitest run --maxWorkers=4 src/app/reader.test.ts$')" -eq 1
assert "the postgres container was removed at the end" test "$(calls | grep -c '^docker rm -fv candidate-pg-')" -eq 1
assert "no spec changed, so no playwright" test "$(calls | grep -c '^e2e ')" -eq 0
grep -q 'candidate preflight step times' "$TMP/out" || die "the candidate run printed no step times"
echo "ok  a candidate run runs every step in order and prints its times"

# The internal/api cut (CS4-b): its ~510 s PG set needs a non-test api or store file.
git -C "$WORK" checkout --quiet -- . && git -C "$WORK" clean --quiet -fd
seed internal/api/api_test.go "package api // edited"
seed internal/other/other.go "package other // edited"
run_preflight BASE=origin/main GATE=none
assert "an api test-only change is green" test "${RC}" -eq 0
assert "an api test-only change leaves the api PG set out" \
  test "$(calls | grep -c '^select example.test/internal/other$')" -eq 1
assert "go test (no DSN) still covers the changed api package" \
  test "$(calls | grep -c '^go test -p 4 -count=1 example.test/internal/api example.test/internal/other$')" -eq 1

git -C "$WORK" checkout --quiet -- . && git -C "$WORK" clean --quiet -fd
seed internal/api/api_test.go "package api // edited"
run_preflight BASE=origin/main GATE=none
assert "an api test-only lane starts no postgres and runs no selector" test "$(nopg)" -eq 0
grep -q 'internal/api only runs on a non-test api or store change' "$TMP/out" || die "the skipped PG step did not say why"

git -C "$WORK" checkout --quiet -- . && git -C "$WORK" clean --quiet -fd
seed internal/api/api.go "package api // edited"
run_preflight BASE=origin/main GATE=none
assert "a non-test api change selects the api set, once, as a pattern" \
  test "$(calls | grep -c '^select ./internal/api/...$')" -eq 1
assert "and runs it" test "$(calls | grep -c '^go test -p 1 .* ./internal/api/...$')" -eq 1

git -C "$WORK" checkout --quiet -- . && git -C "$WORK" clean --quiet -fd
seed internal/store/store_test.go "package store // edited"
run_preflight BASE=origin/main GATE=none
assert "a store test-only change selects store, not api" \
  test "$(calls | grep -c '^select example.test/internal/store$')" -eq 1
echo "ok  internal/api's Postgres set runs only on a non-test api or store change"

# A package with no Postgres tests, and a broken selector, are told apart.
git -C "$WORK" checkout --quiet -- . && git -C "$WORK" clean --quiet -fd
seed internal/other/other.go "package other // edited"
run_preflight BASE=origin/main GATE=none PG_NONE=example.test/internal/other
assert "a selector with nothing to run skips PG and stays green" test "${RC}" -eq 0
assert "and starts no container" test "$(calls | grep -c '^docker ')" -eq 0
run_preflight BASE=origin/main GATE=none PG_BROKEN=1
[[ ${RC} -ne 0 ]] || die "a broken selector passed"
grep -q 'selector exploded' "$TMP/err" || die "the selector's own error was swallowed"
grep -q 'pg-test-select.sh failed' "$TMP/err" || die "the broken selector was not named"
assert "a broken selector starts no container" test "$(calls | grep -c '^docker ')" -eq 0
echo "ok  no-PG-tests skips the step; any other selector failure fails it"

# The first failing step ends the run and is named.
run_preflight BASE=origin/main GATE=none PREFLIGHT_GO_FAIL=1
[[ ${RC} -ne 0 ]] || die "a failing guard passed"
grep -q 'FAILED at step: 2 whole-tree guards ./internal/guard' "$TMP/err" || die "the failing guard step was not named"
assert "nothing ran after the failing guard" test "$(calls | grep -cE '^(select|docker|pnpm) ')" -eq 0
echo "ok  a failing whole-tree guard stops the candidate run and names the step"

# The gate: each go/make/pnpm command goes through it, and WARDYN_GATED=1 turns that off.
run_preflight BASE=origin/main GATE="$BIN/gate"
assert "a present gate wraps make" test "$(calls | grep -c '^gate make ')" -eq 3
assert "a present gate wraps go test and the selector" \
  test "$(calls | grep -cE '^gate (env .*go test|scripts/pg-test-select.sh)')" -ge 3
run_preflight BASE=origin/main GATE="$BIN/gate" WARDYN_GATED=1
assert "WARDYN_GATED=1 leaves the commands bare" test "$(calls | grep -c '^gate ')" -eq 0
echo "ok  the gate wraps every command, and GATE=none / WARDYN_GATED=1 do not"

# A gate others can write would run every command as you: it is refused, not used.
chmod g+w "$BIN/gate"
run_preflight BASE=origin/main GATE="$BIN/gate"
chmod g-w "$BIN/gate"
[[ ${RC} -ne 0 ]] || die "a group-writable gate was used"
grep -q "GATE=$BIN/gate must be an executable you own" "$TMP/err" || die "the refused gate was not named"
assert "a refused gate runs nothing" test "$(calls | grep -c '^gate ')" -eq 0
echo "ok  a gate others can write is refused"

# A changed Playwright spec is left to hosted ui-e2e: the candidate preflight runs none.
git -C "$WORK" checkout --quiet -- . && git -C "$WORK" clean --quiet -fd
seed ui/e2e/flow.spec.ts "// spec edited"
run_preflight BASE=origin/main GATE=none
[[ ${RC} -eq 0 ]] || die "a changed spec failed the candidate run"
assert "a changed spec runs no Playwright pass" test "$(calls | grep -c '^e2e ')" -eq 0
echo "ok  a changed Playwright spec is left to hosted ui-e2e"

# Without --candidate none of it runs.
ARGS=()
seed internal/store/store.go "package store // edited"
run_preflight BASE=origin/main
assert "plain preflight starts no PG and runs no selector" test "$(nopg)" -eq 0
echo "ok  plain preflight is unchanged"

# The guard list is the escape the candidate run closes: a whole-tree test that is not in it is the one
# that goes red only on the hosted run. Pin what the list takes, on a throwaway tree, and that the two
# guards that once slipped past by name are in the real list.
GT="$TMP/guards"
mkdir -p "$GT/pkg/walk" "$GT/pkg/docs" "$GT/pkg/plain"
git -C "$GT" init -q
printf 'package walk\nimport "path/filepath"\nfunc TestWalks(t *testing.T) {\n\t_ = filepath.WalkDir\n}\n' >"$GT/pkg/walk/walk_test.go"
printf 'package docs\nfunc TestReadsDocs(t *testing.T) {\n\t_ = "../../docs/ENV.md"\n}\n' >"$GT/pkg/docs/docs_test.go"
printf 'package plain\nfunc TestPlain(t *testing.T) {\n}\n' >"$GT/pkg/plain/plain_test.go"
got="$(cd "$GT" && python3 -I "$SRC/lib/candidate-tests.py" guards)"
assert "a test that walks the tree is a guard" test "$(grep -c $'^pkg/walk\tTestWalks$' <<<"${got}")" -eq 1
assert "a test that reads docs/ is a guard" test "$(grep -c $'^pkg/docs\tTestReadsDocs$' <<<"${got}")" -eq 1
assert "a plain unit test is not" test "$(grep -c plain <<<"${got}")" -eq 0
real="$(cd "$ROOT" && python3 -I "$SRC/lib/candidate-tests.py" guards)"
assert "the operator-census guard is in the real list" grep -q $'^internal/api\t.*\\bTestIsOperatorCensus\\b' <<<"${real}"
assert "the retired-words guard is in the real list" grep -q $'^internal/runnerpool\t.*\\bTestRetiredPoolWordsAbsentFromCommandsConsoleAndDocs\\b' <<<"${real}"
echo "ok  the guard list takes tree-walking and docs-reading tests and leaves plain ones"

# The cases above, checked against mutated copies of the preflight (skipped when this run IS one of those).
[ -n "${PREFLIGHT_SRC:-}" ] || "$ROOT/scripts/test-preflight-mutations.sh"
