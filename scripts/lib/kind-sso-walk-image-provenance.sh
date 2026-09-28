#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# scripts/kind-sso-walk.sh's image-provenance report, split out to keep the
# walk itself under the 1000-line file-size gate (scripts/check-file-size.sh).
# Sourced, not exec'd: it reads and sets variables (NODE_IMAGES, IMAGES_AGREE)
# the walk uses afterward, and every WARDYN_*/image variable it reads
# (EVIDENCE_DIR, KIND_NODE, ROOT, WARDYND_IMAGE, PROXY_IMAGE, AGENT_IMAGE,
# AWS_SSO_IMAGE, FAKE_IMAGE, AWS_SSO_NODE_IMAGE, WARDYN_KIND_SSO_REBUILD,
# PROXY_INJECT) is already set by the walk before it sources this file.

step "recording the image provenance into ${EVIDENCE_DIR}/images.txt"
# ONE read of the node's store, reused for all five (a `docker exec` per image
# is five round trips for one question).
NODE_IMAGES="$(docker exec "${KIND_NODE}" ctr -n k8s.io images ls 2>/dev/null || true)"
node_digest() { # <repo:tag> -> the node's manifest digest, or ""
  printf '%s\n' "${NODE_IMAGES}" | awk -v r="docker.io/$1" '$1==r {print $3}' | head -1
}
# #891: aws-sso is pulled through the registry (step 1c), so on the node it is
# named exactly AWS_SSO_NODE_IMAGE — `ctr images ls` records a registry pull
# verbatim, with no docker.io/ prefix, unlike the four `kind load`ed images
# above.
node_digest_registry() { # <exact ref as ctr recorded it> -> the node's digest
  printf '%s\n' "${NODE_IMAGES}" | awk -v r="$1" '$1==r {print $3}' | head -1
}
# `.Id` equals `ctr`'s manifest digest only under the containerd image store
# (`docker info`'s Driver Type io.containerd.snapshotter.v1). On the classic
# (graphdriver) image store a pushed manifest digest is never the local image
# ID, so `agree` below reads "NO" on every walk regardless of whether the node
# actually holds what this daemon holds. That only degrades this record and
# the WARNING it can print — nothing in this script GATES on IMAGES_AGREE.
host_digest() { docker image inspect "$1" --format '{{.Id}}' 2>/dev/null; }
# The one indirection the loops below need: aws-sso's NODE-side lookup key is
# the registry ref, not the local build tag the "image" column still shows.
node_ref_for() {
  if [[ "$1" == "${AWS_SSO_IMAGE}" ]]; then
    node_digest_registry "${AWS_SSO_NODE_IMAGE}"
  else
    node_digest "$1"
  fi
}
IMAGES_AGREE=1
{
  echo "walk tree:       $(git -C "${ROOT}" rev-parse HEAD 2>/dev/null || echo '(not a git tree)')"
  # The PATHS, not a count (W6-I SHOULD-1): "dirty: 1 file(s)" names nothing a
  # reader can judge. Bounded, because a stray build artefact must not bury it.
  echo "walk tree dirty: $(git -C "${ROOT}" status --porcelain 2>/dev/null | wc -l) file(s)"
  git -C "${ROOT}" status --porcelain 2>/dev/null | head -20 | sed 's/^/                 /'
  echo "rebuilt:         ${WARDYN_KIND_SSO_REBUILD:-0}"
  echo "proxy inject:    ${PROXY_INJECT} (intended; read back off the deployment in MANIFEST.json)"
  echo
  printf '%-34s %-72s %-72s %s\n' "image" "host daemon" "node ${KIND_NODE}" "agree"
  for img in "${WARDYND_IMAGE}" "${PROXY_IMAGE}" "${AGENT_IMAGE}" "${AWS_SSO_IMAGE}" "${FAKE_IMAGE}"; do
    h="$(host_digest "${img}")"; n="$(node_ref_for "${img}")"
    a="NO"; [[ -n "${h}" && "${h}" == "${n}" ]] && a="yes"
    [[ "${a}" == "yes" ]] || IMAGES_AGREE=0
    printf '%-34s %-72s %-72s %s\n' "${img}" "${h:-(absent)}" "${n:-(absent)}" "${a}"
  done
  echo
  echo "created (host):"
  for img in "${WARDYND_IMAGE}" "${PROXY_IMAGE}" "${AGENT_IMAGE}" "${AWS_SSO_IMAGE}" "${FAKE_IMAGE}"; do
    printf '  %-34s %s\n' "${img}" "$(docker image inspect "${img}" --format '{{.Created}}' 2>/dev/null || echo '(not present locally)')"
  done
} | tee "${EVIDENCE_DIR}/images.txt"
if [[ "${IMAGES_AGREE}" != "1" ]]; then
  echo "" >&2
  echo "WARNING: an image the node runs is NOT the one this daemon holds (see ${EVIDENCE_DIR}/images.txt)." >&2
  echo "         Re-run with WARDYN_KIND_SSO_REBUILD=1, which rebuilds all five (aws-sso is then re-pushed to the registry on this same run, step 1c)." >&2
fi
