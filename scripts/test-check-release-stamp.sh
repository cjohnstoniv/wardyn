#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-check-release-stamp.sh — check-release-stamp.sh passes a binary stamped "true" and refuses an
# unstamped one, a differently stamped one and a target it cannot read. A stub `go` stands in for
# the toolchain, so no Go build and no macOS is needed. Run by guard 31 of test-repo-guards.sh.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "test-check-release-stamp: FAIL: $*" >&2; exit 1; }

SYM=github.com/cjohnstoniv/wardyn/internal/version.releaseBuild
mkdir -p "$WORK/bin"
# The stub prints $NM for `go tool nm` and $INFO for `go version -m`.
cat >"$WORK/bin/go" <<'STUB'
#!/bin/bash
case "$1 $2" in
    "tool nm") printf '%s\n' "$NM" ;;
    "version -m") printf '%s\n' "$INFO" ;;
    *) exit 64 ;;
esac
STUB
chmod +x "$WORK/bin/go"
export PATH="$WORK/bin:$PATH"
INFO=$'x: go1.26\n\tbuild\tGOARCH=arm64\n\tbuild\tGOOS=darwin'

# A file whose bytes at offset 0x40 are the stamp text; the symbol address is that offset + 0x100000000.
printf '%064d%s' 0 true >"$WORK/stamped"
printf '%064d%s' 0 fals >"$WORK/other"
NM_STAMPED="100000040 R ${SYM}.str
100500000 D ${SYM}"
NM_PLAIN="100500000 B ${SYM}"

run() { NM="$1" INFO="$INFO" "$ROOT/scripts/check-release-stamp.sh" "$2" >/dev/null 2>&1 && echo 0 || echo $?; }
[ "$(run "$NM_STAMPED" "$WORK/stamped")" = 0 ] || fail "a binary stamped true was refused"
[ "$(run "$NM_PLAIN" "$WORK/stamped")" = 1 ] || fail "an unstamped binary (zero-initialised variable) passed"
[ "$(run "$NM_STAMPED" "$WORK/other")" = 1 ] || fail "a binary stamped with another value passed"
INFO=$'x: go1.26\n\tbuild\tGOARCH=amd64\n\tbuild\tGOOS=darwin'
[ "$(run "$NM_STAMPED" "$WORK/stamped")" = 2 ] || fail "a darwin/amd64 binary was read at the arm64 offset"
[ "$("$ROOT/scripts/check-release-stamp.sh" >/dev/null 2>&1 && echo 0 || echo $?)" = 2 ] || fail "no argument was not a usage error"
echo "test-check-release-stamp: PASS"
