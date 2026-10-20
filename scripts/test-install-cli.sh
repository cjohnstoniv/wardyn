#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-install-cli.sh - runs the REAL scripts/install-cli.sh, with real curl,
# against a local TLS server that holds fixture release assets under
# /v<version>/. Network-free (loopback only), root-free, docker-free.
#
# What it pins (issue #3319): a checksum mismatch installs nothing; the "not
# checked" line without cosign and the refusal with --require-signature; a
# failing cosign refuses; a re-run replaces the binary by rename (new inode);
# --ca-file works and its absence fails; root is refused; the script never writes
# cli.json or touches a keychain; --artifact-base (flag, and WARDYN_ARTIFACT_BASE
# with the flag winning) fetches <base>/v<version>/ and a layout mismatch fails
# by name.
#
# The PATH the script sees is a symlink farm of the few tools it uses plus
# per-case stubs, so a cosign on the developer's machine never leaks in.
#
# INSTALL_CLI=<path> runs the same cases against another copy (a mutant).
#
# Usage: scripts/test-install-cli.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${INSTALL_CLI:-${ROOT}/scripts/install-cli.sh}"
PS1_SCRIPT="${ROOT}/scripts/install-cli.ps1"
fail() { echo "test-install-cli: FAIL: $*" >&2; exit 1; }
pass() { echo "PASS  $*"; }

for t in curl openssl python3 awk sha256sum; do
  command -v "$t" >/dev/null 2>&1 || { echo "test-install-cli: SKIP ($t not found)"; exit 0; }
done

tmp="$(mktemp -d)"
srv_pid=""
trap '[ -z "$srv_pid" ] || kill "$srv_pid" 2>/dev/null || true; chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT
SH="$(command -v sh)"
tools="$tmp/tools"; stubs="$tmp/stubs"; mkdir -p "$tools" "$stubs" "$tmp/tmp"
for t in curl awk head tail mktemp uname id chmod mv rm mkdir cat tr sha256sum shasum sed cp ls grep; do
  p="$(command -v "$t" 2>/dev/null || true)"
  [ -z "$p" ] || ln -s "$p" "$tools/$t"
done

# ── a TLS server over fixture assets ──────────────────────────────────────
VER=9.9.9
site="$tmp/site"; rel="$site/v$VER"; mkdir -p "$rel"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$tmp/key.pem" -out "$tmp/ca.pem" -days 2 \
  -subj "/CN=wardyn-test-ca" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
  -addext "basicConstraints=critical,CA:TRUE" >/dev/null 2>&1 || fail "could not make a test certificate"
cat > "$tmp/serve.py" <<'PY'
import functools, http.server, os, ssl, sys
root, cert, key, portfile = sys.argv[1:5]
log = os.path.join(os.path.dirname(portfile), "requests.log")
class H(http.server.SimpleHTTPRequestHandler):
    def log_message(self, fmt, *a):
        with open(log, "a") as f:
            f.write(self.path + "\n")
httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(H, directory=root))
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
with open(portfile, "w") as f:
    f.write(str(httpd.server_address[1]))
httpd.serve_forever()
PY
python3 -I "$tmp/serve.py" "$site" "$tmp/ca.pem" "$tmp/key.pem" "$tmp/port" 2>"$tmp/serve.err" &
srv_pid=$!
for _ in $(seq 1 50); do [ -s "$tmp/port" ] && break; sleep 0.1; done
[ -s "$tmp/port" ] || fail "the test server did not start: $(cat "$tmp/serve.err")"
BASE="https://127.0.0.1:$(cat "$tmp/port")"

for plat in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  printf '#!/bin/sh\necho "wardyn fixture %s %s"\n' "$VER" "$plat" > "$rel/wardyn-$plat"
done
publish_sums() { ( cd "$rel" && sha256sum ./wardyn-* > SHA256SUMS ); printf 'sig\n' > "$rel/SHA256SUMS.sig"; printf 'pem\n' > "$rel/SHA256SUMS.pem"; }
publish_sums

