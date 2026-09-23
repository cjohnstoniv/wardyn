#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-report-diagnostics.sh — a red suite must NAME what went wrong in the
# job log itself, not only in the uploaded JSON artifact (G11: 13 of 23 main
# `build` failures since 09-19 showed only `make: *** [Makefile:195:
# test-report] Error 1`, and the test-pg failure on dedaf234 hid a compile
# error entirely).
#
# Runs the real script against a throwaway Go module with one failing test
# and one package that fails to COMPILE, and asserts the failing test's name
# and the compiler's own error text both land on stderr.
#
# Daemon-free, network-free: no substrate beyond `go test` itself.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/scripts"
cp "$ROOT/scripts/test-report.sh" "$TMP/scripts/"

cat > "$TMP/go.mod" <<'EOF'
module reportfixture

go 1.23
EOF

mkdir -p "$TMP/failpkg" "$TMP/buildbrokenpkg"
cat > "$TMP/failpkg/f_test.go" <<'EOF'
package failpkg

import "testing"

func TestNamedFailure(t *testing.T) {
	t.Fatal("this test always fails")
}
EOF
cat > "$TMP/buildbrokenpkg/b_test.go" <<'EOF'
package buildbrokenpkg

import "testing"

func TestNeverRuns(t *testing.T) {
	thisIdentifierDoesNotExist()
}
EOF

fail() { echo "FAIL: $*" >&2; exit 1; }

OUT="$( (cd "$TMP" && ./scripts/test-report.sh fixture ./... 2>&1 >/dev/null) )" && \
  fail "test-report.sh must exit non-zero when the suite has a failing test and a build error"

echo "$OUT" | grep -q "failpkg TestNamedFailure" \
  || fail "stderr must name the failing test (failpkg TestNamedFailure) — got:
$OUT"
echo "ok  names the failing test"

echo "$OUT" | grep -q "thisIdentifierDoesNotExist" \
  || fail "stderr must surface the compiler's own error text — got:
$OUT"
echo "ok  surfaces the compile error"
