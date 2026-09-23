#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Canonical Playwright UI e2e runner.
#
# Each spec file runs against a FRESHLY SEEDED backend (scripts/e2e-backend.sh up
# resets the schema and re-seeds deterministic fixtures), serially within its own
# lane (--workers=1). This reproduces exactly the isolation each spec was
# authored and verified under, so mutating specs (kill/approve/create/delete)
# never contaminate one another — the alternative (all specs against one shared
# backend in parallel) is non-deterministic by construction.
#
# #469: the DEFAULT invocation (no spec args, not LIVE mode) runs
# WARDYN_E2E_LANES (default 3, capped at 3) CONCURRENT lanes, each its own
# isolated wardynd + UI-sandbox listener + Postgres database (WARDYN_E2E_ADDR /
# WARDYN_E2E_UI_ADDR / WARDYN_E2E_PG_DBNAME, knobs e2e-backend.sh already
# honours). Each lane claims the next unclaimed spec from one shared list, so
# the lanes finish together. Within a lane the fresh-reseed-per-spec contract
# above is unchanged: a backend only ever serves one spec at a time. An
# explicit spec list (`run-ui-e2e.sh runs secrets`), WARDYN_E2E_LANES=1 and
# LIVE mode run one lane.
#
# Prereqs: the dockerized Postgres "wardyn-test-pg" on :55432 (override with
# WARDYN_E2E_PG_HOSTPORT + WARDYN_E2E_PG_CONTAINER on a shared box where that
# port/name is taken — see docs/ENV.md's Test/internal-only e2e table, F063)
# and a built ui/dist + .e2e-bin/wardynd (this script builds them once unless
# WARDYN_E2E_SKIP_BUILD=1 / WARDYN_E2E_NO_UI_BUILD=1). Also `jq`, which reads
# Playwright's JSON report for the zero-executed check below — the script aborts
# up front without it rather than judge a spec on a report it cannot parse.
#
# Usage:  scripts/run-ui-e2e.sh                 # all specs, in concurrent lanes
#         scripts/run-ui-e2e.sh runs secrets    # only runs.spec.ts + secrets.spec.ts (single lane)
#         WARDYN_E2E_LANES=1 scripts/run-ui-e2e.sh   # all specs, single lane (debugging)
#
# WARDYN_E2E_ALLOW_ALL_SKIPPED: space-separated spec basenames (no extension,
# e.g. "drives ssh") allowed to report zero executed tests without failing the
# gate — see the zero-executed check below. Empty by default: a spec every one
# of whose tests skipped is a red flag until named here on purpose.
set -uo pipefail

