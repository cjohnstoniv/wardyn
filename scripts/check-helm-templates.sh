#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-helm-templates.sh — every file under the chart's templates/ is a
# *.yaml, a *.tpl or NOTES.txt.
#
# Helm renders every file under templates/ as a manifest, whatever its
# extension, and the chart has no .helmignore: `helm template` installs a
# NetworkPolicy written in templates/notes.md exactly as it would from a
# .yaml (measured on Helm 3.17.2). A file there that does not look like a
# template is therefore a manifest a reviewer is likely to read as prose, and
# one that a path rule keyed on ".md" would class as documentation and skip
# every job that renders or installs the chart.
#
# `helm lint` rejects some of these on its own, but it accepts .yml and any
# .txt, and what it accepts is Helm's to change. This gate needs no helm
# binary, so `make lint` runs it on every change, whatever ci.yml's `changes`
# job made of the paths; `make helm-lint` runs it again before it renders.
set -euo pipefail
cd "$(dirname "$0")/.."

dir=deploy/helm/wardyn/templates
[ -d "$dir" ] || { echo "FAIL: $dir does not exist — this gate would check nothing" >&2; exit 1; }

# `! -type d`, not `-type f`: a symlink or any other entry is a file Helm may load.
stray="$(find "$dir" ! -type d ! -name '*.yaml' ! -name '*.tpl' ! -name NOTES.txt | LC_ALL=C sort)"
if [ -n "$stray" ]; then
  echo "FAIL: only *.yaml, *.tpl and NOTES.txt belong under $dir. Helm renders every file there as a manifest, whatever its extension:" >&2
  printf '%s\n' "$stray" | sed 's/^/  /' >&2
  echo "Move documentation out of templates/ (the chart's README.md is beside it), and name a template .yaml." >&2
  exit 1
fi
echo "helm templates: every file under $dir is a *.yaml, a *.tpl or NOTES.txt"