# ── harness ───────────────────────────────────────────────────────────────
home="$tmp/home"; d1="$home/bin1"; d2="$home/bin2"
reset_home() { chmod -R u+w "$home" 2>/dev/null || true; rm -rf "$home"; mkdir -p "$d1" "$d2"; : > "$tmp/requests.log"; rm -f "$stubs"/*; }
# run ARGS...: the script under sh, on the farm PATH. Sets rc and out.
run() {
  rc=0
  out="$(env -i HOME="$home" PATH="$stubs:$tools" TMPDIR="$tmp/tmp" WARDYN_INSTALL_CLI_DIRS="$d1 $d2" \
    ${ENV_EXTRA:-} "$SH" "$SCRIPT" "$@" 2>&1)" || rc=$?
}
stub() { printf '#!/bin/sh\n%s\n' "$2" > "$stubs/$1"; chmod +x "$stubs/$1"; }
requests() { wc -l < "$tmp/requests.log"; }
GOOD=(--version "$VER" --artifact-base "$BASE" --ca-file "$tmp/ca.pem")
expect_ok() { [ "$rc" = 0 ] || fail "$1: exit $rc, want 0: $out"; }
expect_refused() { [ "$rc" != 0 ] || fail "$1: exit 0, want a refusal: $out"; }
expect_nothing_installed() {
  [ -z "$(find "$d1" "$d2" -mindepth 1)" ] || fail "$1: something was installed: $(find "$d1" "$d2" -mindepth 1)"
}

# ── 1. no cosign: installs, says so plainly ───────────────────────────────
reset_home
run "${GOOD[@]}" --org https://wardyn.example
expect_ok "happy path"
[ -x "$d1/wardyn" ] || fail "happy path: $d1/wardyn is not an executable file: $out"
grep -q "wardyn fixture $VER" <<<"$("$d1/wardyn")" || fail "happy path: the installed binary is not the served one"
grep -q 'signature: not checked (cosign not found)' <<<"$out" || fail "no cosign: the 'not checked' line is missing: $out"
grep -q 'catches a broken download, not a tampered release' <<<"$out" || fail "the checksum line does not say what the check is: $out"
grep -q 'Next: wardyn login https://wardyn.example$' <<<"$out" || fail "--org did not print the exact login line: $out"
pass "installs without cosign and says the signature was not checked"

reset_home
run "${GOOD[@]}"
grep -q "Next: wardyn login <your organisation's URL>" <<<"$out" || fail "without --org the Next line is wrong: $out"
pass "without --org the next step names the placeholder"

# ── 2. a checksum mismatch installs nothing ───────────────────────────────
reset_home
asset="wardyn-$(uname -s | tr '[:upper:]' '[:lower:]')-$(case "$(uname -m)" in x86_64|amd64) echo amd64 ;; *) echo arm64 ;; esac)"
cp "$rel/$asset" "$tmp/asset.orig"
printf '#!/bin/sh\necho tampered\n' > "$rel/$asset"
run "${GOOD[@]}"
expect_refused "checksum mismatch"
grep -q 'checksum mismatch' <<<"$out" || fail "mismatch: no 'checksum mismatch' in: $out"
expect_nothing_installed "checksum mismatch"
cp "$tmp/asset.orig" "$rel/$asset"
pass "a checksum mismatch refuses and installs nothing (not even a stray temp file)"

# ── 3. --require-signature ────────────────────────────────────────────────
reset_home
run "${GOOD[@]}" --require-signature
expect_refused "--require-signature without cosign"
grep -q 'cosign is not on PATH' <<<"$out" || fail "--require-signature: the refusal does not say why: $out"
expect_nothing_installed "--require-signature without cosign"
[ "$(requests)" = 0 ] || fail "--require-signature without cosign fetched before refusing"
pass "--require-signature refuses without cosign, before any download"

# ── 4. cosign present ─────────────────────────────────────────────────────
reset_home
stub cosign 'echo "$@" > "'"$tmp"'/cosign.args"; echo "SSL_CERT_FILE=${SSL_CERT_FILE:-}" > "'"$tmp"'/cosign.env"; exit 0'
run "${GOOD[@]}" --require-signature
expect_ok "cosign passes"
grep -q 'signature: verified with cosign' <<<"$out" || fail "cosign ok: no 'verified' line: $out"
grep -q -- "--certificate-identity https://github.com/cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/v$VER " "$tmp/cosign.args" || fail "cosign was not given the exact tag identity: $(cat "$tmp/cosign.args")"
grep -q -- '--certificate-oidc-issuer https://token.actions.githubusercontent.com' "$tmp/cosign.args" || fail "cosign was not given the issuer"
grep -q "^SSL_CERT_FILE=$tmp/ca.pem\$" "$tmp/cosign.env" || fail "--ca-file did not reach the cosign child as SSL_CERT_FILE: $(cat "$tmp/cosign.env")"
! grep -q 'not checked' <<<"$out" || fail "cosign ran but the output still says not checked"
pass "cosign verifies the checksum file against the tag's identity; --ca-file reaches cosign alone"

reset_home
stub cosign 'echo "no matching signatures" >&2; exit 1'
run "${GOOD[@]}"
expect_refused "failing cosign"
grep -q 'cosign refused' <<<"$out" || fail "failing cosign: the refusal does not name cosign: $out"
expect_nothing_installed "failing cosign"
pass "a cosign failure refuses and installs nothing"

# ── 5. a re-run replaces by rename ────────────────────────────────────────
reset_home
run "${GOOD[@]}"; expect_ok "first install"
first="$(ls -i "$d1/wardyn" | awk '{print $1}')"
exec 9<"$d1/wardyn"   # hold the old file open: an in-place write would change it under us
run "${GOOD[@]}"; expect_ok "re-run"
second="$(ls -i "$d1/wardyn" | awk '{print $1}')"
old_still="$(cat <&9)"; exec 9<&-
[ "$first" != "$second" ] || fail "a re-run kept inode $first: the binary was written in place"
grep -q "wardyn fixture $VER" <<<"$old_still" || fail "the old binary was disturbed while the new one was installed"
[ -z "$(ls -A "$d1" | grep -v '^wardyn$')" ] || fail "a re-run left stray files: $(ls -A "$d1")"
pass "a re-run replaces the binary by rename (new inode, old file untouched, no strays)"

# ── 6. --ca-file ──────────────────────────────────────────────────────────
reset_home
run --version "$VER" --artifact-base "$BASE"
expect_refused "no --ca-file"
grep -q -- '--ca-file' <<<"$out" || fail "no --ca-file: the failure does not point at the flag: $out"
expect_nothing_installed "no --ca-file"
run --version "$VER" --artifact-base "$BASE" --ca-file "$tmp/nope.pem"
expect_refused "unreadable --ca-file"
pass "--ca-file is needed for a private CA, and its absence fails with the flag named"

# ── 7. root is refused ────────────────────────────────────────────────────
reset_home
stub id 'echo 0'
run "${GOOD[@]}"
expect_refused "root"
grep -q 'refusing to run as root' <<<"$out" || fail "root: no refusal line: $out"
expect_nothing_installed "root"
[ "$(requests)" = 0 ] || fail "root: a request left before the refusal"
pass "root is refused before anything is fetched"

# ── 8. never writes a sign-in, token, cli.json or keychain item ───────────
reset_home
stub security 'echo "$@" >> "'"$tmp"'/security.calls"'
stub secret-tool 'echo "$@" >> "'"$tmp"'/security.calls"'
: > "$tmp/security.calls"
run "${GOOD[@]}"; expect_ok "no-credential run"
[ ! -s "$tmp/security.calls" ] || fail "a keychain tool was called: $(cat "$tmp/security.calls")"
extra="$(find "$home" -mindepth 1 -not -path "$d1" -not -path "$d1/wardyn" -not -path "$d2" | head -3)"
[ -z "$extra" ] || fail "the installer wrote outside the one binary: $extra"
! grep -rq 'cli.json' "$SCRIPT" || fail "install-cli.sh mentions cli.json"
pass "no cli.json, no token, no keychain call, nothing but the binary"

# ── 9. the artifact base ──────────────────────────────────────────────────
reset_home
mkdir -p "$site/mirror"; rm -rf "$site/mirror/v$VER"; cp -r "$rel" "$site/mirror/v$VER"
run --version "$VER" --artifact-base "$BASE/mirror" --ca-file "$tmp/ca.pem"
expect_ok "--artifact-base"
grep -q "/mirror/v$VER" <<<"$out" || fail "--artifact-base: the output does not show the mirror URL: $out"
grep -q "^/mirror/v$VER/SHA256SUMS\$" "$tmp/requests.log" || fail "--artifact-base: SHA256SUMS was not fetched from <base>/v<version>/: $(cat "$tmp/requests.log")"
pass "--artifact-base fetches <base>/v<version>/"

reset_home
ENV_EXTRA="WARDYN_ARTIFACT_BASE=$BASE/mirror" run --version "$VER" --ca-file "$tmp/ca.pem"
expect_ok "WARDYN_ARTIFACT_BASE alone"
grep -q "^/mirror/v$VER/" "$tmp/requests.log" || fail "WARDYN_ARTIFACT_BASE was not used: $(cat "$tmp/requests.log")"
reset_home
ENV_EXTRA="WARDYN_ARTIFACT_BASE=$BASE/does-not-exist" run "${GOOD[@]}"
expect_ok "the flag beats WARDYN_ARTIFACT_BASE"
! grep -q 'does-not-exist' "$tmp/requests.log" || fail "the environment base was used although the flag was given"
reset_home
ENV_EXTRA="WARDYN_ARTIFACT_BASE=$BASE" run --version "$VER" --artifact-base "$BASE/does-not-exist" --ca-file "$tmp/ca.pem"
expect_refused "the flag beats a good WARDYN_ARTIFACT_BASE"
pass "WARDYN_ARTIFACT_BASE works, and the flag wins over it (both ways)"

# SHA256SUMS in place but the binary missing from the layout: named.
reset_home
mkdir -p "$site/partial/v$VER"; cp "$rel/SHA256SUMS" "$site/partial/v$VER/SHA256SUMS"
run --version "$VER" --artifact-base "$BASE/partial" --ca-file "$tmp/ca.pem"
expect_refused "layout mismatch"
grep -q "could not fetch wardyn-[a-z]*-[a-z0-9]* from $BASE/partial/v$VER/wardyn-" <<<"$out" || fail "a layout mismatch does not name the asset and URL: $out"
grep -q "<asset>" <<<"$out" || fail "a layout mismatch does not state the layout: $out"
expect_nothing_installed "layout mismatch"
reset_home
run --version "$VER" --artifact-base "$BASE/no-such" --ca-file "$tmp/ca.pem"
expect_refused "no layout at all"
grep -q 'could not fetch SHA256SUMS' <<<"$out" || fail "no layout: the failure does not name SHA256SUMS: $out"
pass "an --artifact-base layout mismatch fails by name"

reset_home
run --version "$VER" --artifact-base "http://127.0.0.1:1"
expect_refused "http base"
grep -q 'https://' <<<"$out" || fail "an http base is not refused as such: $out"
pass "a plain-http artifact base is refused"

# ── 10. versions ──────────────────────────────────────────────────────────
reset_home
run --artifact-base "$BASE" --ca-file "$tmp/ca.pem"
expect_refused "unstamped, no --version"
grep -q -- '--version' <<<"$out" || fail "unstamped: the refusal does not name --version: $out"
sed 's|^RELEASE_VERSION="@WARDYN_VERSION@"$|RELEASE_VERSION="'"$VER"'"|' "$SCRIPT" > "$tmp/stamped.sh"
grep -q "^RELEASE_VERSION=\"$VER\"\$" "$tmp/stamped.sh" || fail "the stamp line did not match; the release job's sed would not either"
SCRIPT_SAVE="$SCRIPT"; SCRIPT="$tmp/stamped.sh"
run --artifact-base "$BASE" --ca-file "$tmp/ca.pem"; expect_ok "stamped, no --version"
grep -q "^/v$VER/" "$tmp/requests.log" || fail "the stamped version was not used"
reset_home
mkdir -p "$site/v1.2.3"; cp "$rel"/* "$site/v1.2.3/"
sed "s|^RELEASE_VERSION=.*|RELEASE_VERSION=\"1.2.3\"|" "$SCRIPT" > "$tmp/stamped2.sh"; SCRIPT="$tmp/stamped2.sh"
run --version "v$VER" --artifact-base "$BASE" --ca-file "$tmp/ca.pem"; expect_ok "--version beats the stamp, v-prefix accepted"
grep -q "^/v$VER/" "$tmp/requests.log" && ! grep -q "^/v1.2.3/" "$tmp/requests.log" || fail "--version did not override the stamp"
SCRIPT="$SCRIPT_SAVE"
reset_home
run --version '../x' --artifact-base "$BASE"
expect_refused "version with a path"
[ "$(requests)" = 0 ] || fail "a version carrying ../ reached the network"
pass "the stamped version is the default, --version overrides it, and a path in a version is refused"

# ── 11. platform ──────────────────────────────────────────────────────────
reset_home
stub uname 'case "$1" in -s) echo FreeBSD ;; *) echo x86_64 ;; esac'
run "${GOOD[@]}"
expect_refused "unpublished OS"; grep -q 'FreeBSD' <<<"$out" || fail "the OS refusal does not name it: $out"
stub uname 'case "$1" in -s) echo Linux ;; *) echo mips ;; esac'
run "${GOOD[@]}"
expect_refused "unpublished arch"; grep -q 'mips' <<<"$out" || fail "the arch refusal does not name it: $out"
stub uname 'case "$1" in -s) echo Darwin ;; *) echo arm64 ;; esac'
run "${GOOD[@]}"
expect_ok "darwin/arm64"; grep -q 'darwin-arm64' <<<"$("$d1/wardyn")" || fail "Darwin arm64 did not fetch wardyn-darwin-arm64"
pass "an unpublished platform is refused by name; Darwin arm64 maps to wardyn-darwin-arm64"

# ── 12. where it goes, and the piped form ─────────────────────────────────
reset_home
chmod 555 "$d1"
run "${GOOD[@]}"
expect_ok "first dir unwritable"
[ -x "$d2/wardyn" ] && [ ! -e "$d1/wardyn" ] || fail "the first writable directory was not used"
chmod 755 "$d1"
reset_home
out="$(cat "$SCRIPT" | env -i HOME="$home" PATH="$stubs:$tools" TMPDIR="$tmp/tmp" WARDYN_INSTALL_CLI_DIRS="$d1 $d2" "$SH" -s -- "${GOOD[@]}" 2>&1)" || fail "the piped form (curl | sh) failed: $out"
[ -x "$d1/wardyn" ] || fail "the piped form installed nothing"
pass "the first writable directory wins, and the curl-pipe form works"

# ── 13. the Windows script ────────────────────────────────────────────────
[ -f "$PS1_SCRIPT" ] || fail "scripts/install-cli.ps1 is missing"
for needle in 'WARDYN_ARTIFACT_BASE' 'ArtifactBase' 'RequireSignature' 'Get-FileHash' 'SHA256SUMS' 'cosign' \
              'signature: not checked (cosign not found)' 'catches a broken download, not a tampered release' \
              'LOCALAPPDATA' 'wardyn-windows-' 'Move-Item' 'wardyn login'; do
  grep -qF -- "$needle" "$PS1_SCRIPT" || fail "install-cli.ps1 lost '$needle'"
done
! grep -q 'cli\.json' "$PS1_SCRIPT" || fail "install-cli.ps1 mentions cli.json"
if command -v pwsh >/dev/null 2>&1; then
  pwsh -NoProfile -Command "\$e = \$null; \$t = \$null; [void][System.Management.Automation.Language.Parser]::ParseFile('$PS1_SCRIPT', [ref]\$t, [ref]\$e); if (\$e.Count) { \$e | Out-String | Write-Host; exit 1 }" \
    || fail "install-cli.ps1 does not parse"
fi
pass "install-cli.ps1 carries the same checks (parsed by pwsh when present)"

echo "test-install-cli: PASS"
