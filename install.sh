#!/usr/bin/env sh
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# install.sh — install Wardyn on this machine.
#
#   curl -fsSL https://github.com/cjohnstoniv/wardyn/releases/download/v0.6.6/install.sh | sh
#
# That URL is a release asset covered by the signed SHA256SUMS. Curling this
# file from `main` also works, but nothing signs tip-of-main. README.md carries
# the same URL; RELEASING.md step 1b sweeps the version in both.
#
# Pulls the published images BY TAG and starts the containerized control plane.
# No clone, no build, no toolchain — Docker is the only requirement. Read
# docs/VERIFY.md §6 before you decide to trust this: those images are
# cosign-verifiABLE, and this script runs no cosign — piping it to sh is a
# decision to trust the release origin for one command.
#
# Deploying to Kubernetes instead? That path needs no clone either:
#   helm install wardyn oci://ghcr.io/cjohnstoniv/charts/wardyn --version <ver>
#
# Building from source is a CONTRIBUTOR path, not an install path: see
# CONTRIBUTING.md.
#
# Env: WARDYN_VERSION (default: latest release), WARDYN_HOME (default ~/.<NS>),
#      WARDYN_PORT (default 8080), WARDYN_NS (default "wardyn" — the container-name
#      prefix; change it to run a second install alongside the first, and the
#      install root follows it so that second stack gets its own .env and keys).
#
# ponytail: sh, not bash — this is the one script that runs on a machine we know
# nothing about, before anything of ours is installed.
set -eu

