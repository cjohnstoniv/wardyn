#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Which trivy-scanned images does a pull request's change reach?
#
#   git diff --name-only --no-renames HEAD^1 HEAD | scripts/image-scope.sh
#
# prints `<image>=true|false` for every image ci.yml's trivy jobs scan (their
# TRIVY_IMAGE / TRIVY_DOCKERFILE env is the list, so a tenth job is picked up
# without editing this file). `true` means the scan must run. The reason for
# each answer goes to stderr.
#
# An image is reached by a changed path that is one of:
#   - the Dockerfile, or the Dockerfile of a local base it builds FROM
#     (`wardyn/<image>:local`: agent-vscode and agent-novnc sit on agent-base);
#   - a path those Dockerfiles COPY (a build-stage COPY of `/build/<path>` counts;
#     `COPY . .` is the Go builder's and is decided by the next rule);
#   - a non-test file in the directory of a Go package in the `go list -deps`
#     closure of a binary those Dockerfiles `go build`, or a file such a package
#     embeds. The closure is read from the tree under test, so a new import
#     changes it, and it is taken for the tags and platforms the Dockerfiles
#     build (none, and docker,k8s; linux and darwin, amd64 and arm64);
#   - go.mod, go.sum, deploy/base-images.txt, .trivyignore, .dockerignore, ci.yml
#     (the scan jobs live there), the mirror action, or this script.
# ui/ is not an input, as before: the images carry the console's built bundle,
# not its npm packages, and trivy reads packages.
#
# Anything this script cannot read or parse answers `true` for every image.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

CI_YML=.github/workflows/ci.yml

if [ -z "${IMAGE_SCOPE_INNER:-}" ]; then
  changed="$(cat)"
  # Re-run as a child so a failure anywhere (set -e is off inside a function
  # called from `if`) falls back to scanning everything, loudly.
  if out="$(IMAGE_SCOPE_INNER=1 bash "$0" <<<"$changed")" && [ -n "$out" ]; then
    printf '%s\n' "$out"
    exit 0
  fi
  echo "::warning title=image-scope failed::scanning every image" >&2
  awk '/^ +TRIVY_IMAGE:/ {print $2 "=true"}' "$CI_YML"
  exit 0
fi

