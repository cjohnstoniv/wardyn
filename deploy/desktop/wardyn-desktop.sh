#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# wardyn-desktop.sh — what com.wardyn.daemon.plist actually runs. Brings the
# desktop-tier compose stack (deploy/desktop/docker-compose.yaml, which
# `include:`s deploy/compose/docker-compose.yaml unmodified) up from the
# MDM-managed envelope at /etc/wardyn, idempotently. Safe to run repeatedly — the plist
# fires this on load and on a StartInterval tick, the same "re-assert, don't
# assume" posture MDM itself uses for the files it owns.
#
# It is deliberately NOT the installer: it mints nothing, creates no
# directory, never touches age.key beyond reading it. That is install.sh's
# job, once, on first setup — see docs/DESKTOP.md "The MDM file table".
#
# Usage: wardyn-desktop.sh [up|down [--purge]]
#
#   up              bring the stack up (default; what the timer fires)
#   down            stop it, keeping ALL data
#   down --purge    stop it and DESTROY the Postgres volume — every run, every
#                   recording, the whole append-only audit log. Irreversible.
#                   It does NOT touch /etc/wardyn/age.key: that is the installer's
#                   to mint and the uninstaller's to remove, and deleting it
#                   orphans every secret stored on this device forever.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../../scripts/lib/common.sh
# shellcheck disable=SC1091 # source= above is relative to this file, not resolvable from CWD
. "${REPO_ROOT}/scripts/lib/common.sh"  # log/warn/die, wait_healthy, wardyn_pick_docker_host

# `up` was the only subcommand, while the LaunchDaemon re-asserts every 300s.
# That left NO way to stop the stack — which uninstall, rollback (which must
# reach a stopped daemon before starting an older one against a
# migrated-forward DB), an offline-lane test, and `wardynd -rotate-age-key`
# (which requires a stopped daemon and enforces nothing) all need.
SUBCOMMAND="${1:-up}"
PURGE=0
case "${SUBCOMMAND}" in
  up) ;;
  down)
    case "${2:-}" in
      "") ;;
      --purge) PURGE=1 ;;
      *) die "wardyn-desktop.sh: unknown flag '$2' for down (only --purge is supported)" ;;
    esac
    ;;
  *) die "wardyn-desktop.sh: unknown subcommand '${SUBCOMMAND}' (up | down [--purge])" ;;
esac

# /etc/wardyn in production. Overridable because this script already EXPORTS
# WARDYN_MANAGED_DIR for the included compose file to mount — reading the same
# variable it exports makes the two consistent, and lets the envelope be
# exercised end-to-end (a wrapper-driven `up`, which is what the timer actually
# runs) without writing to /etc on a test box. The LaunchDaemon and the systemd
# unit set no such variable, so production is unchanged.
MANAGED_DIR="${WARDYN_MANAGED_DIR:-/etc/wardyn}"
ENV_FILE="${MANAGED_DIR}/wardyn.env"
SECRET_FILE="${MANAGED_DIR}/secret.env"
AGE_FILE="${MANAGED_DIR}/age.key"
SITE_CONFIG="${MANAGED_DIR}/site-config.json"

[ -f "${ENV_FILE}" ] || die "wardyn-desktop.sh: ${ENV_FILE} not found — MDM has not delivered the envelope yet (docs/DESKTOP.md)"
[ -f "${AGE_FILE}" ] || die "wardyn-desktop.sh: ${AGE_FILE} not found — run install.sh first (it mints the per-device key; MDM must never supply this one)"

# The per-device secret-store key: a VALUE in WARDYN_AGE_KEY, not a path (no
# WARDYN_AGE_KEY_FILE exists) — see docs/DESKTOP.md "A note on the mechanism".
WARDYN_AGE_KEY="$(cat "${AGE_FILE}")"
export WARDYN_AGE_KEY

# secret.env carries WARDYN_AUDIT_SINKS' live bearer token — export into this
# process's env (compose interpolation prefers shell env over --env-file) so
# it is never written into a compose var file on disk.
if [ -f "${SECRET_FILE}" ]; then
  set -a
  # shellcheck source=/dev/null
  . "${SECRET_FILE}"
  set +a
