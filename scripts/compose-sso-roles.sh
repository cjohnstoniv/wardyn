#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# The role walk on the two COMPOSE-shaped SSO deployments, hermetic and local:
#
#   mprime       the desktop member-mode envelope (deploy/desktop/
#                wardyn.env.m-prime.example: WARDYN_MEMBER_MODE, default role
#                member, the MDM-held admin token)
#   compose-sso  the plain `docker compose --profile sso` stack
#                (deploy/compose/.env.example's role map)
#
# Each shape is the SHIPPED env file with test/sso-roles/<shape>.env layered on
# top (only the role-map / allowlist rows the Dex cast needs), plus this
# script's runtime values (ports, token, images, secrets). Dex is the compose
# stack's own service with test/sso-roles/dex.yaml swapped in. Then
# ui/e2e/live/sso-roles.spec.ts signs every identity in through the real console
# — the same spec the kind walk runs on its two chart renders.
#
# ISOLATION: own compose project per shape (wardyn-roles-<shape>), own
# WARDYN_NS (container names + networks), own ports (18480 console, 15576 Dex,
# 15442 postgres, 15020 registry, 12242 ssh, 18481 ui-sandbox), `down -v` on
# exit. It never stops, reuses or retags anything a developer stack holds:
# wardynd/proxy run as the :quickstart tags (build them from this tree first, as
# the kind walk does), and no sandbox is ever launched, so no agent image is
# touched. It refuses to start while another e2e backend or walk is up.
#
# GUARD: self-skips unless WARDYN_TEST_SSO_ROLES=1. Args: shapes to run
# (default: mprime compose-sso).
set -uo pipefail

if [[ "${WARDYN_TEST_SSO_ROLES:-}" != "1" ]]; then
  echo "compose-sso-roles: set WARDYN_TEST_SSO_ROLES=1 to bring up the compose SSO role walk (skipping)."
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
. "${ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

