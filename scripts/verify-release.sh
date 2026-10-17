#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Post-release verification (RELEASING step 6), READ-ONLY against the published release:
#   V=X.Y.Z scripts/verify-release.sh      (or: scripts/verify-release.sh X.Y.Z)
# Every release image is cosign-verified BY INDEX DIGEST against the release workflow run for THIS tag (an exact
# certificate identity, so another tag's signature is refused), the asset NAMES are compared with the expected set,
# the SHA256SUMS blob is verified, the install.sh URL is fetched and the helm chart is shown. It never creates,
# edits, deletes or retags a release or image.
# The image list is derived from .github/workflows/release.yml (every `- name: <image>` matrix entry); the
# expected assets are one sbom-<image>.cdx.json per image, every name on the release-assets job's `for want in`
# list, and wardyn-$V.tgz. A missing name AND an extra name are each a failure.
# WSL law: an empty DOCKER_CONFIG so the desktop credential helper cannot break anonymous pulls of public packages.
# Exit status = the number of failures (capped at 255).
set -uo pipefail
V=${1:-${V:-}}; [ -n "$V" ] || { echo "usage: V=X.Y.Z $0   (or $0 X.Y.Z)" >&2; exit 2; }
# $V names the tag the signer is bound to below, so it must be a plain version before anything
# runs: no network call, no interpolation of an arbitrary string into an identity.
[[ "$V" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || { echo "not a release version (X.Y.Z or X.Y.Z-suffix): $V" >&2; exit 2; }
TAG=v$V
RELEASE_YML="$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/release.yml"
[ -r "$RELEASE_YML" ] || { echo "cannot read $RELEASE_YML" >&2; exit 2; }
IMAGES=$(grep -oE '^[[:space:]]+- name: [a-z0-9-]+$' "$RELEASE_YML" | awk '{print $3}' | sort -u)
WANT=$(sed -n '/for want in/,/; do/p' "$RELEASE_YML" | sed -e 's/.*for want in//' -e 's/; do.*//' -e 's/\\$//' | tr -s ' \n' '\n\n' | grep .)
[ -n "$IMAGES" ] && [ -n "$WANT" ] || { echo "could not derive the image or asset list from $RELEASE_YML" >&2; exit 2; }
OUT=$(mktemp -d)
export DOCKER_CONFIG="$OUT/dockercfg"; mkdir -p "$DOCKER_CONFIG"; echo '{}' > "$DOCKER_CONFIG/config.json"
fails=0
# The EXACT identity of the release workflow run for the requested tag: an image or checksum file signed
# by any other tag's run is refused. (A promotion adds its own signature at the target tag: RELEASING.md.)
ID="https://github.com/cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/$TAG"
ISS=https://token.actions.githubusercontent.com
for img in $IMAGES; do
  ref="ghcr.io/cjohnstoniv/$img:$V"
  digest=$(docker buildx imagetools inspect "$ref" --format '{{json .Manifest.Digest}}' 2>/dev/null | tr -d '"')
  plats=$(docker buildx imagetools inspect "$ref" --format '{{range .Manifest.Manifests}}{{.Platform.OS}}/{{.Platform.Architecture}} {{end}}' 2>/dev/null)
  if [[ -z "$digest" ]]; then echo "FAIL $img: no index digest"; fails=$((fails+1)); continue; fi
  if cosign verify "ghcr.io/cjohnstoniv/$img@$digest" --certificate-identity "$ID" --certificate-oidc-issuer "$ISS" >"$OUT/cosign-$img.json" 2>"$OUT/cosign-$img.err"; then
    echo "OK   $img@$digest  [$plats]"
  else echo "FAIL $img cosign verify"; tail -3 "$OUT/cosign-$img.err"; fails=$((fails+1)); fi
done
echo "--- assets"
gh release view "$TAG" --repo cjohnstoniv/wardyn --json isPrerelease,isDraft,assets --jq '"prerelease=\(.isPrerelease) draft=\(.isDraft) assets=\(.assets|length)", (.assets[].name)' | tee "$OUT/assets.txt"
expected=$({ for img in $IMAGES; do echo "sbom-$img.cdx.json"; done; echo "$WANT"; echo "wardyn-$V.tgz"; } | sort -u)
have=$(tail -n +2 "$OUT/assets.txt" | sort -u)
for name in $(comm -23 <(echo "$expected") <(echo "$have")); do echo "FAIL missing asset: $name"; fails=$((fails+1)); done
for name in $(comm -13 <(echo "$expected") <(echo "$have")); do echo "FAIL unexpected asset: $name"; fails=$((fails+1)); done
[[ "$expected" == "$have" ]] && echo "OK   asset names match the expected set ($(echo "$expected" | wc -l))"
echo "--- SHA256SUMS"
rm -rf "$OUT/dl"; mkdir -p "$OUT/dl"
gh release download "$TAG" --repo cjohnstoniv/wardyn --dir "$OUT/dl" --pattern 'SHA256SUMS*' --pattern 'install.sh' --pattern 'wardyn-linux-amd64' >/dev/null 2>&1
ls "$OUT/dl"
if [[ -f "$OUT/dl/SHA256SUMS" ]]; then
  bundle=$(ls "$OUT/dl"/SHA256SUMS.* 2>/dev/null | head -5 | tr '\n' ' ')
  echo "signature material: $bundle"
  if [[ -f "$OUT/dl/SHA256SUMS.bundle" ]]; then
    cosign verify-blob "$OUT/dl/SHA256SUMS" --bundle "$OUT/dl/SHA256SUMS.bundle" --certificate-identity "$ID" --certificate-oidc-issuer "$ISS" && echo "OK   SHA256SUMS verify-blob (bundle)" || { echo "FAIL verify-blob"; fails=$((fails+1)); }
  elif [[ -f "$OUT/dl/SHA256SUMS.sig" && -f "$OUT/dl/SHA256SUMS.pem" ]]; then
    cosign verify-blob "$OUT/dl/SHA256SUMS" --signature "$OUT/dl/SHA256SUMS.sig" --certificate "$OUT/dl/SHA256SUMS.pem" --certificate-identity "$ID" --certificate-oidc-issuer "$ISS" && echo "OK   SHA256SUMS verify-blob (sig+pem)" || { echo "FAIL verify-blob"; fails=$((fails+1)); }
  else echo "FAIL no signature material for SHA256SUMS"; fails=$((fails+1)); fi
  (cd "$OUT/dl" && sha256sum --ignore-missing -c SHA256SUMS) || { echo "FAIL checksum"; fails=$((fails+1)); }
else echo "FAIL SHA256SUMS missing"; fails=$((fails+1)); fi
echo "--- install.sh URL"
code=$(curl -s -o /dev/null -w '%{http_code}' "https://github.com/cjohnstoniv/wardyn/releases/download/$TAG/install.sh"); echo "install.sh -> HTTP $code"; [[ "$code" == 302 || "$code" == 200 ]] || fails=$((fails+1))
grep -c "releases/download/$TAG/install.sh" "$OUT/dl/install.sh" 2>/dev/null | sed 's/^/install.sh self-pin count: /'
echo "--- helm chart"
helm show chart oci://ghcr.io/cjohnstoniv/charts/wardyn --version "$V" 2>&1 | grep -E '^(version|appVersion|name):' || { echo "FAIL helm chart"; fails=$((fails+1)); }
echo "fails=$fails"
if [[ $fails -eq 0 ]]; then rm -rf "$OUT"; else echo "evidence kept in $OUT"; fi
exit $(( fails > 255 ? 255 : fails ))