fi

# Read the image pins FROM THE ENVELOPE.
#
# This used to be `export WARDYN_WARDYND_IMAGE="${WARDYN_WARDYND_IMAGE:-...:latest}"`,
# which silently defeated the whole point of pinning: compose prefers the SHELL
# environment over --env-file, and wardyn.env is never sourced into this shell
# (only secret.env is, above). So the launcher's own `:latest` won over whatever
# digest the org shipped, on a 300s timer, while the org believed the fleet was
# pinned. Proven:
#
#     env-file only            -> image: ghcr.io/cjohnstoniv/wardynd@sha256:...
#     shell var also exported  -> image: ghcr.io/cjohnstoniv/wardynd:latest
#
# `publish-image.yml` pushes wardynd:latest on EVERY push to main, so that
# default had managed laptops tracking tip-of-main, unreleased, several times a
# day.
#
# env_get returns empty with exit 0 when the key is absent, so the fallback is
# explicit: without it the var would be set-but-empty and compose would fall
# back to wardyn/wardynd:local, which --no-build cannot build and no laptop can
# pull. env_get is already this file's idiom (see the site-config apply below).
WARDYN_WARDYND_IMAGE="$(env_get "${ENV_FILE}" WARDYN_WARDYND_IMAGE)"
export WARDYN_WARDYND_IMAGE="${WARDYN_WARDYND_IMAGE:-ghcr.io/cjohnstoniv/wardynd:latest}"
# Same for the egress sidecar, which deploy/desktop/ set NOWHERE: the base
# compose file then handed wardynd `wardyn/wardyn-proxy:local`, a ref no laptop
# can resolve, so EVERY run's proxy sidecar was unresolvable. CI never caught it
# because CI builds the proxy from the checkout.
WARDYN_PROXY_IMAGE="$(env_get "${ENV_FILE}" WARDYN_PROXY_IMAGE)"
if [ -n "${WARDYN_PROXY_IMAGE}" ]; then
  export WARDYN_PROXY_IMAGE
fi

# An explicit envelope value wins over auto-detection. This is the escape hatch
# for the tier's likeliest install failure: this script runs as ROOT (a
# LaunchDaemon, or a system systemd unit), while Docker Desktop, Colima,
# rootless Docker and Podman all expose a PER-USER socket. Auto-detection shells
# `docker context inspect`, which as root reads ROOT's contexts, not the
# enrolled user's — so it silently resolves the wrong daemon, or none.
#
# CI never surfaces this: its daemon is root-reachable and the question does not
# arise. MDM sets WARDYN_DOCKER_SOCK in wardyn.env when the fleet's runtime is
# per-user. docs/DESKTOP.md "Which Docker socket" has the matrix.
_env_sock="$(env_get "${ENV_FILE}" WARDYN_DOCKER_SOCK)"
if [ -n "${_env_sock}" ]; then
  export WARDYN_DOCKER_SOCK="${_env_sock}"
  export DOCKER_HOST="unix://${_env_sock}"
fi
unset _env_sock

wardyn_pick_docker_host  # DOCKER_HOST / WARDYN_DOCKER_SOCK, incl. Colima/Rancher

# Refuse LOUDLY rather than hang or half-start. Without this the failure is a
# compose error fifty lines in, or worse, a converge that "succeeds" against a
# daemon that is not the one the developer uses.
if ! docker info >/dev/null 2>&1; then
  die "wardyn-desktop.sh: no reachable Docker daemon.
  Tried: DOCKER_HOST='${DOCKER_HOST:-<unset>}', WARDYN_DOCKER_SOCK='${WARDYN_DOCKER_SOCK:-<unset>}'.
  This script runs as root; Docker Desktop, Colima, rootless Docker and Podman
  all expose a PER-USER socket that root cannot see by default. Set
  WARDYN_DOCKER_SOCK in ${ENV_FILE} to that socket's absolute path
  (e.g. /Users/<user>/.colima/default/docker.sock, or
  /run/user/<uid>/docker.sock for rootless Linux) and MDM will carry it to
  every device. See docs/DESKTOP.md 'Which Docker socket'."
