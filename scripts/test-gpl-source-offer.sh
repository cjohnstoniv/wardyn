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

# Throwaway repos get no background git: a commit or fetch can leave detached
# auto-maintenance writing under .git when the EXIT trap runs rm -rf (#3328).
export GIT_CONFIG_COUNT=3 GIT_CONFIG_KEY_0=maintenance.auto GIT_CONFIG_VALUE_0=false GIT_CONFIG_KEY_1=gc.auto GIT_CONFIG_VALUE_1=0 GIT_CONFIG_KEY_2=gc.autoDetach GIT_CONFIG_VALUE_2=false

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

# 5. check-image-pins.sh fails a "(not yet published)" section for an image the
#    previous release tag already publishes (#2192), and only then. The block is
#    extracted from the real gate and run in a throwaway repo with one tag.
block="$(awk '/^# A "\(not yet published\)" section is true only/{f=1} /^# ── the GPL offer.s hand-listed websockify/{f=0} f' "$ROOT/scripts/check-image-pins.sh")"
[ -n "$block" ] || fail "check-image-pins.sh lost its not-yet-published staleness check"
R="$(mktemp -d)"; trap 'rm -rf "$TMP" "$R"' EXIT
mkdir -p "$R/.github/workflows" "$R/deploy/images"
printf 'jobs:\n  publish:\n    strategy:\n      matrix:\n        include:\n          - name: wardynd\n          - name: wardynd-fips\n' > "$R/.github/workflows/release.yml"
(cd "$R" && git init -q && git add -A && git -c user.name=t -c user.email=t@t commit -qm one && git tag v1.0.0)
run_stale() { (cd "$R" && fail=0 && GPL_OFFER=deploy/images/THIRD-PARTY-GPL.md && eval "$block" 2>&1; echo "fail=$fail"); }
printf '## `ghcr.io/cjohnstoniv/wardynd-fips` (not yet published)\n' > "$R/deploy/images/THIRD-PARTY-GPL.md"
out="$(run_stale)"
case "$out" in *"wardynd-fips"*"fail=1"*) ;; *) fail "a '(not yet published)' section for an image v1.0.0 published must fail. Got: $out" ;; esac
printf '## `ghcr.io/cjohnstoniv/wardynd-fips:1.0.0`\n## `ghcr.io/cjohnstoniv/newimage` (not yet published)\n' > "$R/deploy/images/THIRD-PARTY-GPL.md"
out="$(run_stale)"
case "$out" in *"fail=0"*) ;; *) fail "a published section, and a bootstrap section for an image no tag published, must pass. Got: $out" ;; esac
echo "ok  stale not-yet-published section fails the gate"
# The newest final tag counts even when it is not an ancestor of HEAD (a patch cut
# on a release branch), and a later pre-release tag does not displace it.
(cd "$R" && git checkout -q -b side && printf 'jobs:\n  publish:\n    strategy:\n      matrix:\n        include:\n          - name: wardynd\n          - name: newimage\n' > .github/workflows/release.yml \
  && git -c user.name=t -c user.email=t@t commit -qam two && git tag v1.1.0 && git checkout -q - && git tag v1.2.0-rc.1)
printf '## `ghcr.io/cjohnstoniv/newimage` (not yet published)\n' > "$R/deploy/images/THIRD-PARTY-GPL.md"
out="$(run_stale)"
case "$out" in *"newimage"*"fail=1"*) ;; *) fail "the newest final tag (v1.1.0, not an ancestor of HEAD) was not used for the staleness check. Got: $out" ;; esac
echo "ok  staleness check uses the newest final tag"

