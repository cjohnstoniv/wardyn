#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# lane-preflight.sh — the cheap merge gates, run locally before a push, so a
# lint, static-analysis, typecheck or scoped-test failure is found in minutes
# rather than after a full hosted CI pass. It stops at the first failing step.
#
# Steps, in order: make lint, make staticcheck, make ui-typecheck, go test of
# the packages holding changed Go files, vitest related for changed UI sources.
# "Changed" is against the merge base with $BASE (default origin/main), plus the
# working tree. A lane stacked on a batch branch sets BASE=origin/batch/NNN.
#
# This is not the merge gate. Hosted CI and `make ci` stay authoritative, and
# the union coverage floor (scripts/cover-union.sh) is not checked here.
#
# --candidate widens it to what a release candidate needs, so the defects only hosted CI used to
# catch are found before the push. Against the same merge base, in this order, each step timed and
# the first failure ending the run (exit non-zero, the step named on stderr):
#   lint, staticcheck, ui-typecheck | whole-tree guards | go test of changed packages |
#   Postgres-gated tests of changed packages | vitest | changed Playwright specs x3.
# The guard and Postgres test lists are built by grep (scripts/lib/candidate-tests.py), never by hand.
# Every go/pnpm/e2e command goes through ${GATE:-/tmp/claude-1000/wardyn-gates/gate.sh} when that file
# exists, so do NOT wrap the whole script in the gate (its e2e step takes the heavy slot itself); if you
# must, set WARDYN_GATED=1 so the inner commands skip the gate. The script never sets it.
# Roughly 15 minutes on a narrow diff; a wide one (BASE=v0.9.0) is far more. Still not a substitute
# for hosted CI: no -race, no coverage floor, no tag legs, only the changed packages' Postgres tests.
#
# Usage: [BASE=<ref>] scripts/lane-preflight.sh [--candidate [BASE=<ref>]]
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=lib/common.sh
source scripts/lib/common.sh

candidate=0
for arg in "$@"; do
  case "${arg}" in
    --candidate) candidate=1 ;;
    BASE=*) BASE="${arg#BASE=}" ;;
    *) die "usage: scripts/lane-preflight.sh [--candidate [BASE=<ref>]]" ;;
  esac
done

# Candidate mode only: run each go/pnpm/e2e command through the box's gate semaphore when it exists.
G=()
if [ "${candidate}" = 1 ]; then
  gate="${GATE:-/tmp/claude-1000/wardyn-gates/gate.sh}"
  [ -x "${gate}" ] && [ "${WARDYN_GATED:-}" != 1 ] && G=("${gate}")
fi

# step NAME CMD... — plain mode just runs CMD; candidate mode times it, logs it and, on failure,
# names the step and exits with the command's status.
steps_log=()
step() {
  local name=$1 t0=${SECONDS} rc=0
  shift
  if [ "${candidate}" != 1 ]; then "$@"; return; fi
  log "[candidate] ${name}"
  "$@" || rc=$?
  steps_log+=("$(printf '%5ss  %s%s' $((SECONDS - t0)) "${name}" "$([ "${rc}" = 0 ] || echo '  <- FAILED')")")
  if [ "${rc}" != 0 ]; then
    warn "candidate preflight FAILED at step: ${name} (exit ${rc})" >&2
    exit "${rc}"
  fi
}
summary() { [ "${candidate}" = 1 ] && [ "${#steps_log[@]}" -gt 0 ] && printf 'candidate preflight step times:\n%s\n' "$(printf '  %s\n' "${steps_log[@]}")"; return 0; }
trap summary EXIT

BASE="${BASE:-origin/main}"
base=$(git merge-base "${BASE}" HEAD) || die "no merge base with ${BASE} (run: git fetch origin)"
changed=$( { git diff --name-only --diff-filter=d "${base}"; git ls-files --others --exclude-standard; } | sort -u )

go_pkgs=()
while IFS= read -r d; do
  go_pkgs+=("./${d}")
done < <(grep '\.go$' <<<"${changed}" | xargs -r -n1 dirname | sort -u || true)

ui_files=()
while IFS= read -r f; do
  ui_files+=("${f#ui/}")
done < <(grep -E '^ui/src/.*\.(ts|tsx)$' <<<"${changed}" || true)

