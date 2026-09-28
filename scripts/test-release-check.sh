#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# F19: `make release-check`'s Postgres-gated `if` used to chain
# test-report-pg and test-race-pg with plain `;`, so the if's exit status
# was whichever submake ran LAST — a test-report-pg failure was silently
# swallowed whenever test-race-pg then passed, and release-check printed
# "release-check PASSED" anyway. This script stubs out $(MAKE) so it never
# touches Postgres, drives all four report/race outcome combinations through
# the real release-check recipe, and proves the exit code (and the PASSED
# banner) track a Postgres failure correctly.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

STUB="$(mktemp)"
trap 'rm -f "$STUB"' EXIT
cat >"$STUB" <<'EOF'
#!/usr/bin/env bash
echo "STUB target=$1"
case "$1" in
    test-report-pg) exit "${REPORT_RC:?}" ;;
    test-race-pg)   exit "${RACE_RC:?}" ;;
    *)              exit 99 ;;
esac
EOF
chmod +x "$STUB"

fail=0

check_case() {  # $1=REPORT_RC $2=RACE_RC $3=want ("zero" or "nonzero")
    report_rc="$1" race_rc="$2" want="$3"
    output="$(REPORT_RC="$report_rc" RACE_RC="$race_rc" WARDYN_TEST_PG=probe \
        make --no-print-directory -o ci release-check MAKE="$STUB" 2>&1)"
    rc=$?
    case "$want" in
        zero)
            if [ "$rc" != 0 ]; then
                echo "FAIL: report=$report_rc race=$race_rc rc=$rc (expected 0)"
                fail=1
            fi
            ;;
        nonzero)
            if [ "$rc" = 0 ]; then
                echo "FAIL: report=$report_rc race=$race_rc rc=$rc (expected non-zero)"
                fail=1
            fi
            if printf '%s' "$output" | grep -q "release-check PASSED"; then
                echo "FAIL: report=$report_rc race=$race_rc rc=$rc (printed 'release-check PASSED' despite a PG failure)"
                fail=1
            fi
            ;;
    esac
}

check_case 42 0  nonzero
check_case 0  43 nonzero
check_case 42 43 nonzero
check_case 0  0  zero

if [ "$fail" = 0 ]; then
    echo "--- test-release-check: PASS ---"
else
    echo "--- test-release-check: FAIL ---"
fi
exit "$fail"
