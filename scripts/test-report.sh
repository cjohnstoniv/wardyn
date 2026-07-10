#!/usr/bin/env bash
# Run a Go test suite and emit detailed reports under test/reports/go/<suite>/:
#   - test-output.json  : raw go test -json event stream
#   - report.md         : human report (per-test result, duration, failure reason)
#   - cover.out         : coverage profile
#   - coverage.html     : HTML coverage (go tool cover)
#   - coverage-func.txt : per-func coverage + total
#
# Usage:
#   scripts/test-report.sh <suite-name> "<report title>" [go test args/pkgs...]
# Examples:
#   scripts/test-report.sh unit "Go unit tests" ./...
#   scripts/test-report.sh docker "Go docker tests" -tags docker ./internal/runner/...
#
# Honors env: GOFLAGS, WARDYN_TEST_PG, WARDYN_TEST_DOCKER (passed through to go test).
# Exit code mirrors the test run (non-zero if any test failed).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SUITE="${1:?usage: test-report.sh <suite> <title> [go test args...]}"
TITLE="${2:?usage: test-report.sh <suite> <title> [go test args...]}"
shift 2
PKGS=("$@")
if [ ${#PKGS[@]} -eq 0 ]; then PKGS=("./..."); fi

OUT="$ROOT/test/reports/go/$SUITE"
mkdir -p "$OUT"

echo ">> running suite '$SUITE': go test -json ${PKGS[*]}"
# -coverprofile with atomic mode; capture the JSON stream to a file.
go test -json -covermode=atomic -coverprofile="$OUT/cover.out" "${PKGS[@]}" \
  > "$OUT/test-output.json"
GO_EXIT=$?

# Detailed markdown from the event stream.
python3 "$ROOT/scripts/test2md.py" --title "$TITLE" \
  --out "$OUT/report.md" < "$OUT/test-output.json"

# Coverage artifacts (best-effort; cover.out may be absent if build failed).
if [ -s "$OUT/cover.out" ]; then
  go tool cover -func="$OUT/cover.out" > "$OUT/coverage-func.txt" 2>/dev/null
  go tool cover -html="$OUT/cover.out" -o "$OUT/coverage.html" 2>/dev/null
  TOTAL=$(grep -E "^total:" "$OUT/coverage-func.txt" | awk '{print $NF}')
  echo ">> coverage total: ${TOTAL:-n/a}"
fi

echo ">> reports in $OUT"
exit $GO_EXIT
