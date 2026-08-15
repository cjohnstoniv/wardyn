#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for W9-S1-1: a repo-kind scan-only run whose clone FAILED
# must not scan an empty workdir and upload a
# false-green high-confidence EMPTY profile. Exercises repo_scan_ok() from
# deploy/images/common/agent-run-lib.sh directly (no Docker/network needed —
# clone_one is never called; this only checks the post-clone .git presence
# gate that the WARDYN_SCAN_ONLY branch in agent-run consults).
#
# This script pre-dates repo_scan_ok on base 763beb5 — sourcing the lib and
# calling it fails with "command not found", which this script reports as a
# failure, satisfying "fails on base, passes after the fix".
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$ROOT/deploy/images/common/agent-run-lib.sh"

fail=0
check() {  # $1=description $2=expected(0|1) $3...=setup already done by caller
    local desc="$1" expected="$2" got
    if repo_scan_ok; then got=0; else got=1; fi
    if [[ "$got" != "$expected" ]]; then
        echo "FAIL: $desc — expected repo_scan_ok=$expected, got $got" >&2
        fail=1
    else
        echo "ok: $desc"
    fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# 1) dir-kind scan (no repo requested): always ok, even with an empty workdir.
workdir="$tmp/dirscan"; mkdir -p "$workdir"
unset WARDYN_REPOS WARDYN_REPO_URL 2>/dev/null || true
check "dir-kind scan, empty workdir" 0

# 2) repo-kind scan requested, clone FAILED (no .git anywhere under workdir):
# must refuse.
workdir="$tmp/repofail"; mkdir -p "$workdir"
export WARDYN_REPO_URL="https://example.invalid/x.git"
check "repo-kind scan, clone failed (no .git)" 1

# 3) repo-kind scan requested, clone SUCCEEDED (.git present one level down,
# matching resolve_workdir's clone_one destination layout): must pass.
workdir="$tmp/reposucceed"; mkdir -p "$workdir/myrepo/.git"
check "repo-kind scan, clone succeeded (.git present)" 0
unset WARDYN_REPO_URL

echo "--- test-repo-scan-ok: $([[ $fail -eq 0 ]] && echo PASS || echo FAIL) ---"
exit "$fail"
