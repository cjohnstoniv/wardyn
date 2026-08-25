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
# Usage: wardyn-desktop.sh [up]   (only subcommand today; unrecognized args error)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../../scripts/lib/common.sh
# shellcheck disable=SC1091 # source= above is relative to this file, not resolvable from CWD
. "${REPO_ROOT}/scripts/lib/common.sh"  # log/warn/die, wait_healthy, wardyn_pick_docker_host

case "${1:-up}" in
  up) ;;
  *) die "wardyn-desktop.sh: unknown subcommand '$1' (only 'up' is supported)" ;;
esac

MANAGED_DIR="/etc/wardyn"
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

# The compose wardynd service builds `wardyn/wardynd:local` by default when
# WARDYN_WARDYND_IMAGE is unset (deploy/compose/docker-compose.yaml) — a
# mutable tag no MDM envelope should ship pinned to `latest` unless the
# operator overrides it in wardyn.env. --no-build below refuses to fall back
# to building that from source on a laptop with no repo checkout, so a real
# image ref is required; default to the published tag CI publishes on every
# push to main (docs/CI.md) when the envelope names none.
export WARDYN_WARDYND_IMAGE="${WARDYN_WARDYND_IMAGE:-ghcr.io/cjohnstoniv/wardynd:latest}"

wardyn_pick_docker_host  # DOCKER_HOST / WARDYN_DOCKER_SOCK, incl. Colima/Rancher

COMPOSE_FILE="${REPO_ROOT}/deploy/desktop/docker-compose.yaml"  # includes deploy/compose/docker-compose.yaml
# The included stack mounts the managed dir read-only at this same path inside
# the container; unset (every non-desktop deployment) it mounts an empty path.
export WARDYN_MANAGED_DIR="${MANAGED_DIR}"
compose() { docker compose --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" -p wardyn-desktop "$@"; }

log "Bringing up the desktop compose stack (image ${WARDYN_WARDYND_IMAGE})"
compose up -d --no-build --pull always

PORT="${WARDYN_UP_PORT:-8080}"
BASE_URL="http://127.0.0.1:${PORT}"
log "Waiting for ${BASE_URL}/healthz"
wait_healthy "${BASE_URL}" 60 1 || die "wardyn-desktop.sh: wardynd did not become healthy (docker compose -p wardyn-desktop logs wardynd)"
log "wardynd healthy"

# Idempotent site-config apply. site-config.json is a full-document REPLACE
# (docs/DESKTOP.md "Posture switches are env vars, never site-config"), so
# re-applying the same file on every tick is a safe no-op, not accumulation.
# Runs the CLI baked into the image, in-container: local mode trusts the
# loopback-equivalent peer with no token needed (WARDYN_LOCAL_TRUST_FORWARDER
# in the base compose file); the SSO envelope variant has no CLI-usable
# credential here and is skipped with a warning — a human applies it via the
# console after signing in.
if [ -f "${SITE_CONFIG}" ]; then
  if [ "$(env_get "${ENV_FILE}" WARDYN_LOCAL_MODE)" = "true" ]; then
    compose exec -T wardynd /usr/local/bin/wardyn site-config apply "${SITE_CONFIG}" \
      || warn "wardyn-desktop.sh: site-config apply failed (non-fatal — the stack is up; see docker compose -p wardyn-desktop logs wardynd)"
  else
    warn "wardyn-desktop.sh: WARDYN_LOCAL_MODE is not 'true' — skipping automatic site-config apply (the SSO envelope variant has no CLI-usable credential here; apply ${SITE_CONFIG} via the console after signing in)"
  fi
else
  log "No ${SITE_CONFIG} delivered yet — skipping site-config apply"
fi

log "wardyn-desktop: up (${BASE_URL})"
