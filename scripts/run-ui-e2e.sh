#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Canonical Playwright UI e2e runner.
#
# Each spec file runs against a FRESHLY SEEDED backend (scripts/e2e-backend.sh up
# resets the schema and re-seeds deterministic fixtures), serially within its own
# lane (--workers=1). This reproduces exactly the isolation each spec was
# authored and verified under, so mutating specs (kill/approve/create/delete)
# never contaminate one another — the alternative (all specs against one
# shared backend in parallel) is non-deterministic by construction.
#
# #469: the DEFAULT invocation (no spec args, not LIVE mode) runs
# WARDYN_E2E_LANES CONCURRENT lanes (clamped 1-3), each its own isolated
# wardynd (console + UI-sandbox + internal-TLS listeners) and Postgres
# database. Unset, the default itself is 1 locally and 3 in CI (`$CI` set) —
# three concurrent backends triple the memory of one run, which a shared dev
# box may not have headroom for, while a CI runner is dedicated and sized for
# it (`ci.yml` also pins WARDYN_E2E_LANES=3 explicitly). Every lane's ports
# are picked the SAME way the top-level single-lane default already picks
# ADDR/UI_ADDR — ask the OS for a free port — never a fixed arithmetic offset
# from another lane's port: an offset is not a reservation, and a lane
# bringing itself down must never resort to killing whatever it finds bound
# to a guessed port number (see e2e-backend.sh's cmd_down_quiet, which only
# ever stops the PID it itself started). Each lane claims the next unclaimed
# spec from one shared list, so the lanes finish together. Within a lane the
# fresh-reseed-per-spec contract above is unchanged: a backend only ever
# serves one spec at a time. An explicit spec list (`run-ui-e2e.sh runs
# secrets`), WARDYN_E2E_LANES=1 and LIVE mode run one lane.
#
# Prereqs: the dockerized Postgres "wardyn-test-pg" on :55432 (override with
# WARDYN_E2E_PG_HOSTPORT + WARDYN_E2E_PG_CONTAINER on a shared box where that
# port/name is taken — see docs/ENV.md's Test/internal-only e2e table, F063)
# and a built ui/dist-e2e + .e2e-bin/wardynd (this script builds them once unless
# WARDYN_E2E_SKIP_BUILD=1 / WARDYN_E2E_NO_UI_BUILD=1). Also `jq`, which reads
# Playwright's JSON report for the zero-executed check below — the script aborts
# up front without it rather than judge a spec on a report it cannot parse.
#
# Usage:  scripts/run-ui-e2e.sh                 # all specs, 1 lane locally / 3 in CI
#         scripts/run-ui-e2e.sh runs secrets    # only runs.spec.ts + secrets.spec.ts (single lane)
#         WARDYN_E2E_LANES=3 scripts/run-ui-e2e.sh   # all specs, 3 lanes (local override)
#
# WARDYN_E2E_ALLOW_ALL_SKIPPED: space-separated spec basenames (no extension,
# e.g. "drives ssh") allowed to report zero executed tests without failing the
# gate — see the zero-executed check below. Empty by default: a spec every one
# of whose tests skipped is a red flag until named here on purpose.
#
# Flaky tests (X2-F10, #1461 R3): a test that passed only after a Playwright
# retry is a defect and fails the gate — unless it is listed in
# ui/e2e/quarantine.txt (issue, owner, expiry <= 14 days; format in that file),
# in which case it passes with a warning (::warning under $CI). The quarantine
# file is validated before any backend starts: a malformed or expired entry
# exits 1 here. Every flake, quarantined or not, is written to
# test/reports/e2e/flaky.tsv (always written, empty when nothing flaked).
set -uo pipefail

