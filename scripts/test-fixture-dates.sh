#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-fixture-dates.sh — check-fixture-dates.sh still SEES every shape of
# literal date it claims to count, in a throwaway git repo.
#
# Why this exists: the gate's awk classifier has no test of its own. If a
# later edit breaks the regex or the pathspec so nothing prints, every file
# counts 0 and the gate stays green while catching nothing — a gate that
# stops detecting is worse than no gate, so this pins BOTH directions: cases
# that must FAIL, and cases that must stay OK.
#
# Daemon-free, network-free: it runs the real gate against a throwaway repo.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/scripts"
cp "$ROOT/scripts/check-fixture-dates.sh" "$TMP/scripts/"
(cd "$TMP" && git init -q && git config user.email t@example.invalid && git config user.name t)

fail() { echo "FAIL: $*" >&2; exit 1; }

THIS_YEAR="$(date -u +%Y)"
NEXT_YEAR=$((THIS_YEAR + 1))
PAST_YEAR=2020 # always past; the gate only cares that it precedes THIS_YEAR

# run_gate — stage whatever is on disk and run the real gate against it.
run_gate() { (cd "$TMP" && git add -A && ./scripts/check-fixture-dates.sh); }

# 1. a date-time counts regardless of year — even one already past.
cat > "$TMP/probe_test.go" <<EOF
package probe

import "time"

func x() time.Time { return time.Time{} } // "${PAST_YEAR}-01-02T03:04" as a literal below
var _ = "${PAST_YEAR}-01-02T03:04"
EOF
if run_gate >/dev/null 2>&1; then fail "a date-time literal must FAIL regardless of year"; fi
echo "ok  date-time literal (any year) fails"

# 2. a quoted bare date this year or later must FAIL.
cat > "$TMP/probe_test.go" <<EOF
package probe

var _ = "${NEXT_YEAR}-01-02"
EOF
if run_gate >/dev/null 2>&1; then fail "a bare date this-year-or-later must FAIL"; fi
echo "ok  bare date this year or later fails"

# 3. a quoted bare date from an earlier year is always past: OK.
cat > "$TMP/probe_test.go" <<EOF
package probe

var _ = "${PAST_YEAR}-01-02"
EOF
run_gate >/dev/null 2>&1 || fail "a bare date from an earlier year must stay OK"
echo "ok  bare date from an earlier year stays OK"

# 4. Go's reference layout and zero time are formats, not points in time: OK.
cat > "$TMP/probe_test.go" <<EOF
package probe

import "time"

const layout = "2006-01-02T15:04"
var zero = time.Time{} // 0001-01-01T00:00
EOF
run_gate >/dev/null 2>&1 || fail "the reference layout / zero time must stay OK"
echo "ok  reference layout and zero time stay OK"

# 5. an unquoted bare date (no leading quote — e.g. inside a comment) never
#    counts: only a quoted bare date opens a string literal.
cat > "$TMP/probe_test.go" <<EOF
package probe

// released on ${NEXT_YEAR}-01-02, not a fixture
var x = 1
EOF
run_gate >/dev/null 2>&1 || fail "an unquoted bare date (e.g. in a comment) must stay OK"
echo "ok  unquoted bare date (comment) stays OK"

# 6. time.Date(YYYY, ...) this year or later must FAIL — the usual way a Go
#    fixture writes a time, with no YYYY-MM-DD string anywhere on the line
#    (#210: internal/api/runs_bedrock_ssoinject_test.go:69 was exactly this
#    shape and went uncounted before this gate learned to read it).
cat > "$TMP/probe_test.go" <<EOF
package probe

import "time"

var _ = time.Date(${NEXT_YEAR}, 1, 1, 0, 0, 0, 0, time.UTC)
EOF
if run_gate >/dev/null 2>&1; then fail "time.Date(this-year-or-later, …) must FAIL"; fi
echo "ok  time.Date(this year or later, …) fails"

# 7. time.Date(YYYY, ...) from an earlier year is always past: OK.
cat > "$TMP/probe_test.go" <<EOF
package probe

import "time"

var _ = time.Date(${PAST_YEAR}, 1, 1, 0, 0, 0, 0, time.UTC)
EOF
run_gate >/dev/null 2>&1 || fail "time.Date(earlier year, …) must stay OK"
echo "ok  time.Date(earlier year, …) stays OK"

# 8. new Date(YYYY, …) and Date.UTC(YYYY, …) in TS this year or later must
#    FAIL the same way as their Go counterpart.
rm -f "$TMP/probe_test.go"
cat > "$TMP/probe.test.ts" <<EOF
const a = new Date(${NEXT_YEAR}, 0, 1);
const b = Date.UTC(${NEXT_YEAR}, 0, 1);
EOF
mkdir -p "$TMP/ui/src"
mv "$TMP/probe.test.ts" "$TMP/ui/src/probe.test.ts"
if run_gate >/dev/null 2>&1; then fail "new Date(this-year-or-later, …) / Date.UTC(…) in TS must FAIL"; fi
echo "ok  new Date/Date.UTC (this year or later) fails in TS"

# 9. …and from an earlier year: OK.
cat > "$TMP/ui/src/probe.test.ts" <<EOF
const a = new Date(${PAST_YEAR}, 0, 1);
const b = Date.UTC(${PAST_YEAR}, 0, 1);
EOF
run_gate >/dev/null 2>&1 || fail "new Date/Date.UTC (earlier year) must stay OK"
echo "ok  new Date/Date.UTC (earlier year) stays OK"

echo "test-fixture-dates: PASS"