mapfile -t changed_files < <(cat)
mapfile -t images < <(awk '/^ +TRIVY_IMAGE:/ {i=$2} /^ +TRIVY_DOCKERFILE:/ {print i " " $2}' "$CI_YML")
[ "${#images[@]}" -gt 0 ] || { echo "no TRIVY_IMAGE jobs in $CI_YML" >&2; exit 1; }
declare -A dockerfile_of
for pair in "${images[@]}"; do dockerfile_of[${pair%% *}]=${pair#* }; done

always=(go.mod go.sum deploy/base-images.txt .trivyignore .dockerignore "$CI_YML" scripts/image-scope.sh)

# The paths a Dockerfile COPYs or ADDs from the build context.
copy_sources() {
  if grep -Eq '^[[:space:]]*(COPY|ADD)[[:space:]]+\[' "$1"; then
    echo "$1: JSON-form COPY is not parsed" >&2; return 1
  fi
  awk '
    /^[[:space:]]*(COPY|ADD)[[:space:]]/ {
      from = ""; n = 0
      for (i = 2; i <= NF; i++) {
        if ($i ~ /^--from=/) { from = substr($i, 8); continue }
        if ($i ~ /^--/) continue
        src[++n] = $i
      }
      for (i = 1; i < n; i++) {      # the last word is the destination
        if (from == "") print src[i]
        else if (from == "builder" && src[i] ~ /^\/build\//) print substr(src[i], 8)
      }
    }' "$1"
}

# The module-local packages a set of main packages links, as `D <dir>` and
# `E <embedded file>` lines. GOPROXY=off and -e: third-party packages are
# not resolved (a module change is go.mod/go.sum) but the local ones are, so no
# module download is needed.
go_closure() {
  local tags plat
  for tags in "" "docker,k8s"; do
    for plat in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
      GOOS=${plat%/*} GOARCH=${plat#*/} CGO_ENABLED=0 GOPROXY=off GOFLAGS="${GOFLAGS:-} -mod=readonly" \
        go list -e -tags "$tags" -deps \
        -f '{{with .Module}}{{if .Main}}D {{$.Dir}}{{"\n"}}{{range $.EmbedFiles}}E {{$.Dir}}/{{.}}{{"\n"}}{{end}}{{end}}{{end}}' \
        "$@" 2>/dev/null || return 1
    done
  done
}

root="$PWD/"
for img in "${!dockerfile_of[@]}"; do
  # IMAGE_SCOPE_ONLY="a b": the guard test asks about one or two images only.
  [ -z "${IMAGE_SCOPE_ONLY:-}" ] || [[ " $IMAGE_SCOPE_ONLY " == *" $img "* ]] || continue
  # The Dockerfile closure: this image's, plus each local base it builds FROM.
  files=("${dockerfile_of[$img]}")
  for ((k = 0; k < ${#files[@]}; k++)); do
    while read -r base; do
      [ -n "$base" ] || continue
      [ -n "${dockerfile_of[$base]:-}" ] || { echo "${files[k]} builds FROM wardyn/$base:local, which no trivy job scans" >&2; exit 1; }
      [[ " ${files[*]} " == *" ${dockerfile_of[$base]} "* ]] || files+=("${dockerfile_of[$base]}")
    done < <(grep -Ev '^[[:space:]]*#' "${files[k]}" | grep -Eo 'wardyn/[a-z0-9-]+:local' | sed -E 's#wardyn/(.*):local#\1#' | sort -u || true)
  done

  copies=() mains=()
  for df in "${files[@]}"; do
    [ -f "$df" ] || { echo "$df: no such Dockerfile" >&2; exit 1; }
    srcs="$(copy_sources "$df")"
    mapfile -t src <<<"$srcs"
    for s in "${src[@]}"; do
      case "$s" in ''|.|./|ui|ui/*) ;; *) copies+=("$s") ;; esac
    done
    mapfile -t m < <(grep -Ev '^[[:space:]]*#' "$df" | grep -E 'go build' | grep -Eo '\./cmd/[A-Za-z0-9_-]+' || true)
    mains+=("${m[@]}")
  done

  mapfile -t mains < <(printf '%s\n' "${mains[@]}" | sort -u)
  declare -A dirs=() embeds=()
  if [ "${#mains[@]}" -gt 0 ]; then
    closure="$(go_closure "${mains[@]}")"
    while read -r kind path; do
      path=${path#"$root"}
      if [ "$kind" = D ]; then dirs[$path]=1; else embeds[$path]=1; fi
    done <<<"$closure"
    [ "${#dirs[@]}" -gt 0 ] || { echo "$img: go list found no local package for ${mains[*]}" >&2; exit 1; }
  fi

  # The reason a changed path reaches this image, or nothing.
  reach() {
    local f=$1 a df s d
    case "$f" in .github/actions/base-image-mirror/*) echo "$f (shared input)"; return 0 ;; esac
    for a in "${always[@]}"; do [ "$f" = "$a" ] && { echo "$f (shared input)"; return 0; }; done
    for df in "${files[@]}"; do [ "$f" = "$df" ] && { echo "$f (Dockerfile)"; return 0; }; done
    for s in "${copies[@]}"; do
      # shellcheck disable=SC2254 # $s is the pattern: a COPY source may be a glob
      case "$f" in $s|"$s"/*) echo "$f (COPY $s)"; return 0 ;; esac
      [[ "$s" == */ && "$f" == "$s"* ]] && { echo "$f (COPY $s)"; return 0; }
    done
    case "$f" in *_test.go) return 0 ;; esac
    d=${f%/*}; [ "$d" = "$f" ] && d=.
    if [ -n "${dirs[$d]:-}" ] || [ -n "${embeds[$f]:-}" ]; then echo "$f (Go package of ${mains[*]})"; fi
  }

  reason=""
  for f in "${changed_files[@]}"; do
    [ -n "$f" ] || continue
    r="$(reach "$f")"
    # IMAGE_SCOPE_PER_FILE=1: one row per image and path, for the guard test.
    if [ -n "${IMAGE_SCOPE_PER_FILE:-}" ]; then echo "row $img $([ -n "$r" ] && echo true || echo false) $f"; continue; fi
    if [ -n "$r" ]; then reason=$r; break; fi
  done
  unset dirs embeds
  [ -z "${IMAGE_SCOPE_PER_FILE:-}" ] || continue
  if [ -n "$reason" ]; then
    echo "scan $img: $reason" >&2; echo "$img=true"
  else
    echo "skip $img: no input changed" >&2; echo "$img=false"
  fi
done | sort
