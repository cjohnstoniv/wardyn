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
#
# Run it in every release commit (RELEASING.md): the offer must name the digests
# the previous release published, never a pre-publication scan of an image that
# has since shipped. scripts/check-image-pins.sh fails a "(not yet published)"
# section for an image the previous tag's release.yml already publishes.
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
#     for i in $(git show v<prev-tag>:.github/workflows/release.yml \
#                | grep -oE '^[[:space:]]+- name: [a-z0-9-]+$' | awk '{print $3}' | sort -u); do
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
# Build it with make, not a hand `docker build`: the Makefile's image targets
# stamp the build commit on the image as org.opencontainers.image.revision
# (with a `-dirty` suffix when tracked files differed from it), and the
# generated note names that commit, read back from the SBOM. An image with no
# such label, or one built from a dirty tree, is refused: its note would name
# a commit the image was not built from.
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

# ── manual supplements: components syft cannot see ──────────────────────────
#
# syft reads package-manager metadata (dpkg/apk/npm/...). A component
# installed straight from a source tarball, with no manifest syft recognises,
# is conveyed but invisible to every scan — the exact gap that shipped with
# zero rows for websockify (deploy/images/novnc/Dockerfile installs it from a
# GitHub release tarball, not apt). Each entry here is merged into that
# image's table on every regeneration, so it survives a rescan instead of
# being hand-typed into a file this script overwrites wholesale.
#
# Fields: image|package|version|licence|source-url. Keep the version and URL
# in sync with the Dockerfile ARG that pins them. scripts/check-image-pins.sh
# (`make lint`) fails when the websockify row's version or URL drifts from
# deploy/images/novnc/Dockerfile; a new entry here needs its own check there.
MANUAL_ENTRIES=(
  "agent-novnc|websockify|0.13.0|LGPL-3.0|https://github.com/novnc/websockify/archive/refs/tags/v0.13.0.tar.gz"
  "agent-novnc-signin|websockify|0.13.0|LGPL-3.0|https://github.com/novnc/websockify/archive/refs/tags/v0.13.0.tar.gz"
)

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

# ── packages Wardyn repacks (read from the Dockerfiles, not from any SBOM) ───
#
# A package a Dockerfile rebuilds with a `+wardyn<N>` version suffix is MODIFIED
# by Wardyn, and the offer must say so. The previous release's SBOMs (this
# script's input) cannot show a package that first ships in the release being
# written, so the recipes are read from the Dockerfiles themselves: a RUN that
# runs `apt-get download <pkg>` and sets a `+wardyn<N>` version. An image built
# FROM a local Wardyn image (`wardyn/agent-<name>:local`) carries its base's
# modified packages too. scripts/test-gpl-source-offer.sh pins the parse against
# the real Dockerfiles.
declare -A DOCKERFILE_OF
for img in "${ALL_IMAGES[@]}"; do
  df=$(awk -v n="$img" '$1 == "-" && $2 == "name:" {cur = $3} $1 == "dockerfile:" && cur == n {print $2; exit}' "$RELEASE_WF")
  [ -n "$df" ] || df="deploy/images/${img#agent-}/Dockerfile"
  DOCKERFILE_OF["$img"]="$df"
done

# modified_recipes <Dockerfile>: one `pkg|suffix|depends-before|depends-after|dockerfile`
# line per modified package, the Dockerfile's own and its local base's.
modified_recipes() {
  local df=$1 base logical pkg suffix re before after
  [ -f "$df" ] || return 0
  base=$(sed -n 's|^ARG BASE_IMAGE=wardyn/agent-\([a-z0-9-]*\):local$|\1|p' "$df" | head -n 1)
  [ -z "$base" ] || modified_recipes "deploy/images/$base/Dockerfile"
  re='s/\^Depends: ([^/]*)\$/Depends: ([^/]*)/'
  while IFS= read -r logical; do
    [[ "$logical" =~ \+wardyn[0-9]+ ]] || continue
    suffix="${BASH_REMATCH[0]}"
    # A `+wardyn<N>` RUN this parse cannot read is a modified package the offer
    # would silently omit: refuse instead.
    [[ "$logical" =~ apt-get\ download\ ([a-z0-9.+-]+) ]] || { echo "FATAL: $df sets a +wardyn version in a RUN with no 'apt-get download <pkg>'; teach $0 this recipe shape" >&2; exit 1; }
    pkg="${BASH_REMATCH[1]}"
    [[ "$logical" =~ $re ]] || { echo "FATAL: $df repacks $pkg but its Depends edit is not the 's/^Depends: .../Depends: .../' form $0 reads" >&2; exit 1; }
    before="${BASH_REMATCH[1]}"; after="${BASH_REMATCH[2]}"
    printf '%s|%s|%s|%s|%s\n' "$pkg" "$suffix" "$before" "$after" "$df"
  done < <(grep -Ev '^[[:space:]]*#' "$df" | awk '{ if (sub(/\\[[:space:]]*$/, "")) {buf = buf $0} else {print buf $0; buf = ""} }')
}
declare -A MODIFIED
modified_total=0
for img in "${ALL_IMAGES[@]}"; do
  MODIFIED["$img"]=$(modified_recipes "${DOCKERFILE_OF[$img]}" | sort -u)
  [ -z "${MODIFIED[$img]}" ] || modified_total=$((modified_total + 1))
