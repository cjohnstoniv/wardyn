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
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"

TMP="$(mktemp -d)"
TMP2="$(mktemp -d)"
trap 'rm -rf "$TMP" "$TMP2"' EXIT
mkdir -p "$TMP/scripts/lib"
cp "$ROOT/scripts/check-image-pins.sh" "$TMP/scripts/"
cp "$ROOT/scripts/lib/common.sh" "$ROOT/scripts/lib/image_downloads.py" "$TMP/scripts/lib/"

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
  trivy-wardynd:
    name: trivy (wardynd)
    env:
      TRIVY_IMAGE: wardynd
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
  *) fail "an image published but absent from ci.yml's trivy jobs must be NAMED at the canonical indent. Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  unscanned published image is named at the 10-space indent"

# 5. …and at ANY other indent. This is the one that matters.
write_release_wf '      '
out="$(gate_says || true)"
case "$out" in
  *"published by"*"never scanned"*"agent-unscanned"*) ;;
  *) fail "the scan-coverage cross-check went BLIND at a 6-space indent — a published image was never compared against the trivy jobs (F214). Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
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

# 6a. a trivy job named for one image and scanning another: the required
#     `trivy (wardynd)` check would be green for an image nothing scanned.
write_release_wf '          '
cat > "$TMP/.github/workflows/ci.yml" <<'EOF'
jobs:
  trivy-wardynd:
    name: trivy (wardynd)
    env:
      TRIVY_IMAGE: agent-unscanned
EOF
out="$(gate_says || true)"
case "$out" in
  *"named for one image and scans another"*"trivy (wardynd) scans agent-unscanned"*) ;;
  *) fail "a trivy job whose name and TRIVY_IMAGE disagree must be named. Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  a trivy job named for one image and scanning another fails"

# ── #357: the GPL offer's hand-listed websockify entry tracks the Dockerfile ─
#
# The gate reads MANUAL_ENTRIES' websockify row out of gpl-source-offer.sh and
# the pin + download URL out of the novnc Dockerfile. Other sections of this
# fixture already fail (case 6's release.yml), so these cases assert on the
# websockify message alone.
mkdir -p "$TMP/deploy/images/novnc"
cat > "$TMP/deploy/images/novnc/Dockerfile" <<'EOF'
ARG WEBSOCKIFY_VERSION=0.13.0
RUN curl -fsSL "https://github.com/novnc/websockify/archive/refs/tags/v${WEBSOCKIFY_VERSION}.tar.gz" -o /tmp/websockify.tar.gz \
 && echo "b6413e364efd04f3c92ec8c17747e3c4adc20157c2ef1c5d019a26d944a46df8  /tmp/websockify.tar.gz" | sha256sum -c -
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

cat > "$TMP/Dockerfile" <<'EOF'
RUN set -e; curl -fsSL https://example.invalid/unchecked -o /tmp/unchecked
EOF
out="$(gate_says || true)"
case "$out" in
  *"/tmp/unchecked"*"same-file checksum"*) ;;
  *) fail "the image-pin gate must call the download guard. Got: $out" ;;
esac
echo "ok  unchecked curl fails through the image-pin gate"
python3 "$ROOT/scripts/test-image-downloads.py"
python3 "$ROOT/scripts/test-image-download-safety.py"
python3 "$ROOT/scripts/test-image-recipes.py"

# ── two release lines: the GPL-offer check reads the tree's own line, not the newest tag ──
#
# Once 0.11 is released, a release/0.10 tree must compare its GPL offer with the
# last 0.10 release's publish matrix, not 0.11's. A scratch repository holds
# v0.10.3 (publishes "bar") on this branch and v0.11.0 (publishes "foo") on a
# side branch, so v0.11.0 is the newest final by version and not an ancestor.
# Auto-maintenance is off, as in scripts/test-kind-follow-main.sh (guard 25).
export GIT_CONFIG_COUNT=3 GIT_CONFIG_KEY_0=maintenance.auto GIT_CONFIG_VALUE_0=false \
  GIT_CONFIG_KEY_1=gc.auto GIT_CONFIG_VALUE_1=0 GIT_CONFIG_KEY_2=gc.autoDetach GIT_CONFIG_VALUE_2=false
g2() { git -C "$TMP2" -c user.name=t -c user.email=t@t "$@"; }
gate_says2() { (cd "$TMP2" && ./scripts/check-image-pins.sh 2>&1 >/dev/null); }
mkdir -p "$TMP2/scripts/lib" "$TMP2/.github/workflows" "$TMP2/deploy/images" "$TMP2/internal/version"
cp "$ROOT/scripts/check-image-pins.sh" "$TMP2/scripts/"
cp "$ROOT/scripts/lib/common.sh" "$ROOT/scripts/lib/image_downloads.py" "$TMP2/scripts/lib/"
cp "$TMP/Dockerfile" "$TMP2/Dockerfile"
printf '## `ghcr.io/cjohnstoniv/foo` (not yet published)\n' > "$TMP2/deploy/images/THIRD-PARTY-GPL.md"
g2 init -q -b main
printf '          - name: bar\n' > "$TMP2/.github/workflows/release.yml"
g2 add -A && g2 commit -q -m a && g2 tag v0.10.3
g2 switch -q -c side
printf '          - name: foo\n' > "$TMP2/.github/workflows/release.yml"
g2 commit -q -am b && g2 tag v0.11.0
g2 switch -q main

# 7. a 0.10.4 tree reads v0.10.3, so foo is not yet published to it.
printf 'const Version = "0.10.4"\n' > "$TMP2/internal/version/version.go"
out="$(gate_says2 || true)"
case "$out" in
  *"already published"*) fail "a release/0.10 tree read a newer line's publish matrix. Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  a release/0.10 tree reads its own line's last final"

# 8. a 0.11.1 tree reads v0.11.0, which published foo.
printf 'const Version = "0.11.1"\n' > "$TMP2/internal/version/version.go"
out="$(gate_says2 || true)"
case "$out" in
  *"that v0.11.0 already published"*) ;;
  *) fail "a 0.11 tree must read v0.11.0's publish matrix. Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  a 0.11 tree reads v0.11.0"

# 9. no version line: the gate cannot tell its line and says so.
rm "$TMP2/internal/version/version.go"
out="$(gate_says2 || true)"
case "$out" in
  *"has no 'const Version"*) ;;
  *) fail "a tree with no version line must fail loudly. Got: $(printf '%s' "$out" | tr '\n' ' ')" ;;
esac
echo "ok  a tree with no version line fails loudly"

echo "test-image-pins: PASS"
