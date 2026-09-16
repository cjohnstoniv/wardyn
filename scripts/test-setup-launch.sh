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

echo "test-setup-launch: self-test PASS"
