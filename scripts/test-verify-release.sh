#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free, network-free behaviour test for scripts/verify-release.sh (#1496).
#
# THE property: `verify-release.sh X.Y.Z` accepts a signature made by the release
# workflow run for tag vX.Y.Z and REFUSES one made for any other tag. It used to
# pin only "release.yml at some refs/tags/v*", so an image or checksum file
# signed by an older tag's run passed when a newer one was requested.
#
# A Go regexp test of the expression proves nothing about the script, so this
# runs the real script against stubs on PATH (cosign, docker, gh, helm, curl).
# The stub cosign MODELS cosign's matching: an exact --certificate-identity must
# equal the signer's identity, an --certificate-identity-regexp must match it,
# and the signer is $SIGNER_TAG. The old regexp form therefore accepts any tag
# and this test fails on it. The stubs never reach a network, a registry or a
# release, and the script is run read-only: any mutating gh/docker verb fails.
#
# Usage: scripts/test-verify-release.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERIFY="${REPO_ROOT}/scripts/verify-release.sh"
RELEASE_YML="${REPO_ROOT}/.github/workflows/release.yml"
fail() { echo "test-verify-release: FAIL: $*" >&2; exit 1; }
pass() { echo "PASS  $*"; }

dir="$(mktemp -d)"; trap 'rm -rf "${dir}"' EXIT
bin="${dir}/bin"; mkdir -p "${bin}"
WANT="$(sed -n '/for want in/,/; do/p' "${RELEASE_YML}" | sed -e 's/.*for want in//' -e 's/; do.*//' -e 's/\\$//' | tr -s ' \n' '\n\n' | grep .)"
IMAGES="$(grep -oE '^[[:space:]]+- name: [a-z0-9-]+$' "${RELEASE_YML}" | awk '{print $3}' | sort -u)"
[ -n "${WANT}" ] && [ -n "${IMAGES}" ] || fail "could not derive the image/asset lists from release.yml"
printf '%s\n' "${WANT}" > "${dir}/want.txt"; printf '%s\n' "${IMAGES}" > "${dir}/images.txt"

cat > "${bin}/cosign" <<'STUB'
#!/usr/bin/env bash
echo "cosign $*" >> "${LOG}"
mode="" id="" re="" iss=""
while [ $# -gt 0 ]; do
  case "$1" in
    --certificate-identity) id="$2"; shift ;;
    --certificate-identity-regexp) re="$2"; shift ;;
    --certificate-oidc-issuer) iss="$2"; shift ;;
  esac
  shift
done
signer="https://github.com/cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/${SIGNER_TAG}"
[ "${iss}" = "https://token.actions.githubusercontent.com" ] || { echo "no issuer pinned" >&2; exit 1; }
if [ -n "${id}" ]; then [ "${id}" = "${signer}" ] && exit 0; echo "identity mismatch" >&2; exit 1; fi
if [ -n "${re}" ]; then [[ "${signer}" =~ ${re} ]] && exit 0; echo "regexp mismatch" >&2; exit 1; fi
echo "no identity pinned" >&2; exit 1
STUB
cat > "${bin}/docker" <<'STUB'
#!/usr/bin/env bash
echo "docker $*" >> "${LOG}"
case "$*" in
  *"buildx imagetools inspect"*"Manifest.Digest"*) echo '"sha256:0000000000000000000000000000000000000000000000000000000000000001"' ;;
  *"buildx imagetools inspect"*) echo "linux/amd64 linux/arm64 " ;;
  *) echo "docker: mutating or unexpected verb: $*" >&2; exit 99 ;;
esac
STUB
cat > "${bin}/gh" <<'STUB'
#!/usr/bin/env bash
echo "gh $*" >> "${LOG}"
case "$1 $2" in
  "release view")
    echo "prerelease=false draft=false assets=0"
    while read -r i; do echo "sbom-$i.cdx.json"; done < "${DIR}/images.txt"
    cat "${DIR}/want.txt"
    echo "wardyn-${VER}.tgz" ;;
  "release download")
    while [ $# -gt 0 ]; do [ "$1" = "--dir" ] && out="$2"; shift; done
    echo binary > "${out}/wardyn-linux-amd64"
    ( cd "${out}" && sha256sum wardyn-linux-amd64 > SHA256SUMS )
    echo "releases/download/v${VER}/install.sh" > "${out}/install.sh"
    if [ "${SIGFORM}" = bundle ]; then echo b > "${out}/SHA256SUMS.bundle"
    else echo s > "${out}/SHA256SUMS.sig"; echo p > "${out}/SHA256SUMS.pem"; fi ;;
  *) echo "gh: mutating or unexpected verb: $*" >&2; exit 99 ;;