# 6. A package a Dockerfile repacks with a `+wardyn<N>` version is described as
#    MODIFIED even when no SBOM shows it yet (the first release it ships in is
#    written from the PREVIOUS release's SBOMs). The recipes are read from the
#    real Dockerfiles; with none, the original sentence and preamble stay.
M="$(mktemp -d)"; trap 'rm -rf "$TMP" "$R" "$M"' EXIT
mkdir -p "$M/scripts" "$M/.github/workflows" "$M/deploy/images/base" "$M/deploy/images/codex-cli" "$M/deploy/images/vscode" "$M/sbom"
cp "$ROOT/scripts/gpl-source-offer.sh" "$M/scripts/"
cp "$ROOT/deploy/images/base/Dockerfile" "$M/deploy/images/base/"
cp "$ROOT/deploy/images/codex-cli/Dockerfile" "$M/deploy/images/codex-cli/"
printf 'ARG BASE_IMAGE=wardyn/agent-base:local\nFROM ${BASE_IMAGE}\n' > "$M/deploy/images/vscode/Dockerfile"
echo "## historical" > "$M/deploy/images/third-party-gpl-historical.md"
matrix() { printf 'jobs:\n  publish:\n    strategy:\n      matrix:\n        include:\n'; for i in "$@"; do printf '          - name: %s\n            dockerfile: deploy/images/%s/Dockerfile\n' "$i" "${i#agent-}"; done; }
sbom() { # IMAGE ASCIINEMA-VERSION
  printf '{"source":{"type":"image","metadata":{}},"artifacts":[{"name":"asciinema","version":"%s","licenses":[{"value":"GPL-3"}],"type":"deb"}]}\n' "$2" > "$M/sbom/04-sbom-$1-v0.0.2-amd64.json"
}
offer() { (cd "$M" && ./scripts/gpl-source-offer.sh sbom v0.0.2 >/dev/null 2>&1) && cat "$M/deploy/images/THIRD-PARTY-GPL.md"; }
section() { awk -v h="## \`ghcr.io/cjohnstoniv/$2:v0.0.2\`" '/^## /{f = ($0 == h)} f' <<<"$1"; }

# 6a. no recipe (Dockerfiles absent) => unchanged wording, and the preamble is
#     byte-for-byte the pinned no-modification preamble (the one every offer
#     before the first repack carried). It is pinned in a fixture, not read from
#     the live offer, because the live offer gains the modified preamble once a
#     release ships a repacked package.
matrix wardynd > "$M/.github/workflows/release.yml"; echo '{"source":{"type":"image","metadata":{}},"artifacts":[]}' > "$M/sbom/04-sbom-wardynd-v0.0.2-amd64.json"
out="$(offer)" || fail "the generator must succeed with no repacked package"
case "$out" in *"None of these packages are modified by Wardyn."*) ;; *) fail "with no repacked package the offer must keep its 'None of these packages are modified' sentence" ;; esac
case "$out" in *"Modified by Wardyn"*) fail "with no repacked package the offer must not carry a modified-package section" ;; esac
# $(...) drops trailing newlines on both sides, so the fixture needs no blank last line.
cmp -s <(printf '%s\n' "$(awk '/^## /{exit} {print}' <<<"$out")") <(printf '%s\n' "$(cat "$ROOT/scripts/test-gpl-source-offer.d/preamble-unmodified.md")") \
  || fail "the no-modification preamble must stay byte-identical to scripts/test-gpl-source-offer.d/preamble-unmodified.md"
echo "ok  no repacked package: wording and preamble unchanged"

# 6b. the real base and codex-cli Dockerfiles carry the repack; the SBOMs predate it.
matrix agent-base agent-codex-cli agent-vscode > "$M/.github/workflows/release.yml"
sbom agent-base 2.4.0-1; sbom agent-codex-cli 2.4.0-1; sbom agent-vscode 2.4.0-1
out="$(offer)" || fail "the generator must succeed with a repacked package"
case "$out" in *"None of these packages are modified"*) fail "an offer with a repacked package must not say none are modified" ;; esac
for img in agent-base agent-codex-cli agent-vscode; do
  sec="$(section "$out" "$img")"
  for want in 'Modified by Wardyn: `asciinema`' '`Depends:` changed from `python3:any, python3-pkg-resources` to `python3:any` (dropped `python3-pkg-resources`)' '`Version:` gained the suffix `+wardyn1`' 'apt-get source asciinema=<that version>' 'built before this change'; do
    grep -qF -- "$want" <<<"$sec" || fail "$img section must say: $want"
  done
done
grep -qF 'RUN` in `deploy/images/codex-cli/Dockerfile`' <<<"$(section "$out" agent-codex-cli)" || fail "codex-cli's section must point at its own Dockerfile"
grep -qF 'RUN` in `deploy/images/base/Dockerfile`' <<<"$(section "$out" agent-vscode)" || fail "an image built FROM agent-base must point at the base Dockerfile's recipe"
echo "ok  repacked package described from the Dockerfiles, before any SBOM shows it"

# 6c. once an SBOM row carries the suffix the section names the exact Debian version.
sbom agent-base 2.4.0-1+wardyn1
sec="$(section "$(offer)" agent-base)"
grep -qF 'This image carries `asciinema 2.4.0-1+wardyn1`, which is Debian'"'"'s `asciinema 2.4.0-1`' <<<"$sec" || fail "an SBOM row with the suffix must name Debian's own version"
echo "ok  suffixed SBOM row names the Debian version"

echo "test-gpl-source-offer: PASS"