fi

COMPOSE_FILE="${REPO_ROOT}/deploy/desktop/docker-compose.yaml"  # includes deploy/compose/docker-compose.yaml
# The included stack mounts the managed dir read-only at this same path inside
# the container; unset (every non-desktop deployment) it mounts an empty path.
export WARDYN_MANAGED_DIR="${MANAGED_DIR}"
compose() { docker compose --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" -p wardyn-desktop "$@"; }

if [ "${SUBCOMMAND}" = "down" ]; then
  if [ "${PURGE}" -eq 1 ]; then
    warn "wardyn-desktop.sh: --purge DESTROYS the Postgres volume: every run, every recording, and the whole append-only audit log. This cannot be undone."
    warn "wardyn-desktop.sh: ${AGE_FILE} is NOT removed — deleting it orphans every secret stored on this device. Remove it deliberately, with the uninstaller."
    compose down -v
    log "wardyn-desktop: down (volumes destroyed)"
  else
    compose down
    log "wardyn-desktop: down (data kept; use 'down --purge' to destroy the Postgres volume)"
  fi
  exit 0
fi

# --pull missing, not --pull always: `always` makes every 300s tick a registry
# round-trip, so an offline laptop dies here under `set -euo pipefail` and the
# stack does not come up AT ALL even though every image is already local. With
# the envelope pins above, an upgrade is an MDM rewrite of wardyn.env rather
# than a tag moving under the fleet, so there is nothing for `always` to catch.
log "Bringing up the desktop compose stack (image ${WARDYN_WARDYND_IMAGE})"
compose up -d --no-build --pull missing

PORT="${WARDYN_UP_PORT:-8080}"
BASE_URL="http://127.0.0.1:${PORT}"
log "Waiting for ${BASE_URL}/healthz"
wait_healthy "${BASE_URL}" 60 1 || die "wardyn-desktop.sh: wardynd did not become healthy (docker compose -p wardyn-desktop logs wardynd)"
log "wardynd healthy"

# Idempotent site-config apply. site-config.json is a full-document REPLACE
# (docs/DESKTOP.md "Posture switches are env vars, never site-config"), so
# re-applying the same file on every tick is a safe no-op, not accumulation.
# Runs the CLI baked into the image, in-container. This used to be gated on
# WARDYN_LOCAL_MODE=true, skipping the m-prime (SSO) variant with "the SSO
# envelope variant has no CLI-usable credential here" — and that PREMISE was
# wrong, which is why the gate is gone rather than worked around.
#
# The credential exists on both variants:
#   local mode  the loopback-equivalent peer is trusted with no token
#               (WARDYN_LOCAL_TRUST_FORWARDER in the base compose file)
#   m-prime     WARDYN_ADMIN_TOKEN ships in /etc/wardyn/secret.env, is sourced
#               above, and compose interpolates it into the wardynd container —
#               so `compose exec` INHERITS it and the CLI reads it from the
#               environment. It authenticates even with OIDC configured: a
#               rejected session cookie falls through to admin-token auth.
#
# No `-e` flag is needed or wanted: this shell has no such variable (only
# secret.env is sourced into it), and the container already carries it.
#
# UPSTREAM PROXY: site-config may set a corporate proxy, and the corp-proxy hop
# defers the post-DNS IP re-vet to that proxy — so this auto-apply asserts that
# residual on every laptop, every boot, with no human in the loop. That is a
# deliberate, recorded decision (docs/DESKTOP.md); the org authored the file MDM
# pushed.
#
# site-config.json is a full-document REPLACE, so re-applying the same file on
# every tick is a safe no-op, not accumulation.
if [ -f "${SITE_CONFIG}" ]; then
  compose exec -T wardynd /usr/local/bin/wardyn site-config apply "${SITE_CONFIG}" \
    || warn "wardyn-desktop.sh: site-config apply failed (non-fatal — the stack is up; see docker compose -p wardyn-desktop logs wardynd)"
else
  log "No ${SITE_CONFIG} delivered yet — skipping site-config apply"
fi

log "wardyn-desktop: up (${BASE_URL})"
