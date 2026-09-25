#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-image-pins.sh — check-image-pins.sh still SEES a `FROM` that carries a
# flag.
#
# Why this exists: the gate reads the image ref out of the FROM line by field
# position. Adding `FROM --platform=$BUILDPLATFORM <image>` to the cross-
# compiling build stages moved the ref from field 2 to field 3, and the first
# symptom was the gate failing every pinned stage. The DANGEROUS symptom is the
# mirror image — a future parse change that makes the gate skip flagged lines
# entirely would leave it green while an unpinned base slipped in. A security
# gate that stops detecting is worse than no gate, so assert BOTH directions.
#
# Daemon-free, network-free: it runs the real gate against a throwaway tree.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/scripts"
cp "$ROOT/scripts/check-image-pins.sh" "$TMP/scripts/"

fail() { echo "FAIL: $*" >&2; exit 1; }

# The gate resolves its own repo root from its location, so running the COPY
# scans $TMP and nothing else.
run_gate() { (cd "$TMP" && ./scripts/check-image-pins.sh >/dev/null 2>&1); }
# Cases 4-6 assert on the gate's MESSAGE, not just its status: an unguarded
# `published=$(grep …)` that matches nothing exits 1 under `set -e` too, and a
# CRASH would satisfy a bare non-zero assertion while detecting nothing.
gate_says() { (cd "$TMP" && ./scripts/check-image-pins.sh 2>&1 >/dev/null); }

# 1. flagged AND pinned => pass. (Regression: the ref used to be read as the
#    flag itself, so this shape failed as "not digest-pinned".)
cat > "$TMP/Dockerfile" <<'EOF'
FROM --platform=$BUILDPLATFORM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc AS builder
FROM builder
EOF
run_gate || fail "a flagged, digest-pinned FROM must PASS"
echo "ok  flagged + pinned passes"

# 2. flagged and UNPINNED => fail. This is the one that matters: the gate must
#    not skip a line just because it carries a flag.
cat > "$TMP/Dockerfile" <<'EOF'
FROM --platform=$BUILDPLATFORM alpine:3.20
EOF
if run_gate; then fail "a flagged, UNPINNED FROM must FAIL — the gate went blind"; fi
echo "ok  flagged + unpinned fails"

# 3. unflagged and unpinned => fail (the gate's original job, unchanged).
cat > "$TMP/Dockerfile" <<'EOF'
FROM alpine:3.20
EOF
if run_gate; then fail "an unflagged, UNPINNED FROM must FAIL"; fi
echo "ok  unflagged + unpinned fails"

# ── F214: the published-vs-scanned cross-check must not be indent-anchored ──
#
# The scan-coverage section derived its "published" list with an exact
# ^[[:space:]]{10} anchor while the "scanned" side beside it was already
# indent-agnostic. A publish job written at any other indent — a second matrix,
# a reformat, a `yamlfmt` — dropped every one of its images out of the
# comparison, so an image shipped to a public registry having never been
# scanned produced a GREEN gate. Same failure mode the emptiness guard at the
# dockerfile: extraction already covers, on the other list.
mkdir -p "$TMP/.github/workflows" "$TMP/deploy/images/base"
cat > "$TMP/Dockerfile" <<'EOF'
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
EOF
# A fixture Dockerfile that satisfies the licence/label loop, so the ONLY thing
# cases 4-6 can fail on is the scan-coverage cross-check.
cat > "$TMP/deploy/images/base/Dockerfile" <<'EOF'
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
LABEL org.opencontainers.image.title="fixture" \
      org.opencontainers.image.description="fixture" \
      org.opencontainers.image.licenses="Apache-2.0 AND MIT" \
      org.opencontainers.image.source="https://example.invalid"
COPY LICENSE NOTICE THIRD-PARTY-NOTICES.md LICENSING.md /usr/share/doc/wardyn/
COPY licenses/texts/ /usr/share/doc/wardyn/licenses/
EOF
# ci.yml scans wardynd and nothing else.
cat > "$TMP/.github/workflows/ci.yml" <<'EOF'
jobs:
  trivy:
    strategy:
      matrix:
        include:
          - name: wardynd
  notices:
    runs-on: ubuntu-latest
EOF

write_release_wf() { # INDENT — spaces before the `- name:` rows
  local i="$1"
  { printf 'jobs:\n  publish:\n    strategy:\n      matrix:\n        include:\n'
    printf '%s- name: wardynd\n'          "$i"
    printf '%s  dockerfile: deploy/images/base/Dockerfile\n' "$i"
    printf '%s- name: agent-unscanned\n'  "$i"
    printf '%s  dockerfile: deploy/images/base/Dockerfile\n' "$i"
  } > "$TMP/.github/workflows/release.yml"
}

