#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-gpl-source-offer.sh — a bootstrap (pre-publication) section of the GPL
# offer names the commit the local image was BUILT from, read from the image's
# org.opencontainers.image.revision label (stamped by the Makefile's image
# targets), and the script refuses an image built from a dirty tree or carrying
# no build commit at all (#357).
#
# Daemon-free, network-free: it runs the real script against a throwaway tree
# with hand-written syft-json fixtures.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/scripts" "$TMP/.github/workflows" "$TMP/deploy/images" "$TMP/sbom"
cp "$ROOT/scripts/gpl-source-offer.sh" "$TMP/scripts/"
printf 'jobs:\n  publish:\n    strategy:\n      matrix:\n        include:\n          - name: wardynd\n' \
  > "$TMP/.github/workflows/release.yml"
echo "## historical" > "$TMP/deploy/images/third-party-gpl-historical.md"
echo '{"source":{"type":"image","metadata":{}},"artifacts":[]}' > "$TMP/sbom/04-sbom-wardynd-v0.0.1-amd64.json"

fail() { echo "FAIL: $*" >&2; exit 1; }

# The script resolves its repo root from its own location, so running the COPY
# writes $TMP/deploy/images/THIRD-PARTY-GPL.md and nothing else.
write_local_sbom() { # LABELS-JSON
  printf '{"source":{"type":"image","metadata":{"labels":%s}},"artifacts":[]}\n' "$1" \
    > "$TMP/sbom/local-sbom-agent-novnc.json"
}
run_offer() { (cd "$TMP" && BOOTSTRAP_IMAGES=agent-novnc ./scripts/gpl-source-offer.sh sbom v0.0.1 2>&1); }

# 1. a clean build commit on the image => the note names THAT commit.
write_local_sbom '{"org.opencontainers.image.revision":"abc1234"}'
out="$(run_offer)" || fail "a bootstrap image labelled with a clean build commit must succeed. Got: $out"
grep -q 'built at commit' "$TMP/deploy/images/THIRD-PARTY-GPL.md" \
  && grep -q '`abc1234`' "$TMP/deploy/images/THIRD-PARTY-GPL.md" \
  || fail "the bootstrap note must name the image's build commit abc1234"
echo "ok  bootstrap note names the image's build commit"

# 2. built from a dirty tree => refuse, and leave the last offer untouched.
cp "$TMP/deploy/images/THIRD-PARTY-GPL.md" "$TMP/before.md"
write_local_sbom '{"org.opencontainers.image.revision":"abc1234-dirty"}'
if out="$(run_offer)"; then fail "an image built from a dirty tree must be REFUSED. Got: $out"; fi
case "$out" in *agent-novnc*dirty*) ;; *) fail "the refusal must name the image and the dirty build. Got: $out" ;; esac
cmp -s "$TMP/before.md" "$TMP/deploy/images/THIRD-PARTY-GPL.md" || fail "a refused run must not rewrite the offer"
echo "ok  dirty-tree build refused"

# 3. no revision label (a hand `docker build`, or a directory scan) => refuse
#    rather than name a commit nobody recorded.
write_local_sbom '{}'
if out="$(run_offer)"; then fail "an image with no build-commit label must be REFUSED. Got: $out"; fi
case "$out" in *agent-novnc*revision*) ;; *) fail "the refusal must name the image and the missing label. Got: $out" ;; esac
echo "ok  missing build-commit label refused"

# 4. a dirty tree AT SCAN TIME does not matter: only the build commit does.
#    (The generated offer itself is written into the tree, so a scan-time
#    check refused every second run.)
(cd "$TMP" && git init -q && git add -A && git -c user.name=t -c user.email=t@t commit -qm fixture && echo scratch > untracked.txt)
write_local_sbom '{"org.opencontainers.image.revision":"abc1234"}'
out="$(run_offer)" || fail "a scan-time dirty tree must not block a clean-built image. Got: $out"
echo "ok  scan-time working tree ignored"

echo "test-gpl-source-offer: PASS"
