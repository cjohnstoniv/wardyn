#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Generate (or verify) THIRD-PARTY-NOTICES.md and licenses/texts/.
#
# WHY THIS IS CHECKED IN. MIT, BSD, ISC and OFL-1.1 all require their copyright and
# permission notice to accompany the distribution. Wardyn distributes statically
# linked Go binaries and container images; those are copies. Telling a recipient to
# run `go-licenses report ./...` themselves moves our obligation onto them, which is
# not compliance. The generated files ship with the artifact.
#
# SCOPE IS ./cmd/..., NOT ./... — a notice describes what is DISTRIBUTED, not what
# the test suite imports. The merge gate (`make licenses`) is the one that runs over
# everything.
#
# DETERMINISM. Output must be byte-identical between a laptop and CI or the drift
# gate is useless: LC_ALL=C sort everywhere, no timestamps, no hostnames, and never
# emit pnpm's `paths[]` (they are absolute — /home/cjohn/... locally, /home/runner/...
# in CI).
#
# PACKAGES THAT PUBLISH NO LICENCE FILE. ~36 of the runtime UI dependencies (the
# @radix-ui family, react-remove-scroll-bar) declare a licence in package.json but
# ship no LICENCE file in the npm tarball. Harvesting text from the installed tree
# alone would silently emit nothing for a third of the runtime deps. They are listed
# explicitly with their declared licence and copyright holder, and the canonical text
# for that licence id is included once under licenses/texts/common/. Silence is not
# an option here; an empty section is.
set -euo pipefail
cd "$(dirname "$0")/.."
export LC_ALL=C

GO_LICENSES_VERSION="$(grep -oP '^GO_LICENSES_VERSION\s*\?=\s*\K\S+' Makefile)"
NOTICES=THIRD-PARTY-NOTICES.md
TEXTS=licenses/texts
mode="${1:---check}"

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

# ── Go: the production build is -tags docker,k8s ─────────────────────────────
GOFLAGS=-tags=docker,k8s go run "github.com/google/go-licenses@${GO_LICENSES_VERSION}" \
  report ./cmd/... > "$tmp/go.csv" 2>/dev/null
GOFLAGS=-tags=docker,k8s go run "github.com/google/go-licenses@${GO_LICENSES_VERSION}" \
  save ./cmd/... --save_path="$tmp/gotexts" --force >/dev/null 2>&1

# ── UI: prod dependencies only ───────────────────────────────────────────────
(cd ui && pnpm licenses list --prod --json) > "$tmp/ui.json"
n_ui=$(jq '[.[][]] | length' "$tmp/ui.json")
[ "$n_ui" -ge 1 ] || { echo "ERROR: examined 0 production UI packages — is ui/node_modules installed? Failing closed."; exit 1; }

rm -rf "$tmp/out"; mkdir -p "$tmp/out/go" "$tmp/out/npm" "$tmp/out/common"
cp -r "$tmp/gotexts/." "$tmp/out/go/" 2>/dev/null || true
# go-licenses save nests under the module path; drop our own module.
rm -rf "$tmp/out/go/github.com/cjohnstoniv/wardyn"

# npm texts, plus the roster of packages that ship none
: > "$tmp/ui-missing.tsv"
jq -r '.[][] | [.name, ((.versions // [.version]) | map(tostring) | join(", ")), (.license // ""), (.author // ""), (.homepage // ""), (.paths[0] // "")] | @tsv' "$tmp/ui.json" \
| sort > "$tmp/ui.tsv"
while IFS=$'\t' read -r name vers lic author home path; do
  src=""
  if [ -n "$path" ]; then
    src=$(ls "$path"/LICENSE* "$path"/LICENCE* "$path"/license* 2>/dev/null | head -1 || true)
  fi
  if [ -n "$src" ]; then
    mkdir -p "$tmp/out/npm/$name"; cp "$src" "$tmp/out/npm/$name/LICENSE"
  else
    printf '%s\t%s\t%s\t%s\n' "$name" "$vers" "$lic" "${author:-$home}" >> "$tmp/ui-missing.tsv"
  fi
done < "$tmp/ui.tsv"

# Canonical text for every licence id in use, so a package with no own text is
# still fully covered. Sourced from licenses/canonical/ (checked in, offline).
while IFS= read -r id; do
  [ -f "licenses/canonical/$id.txt" ] && cp "licenses/canonical/$id.txt" "$tmp/out/common/$id.txt"
done < <( { cut -d, -f3 "$tmp/go.csv"; jq -r '.[][] | .license // empty' "$tmp/ui.json"; } \
          | tr ',' '\n' | sed -E 's/^[[:space:]]+|[[:space:]]+$//g' | grep -v '^$' | sort -u )