log "1/5 make lint"
step "1 make lint (go vet, golangci, file size, fixture dates, migration numbers)" "${G[@]}" make lint
log "2/5 make staticcheck"
step "1 make staticcheck" "${G[@]}" make staticcheck
log "3/5 make ui-typecheck"
step "1 make ui-typecheck" "${G[@]}" make ui-typecheck

if [ "${candidate}" = 1 ]; then
  # Whole-tree guards: every Go test whose file reads the repository (docs, ui/, .github, go/ast, a
  # directory walk), in EVERY package, because a change anywhere can break a guard elsewhere.
  # Unraced and uncached, WARDYN_TEST_PG unset, as ci.yml's guard step.
  guard_out=$(python3 -I scripts/lib/candidate-tests.py guards) || die "2 whole-tree guards: scripts/lib/candidate-tests.py failed"
  [ -n "${guard_out}" ] || die "2 whole-tree guards: no guard test found; the discovery pattern regressed"
  mapfile -t guard_lines <<<"${guard_out}"
  for line in "${guard_lines[@]}"; do
    IFS=$'\t' read -r dir names <<<"${line}"
    step "2 whole-tree guards ./${dir}" "${G[@]}" env WARDYN_TEST_PG='' go test -count=1 -run "^(${names})\$" "./${dir}"
  done
fi

if [ "${#go_pkgs[@]}" -gt 0 ]; then
  # A package whose files all sit behind a build tag fails `go test` setup under default tags; drop those.
  mapfile -t go_pkgs < <(go list -e -f '{{if or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' "${go_pkgs[@]}")
fi

if [ "${#go_pkgs[@]}" -gt 0 ]; then
  log "4/5 go test ${go_pkgs[*]}"
  step "3 go test changed packages" "${G[@]}" go test -p 4 -count=1 "${go_pkgs[@]}"
else
  log "4/5 go test: no changed Go packages buildable under default tags (build-tagged ones are left to hosted CI), skipped"
fi

if [ "${candidate}" = 1 ]; then
  # Postgres-gated tests of every changed package, whether its test files or its sources changed: the
  # selection is scripts/pg-test-select.sh's (the tests that skip without WARDYN_TEST_PG, plus the
  # probes), not the package's whole suite. A throwaway tmpfs postgres:17, as ci-parity-local.sh, with
  # the pool guard on, superuser asserted and -p 1. Exit 1 with "no PG tests found" means none to run;
  # any other selector failure fails the step.
  pg_re=""
  if [ "${#go_pkgs[@]}" -gt 0 ]; then
    sel_err=$(mktemp)
    if pg_re=$("${G[@]}" scripts/pg-test-select.sh "${go_pkgs[@]}" 2>"${sel_err}"); then
      :
    elif grep -q 'no PG tests found' "${sel_err}"; then
      pg_re=""
    else
      cat "${sel_err}" >&2
      die "4 Postgres-gated tests: scripts/pg-test-select.sh failed"
    fi
    rm -f "${sel_err}"
  fi
  if [ -n "${pg_re}" ]; then
    name="candidate-pg-$$"
    trap 'docker rm -fv "${name}" >/dev/null 2>&1 || true; summary' EXIT
    step "4 start throwaway postgres:17" docker run -d --name "${name}" -e POSTGRES_PASSWORD=wardyn -e POSTGRES_DB=wardyn \
      --tmpfs /var/lib/postgresql/data -p 127.0.0.1::5432 postgres:17 -c fsync=off >/dev/null
    # The image restarts its server once after initdb: ready is the second "ready to accept connections".
    step "4 wait for postgres" timeout 90 awk '/ready to accept connections/ { if (++n == 2) exit }' < <(docker logs -f "${name}" 2>&1)
    port=$(docker port "${name}" 5432/tcp | head -1 | sed 's/.*://')
    step "4 Postgres-gated tests (${#go_pkgs[@]} packages)" "${G[@]}" env \
      WARDYN_TEST_PG="postgres://postgres:wardyn@localhost:${port}/wardyn?sslmode=disable" \
      WARDYN_TEST_PG_SUPERUSER=1 WARDYN_TEST_POOL_GUARD=1 \
      go test -p 1 -count=1 -timeout 30m -run "${pg_re}" "${go_pkgs[@]}"
  else
    log "[candidate] 4 Postgres-gated tests: no changed package has any, skipped"
  fi

  # vitest files that read outside ui/ (Go sources, docs, the repo root): ci.yml's discovery, verbatim.
  # They run when anything outside ui/ changed, or one of the two helpers they share.
  if grep -qvE '^ui/' <<<"${changed}" || grep -qE '^ui/src/app/lib/(copy-doc-parity|test-fixtures)\.ts$' <<<"${changed}"; then
    tops="$(git ls-tree --full-tree --name-only HEAD | grep -vx ui | sed 's/[^A-Za-z0-9_-]/\\&/g' | paste -sd'|')"
    reads=$'[\'"](node:)?(fs(/promises)?|child_process)[\'"]|process\\.cwd\\(\\)|import\\.meta\\.(url|glob)|__dirname|\\?raw[\'"]|[\'"`](\\.\\./)+('"${tops}"$')([/\'"`?]|$)'
    helpers="$(git -C ui grep -l -E "${reads}" -- 'src/*.ts' 'src/*.tsx' ':!*.test.ts' ':!*.test.tsx' || true)"
    # shellcheck disable=SC2086 # one word per file
    if [ "$(echo ${helpers})" != "src/app/lib/copy-doc-parity.ts src/app/lib/test-fixtures.ts" ]; then
      log "[candidate] a module outside the two known helpers reads outside ui/ (${helpers}); running the whole vitest suite"
      step "5 vitest (whole suite)" "${G[@]}" pnpm -C ui exec vitest run --maxWorkers=4
    else
      mapfile -t reader_tests < <(git -C ui grep -l -E "${reads}|copy-doc-parity|expectNoOwnCopy\(" -- 'src/*.test.ts' 'src/*.test.tsx')
      [ "${#reader_tests[@]}" -gt 0 ] || { warn "no vitest file reads outside ui/; the discovery pattern regressed" >&2; exit 1; }
      step "5 vitest files reading outside ui/ (${#reader_tests[@]} files)" "${G[@]}" pnpm -C ui exec vitest run --maxWorkers=4 "${reader_tests[@]}"
    fi
  else
    log "[candidate] 5 vitest files reading outside ui/: only ui/ changed, covered by vitest related, skipped"
  fi