# 4. the canonical 10-space indent: an unscanned published image FAILS. (This
#    is the behaviour that already worked; it is the counterweight to 5.)
write_release_wf '          '
out="$(gate_says || true)"
case "$out" in
  *"published by"*"never scanned"*"agent-unscanned"*) ;;
  *) fail "an image published but absent from ci.yml's trivy matrix must be NAMED at the canonical indent. Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  unscanned published image is named at the 10-space indent"

# 5. …and at ANY other indent. This is the one that matters.
write_release_wf '      '
out="$(gate_says || true)"
case "$out" in
  *"published by"*"never scanned"*"agent-unscanned"*) ;;
  *) fail "the scan-coverage cross-check went BLIND at a 6-space indent — a published image was never compared against the trivy matrix (F214). Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  unscanned published image is named at a non-canonical indent"

# 6. a release.yml with dockerfile: rows but NO `- name:` rows must say so
#    rather than compare an empty list against anything (the same emptiness
#    guard the dockerfile: extraction carries).
{ printf 'jobs:\n  publish:\n    strategy:\n      matrix:\n        include:\n'
  printf '        - image: wardynd\n'
  printf '          dockerfile: deploy/images/base/Dockerfile\n'
} > "$TMP/.github/workflows/release.yml"
out="$(gate_says || true)"
case "$out" in
  *"no '- name:'"*) ;;
  *) fail "a release.yml whose image names the gate cannot parse must say so — an unguarded empty list either compares nothing or dies under set -e with no message at all (F214). Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  an unparseable published-image list fails loudly"

# ── #357: the GPL offer's hand-listed websockify entry tracks the Dockerfile ─
#
# The gate reads MANUAL_ENTRIES' websockify row out of gpl-source-offer.sh and
# the pin + download URL out of the novnc Dockerfile. Other sections of this
# fixture already fail (case 6's release.yml), so these cases assert on the
# websockify message alone.
mkdir -p "$TMP/deploy/images/novnc"
cat > "$TMP/deploy/images/novnc/Dockerfile" <<'EOF'
ARG WEBSOCKIFY_VERSION=0.13.0
RUN curl -fsSL "https://github.com/novnc/websockify/archive/refs/tags/v${WEBSOCKIFY_VERSION}.tar.gz" -o /tmp/websockify.tar.gz
EOF
write_manual_entry() { # the whole MANUAL_ENTRIES row
  printf 'MANUAL_ENTRIES=(\n  "%s"\n)\n' "$1" > "$TMP/scripts/gpl-source-offer.sh"
}
good_url=https://github.com/novnc/websockify/archive/refs/tags/v0.13.0.tar.gz

# 7. version and URL both match the Dockerfile => no websockify complaint.
write_manual_entry "agent-novnc|websockify|0.13.0|LGPL-3.0|$good_url"
out="$(gate_says || true)"
case "$out" in
  *websockify*) fail "a websockify entry matching the Dockerfile pin must not be flagged. Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  a matching websockify entry passes"

# 8. the version drifts.
write_manual_entry "agent-novnc|websockify|0.12.0|LGPL-3.0|$good_url"
out="$(gate_says || true)"
case "$out" in
  *"websockify 0.12.0"*"WEBSOCKIFY_VERSION=0.13.0"*) ;;
  *) fail "a websockify version that drifts from the Dockerfile pin must FAIL. Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  a drifted websockify version fails"

# 9. the version matches but the source URL still names the old archive: the
#    offer would point at the wrong source.
write_manual_entry "agent-novnc|websockify|0.13.0|LGPL-3.0|https://github.com/novnc/websockify/archive/refs/tags/v0.12.0.tar.gz"
out="$(gate_says || true)"
case "$out" in
  *"websockify source URL"*"v0.12.0.tar.gz"*) ;;
  *) fail "a websockify source URL that drifts from the Dockerfile's download must FAIL. Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  a drifted websockify source URL fails"

# 10. no websockify row the gate can parse (e.g. a changed MANUAL_ENTRIES
#     format) must say so, not pass blind.
write_manual_entry "agent-novnc websockify 0.13.0"
out="$(gate_says || true)"
case "$out" in
  *"no 'agent-novnc|websockify|...' entry"*) ;;
  *) fail "an unparseable websockify entry must fail loudly. Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  an unparseable websockify entry fails loudly"

echo "test-image-pins: PASS"