done

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

# A bootstrap entry's note names the commit its local image was BUILT from:
# the org.opencontainers.image.revision label the Makefile stamps at build time
# (see the header), which syft records under .source.metadata.labels. The
# working tree at scan time says nothing about the build, so it is not read.
# No label, or a `-dirty` one, means no commit describes what was built —
# refuse rather than write a note naming the wrong source.
declare -A BUILD_REV
unbuilt=()
for img in "${ALL_IMAGES[@]}"; do
  [ "${IS_BOOTSTRAP[$img]}" = 1 ] || continue
  rev=$(jq -r '.source.metadata.labels["org.opencontainers.image.revision"] // ""' "${SBOM_FILE[$img]}")
  case "$rev" in
    "")      unbuilt+=("$img: no org.opencontainers.image.revision label — build it with \`make agent-image-<name>\`, then rescan") ;;
    *-dirty) unbuilt+=("$img: built from a dirty tree ($rev) — commit, rebuild with make, then rescan") ;;
    *)       BUILD_REV["$img"]="$rev" ;;
  esac
done
if [ "${#unbuilt[@]}" -gt 0 ]; then
  echo "FATAL: cannot name the build commit of bootstrap image(s):" >&2
  printf '  %s\n' "${unbuilt[@]}" >&2
  exit 1
fi

# An SBOM row carrying a `+wardyn<N>` version is a package Wardyn modified. If
# no Dockerfile recipe describes it any more (the repack was dropped after that
# image shipped), the offer cannot say how it differs from Debian's: refuse
# rather than print "None of these packages are modified" over that row.
orphans=()
for img in "${ALL_IMAGES[@]}"; do
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    [[ $'\n'"${MODIFIED[$img]}" == *$'\n'"${row%% *}|"* ]] || orphans+=("$img: $row")
  done < <(jq -r '.artifacts[] | select((.version // "") | test("\\+wardyn[0-9]+$")) | "\(.name) \(.version)"' "${SBOM_FILE[$img]}")
