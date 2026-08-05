#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Coverage floor gate over the real shipped set.
#
# `go test -coverprofile` measures ONE build, and Wardyn ships three: the
# tagless build, the `-tags docker` build, and the `-tags k8s` build. The
# docker build contains the container-hardening driver (internal/runner/docker),
# internal/envbuild, and the wardynd wiring that calls them; the k8s build
# contains the Kubernetes confinement substrate (internal/runner/k8s) — so a
# tagless-only total is a number for code that is not what ships. This unions
# the profiles from every build given (a block counts as covered if ANY build
# covered it) and enforces the floor on the union. No package is excluded to
# flatter the number.
#
# COUNTED: every package `go test ./...` reaches under ANY of the tag sets
# whose profile is passed in.
#
# NOT COUNTED — nothing is excluded, but some suites self-skip when their
# backing service is absent, so their lines land in the union as *uncovered*
# rather than being hidden from the denominator:
#   - real-Docker-daemon cases  (skip unless WARDYN_TEST_DOCKER=1)
#   - real-Kubernetes-cluster cases (skip unless WARDYN_TEST_K8S=1 + a kubeconfig)
#   - real-Postgres cases       (skip unless WARDYN_TEST_PG is set; see
#                                `make test-report-pg`)
# The per-PR CI job sets none of these, so the enforced number is exactly what
# CI can really run. The fake-clientset/fakeDocker-backed tests still cover
# those same drivers.
#
# The percentage itself is always `go tool cover -func`'s own total — this script
# only merges the profiles, it does not reimplement Go's coverage math.
#
# Usage: cover-union.sh <floor-pct> <out-dir> <profile.out> [profile.out...]
#        cover-union.sh --self-test
set -euo pipefail

# Merge text coverage profiles. A block present in both builds' profiles is the
# same source span, so key on "span numstmt" and sum the hit counts; blocks
# unique to one build (a file the other build cannot compile) carry through
# untouched. Emits a valid profile on stdout.
merge() {
  awk 'FNR==1 && /^mode:/ {next}
       NF==3 {c[$1" "$2] += $3}
       END {print "mode: atomic"; for (k in c) print k, c[k]}' "$@"
}

# `go tool cover -func`'s total for a profile, as a bare number ("66.1").
total_pct() {
  go tool cover -func="$1" | awk '/^total:/ {print $NF}' | tr -d '%'
}

self_test() {
  local d got want
  d="$(mktemp -d)"
  trap 'rm -rf "$d"' RETURN

  # Stands in for the two builds. a.out is the tagless build; b.out is the
  # -tags docker build, which alone sees b.go — a file the tagless build cannot
  # compile at all. a.go:1 is covered only by b's run, a.go:3 only by a's.
  printf 'mode: atomic\na.go:1.1,2.2 1 0\na.go:3.1,4.2 3 1\n' > "$d/a.out"
  printf 'mode: atomic\na.go:1.1,2.2 1 1\nb.go:1.1,2.2 4 0\n' > "$d/b.out"

  # The union must: sum counts for the shared block (0+1 => covered), keep each
  # build's exclusive blocks, and keep the docker-only b.go IN the denominator
  # as uncovered. That last part is the whole point — pulling the docker-only
  # file in is what drops the headline instead of flattering it.
  want=$'mode: atomic\na.go:1.1,2.2 1 1\na.go:3.1,4.2 3 1\nb.go:1.1,2.2 4 0'
  got="$(merge "$d/a.out" "$d/b.out" | LC_ALL=C sort)"
  [ "$got" = "$(printf '%s\n' "$want" | LC_ALL=C sort)" ] || {
    echo "self-test FAIL: union profile wrong" >&2
    diff <(printf '%s\n' "$want" | LC_ALL=C sort) <(printf '%s\n' "$got") >&2 || true
    exit 1
  }

  # Merging must be idempotent-safe on a single profile (no double counting).
  got="$(merge "$d/a.out" | LC_ALL=C sort)"
  [ "$got" = "$(printf 'mode: atomic\na.go:1.1,2.2 1 0\na.go:3.1,4.2 3 1' | LC_ALL=C sort)" ] ||
    { echo "self-test FAIL: single-profile merge altered the profile" >&2; exit 1; }
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  echo "cover-union: self-test PASS"
  exit 0
fi

FLOOR="${1:?usage: cover-union.sh <floor-pct> <out-dir> <profile.out>...}"
OUT="${2:?usage: cover-union.sh <floor-pct> <out-dir> <profile.out>...}"
shift 2
[ $# -gt 0 ] || { echo "cover-union: no coverage profiles given" >&2; exit 2; }
for p; do
  [ -s "$p" ] || { echo "cover-union: missing or empty coverage profile: $p" >&2; exit 2; }
done

mkdir -p "$OUT"
merge "$@" > "$OUT/cover.out"

for p; do printf '  %-48s %5s%%\n' "$p" "$(total_pct "$p")"; done

# Human-readable artifacts, and the enforced total, both from go tool cover.
go tool cover -func="$OUT/cover.out" > "$OUT/coverage-func.txt"
go tool cover -html="$OUT/cover.out" -o "$OUT/coverage.html" 2>/dev/null || true
TOTAL="$(awk '/^total:/ {print $NF}' "$OUT/coverage-func.txt" | tr -d '%')"

echo "Union Go coverage (tagless + -tags docker + -tags k8s): ${TOTAL}% (floor ${FLOOR}%)"
awk -v t="$TOTAL" -v m="$FLOOR" 'BEGIN{exit !(t + 0 >= m + 0)}' ||
  { echo "coverage ${TOTAL}% below floor ${FLOOR}%"; exit 1; }
