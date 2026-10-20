#!/usr/bin/env sh
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# install-cli.sh - install the `wardyn` command-line client for the person
# running it. A release asset (the release job writes its own version into the
# RELEASE_VERSION line below); macOS, Linux and WSL2. Windows: install-cli.ps1.
#
#   curl -fsSL https://github.com/cjohnstoniv/wardyn/releases/download/v<version>/install-cli.sh | sh
#
# This is not install.sh (the platform installer, which starts the stack) and not
# runner/install.sh (the runner wrapper inside wardyn-runner-units.tar.gz). It
# installs `wardyn` and nothing else: the runner comes with `wardyn runner setup`.
#
# Options:
#   --version <x.y.z>        install this release instead of this script's own
#   --artifact-base <url>    fetch <url>/v<version>/<asset> instead of the GitHub
#                            release (an internal mirror); https only. Env
#                            WARDYN_ARTIFACT_BASE is read when the flag is absent,
#                            so `... | WARDYN_ARTIFACT_BASE=<url> sh` works.
#   --ca-file <pem>          trust this CA file for these downloads only (curl
#                            --cacert, which replaces curl's default roots for
#                            them; SSL_CERT_FILE for cosign alone). Not kept, not
#                            copied. curl honours HTTPS_PROXY and NO_PROXY itself.
#   --require-signature      refuse unless cosign verified the checksum file
#   --org <url>              print the exact `wardyn login <url>` line at the end
#
# It installs user-locally, never with sudo, and refuses to run as root: into the
# first writable directory of /usr/local/bin, ~/.local/bin and ~/bin, by writing
# a sibling file and renaming it over the target (never in place, so a re-run is
# an upgrade and a running signed binary on macOS is not overwritten).
#
# It never signs in, never writes a token and never writes the CLI profile.
#
# Env WARDYN_INSTALL_CLI_DIRS (space-separated directories) replaces the three
# candidate directories; it exists for the tests.
#
# ponytail: sh, not bash - this runs before anything of ours is installed.
set -eu

REPO=cjohnstoniv/wardyn
# The release job rewrites this one line (and only this line).
RELEASE_VERSION="@WARDYN_VERSION@"
ISSUER=https://token.actions.githubusercontent.com

say() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

version="" base="${WARDYN_ARTIFACT_BASE:-}" cafile="" require_sig="" org=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version|--artifact-base|--ca-file|--org)
      [ $# -ge 2 ] || die "$1 needs a value"
      val="$2"; opt="$1"; shift 2 ;;
    --version=*|--artifact-base=*|--ca-file=*|--org=*)
      val="${1#*=}"; opt="${1%%=*}"; shift ;;
    --require-signature) require_sig=1; shift; continue ;;
    -h|--help)
      echo "usage: install-cli.sh [--version X.Y.Z] [--artifact-base URL] [--ca-file PEM] [--require-signature] [--org URL]"
      echo "Installs the wardyn CLI for you, without sudo. Options are described at the top of this script."
      exit 0 ;;
    *) die "unknown option: $1 (see --help)" ;;
  esac
  case "$opt" in
    --version) version="$val" ;;
    # The flag wins over WARDYN_ARTIFACT_BASE.
    --artifact-base) base="$val" ;;
    --ca-file) cafile="$val" ;;
    --org) org="$val" ;;
  esac
done

[ "$(id -u)" != 0 ] || die "refusing to run as root: wardyn installs for one person, into their own directory. Run this as the user who will use it, without sudo."

[ -n "$version" ] || version="$RELEASE_VERSION"
case "$version" in
  "@"*) die "this copy of install-cli.sh was not stamped with a release version: pass --version <x.y.z>, or use the copy attached to the release" ;;
esac
version="${version#v}"
case "$version" in
  ''|*[!0-9A-Za-z.+-]*) die "not a release version: $version" ;;
esac

[ -n "$base" ] || base="https://github.com/${REPO}/releases/download"
base="${base%/}"
case "$base" in
  https://*) ;;
  *) die "the artifact base must be an https:// URL, got: $base" ;;
esac
if [ -n "$cafile" ] && [ ! -r "$cafile" ]; then
  die "--ca-file: cannot read $cafile"