# ── the index ────────────────────────────────────────────────────────────────
{
  echo "# Third-party notices"
  echo
  echo "Wardyn is Apache-2.0. This file lists the third-party components distributed"
  echo "with it and the licences they are distributed under. Verbatim licence texts are"
  echo "in \`licenses/texts/\`. Regenerate with \`make notices ARGS=fix\`; CI fails on drift."
  echo
  echo "## Go modules (compiled into the shipped binaries)"
  echo
  echo "Scope: reachable from \`./cmd/...\` under the production build tags \`docker,k8s\`."
  echo
  echo "| module | licence | source |"
  echo "|---|---|---|"
  sort -t, -k1,1 "$tmp/go.csv" | while IFS=, read -r mod url lic; do
    [ "$mod" = "github.com/cjohnstoniv/wardyn" ] && continue
    printf '| `%s` | %s | %s |\n' "$mod" "$lic" "$url"
  done
  echo
  echo "## UI packages (bundled into the console shipped inside wardynd)"
  echo
  echo "| package | version | licence |"
  echo "|---|---|---|"
  while IFS=$'\t' read -r name vers lic _ _ _; do
    printf '| `%s` | %s | %s |\n' "$name" "$vers" "$lic"
  done < "$tmp/ui.tsv"
  echo
  echo "## Fonts"
  echo
  echo "The console bundles the Inter and JetBrains Mono typefaces (\`@fontsource/inter\`,"
  echo "\`@fontsource/jetbrains-mono\`), both under the SIL Open Font License 1.1. The OFL"
  echo "text is in \`licenses/texts/common/OFL-1.1.txt\` and ships alongside the fonts in"
  echo "the built console. Reserved Font Names must not be reused by derived works."
  echo
  echo "## UI packages that publish no licence file"
  echo
  echo "These declare a licence in \`package.json\` but ship no licence file in their npm"
  echo "tarball. The declared licence and copyright holder are recorded here, and the"
  echo "canonical text for that licence is in \`licenses/texts/common/\`."
  echo
  if [ -s "$tmp/ui-missing.tsv" ]; then
    echo "| package | version | declared licence | copyright holder |"
    echo "|---|---|---|---|"
    sort "$tmp/ui-missing.tsv" | while IFS=$'\t' read -r n v l a; do
      printf '| `%s` | %s | %s | %s |\n' "$n" "$v" "$l" "$a"
    done
  else
    echo "None."
  fi
  echo
  echo "## In-tree components derived from third-party projects"
  echo
  echo "Not dependencies — third-party source adapted into this repository, and therefore"
  echo "carrying that upstream project's notice-retention obligation into every build."
  echo
  echo "| path | upstream | licence |"
  echo "|---|---|---|"
  echo "| \`ui/src/app/components/ui/\` | shadcn/ui — https://ui.shadcn.com | MIT, Copyright (c) 2023 shadcn |"
  echo "| \`ui/e2e/demo/assets/player/\` | asciinema-player 3.16.0 | Apache-2.0 |"
  echo
  echo "## Components invoked as separate processes, not linked"
  echo
  echo "The agent container images apt-install \`asciinema\` (GPL-3.0), which \`wardyn-rec\`"
  echo "executes as a subprocess and never links. Publishing those images nonetheless"
  echo "conveys that binary; see \`deploy/images/THIRD-PARTY-GPL.md\` for the corresponding"
  echo "source offer covering it and every other GPL/LGPL package in the base images."
} > "$tmp/NOTICES.md"

if [ "$mode" = "--fix" ]; then
  rm -rf "$TEXTS"; mkdir -p licenses
  cp -r "$tmp/out" "$TEXTS"
  mv "$tmp/NOTICES.md" "$NOTICES"
  echo "notices: regenerated $NOTICES and $TEXTS/"
  exit 0
fi

# --check: regenerate into place, then let the caller's git status catch drift.
rm -rf "$TEXTS"; mkdir -p licenses
cp -r "$tmp/out" "$TEXTS"
mv "$tmp/NOTICES.md" "$NOTICES"

# git status, NOT git diff: `git diff` cannot see untracked files, so a brand-new
# dependency's brand-new licence text would produce a GREEN gate. That single case
# is the entire reason this gate exists.
drift=$(git status --porcelain --untracked-files=all -- "$NOTICES" "$TEXTS")
if [ -n "$drift" ]; then
  echo "ERROR: third-party notices are out of date. Run: make notices ARGS=fix"
  echo "$drift"
  exit 1
fi
echo "notices: up to date ($(grep -c '^| `' "$NOTICES") entries)"
