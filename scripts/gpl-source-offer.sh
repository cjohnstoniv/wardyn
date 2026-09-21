#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Generate deploy/images/THIRD-PARTY-GPL.md — the GPL/LGPL corresponding-source
# offer for the packages inside every PUBLISHED container image.
#
# WHY. NOTICE used to argue that asciinema is executed as a subprocess and never
# linked. That is true, and it answers GPL s5 (derivative works). It does not
# answer s6 (conveying): pushing an image to a public registry conveys every
# binary in it. The obligation attaches to the base-image packages too, so scoping
# this to asciinema alone would have been wrong in the same way, only quieter.
#
# MACHINE-DERIVED ON PURPOSE, image LIST included. A hand-written list of ~95
# Debian packages per image would be wrong within one base-image refresh — and
# a hand-written IMAGES= array drifted too: agent-claude-code stayed listed
# long after 0.6.2 stopped publishing it and started publishing agent-base in
# its place, so the loop errored on the retired name (an interactive paste with
# no `set -e` just carries on) while agent-base — the image actually published
# — was never scanned at all. IMAGES below is read straight out of release.yml's
# publish matrix (the same `- name:` idiom scripts/check-image-pins.sh already
# uses for its own published-image derivation), so this file and the workflow
# that actually pushes images cannot disagree.
#
# Usage: scripts/gpl-source-offer.sh <sbom-dir> <tag>
set -euo pipefail
cd "$(dirname "$0")/.."
# INPUT. This reads syft SBOMs from a directory. Nothing in the repo produces
# one any more — `make sbom` and every syft call were deleted, and the only
# surviving syft run is in release.yml against a PUSHED digest, i.e. available
# only AFTER the tag, while this file must be correct IN the release commit.
#
# So for an image with a PRIOR release, the input is produced by hand, pre-tag,
# against that PREVIOUS release's published digest:
#
#     mkdir -p /tmp/sbom
#     for i in wardynd wardyn-proxy agent-base agent-codex-cli agent-aws-sso; do
#       syft "registry:ghcr.io/cjohnstoniv/$i:<prev-tag>" -o syft-json \
#         > "/tmp/sbom/04-sbom-$i-<prev-tag>-amd64.json"
#     done
#     scripts/gpl-source-offer.sh /tmp/sbom <prev-tag>
#
# BOOTSTRAP. An image entering the publish matrix for the FIRST time (e.g. #141
# adding agent-vscode/agent-novnc) has no prior published digest — the loop
# above finds no file for it, and until this script's own coverage cross-check
# existed that meant it shipped with no offer at all, silently. For that case,
# scan the LOCAL build instead (the identical recipe the image will actually
# publish from) and drop the result beside the other SBOMs under a
# `local-sbom-<image>.json` name:
#
#     make agent-image-novnc   # or whichever image
#     syft "wardyn/agent-novnc:local" -o syft-json \
#       > "/tmp/sbom/local-sbom-agent-novnc.json"
#
# The per-image loop below tries the published-digest file first and falls
# back to this local one automatically — no flag needed once the image is
# listed in release.yml. To cover an image that is NOT YET in release.yml at
# all — the exact situation an offer must be produced in ahead of a first
# publish — name it explicitly:
#
#     BOOTSTRAP_IMAGES="agent-vscode agent-novnc" scripts/gpl-source-offer.sh /tmp/sbom <tag>
#
# Either way the generated section says plainly that it was scanned
# pre-publication from the local recipe, not from a pushed digest.
#
# Do NOT try to source any of this from release.yml's dry run: under dry_run
# that job writes `{"components":[]}` stubs and skips the merge entirely, so it
# produces zero-component files, not image SBOMs.
SBOM_DIR="${1:?usage: gpl-source-offer.sh <sbom-dir> <tag>}"
TAG="${2:?usage: gpl-source-offer.sh <sbom-dir> <tag> — no default; a stale default silently regenerates the offer for the wrong release}"
OUT=deploy/images/THIRD-PARTY-GPL.md
export LC_ALL=C

# The CURRENTLY PUBLISHED set, read from release.yml's own matrix rather than
# hand-listed (see the header). Same idiom check-image-pins.sh already uses.
RELEASE_WF=.github/workflows/release.yml
[ -f "$RELEASE_WF" ] || { echo "FATAL: $RELEASE_WF not found — cannot derive the published-image list." >&2; exit 1; }
mapfile -t IMAGES < <(grep -oE '^[[:space:]]+- name: [a-z0-9-]+$' "$RELEASE_WF" | awk '{print $3}' | sort -u)
[ "${#IMAGES[@]}" -gt 0 ] || { echo "FATAL: derived zero images from $RELEASE_WF's publish matrix — refusing to generate an offer covering nothing." >&2; exit 1; }

# Images not yet IN that matrix but about to be (see BOOTSTRAP above). Added
# after IMAGES, skipping any name that already came from the matrix — once an
# image lands in release.yml it no longer needs to be named here.
ALL_IMAGES=("${IMAGES[@]}")
for b in ${BOOTSTRAP_IMAGES:-}; do
  skip=0
  for i in "${IMAGES[@]}"; do [ "$i" = "$b" ] && { skip=1; break; }; done
  ((skip)) || ALL_IMAGES+=("$b")
done

