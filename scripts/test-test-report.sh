#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free test for the two Go-suite controls in scripts/test-report.sh: the flaky-test quarantine
# (test/go-quarantine.txt, `go test -skip`) and the PG shard's test selection
# (scripts/pg-test-select.sh and its unit-run guard). Fixture modules under a temp dir; the checked-in
# quarantine file is validated against today's date, which is what turns every PR red the day an
# entry expires.
#
# Usage: scripts/test-test-report.sh   (exit 0 = PASS, non-zero = FAIL)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "${REPO_ROOT}/scripts/lib/e2e-quarantine.sh"

fail() { echo "test-test-report: FAIL: $*" >&2; exit 1; }
ok() { echo "  [pass] $*"; }

tmp="$(mktemp -d)"
report="tqfixture-$$"
trap 'rm -rf "${tmp}" "${REPO_ROOT}/test/reports/go/${report}"' EXIT
today="$(date -u +%F)"

out="$(quarantine_validate "${REPO_ROOT}/test/go-quarantine.txt" "${today}")" \
  || fail "test/go-quarantine.txt: ${out}"
ok "checked-in test/go-quarantine.txt passes against today"

# ── quarantine, through test-report.sh ────────────────────────────────────────
mkdir "${tmp}/q"
cat > "${tmp}/q/go.mod" <<'MOD'
module fixture

go 1.20
MOD
cat > "${tmp}/q/a_test.go" <<'GO'
package fixture

import "testing"

func TestKeep(t *testing.T)  {}
func TestFlaky(t *testing.T) { t.Fatal("flaky") }
func TestSub(t *testing.T) {
	t.Run("good", func(t *testing.T) {})
	t.Run("bad", func(t *testing.T) { t.Fatal("flaky subtest") })
}
GO
run_report() { # <quarantine file>: runs the fixture suite, prints its log, returns its exit code
  (cd "${tmp}/q" && WARDYN_GO_QUARANTINE="$1" WARDYN_TEST_REPORT_NAME="${report}" \
    WARDYN_TEST_REPORT_SKIP_FLOOR=1 WARDYN_TEST_REPORT_COVER=0 "${REPO_ROOT}/scripts/test-report.sh" fixture ./... 2>&1)
}
future="$(date -u -d "${today} + 3 days" +%F)"
past="$(date -u -d "${today} - 1 day" +%F)"
far="$(date -u -d "${today} + 30 days" +%F)"

: > "${tmp}/none.txt"
run_report "${tmp}/none.txt" >/dev/null && fail "control: the fixture's TestFlaky should fail an unquarantined run"
ok "control: an unquarantined flaky test fails the suite"

echo "fixture | TestFlaky | #12 | @cjohnstoniv | ${past}" > "${tmp}/expired.txt"
log="$(run_report "${tmp}/expired.txt")" && fail "an expired entry must fail the suite"
grep -q "expired on ${past}" <<<"${log}" || fail "expired entry: no 'expired' message in: ${log}"
grep -q "running suite" <<<"${log}" && fail "expired entry: the suite ran anyway"
ok "an expired entry fails before anything runs"

echo "fixture | TestFlaky | #12 | @cjohnstoniv | ${far}" > "${tmp}/far.txt"
run_report "${tmp}/far.txt" >/dev/null && fail "an entry past the 14-day cap must fail the suite"
echo "fixture | TestFlaky | cjohnstoniv | ${future}" > "${tmp}/bad.txt"
run_report "${tmp}/bad.txt" >/dev/null && fail "a malformed entry must fail the suite"
ok "a >14-day and a malformed entry fail the suite"

printf 'fixture | TestFlaky | #12 | @cjohnstoniv | %s\nfixture | TestSub/bad | #13 | @cjohnstoniv | %s\n' "${future}" "${future}" > "${tmp}/valid.txt"
log="$(run_report "${tmp}/valid.txt")" || fail "valid entries should let the suite pass, got: ${log}"
grep -q "QUARANTINED" <<<"${log}" || fail "the quarantined tests are not printed: ${log}"
grep -q "fixture TestFlaky #12" <<<"${log}" || fail "the summary does not name TestFlaky: ${log}"
json="${REPO_ROOT}/test/reports/go/${report}/test-output.json"
grep -q '"Test":"TestFlaky"' "${json}" && fail "TestFlaky ran although quarantined"
grep -q '"Action":"pass","Package":"fixture","Test":"TestKeep"' "${json}" || fail "TestKeep did not run"
grep -q '"Action":"pass","Package":"fixture","Test":"TestSub/good"' "${json}" || fail "TestSub/good did not run"
grep -q '"Test":"TestSub/bad"' "${json}" && fail "TestSub/bad ran although quarantined"
ok "a valid entry skips exactly the named test (and subtest), the rest run, the summary names them"

