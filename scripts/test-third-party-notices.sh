#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-third-party-notices.sh — pins that scripts/third-party-notices.sh prints
# go-licenses' stderr and names the tool, not drift, when go-licenses fails
# (#3246). A stub `go` that fails with a known line stands in for the tool.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/bin"
cat > "$tmp/bin/go" <<'STUB'
#!/usr/bin/env bash
echo "stub go-licenses: dial tcp: lookup proxy.golang.org: i/o timeout" >&2
exit 1
STUB
chmod +x "$tmp/bin/go"

for mode in --check --fix; do
  rc=0
  out="$(PATH="$tmp/bin:$PATH" "$ROOT/scripts/third-party-notices.sh" "$mode" 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] || { echo "FAIL: $mode exited 0 although go-licenses failed"; exit 1; }
  case "$out" in
    *"stub go-licenses: dial tcp: lookup proxy.golang.org: i/o timeout"*) ;;
    *) echo "FAIL: $mode did not print go-licenses stderr; got: $out"; exit 1 ;;
  esac
  case "$out" in
    *"go-licenses failed"*) ;;
    *) echo "FAIL: $mode did not say the tool failed; got: $out"; exit 1 ;;
  esac
  case "$out" in
    *"out of date"*) echo "FAIL: $mode reported drift for a tool failure"; exit 1 ;;
  esac
done
echo "test-third-party-notices: ok"
