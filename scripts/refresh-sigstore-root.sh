#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Refresh internal/releasesig/trusted_root.json, the Sigstore public-good trust root that
# `wardyn runner install|upgrade` verifies a release's SHA256SUMS bundle against offline.
# cosign fetches it through TUF (its embedded TUF root checks the chain), in a container,
# at the cosign version release.yml signs with. Commit the diff; the next CLI release carries it.
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/lib/common.sh

COSIGN_IMAGE="gcr.io/projectsigstore/cosign:v2.6.4"
home=$(mktemp -d); trap 'rm -rf "$home"' EXIT
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/h -v "$home:/h" "$COSIGN_IMAGE" initialize >/dev/null
src="$home/.sigstore/root/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json"
[ -s "$src" ] || die "cosign initialize left no trusted_root.json"
cp "$src" internal/releasesig/trusted_root.json
log "refreshed internal/releasesig/trusted_root.json (sha256 $(sha256sum "$src" | cut -c1-64))"