# Frozen history: offers owed for artifacts that can no longer be re-scanned
# (withdrawn tags, the deleted agent-claude-code package) live as static text
# in deploy/images/third-party-gpl-historical.md, emitted verbatim below.
# Regenerating from the CURRENT publish set alone once deleted the only offer
# covering still-owed copies — a legal regression that produces no error — so
# the historical file is REQUIRED: its absence fails this script rather than
# silently narrowing the offer. Retire content from it only when a section's
# stated three-year window has lapsed.
HISTORICAL_FILE=deploy/images/third-party-gpl-historical.md
[ -f "$HISTORICAL_FILE" ] || { echo "FATAL: $HISTORICAL_FILE missing — regenerating without it would delete offers still owed" >&2; exit 1; }

# ── resolve every image's SBOM BEFORE writing anything ──────────────────────
#
# A separate pass, ahead of the `> "$OUT"` write below: a missing SBOM fails
# loudly here and leaves the last-known-good committed offer untouched, rather
# than clobbering it with a partial file that silently narrows coverage — the
# exact failure this script exists to close (see "## `$img`" / "_No SBOM
# available._", which used to be how a gap like that looked: present, quiet,
# and covering nothing).
declare -A SBOM_FILE IS_BOOTSTRAP
missing=()
for img in "${ALL_IMAGES[@]}"; do
  pub="$SBOM_DIR/04-sbom-${img}-${TAG}-amd64.json"
  local="$SBOM_DIR/local-sbom-${img}.json"
  if [ -f "$pub" ]; then
    SBOM_FILE["$img"]="$pub"
    IS_BOOTSTRAP["$img"]=0
  elif [ -f "$local" ]; then
    SBOM_FILE["$img"]="$local"
    IS_BOOTSTRAP["$img"]=1
  else
    missing+=("$img (looked for $pub and $local)")
  fi
done
if [ "${#missing[@]}" -gt 0 ]; then
  echo "FATAL: no SBOM for:" >&2
  printf '  %s\n' "${missing[@]}" >&2
  echo "Scan the published digest (previous tag), or for a first-time publish the local build — see this script's header." >&2
  exit 1
fi

{
  echo "# Corresponding source for GPL and LGPL components in the published images"
  echo
  echo "Wardyn's own code is Apache-2.0 and contains no copyleft. The **container"
  echo "images** are a different artifact: an image is an operating system, and the"
  echo "Debian and Alpine base layers carry GPL- and LGPL-licensed packages."
  echo
  echo "Publishing an image conveys those binaries. GPL-2.0 s3 and GPL-3.0 s6 attach a"
  echo "corresponding-source obligation to whoever conveys them — including you, if you"
  echo "mirror these images into a registry other people pull from. This file is that"
  echo "offer, and it is what you would pass on."
  echo
  echo "None of these packages are modified by Wardyn. Every one is the unmodified"
  echo "distribution package, so the corresponding source is the distribution's own,"
  echo "obtainable from:"
  echo
  echo "- **Debian** (\`debian:bookworm-slim\`, \`node:24-bookworm-slim\`, and the"
  echo "  \`gcr.io/distroless/static-debian12\` base): \`https://snapshot.debian.org\`"
  echo "  pinned to the package version below, or \`apt-get source <package>\` on a"
  echo "  bookworm host. Per-package copyright and licence text also ships inside each"
  echo "  image at \`/usr/share/doc/<package>/copyright\`."
  echo "- **Alpine** (\`alpine:3.24\`): \`https://gitlab.alpinelinux.org/alpine/aports\`"
  echo "  at the matching aport version."
  echo "- **asciinema** (installed by Wardyn's own Dockerfile, not inherited):"
  echo "  \`https://github.com/asciinema/asciinema\` at the version below."
  echo
  echo "This offer stands while an image remains pullable and for three years after"
  echo "the last copy of it is conveyed (GPLv3 s6(b)); withdrawing a tag starts that"
  echo "three-year clock rather than ending the offer. To request source directly"
  echo "rather than from the upstream archives, open an issue at"
  echo "\`https://github.com/cjohnstoniv/wardyn\`."
  echo
  echo "Generated by \`scripts/gpl-source-offer.sh\` from the syft SBOMs of the published"
  echo "image digests. Regenerate whenever a base image changes."
  echo
  for img in "${ALL_IMAGES[@]}"; do
    f="${SBOM_FILE[$img]}"
    n=$(jq -r '[.artifacts[] | select(((.licenses//[])|map(.value//.spdxExpression//"")|join(" "))|test("GPL";"i"))] | length' "$f")
    echo "## \`ghcr.io/cjohnstoniv/${img}:${TAG}\`"
    echo
    if [ "${IS_BOOTSTRAP[$img]}" = 1 ]; then
      echo "_Scanned before publication, from \`wardyn/${img}:local\` — the identical build"
      echo "recipe this image publishes from. No published digest exists yet to scan; this"
      echo "row will be re-scanned from the pushed digest once one does._"
      echo
    fi
    echo "$n package(s) carrying a GPL or LGPL term."
    echo
    if [ "$n" -gt 0 ]; then
      echo "| package | version | licence | type |"
      echo "|---|---|---|---|"
      jq -r '.artifacts[]
             | select(((.licenses//[])|map(.value//.spdxExpression//"")|join(" "))|test("GPL";"i"))
             | [.name, .version, (((.licenses//[])|map(.value//.spdxExpression//"")|join(", "))), .type]
             | @tsv' "$f" \
        | sort -u | while IFS=$'\t' read -r n v l t; do printf '| `%s` | %s | %s | %s |\n' "$n" "$v" "$l" "$t"; done
    fi
    echo
  done

  # ── frozen history (see HISTORICAL_FILE above) ────────────────────────────
  cat "$HISTORICAL_FILE"
} > "$OUT"
echo "wrote $OUT ($(wc -l < "$OUT") lines)"