esac
STUB
cat > "${bin}/helm" <<'STUB'
#!/usr/bin/env bash
echo "helm $*" >> "${LOG}"
printf 'name: wardyn\nversion: %s\nappVersion: %s\n' "${VER}" "${VER}"
STUB
cat > "${bin}/curl" <<'STUB'
#!/usr/bin/env bash
echo "curl $*" >> "${LOG}"
echo 302
STUB
chmod +x "${bin}"/*

# run VERSION SIGNER_TAG SIGFORM -> sets rc and out
run() {
  : > "${dir}/log"
  rc=0
  out="$(PATH="${bin}:${PATH}" LOG="${dir}/log" DIR="${dir}" VER="$1" SIGNER_TAG="$2" SIGFORM="$3" "${VERIFY}" "$1" 2>&1)" || rc=$?
}

for form in bundle sigpem; do
  # 1. the signer IS the requested tag: passes, exactly, for every image and the blob.
  run 0.8.3 v0.8.3 "${form}"
  [ "${rc}" = 0 ] || fail "V=0.8.3 signed by v0.8.3 (${form}) exited ${rc}, want 0: $(printf '%s' "${out}" | tail -5)"
  printf '%s' "${out}" | grep -q '^fails=0$' || fail "V=0.8.3 signed by v0.8.3 (${form}) did not report fails=0"
  id='https://github.com/cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/v0.8.3'
  nimg="$(wc -l < "${dir}/images.txt")"
  nverify="$(grep -c "^cosign verify .*--certificate-identity ${id} --certificate-oidc-issuer https://token.actions.githubusercontent.com\$" "${dir}/log" || true)"
  [ "${nverify}" = "${nimg}" ] || fail "(${form}) ${nverify} of ${nimg} 'cosign verify' calls carried the exact identity + OIDC issuer: $(grep '^cosign' "${dir}/log" | head -3)"
  grep -q "^cosign verify-blob .*--certificate-identity ${id} --certificate-oidc-issuer https://token.actions.githubusercontent.com\$" "${dir}/log" \
    || fail "(${form}) verify-blob did not pin the exact identity + OIDC issuer: $(grep verify-blob "${dir}/log")"
  ! grep -q -- '-regexp' "${dir}/log" || fail "(${form}) a -regexp identity form still reaches cosign: $(grep -- -regexp "${dir}/log" | head -1)"
  case "${form}" in
    bundle) grep -q '^cosign verify-blob .*--bundle' "${dir}/log" || fail "the bundle branch did not run verify-blob --bundle" ;;
    sigpem) grep -q '^cosign verify-blob .*--signature' "${dir}/log" || fail "the sig+pem branch did not run verify-blob --signature" ;;
  esac

  # 2. another tag's signature is refused, for the images and for the blob.
  run 0.8.3 v0.7.0 "${form}"
  [ "${rc}" != 0 ] || fail "V=0.8.3 signed by v0.7.0 (${form}) exited 0: the requested tag plays no part in the identity check"
  nfail="$(printf '%s' "${out}" | grep -c '^FAIL .* cosign verify$' || true)"
  [ "${nfail}" = "${nimg}" ] || fail "(${form}) ${nfail} of ${nimg} images were refused for the wrong signer tag"
  printf '%s' "${out}" | grep -q '^FAIL verify-blob' || fail "(${form}) the checksum blob was accepted from the wrong signer tag"
  printf '%s' "${out}" | grep -q '^fails=[1-9]' || fail "(${form}) fails was not > 0 for the wrong signer tag"
done
pass "verify-release binds the signer to the requested tag (images, bundle blob, sig+pem blob)"

# 3. a prerelease version is a version too; its tag is exact as well.
run 0.8.5-rc.1 v0.8.5-rc.1 bundle
[ "${rc}" = 0 ] || fail "V=0.8.5-rc.1 signed by v0.8.5-rc.1 exited ${rc}: $(printf '%s' "${out}" | tail -3)"
run 0.8.5-rc.1 v0.8.5 bundle
[ "${rc}" != 0 ] || fail "V=0.8.5-rc.1 accepted a v0.8.5 signature"
pass "a prerelease version is bound to its own tag"

# 4. a malformed V exits 2 BEFORE any network call, and never reaches a regexp.
for bad in '0.8.3.*' '0.8' 'v0.8.3' '0.8.3 ' '0.8.3/../x' '0.8.3;id' '..*'; do
  : > "${dir}/log"; rc=0
  PATH="${bin}:${PATH}" LOG="${dir}/log" DIR="${dir}" VER=x SIGNER_TAG=v0.8.3 SIGFORM=bundle "${VERIFY}" "${bad}" >/dev/null 2>&1 || rc=$?
  [ "${rc}" = 2 ] || fail "V='${bad}' exited ${rc}, want 2"
  [ ! -s "${dir}/log" ] || fail "V='${bad}' reached the network before it was refused: $(head -1 "${dir}/log")"
done
rc=0; PATH="${bin}:${PATH}" LOG="${dir}/log" "${VERIFY}" '' >/dev/null 2>&1 || rc=$?
[ "${rc}" = 2 ] || fail "an empty version exited ${rc}, want 2"
pass "a malformed version is refused with exit 2 before any network call"

# 5. read-only: nothing mutating was ever attempted (the stubs exit 99 on one).
run 0.8.3 v0.8.3 bundle
! grep -qE '^(gh (release (create|edit|delete|upload)|api)|docker (push|tag|rmi|login))' "${dir}/log" || fail "verify-release attempted a mutating command: $(grep -E '^(gh|docker)' "${dir}/log")"
pass "verify-release stays read-only"
echo "test-verify-release: PASS"
