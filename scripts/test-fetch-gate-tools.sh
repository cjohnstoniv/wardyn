#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free test that scripts/fetch-gate-tools.sh pins the same tool versions
# the gates run (Makefile, license-headers.sh) and refuses a name it does not know.
# Usage: scripts/test-fetch-gate-tools.sh   (exit 0 = PASS, non-zero = FAIL)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "test-fetch-gate-tools: FAIL: $*" >&2; exit 1; }

list="$("${REPO_ROOT}/scripts/fetch-gate-tools.sh" --list)" || fail "--list failed"
for pin in "honnef.co/go/tools/cmd/staticcheck@$(grep -oP '^STATICCHECK_VERSION\s*\?=\s*\K\S+' "${REPO_ROOT}/Makefile")" \
           "github.com/google/go-licenses@$(grep -oP '^GO_LICENSES_VERSION\s*\?=\s*\K\S+' "${REPO_ROOT}/Makefile")" \
           "github.com/google/addlicense@v1.1.1"; do
  grep -qF -- " ${pin}" <<<"${list}" || fail "--list lacks ${pin}: ${list}"
done
[[ "$(wc -l <<<"${list}")" -eq 5 ]] || fail "want 5 tools, got: ${list}"

"${REPO_ROOT}/scripts/fetch-gate-tools.sh" no-such-tool >/dev/null 2>&1
[[ $? -eq 2 ]] || fail "an unknown tool name did not exit 2"

echo "test-fetch-gate-tools: PASS"