done
if [ "${#orphans[@]}" -gt 0 ]; then
  echo "FATAL: these SBOM rows carry a +wardyn version that no Dockerfile recipe describes; describe them before writing the offer:" >&2
  printf '  %s\n' "${orphans[@]}" >&2
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
  if [ "$modified_total" -eq 0 ]; then
    echo "None of these packages are modified by Wardyn. Most are the unmodified"
  else
    echo "Wardyn modifies only the packages named under \"Modified by Wardyn\" in the"
    echo "image sections below. Every other package is unmodified: most are the unmodified"
  fi
  echo "distribution package; a few are an unmodified upstream release tarball or an"
  echo "npm package bundled by the vendor tool that ships it, named per-entry where"
  echo "that applies. The corresponding source is the applicable one below:"
  echo
  echo "- **Debian** (\`debian:trixie-slim\`, \`node:24-trixie-slim\`, and the"
  echo "  \`gcr.io/distroless/static-debian12\` base): \`https://snapshot.debian.org\`"
  echo "  pinned to the package version below, or \`apt-get source <package>\` on a"
  echo "  trixie host. Per-package copyright and licence text also ships inside each"
  echo "  image at \`/usr/share/doc/<package>/copyright\`."
  echo "- **Alpine** (\`alpine:3.24\`): \`https://gitlab.alpinelinux.org/alpine/aports\`"
  echo "  at the matching aport version."
  echo "- **asciinema** (installed by Wardyn's own Dockerfile, not inherited):"
  echo "  \`https://github.com/asciinema/asciinema\` at the version below."
  if [ "$modified_total" -gt 0 ]; then
    echo "  Where an image section says Wardyn modified a package, that section also"
    echo "  states exactly what changed."
  fi
  echo "- **npm packages bundled inside a vendor tool's own release tarball**"
  echo "  (\`agent-vscode\`'s code-server bundles \`jschardet\`, for example): the"
  echo "  package's own entry at \`https://registry.npmjs.org/<name>\`, or the"
  echo "  upstream repository linked from that page. Wardyn's Dockerfile installs"
  echo "  the vendor tarball whole; it does not install these individually."
  echo "- **Source tarballs a Dockerfile installs directly, outside any package"
  echo "  manager** (\`agent-novnc\`'s \`websockify\`, for instance): syft cannot see"
  echo "  these, so they are listed by hand per image below, with the exact tagged"
  echo "  archive URL Wardyn's own Dockerfile pins."
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
    syft_n=$(jq -r '[.artifacts[] | select(((.licenses//[])|map(.value//.spdxExpression//"")|join(" "))|test("GPL";"i"))] | length' "$f")

    # This image's manual supplements (see MANUAL_ENTRIES above), if any.
    manual_rows=()
    for m in "${MANUAL_ENTRIES[@]:-}"; do
      [ -n "$m" ] || continue
      IFS='|' read -r mimg mpkg mver mlic murl <<<"$m"
      [ "$mimg" = "$img" ] && manual_rows+=("$mpkg|$mver|$mlic|$murl")
    done
    n=$((syft_n + ${#manual_rows[@]}))

    if [ "${IS_BOOTSTRAP[$img]}" = 1 ]; then
      # No tag exists to name — this image has never been published, so a
      # fabricated "${img}:${TAG}" header would claim a ref nobody can pull.
      echo "## \`ghcr.io/cjohnstoniv/${img}\` (not yet published)"
      echo
      echo "_Scanned before publication, from \`wardyn/${img}:local\` built at commit"
      echo "\`${BUILD_REV[$img]}\` — the same recipe this image will publish from once it"
      echo "lands, though apt package versions can float between rebuilds of that"
      echo "recipe. No published digest exists yet to scan; this section will be"
      echo "replaced by a digest scan of the real tag once one exists._"
      echo
    else
      echo "## \`ghcr.io/cjohnstoniv/${img}:${TAG}\`"
      echo
    fi
    echo "$n package(s) carrying a GPL or LGPL term."
    echo
    if [ "$syft_n" -gt 0 ]; then
      echo "| package | version | licence | type |"
      echo "|---|---|---|---|"
      jq -r '.artifacts[]
             | select(((.licenses//[])|map(.value//.spdxExpression//"")|join(" "))|test("GPL";"i"))
             | [.name, .version, (((.licenses//[])|map(.value//.spdxExpression//"")|join(", "))), .type]
             | @tsv' "$f" \
        | sort -u | while IFS=$'\t' read -r n v l t; do printf '| `%s` | %s | %s | %s |\n' "$n" "$v" "$l" "$t"; done
      echo
    fi
    if [ "${#manual_rows[@]}" -gt 0 ]; then
      echo "_Also conveyed, not visible to the SBOM scan (installed from a source"
      echo "tarball with no package manifest for syft to read — see this script's"
      echo "MANUAL_ENTRIES):_"
      echo
      echo "| package | version | licence | source |"
      echo "|---|---|---|---|"
      for row in "${manual_rows[@]}"; do
        IFS='|' read -r mpkg mver mlic murl <<<"$row"
        printf '| `%s` | %s | %s | `%s` |\n' "$mpkg" "$mver" "$mlic" "$murl"
      done
      echo
    fi
    if [ -n "${MODIFIED[$img]}" ]; then
      while IFS='|' read -r mpkg msuffix mbefore mafter mdf; do
        echo "**Modified by Wardyn: \`${mpkg}\`.** Wardyn rebuilds Debian's \`${mpkg}\` package"
        echo "with changes to its control file and nothing else; the packaged files are"
        echo "Debian's, byte for byte:"
        echo
        if [ -n "$mbefore" ]; then
          dropped=$(comm -23 <(tr -d ' ' <<<"$mbefore" | tr ',' '\n' | sort) <(tr -d ' ' <<<"$mafter" | tr ',' '\n' | sort) | paste -sd, - | sed 's/,/, /g')
          echo "- \`Depends:\` changed from \`${mbefore}\` to \`${mafter}\`${dropped:+ (dropped \`$dropped\`)}."
        fi
        echo "- \`Version:\` gained the suffix \`${msuffix}\`, so the dpkg record names a"
        echo "  package that is not Debian's own."
        echo
        exact=$(jq -r --arg p "$mpkg" '[.artifacts[] | select(.name == $p and ((.version // "") | test("\\+wardyn[0-9]+$")))] | first | .version // ""' "$f")
        if [ -n "$exact" ]; then
          echo "This image carries \`${mpkg} ${exact}\`, which is Debian's \`${mpkg} ${exact%"$msuffix"}\`"
          echo "with those two edits."
        else
          echo "The installed version is Debian's package version followed by \`${msuffix}\`."
          echo "If the table above lists \`${mpkg}\` without that suffix, the table was"
          echo "scanned from an image built before this change; images built from this"
          echo "repository's recipe carry the modified package."
        fi
        echo "Remove the suffix to get the Debian version, whose corresponding source is"
        echo "\`apt-get source ${mpkg}=<that version>\` on a trixie host, or"
        echo "\`https://snapshot.debian.org\` pinned to it. The recipe that makes the edits is"
        echo "the \`RUN\` in \`${mdf}\` that sets the \`${msuffix}\` version, in"
        echo "\`https://github.com/cjohnstoniv/wardyn\`."
        echo
      done <<<"${MODIFIED[$img]}"
    fi
  done

  # ── frozen history (see HISTORICAL_FILE above) ────────────────────────────
  cat "$HISTORICAL_FILE"
} > "$OUT"
echo "wrote $OUT ($(wc -l < "$OUT") lines)"
