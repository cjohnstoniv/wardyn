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
# Runs the real script against a throwaway Go module with one failing test,
# one package that fails to COMPILE, and two that fail with no test to name
# (os.Exit in TestMain, a panic in init), and asserts the failing test's name,
# the compiler's own error text and each package's cause all land on stderr.
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

mkdir -p "$TMP/failpkg" "$TMP/buildbrokenpkg" "$TMP/mainexitpkg" "$TMP/initpanicpkg"
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
cat > "$TMP/mainexitpkg/m_test.go" <<'EOF'
package mainexitpkg

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	fmt.Println("setup: cannot reach the fixture database")
	os.Exit(1)
}

func TestNeverRuns(t *testing.T) {}
EOF
cat > "$TMP/initpanicpkg/i_test.go" <<'EOF'
package initpanicpkg

import "testing"

var _ = func() int { panic("fixture init exploded") }()

func TestNeverRuns(t *testing.T) {}
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

for want in "mainexitpkg" "setup: cannot reach the fixture database" "initpanicpkg" "panic: fixture init exploded"; do
  echo "$OUT" | grep -qF "$want" \
    || fail "stderr must name a package that failed outside any test and its cause ($want) — got:
$OUT"
done
echo "ok  names a package that failed outside any test"
