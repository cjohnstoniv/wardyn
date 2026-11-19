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
# The /vN licence-URL flip go-licenses makes when its online probe fails normalises away; any other change does not.
eval "$(grep '^notices_vnorm()' "$ROOT/scripts/third-party-notices.sh")"
a='| `github.com/aws/aws-sdk-go-v2/internal/endpoints/v2` | Apache-2.0 | https://github.com/aws/aws-sdk-go-v2/blob/internal/endpoints/v2.8.7/internal/endpoints/v2/LICENSE.txt |'
b='| `github.com/aws/aws-sdk-go-v2/internal/endpoints/v2` | Apache-2.0 | https://github.com/aws/aws-sdk-go-v2/blob/internal/endpoints/v2.8.7/internal/endpoints/LICENSE.txt |'
c='| `github.com/aws/aws-sdk-go-v2/internal/endpoints/v2` | MIT | https://github.com/aws/aws-sdk-go-v2/blob/internal/endpoints/v2.8.7/internal/endpoints/v2/LICENSE.txt |'
[ "$(notices_vnorm <<<"$a")" = "$(notices_vnorm <<<"$b")" ] || { echo "FAIL: the /vN URL flip is reported as drift"; exit 1; }
[ "$(notices_vnorm <<<"$a")" != "$(notices_vnorm <<<"$c")" ] || { echo "FAIL: a licence change normalised away"; exit 1; }
echo "test-third-party-notices: ok"