command -v jq >/dev/null 2>&1 || { echo "run-ui-e2e.sh: jq is required (used to read Playwright's JSON report)" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# One daemon everywhere (same rule as setup/up/e2e-backend): export the picked
# DOCKER_HOST here so the Playwright child processes (approvals.spec.ts shells
# out to `docker exec wardyn-test-pg`) hit the daemon e2e-backend.sh provisions
# on — not the default one.
. "${REPO_ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

PORT="${WARDYN_E2E_ADDR:-:8088}"; PORT="${PORT#*:}"
DB="${WARDYN_E2E_PG_DBNAME:-wardyn_e2e}"
# Overridable PG host:port (the default may be held by a foreign container on a
# shared box); the database name stays coupled to WARDYN_E2E_PG_DBNAME. The
# seed/reset path (e2e-backend.sh) still goes through `docker exec
# WARDYN_E2E_PG_CONTAINER`, blind to this port — pairing a non-default
# WARDYN_E2E_PG_HOSTPORT with a container that doesn't actually publish it
# would silently serve one database while seeding another, so e2e-backend.sh's
# cmd_up fails loudly on that mismatch before touching anything.
PG_HOSTPORT="${WARDYN_E2E_PG_HOSTPORT:-localhost:55432}"
export WARDYN_E2E_ADDR=":${PORT}"
export WARDYN_E2E_DSN="postgres://wardyn:wardyn@${PG_HOSTPORT}/${DB}?sslmode=disable"
export WARDYN_E2E_PG_DBNAME="${DB}"
export WARDYN_E2E_PG_CONTAINER="${WARDYN_E2E_PG_CONTAINER:-wardyn-test-pg}"
export WARDYN_E2E_BASE_URL="http://localhost:${PORT}"

log() { printf '\033[1;34m[e2e-ui]\033[0m %s\n' "$*"; }

# LIVE mode (WARDYN_E2E_LIVE_BASE_URL): run a spec from ui/e2e/live/ against an
# ALREADY-RUNNING external Wardyn — the kind SSO cluster
# (scripts/kind-sso-walk.sh) — instead of the hermetic `-runner none` backend
# this script otherwise boots and re-seeds per spec. Nothing about the default
# path changes: the whole of it is skipped below on one variable, and unset
# (every other caller) the script is byte-identical to before this block.
#
# The `live` Playwright project is matched by the spec PATH, not a --project
# flag: chromium testIgnores live/**, and no other project matches it, so
# `playwright test e2e/live/<x>.spec.ts` selects exactly one project.
LIVE_BASE_URL="${WARDYN_E2E_LIVE_BASE_URL:-}"
if [[ -n "${LIVE_BASE_URL}" ]]; then
  export WARDYN_E2E_BASE_URL="${LIVE_BASE_URL}"
  log "LIVE mode: specs run against ${LIVE_BASE_URL} (no hermetic backend, no re-seed)"
fi

# Build the backend + UI once; subsequent per-spec `up` calls (in every lane)
# reuse them (each `up` also creates its own ${DB} if it does not exist — see
# e2e-backend.sh cmd_up).
if [[ -z "${LIVE_BASE_URL}" ]]; then
  log "Building backend + UI bundle once"
  ./scripts/e2e-backend.sh build || { echo "build failed"; exit 1; }
  export WARDYN_E2E_SKIP_BUILD=1
fi

# Spec selection: args map to e2e/<arg>.spec.ts; default = all *.spec.ts.
spec_dir="ui/e2e"
[[ -n "${LIVE_BASE_URL}" ]] && spec_dir="ui/e2e/live"
specs=()
if [[ $# -gt 0 ]]; then
  for a in "$@"; do specs+=("${spec_dir}/${a}.spec.ts"); done
else
  for f in "${spec_dir}"/*.spec.ts; do specs+=("$f"); done
fi

allow_all_skipped=" ${WARDYN_E2E_ALLOW_ALL_SKIPPED:-} "

# #469: how many lanes. Only the DEFAULT all-spec invocation fans out; an
# explicit spec list (a developer running two named specs by hand) and LIVE
# mode (one already-running external Wardyn) run a single lane.
NUM_LANES="${WARDYN_E2E_LANES:-3}"
[[ $# -gt 0 || -n "${LIVE_BASE_URL}" ]] && NUM_LANES=1
[[ ${NUM_LANES} -lt 1 ]] && NUM_LANES=1
[[ ${NUM_LANES} -gt 3 ]] && NUM_LANES=3

# Scratch for this run: one directory per spec, created by the lane that claims
# it (mkdir is atomic, so two lanes never run the same spec) and holding that
# spec's result line, plus each lane's backend-up log.
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# Exports lane $1's backend. Lane 0 is the backend this script has always used
# (WARDYN_E2E_ADDR, default :8088; e2e-backend.sh's UI-sandbox default :8089;
# ${DB}). THE one place lane ports are reserved: lane i>0 listens on
# PORT+1000*i and PORT+1000*i+1 (:9088/:9089, :10088/:10089 by default), with
# database ${DB}_lane<i>. Keep them clear of every other fixed port
# (screenshots.sh's :8098/:8099, the demo CI stack's :8099): e2e-backend.sh's
# `up` fuser -k's both ports before it binds them.
use_lane() {
  [[ $1 -eq 0 ]] && return 0
  local port=$((PORT + 1000 * $1)) db="${DB}_lane$1"
  export WARDYN_E2E_ADDR=":${port}" WARDYN_E2E_UI_ADDR=":$((port + 1))" WARDYN_E2E_PG_DBNAME="${db}"
  # wardynd connects through the DSN, not PG_DBNAME, so the DSN moves too.
  export WARDYN_E2E_DSN="postgres://wardyn:wardyn@${PG_HOSTPORT}/${db}?sslmode=disable"
  export WARDYN_E2E_BASE_URL="http://localhost:${port}"
}

# Lane $1: claims the next unclaimed spec, runs it against its own freshly
# seeded backend, records the result in ${work}/<spec>/result, and repeats
# until every spec is claimed. Claiming from one shared list balances the
# lanes by actual duration, with no per-spec timing table to keep in sync.
run_lane() {
  local lane="$1" label=""
  [[ ${NUM_LANES} -gt 1 ]] && label="lane${lane}"
  use_lane "${lane}"
  # Runs in its own subshell (backgrounded), so this redefinition only tags
  # this lane's lines in the interleaved output.
  if [[ -n "${label}" ]]; then
    log() { printf '\033[1;34m[e2e-ui:%s]\033[0m %s\n' "${label}" "$*"; }
  fi
  local up_log="${work}/up-${lane}.log"

  # Per-spec Playwright JSON report (schema: .stats.{expected,unexpected,flaky,
  # skipped}), overwritten by each spec's own run and read immediately after —
  # NEVER aggregated across specs by Playwright itself, only by this loop. This
  # is what closes F061: `--reporter=line` alone judges a spec file solely by
  # Playwright's exit code, which is 0 when every test in the file is
  # `test.skip()`-ed, so a guard that skips its whole file used to be counted as
  # "passed" here with nothing distinguishing it from a real pass.
  local results_json="${REPO_ROOT}/test/reports/e2e/results${label:+-${label}}.json"

  local spec base spec_rel spec_ok spec_name verdict
  local stats_expected stats_unexpected stats_flaky stats_skipped executed total
  for spec in "${specs[@]}"; do
    base="$(basename "${spec}")"
    mkdir "${work}/${base}" 2>/dev/null || continue
    spec_name="${base%.spec.ts}"
    # Playwright is run from ui/, so its argument is the spec path with the "ui/"
    # prefix dropped — "e2e/foo.spec.ts", or "e2e/live/foo.spec.ts" in LIVE mode.
    spec_rel="${spec#ui/}"
    # Retry once, and SHOW the failure. This used to be a single attempt with all
    # output sent to /dev/null, so a transient port race looked identical to a
    # genuine spec failure — the run reported "<spec> failed" with nothing to read.
    if [[ -n "${LIVE_BASE_URL}" ]]; then
      : # the external Wardyn owns its own lifecycle; nothing to seed or reset
    elif ! ./scripts/e2e-backend.sh up >"${up_log}" 2>&1; then
      log "backend up failed for ${base} — retrying once"
      tail -20 "${up_log}" >&2 || true
      ./scripts/e2e-backend.sh down >/dev/null 2>&1 || true
      if ! ./scripts/e2e-backend.sh up >"${up_log}" 2>&1; then
        log "backend up failed for ${base} (twice) — this is the backend, not the spec"
        tail -30 "${up_log}" >&2 || true
        echo "fail 0 0" > "${work}/${base}/result"
        continue
      fi
    fi
    if [[ -n "${LIVE_BASE_URL}" ]]; then
      log "running ${base} against ${LIVE_BASE_URL}"
    else
      log "running ${base} against a fresh backend"
    fi
    rm -f "${results_json}"
    spec_ok=0
    # --output: each spec keeps its own artifacts dir (screenshots, videos,
    # traces) under ui/test-results/. Playwright empties its output dir when a
    # run starts, so one shared dir let every spec — and every concurrent
    # lane — delete the failure evidence of the spec before it.
    ( cd ui && PLAYWRIGHT_JSON_OUTPUT_NAME="${results_json}" pnpm exec playwright test "${spec_rel}" --workers=1 --reporter=list,json --output="test-results/${spec_name}" ) && spec_ok=1

    # Read the stats Playwright's own run just wrote, defaulting every field to 0
    # (`// 0`) so a missing/corrupt results.json cannot throw arithmetic garbage
    # into the check below. Note what that degradation does and does NOT buy: a
    # missing report leaves total=0, so the zero-executed check never fires and
    # this spec is judged solely by Playwright's exit code (spec_ok) — which is
    # the correct fallback, because a report Playwright failed to write is not
    # evidence that nothing ran. The check below is aimed at the opposite case: a
    # report that says, in Playwright's own numbers, that every test was skipped.
    # `set -e` is not in effect here (`set -uo pipefail` only), so a jq parse
    # failure returns "null" cleanly through `// 0` instead of aborting.
    stats_expected=$(jq -r '.stats.expected // 0' "${results_json}" 2>/dev/null || echo 0)
    stats_unexpected=$(jq -r '.stats.unexpected // 0' "${results_json}" 2>/dev/null || echo 0)
    stats_flaky=$(jq -r '.stats.flaky // 0' "${results_json}" 2>/dev/null || echo 0)
    stats_skipped=$(jq -r '.stats.skipped // 0' "${results_json}" 2>/dev/null || echo 0)
    executed=$((stats_expected + stats_unexpected + stats_flaky))
    total=$((executed + stats_skipped))
    # stats_flaky counts tests that only passed after Playwright retried them —
    # folded into `executed` above (so a flaky run still counts as spec_ok=1 and
    # never fails the gate), and until now never surfaced anywhere else. A flaky
    # test IS a defect (X2-F10): print it per spec and fail the whole gate on any
    # nonzero total, the same way a genuine failure does. Nonzero only where
    # retries are enabled — playwright.config.ts's `retries: process.env.CI ? 2
    # : 0` means stats_flaky is structurally 0 on an uncustomized dev box; this
    # check has teeth in CI (and anywhere else CI=1 is set).
    if [[ ${stats_flaky} -gt 0 ]]; then
      log "${base}: ${stats_flaky} flaky test(s) (passed only after a Playwright retry)"
    fi

    verdict=fail
    [[ ${spec_ok} -eq 1 ]] && verdict=pass
    if [[ ${total} -gt 0 && ${executed} -eq 0 ]]; then
      # Every test in the file skipped. Playwright itself exits 0 for this — the
      # guard that hid a dozen tests before 814c20f6 (ui/e2e/drives.spec.ts:60-65
      # documents that incident) looked exactly like this. Fail closed unless the
      # spec is named in WARDYN_E2E_ALLOW_ALL_SKIPPED on purpose.
      if [[ "${allow_all_skipped}" == *" ${spec_name} "* ]]; then
        log "${base}: all ${stats_skipped} test(s) skipped — allowlisted (WARDYN_E2E_ALLOW_ALL_SKIPPED)"
      else
        log "${base}: ALL ${stats_skipped} test(s) skipped, 0 executed — treating as failed (not allowlisted)"
        verdict=zero
      fi
    fi
    echo "${verdict} ${stats_skipped} ${stats_flaky}" > "${work}/${base}/result"
  done

  [[ -n "${LIVE_BASE_URL}" ]] || ./scripts/e2e-backend.sh down >/dev/null 2>&1 || true
}

# Ctrl-C or a cancelled CI job: stop every lane rather than leave them running
# their remaining specs (a backgrounded lane ignores SIGINT), then free each
# lane's ports.
pids=()
stop_lanes() {
  trap - INT TERM
  kill "${pids[@]}" 2>/dev/null
  wait
  if [[ -z "${LIVE_BASE_URL}" ]]; then
    for ((i = 0; i < NUM_LANES; i++)); do
      ( use_lane "${i}"; ./scripts/e2e-backend.sh down >/dev/null 2>&1 )
    done
  fi
  exit 130
}
trap stop_lanes INT TERM

[[ ${NUM_LANES} -gt 1 ]] && log "Running ${#specs[@]} specs across ${NUM_LANES} concurrent lanes"
for ((i = 0; i < NUM_LANES; i++)); do
  run_lane "${i}" &
  pids+=($!)
done
wait
trap - INT TERM

pass=0; fail=0; failed_specs=(); skipped_total=0; zero_executed_specs=(); flaky_total=0
for spec in "${specs[@]}"; do
  base="$(basename "${spec}")"
  verdict=none; stats_skipped=0; stats_flaky=0
  [[ -s "${work}/${base}/result" ]] && read -r verdict stats_skipped stats_flaky < "${work}/${base}/result"
  skipped_total=$((skipped_total + stats_skipped))
  flaky_total=$((flaky_total + stats_flaky))
  case "${verdict}" in
    pass) pass=$((pass+1)) ;;
    zero) fail=$((fail+1)); failed_specs+=("${base}"); zero_executed_specs+=("${base}") ;;
    none) log "${base}: no result recorded (its lane died first)"; fail=$((fail+1)); failed_specs+=("${base}") ;;
    *) fail=$((fail+1)); failed_specs+=("${base}") ;;
  esac
done

echo
log "UI e2e summary: ${pass} spec file(s) passed, ${fail} failed, ${skipped_total} test(s) skipped, ${flaky_total} test(s) flaky"
if [[ ${#zero_executed_specs[@]} -gt 0 ]]; then
  log "zero executed (all skipped, not allowlisted): ${zero_executed_specs[*]}"
fi
if [[ ${fail} -gt 0 ]]; then
  log "failed: ${failed_specs[*]}"
  exit 1
fi
if [[ ${flaky_total} -gt 0 ]]; then
  log "flaky total is ${flaky_total}, not 0 — a flaky test is a defect, not a pass"
  exit 1
fi
