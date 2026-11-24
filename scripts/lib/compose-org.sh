# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash

# scripts/lib/compose-org.sh: a compose organisation on this engine's bridge gateway, served over TLS from a CA
# the walk mints, with runners on, a member token holding the self-host grant, and wardyn-runnerd's register and
# serve on the host. Sourced, not exec'd. The caller sets, before sourcing: PROJECT, ORG_HOST, ORG_PORT, ORG_URL,
# ADMIN_TOKEN, MEMBER, MEMBER_TOKEN, RUNNER_NET, PG_CONTAINER, POOL, EVIDENCE_DIR, TMP, and SERVE_PID, ORG_WARDYND,
# ORG_PROXY and RIDS empty; and AGENT_IMAGE before org_up. It defines pass and fail itself only if the caller has
# not. scripts/lib/common.sh and scripts/lib/walk-api.sh must already be sourced.
declare -F pass >/dev/null || pass() { printf '  [pass] %s\n' "$*"; }
declare -F fail >/dev/null || fail() { printf '  [FAIL] %s\n' "$*"; FAILED=1; }

compose() {
  COMPOSE_PROJECT_NAME="${PROJECT}" WARDYN_NS="${PROJECT}" \
    WARDYN_UP_PORT=0 WARDYN_PG_PORT=0 WARDYN_REGISTRY_PORT=0 WARDYN_SSH_PORT=0 WARDYN_UI_SANDBOX_PORT=0 \
    WARDYN_DOCKER_SOCK="${DOCKER_HOST#unix://}" WARDYN_WARDYND_IMAGE="${ORG_WARDYND}" WARDYN_PROXY_IMAGE="${ORG_PROXY}" \
    WARDYN_AGENT_IMAGES="$(jq -nc --arg i "${AGENT_IMAGE}" '{"claude-code":$i}')" \
    docker compose -p "${PROJECT}" -f deploy/compose/docker-compose.yaml -f "${TMP}/override.yaml" "$@"
}

# api TOKEN METHOD PATH [BODY] -> body in $TMP/resp.json, status on stdout.
api() { walk_api "${ORG_URL}" "$1" "${TMP}/resp.json" 15 "${@:2}"; }
psql1() { docker exec "${PG_CONTAINER}" psql -U wardyn -d wardyn -tAc "$1"; }

stop_serve() {
  [[ -n "${SERVE_PID}" ]] || return 0
  kill -TERM "${SERVE_PID}" 2>/dev/null
  wait "${SERVE_PID}"
  echo "serve stopped (exit $?)"
  SERVE_PID=""
}

# Runs and their networks are labelled by runner, so a runner id removes what it made. The sandboxes the organisation's
# own server runs are labelled with its control-plane network (${PROJECT}-internal), so that label removes those.
clean_runners() {
  local rid dep="wardyn.deployment=${PROJECT}-internal"
  for rid in ${RIDS}; do
    docker ps -aq --filter "label=wardyn.runner-id=${rid}" | xargs -r docker rm -f >/dev/null 2>&1
    docker volume ls -q --filter "label=wardyn.runner-id=${rid}" | xargs -r docker volume rm >/dev/null 2>&1
    docker network ls -q --filter "label=wardyn.runner-id=${rid}" | xargs -r docker network rm >/dev/null 2>&1
  done
  docker ps -aq --filter "label=${dep}" | xargs -r docker rm -f >/dev/null 2>&1
  docker network ls -q --filter "label=${dep}" | xargs -r docker network rm >/dev/null 2>&1
}

org_down() {
  [[ -n "${ORG_WARDYND}" ]] || return 0
  compose logs wardynd >"${EVIDENCE_DIR}/wardynd-$1.log" 2>&1
  clean_runners
  compose down --volumes >/dev/null 2>&1
  ORG_WARDYND=""
}

# org_up LABEL WARDYND PROXY: a fresh organisation with runners on, a member token, and the self-host grant.
org_up() {
  ORG_WARDYND="$2" ORG_PROXY="$3"
  log "bringing up the $1 organisation (${ORG_WARDYND}) at ${ORG_URL}"
  compose up -d --no-build wardynd >"${EVIDENCE_DIR}/compose-up-$1.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/compose-up-$1.log"; die "compose up failed"; }
  wait_healthy "${ORG_URL}" 90 2 || die "the $1 organisation never answered ${ORG_URL}/healthz"
  api "${ADMIN_TOKEN}" POST /api/v1/setup/onboarding-complete >/dev/null
  [[ "$(api "${ADMIN_TOKEN}" PUT /api/v1/runners/settings '{"enabled":true}')" == 200 ]] || die "enabling runners failed: $(cat "${TMP}/resp.json")"
  psql1 "INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256)
    VALUES (gen_random_uuid(), '${MEMBER}', '${MEMBER}@walk.wardyn.invalid', 'user', 'standard', '[]', false, '${PROJECT}',
    '$(printf '%s' "${MEMBER_TOKEN}" | sha256sum | cut -d' ' -f1)')" >/dev/null || die "seeding the member's token failed"
  local code
  code="$(api "${ADMIN_TOKEN}" POST /api/v1/permissions/grants '{"subject_type":"all","subject":"","capability":"feature","value":"self_host_runner","effect":"allow"}')"
  [[ "${code}" == 20[01] ]] || die "the self_host_runner allow was refused: ${code} $(cat "${TMP}/resp.json")"
  pass "$1 organisation healthy, version $(curl -fsS -m 5 "${ORG_URL}/healthz" | jq -r '.version // "unknown"')"
}