fi

if [ "${#ui_files[@]}" -gt 0 ]; then
  log "5/5 vitest related (${#ui_files[@]} files)"
  step "5 vitest related (${#ui_files[@]} files)" "${G[@]}" pnpm -C ui exec vitest related --run --maxWorkers=4 "${ui_files[@]}"
else
  log "5/5 vitest related: no changed UI sources, skipped"
fi

if [ "${candidate}" = 1 ]; then
  # Changed Playwright specs, three times each: a race shows as a failure on one of three fresh seeds.
  # run-ui-e2e.sh takes no extra Playwright args, so the spec list is looped; the first pass builds
  # the backend and the UI bundle, the others reuse them. A pass that needed a retry fails the run.
  e2e_specs=()
  while IFS= read -r f; do
    [ -f "${f}" ] && e2e_specs+=("$(basename "${f}" .spec.ts)")
  done < <(grep -E '^ui/e2e/[^/]+\.spec\.ts$' <<<"${changed}" || true)
  if [ "${#e2e_specs[@]}" -gt 0 ]; then
    ss -ltn 2>/dev/null | grep -q ':8088 ' && log "[candidate] note: :8088 is held by another process; run-ui-e2e.sh picks free ports itself, so this does not collide"
    for pass in 1 2 3; do
      if [ "${pass}" = 1 ]; then
        step "6 playwright pass 1/3 (${e2e_specs[*]})" "${G[@]}" scripts/run-ui-e2e.sh "${e2e_specs[@]}"
      else
        step "6 playwright pass ${pass}/3 (${e2e_specs[*]})" "${G[@]}" env WARDYN_E2E_SKIP_BUILD=1 WARDYN_E2E_NO_UI_BUILD=1 scripts/run-ui-e2e.sh "${e2e_specs[@]}"
      fi
    done
  else
    log "[candidate] 6 playwright: no changed spec under ui/e2e, skipped"
  fi
fi

log "preflight passed (not a substitute for hosted CI or the coverage floor)"