command -v jq >/dev/null 2>&1 || { echo "run-ui-e2e.sh: jq is required (used to read Playwright's JSON report)" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# One daemon everywhere (same rule as setup/up/e2e-backend): export the picked
# DOCKER_HOST here so the Playwright child processes (approvals.spec.ts shells
# out to `docker exec wardyn-test-pg`) hit the daemon e2e-backend.sh provisions
# on — not the default one.
WARDYN_LOG_TAG="[e2e-ui]"
. "${REPO_ROOT}/scripts/lib/common.sh"
. "${REPO_ROOT}/scripts/lib/e2e-quarantine.sh"
wardyn_pick_docker_host

# An expired or malformed quarantine entry fails the run before anything boots.
quarantine_validate ui/e2e/quarantine.txt "$(date -u +%F)" >&2 || exit 1

# Two default invocations on one host — two worktrees, two lanes, a developer
# box and CI at once — used to fight over the same fixed :8088/:8089/:8443 and
# the same "wardyn_e2e" database name (#210, #469). Auto-pick when the caller
# has not pinned one; an explicit WARDYN_E2E_ADDR/WARDYN_E2E_UI_ADDR/
# WARDYN_E2E_INTERNAL_ADDR/WARDYN_E2E_PG_DBNAME is still honored verbatim,
# exactly as before.
if [[ -z "${WARDYN_E2E_ADDR:-}" ]]; then
  WARDYN_E2E_ADDR=":$(pick_free_port)"
fi
if [[ -z "${WARDYN_E2E_UI_ADDR:-}" ]]; then
  ui_port="$(pick_free_port)"
  # wardynd refuses to boot with UI_ADDR == ADDR (e2e-backend.sh's own
  # comment); pick_free_port's bind-then-close race makes that collision rare
  # but not impossible, so reroll once rather than fail the whole run over it.
  [[ ":${ui_port}" == "${WARDYN_E2E_ADDR}" ]] && ui_port="$(pick_free_port)"
  WARDYN_E2E_UI_ADDR=":${ui_port}"
fi
if [[ -z "${WARDYN_E2E_INTERNAL_ADDR:-}" ]]; then
  # wardynd's proxy-facing internal TLS listener (-internal-listen) binds
  # unconditionally whenever -control-plane-url is https, which is the
  # daemon's own default — regardless of ADDR/UI_ADDR. Fixed at :8443 with
  # nothing overriding it, a second instance on the same host (a second
  # worktree, a second lane below) dies with "address already in use" the
  # moment it tries to boot.
  internal_port="$(pick_free_port)"
  [[ ":${internal_port}" == "${WARDYN_E2E_ADDR}" || ":${internal_port}" == "${WARDYN_E2E_UI_ADDR}" ]] && internal_port="$(pick_free_port)"
  WARDYN_E2E_INTERNAL_ADDR=":${internal_port}"
fi
export WARDYN_E2E_ADDR WARDYN_E2E_UI_ADDR WARDYN_E2E_INTERNAL_ADDR
db_autonamed=""
if [[ -z "${WARDYN_E2E_PG_DBNAME:-}" ]]; then
  # $$ (this script's own PID), not the picked port: two lanes racing to
  # provision the SAME never-before-seen database name would otherwise both
  # pass cmd_up's "CREATE DATABASE ... || true" and share one schema reset.
  WARDYN_E2E_PG_DBNAME="wardyn_e2e_$$"
  db_autonamed=1
fi

PORT="${WARDYN_E2E_ADDR}"; PORT="${PORT#*:}"
DB="${WARDYN_E2E_PG_DBNAME}"
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

# Base-path mode (WARDYN_E2E_BASE_PATH, e.g. /wardyn): e2e-backend.sh serves the
# backend under that WARDYN_BASE_PATH behind test/basepathproxy, and Playwright
# browses the proxy under the prefix. The trailing slash is load-bearing: a
# spec's relative page.goto("runs") then resolves under the prefix. Specs
# written for the root use absolute paths and are not meant for this mode.
# Unset, nothing below changes.
if [[ -n "${WARDYN_E2E_BASE_PATH:-}" ]]; then
  if [[ -z "${WARDYN_E2E_PROXY_ADDR:-}" ]]; then
    WARDYN_E2E_PROXY_ADDR=":$(pick_free_port)"
  fi
  export WARDYN_E2E_PROXY_ADDR
  export WARDYN_E2E_BASE_URL="http://localhost:${WARDYN_E2E_PROXY_ADDR##*:}${WARDYN_E2E_BASE_PATH}/"
  log "base-path mode: backend under ${WARDYN_E2E_BASE_PATH}, browsed through ${WARDYN_E2E_BASE_URL}"
fi

# log() uses WARDYN_LOG_TAG="[e2e-ui]" set before sourcing common.sh above.

# LIVE mode (WARDYN_E2E_WALK_BASE_URL): run a spec from ui/e2e/walk/ against an
# ALREADY-RUNNING external Wardyn — the kind SSO cluster
# (scripts/kind-sso-walk.sh) — instead of the hermetic `-runner none` backend
# this script otherwise boots and re-seeds per spec. Nothing about the default
# path changes: the whole of it is skipped below on one variable, and unset
# (every other caller) the script is byte-identical to before this block.
#
# The `walk` Playwright project is matched by the spec PATH, not a --project
# flag: chromium testIgnores walk/**, and no other project matches it, so
# `playwright test e2e/walk/<x>.spec.ts` selects exactly one project.
LIVE_BASE_URL="${WARDYN_E2E_WALK_BASE_URL:-}"
if [[ -n "${LIVE_BASE_URL}" ]]; then
  export WARDYN_E2E_BASE_URL="${LIVE_BASE_URL}"
  log "LIVE mode: specs run against ${LIVE_BASE_URL} (no hermetic backend, no re-seed)"
fi

# Build the backend + UI once; subsequent per-spec `up` calls (in every lane)
# reuse them (each `up` also creates its own ${DB} if it does not exist — see
# e2e-backend.sh cmd_up).
# Specs named cockpit-terminal-tmux-* drive a REAL tmux through the production
# attach endpoint: their backend is the e2etmux build, served with
# WARDYN_E2E_TMUX=1 (see e2e-backend.sh). Every other spec keeps the none runner.
needs_tmux_build=0
if [[ $# -gt 0 ]]; then
  for a in "$@"; do [[ "${a}" == cockpit-terminal-tmux-* ]] && needs_tmux_build=1; done
elif compgen -G "ui/e2e/cockpit-terminal-tmux-*.spec.ts" >/dev/null; then
  needs_tmux_build=1
fi
[[ ${needs_tmux_build} -eq 1 ]] && export WARDYN_E2E_TMUX_BUILD=1

if [[ -z "${LIVE_BASE_URL}" ]]; then
  log "Building backend + UI bundle once"
  ./scripts/e2e-backend.sh build || { echo "build failed"; exit 1; }
  export WARDYN_E2E_SKIP_BUILD=1
fi

# Spec selection: args map to e2e/<arg>.spec.ts; default = all *.spec.ts.
spec_dir="ui/e2e"
[[ -n "${LIVE_BASE_URL}" ]] && spec_dir="ui/e2e/walk"
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
#
# The UNSET default itself depends on CI: 1 locally, 3 in CI. Three
# concurrent backends (three wardynd + three Postgres schemas) triple a
# single run's memory footprint, and a shared dev box has a hard cap on
# concurrent heavy jobs and has OOM-crashed before — a bare `run-ui-e2e.sh`
# on such a box must not silently fan out to 3 by default. CI runners are
# dedicated and sized for it, and `ci.yml` also pins WARDYN_E2E_LANES=3
# explicitly rather than lean on this default. An explicit WARDYN_E2E_LANES
# always wins over both.
default_lanes=1
[[ -n "${CI:-}" ]] && default_lanes=3
NUM_LANES="${WARDYN_E2E_LANES:-${default_lanes}}"
[[ ${NUM_LANES} =~ ^[0-9]+$ ]] || { echo "run-ui-e2e.sh: WARDYN_E2E_LANES must be a positive integer, got '${NUM_LANES}'" >&2; exit 1; }
[[ $# -gt 0 || -n "${LIVE_BASE_URL}" ]] && NUM_LANES=1
[[ ${NUM_LANES} -lt 1 ]] && NUM_LANES=1
[[ ${NUM_LANES} -gt 3 ]] && NUM_LANES=3

# Every lane's own ADDR/UI_ADDR/INTERNAL_ADDR/DB, resolved up front and
# SEQUENTIALLY in this one process, before any lane is backgrounded — never a
# PORT+1000*i arithmetic offset (that is a guess, not a reservation: it can
# land on a port screenshots.sh, a concurrent lane or an unrelated session on
# the box is already using). Lane 0 reuses what this script already resolved
# above (explicit or auto-picked); i>0 always asks the OS for a fresh one, the
# same pick-then-bind way lane 0's own auto-pick does above.
LANE_ADDR=("${WARDYN_E2E_ADDR}")
LANE_UI_ADDR=("${WARDYN_E2E_UI_ADDR}")
LANE_INTERNAL_ADDR=("${WARDYN_E2E_INTERNAL_ADDR}")
LANE_DB=("${DB}")
for ((i = 1; i < NUM_LANES; i++)); do
  LANE_ADDR+=(":$(pick_free_port)")
  LANE_UI_ADDR+=(":$(pick_free_port)")
  LANE_INTERNAL_ADDR+=(":$(pick_free_port)")
  LANE_DB+=("${DB}_lane${i}")
done

# Scratch for this run: one directory per spec, created by the lane that
# claims it (mkdir is atomic, so two lanes never run the same spec) and
# holding that spec's result line. Removed on exit by cleanup_all below.
work="$(mktemp -d)"

# Lane $1: claims the next unclaimed spec, runs it against its own freshly
# seeded backend, records the result in ${work}/<spec>/result, and repeats
# until every spec is claimed. Claiming from one shared list balances the
# lanes by actual duration, with no per-spec timing table to keep in sync.
# Runs in its own subshell (backgrounded by the caller below), so env
# exports and the log() redefinition here are invisible to sibling lanes.
run_lane() {
  local lane="$1" label=""
  [[ ${NUM_LANES} -gt 1 ]] && label="lane${lane}"
  export WARDYN_E2E_ADDR="${LANE_ADDR[lane]}" WARDYN_E2E_UI_ADDR="${LANE_UI_ADDR[lane]}"
  export WARDYN_E2E_INTERNAL_ADDR="${LANE_INTERNAL_ADDR[lane]}" WARDYN_E2E_PG_DBNAME="${LANE_DB[lane]}"
  export WARDYN_E2E_DSN="postgres://wardyn:wardyn@${PG_HOSTPORT}/${LANE_DB[lane]}?sslmode=disable"
  # #1207: LIVE mode already set WARDYN_E2E_BASE_URL to the external Wardyn
  # above (LIVE_BASE_URL); LANE_ADDR[0] there is only the unused, auto-picked
  # port reserved for a hermetic backend that never boots, so overwriting it
  # here sent every walk spec to a random local port nothing listens on.
  [[ -n "${LIVE_BASE_URL}" ]] || export WARDYN_E2E_BASE_URL="http://localhost:${LANE_ADDR[lane]#*:}"
  if [[ -n "${label}" ]]; then
    log() { printf '\033[1;34m[e2e-ui:%s]\033[0m %s\n' "${label}" "$*"; }
  fi
  # Per-lane, not per-$$ ("${work}/up-${lane}.log" is unique per lane; "$$"
  # is NOT — bash keeps $$ pinned to the top-level shell's PID inside a
  # background subshell, so every concurrent lane sees the SAME $$ and would
  # otherwise clobber one shared /tmp file).
  local up_log="${work}/up-${lane}.log"
  # Per-lane Playwright JSON report (schema: .stats.{expected,unexpected,
  # flaky,skipped}), overwritten by each spec's own run and read immediately
  # after — NEVER aggregated across specs by Playwright itself, only by this
  # loop. This is what closes F061: `--reporter=line` alone judges a spec
  # file solely by Playwright's exit code, which is 0 when every test in the
  # file is `test.skip()`-ed, so a guard that skips its whole file used to be
  # counted as "passed" here with nothing distinguishing it from a real pass.
  local results_json="${REPO_ROOT}/test/reports/e2e/results${label:+-${label}}.json"

  local spec base spec_rel spec_ok spec_name verdict
  local stats_expected stats_unexpected stats_flaky stats_skipped executed total
  local flaky_new flaky_quarantined flaky_tsv
  for spec in "${specs[@]}"; do
    base="$(basename "${spec}")"
    mkdir "${work}/${base}" 2>/dev/null || continue
    spec_name="${base%.spec.ts}"
    if [[ "${spec_name}" == cockpit-terminal-tmux-* ]]; then export WARDYN_E2E_TMUX=1; else unset WARDYN_E2E_TMUX; fi
    # Playwright is run from ui/, so its argument is the spec path with the "ui/"
    # prefix dropped — "e2e/foo.spec.ts", or "e2e/walk/foo.spec.ts" in LIVE mode.
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
        echo "fail 0 0 0" > "${work}/${base}/result"
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
    # A per-spec output dir: every Playwright run empties its output dir first,
    # so a shared dir would let one spec — or one concurrent lane — delete
    # another's failure evidence (screenshot, video, error-context) before
    # anything saw it.
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
    # never fails the gate on its own). A flaky test IS a defect (X2-F10): each
    # one is classified against ui/e2e/quarantine.txt, and every NEW (unlisted)
    # one fails the whole gate below, the same way a genuine failure does. Nonzero
    # only where retries are enabled — playwright.config.ts's `retries:
    # process.env.CI ? 2 : 0` means it is structurally 0 on an uncustomized dev
    # box; this check has teeth in CI (and anywhere else CI=1 is set).
    flaky_tsv="${work}/${base}/flaky.tsv"
    quarantine_classify "${results_json}" "${base}" ui/e2e/quarantine.txt > "${flaky_tsv}"
    flaky_new=$(awk -F'\t' '$3 == "new"' "${flaky_tsv}" | wc -l)
    flaky_quarantined=$(awk -F'\t' '$3 == "quarantined"' "${flaky_tsv}" | wc -l)
    # Fail closed: Playwright counted more flaky tests than the report walk
    # classified (an unexpected report shape) — the unaccounted ones are new.
    if [[ ${stats_flaky} -gt $((flaky_new + flaky_quarantined)) ]]; then
      flaky_new=$((stats_flaky - flaky_quarantined))
    fi
    stats_flaky=${flaky_new}
    if [[ ${stats_flaky} -gt 0 || ${flaky_quarantined} -gt 0 ]]; then
      log "${base}: ${stats_flaky} flaky test(s) (passed only after a Playwright retry), ${flaky_quarantined} quarantined"
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
    echo "${verdict} ${stats_skipped} ${stats_flaky} ${flaky_quarantined}" > "${work}/${base}/result"
  done

  [[ -n "${LIVE_BASE_URL}" ]] || ./scripts/e2e-backend.sh down >/dev/null 2>&1 || true
}

# kill_tree PID: TERM the whole descendant tree of PID, children first (so a
# parent doesn't get orphaned before its own children are found). Walks
# /proc-derived parent links via `pgrep -P`, never `pkill -f` (a name/cmdline
# match can hit an unrelated process sharing the pattern). Used below because
# `kill "$pid"` alone only reaches the lane subshell: the `pnpm exec
# playwright` child and its chromium survive it as orphans, keep running
# their remaining tests against a backend this script is about to tear down,
# and keep writing into ui/test-results/<spec>/ and test/reports/e2e/ after
# the script has exited. (A process-group kill via `set -m` + `kill -- -pid`
# was tried instead and rejected: without a controlling terminal — exactly
# the case here under `timeout`/CI — job control can hand the new group
# unexpected signal/terminal ownership, which risked killing more than the
# lane.)
kill_tree() {
  local pid="$1" child
  for child in $(pgrep -P "${pid}" 2>/dev/null); do
    kill_tree "${child}"
  done
  kill -TERM "${pid}" 2>/dev/null
}

# Stops every lane's backend and drops every lane's auto-named database.
# Runs on EVERY exit — normal completion, a failed check above, or stop_lanes'
# `exit 130` below — so this is the ONE place that owns backend/db teardown;
# a lane also tears its OWN backend down as soon as it finishes claiming
# specs (the end of run_lane above), purely so an idle lane's backend does
# not sit around waiting on a slower sibling — cmd_down_quiet is idempotent,
# so calling it again here for an already-down lane is a safe no-op.
cleanup_all() {
  rm -rf "${work}"
  [[ -n "${LIVE_BASE_URL}" ]] && return 0
  local i
  for ((i = 0; i < NUM_LANES; i++)); do
    (
      export WARDYN_E2E_ADDR="${LANE_ADDR[i]}" WARDYN_E2E_UI_ADDR="${LANE_UI_ADDR[i]}"
      export WARDYN_E2E_INTERNAL_ADDR="${LANE_INTERNAL_ADDR[i]}" WARDYN_E2E_PG_DBNAME="${LANE_DB[i]}"
      ./scripts/e2e-backend.sh down >/dev/null 2>&1
    ) || true
    # A caller-named base database is the caller's to keep; nothing else
    # would ever drop it. Its derived per-lane siblings (${DB}_lane<i>) follow
    # the same rule, tied to the SAME db_autonamed flag as the base.
    [[ -z "${db_autonamed}" ]] && continue
    docker exec "${WARDYN_E2E_PG_CONTAINER}" psql -U wardyn -d wardyn \
      -c "DROP DATABASE IF EXISTS \"${LANE_DB[i]}\" WITH (FORCE)" >/dev/null 2>&1 || true
  done
}
trap cleanup_all EXIT

# Ctrl-C or a cancelled CI job: stop every lane's still-running spec rather
# than leave it running to completion (a backgrounded lane ignores SIGINT),
# then let cleanup_all (EXIT trap, above) tear down every lane's backend + db.
pids=()
stop_lanes() {
  trap - INT TERM
  local pid
  for pid in "${pids[@]}"; do
    kill_tree "${pid}"
  done
  wait
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

pass=0; fail=0; failed_specs=(); skipped_total=0; zero_executed_specs=(); flaky_total=0; quarantined_total=0
# Every flake of the run, quarantined or not — always written, empty when none.
flaky_report="${REPO_ROOT}/test/reports/e2e/flaky.tsv"
mkdir -p "$(dirname "${flaky_report}")"
: > "${flaky_report}"
for spec in "${specs[@]}"; do
  base="$(basename "${spec}")"
  verdict=none; stats_skipped=0; stats_flaky=0; stats_quarantined=0
  [[ -s "${work}/${base}/result" ]] && read -r verdict stats_skipped stats_flaky stats_quarantined < "${work}/${base}/result"
  skipped_total=$((skipped_total + stats_skipped))
  flaky_total=$((flaky_total + stats_flaky))
  quarantined_total=$((quarantined_total + stats_quarantined))
  if [[ -s "${work}/${base}/flaky.tsv" ]]; then
    cat "${work}/${base}/flaky.tsv" >> "${flaky_report}"
    # Quarantined flakes pass with a warning, naming the entry that excuses them.
    while IFS=$'\t' read -r q_spec q_title q_verdict; do
      [[ "${q_verdict}" == quarantined ]] || continue
      q_meta="$(Q_SPEC="${q_spec}" Q_TITLE="${q_title}" awk -F' [|] ' '
        function trim(v) { gsub(/^[ \t]+|[ \t]+$/, "", v); return v }
        NF == 5 && (trim($1) "") == ENVIRON["Q_SPEC"] "" && (trim($2) "") == ENVIRON["Q_TITLE"] "" {
          print trim($3) ", " trim($4) ", until " trim($5); exit }' ui/e2e/quarantine.txt)"
      if [[ -n "${CI:-}" ]]; then
        echo "::warning title=Quarantined flaky test::${q_spec} › ${q_title} (${q_meta})"
      else
        log "quarantined flaky test: ${q_spec} › ${q_title} (${q_meta})"
      fi
    done < "${work}/${base}/flaky.tsv"
  fi
  case "${verdict}" in
    pass) pass=$((pass+1)) ;;
    zero) fail=$((fail+1)); failed_specs+=("${base}"); zero_executed_specs+=("${base}") ;;
    none) log "${base}: no result recorded (its lane died first)"; fail=$((fail+1)); failed_specs+=("${base}") ;;
    *) fail=$((fail+1)); failed_specs+=("${base}") ;;
  esac
done

echo
log "UI e2e summary: ${pass} spec file(s) passed, ${fail} failed, ${skipped_total} test(s) skipped, ${flaky_total} test(s) flaky (${quarantined_total} quarantined)"
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