# register NAME RUNNERD CLI STATE: self-mint, register, then claim with the CLI of the runner's own version
# (what `setup --claim` does). Sets RID.
register() {
  local code
  code="$(api "${MEMBER_TOKEN}" POST /api/v1/me/runners/tokens '{}')"
  [[ "${code}" == 201 ]] || die "self-mint answered ${code} $(jq -c 'del(.token)' "${TMP}/resp.json")"
  jq -r .token "${TMP}/resp.json" >"${TMP}/reg-token" && chmod 600 "${TMP}/reg-token"
  "$2" register --org "${ORG_URL}" --token-file "${TMP}/reg-token" --state-dir "$4" --name "$1" --total-disk 20 \
    </dev/null >"${EVIDENCE_DIR}/register-$1.log" 2>&1 || { cat "${EVIDENCE_DIR}/register-$1.log"; die "$1: register failed"; }
  RID="$(jq -r .runner_id "$4/runner.json")"
  RIDS="${RIDS} ${RID}"
  WARDYN_URL="${ORG_URL}" WARDYN_TOKEN="${MEMBER_TOKEN}" "$3" runner claim --url "${ORG_URL}" --state-dir "$4" --pool "${POOL}" --default \
    >>"${EVIDENCE_DIR}/register-$1.log" 2>&1 || { tail -5 "${EVIDENCE_DIR}/register-$1.log"; die "$1: claim failed"; }
  pass "$1: registered as ${RID} with $("$2" --version 2>&1 | head -1) and claimed into ${POOL}"
}

# serve RUNNERD STATE LOGNAME: serve in the background until the runner reads online.
serve() {
  "$1" serve --org "${ORG_URL}" --state-dir "$2" --proxy-image "${ORG_PROXY}" --internal-network "${RUNNER_NET}" \
    >"${EVIDENCE_DIR}/serve-$3.log" 2>&1 &
  SERVE_PID=$!
  local rid
  rid="$(jq -r .runner_id "$2/runner.json")"
  for _ in $(seq 1 45); do
    api "${MEMBER_TOKEN}" GET "/api/v1/me/runners/${rid}" >/dev/null
    jq -e '.online == true' "${TMP}/resp.json" >/dev/null && break
    kill -0 "${SERVE_PID}" 2>/dev/null || break
    sleep 2
  done
  if jq -e '.online == true' "${TMP}/resp.json" >/dev/null; then pass "$3: online ($("$1" --version 2>&1 | head -1))"; return 0; fi
  tail -20 "${EVIDENCE_DIR}/serve-$3.log"
  fail "$3: the runner never came online"
  stop_serve
  return 1
}

# org_tls: mint the walk CA and the organisation's certificate for ORG_HOST, trust only that CA from here on, and
# write the compose override that serves wardynd over TLS on ORG_HOST:ORG_PORT.
org_tls() {
  log "minting the walk CA and the organisation's certificate for ${ORG_HOST}"
  T="${TMP}/tls"
  mkdir -p "${T}" || die "no tls dir"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=${PROJECT} CA" \
    -keyout "${T}/ca.key" -out "${T}/ca.crt" 2>/dev/null || die "CA mint failed"
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=${ORG_HOST}" -keyout "${T}/tls.key" -out "${T}/tls.csr" 2>/dev/null || die "CSR failed"
  printf 'subjectAltName=IP:%s,DNS:localhost\n' "${ORG_HOST}" >"${T}/san.ext"
  openssl x509 -req -in "${T}/tls.csr" -CA "${T}/ca.crt" -CAkey "${T}/ca.key" -CAcreateserial -days 1 -extfile "${T}/san.ext" \
    -out "${T}/tls.crt" 2>/dev/null || die "leaf mint failed"
  chmod 600 "${T}/tls.key"
  # From here on only the walk CA is trusted: every TLS call left is to the organisation.
  export SSL_CERT_FILE="${T}/ca.crt" CURL_CA_BUNDLE="${T}/ca.crt"
  cat >"${TMP}/override.yaml" <<EOF
services:
  wardynd:
    ports: ["${ORG_HOST}:${ORG_PORT}:8080"]
    environment:
      WARDYN_TLS_CERT: /walk-tls/tls.crt
      WARDYN_TLS_KEY: /walk-tls/tls.key
      WARDYN_RUNNER_ORG_URL: "${ORG_URL}"
    volumes: ["${TMP}/tls:/walk-tls:ro"]
    healthcheck: {disable: true}
EOF
}
