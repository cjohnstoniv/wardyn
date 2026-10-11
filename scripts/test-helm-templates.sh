#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-helm-templates.sh — the chart template-file gate must FAIL on the shapes
# it exists to catch. `make lint` runs it on every change, where a gate that
# only ever passes is worse than none: Helm renders every file under
# templates/ as a manifest whatever its extension, and the chart has no
# .helmignore, so a stray file installs exactly as a .yaml would.
#
# Daemon-free, network-free: a throwaway tree, never the repo's own chart.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/scripts"
cp "$ROOT/scripts/check-helm-templates.sh" "$TMP/scripts/"
REL=deploy/helm/wardyn/templates
DIR="$TMP/$REL"

# The gate's own exit code is the thing under test, so it never aborts the
# fixture: each case asserts on the code it returned and on what it wrote.
run_gate() {
  RC=0
  (cd "$TMP" && ./scripts/check-helm-templates.sh) >"$TMP/out" 2>"$TMP/err" || RC=$?
}
refused() { # <label> <err pattern>
  run_gate
  [[ ${RC} -ne 0 && -s "$TMP/err" ]] && grep -qE "$2" "$TMP/err" \
    || { cat "$TMP/out" "$TMP/err" >&2; die "$1"; }
  echo "ok  $1"
}
passed() { # <label> <stdout pattern>
  run_gate
  [[ ${RC} -eq 0 && ! -s "$TMP/err" ]] && grep -qE "$2" "$TMP/out" \
    || { cat "$TMP/out" "$TMP/err" >&2; die "$1"; }
  echo "ok  $1"
}

rm -rf "$DIR"
refused "a missing templates/ fails instead of checking nothing" "does not exist"
mkdir -p "$DIR"

# The shipped shape: every file is a *.yaml, a *.tpl or NOTES.txt, plus a
# nested directory (find's `-type d` exclusion).
touch "$DIR/deployment.yaml" "$DIR/_helpers.tpl" "$DIR/NOTES.txt"
mkdir -p "$DIR/nested"
touch "$DIR/nested/inner.yaml"
passed "the shipped shape passes" "every file under ${REL} is"

# A file Helm loads as a manifest that reads as prose — the shape this gate
# exists for, measured on Helm 3.17.2.
touch "$DIR/notes.md"
refused "a prose file under templates/ fails" "only \*\.yaml, \*\.tpl and NOTES\.txt belong"
refused "the failure names the stray file" "^  ${REL}/notes\.md$"
rm "$DIR/notes.md"

# helm lint accepts .yml and any .txt; this gate's rule is the extension, not
# Helm's tolerance of it, so none of these may pass.
for stray in notes.yml NOTES.md extra.json .gitkeep; do
  touch "$DIR/$stray"
  run_gate
  [[ ${RC} -ne 0 ]] || { cat "$TMP/out" >&2; die "${stray} under templates/ passed"; }
  echo "ok  a ${stray} under templates/ fails"
  rm "$DIR/$stray"
done

# `! -type d`, not `-type f`: a symlink is a file Helm may load whatever it
# points at, and find does not follow it, so a `-type f` filter would drop it
# from the report entirely.
ln -s deployment.yaml "$DIR/notes.md"
run_gate
[[ ${RC} -ne 0 ]] || { cat "$TMP/out" >&2; die "a symlink under templates/ passed"; }
echo "ok  a symlink under templates/ fails"
rm "$DIR/notes.md"

# Every stray is listed, sorted, so the message is a worklist rather than a
# hint about the first one.
touch "$DIR/zeta.md" "$DIR/mike.md" "$DIR/alpha.md" "$DIR/kilo.md" "$DIR/bravo.md"
run_gate
[[ ${RC} -ne 0 ]] || { cat "$TMP/out" >&2; die "two strays passed"; }
listed="$(grep "^  ${REL}/" "$TMP/err" || true)"
[[ "${listed}" = "  ${REL}/alpha.md
  ${REL}/bravo.md
  ${REL}/kilo.md
  ${REL}/mike.md
  ${REL}/zeta.md" ]] || { echo "${listed}" >&2; die "the strays are not listed sorted"; }
echo "ok  every stray is listed, sorted"
rm "$DIR/zeta.md" "$DIR/mike.md" "$DIR/alpha.md" "$DIR/kilo.md" "$DIR/bravo.md"

# The refusals above are a real signal only if the gate is green again once the
# strays are gone.
passed "the clean tree passes again" "every file under ${REL} is"

log "helm template-file gate: PASS"