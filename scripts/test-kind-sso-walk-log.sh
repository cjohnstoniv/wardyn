#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-kind-sso-walk-log.sh — proves scripts/check-kind-sso-walk-log.sh
# actually distinguishes a genuine PASS from a self-skip (or a walk that died
# mid-run) for BOTH of nightly.yml's live kind SSO jobs — kind-sso-walk
# (default) and kind-sso-ado-walk (ado) (#695) — the executed-not-skipped
# assertion, red on a skipped log and green on an executed one, without a
# kind cluster.
#
# Daemon-free, network-free: every case is a synthetic log fixture.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="${ROOT}/scripts/check-kind-sso-walk-log.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
check() { "${CHECK}" "$@" >/dev/null 2>&1; }

# 1. the self-skip line alone (WARDYN_TEST_K8S unset) must FAIL, for both
#    profiles — this is the exact regression #695 exists to catch: a job that
#    is permanently green because it never reached the walk step.
skip_log="${TMP}/skip.log"
echo "kind-sso-walk: set WARDYN_TEST_K8S=1 to run the cluster-dependent AWS SSO walk (skipping)." > "${skip_log}"
if check default "${skip_log}"; then fail "a self-skipped log must FAIL the default check"; fi
if check ado "${skip_log}"; then fail "a self-skipped log must FAIL the ado check"; fi
echo "ok  self-skip fails both profiles"

# 2. a log with no spec-count line and no PASS line (e.g. a crash before the
#    walk ever ran a spec) must FAIL.
empty_log="${TMP}/empty.log"
: > "${empty_log}"
if check default "${empty_log}"; then fail "an empty log must FAIL the default check"; fi
if check ado "${empty_log}"; then fail "an empty log must FAIL the ado check"; fi
echo "ok  an empty/crashed log fails both profiles"

# 3. a log naming executed specs but missing the closing PASS line (the walk
#    ran specs then died before its own PASS echo) must FAIL.
partial_log="${TMP}/partial.log"
printf 'UI e2e summary: 3 spec file(s) passed\n' > "${partial_log}"
if check default "${partial_log}"; then fail "a log missing the PASS line must FAIL the default check"; fi
if check ado "${partial_log}"; then fail "a log missing the PASS line must FAIL the ado check"; fi
echo "ok  specs-executed-but-no-PASS-line fails both profiles"

# 4. a genuine DEFAULT-profile PASS: specs executed and the default script's
#    own closing line. Must PASS the default check, and must FAIL the ado
#    check — a default log fed to the ado leg's assertion (wrong closing
#    line) must not silently pass.
default_pass_log="${TMP}/default-pass.log"
{
  printf 'UI e2e summary: 3 spec file(s) passed\n'
  printf 'kind-sso-walk: PASS - evidence in /tmp/evidence\n'
} > "${default_pass_log}"
check default "${default_pass_log}" || fail "a genuine default PASS log must PASS the default check"
if check ado "${default_pass_log}"; then fail "a default-profile PASS log must not pass the ado check (different closing line)"; fi
echo "ok  genuine default PASS passes only the default check"

# 5. a genuine ADO-profile PASS: specs executed and scripts/lib/kind-sso-walk-ado.sh's
#    own closing line (#695's target leg — the only one with a forward proxy +
#    walk CA + per-user Azure DevOps capture + spaced names). Must PASS the
#    ado check, and must FAIL the default check.
ado_pass_log="${TMP}/ado-pass.log"
{
  printf 'UI e2e summary: 1 spec file(s) passed\n'
  printf 'kind-sso-walk (ado): PASS - evidence in /tmp/evidence\n'
} > "${ado_pass_log}"
check ado "${ado_pass_log}" || fail "a genuine ado PASS log must PASS the ado check"
if check default "${ado_pass_log}"; then fail "an ado-profile PASS log must not pass the default check (different closing line)"; fi
echo "ok  genuine ado PASS passes only the ado check"

# 6. an unknown profile argument must FAIL loudly rather than silently
#    matching neither pattern.
if check bogus "${default_pass_log}"; then fail "an unknown profile must FAIL, not silently pass"; fi
echo "ok  an unknown profile fails"

echo "test-kind-sso-walk-log: PASS"