REPO=cjohnstoniv/wardyn

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# env_get KEY [FILE] — the last uncommented KEY= value in FILE (default .env),
# stripped of surrounding whitespace and ONE layer of matching quotes (the `\1`
# back-reference is what keeps `"x'` from being unquoted). EVERY read of an
# existing install's .env routes through here, because `KEY="value"` is a shape
# docker compose accepts — it strips the quotes and the stack runs — so a reader
# that tests the RAW line calls a quoted key ABSENT. That is how the upgrade
# refused a perfectly good WARDYN_AGE_KEY with "has no … line", and how the
# admin-token predicate this replaced counted `""` as a credential.
env_get() {
  sed -n "s/^$1=//p" "${2:-.env}" 2>/dev/null | tail -1 \
    | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' \
          -e 's/^\(["'\'']\)\(.*\)\1$/\2/' \
          -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}

# fetch URL [curl args…] — every network read this script makes. curl's default
# is to wait FOREVER, and a peer that completes the handshake and then answers
# nothing hangs `curl … | sh` with no output and no exit — indistinguishable
# from a machine that has stopped. One wrapper keeps the four fetches in step.
fetch() { curl -fsSL --connect-timeout 10 --max-time 120 "$@"; }

# The namespace names the containers AND, by default, the install root. A second
# install under a different WARDYN_NS needs its own .env or it is not a second
# install at all: it shares the first one's age key, admin token and ports and
# merely redeploys it, which is exactly what the "install alongside it" remedy
# below used to do.
NS="${WARDYN_NS:-wardyn}"
HOME_DIR="${WARDYN_HOME:-}"
if [ -z "${HOME_DIR}" ]; then
  HOME_DIR="${HOME}/.${NS}"
  # Installs made before that default all live in ~/.wardyn whatever their NS.
  # Keep upgrading THAT one when its .env claims this very namespace: deriving a
  # fresh root beside it would mint a second age key and orphan every secret the
  # first one holds.
  if [ ! -f "${HOME_DIR}/.env" ] && \
     [ "$(env_get WARDYN_NS "${HOME}/.wardyn/.env")" = "${NS}" ]; then
    HOME_DIR="${HOME}/.wardyn"
  fi
fi
PORT="${WARDYN_PORT:-8080}"

# sha256_hex [FILE] — the hex digest of FILE, or of stdin with no argument.
# macOS ships `shasum`, not GNU coreutils' `sha256sum`, so BOTH hashing sites
# route through here: install_cli's SHA256SUMS check, which already knew that,
# and mint_admin_token, which did not — and so minted an EMPTY admin token on
# every mac, which compose then substitutes with its published demo token.
# T2 of scripts/test-install-sh-trust.sh runs this whole script on a MAC-SHAPED
# PATH (no `sha256sum` anywhere on it), so the fallback below is executed there,
# not merely read.
sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi | awk '{print $1}'
}

# mint_age_key WHAT — one `AGE-SECRET-KEY-…` line minted by the wardynd image,
# or die. BOTH mints route through here: the secret-store key written to .env,
# and the INDEPENDENT key mint_admin_token hashes (the admin token is never
# derived from the store key). The mint is checked on its OWN line before
# anything consumes it: POSIX sh has no pipefail, so `set -e` cannot see a
# docker failure inside a pipeline, and the empty stdin such a failure leaves
# behind would be hashed into sha256("")[0:48] — a public constant — and
# installed as this box's admin credential.
mint_age_key() {
  minted=$(docker run --rm "${IMG}" -gen-age-key 2>/dev/null \
           | grep -E '^AGE-SECRET-KEY-' | head -1 || true)
  [ -n "$minted" ] || die "could not mint $1 from ${IMG}"
  printf '%s' "$minted"
}

# mint_admin_token — a 48-hex admin token. The non-empty guard lives HERE, not
# at each call site: `TOKEN=$(mint_admin_token)` is a plain assignment, so a
# `die` inside the substitution surfaces as a non-zero status that the caller's
# `set -e` acts on, and one guard cannot drift out of step with the other.
mint_admin_token() {
  mint_key=$(mint_age_key "an admin token")
  TOKEN=$(printf '%s' "$mint_key" | sha256_hex | cut -c1-48)
  [ -n "$TOKEN" ] || die "could not derive this install's admin token"
  printf '%s' "$TOKEN"
}

# older_than A B — true when A is a strictly OLDER release than B, comparing the
# numeric major.minor.patch and nothing else. Not `sort -V`: that is a GNU
# extension and this is the one script that runs on a machine we know nothing
# about. Anything that is not three dotted numbers on BOTH sides (a hand-edited
# .env header, a build with no patch component) is NOT called older — the
# refusal below skips rather than guess an ordering it cannot justify.
older_than() {
  awk -v a="${1#v}" -v b="${2#v}" 'BEGIN{
    if (split(a, x, ".") < 3 || split(b, y, ".") < 3) exit 1
    for (i = 1; i <= 3; i++) {
      if (x[i] + 0 < y[i] + 0) exit 0
      if (x[i] + 0 > y[i] + 0) exit 1
    }
    exit 1 }'
}

command -v docker >/dev/null 2>&1 || die "docker is required — https://docs.docker.com/get-docker/"
docker info >/dev/null 2>&1 || die "the docker daemon is not reachable (is Docker running?)"
docker compose version >/dev/null 2>&1 || die "docker compose v2 is required (docker-compose v1 is not supported)"

# Resolve the version. The GitHub API needs no token for a public repo; if it is
# rate-limited or offline, say so rather than silently installing something else.
#
# NOT `releases/latest`: that endpoint EXCLUDES pre-releases, and RELEASING.md
# mandates `--prerelease` on every Wardyn release — so it returns HTTP 404 and
# this script used to die two lines below on every install. `releases?per_page=1`
# is the newest release of any kind. RELEASING.md says the same thing about
# `releases/latest/download/`.
VERSION="${WARDYN_VERSION:-}"
if [ -z "$VERSION" ]; then
  say "Resolving the latest release"
  VERSION=$(fetch "https://api.github.com/repos/${REPO}/releases?per_page=1" 2>/dev/null \
            | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$VERSION" ] || die "could not resolve the latest release — set WARDYN_VERSION=vX.Y.Z"
fi
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac
# A release TAG — never anything that can act as a URL path. VERSION is
# interpolated into four fetch URLs (the compose file on raw.githubusercontent,
# the CLI asset and the SHA256SUMS it is checked against on github.com) and into
# every image ref below. curl resolves `..` segments BEFORE it dials (RFC 3986
# s5.2.4), so `WARDYN_VERSION=v/../../../attacker/evil/main` walks out of
# ${REPO} and re-points those downloads at an arbitrary owner — SHA256SUMS
# included, which is exactly why install_cli's same-origin checksum gate cannot
# catch it: it would compare the attacker's binary against the attacker's own
# digest. Allow exactly what a git tag needs and refuse the rest.
case "$VERSION" in
  *[!A-Za-z0-9._+-]*|*..*)
    die "WARDYN_VERSION must be a release tag (letters, digits and . _ + -), not '${VERSION}' — it is used as a URL path segment and a container image tag." ;;
esac
SEMVER="${VERSION#v}"
# The wardynd image is named four times — two mints, the fresh .env, the upgrade
# rewrite — and a version-pin that disagreed with itself across them is exactly
# the class of bug the upgrade path already shipped once. Name it once.
IMG="ghcr.io/${REPO%/*}/wardynd:${SEMVER}"
# The agent images this release publishes, as `<catalog name>:<image name>` —
# the fresh .env seeds the map below from these, and the upgrade merges into
# whatever map the box already has, row by row.
AGENT_IMAGE_ROWS="claude-code:agent-base codex-cli:agent-codex-cli aws-sso:agent-aws-sso"
say "Installing Wardyn ${VERSION} into ${HOME_DIR}"

# Container names are ${WARDYN_NS:-wardyn}-*, so a stack already running from a
# clone owns them. Surface that as a sentence instead of a daemon conflict error
# fifty lines into a compose transcript.
# Only when this is a FRESH install, though. On an upgrade — "re-run this
# installer at the new version", the procedure the closing banner prints — the
# containers `docker ps -a` reports are OUR OWN, so this fired on every box that
# had ever run the installer and made the documented upgrade exit 1 with a
# message about somebody else's stack.
#
# The remedy below is written to be PASTED: a `VAR=x curl … | sh` prefix applies
# to `curl`, never to the piped `sh` (/bin/sh is dash on most Linux), so the
# form this used to print re-ran the installer with a default environment and
# reproduced the identical error.
if [ ! -f "${HOME_DIR}/.env" ] && docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "${NS}-api"; then
  die "a Wardyn stack named '${NS}' already exists on this Docker daemon.
  Stop it first (cd ${HOME_DIR} && docker compose down), or install alongside it
  under a different name and free ports:
      curl -fsSL <this-url> | WARDYN_NS=wardyn2 WARDYN_PORT=8090 WARDYN_PG_PORT=5433 \\
        WARDYN_REGISTRY_PORT=5011 WARDYN_SSH_PORT=2223 WARDYN_UI_SANDBOX_PORT=8091 sh
  That second stack gets its own install root, ${HOME}/.wardyn2, and its own keys."
fi

mkdir -p "$HOME_DIR"
cd "$HOME_DIR"

# REFUSALS COME FIRST — before the compose file is overwritten at the new tag
# and before .env is touched — so a refusal leaves the box exactly as it was
# found. This one used to run AFTER both rewrites, which left a stopped upgrade
# pinning a version it had just been told it cannot start.
#
# An .env with no store key cannot decrypt one secret this box has already
# written, and compose would start wardynd against an empty WARDYN_AGE_KEY.
# Minting a fresh key here would be WORSE than stopping — it would silently
# orphan every existing secret behind a key that never encrypted them — so this
# is the one upgrade condition the installer refuses instead of fixing.
if [ -f .env ]; then
  case "$(env_get WARDYN_AGE_KEY)" in
    AGE-SECRET-KEY-*) ;;
    *) die "${HOME_DIR}/.env has no WARDYN_AGE_KEY=AGE-SECRET-KEY-… line.
  Minting a new one here would orphan every secret already stored on this box.
  Restore that line from your backup, or move ${HOME_DIR}/.env aside to start
  over (every stored secret then becomes unrecoverable)." ;;
  esac

  # An .env here means this is an UPGRADE, and the closing banner's whole
  # instruction for one is "re-run this installer at the new version". True, and
  # it omits the part that cannot be undone: migrations are forward-only
  # (internal/db applies anything new on boot; there are no `down` migrations),
  # so the Postgres dump is the only rollback this box has and it has to be
  # taken BEFORE the restart. Say it here, where it is still actionable, rather
  # than only in a doc the operator reached this script instead of reading.
  # The trailing `[0-9]` is what stops the greedy `[0-9.]*` from swallowing the
  # sentence-ending period of "…for Wardyn v0.6.5. Safe to edit." and reporting
  # this box as running "0.6.5.".
  INSTALLED=$(sed -n '1s/^# Generated by install\.sh for Wardyn v\{0,1\}\([0-9][0-9.]*[0-9]\).*/\1/p' .env)
  say "Upgrading the install already in ${HOME_DIR}${INSTALLED:+ (${INSTALLED})} — if you have not taken a dump, Ctrl-C and take one:"
  say "  docker exec ${NS}-postgres pg_dump -U wardyn wardyn > wardyn-\$(date +%F).sql"
  say "  Full procedure: https://github.com/${REPO}/blob/${VERSION}/docs/OPERATIONS.md#upgrading-a-one-line-install"

  # …and the same forward-only rule is why moving BACKWARDS is refused rather
  # than warned about. This branch is about to rewrite the image pins in place,
  # which is what would leave an OLDER wardynd booting against a database a
  # newer one has already migrated — unsupported, with no `down` migration to
  # walk it back. Until now nothing stopped it, at the one moment it could be.
  # Refused with the rest of the refusals, before the first write.
  if [ -n "${INSTALLED}" ] && older_than "${SEMVER}" "${INSTALLED}"; then
    die "${HOME_DIR} is running ${INSTALLED} and this installer is ${SEMVER} — that is a DOWNGRADE.
  Migrations are forward-only: there are no \`down\` migrations, so an older wardynd against a
  database a newer one has already migrated is unsupported and nothing can walk it back.
  Re-run at ${INSTALLED} or newer (WARDYN_VERSION=vX.Y.Z), or restore your pg_dump into a
  FRESH install root (WARDYN_HOME=~/.wardyn-${SEMVER}).
  https://github.com/${REPO}/blob/${VERSION}/docs/OPERATIONS.md#upgrading-a-one-line-install"
  fi
fi

# The compose file comes from the tag itself, so this works the moment a release
# exists — no dependency on a release asset having been uploaded.
say "Fetching the compose definition"
fetch "https://raw.githubusercontent.com/${REPO}/${VERSION}/deploy/compose/docker-compose.yaml" \
  -o docker-compose.yaml || die "could not fetch the compose file for ${VERSION}"

# .env holds WARDYN_AGE_KEY — the master key for every secret on this box — and
# the admin token. Born 0600, rather than chmod'd 0600 a moment after it is
# written: on a shared host that window is enough to read both. OLD_UMASK is
# restored below so install_cli's PATH directories keep their normal modes.
#
# There is no `chmod 600 .env` any more, on either branch: under `umask 077`
# both were dead code that could only ever re-assert the mode the file already
# had. An .env some older install created 0644 is not left behind by dropping
# them — the upgrade branch rewrites the file through `.env.tmp` + `mv`, and
# that tmp is born 0600 under this same umask. T4 of
# scripts/test-install-sh-trust.sh runs under `umask 022` with a RECORD-ONLY
# chmod stub and asserts the final mode is 600, so a chmod could not be what
# produced it.
OLD_UMASK=$(umask)
umask 077
if [ ! -f .env ]; then
  say "Minting this install's secret-store key"
  # Same mechanism the repo's own installers use. The key never leaves this box.
  KEY=$(mint_age_key "an age key")
  TOKEN=$(mint_admin_token)

  cat > .env <<EOF
# Generated by install.sh for Wardyn ${VERSION}. Safe to edit.
WARDYN_AGE_KEY=${KEY}
WARDYN_ADMIN_TOKEN=${TOKEN}
WARDYN_UP_PORT=${PORT}
WARDYN_NS=${NS}
WARDYN_PG_PORT=${WARDYN_PG_PORT:-5432}
WARDYN_REGISTRY_PORT=${WARDYN_REGISTRY_PORT:-5010}
WARDYN_SSH_PORT=${WARDYN_SSH_PORT:-2222}
WARDYN_UI_SANDBOX_PORT=${WARDYN_UI_SANDBOX_PORT:-8081}
# The PORT vars above only publish the host port. The LISTEN vars below are what
# actually start a listener — empty means off, and the host key is not even
# generated. Setting one without the other gives a published port that refuses
# every connection, which is what this installer shipped in 0.6.3.
WARDYN_SSH_LISTEN=:2222
WARDYN_SSH_ADVERTISE=127.0.0.1:${WARDYN_SSH_PORT:-2222}
# The UI-sandbox relay stays OFF: no agent-vscode or noVNC image is published,
# so the listener would have nothing to serve. See docs/UI-SANDBOXES.md.
# WARDYN_UI_SANDBOX_LISTEN=:8081
# Published images — this install pulls, it never builds.
WARDYN_WARDYND_IMAGE=${IMG}
WARDYN_PROXY_IMAGE=ghcr.io/${REPO%/*}/wardyn-proxy:${SEMVER}
WARDYN_AGENT_IMAGES={"claude-code":"ghcr.io/${REPO%/*}/agent-base:${SEMVER}","codex-cli":"ghcr.io/${REPO%/*}/agent-codex-cli:${SEMVER}","aws-sso":"ghcr.io/${REPO%/*}/agent-aws-sso:${SEMVER}"}
EOF
else
  # UPGRADE PATH. This used to be the whole story — "reusing the existing .env" —
  # and that was a trap. Re-running the installer at a NEW tag overwrites
  # docker-compose.yaml (above) at that tag, while this branch skipped the
  # entire .env write INCLUDING THE IMAGE PINS. The box then ran the new
  # topology against the OLD release's images and was told "Wardyn is running".
  # WARDYN_VERSION did not help: the pins are inside the same guard.
  #
  # Rewrite exactly the version-derived lines and leave everything else — the
  # age key, the admin token, the ports, any hand edits — untouched.
  say "Upgrading the existing ${HOME_DIR}/.env to ${VERSION}"
  env_set() { # KEY VALUE — replace an uncommented KEY= line, or append.
    k=$1; v=$2
    if grep -qE "^${k}=" .env; then
      awk -v k="$k" -v v="$v" 'BEGIN{FS=OFS="="} $1==k {print k "=" v; next} {print}' .env > .env.tmp \
        && mv .env.tmp .env
    else
      printf '%s=%s\n' "$k" "$v" >> .env
    fi
  }
  # The header names a version too; leaving it stale makes the file lie about
  # what it pins.
  awk -v v="${VERSION}" 'NR==1 && /^# Generated by install.sh for Wardyn / {print "# Generated by install.sh for Wardyn " v ". Safe to edit."; next} {print}' .env > .env.tmp && mv .env.tmp .env
  env_set WARDYN_WARDYND_IMAGE "${IMG}"
  env_set WARDYN_PROXY_IMAGE "ghcr.io/${REPO%/*}/wardyn-proxy:${SEMVER}"
  # WARDYN_AGENT_IMAGES is MERGED, not replaced. docs/UI-SANDBOXES.md tells
  # operators to add their own images to this same map, and rewriting the line
  # wholesale deleted every one of them on the next upgrade — silently, under
  # the comment above promising that hand edits survive. Only rows still
  # pointing at ghcr.io/<owner>/ are bumped, so an operator who REPOINTED a
  # known name at their own registry keeps that too; a published row the file
  # never had (0.6.x seeded no claude-code) is added.
  images=$(env_get WARDYN_AGENT_IMAGES)
  case "$(printf '%s' "${images}" | tr -d '[:space:]')" in
    '{'*'}') ;;
    *) images='{}' ;;   # absent, or not an object at all — every row is missing
  esac
  for row in ${AGENT_IMAGE_ROWS}; do
    name=${row%%:*}; img="ghcr.io/${REPO%/*}/${row#*:}:${SEMVER}"
    if printf '%s' "${images}" | grep -q "\"${name}\"[[:space:]]*:[[:space:]]*\"ghcr\.io/${REPO%/*}/"; then
      images=$(printf '%s' "${images}" \
        | sed "s|\"${name}\"[[:space:]]*:[[:space:]]*\"ghcr\.io/${REPO%/*}/[^\"]*\"|\"${name}\":\"${img}\"|")
    elif ! printf '%s' "${images}" | grep -q "\"${name}\"[[:space:]]*:"; then
      images=$(printf '%s' "${images}" | sed "s|^{[[:space:]]*|{\"${name}\":\"${img}\",|")
    fi
  done
  # …which leaves `{"a":"b",}` when the map started out empty.
  env_set WARDYN_AGENT_IMAGES "$(printf '%s' "${images}" | sed 's|,[[:space:]]*}$|}|')"
  # Listeners are additive: an install from before they existed has neither, and
  # without them the published ports stay inert.
  grep -qE '^WARDYN_SSH_LISTEN=' .env || printf 'WARDYN_SSH_LISTEN=:2222\n' >> .env
  # …and from the port THIS INSTALL publishes: compose reads WARDYN_SSH_PORT
  # from the same .env, so backfilling the advertise from the process
  # environment told the console and /healthz a port nothing forwards.
  ssh_port=$(env_get WARDYN_SSH_PORT)
  grep -qE '^WARDYN_SSH_ADVERTISE=' .env || printf 'WARDYN_SSH_ADVERTISE=127.0.0.1:%s\n' "${ssh_port:-${WARDYN_SSH_PORT:-2222}}" >> .env
  # An install that landed an EMPTY admin token — 0.6.x on a host with no
  # sha256sum, or a docker run that failed mid-pipeline — carried it forward
  # through every later upgrade in silence, and the compose file substitutes its
  # PUBLISHED demo token for an empty value. Re-mint, and say so: this is the
  # credential the closing banner tells the operator to go and read.
  #
  # Read the VALUE, rather than asking `grep -qE '^WARDYN_ADMIN_TOKEN=.'` whether
  # SOME character follows the `=`: that predicate is satisfied by `=""`, by
  # `=''`, by a single space, and by both placeholders an operator is most likely
  # to have copied in — compose's published `demo-admin-token` and a `change-me`
  # — so each of those was carried forward as though it were a credential.
  case "$(env_get WARDYN_ADMIN_TOKEN)" in
    ''|demo-admin-token|change-me)
      say "The existing WARDYN_ADMIN_TOKEN is empty or a placeholder — minting a new one"
      TOKEN=$(mint_admin_token)
      env_set WARDYN_ADMIN_TOKEN "${TOKEN}"
      ;;
  esac
fi
umask "${OLD_UMASK}"

# From here on the banner describes THE INSTALL THAT NOW EXISTS, not the
# variables this invocation happened to carry. The upgrade branch leaves the
# ports in .env untouched by design, so echoing ${WARDYN_PORT:-8080} ended every
# upgrade re-run by sending the operator to a port nothing listens on. Both
# branches route through the same read: on a fresh install .env holds the values
# just written, so there is one code path and nothing to keep in step.
PORT=$(env_get WARDYN_UP_PORT); PORT="${PORT:-${WARDYN_PORT:-8080}}"
SSH_PORT=$(env_get WARDYN_SSH_PORT); SSH_PORT="${SSH_PORT:-${WARDYN_SSH_PORT:-2222}}"

say "Pulling signed images"
docker compose pull --quiet 2>/dev/null || docker compose pull
# `docker compose pull` resolves only the three default-profile services. The
# proxy sidecar (WARDYN_PROXY_IMAGE) and the three agent images the .env above
# registers are pulled by wardynd ITSELF, at the first run — long after this
# transcript has scrolled past — so the line above covers three of the seven
# images this release publishes. docs/VERIFY.md §6 says so; nothing said it HERE,
# where an operator counting what arrived is standing.
echo "                (the control plane: wardynd, postgres, registry. The proxy and the three"
echo "                 agent images are pulled by wardynd at your first run — same release tag,"
echo "                 same signatures, verified the same way: docs/VERIFY.md §6.)"

# --no-build is the guarantee: the compose file carries build stanzas for
# contributors, and this install must never trigger one.
say "Starting"
docker compose up -d --no-build

# The CLI. Without it this install has NO host binary at all: the only command
# path is `docker compose exec`, which is in-container and root-only, so
# `wardyn ssh <run-id>` — the whole point of the SSH listener above — has no
# client on the machine that just enabled it.
#
# Release assets are per os/arch and listed in SHA256SUMS, so hash-check before
# installing. Be honest about the strength of that check: SHA256SUMS is fetched
# from the SAME release base as the binary, and this script does not read
# SHA256SUMS.sig/.pem. It defeats a corrupted or swapped asset, not a tampered
# release. The signature check is the operator's, in docs/VERIFY.md §5.
install_cli() {
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) say "No published wardyn CLI for $(uname -m); skipping (the console still works)"; return 0 ;;
  esac
  case "$os" in linux|darwin) ;; *) say "No published wardyn CLI for ${os}; skipping"; return 0 ;; esac

  asset="wardyn-${os}-${arch}"
  base="https://github.com/${REPO}/releases/download/${VERSION}"
  tmp="${HOME_DIR}/.wardyn-cli.$$"
  fetch "${base}/${asset}" -o "${tmp}" 2>/dev/null || {
    say "Could not download the ${asset} CLI; skipping (the console still works)"
    rm -f "${tmp}"; return 0
  }

  # Verify against the release's SHA256SUMS. A failure here is FATAL, not a
  # skip: silently installing an unverified binary onto PATH is worse than
  # having no CLI at all.
  if sums=$(fetch "${base}/SHA256SUMS" 2>/dev/null) && [ -n "${sums}" ]; then
    # SHA256SUMS lists names as `<hash>  ./<name>`, so strip the leading ./ and
    # match the basename EXACTLY — a substring match would let
    # wardyn-linux-amd64 be satisfied by a line for some other asset.
    want=$(printf '%s\n' "${sums}" | awk -v a="${asset}" '{n=$2; sub(/^\.\//,"",n); if (n==a) print $1}' | head -1)
    if [ -n "${want}" ]; then
      got=$(sha256_hex "${tmp}")
      [ "${want}" = "${got}" ] || { rm -f "${tmp}"; die "checksum mismatch for ${asset} (want ${want}, got ${got}) — refusing to install it"; }
    else
      say "SHA256SUMS lists no ${asset}; skipping the CLI rather than installing it unverified"
      rm -f "${tmp}"; return 0
    fi
  else
    say "Could not fetch SHA256SUMS; skipping the CLI rather than installing it unverified"
    rm -f "${tmp}"; return 0
  fi

  chmod 0755 "${tmp}"
  # Prefer a dir already on PATH and writable WITHOUT sudo — this script has not
  # asked for privilege anywhere else and should not start here.
  for d in /usr/local/bin "${HOME}/.local/bin" "${HOME_DIR}/bin"; do
    [ -d "${d}" ] || mkdir -p "${d}" 2>/dev/null || continue
    [ -w "${d}" ] || continue
    mv -f "${tmp}" "${d}/wardyn" 2>/dev/null || continue
    CLI_PATH="${d}/wardyn"
    return 0
  done
  rm -f "${tmp}"
  say "No writable directory on PATH for the CLI; skipping (the console still works)"
}
CLI_PATH=""
say "Installing the wardyn CLI"
install_cli

say "Wardyn is running: http://127.0.0.1:${PORT}"
echo
if [ -n "${CLI_PATH}" ]; then
  echo "  CLI:          ${CLI_PATH}"
  case ":${PATH}:" in
    *":$(dirname "${CLI_PATH}"):"*) ;;
    *) echo "                (not on your PATH — add: export PATH=\"$(dirname "${CLI_PATH}"):\$PATH\")" ;;
  esac
  echo "  Attach:       wardyn ssh <run-id>   (the SSH gateway is on at 127.0.0.1:${SSH_PORT})"
fi
echo "  Mode:         single-user — you are the admin. Multiple people? https://github.com/${REPO}/blob/${VERSION}/docs/OPERATIONS.md#second-user-same-host"
echo "  Admin token:  grep WARDYN_ADMIN_TOKEN ${HOME_DIR}/.env"
echo "  Stop:         cd ${HOME_DIR} && docker compose down            (keeps all data)"
echo "  Upgrade:      re-run this installer at the new version"
echo "                pg_dump FIRST — migrations are forward-only, so that dump is your only"
echo "                rollback: https://github.com/${REPO}/blob/${VERSION}/docs/OPERATIONS.md#upgrading-a-one-line-install"
echo "  Uninstall:    cd ${HOME_DIR} && docker compose down            then remove ${HOME_DIR} yourself."
echo "                ${HOME_DIR}/.env holds WARDYN_AGE_KEY in cleartext: deleting it"
echo "                makes every secret stored on this box UNRECOVERABLE. \`docker compose"
echo "                down -v\` additionally destroys every run, recording and audit row."
echo "  Verify what you pulled: https://github.com/${REPO}/blob/${VERSION}/docs/VERIFY.md"
echo
echo "  No model access is configured yet — that is a supported end state."
echo "  Add one in the console under Settings -> Model provider."
