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
# MACHINE-DERIVED ON PURPOSE. A hand-written list of ~95 Debian packages per image
# would be wrong within one base-image refresh. This reads the same syft SBOMs the
# licence gate reads, so the list is whatever is actually in the image.
#
# Usage: scripts/gpl-source-offer.sh <sbom-dir> [tag]
set -euo pipefail
cd "$(dirname "$0")/.."
# INPUT. This reads syft SBOMs from a directory. Nothing in the repo produces
# one any more — `make sbom` and every syft call were deleted, and the only
# surviving syft run is in release.yml against a PUSHED digest, i.e. available
# only AFTER the tag, while this file must be correct IN the release commit.
#
# So the input is produced by hand, pre-tag, against the PREVIOUS release's
# published digests:
#
#     mkdir -p /tmp/sbom
#     for i in wardynd wardyn-proxy agent-base agent-codex-cli agent-aws-sso; do
#       syft "registry:ghcr.io/cjohnstoniv/$i:<prev-tag>" -o syft-json \
#         > "/tmp/sbom/04-sbom-$i-<prev-tag>-amd64.json"
#     done
#     scripts/gpl-source-offer.sh /tmp/sbom <prev-tag>
#
# Do NOT try to source this from release.yml's dry run: under dry_run that job
# writes `{"components":[]}` stubs and skips the merge entirely, so it produces
# five zero-component files, not image SBOMs.
SBOM_DIR="${1:?usage: gpl-source-offer.sh <sbom-dir> [tag]}"
TAG="${2:?usage: gpl-source-offer.sh <sbom-dir> <tag> — no default; a stale default silently regenerates the offer for the wrong release}"
OUT=deploy/images/THIRD-PARTY-GPL.md
export LC_ALL=C

# The CURRENTLY PUBLISHED set, which is what release.yml's matrix pushes.
# agent-claude-code is deliberately ABSENT: 0.6.2 stopped publishing it and
# publishes agent-base in its place. It was still listed here long after that,
# so the loop errored on it (an interactive paste with no `set -e` just carries
# on) while agent-base — the image that IS published — was never scanned at all.
IMAGES=(wardynd wardyn-proxy agent-base agent-codex-cli agent-aws-sso)

# Frozen history: offers owed for artifacts that can no longer be re-scanned
# (withdrawn tags, the deleted agent-claude-code package) live as static text
# in deploy/images/third-party-gpl-historical.md, emitted verbatim below.
# Regenerating from the CURRENT publish set alone once deleted the only offer
# covering still-owed copies — a legal regression that produces no error — so
# the historical file is REQUIRED: its absence fails this script rather than
# silently narrowing the offer. Retire content from it only when a section's
# stated three-year window has lapsed.
HISTORICAL_FILE=deploy/images/third-party-gpl-historical.md

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
  echo "- **Debian** (\`debian:bookworm-slim\`, \`node:22-bookworm-slim\`, and the"
  echo "  \`gcr.io/distroless/static-debian12\` base): \`https://snapshot.debian.org\`"
  echo "  pinned to the package version below, or \`apt-get source <package>\` on a"
  echo "  bookworm host. Per-package copyright and licence text also ships inside each"
  echo "  image at \`/usr/share/doc/<package>/copyright\`."
  echo "- **Alpine** (\`alpine:3.20\`): \`https://gitlab.alpinelinux.org/alpine/aports\`"
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
  for img in "${IMAGES[@]}"; do
    f="$SBOM_DIR/04-sbom-${img}-${TAG}-amd64.json"
    [ -f "$f" ] || { echo "## \`$img\`"; echo; echo "_No SBOM available._"; echo; continue; }
    n=$(jq -r '[.artifacts[] | select(((.licenses//[])|map(.value//.spdxExpression//"")|join(" "))|test("GPL";"i"))] | length' "$f")
    echo "## \`ghcr.io/cjohnstoniv/${img}:${TAG}\`"
    echo
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
  [ -f "$HISTORICAL_FILE" ] || { echo "FATAL: $HISTORICAL_FILE missing — regenerating without it would delete offers still owed" >&2; exit 1; }
  cat "$HISTORICAL_FILE"
} > "$OUT"
echo "wrote $OUT ($(wc -l < "$OUT") lines)"
