#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-install-sh.sh — the root install.sh and the two documented install
# blocks resolve a version that actually exists, and refuse rather than guess.
#
# Why this exists: install.sh shipped for two releases resolving its version
# from `releases/latest`, which EXCLUDES pre-releases. Every Wardyn release is
# one (RELEASING.md mandates --prerelease), so that endpoint returns HTTP 404,
# the sed matched nothing, and the script died on every install. The README's
# two Helm blocks used the same endpoint and failed the OPPOSITE way: the empty
# string went into `helm install --version ""`, which helm accepts as UNPINNED
# and silently resolves to the newest chart — the exact outcome the prose two
# lines above it warns about.
#
# Nothing caught either one. The root install.sh had no lint, no `sh -n` and no
# test: scripts/test-desktop-profile.sh's shellcheck loop covers only
# deploy/desktop/, and no Makefile target or workflow touched the root file —
# though release.yml ships it in the cosign-signed SHA256SUMS.
#
# Daemon-free and network-free: every assertion is against the tracked files.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

fail() { echo "FAIL: $*" >&2; exit 1; }

INSTALL="install.sh"
READMES=("README.md" "deploy/helm/wardyn/README.md")

# ── 1. It is a shell script, and it parses ────────────────────────────────
[ -f "$INSTALL" ] || fail "$INSTALL is missing"
# POSIX sh, deliberately — it runs on a machine we know nothing about.
head -1 "$INSTALL" | grep -q 'env sh$' \
  || fail "$INSTALL is no longer #!/usr/bin/env sh — it runs before anything of ours is installed, so it must not need bash"
sh -n "$INSTALL" || fail "$INSTALL failed sh -n"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -s sh "$INSTALL" || fail "$INSTALL failed shellcheck"
fi

# ── 2. The defect itself: releases/latest 404s on a prerelease-only repo ──
# Scoped to the resolving code, not to prose — the comments explaining the
# 404 legitimately name the endpoint.
grep -n 'api\.github\.com[^"]*releases/latest' "$INSTALL" \
  && fail "$INSTALL resolves a version from releases/latest, which EXCLUDES pre-releases and 404s on this repo"
for f in "${READMES[@]}"; do
  grep -n 'api\.github\.com[^"]*releases/latest' "$f" \
    && fail "$f resolves a version from releases/latest — it 404s, and the empty result makes 'helm --version \"\"' an UNPINNED install"
done

# ── 3. …and that it still resolves SOMETHING ──────────────────────────────
grep -q 'releases?per_page=1' "$INSTALL" \
  || fail "$INSTALL no longer resolves a version from releases?per_page=1"
for f in "${READMES[@]}"; do
  grep -q 'releases?per_page=1' "$f" || fail "$f no longer resolves a version from releases?per_page=1"
done

# ── 4. An unresolved version must REFUSE, never install unpinned ──────────
# The guard has to sit on the helm command itself: a check in a separate fenced
# block is one a reader pastes straight past, and `--version ""` is silently
# accepted as "newest chart".
for f in "${READMES[@]}"; do
  grep -q 'WARDYN_VERSION:?' "$f" \
    || fail "$f's helm install does not guard an empty version (\${WARDYN_VERSION:?...}) — 'helm --version \"\"' silently follows the newest chart"
done
grep -q 'could not resolve the latest release' "$INSTALL" \
  || fail "$INSTALL no longer dies on an unresolvable version"

# ── 5. The README hands out the SIGNED copy, not tip-of-main ──────────────
# release.yml ships install.sh in the cosign-signed SHA256SUMS; curling it from
# `main` means the signed copy is never the executed one.
grep -q 'raw\.githubusercontent\.com/cjohnstoniv/wardyn/main/install\.sh' README.md \
  && fail "README.md curls install.sh from main — nothing signs tip-of-main. Point it at the releases/download/ asset."
grep -q 'releases/download/v[0-9]\+\.[0-9]\+\.[0-9]\+/install\.sh' README.md \
  || fail "README.md's install one-liner does not point at a pinned releases/download/ install.sh"

# ── 6. That pin is a version string, so it must be swept as one ───────────
# RELEASING.md step 1b owns the bump; this only catches the two drifting apart.
readme_v="$(grep -o 'releases/download/v[0-9]\+\.[0-9]\+\.[0-9]\+/install\.sh' README.md | head -1 | sed 's|.*/v\([0-9.]*\)/.*|\1|')"
header_v="$(grep -o 'releases/download/v[0-9]\+\.[0-9]\+\.[0-9]\+/install\.sh' "$INSTALL" | head -1 | sed 's|.*/v\([0-9.]*\)/.*|\1|')"
[ -n "$readme_v" ] || fail "could not read the pinned version out of README.md"
[ -n "$header_v" ] || fail "$INSTALL's header comment lost its pinned install URL"
[ "$readme_v" = "$header_v" ] \
  || fail "README.md pins install.sh at v${readme_v} but ${INSTALL}'s header says v${header_v} — RELEASING.md step 1b sweeps both"

echo "test-install-sh: PASS (install.sh + ${#READMES[@]} documented install blocks)"
