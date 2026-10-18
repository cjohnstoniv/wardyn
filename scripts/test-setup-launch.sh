#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# B12b-F4 — no-docker, no-network regression coverage for scripts/setup.sh's
# finish_unhealthy_launch: the tail launch_wardynd's health wait falls into
# when /healthz never answers 200. Pins that a wardynd still alive past the
# wait budget (the shape a slow migration, or run-host.sh simply still
# starting, produces) keeps its pidfile — `make stop-host` and the
# already-running check at the top of setup.sh both key off it, so deleting it
# orphaned a live process — while one that has genuinely exited still has its
# pidfile cleaned up as before.
#
# SOURCES the function under test straight from scripts/lib/setup-launch.sh —
# a live test against the real source, not a copy. finish_unhealthy_launch
# lives in its own lib file (not inline in setup.sh) because setup.sh is on
# scripts/check-file-size.sh's frozen allowlist. A real short-lived background
# process stands in for run-host.sh: finish_unhealthy_launch only ever asks
# `kill -0 $WPID`, so nothing about it is specific to wardynd.
#
# Usage: scripts/test-setup-launch.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SETUP_SH="${REPO_ROOT}/scripts/setup.sh"
SETUP_LAUNCH_SH="${REPO_ROOT}/scripts/lib/setup-launch.sh"

# shellcheck source=lib/common.sh
. "${REPO_ROOT}/scripts/lib/common.sh"  # warn (finish_unhealthy_launch's own output)
[ -f "${SETUP_LAUNCH_SH}" ] || { echo "test-setup-launch: ${SETUP_LAUNCH_SH} not found (renamed/removed?)" >&2; exit 1; }
# shellcheck source=lib/setup-launch.sh
. "${SETUP_LAUNCH_SH}"

fail() { echo "test-setup-launch: FAIL: $*" >&2; exit 1; }

dir="$(mktemp -d)"; trap 'rm -rf "$dir"' EXIT

# 1) Still alive past the wait budget: the pidfile survives, still naming the
# live PID — deleting it here is exactly what orphaned a still-starting
# wardynd (B12b-F4).
pidfile="${dir}/pid-live"; logfile="${dir}/log-live"
: > "${logfile}"
sleep 60 & wpid=$!
echo "${wpid}" > "${pidfile}"
finish_unhealthy_launch "${pidfile}" "${wpid}" "${logfile}" >/dev/null
[ -f "${pidfile}" ] || fail "finish_unhealthy_launch deleted the pidfile of a PID that is still alive — the process is now orphaned (unmanageable by make stop-host or the next make setup)"
[ "$(cat "${pidfile}")" = "${wpid}" ] || fail "pidfile content changed for a still-alive PID"
kill "${wpid}" 2>/dev/null; wait "${wpid}" 2>/dev/null || true

# 2) Genuinely exited: the pidfile is still cleaned up — the pre-fix behavior,
# unchanged for the case it was always correct for.
pidfile="${dir}/pid-dead"; logfile="${dir}/log-dead"
: > "${logfile}"
( exit 0 ) & dead_wpid=$!
wait "${dead_wpid}" 2>/dev/null || true
echo "${dead_wpid}" > "${pidfile}"
finish_unhealthy_launch "${pidfile}" "${dead_wpid}" "${logfile}" >/dev/null
[ -f "${pidfile}" ] && fail "finish_unhealthy_launch left a pidfile behind for a PID that already exited"

# 3) The wait budget itself: B12b-F4 raised launch_wardynd's wait from a bare
# 45s to wardynd's own connect(30s)+migrate(default 5m) boot budget — guard
# the constant so it cannot quietly shrink back under the sum it must cover
# (cmd/wardynd/main.go connectAndMigrate; the same sum
# deploy/helm/wardyn/templates/deployment.yaml's startupProbe budgets for).
grep -q 'seq 1 330' "${SETUP_SH}" \
  || fail "launch_wardynd's wait loop is no longer 'seq 1 330' (330s = the fixed 30s connect + the default 300s WARDYN_MIGRATE_TIMEOUT) — a shorter wait hits finish_unhealthy_launch while wardynd is still working, not stuck"