# ── PG test selection ────────────────────────────────────────────────────────
mkdir "${tmp}/s"
cat > "${tmp}/s/go.mod" <<'MOD'
module fixture

go 1.20
MOD
cat > "${tmp}/s/gate_test.go" <<'GO'
package fixture

import (
	"os"
	"testing"

	"fixture/pgtestutil"
)

var testfloor = struct{ Mark func(*testing.T, string) }{func(*testing.T, string) {}}

func pgOpen(t *testing.T) {
	if os.Getenv("WARDYN_TEST_PG") == "" {
		t.Skip("WARDYN_TEST_PG not set")
	}
}

func helper(t *testing.T) { pgOpen(t) }

// An export_test.go alias: a var, not a func, that a test reaches the gate through.
var Alias = helper

type fx struct{}

func (fx) viaMethod(t *testing.T) { helper(t) }

// TestInComment mentions WARDYN_TEST_PG only here.
func TestInComment(t *testing.T) {}
func TestPlain(t *testing.T)     {}
func TestDirect(t *testing.T)    { pgOpen(t) }
func TestViaHelper(t *testing.T) { helper(t) }
func TestViaMethod(t *testing.T) { fx{}.viaMethod(t) }
func TestViaAlias(t *testing.T)  { Alias(t) }
func TestCrossPkg(t *testing.T)  { pgtestutil.Open2(t) }
func TestProbe(t *testing.T) {
	testfloor.Mark(t, "pg")
}
GO
# A gate that lives in a helper package (no _test.go file), reached through a second helper.
mkdir "${tmp}/s/pgtestutil"
cat > "${tmp}/s/pgtestutil/pgtestutil.go" <<'GO'
package pgtestutil

import (
	"os"
	"testing"
)

func Open(t *testing.T) {
	if os.Getenv("WARDYN_TEST_PG") == "" {
		t.Skip("WARDYN_TEST_PG not set")
	}
}

func Open2(t *testing.T) { Open(t) }
GO
got="$(cd "${tmp}/s" && "${REPO_ROOT}/scripts/pg-test-select.sh" ./...)" || fail "selector failed on the fixture"
want='^(TestCrossPkg|TestDirect|TestProbe|TestViaAlias|TestViaHelper|TestViaMethod)$'
[ "${got}" = "${want}" ] || fail "selector picked ${got}, want ${want}"
ok "selector: direct, helper, method, alias, cross-package and probe tests selected; plain and comment-only not"

# The guard: a unit run (DSN unset) where TestHidden skipped naming WARDYN_TEST_PG, but the selector
# cannot see how it reaches the gate, must fail; one the selector does see must pass.
ev() { printf '{"Action":"%s","Package":"fixture","Test":"%s"%s}\n' "$1" "$2" "${3:-}"; }
{
  ev output TestDirect ',"Output":"    gate_test.go:1: WARDYN_TEST_PG not set\\n"'; ev skip TestDirect
  ev pass TestPlain
} > "${tmp}/unit-ok.json"
(cd "${tmp}/s" && "${REPO_ROOT}/scripts/pg-test-select.sh" --guard "${tmp}/unit-ok.json" ./... >/dev/null 2>&1) \
  || fail "guard rejected a selected PG test"
{
  cat "${tmp}/unit-ok.json"
  ev output TestHidden ',"Output":"    x_test.go:9: WARDYN_TEST_PG not set\\n"'; ev skip TestHidden
} > "${tmp}/unit-bad.json"
glog="$(cd "${tmp}/s" && "${REPO_ROOT}/scripts/pg-test-select.sh" --guard "${tmp}/unit-bad.json" ./... 2>&1)" \
  && fail "guard passed a PG-skipping test outside the selection"
grep -q "fixture TestHidden" <<<"${glog}" || fail "guard did not name TestHidden: ${glog}"
ok "guard: a test that skips for WARDYN_TEST_PG outside the selection fails, naming it"
# The shard package lists: api by default, store on request, anything else refused; the store shard
# is the old whole-package list, so a package dropped from it would silently lose its PG tests.
sel="${REPO_ROOT}/scripts/pg-test-select.sh"
[ "$("${sel}" --packages)" = "$("${sel}" --packages api)" ] || fail "--packages without a set is not api"
grep -q 'test/apie2e' <<<"$("${sel}" --packages api)" || fail "api set lost test/apie2e"
for p in internal/store internal/db internal/secretstore internal/broker internal/recording cmd/wardynd internal/notify internal/livebus; do
  grep -qF "./${p}/..." <<<"$("${sel}" --packages store)" || fail "store set lost ${p}"
done
"${sel}" --packages nope >/dev/null 2>&1 && fail "--packages accepted an unknown set"
ok "selector: --packages names the api and store shard lists, refuses others"
echo "test-test-report: PASS"
