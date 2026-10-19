#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for scripts/stage-agent-binary.sh's target-arch choice: --arch beats
# WARDYN_AGENT_ARCH, which beats `uname -m`, and an unknown arch is refused. curl and uname are
# shims on PATH (no network); the script runs from a scratch copy so nothing lands in the repo.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
mkdir -p "$T/scripts/lib" "$T/deploy/images/claude-code" "$T/deploy/images/codex-cli" "$T/bin" "$T/fix"
cp "$ROOT/scripts/stage-agent-binary.sh" "$T/scripts/"; cp "$ROOT/scripts/lib/common.sh" "$T/scripts/lib/"

printf 'x64 blob' > "$T/fix/linux-x64"; printf 'arm64 blob' > "$T/fix/linux-arm64"
sx="$(sha256sum "$T/fix/linux-x64" | cut -d' ' -f1)"; sa="$(sha256sum "$T/fix/linux-arm64" | cut -d' ' -f1)"
printf '{"platforms":{\n"linux-x64": {\n"checksum": "%s"\n},\n"linux-arm64": {\n"checksum": "%s"\n}}}\n' "$sx" "$sa" > "$T/fix/manifest"

cat > "$T/bin/curl" <<'SH'
#!/bin/sh
echo "$*" >> "$T/curl.log"
out=; url=
while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2;; -*) shift;; *) url="$1"; shift;; esac; done
case "$url" in
  */stable|*/latest) echo 9.9.9 ;;
  */manifest.json) cat "$T/fix/manifest" ;;
  */linux-x64/claude) cp "$T/fix/linux-x64" "$out" ;;
  */linux-arm64/claude) cp "$T/fix/linux-arm64" "$out" ;;
  *) cp "$T/fix/linux-x64" "$out" ;;
esac
SH
printf '#!/bin/sh\necho x86_64\n' > "$T/bin/uname"
chmod +x "$T/bin/curl" "$T/bin/uname"

fail() { echo "FAIL: $*" >&2; echo "--- test-stage-agent-binary: FAIL ---"; exit 1; }
BIN="$T/deploy/images/claude-code/claude-bin"
run() { env T="$T" PATH="$T/bin:$PATH" "$@" 2>&1; }

# stage <platform fixture wanted> <label> [VAR=value ...] -- <script args...>
stage() {
  local want="$1" label="$2"; shift 2
  rm -f "$BIN" "$T/curl.log"
  local envs=(); while [ "$1" != "--" ]; do envs+=("$1"); shift; done; shift
  local out
  out="$(run ${envs[@]+"${envs[@]}"} bash "$T/scripts/stage-agent-binary.sh" "$@")" || fail "$label: script exited non-zero: $out"
  cmp -s "$BIN" "$T/fix/$want" || fail "$label: staged the wrong blob (wanted $want)"
  grep -q "/${want}/claude" "$T/curl.log" || fail "$label: curl never asked for the $want path"
  echo "ok: $label -> $want"
}

stage linux-x64   "no flag resolves from uname"   -- claude-code 9.9.9
stage linux-arm64 "--arch arm64 on an amd64 host" -- claude-code 9.9.9 --arch arm64
stage linux-arm64 "--arch aarch64 spelling"       -- claude-code --arch aarch64 9.9.9
stage linux-arm64 "--arch=arm64 form"             -- claude-code 9.9.9 --arch=arm64
stage linux-arm64 "env alone"                     WARDYN_AGENT_ARCH=arm64 -- claude-code 9.9.9
stage linux-x64   "flag beats env"                WARDYN_AGENT_ARCH=arm64 -- claude-code 9.9.9 --arch amd64
stage linux-arm64 "channel + flag"                -- claude-code stable --arch arm64
grep -q "arm64" <(run bash "$T/scripts/stage-agent-binary.sh" claude-code 9.9.9 --arch arm64) || fail "the log line does not name the arch"
echo "ok: log line names the arch"

out="$(run bash "$T/scripts/stage-agent-binary.sh" claude-code 9.9.9 --arch riscv64)" && fail "unknown arch was accepted"
echo "$out" | grep -q "unsupported arch riscv64" || fail "unknown arch gave no clear message: $out"
echo "ok: unknown arch refused"
run bash "$T/scripts/stage-agent-binary.sh" claude-code 9.9.9 --arch >/dev/null && fail "--arch with no value was accepted"
echo "ok: --arch with no value refused"

out="$(run WARDYN_CODEX_BIN_URL=https://example.invalid/codex bash "$T/scripts/stage-agent-binary.sh" codex-cli --arch arm64)" || fail "codex-cli with --arch: $out"
echo "$out" | grep -q "arm64" || fail "codex-cli did not log the arch: $out"
echo "ok: codex-cli accepts --arch and logs it"
echo "--- test-stage-agent-binary: PASS ---"
