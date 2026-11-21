#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Checks the macOS 26.0 floor guard (above26) in ci.yml and release.yml: both copies are identical, and each
# compares major, minor and patch, so a slice or Distribution that asks for 26.0.1 is refused against a
# 26.0 floor while 26 and 26.0 pass. The function text is read from the workflows, so the test runs what
# they run.
#
# Usage: scripts/test-min-os-guard.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
fail() { echo "test-min-os-guard: FAIL: $*" >&2; exit 1; }

def() { grep -h '^ *above26() ' "${REPO_ROOT}/.github/workflows/$1" | sed 's/^ *//'; }
ci="$(def ci.yml)"; rel="$(def release.yml)"
[ -n "$ci" ] && [ "$(wc -l <<<"$ci")" = 1 ] || fail "ci.yml does not hold exactly one above26"
[ "$ci" = "$rel" ] || fail "ci.yml and release.yml carry different above26 functions"
eval "$ci"

# version -> 1 when it is above the 26.0 floor, 0 when it is at or below it.
while read -r v want; do
  got=0; if above26 "$v"; then got=1; fi
  [ "$got" = "$want" ] || fail "above26 $v = $got, want $want"
done <<'CASES'
25 0
25.9.9 0
26 0
26.0 0
26.0.0 0
26.0.1 1
26.1 1
26.1.0 1
27 1
27.0 1
CASES
echo "test-min-os-guard: ok"
