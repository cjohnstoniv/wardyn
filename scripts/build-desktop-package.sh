#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# build-desktop-package.sh — build the desktop tier's MDM-distributable payload
# as a .deb and a tarball.
#
#   scripts/build-desktop-package.sh [--version X.Y.Z] [--out DIR]
#
# 🔴 THE PAYLOAD IS BUILT FROM A CLEAN GIT TREE, NEVER BY COPYING THE WORKING
# DIRECTORY. deploy/compose/.env is a real file on a maintainer's box — 0600,
# gitignored, carrying a LIVE WARDYN_AGE_KEY. "Copy the compose dir" copies it;
# packaging tools normalize modes; and then the maintainer's age identity lands
# on every managed laptop, making every device's secret store decryptable by
# anyone holding the package. docs/DESKTOP.md is explicit that age.key travels
# "never via MDM". `git archive` cannot pick up an untracked or ignored file, so
# the guarantee is structural rather than a rule someone has to remember. The
# assertion below is the belt to that braces.
#
# LAYOUT — the `deploy/` level is LOAD-BEARING, not tidiness:
#
#   /usr/local/lib/wardyn/deploy/desktop/     wardyn-desktop.sh, install.sh, units
#   /usr/local/lib/wardyn/deploy/compose/     the included stack + dex.yaml + tetragon
#   /usr/local/lib/wardyn/scripts/lib/        common.sh
#   /usr/local/bin/wardyn                     the CLI
#
# wardyn-desktop.sh computes REPO_ROOT as "../.." from its own location. Rooted
# at /usr/local/lib/wardyn/desktop/ that resolves to /usr/local/lib — the wrapper
# would source /usr/local/lib/scripts/lib/common.sh (nothing there) and die at
# its own line 22 under `set -euo pipefail`. With `deploy/` present it lands on
# /usr/local/lib/wardyn and every path resolves WITH NO CODE CHANGE.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION=""
OUT="${ROOT}/dist"
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    *) echo "usage: $0 [--version X.Y.Z] [--out DIR]" >&2; exit 1 ;;
  esac
done
[ -n "$VERSION" ] || VERSION="$(sed -n 's/.*Version = "\([^"]*\)".*/\1/p' internal/version/version.go | head -1)"
[ -n "$VERSION" ] || { echo "could not resolve a version; pass --version" >&2; exit 1; }

PKG="wardyn-desktop"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
PAYLOAD="${STAGE}/root"
PREFIX="${PAYLOAD}/usr/local/lib/wardyn"
mkdir -p "${PREFIX}" "${PAYLOAD}/usr/local/bin" "${OUT}"

echo "==> Exporting a CLEAN tree at HEAD (never the working directory)"
git archive HEAD deploy/desktop deploy/compose scripts/lib/common.sh | tar -x -C "${PREFIX}"

# ── the assertion that makes the guarantee checkable ───────────────────────
# `-name '.env'` explicitly: a bare `*.env` shell glob does NOT match a file
# literally named `.env`, which is the ONLY one this exists to stop.
echo "==> Asserting no env file reached the payload"
leaked="$(find "${PAYLOAD}" \( -name '.env' -o -name '*.env' \) ! -name '*.example' -print)"
if [ -n "${leaked}" ]; then
  echo "REFUSING TO PACKAGE — an env file reached the payload:" >&2
  printf '  %s\n' ${leaked} >&2
  echo "  deploy/compose/.env carries a LIVE WARDYN_AGE_KEY. Shipping it makes every" >&2
  echo "  device's secret store decryptable by anyone holding this package." >&2
  exit 1
fi
# Same for a stray age key under any filename. Match the KEY SHAPE, not the bare
# prefix: `AGE-SECRET-KEY-` appears legitimately in prose all over the payload
# (deploy/compose/README.md, both wardyn.env.*.example, deploy/desktop/install.sh)
# where it names the variable rather than carrying a value. A real bech32 age
# identity is the prefix plus ~58 chars of base32 — that is what must never ship.
if grep -rElq 'AGE-SECRET-KEY-1[0-9A-Z]{50,}' "${PAYLOAD}" 2>/dev/null; then
  echo "REFUSING TO PACKAGE — a real age identity is present in the payload:" >&2
  grep -rEl 'AGE-SECRET-KEY-1[0-9A-Z]{50,}' "${PAYLOAD}" 2>/dev/null | sed 's/^/  /' >&2
  exit 1
fi

# ── the CLI ────────────────────────────────────────────────────────────────
# The tier installed NO host binary: the only command path was `compose exec`,
# which is in-container and root-only, so `wardyn ssh` had no client and
# `wardyn secret set` (A5's own remedy) was unreachable.
echo "==> Building the wardyn CLI"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "${PAYLOAD}/usr/local/bin/wardyn" ./cmd/wardyn
chmod 0755 "${PAYLOAD}/usr/local/bin/wardyn"
chmod 0755 "${PREFIX}/deploy/desktop/"*.sh

# ── tarball ────────────────────────────────────────────────────────────────
TARBALL="${OUT}/${PKG}-${VERSION}.tar.gz"
tar -C "${PAYLOAD}" -czf "${TARBALL}" .
echo "==> ${TARBALL}"

# ── .deb ───────────────────────────────────────────────────────────────────
if ! command -v dpkg-deb >/dev/null 2>&1; then
  echo "==> dpkg-deb absent — tarball only. Unpack it at / and run"
  echo "    /usr/local/lib/wardyn/deploy/desktop/install.sh"
  exit 0
fi
mkdir -p "${PAYLOAD}/DEBIAN"
cat > "${PAYLOAD}/DEBIAN/control" <<CONTROL
Package: ${PKG}
Version: ${VERSION}
Section: devel
Priority: optional
Architecture: all
Maintainer: The Wardyn Authors <noreply@github.com>
Depends: docker.io | docker-ce | podman
Description: Wardyn desktop tier — governed agent sandboxes on a managed laptop
 Installs the compose payload, the converge job (systemd timer) and the wardyn
 CLI. It does NOT deliver the MDM envelope: /etc/wardyn/wardyn.env, secret.env,
 policy.json and site-config.json are the management plane's files, and
 age.key is minted per-device by the postinstall and never travels in a payload.
CONTROL
cat > "${PAYLOAD}/DEBIAN/postinst" <<'POSTINST'
#!/bin/sh
set -e
[ "$1" = "configure" ] || exit 0
# Mints age.key (per-device, never from a payload) and registers the timer.
/usr/local/lib/wardyn/deploy/desktop/install.sh
POSTINST
cat > "${PAYLOAD}/DEBIAN/prerm" <<'PRERM'
#!/bin/sh
set -e
# KEEPS /etc/wardyn (age.key + envelope) and the Postgres volume: a re-install
# recovers the device. `install.sh --uninstall --purge` destroys them, and that
# stays a deliberate operator act rather than a side effect of apt remove.
[ -x /usr/local/lib/wardyn/deploy/desktop/install.sh ] && \
  /usr/local/lib/wardyn/deploy/desktop/install.sh --uninstall || true
PRERM
chmod 0755 "${PAYLOAD}/DEBIAN/postinst" "${PAYLOAD}/DEBIAN/prerm"
DEB="${OUT}/${PKG}_${VERSION}_all.deb"
fakeroot dpkg-deb --build "${PAYLOAD}" "${DEB}" >/dev/null
echo "==> ${DEB}"
dpkg-deb --contents "${DEB}" | awk '{print $NF}' | grep -cE '^\./' | sed 's/^/    entries: /'