UP_PORT=18480
DEX_PORT=15576   # must agree with test/sso-roles/dex.yaml's issuer + redirect
PG_PORT=15442
REGISTRY_PORT=15020
SSH_PORT=12242
UISANDBOX_PORT=18481
WARDYND_IMAGE="${WARDYN_ROLES_WARDYND_IMAGE:-wardyn/wardynd:quickstart}"
PROXY_IMAGE="${WARDYN_ROLES_PROXY_IMAGE:-wardyn/wardyn-proxy:quickstart}"
EVIDENCE_DIR="${WARDYN_ROLES_EVIDENCE:-${ROOT}/local/evidence/compose-sso-roles}"
SHAPES=("$@")
[[ ${#SHAPES[@]} -gt 0 ]] || SHAPES=(mprime compose-sso)
mkdir -p "${EVIDENCE_DIR}"

# ── preflight: one e2e backend at a time, and nothing on our ports ──────────
for p in "${UP_PORT}" "${DEX_PORT}" "${PG_PORT}" "${REGISTRY_PORT}" "${SSH_PORT}" "${UISANDBOX_PORT}"; do
  ss -ltn "( sport = :${p} )" | grep -q LISTEN && die "port ${p} is taken — another stack? refusing to start"
done
ss -ltn "( sport = :8088 )" | grep -q LISTEN && die "the hermetic e2e backend (:8088) is up — one e2e backend at a time"
pgrep -f "[k]ind-sso-walk.sh" >/dev/null && die "a kind SSO walk is running — one e2e backend at a time"
for img in "${WARDYND_IMAGE}" "${PROXY_IMAGE}"; do
  docker image inspect "${img}" >/dev/null 2>&1 || die "${img} is not on ${DOCKER_HOST:-the default daemon} — build it from this tree (deploy/compose/Dockerfile.wardynd / Dockerfile.proxy)"
done

TOKEN="roles-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
AGE_KEY="$(docker run --rm --pull=never --entrypoint /wardynd "${WARDYND_IMAGE}" -gen-age-key | grep -E '^AGE-SECRET-KEY-' | head -1)"
[[ -n "${AGE_KEY}" ]] || die "could not mint an age key with ${WARDYND_IMAGE}"
SCRATCH="$(mktemp -d)"

PROJECT=""
compose() {
  docker compose -p "${PROJECT}" --env-file "${SCRATCH}/${PROJECT}.env" \
    -f deploy/compose/docker-compose.yaml -f test/sso-roles/compose-override.yaml --profile sso "$@"
}
teardown() {
  [[ -n "${PROJECT}" ]] && compose down -v --remove-orphans >"${EVIDENCE_DIR}/${PROJECT}-down.log" 2>&1
  PROJECT=""
}
trap 'teardown; rm -rf "${SCRATCH}"' EXIT

# envfile <base> <overrides> <out>: the base's uncommented KEY=VALUE lines, a key
# in overrides (or in the runtime block) replacing the base's — last one wins,
# written once, so compose never sees a duplicate.
envfile() {
  local runtime="${SCRATCH}/runtime.env"
  cat >"${runtime}" <<EOF
WARDYN_NS=${PROJECT}
WARDYN_UP_PORT=${UP_PORT}
WARDYN_DEX_PORT=${DEX_PORT}
WARDYN_PG_PORT=${PG_PORT}
WARDYN_REGISTRY_PORT=${REGISTRY_PORT}
WARDYN_SSH_PORT=${SSH_PORT}
WARDYN_UI_SANDBOX_PORT=${UISANDBOX_PORT}
WARDYN_WARDYND_IMAGE=${WARDYND_IMAGE}
WARDYN_PROXY_IMAGE=${PROXY_IMAGE}
WARDYN_ADMIN_TOKEN=${TOKEN}
WARDYN_AGE_KEY=${AGE_KEY}
WARDYN_OIDC_ISSUER=http://localhost:${DEX_PORT}
WARDYN_OIDC_CLIENT_ID=wardyn
WARDYN_OIDC_CLIENT_SECRET=wardyn-roles-oidc-secret
WARDYN_OIDC_REDIRECT_URL=http://localhost:${UP_PORT}/auth/callback
WARDYN_OIDC_EMAIL_DOMAINS=wardyn.local
ROLES_DEX_CONFIG=${ROOT}/test/sso-roles/dex.yaml
$(cat "${SCRATCH}/extra.env" 2>/dev/null)
EOF
  grep -hE '^[A-Z_]+=' "$1" "$2" "${runtime}" \
    | awk -F= '{ v[$1] = $0; if (!($1 in seen)) { seen[$1] = 1; order[++n] = $1 } } END { for (i = 1; i <= n; i++) print v[order[i]] }' >"$3"
}

rc=0
for shape in "${SHAPES[@]}"; do
  PROJECT="wardyn-roles-${shape}"
  : >"${SCRATCH}/extra.env"
  case "${shape}" in
    mprime)
      base=deploy/desktop/wardyn.env.m-prime.example
      # The envelope's host-path and fleet-image values have no meaning on this
      # box: its managed ceiling gets a real policy.json in a scratch managed
      # dir, its REPLACE_ME image pins fall back to compose's defaults (no run
      # is ever launched), and its /Users/Shared roots move to scratch.
      mkdir -p "${SCRATCH}/managed" "${SCRATCH}/src"
      cp examples/policies/demo.json "${SCRATCH}/managed/policy.json"
      cat >"${SCRATCH}/extra.env" <<EOF
WARDYN_MANAGED_DIR=${SCRATCH}/managed
WARDYN_AGENT_IMAGES=
WARDYN_WORKSPACES_ROOT=${SCRATCH}/src
WARDYN_MEMBER_WORKSPACE_ROOTS=${SCRATCH}/src
WARDYN_MEMBER_WRITABLE_ROOTS=${SCRATCH}/src
WARDYN_MEMBER_WRITABLE_DENY=${SCRATCH}/src/.git
WARDYN_LOCAL_OPERATOR=local:roles-harness
WARDYN_SSH_ADVERTISE=127.0.0.1:${SSH_PORT}
EOF
      ;;
    compose-sso) base=deploy/compose/.env.example ;;
    *) die "unknown shape '${shape}' (mprime | compose-sso)" ;;
  esac
  envfile "${base}" "test/sso-roles/${shape}.env" "${SCRATCH}/${PROJECT}.env"
  # The effective role config, for the record (no secrets in these keys).
  grep -E '^WARDYN_(MEMBER_MODE|OIDC_ROLE_MAP|OIDC_DEFAULT_ROLE|OIDC_OPERATOR_EMAILS|LOCAL_MODE)=' \
    "${SCRATCH}/${PROJECT}.env" >"${EVIDENCE_DIR}/${shape}-role-config.env"

  log "${shape}: bringing up ${PROJECT} (postgres + dex + wardynd) on :${UP_PORT}"
  if ! compose up -d postgres dex wardynd >"${EVIDENCE_DIR}/${shape}-up.log" 2>&1; then
    tail -20 "${EVIDENCE_DIR}/${shape}-up.log" >&2
    compose logs wardynd >"${EVIDENCE_DIR}/${shape}-wardynd.log" 2>&1
    echo "FAIL: ${shape} did not come up (see ${EVIDENCE_DIR}/${shape}-wardynd.log)" >&2
    rc=1; teardown; continue
  fi
  ok=""
  for _ in $(seq 1 90); do
    curl -sf "http://localhost:${UP_PORT}/healthz" >"${EVIDENCE_DIR}/${shape}-healthz.json" 2>/dev/null && { ok=1; break; }
    sleep 2
  done
  if [[ -z "${ok}" ]]; then
    compose logs wardynd >"${EVIDENCE_DIR}/${shape}-wardynd.log" 2>&1
    echo "FAIL: ${shape} never answered /healthz (see ${EVIDENCE_DIR}/${shape}-wardynd.log)" >&2
    rc=1; teardown; continue
  fi

  log "${shape}: role walk (ui/e2e/live/sso-roles.spec.ts)"
  WARDYN_E2E_LIVE_BASE_URL="http://localhost:${UP_PORT}" WARDYN_LIVE_ROLES_RENDER="${shape}" \
    WARDYN_LIVE_ADMIN_TOKEN="${TOKEN}" \
    ./scripts/run-ui-e2e.sh sso-roles 2>&1 | tee "${EVIDENCE_DIR}/${shape}.log"
  [[ "${PIPESTATUS[0]}" -eq 0 ]] || rc=1
  compose logs wardynd >"${EVIDENCE_DIR}/${shape}-wardynd.log" 2>&1
  teardown
done

[[ "${rc}" -eq 0 ]] || { echo "compose-sso-roles: FAILED (see ${EVIDENCE_DIR})" >&2; exit 1; }
echo "compose-sso-roles: PASS (${SHAPES[*]}) — evidence in ${EVIDENCE_DIR}"