fi
if [ -n "$org" ]; then
  case "$org" in
    http://*|https://*) ;;
    *) die "--org must be an http:// or https:// URL, got: $org" ;;
  esac
  case "$org" in
    *[[:space:]\"\'\`\$\;\&\|\<\>\\\(\)]*) die "--org holds a character a URL does not carry: $org" ;;
  esac
fi

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "no wardyn CLI is published for $(uname -s); this script covers macOS, Linux and WSL2 (Windows: install-cli.ps1)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "no wardyn CLI is published for $(uname -m) (published: amd64 and arm64)" ;;
esac
asset="wardyn-${os}-${arch}"
rel="${base}/v${version}"

have_cosign=""
signed=""
command -v cosign >/dev/null 2>&1 && have_cosign=1
if [ -n "$require_sig" ] && [ -z "$have_cosign" ]; then
  die "--require-signature: cosign is not on PATH, so the signature cannot be checked. Install cosign (https://docs.sigstore.dev/cosign/installation/), or drop the flag and accept the checksum alone."
fi

# Where it goes: the first writable directory.
dir=""
if [ -n "${WARDYN_INSTALL_CLI_DIRS:-}" ]; then
  # shellcheck disable=SC2086
  set -- ${WARDYN_INSTALL_CLI_DIRS}
else
  set -- /usr/local/bin "${HOME}/.local/bin" "${HOME}/bin"
fi
for d in "$@"; do
  [ -d "$d" ] || mkdir -p "$d" 2>/dev/null || continue
  [ -w "$d" ] || continue
  dir="$d"; break
done
[ -n "$dir" ] || die "no writable directory among /usr/local/bin, ~/.local/bin and ~/bin; create one you own and re-run"

work=$(mktemp -d "${TMPDIR:-/tmp}/wardyn-install.XXXXXX") || die "could not make a temporary directory"
stage="${dir}/.wardyn.new.$$"
cleanup() { rm -rf "$work"; rm -f "$stage"; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

# get NAME DEST - one asset from <base>/v<version>/, or die naming it.
get() {
  rc=0
  if [ -n "$cafile" ]; then
    curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 15 --max-time 600 \
      --cacert "$cafile" -o "$2" "${rel}/$1" || rc=$?
  else
    curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 15 --max-time 600 \
      -o "$2" "${rel}/$1" || rc=$?
  fi
  [ "$rc" = 0 ] && return 0
  hint=""
  case "$rc" in
    60|77|35) hint=" (the server's certificate was not trusted: if your organisation's CA signs it, pass --ca-file <pem>)" ;;
    22) hint=" (the server answered with an error: a mirror must hold the release's own layout, ${base}/v${version}/<asset>, under the same asset names)" ;;
  esac
  die "could not fetch $1 from ${rel}/$1 (curl exit ${rc})${hint}"
}

sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'
}

say "installing wardyn ${version} (${asset}) from ${rel}"
get SHA256SUMS "$work/SHA256SUMS"

if [ -n "$have_cosign" ]; then
  get SHA256SUMS.sig "$work/SHA256SUMS.sig"
  get SHA256SUMS.pem "$work/SHA256SUMS.pem"
  identity="https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/v${version}"
  vrc=0
  if [ -n "$cafile" ]; then
    SSL_CERT_FILE="$cafile" cosign verify-blob "$work/SHA256SUMS" --signature "$work/SHA256SUMS.sig" \
      --certificate "$work/SHA256SUMS.pem" --certificate-identity "$identity" \
      --certificate-oidc-issuer "$ISSUER" >"$work/cosign.out" 2>&1 || vrc=$?
  else
    cosign verify-blob "$work/SHA256SUMS" --signature "$work/SHA256SUMS.sig" \
      --certificate "$work/SHA256SUMS.pem" --certificate-identity "$identity" \
      --certificate-oidc-issuer "$ISSUER" >"$work/cosign.out" 2>&1 || vrc=$?
  fi
  if [ "$vrc" != 0 ]; then
    tail -n 5 "$work/cosign.out" >&2
    last=$(tail -n 1 "$work/cosign.out")
    if [ -n "$cafile" ]; then
      die "cosign could not verify SHA256SUMS (identity ${identity}): a network or trust failure, or a bad signature; --ca-file is also given to cosign as SSL_CERT_FILE. cosign said: ${last}. Nothing was installed."
    fi
    die "cosign refused the signature on SHA256SUMS (identity ${identity}); cosign said: ${last}. Nothing was installed."
  fi
  signed=1
  say "signature: verified with cosign against ${identity}"
  say "  a script piped from the same origin also names this identity, so for a piped run that line is what you check by hand"
else
  say "signature: not checked (cosign not found)"
fi

get "$asset" "$stage"
want=$(awk -v a="$asset" '{n=$2; sub(/^\.\//,"",n); if (n==a) print $1}' "$work/SHA256SUMS" | head -1)
[ -n "$want" ] || die "SHA256SUMS lists no ${asset}; nothing was installed"
got=$(sha256_hex "$stage")
[ "$want" = "$got" ] || die "checksum mismatch for ${asset} (want ${want}, got ${got}); nothing was installed"
if [ -n "$signed" ]; then
  say "checksum: matches the SHA256SUMS list that cosign verified as signed by the release"
else
  say "checksum: matches SHA256SUMS from the same place: catches a broken download, not a tampered release"
fi

chmod 0755 "$stage"
mv -f "$stage" "${dir}/wardyn"
say "installed ${dir}/wardyn"
case ":${PATH}:" in
  *":${dir}:"*) ;;
  *) say "${dir} is not on your PATH; add it: export PATH=\"${dir}:\$PATH\"" ;;
esac

if [ -n "$org" ]; then
  say "Next: wardyn login ${org}"
else
  say "Next: wardyn login <your organisation's URL>"
fi
