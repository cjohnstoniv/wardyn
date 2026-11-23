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
grep -qx 'install-cli.sh' <<<"${WANT}" && grep -qx 'install-cli.ps1' <<<"${WANT}" || fail "the CLI installers are not on release.yml's required-asset list, so verify-release would not check them"
grep -qx 'wardyn-runnerd.pkg' <<<"${WANT}" || fail "the macOS runner package is not on release.yml's required-asset list, so verify-release would not check it"
printf '%s\n' "${WANT}" > "${dir}/want.txt"; printf '%s\n' "${IMAGES}" > "${dir}/images.txt"
# The version each image first shipped in, kept literal here on purpose so that a change to verify-release.sh's table fails this test.
since_of() { case "$1" in wardynd-fips) echo 0.8.6 ;; wardyn-runnerd) echo 0.9.3 ;; agent-base) echo 0.6.2 ;;
  agent-vscode|agent-novnc) echo 0.8.0 ;; agent-novnc-signin) echo 0.10.0 ;; *) echo 0.0.0 ;; esac; }

cat > "${bin}/cosign" <<'STUB'
#!/usr/bin/env bash
echo "cosign $*" >> "${LOG}"
[ -z "${CHART_UNSIGNED:-}" ] || [[ "$*" != */charts/* ]] || { echo "no matching signatures" >&2; exit 1; }
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
  *"buildx imagetools inspect"*) [ -z "${IMG_ERR:-}" ] || { echo "${IMG_ERR}" >&2; exit 1; } ;;&
  *"buildx imagetools inspect"*":latest --format"*"Manifest.Digest"*) echo "\"${LATEST_DIGEST:-sha256:0000000000000000000000000000000000000000000000000000000000000001}\"" ;;
  *"buildx imagetools inspect"*"Manifest.Digest"*) echo '"sha256:0000000000000000000000000000000000000000000000000000000000000001"' ;;
  # A tag repointed after its digest was read: a platform list asked of the tag (not of a digest) is the replacement's.
  *"buildx imagetools inspect"*) if [ -n "${REPOINT:-}" ] && [[ "$*" != *"@sha256:"* ]]; then echo "linux/amd64 linux/arm64 linux/s390x "; else echo "linux/amd64 linux/arm64 "; fi ;;
  *) echo "docker: mutating or unexpected verb: $*" >&2; exit 99 ;;
esac
STUB
cat > "${bin}/gh" <<'STUB'
#!/usr/bin/env bash
echo "gh $*" >> "${LOG}"
case "$1 $2" in
  "release view")
    pre=false; case "${VER}" in *-*) pre=true ;; esac
    echo "prerelease=${RELEASE_PRERELEASE-${pre}} draft=${RELEASE_DRAFT-false} assets=0 title=\"$(t=${RELEASE_TITLE-Wardyn v${VER}}; printf '%s' "${t//$'\n'/\\n}")\""
    # A historical fixture lists exactly the names of the file HISTORY points at.
    if [ -n "${HISTORY:-}" ]; then cat "${HISTORY}"; exit 0; fi
    while read -r i; do echo "sbom-$i.cdx.json"; done < "${DIR}/images-ver.txt"
    # The package exists from 0.9.3 (and a test can leave it out).
    # The licence tarball exists from 0.9.4.
    if [ "$(printf '%s\n' "${VER%%-*}" 0.9.4 | sort -V | head -1)" = 0.9.4 ]; then cat "${DIR}/want.txt"; else grep -vx 'wardyn-licenses.tar.gz' "${DIR}/want.txt"; fi > "${DIR}/want-ver0.txt"
    # The CLI installers exist from 0.10.0 (and a test can leave them out).
    if [ -n "${NO_INSTALL_CLI:-}" ] || [ "$(printf '%s\n' "${VER%%-*}" 0.10.0 | sort -V | head -1)" != 0.10.0 ]; then grep -vE '^(install-cli\.|SHA256SUMS\.sigstore\.json$|wardyn-windows-)' "${DIR}/want-ver0.txt"; elif [ -n "${NO_WINDOWS:-}" ]; then grep -v '^wardyn-windows-' "${DIR}/want-ver0.txt"; else cat "${DIR}/want-ver0.txt"; fi > "${DIR}/want-ver.txt"
    if [ -n "${NO_PKG:-}" ] || [ "$(printf '%s\n' "${VER%%-*}" 0.9.3 | sort -V | head -1)" != 0.9.3 ] || [ "${VER}" = 0.9.3-rc.1 ] || [ "${VER}" = 0.9.3-rc.2 ]; then grep -vx 'wardyn-runnerd.pkg' "${DIR}/want-ver.txt"; else cat "${DIR}/want-ver.txt"; fi
    echo "wardyn-${VER}.tgz" ;;
  "release list")
    if [ -n "${RELEASES+x}" ]; then printf '%s\n' "${RELEASES}"
    else case "${VER}" in *-*) echo "v${VER} false false true"; echo "v0.9.9 true false false" ;; *) echo "v${VER} true false false" ;; esac; fi ;;
  "release download")
    pats=()
    while [ $# -gt 0 ]; do [ "$1" = "--dir" ] && out="$2"; [ "$1" = "--pattern" ] && pats+=("$2"); shift; done
    # Every asset the release lists (the signature material is written below), each with bytes of its own.
    while read -r n; do
      case "${n}" in SHA256SUMS*) continue ;; esac
      echo "bytes of ${n}" > "${out}/${n}"
    done < <("$0" release view x | tail -n +2)
    echo "releases/download/v${VER}/install.sh" > "${out}/install.sh"
    ( cd "${out}" && sha256sum -- * > SHA256SUMS )
    # A published asset that differs from the listed digest, and one the list does not name.
    # A partial download: OMIT is on the release and not in SHA256SUMS, and was not downloaded.
    [ -z "${OMIT:-}" ] || { rm -f "${out}/${OMIT}"; ( cd "${out}" && sha256sum -- * > SHA256SUMS ); }
    [ -z "${DL_FAIL:-}" ] || { echo "HTTP 502" >&2; exit 1; }
    [ -z "${TAMPER:-}" ] || echo "swapped bytes" > "${out}/${TAMPER}"
    if [ -n "${UNLISTED:-}" ]; then grep -v " ${UNLISTED}\$" "${out}/SHA256SUMS" > "${out}/.sums"; mv "${out}/.sums" "${out}/SHA256SUMS"; fi
    if [ "${SIGFORM}" = bundle ]; then echo b > "${out}/SHA256SUMS.sigstore.json"
    else echo s > "${out}/SHA256SUMS.sig"; echo p > "${out}/SHA256SUMS.pem"; fi
    # Like gh: with --pattern, only the files that match one are downloaded.
    if [ "${#pats[@]}" -gt 0 ]; then
      for f in "${out}"/*; do
        keep=""; for pt in "${pats[@]}"; do case "$(basename "${f}")" in ${pt}) keep=1 ;; esac; done
        [ -n "${keep}" ] || rm -f "${f}"
      done
    fi ;;
  *) echo "gh: mutating or unexpected verb: $*" >&2; exit 99 ;;
esac
STUB
cat > "${bin}/helm" <<'STUB'
#!/usr/bin/env bash
echo "helm $*" >> "${LOG}"
printf 'name: wardyn\nversion: %s\nappVersion: %s\n' "${VER}" "${APP_VER:-${VER}}"
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
  # The images this version shipped: each image's first version is at or below it (wardyn-runnerd also missed 0.9.3-rc.1 and rc.2).
  while read -r i; do
    [ "$(printf '%s\n' "${1%%-*}" "$(since_of "${i}")" | sort -V | head -1)" = "$(since_of "${i}")" ] || continue
    case "${i}:$1" in wardyn-runnerd:0.9.3-rc.1|wardyn-runnerd:0.9.3-rc.2) continue ;; esac
    echo "${i}"
  done < "${dir}/images.txt" > "${dir}/images-ver.txt"
  out="$(PATH="${bin}:${PATH}" REPOINT="${REPOINT:-}" NO_PKG="${NO_PKG:-}" NO_INSTALL_CLI="${NO_INSTALL_CLI:-}" CHART_UNSIGNED="${CHART_UNSIGNED:-}" OMIT="${OMIT:-}" DL_FAIL="${DL_FAIL:-}" TAMPER="${TAMPER:-}" UNLISTED="${UNLISTED:-}" RELEASE_TITLE="${RELEASE_TITLE-Wardyn v$1}" APP_VER="${APP_VER:-}" IMG_ERR="${IMG_ERR:-}" LOG="${dir}/log" DIR="${dir}" VER="$1" SIGNER_TAG="$2" SIGFORM="$3" "${VERIFY}" "$1" 2>&1)" || rc=$?
}

for form in bundle sigpem; do
  # 1. the signer IS the requested tag: passes, exactly, for every image and the blob.
  run 0.8.3 v0.8.3 "${form}"
  [ "${rc}" = 0 ] || fail "V=0.8.3 signed by v0.8.3 (${form}) exited ${rc}, want 0: $(printf '%s' "${out}" | tail -5)"
  printf '%s' "${out}" | grep '^fails=0$' >/dev/null || fail "V=0.8.3 signed by v0.8.3 (${form}) did not report fails=0"
  id='https://github.com/cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/v0.8.3'
  nimg="$(wc -l < "${dir}/images-ver.txt")"
  nverify="$(grep -c "^cosign verify .*--certificate-identity ${id} --certificate-oidc-issuer https://token.actions.githubusercontent.com\$" "${dir}/log" || true)"
  # one per image, plus the Helm chart (docs/VERIFY.md section 4), by digest and under the same exact identity
  [ "${nverify}" = "$((nimg + 1))" ] || fail "(${form}) ${nverify} of $((nimg + 1)) 'cosign verify' calls carried the exact identity + OIDC issuer: $(grep '^cosign' "${dir}/log" | head -3)"
  grep -q "^cosign verify ghcr.io/cjohnstoniv/charts/wardyn@sha256:0*1 --certificate-identity ${id} --certificate-oidc-issuer https://token.actions.githubusercontent.com\$" "${dir}/log" \
    || fail "(${form}) the Helm chart was not cosign-verified by digest with the exact identity: $(grep '/charts/' "${dir}/log" | head -3)"
  grep -q "^cosign verify-blob .*--certificate-identity ${id} --certificate-oidc-issuer https://token.actions.githubusercontent.com\$" "${dir}/log" \
    || fail "(${form}) verify-blob did not pin the exact identity + OIDC issuer: $(grep verify-blob "${dir}/log")"
  ! grep -q -- '-regexp' "${dir}/log" || fail "(${form}) a -regexp identity form still reaches cosign: $(grep -- -regexp "${dir}/log" | head -1)"
  case "${form}" in
    bundle) grep -q '^cosign verify-blob .*--bundle .*/SHA256SUMS\.sigstore\.json ' "${dir}/log" || fail "the bundle branch did not run verify-blob --bundle on SHA256SUMS.sigstore.json" ;;
    sigpem) grep -q '^cosign verify-blob .*--signature' "${dir}/log" || fail "the sig+pem branch did not run verify-blob --signature" ;;
  esac

  # 2. another tag's signature is refused, for the images and for the blob.
  run 0.8.3 v0.7.0 "${form}"
  [ "${rc}" != 0 ] || fail "V=0.8.3 signed by v0.7.0 (${form}) exited 0: the requested tag plays no part in the identity check"
  nfail="$(printf '%s' "${out}" | grep -c '^FAIL .* cosign verify$' || true)"
  [ "${nfail}" = "$((nimg + 1))" ] || fail "(${form}) ${nfail} of $((nimg + 1)) images and charts were refused for the wrong signer tag"
  printf '%s' "${out}" | grep '^FAIL helm chart cosign verify$' >/dev/null || fail "(${form}) the chart was accepted from the wrong signer tag"
  printf '%s' "${out}" | grep '^FAIL verify-blob' >/dev/null || fail "(${form}) the checksum blob was accepted from the wrong signer tag"
  printf '%s' "${out}" | grep '^fails=[1-9]' >/dev/null || fail "(${form}) fails was not > 0 for the wrong signer tag"
done
pass "verify-release binds the signer to the requested tag (images, bundle blob, sig+pem blob)"

# 2b. every asset is checksummed: a swapped binary, or an asset SHA256SUMS does not list, fails the run.
for tamper in wardyn-darwin-arm64 wardyn-runnerd.pkg; do
  TAMPER="${tamper}" run 0.10.0 v0.10.0 bundle
  [ "${rc}" != 0 ] || fail "a swapped ${tamper} still exited 0"
  grep -q "^${tamper}: FAILED" <<<"${out}" || fail "a swapped ${tamper} was not named as a checksum failure: $(printf '%s' "${out}" | grep -i 'checksum\|FAILED' | head -3)"
  grep -q '^FAIL checksum$' <<<"${out}" || fail "a swapped ${tamper} did not print FAIL checksum"
done
UNLISTED=wardyn-linux-arm64 run 0.10.0 v0.10.0 bundle
[ "${rc}" != 0 ] || fail "an asset missing from SHA256SUMS still exited 0"
grep -q 'wardyn-linux-arm64 is a release asset that SHA256SUMS does not list' <<<"${out}" || fail "the unlisted asset was not named"
OMIT=wardyn-darwin-arm64 run 0.10.0 v0.10.0 bundle
[ "${rc}" != 0 ] || fail "an asset left out of SHA256SUMS and of the download still exited 0"
grep -q 'wardyn-darwin-arm64 is a release asset that SHA256SUMS does not list' <<<"${out}" || fail "the omitted asset was not named: $(grep FAIL <<<"${out}" | head -3)"
DL_FAIL=1 run 0.10.0 v0.10.0 bundle
[ "${rc}" != 0 ] || fail "a failed release download still exited 0"
grep -q '^FAIL release download' <<<"${out}" || fail "a failed download was not reported"
run 0.10.0 v0.10.0 bundle
[ "${rc}" = 0 ] || fail "an untouched 0.10.0 release exited ${rc}: $(printf '%s' "${out}" | tail -5)"
! grep -q -- '--ignore-missing' "${VERIFY}" || fail "verify-release still passes --ignore-missing to sha256sum"
pass "verify-release checksums every asset and refuses a swapped or unlisted one"

# 3. a prerelease version is a version too; its tag is exact as well.
run 0.8.5-rc.1 v0.8.5-rc.1 bundle
[ "${rc}" = 0 ] || fail "V=0.8.5-rc.1 signed by v0.8.5-rc.1 exited ${rc}: $(printf '%s' "${out}" | tail -3)"
run 0.8.5-rc.1 v0.8.5 bundle
[ "${rc}" != 0 ] || fail "V=0.8.5-rc.1 accepted a v0.8.5 signature"
pass "a prerelease version is bound to its own tag"

# 3b. the macOS package is required from 0.9.3 and not before.
# 0.9.3-rc.1 and rc.2 were built before the package job existed: no package is expected of them.
run 0.9.3-rc.1 v0.9.3-rc.1 bundle
[ "${rc}" = 0 ] || fail "V=0.9.3-rc.1 (no package in that prerelease) exited ${rc}: $(printf '%s' "${out}" | tail -3)"
run 0.9.3-rc.2 v0.9.3-rc.2 bundle
[ "${rc}" = 0 ] || fail "V=0.9.3-rc.2 (no package in that prerelease) exited ${rc}: $(printf '%s' "${out}" | tail -3)"
run 0.9.3-rc.3 v0.9.3-rc.3 bundle
[ "${rc}" = 0 ] || fail "V=0.9.3-rc.3 with the package exited ${rc}: $(printf '%s' "${out}" | tail -3)"
NO_PKG=1 run 0.9.3-rc.3 v0.9.3-rc.3 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep 'FAIL missing asset: wardyn-runnerd.pkg' >/dev/null || fail "V=0.9.3-rc.3 without wardyn-runnerd.pkg was accepted"
NO_PKG=1 run 0.9.3 v0.9.3 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep 'FAIL missing asset: wardyn-runnerd.pkg' >/dev/null || fail "V=0.9.3 without wardyn-runnerd.pkg was accepted"
run 0.9.2 v0.9.2 bundle
[ "${rc}" = 0 ] || fail "V=0.9.2 (no package in that release) exited ${rc}: $(printf '%s' "${out}" | tail -3)"
pass "wardyn-runnerd.pkg is required from 0.9.3 on"
run 0.9.4 v0.9.4 bundle
[ "${rc}" = 0 ] || fail "V=0.9.4 with the licence tarball exited ${rc}: $(printf '%s' "${out}" | tail -3)"
pass "wardyn-licenses.tar.gz is required from 0.9.4 on, and not before"

# 3b2. the CLI installers are required from 0.10.0 and not before.
run 0.10.0 v0.10.0 bundle
[ "${rc}" = 0 ] || fail "V=0.10.0 with the CLI installers exited ${rc}: $(printf '%s' "${out}" | tail -3)"
run 0.10.0-rc.1 v0.10.0-rc.1 bundle
[ "${rc}" = 0 ] || fail "V=0.10.0-rc.1 with the CLI installers exited ${rc}: $(printf '%s' "${out}" | tail -3)"
NO_INSTALL_CLI=1 run 0.10.0 v0.10.0 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep 'FAIL missing asset: install-cli.sh' >/dev/null && printf '%s' "${out}" | grep 'FAIL missing asset: install-cli.ps1' >/dev/null \
  || fail "V=0.10.0 without install-cli.sh and install-cli.ps1 was accepted"
run 0.9.5 v0.9.5 bundle
[ "${rc}" = 0 ] || fail "V=0.9.5 (no CLI installers in that release) exited ${rc}: $(printf '%s' "${out}" | tail -3)"
NO_WINDOWS=1 run 0.10.0 v0.10.0 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep 'FAIL missing asset: wardyn-windows-amd64.exe' >/dev/null && printf '%s' "${out}" | grep 'FAIL missing asset: wardyn-windows-arm64.exe' >/dev/null \
  || fail "V=0.10.0 without the Windows CLI binaries was accepted"
pass "install-cli.sh, install-cli.ps1 and the Windows CLI binaries are required from 0.10.0 on, and not before"

# 3b2b. an older release is held to what it shipped: v0.9.5's real asset names, as its tagged release workflow defines them
# (no agent-novnc-signin image or SBOM, no Windows binary, no install-cli script, no Sigstore bundle).
hist="${dir}/hist-0.9.5.txt"
{ printf '%s\n' SHA256SUMS SHA256SUMS.sig SHA256SUMS.pem THIRD-PARTY-NOTICES.md LICENSE NOTICE install.sh \
    wardyn-linux-amd64 wardyn-linux-arm64 wardyn-darwin-amd64 wardyn-darwin-arm64 \
    wardyn-runnerd-linux-amd64 wardyn-runnerd-linux-arm64 wardyn-runnerd-darwin-amd64 wardyn-runnerd-darwin-arm64 \
    wardyn-runner-units.tar.gz wardyn-runnerd.pkg wardyn-licenses.tar.gz wardyn-0.9.5.tgz
  for i in agent-aws-sso agent-base agent-codex-cli agent-novnc agent-vscode wardyn-proxy wardyn-runnerd wardynd wardynd-fips; do echo "sbom-${i}.cdx.json"; done; } > "${hist}"
HISTORY="${hist}" run 0.9.5 v0.9.5 sigpem
[ "${rc}" = 0 ] && printf '%s' "${out}" | grep -Fx 'OK   asset names match the expected set (28)' >/dev/null \
  || fail "H1 v0.9.5's own asset list was refused: rc=${rc} $(printf '%s' "${out}" | grep -E 'FAIL|missing|unexpected' | head -4)"
{ cat "${hist}"; echo wardyn-windows-amd64.exe; } > "${dir}/hist-0.9.5-win.txt"
HISTORY="${dir}/hist-0.9.5-win.txt" run 0.9.5 v0.9.5 sigpem
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep -Fx 'FAIL unexpected asset: wardyn-windows-amd64.exe' >/dev/null \
  || fail "H2 a Windows binary in v0.9.5 was accepted: rc=${rc}"
pass "an older release is checked against the images and files it shipped (v0.9.5's own list passes; a Windows binary in it fails)"

# 3b3. the Release title is "Wardyn v<version>" from 0.10.0 on, and not checked before.
run 0.10.0-rc.14 v0.10.0-rc.14 bundle
printf '%s' "${out}" | grep '^OK   release title$' >/dev/null || fail "V=0.10.0-rc.14 with its title did not report 'OK   release title': $(printf '%s' "${out}" | grep -i title)"
RELEASE_TITLE='v0.10.0-rc.14' run 0.10.0-rc.14 v0.10.0-rc.14 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep '^FAIL release title: "v0.10.0-rc.14" (want "Wardyn v0.10.0-rc.14")$' >/dev/null || fail "V=0.10.0-rc.14 with a wrong title was accepted: rc=${rc} $(printf '%s' "${out}" | grep -i title)"
RELEASE_TITLE='' run 0.10.0 v0.10.0 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep '^FAIL release title: "" (want "Wardyn v0.10.0")$' >/dev/null || fail "V=0.10.0 with an empty title was accepted: rc=${rc}"
# A title with a line break must not pass, nor stand in for an asset that is missing.
NO_PKG=1 RELEASE_TITLE="Wardyn v0.10.0"$'\n'"wardyn-runnerd.pkg" run 0.10.0 v0.10.0 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep '^FAIL release title: ' >/dev/null && printf '%s' "${out}" | grep 'FAIL missing asset: wardyn-runnerd.pkg' >/dev/null \
  || fail "V=0.10.0 with a two-line title naming a missing asset was accepted: rc=${rc} $(printf '%s' "${out}" | grep -iE 'title|missing')"
RELEASE_TITLE='0.9.5' run 0.9.5 v0.9.5 bundle
[ "${rc}" = 0 ] || fail "V=0.9.5 (cut before titles were held) exited ${rc} over its title: $(printf '%s' "${out}" | tail -3)"
pass "the release title is required from 0.10.0 on, and not before"

# 3b4. from 0.10.0 on, a final release is published and not a prerelease, and an RC is a prerelease; a draft never passes.
run 0.10.0 v0.10.0 bundle
printf '%s' "${out}" | grep '^OK   release state (prerelease=false draft=false)$' >/dev/null || fail "V=0.10.0 published as a final did not report its state: $(printf '%s' "${out}" | grep -i 'release state')"
RELEASE_PRERELEASE=true run 0.10.0 v0.10.0 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep '^FAIL release state: prerelease=true draft=false (want prerelease=false draft=false)$' >/dev/null || fail "V=0.10.0 left as a prerelease was accepted: rc=${rc}"
RELEASE_PRERELEASE=false run 0.10.0-rc.14 v0.10.0-rc.14 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep '^FAIL release state: prerelease=false draft=false (want prerelease=true draft=false)$' >/dev/null || fail "V=0.10.0-rc.14 published as a final was accepted: rc=${rc}"
RELEASE_DRAFT=true run 0.10.0 v0.10.0 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep '^FAIL release state: prerelease=false draft=true ' >/dev/null || fail "V=0.10.0 still a draft was accepted: rc=${rc}"
# Latest names the highest published final across every line (lines are: tag isLatest isDraft isPrerelease).
latest_case() { # NAME RELEASES VERSION WANT_RC LINE
  RELEASES="$2" run "$3" "v$3" bundle
  if [ "$4" = 0 ]; then [ "${rc}" = 0 ] || fail "latest $1: exited ${rc}: $(printf '%s' "${out}" | grep -i latest)"
  else [ "${rc}" != 0 ] || fail "latest $1: was accepted"; fi
  printf '%s' "${out}" | grep -Fx -- "$5" >/dev/null || fail "latest $1: no line [$5]: $(printf '%s' "${out}" | grep -i latest)"
}
latest_case "L1 label left behind" $'v0.10.0 false false false\nv0.9.9 true false false' 0.10.0 1 'FAIL latest release: v0.9.9 (want v0.10.0, the highest published final)'
latest_case "L2 no Latest" 'v0.10.0 false false false' 0.10.0 1 'FAIL latest release: none (want v0.10.0, the highest published final)'
latest_case "L3 a newer final is Latest" $'v0.10.1 true false false\nv0.10.0 false false false' 0.10.0 0 'OK   latest release (v0.10.1)'
latest_case "L4 an older-line final" $'v0.11.0 true false false\nv0.10.9 false false false' 0.10.9 0 'OK   latest release (v0.11.0)'
latest_case "L5 label moved back" $'v0.11.0 false false false\nv0.10.9 true false false' 0.10.9 1 'FAIL latest release: v0.10.9 (want v0.11.0, the highest published final)'
latest_case "L6 a draft and an RC are not the bar" $'v0.11.0 false true false\nv0.11.1-rc.1 false false true\nv0.10.9 true false false' 0.10.9 0 'OK   latest release (v0.10.9)'
latest_case "L7 an RC is checked too" $'v0.11.0-rc.2 false false true\nv0.10.9 false false false\nv0.10.8 true false false' 0.11.0-rc.2 1 'FAIL latest release: v0.10.8 (want v0.10.9, the highest published final)'
# :latest on every floating image is the highest final's image.
LATEST_DIGEST="sha256:$(printf '0%.0s' $(seq 63))2" run 0.10.0 v0.10.0 bundle
d1="sha256:$(printf '0%.0s' $(seq 63))1"; d2="sha256:$(printf '0%.0s' $(seq 63))2"
[ "${rc}" != 0 ] || fail "F1 :latest on another digest was accepted"
for img in agent-base agent-codex-cli agent-aws-sso; do
  printf '%s' "${out}" | grep -Fx -- "FAIL ${img}:latest is ${d2}, not v0.10.0's image (${d1})" >/dev/null || fail "F1 no FAIL line for ${img}: $(printf '%s' "${out}" | grep ':latest')"
done
[ "$(printf '%s' "${out}" | grep -c '^FAIL .*:latest is ' || true)" = 3 ] || fail "F1 want exactly three :latest FAIL lines: $(printf '%s' "${out}" | grep ':latest')"
run 0.10.0 v0.10.0 bundle
[ "${rc}" = 0 ] || fail "F2 :latest in sync exited ${rc}"
for img in agent-base agent-codex-cli agent-aws-sso; do
  printf '%s' "${out}" | grep -Fx -- "OK   ${img}:latest is v0.10.0's image (${d1})" >/dev/null || fail "F2 no OK line for ${img}: $(printf '%s' "${out}" | grep ':latest')"
done
run 0.10.0 v0.10.0 bundle
printf '%s' "${out}" | grep '^OK   latest release (v0.10.0)$' >/dev/null || fail "V=0.10.0 as Latest did not report it: $(printf '%s' "${out}" | grep -i latest)"
pass "the Latest label and :latest on every floating image must name the highest published final"
RELEASE_PRERELEASE=true run 0.9.5 v0.9.5 bundle
[ "${rc}" = 0 ] || fail "V=0.9.5 (cut before the state was held) exited ${rc} over its state: $(printf '%s' "${out}" | tail -3)"
pass "a final is published and not a prerelease, and an RC is a prerelease, from 0.10.0 on"

# 3c. a failed imagetools call says why, instead of only "no index digest".
IMG_ERR='ERROR: docker: unknown command: docker buildx' run 0.8.3 v0.8.3 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep 'FAIL wardynd: no index digest' >/dev/null && printf '%s' "${out}" | grep 'unknown command: docker buildx' >/dev/null \
  || fail "a failed imagetools call did not surface its stderr: $(printf '%s' "${out}" | head -4)"
pass "an imagetools failure reports its cause"

# 3c2. the tag is resolved once: the platform list comes from the digest cosign verifies, not from the tag
# looked up again, which a repair may have repointed in between.
REPOINT=1 run 0.8.3 v0.8.3 bundle
nimg="$(wc -l < "${dir}/images-ver.txt")"
[ "${rc}" = 0 ] || fail "a repointed tag changed the exit code to ${rc}: $(printf '%s' "${out}" | grep FAIL | head -3)"
! printf '%s' "${out}" | grep 's390x' >/dev/null || fail "an OK line reports the platforms of a replacement index, not of the verified digest: $(printf '%s' "${out}" | grep s390x | head -1)"
[ "$(printf '%s' "${out}" | grep -c '^OK   .*@sha256:0*1  \[linux/amd64 linux/arm64 \]$' || true)" = "${nimg}" ] \
  || fail "not every image reported the verified digest's platforms: $(printf '%s' "${out}" | grep '^OK   .*@sha256' | head -3)"
[ "$(grep -c '^docker buildx imagetools inspect ghcr.io/cjohnstoniv/.*@sha256:0*1 --format {{range' "${dir}/log" || true)" = "${nimg}" ] \
  || fail "the platform list was not read from the verified digest: $(grep 'range' "${dir}/log" | head -3)"
pass "the platform list is read from the digest that is verified, not from the tag again"

# 3d. the chart is held to its own signature and to this release's appVersion (#2227).
CHART_UNSIGNED=1 run 0.8.3 v0.8.3 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep '^FAIL helm chart cosign verify$' >/dev/null && ! printf '%s' "${out}" | grep '^FAIL .*wardynd' >/dev/null \
  || fail "an unsigned chart (images signed) was not the one failure: rc=${rc} $(printf '%s' "${out}" | grep FAIL)"
APP_VER=0.8.2 run 0.8.3 v0.8.3 bundle
[ "${rc}" != 0 ] && printf '%s' "${out}" | grep "^FAIL helm chart appVersion '0.8.2' is not 0.8.3$" >/dev/null \
  || fail "a chart whose appVersion is another release was accepted: rc=${rc}"
run 0.8.3 v0.8.3 bundle
grep -q '^helm show chart oci://ghcr.io/cjohnstoniv/charts/wardyn@sha256:0*1$' "${dir}/log" \
  || fail "appVersion was not read from the cosign-verified digest: $(grep '^helm' "${dir}/log")"
pass "the Helm chart's signature and appVersion are verified"

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
