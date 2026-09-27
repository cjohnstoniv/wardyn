#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-kind-sso-walk-log.sh — asserts a scripts/kind-sso-walk.sh log actually
# EXECUTED the walk for the given profile, rather than self-skipping. An
# unset WARDYN_TEST_K8S makes kind-sso-walk.sh print "skipping" and exit 0 —
# identical, from nightly.yml's `if: always()` upload step, to a genuine PASS.
#
# Shared by .github/workflows/nightly.yml's two live kind SSO jobs —
# kind-sso-walk (default AWS/Dex profile) and kind-sso-ado-walk (the
# forward-proxy + walk-CA + per-user-capture + spaced-names Azure DevOps
# profile) — so the "actually executed" assertion is ONE checker instead of
# two copies that can drift (#695), and so scripts/test-kind-sso-walk-log.sh
# can feed it fixture logs without a kind cluster. default and ado close on
# DIFFERENT lines — scripts/kind-sso-walk.sh's own "kind-sso-walk: PASS" for
# default, scripts/lib/kind-sso-walk-ado.sh's "kind-sso-walk (ado): PASS" for
# ado — so the expected PASS line is picked from the profile argument, not
# hardcoded once.
#
# Usage: check-kind-sso-walk-log.sh <default|ado> <log-file>
set -euo pipefail

profile="${1:?usage: check-kind-sso-walk-log.sh <default|ado> <log-file>}"
log="${2:?usage: check-kind-sso-walk-log.sh <default|ado> <log-file>}"
[ -f "${log}" ] || { echo "::error::log file not found: ${log}" >&2; exit 1; }

case "${profile}" in
  default) pass_line='^kind-sso-walk: PASS' ;;
  ado)     pass_line='^kind-sso-walk (ado): PASS' ;;
  *) echo "::error::profile must be default or ado (got ${profile})" >&2; exit 1 ;;
esac

if grep -q 'set WARDYN_TEST_K8S=1 to run the cluster-dependent AWS SSO walk (skipping)' "${log}"; then
  echo "::error::kind-sso-walk.sh (${profile}) self-skipped — WARDYN_TEST_K8S did not reach the walk step" >&2
  exit 1
fi
if ! grep -qE 'UI e2e summary: [1-9][0-9]* spec file\(s\) passed' "${log}"; then
  echo "::error::no spec file was reported as executed and passed (${profile}) — see the uploaded log" >&2
  exit 1
fi
if ! grep -q "${pass_line}" "${log}"; then
  echo "::error::the walk (${profile}) never reached its own closing PASS line — see the uploaded log" >&2
  exit 1
fi
