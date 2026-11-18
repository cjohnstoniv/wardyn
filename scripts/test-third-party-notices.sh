#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-third-party-notices.sh — pins that scripts/third-party-notices.sh prints
# go-licenses' stderr and names the tool, not drift, when go-licenses fails
# (#3246), and that --check keeps the committed licence URL of a row go-licenses
# could not discover instead of calling it drift. A stub `go` that fails with a
# known line stands in for the tool.
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
# A licence URL go-licenses could not discover (`Unknown` plus a discovery error on stderr) keeps the committed URL; every other change stays.
eval "$(sed -n '/^notices_keep_url() {/,/^}/p' "$ROOT/scripts/third-party-notices.sh")"
cat > "$tmp/committed.md" <<'MD'
| Module | Licence | Source |
| `gopkg.in/inf.v0` | BSD-3-Clause | https://github.com/go-inf/inf/blob/v0.9.1/LICENSE |
| `gopkg.in/yaml.v3` | MIT | https://github.com/go-yaml/yaml/blob/v3.0.1/LICENSE |
MD
err_for() { printf 'W1111 16:39:52.818949    5279 report.go:108] Error discovering license URL: getting file URL in library %s: source.ModuleInfo(ctx, "%s", "v0.9.1"): fetchMeta\n' "$1" "$1"; }
{ err_for gopkg.in/inf.v0; err_for gopkg.in/new.v1; } > "$tmp/keep.err"
err_for gopkg.in/inf.v01 > "$tmp/prefix.err"
: > "$tmp/empty.err"
inf_url=https://github.com/go-inf/inf/blob/v0.9.1/LICENSE
keep() { notices_keep_url "$tmp/committed.md" "$1" 2> "$tmp/keep.warn"; }
got="$(keep "$tmp/keep.err" <<<"gopkg.in/inf.v0,Unknown,BSD-3-Clause")"
[ "$got" = "gopkg.in/inf.v0,$inf_url,BSD-3-Clause" ] || { echo "FAIL: K1 an unreachable licence URL was not kept; got: $got"; exit 1; }
grep -q 'gopkg.in/inf.v0' "$tmp/keep.warn" || { echo "FAIL: K1 no warning named the module"; exit 1; }
got="$(keep "$tmp/keep.err" <<<"gopkg.in/yaml.v3,Unknown,MIT")"
[ "$got" = "gopkg.in/yaml.v3,Unknown,MIT" ] || { echo "FAIL: K2 Unknown kept without a discovery error; got: $got"; exit 1; }
got="$(keep "$tmp/keep.err" <<<"gopkg.in/new.v1,Unknown,MIT")"
[ "$got" = "gopkg.in/new.v1,Unknown,MIT" ] || { echo "FAIL: K3 a module with no committed row was kept; got: $got"; exit 1; }
got="$(keep "$tmp/keep.err" <<<"gopkg.in/inf.v0,https://example.com/LICENSE,BSD-3-Clause")"
[ "$got" = "gopkg.in/inf.v0,https://example.com/LICENSE,BSD-3-Clause" ] || { echo "FAIL: K4 a real URL change was masked; got: $got"; exit 1; }
got="$(keep "$tmp/keep.err" <<<"gopkg.in/inf.v0,Unknown,MIT")"
[ "$got" = "gopkg.in/inf.v0,$inf_url,MIT" ] || { echo "FAIL: K5 the licence was taken from the committed row; got: $got"; exit 1; }
got="$(keep "$tmp/prefix.err" <<<"gopkg.in/inf.v0,Unknown,BSD-3-Clause")"
[ "$got" = "gopkg.in/inf.v0,Unknown,BSD-3-Clause" ] || { echo "FAIL: K6 an error for gopkg.in/inf.v01 kept gopkg.in/inf.v0; got: $got"; exit 1; }
printf '%s\n' "gopkg.in/inf.v0,Unknown,BSD-3-Clause" "github.com/a/b,https://github.com/a/b/blob/v1/LICENSE,MIT" "golang.org/x/sys,Unknown,BSD-3-Clause" > "$tmp/in.csv"
keep "$tmp/empty.err" < "$tmp/in.csv" > "$tmp/out.csv"
cmp -s "$tmp/in.csv" "$tmp/out.csv" || { echo "FAIL: K7 rows changed with an empty stderr"; exit 1; }
keep "$tmp/keep.err" < "$tmp/in.csv" > "$tmp/out.csv"
[ "$(wc -l < "$tmp/out.csv")" = 3 ] && [ "$(sed -n 2,3p "$tmp/out.csv")" = "$(sed -n 2,3p "$tmp/in.csv")" ] || { echo "FAIL: K7 an untouched row changed or the row count moved"; exit 1; }
echo "test-third-party-notices: ok"
