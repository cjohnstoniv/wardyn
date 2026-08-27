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

# ── 6b. the listeners, and the CLI that uses them ────────────────────────
# install.sh wrote WARDYN_SSH_PORT and WARDYN_UI_SANDBOX_PORT but no _LISTEN
# vars. The PORT vars only publish a host port; the LISTEN vars are what start a
# listener, and empty means off — not even a generated host key. So the one-line
# install shipped published ports that refused every connection, while the docs
# promised a governed sandbox reachable from your own terminal.
grep -qE '^WARDYN_SSH_LISTEN=' "$INSTALL" \
  || fail "$INSTALL writes no WARDYN_SSH_LISTEN — compose still publishes 2222, so the install ships a port that refuses every connection"
grep -qE '^WARDYN_SSH_ADVERTISE=' "$INSTALL" \
  || fail "$INSTALL writes no WARDYN_SSH_ADVERTISE — the console's attach pane then prints no usable ssh command"

# It also installed NO host binary, so `wardyn ssh <run-id>` had no client on
# the very machine that enables the gateway: the only command path was
# `docker compose exec`, which is in-container and root-only.
# Anchored, and BOTH halves: a bare substring match on "install_cli" is
# satisfied by a renamed-out `_disabled_install_cli`, and defining the function
# without calling it installs nothing.
grep -qE '^install_cli\(\) \{' "$INSTALL" \
  || fail "$INSTALL defines no install_cli function — 'wardyn ssh' then has no client on a no-clone box"
grep -qE '^install_cli$' "$INSTALL" \
  || fail "$INSTALL defines install_cli but never calls it — the CLI is never installed"

# The CLI is fetched over the network and put on PATH, so it MUST be verified.
# The release ships a cosign-signed SHA256SUMS; use it, and FAIL CLOSED.
grep -q 'SHA256SUMS' "$INSTALL" \
  || fail "$INSTALL downloads the CLI without consulting SHA256SUMS — it would put an unverified binary on PATH"
grep -q 'checksum mismatch' "$INSTALL" \
  || fail "$INSTALL has no checksum-mismatch refusal — a substituted binary would install silently"
# A mismatch must DIE, not warn-and-continue.
grep -q 'die "checksum mismatch' "$INSTALL" \
  || fail "$INSTALL does not die on a checksum mismatch; installing an unverified binary is worse than having no CLI"
# SHA256SUMS lists names as `<hash>  ./<name>`. A lookup that ignores the ./
# prefix silently finds nothing and skips the CLI (which is how this was first
# written); a SUBSTRING match would let the wrong asset's hash satisfy it.
grep -q 'sub(/\^\\.\\//' "$INSTALL" \
  || fail "$INSTALL's checksum lookup does not strip the leading ./ from SHA256SUMS names — it will match nothing and silently skip the CLI"

# ── 7. the agent images it seeds are ones we actually publish ────────────
# install.sh seeded WARDYN_AGENT_IMAGES with codex-cli and aws-sso only. That
# left `claude-code` — the catalog's first row and the default agent — resolving
# through the ghcr convention fallback, which pointed at the unpublished
# agent-claude-code. On the one-line install that meant only ONE of three agent
# names worked.
grep -q '"claude-code"' "$INSTALL" \
  || fail "$INSTALL seeds no claude-code entry in WARDYN_AGENT_IMAGES — the catalog's default agent then resolves through the ghcr fallback"
grep -qE 'ghcr\.io/[^"]*/agent-claude-code' "$INSTALL" \
  && fail "$INSTALL pins agent-claude-code, which this project does not publish — it 404s on every install"
grep -q 'agent-base' "$INSTALL" \
  || fail "$INSTALL never names agent-base, the image published in agent-claude-code's place"

echo "test-install-sh: PASS (install.sh + ${#READMES[@]} documented install blocks)"
