#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-up-seed-host-proxy.sh — scripts/lib/up-seed.sh seed_host_proxy extracts the host's
# own CLI from the wardynd image: the per-OS/arch binary, never a foreign one,
# and never over a native bin/wardyn that runs here. Drives the real function
# against a stub `docker` and fixture binaries; needs no daemon.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "FAIL: $*" >&2; exit 1; }

body="$(sed -n '/^_shp_sum() /p;/^seed_host_proxy() {/,/^}/p' "${ROOT}/scripts/lib/up-seed.sh")"
[ -n "${body}" ] || fail "scripts/lib/up-seed.sh has no seed_host_proxy()"
eval "${body}"

tmp="$(mktemp -d)"; trap 'rm -rf "${tmp}"' EXIT
mkdir -p "${tmp}/img" "${tmp}/stubs"
# The stub image: files under ${tmp}/img stand for /host/* in the container.
cat > "${tmp}/stubs/docker" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  create) echo cid ;;
  rm) ;;
  cp) src="${2#cid:/host/}"; [ -f "${IMG}/${src}" ] && cp "${IMG}/${src}" "$3" || exit 1 ;;
esac
STUB
chmod +x "${tmp}/stubs/docker"
export IMG="${tmp}/img" PATH="${tmp}/stubs:${PATH}"
mkfix() { printf '#!/bin/sh\necho "%s"\n' "$2" > "$1"; chmod +x "$1"; }   # a CLI that runs
badfix() { printf 'not an executable for this host\n' > "$1"; chmod +x "$1"; }  # exec format error

warn() { WARNED=1; }
log() { :; }
env_set() { :; }
host_goarch() { echo amd64; }
REPO_ROOT="${tmp}/repo"
ENV_FILE="${tmp}/env"

reset() { rm -rf "${REPO_ROOT}"; mkdir -p "${REPO_ROOT}/bin"; WARNED=0; }

# 1. A pulled image on an Apple Silicon host: the per-platform binary is taken,
#    not /host/wardyn (linux/amd64).
reset; rm -f "${IMG}"/*; mkfix "${IMG}/wardyn" linux-amd64; mkfix "${IMG}/wardyn-darwin-arm64" darwin-arm64
IMAGES_ORIGIN=pulled WARDYN_HOST_GOOS=darwin WARDYN_HOST_GOARCH=arm64 seed_host_proxy
[ "$("${REPO_ROOT}/bin/wardyn" --help)" = darwin-arm64 ] || fail "darwin/arm64 host did not get /host/wardyn-darwin-arm64"

# 2. A pulled image with no per-platform file (an older image) on a non-amd64 host:
#    the linux/amd64 /host/wardyn is NOT used.
reset; rm -f "${IMG}"/*; mkfix "${IMG}/wardyn" linux-amd64
IMAGES_ORIGIN=pulled WARDYN_HOST_GOOS=darwin WARDYN_HOST_GOARCH=arm64 seed_host_proxy
[ ! -e "${REPO_ROOT}/bin/wardyn" ] || fail "a linux/amd64 /host/wardyn was installed on a darwin/arm64 host"
[ "${WARNED}" = 1 ] || fail "no go-install hint when the image has no CLI for this host"

# 3. An existing native CLI is never overwritten.
reset; rm -f "${IMG}"/*; mkfix "${IMG}/wardyn" linux-amd64; mkfix "${IMG}/wardyn-linux-amd64" linux-amd64
mkfix "${REPO_ROOT}/bin/wardyn" mine
IMAGES_ORIGIN=pulled WARDYN_HOST_GOOS=linux WARDYN_HOST_GOARCH=amd64 seed_host_proxy
[ "$("${REPO_ROOT}/bin/wardyn" --help)" = mine ] || fail "an existing native bin/wardyn was overwritten"

# 4. A foreign-arch seed left by an earlier setup (cannot run) is replaced.
reset; rm -f "${IMG}"/*; mkfix "${IMG}/wardyn-linux-arm64" linux-arm64
badfix "${REPO_ROOT}/bin/wardyn"
IMAGES_ORIGIN=pulled WARDYN_HOST_GOOS=linux WARDYN_HOST_GOARCH=arm64 seed_host_proxy
[ "$("${REPO_ROOT}/bin/wardyn" --help)" = linux-arm64 ] || fail "an unrunnable bin/wardyn was not replaced by the host's CLI"

# 5. A binary this function extracted earlier (marked) is refreshed.
mkfix "${IMG}/wardyn-linux-arm64" linux-arm64-v2
IMAGES_ORIGIN=pulled WARDYN_HOST_GOOS=linux WARDYN_HOST_GOARCH=arm64 seed_host_proxy
[ "$("${REPO_ROOT}/bin/wardyn" --help)" = linux-arm64-v2 ] || fail "a previously extracted bin/wardyn was not refreshed"

# 6. Nothing runs and nothing extracts: a broken seed is removed, not left for the hand-off.
reset; rm -f "${IMG}"/*; badfix "${REPO_ROOT}/bin/wardyn"
IMAGES_ORIGIN=pulled WARDYN_HOST_GOOS=darwin WARDYN_HOST_GOARCH=arm64 seed_host_proxy
[ ! -e "${REPO_ROOT}/bin/wardyn" ] || fail "an unrunnable bin/wardyn was left in place"

# 7. Seed, then the operator builds their own CLI over it: the next `up` keeps the
#    native build. The marker holds the sha256 of what was extracted, so a changed
#    file is no longer ours (the old bare marker overwrote it).
reset; rm -f "${IMG}"/*; mkfix "${IMG}/wardyn-linux-amd64" image-cli
IMAGES_ORIGIN=pulled WARDYN_HOST_GOOS=linux WARDYN_HOST_GOARCH=amd64 seed_host_proxy
[ "$("${REPO_ROOT}/bin/wardyn" --help)" = image-cli ] || fail "first up did not seed the image CLI"
mkfix "${REPO_ROOT}/bin/wardyn" native-build
mkfix "${IMG}/wardyn-linux-amd64" image-cli-v2
IMAGES_ORIGIN=pulled WARDYN_HOST_GOOS=linux WARDYN_HOST_GOARCH=amd64 seed_host_proxy
[ "$("${REPO_ROOT}/bin/wardyn" --help)" = native-build ] || fail "a native build made after seeding was overwritten on re-up"

# 8. A seed from before 0.10 has no marker: it is kept, not guessed at.
reset; rm -f "${IMG}"/*; mkfix "${IMG}/wardyn-linux-amd64" image-cli
mkfix "${REPO_ROOT}/bin/wardyn" old-seed
IMAGES_ORIGIN=pulled WARDYN_HOST_GOOS=linux WARDYN_HOST_GOARCH=amd64 seed_host_proxy
[ "$("${REPO_ROOT}/bin/wardyn" --help)" = old-seed ] || fail "a marker-less bin/wardyn was overwritten"

echo "test-up-seed-host-proxy: PASS"