# 4) R-10: the 330s wait (up from a bare 45s) needs a progress line, or the
# operator watches total silence for up to 5.5 minutes with no signal
# anything is happening.
grep -q 'still waiting on.*healthz' "${SETUP_SH}" \
  || fail "launch_wardynd's wait loop no longer prints a 'still waiting' progress line — a 330s wait with zero output looks hung, not busy (R-10)"

# 5) local_build_needed: a built tree is left alone; a missing CLI or a Go source newer than ./bin/wardyn
# (an old CLI lacks newer flags) asks for a rebuild; a test file or a docs change does not.
bt="${dir}/tree"; mkdir -p "${bt}/bin" "${bt}/ui/dist" "${bt}/cmd" "${bt}/internal"
: > "${bt}/ui/dist/index.html"; : > "${bt}/cmd/a.go"; : > "${bt}/go.mod"
printf '#!/bin/sh\n' > "${bt}/bin/wardynd"; cp "${bt}/bin/wardynd" "${bt}/bin/wardyn"; chmod +x "${bt}/bin/wardynd" "${bt}/bin/wardyn"
touch -d '1 hour ago' "${bt}/cmd/a.go" "${bt}/go.mod"
(cd "${bt}" && local_build_needed) && fail "local_build_needed asked for a rebuild of an up-to-date tree"
: > "${bt}/internal/x_test.go"
(cd "${bt}" && local_build_needed) && fail "local_build_needed rebuilt for a _test.go file"
: > "${bt}/internal/x.go"; touch -d 'now + 1 minute' "${bt}/internal/x.go"
(cd "${bt}" && local_build_needed) || fail "local_build_needed missed a Go source newer than ./bin/wardyn"
rm "${bt}/internal/x.go"; rm "${bt}/bin/wardyn"
(cd "${bt}" && local_build_needed) || fail "local_build_needed missed a missing ./bin/wardyn"

# 6) #2198: the stale-secret TRUNCATE needs consent and never runs against a key the shell supplied.
# stdin is not a tty here, so the interactive question answers "no" like a headless run.
ask_yn() { return 0; }
printf 'WARDYN_AGE_KEY=AGE-SECRET-KEY-ENVFILE\n' > "${dir}/env"
consent() { ( unset WARDYN_AGE_KEY WARDYN_AGE_KEY_FILE WARDYN_PLATFORM_KEY_FILE WARDYN_FORCE_RESET; "$@"; stale_secrets_wipe_consent "${dir}/env" ) >/dev/null 2>&1 < /dev/null; }
consent true && fail "stale_secrets_wipe_consent agreed to TRUNCATE with no consent (headless, no WARDYN_FORCE_RESET) (#2198)"
consent export WARDYN_FORCE_RESET=1 || fail "WARDYN_FORCE_RESET=1 no longer consents to the secrets TRUNCATE (#2198)"
consent export WARDYN_FORCE_RESET=1 WARDYN_AGE_KEY=AGE-SECRET-KEY-ENVFILE || fail "a shell key equal to the .env key was treated as a conflict (#2198)"
consent export WARDYN_FORCE_RESET=1 WARDYN_AGE_KEY=AGE-SECRET-KEY-OTHER && fail "a shell WARDYN_AGE_KEY that differs from the .env key was allowed to TRUNCATE secrets that the .env key still decrypts (#2198)"
consent export WARDYN_FORCE_RESET=1 WARDYN_AGE_KEY_FILE=/tmp/other.key && fail "a WARDYN_AGE_KEY_FILE in the shell was allowed to TRUNCATE the secrets (#2198)"
consent export WARDYN_FORCE_RESET=1 WARDYN_PLATFORM_KEY_FILE=/tmp/other.key && fail "a WARDYN_PLATFORM_KEY_FILE in the shell was allowed to TRUNCATE the secrets (#2198)"
grep -B3 "TRUNCATE secrets;'" "${SETUP_SH}" | grep -q 'stale_secrets_wipe_consent' \
  || fail "setup.sh runs TRUNCATE secrets without asking stale_secrets_wipe_consent first (#2198)"
unset -f ask_yn

echo "test-setup-launch: self-test PASS"
