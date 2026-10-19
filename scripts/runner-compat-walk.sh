#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Runner/organisation version compatibility walk: this tree (the candidate)
# against the newest published release of a series (default 0.9; release
# candidates excluded). Every release is flagged pre-release on GitHub, so the
# tag's shape (vX.Y.Z, no suffix) decides what counts as released, not the flag.
#
#   1. a released organisation (the released wardynd and proxy images): the
#      candidate wardyn-runnerd registers, is claimed, goes online and runs one
#      exec run to COMPLETED. Below 0.9.3 the run must instead FAIL with the
#      refusal that names its fix: a 0.9.3 runner hands its proxy a setting
#      (decision_sink) an older proxy image refuses, which is the documented
#      "upgrade the organisation before its laptop runners" break, so the run is
#      FAILED and its reason says "upgrade the organisation before its runners".
#      Any other outcome, a completed run included, fails the step;
#   2. rollback on that organisation: a candidate runner's state directory is
#      served by the released wardyn-runnerd, which must come online and run one
#      exec run to COMPLETED. A release before 0.9.3 cannot take back a runner
#      that has retired a run (docs/runner-registration.md, rolling back), so
#      against one the rolled-back runner has only registered and gone online;
#      from 0.9.3 on it is the runner from step 1, after its run, stopped only
#      once its audit spool is acknowledged through its last record (the same
#      page's condition for that rollback);
#   3. a candidate organisation (images built from this tree): the released
#      wardyn-runnerd registers, is claimed, goes online and runs one exec run
#      to COMPLETED.
# Each run is checked on the runner it should have landed on, and for an
# egress decision its proxy made and the runner shipped over its session.
#
# WHAT IT IS NOT. A smoke of the runner wire, not a conformance suite: one exec
# run per step (released agent-base image, no model provider, no repository).
# Released binaries come from the release's assets, checked against its
# SHA256SUMS; released images from ghcr.io. The candidate's compose file runs
# both organisations: it has not changed since 0.9.2, so a released wardynd
# reads the configuration it shipped with. Recheck that if it changes.
#
# TOPOLOGY. One native Docker engine (a hosted CI runner's): the organisation
# is a compose project published on that engine's bridge gateway with TLS from
# a CA this walk mints; wardyn-runnerd runs on the host, and its run relay takes
# the gateway's :8443 (compose's WARDYN_CONTROL_PLANE_URL port), which nothing
# else may hold. Docker Desktop is refused: scripts/runner-laptop-walk.sh covers
# that engine.
#
# GUARD: self-skips (77) unless WARDYN_TEST_DOCKER=1. WARDYN_COMPAT_RELEASED
# pins the released tag instead of resolving it; WARDYN_COMPAT_DOCKER names the
# engine (default DOCKER_HOST, else /var/run/docker.sock). Removes everything it
# creates.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}" || exit 1
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

[[ "${WARDYN_TEST_DOCKER:-}" == "1" ]] || skip_lane "runner-compat-walk: set WARDYN_TEST_DOCKER=1 to run it (skipping)."
for bin in docker curl jq go openssl ss sha256sum; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done
export DOCKER_HOST="${WARDYN_COMPAT_DOCKER:-${DOCKER_HOST:-unix:///var/run/docker.sock}}"
engine_os="$(docker info --format '{{.OperatingSystem}}' 2>/dev/null)" || skip_lane "runner-compat-walk: no engine at ${DOCKER_HOST} (skipping)."
[[ "${engine_os}" != "Docker Desktop" ]] || skip_lane "runner-compat-walk: ${DOCKER_HOST} is Docker Desktop; this walk needs a native engine (skipping)."

REPO="${WARDYN_COMPAT_REPO:-cjohnstoniv/wardyn}"
REGISTRY="ghcr.io/${REPO%%/*}"
SERIES="${WARDYN_COMPAT_SERIES:-0.9}"
PROJECT="${WARDYN_COMPAT_PROJECT:-wardyn-compat}"
ORG_HOST="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
ORG_PORT="${WARDYN_COMPAT_ORG_PORT:-18643}"
ORG_URL="https://${ORG_HOST}:${ORG_PORT}"
RELAY_PORT=8443
[[ -n "${ORG_HOST}" && "${ORG_HOST}" != 127.* ]] || die "the bridge gateway '${ORG_HOST}' is not a non-loopback address"
if ss -Hltn "sport = :${RELAY_PORT}" | grep . >/dev/null; then die "something already listens on :${RELAY_PORT}, the relay's port"; fi

ADMIN_TOKEN="demo-admin-token" # compose's own default
MEMBER="compat-walk-member"
MEMBER_TOKEN="wdn_$(openssl rand -hex 32)"
RUNNER_NET="${PROJECT}-runner-internal"
CAND_WARDYND="wardyn/wardynd:${PROJECT}"
CAND_PROXY="wardyn/wardyn-proxy:${PROJECT}"
PG_CONTAINER="${PROJECT}-postgres"
EVIDENCE_DIR="${WARDYN_COMPAT_EVIDENCE:-${ROOT}/local/evidence/runner-compat-walk}"
mkdir -p "${EVIDENCE_DIR}"
TMP="$(mktemp -d /tmp/wardyn-compat-walk.XXXXXX)"
SERVE_PID="" ORG_WARDYND="" ORG_PROXY="" RIDS=""
FAILED=0
pass() { printf '  [pass] %s\n' "$*"; }
fail() { printf '  [FAIL] %s\n' "$*"; FAILED=1; }

compose() {
  COMPOSE_PROJECT_NAME="${PROJECT}" WARDYN_NS="${PROJECT}" \
    WARDYN_UP_PORT=0 WARDYN_PG_PORT=0 WARDYN_REGISTRY_PORT=0 WARDYN_SSH_PORT=0 WARDYN_UI_SANDBOX_PORT=0 \
    WARDYN_DOCKER_SOCK="${DOCKER_HOST#unix://}" WARDYN_WARDYND_IMAGE="${ORG_WARDYND}" WARDYN_PROXY_IMAGE="${ORG_PROXY}" \
    WARDYN_AGENT_IMAGES="$(jq -nc --arg i "${AGENT_IMAGE}" '{"claude-code":$i}')" \
    docker compose -p "${PROJECT}" -f deploy/compose/docker-compose.yaml -f "${TMP}/override.yaml" "$@"
}

# api TOKEN METHOD PATH [BODY] -> body in $TMP/resp.json, status on stdout.
api() {
  local args=(-sS --max-time 15 -o "${TMP}/resp.json" -w '%{http_code}' -X "$2" "${ORG_URL}$3" -H "Authorization: Bearer $1")
  [[ -n "${4:-}" ]] && args+=(-H "Content-Type: application/json" -d "$4")
  curl "${args[@]}"
}
psql1() { docker exec "${PG_CONTAINER}" psql -U wardyn -d wardyn -tAc "$1"; }

stop_serve() {
  [[ -n "${SERVE_PID}" ]] || return 0
  kill -TERM "${SERVE_PID}" 2>/dev/null
  wait "${SERVE_PID}"
  echo "serve stopped (exit $?)"
  SERVE_PID=""
}

# Runs and their networks are labelled by runner, so a runner id removes what it made.
clean_runners() {
  local rid
  for rid in ${RIDS}; do
    docker ps -aq --filter "label=wardyn.runner-id=${rid}" | xargs -r docker rm -f >/dev/null 2>&1
    docker volume ls -q --filter "label=wardyn.runner-id=${rid}" | xargs -r docker volume rm >/dev/null 2>&1
    docker network ls -q --filter "label=wardyn.runner-id=${rid}" | xargs -r docker network rm >/dev/null 2>&1
  done
}

org_down() {
  [[ -n "${ORG_WARDYND}" ]] || return 0
  compose logs wardynd >"${EVIDENCE_DIR}/wardynd-$1.log" 2>&1
  clean_runners
  compose down --volumes >/dev/null 2>&1
  ORG_WARDYND=""
}

cleanup() {
  stop_serve
  org_down last
  docker network rm "${RUNNER_NET}" >/dev/null 2>&1
  [[ "${WARDYN_COMPAT_KEEP_IMAGES:-}" == "1" ]] || docker rmi "${CAND_WARDYND}" "${CAND_PROXY}" >/dev/null 2>&1
  rm -rf "${TMP}"
  echo "cleaned up ${PROJECT}"
}
trap cleanup EXIT

# org_up LABEL WARDYND PROXY: a fresh organisation with runners on, a member token, and the self-host grant.
org_up() {
  ORG_WARDYND="$2" ORG_PROXY="$3"
  log "bringing up the $1 organisation (${ORG_WARDYND}) at ${ORG_URL}"
  compose up -d --no-build wardynd >"${EVIDENCE_DIR}/compose-up-$1.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/compose-up-$1.log"; die "compose up failed"; }
  wait_healthy "${ORG_URL}" 90 2 || die "the $1 organisation never answered ${ORG_URL}/healthz"
  api "${ADMIN_TOKEN}" POST /api/v1/setup/onboarding-complete >/dev/null
  [[ "$(api "${ADMIN_TOKEN}" PUT /api/v1/runners/settings '{"enabled":true}')" == 200 ]] || die "enabling runners failed: $(cat "${TMP}/resp.json")"
  psql1 "INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256)
    VALUES (gen_random_uuid(), '${MEMBER}', '${MEMBER}@walk.wardyn.invalid', 'user', 'standard', '[]', false, 'compat walk',
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
  WARDYN_URL="${ORG_URL}" WARDYN_TOKEN="${MEMBER_TOKEN}" "$3" runner claim --url "${ORG_URL}" --state-dir "$4" --pool Compat --default \
    >>"${EVIDENCE_DIR}/register-$1.log" 2>&1 || { tail -5 "${EVIDENCE_DIR}/register-$1.log"; die "$1: claim failed"; }
  pass "$1: registered as ${RID} with $("$2" --version 2>&1 | head -1) and claimed into Compat"
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

# smoke LABEL CLI RID: one exec run on the member's default pool lands on RID and completes, and the runner
# ships its proxy's deny for the walk's denied host (which also drains the runner's audit spool before a stop).
smoke() {
  local rc run_id deny=""
  WARDYN_URL="${ORG_URL}" WARDYN_TOKEN="${MEMBER_TOKEN}" "$2" run --agent claude-code --task-mode exec --placement local \
    --policy-file "${TMP}/policy.json" --wait --timeout 8m --json \
    --task "echo compat-walk-marker; curl -sS -m 10 -o /dev/null https://denied.compat-walk.invalid/ || true" \
    >"${TMP}/run.json" 2>"${TMP}/run.err"
  rc=$?
  run_id="$(jq -r '.id // empty' "${TMP}/run.json" 2>/dev/null)"
  [[ -n "${run_id}" ]] || { fail "$1: no run id (exit ${rc}): $(tail -3 "${TMP}/run.err")"; return 1; }
  api "${MEMBER_TOKEN}" GET "/api/v1/runs/${run_id}" >/dev/null
  cp "${TMP}/resp.json" "${EVIDENCE_DIR}/run-$1.json"
  if [[ "${rc}" -eq 0 ]] && jq -e --arg r "$3" '.state == "COMPLETED" and .runner_id == $r' "${TMP}/resp.json" >/dev/null; then
    pass "$1: run ${run_id} COMPLETED on runner $3"
  else fail "$1: run ${run_id} exit ${rc}, $(jq -c '{state, runner_id}' "${TMP}/resp.json"); $(tail -3 "${TMP}/run.err")"; return 1; fi
  for _ in $(seq 1 15); do
    deny="$(psql1 "SELECT target FROM audit_events WHERE run_id = '${run_id}' AND action = 'egress.deny'
      AND actor = 'runner:$3' AND target LIKE 'denied.compat-walk.invalid%' LIMIT 1")"
    [[ -n "${deny}" ]] && break
    sleep 2
  done
  if [[ -n "${deny}" ]]; then pass "$1: the run's egress.deny ${deny} arrived from runner:$3"
  else fail "$1: no egress.deny for the denied host from runner:$3"; return 1; fi
}

# smoke_refused LABEL CLI RID: below 0.9.3, the run on a candidate runner under a released organisation fails at
# its proxy's config load, and the failure names the fix.
smoke_refused() {
  local rc run_id
  WARDYN_URL="${ORG_URL}" WARDYN_TOKEN="${MEMBER_TOKEN}" "$2" run --agent claude-code --task-mode exec --placement local \
    --policy-file "${TMP}/policy.json" --wait --timeout 8m --json --task "echo compat-walk-marker" \
    >"${TMP}/run.json" 2>"${TMP}/run.err"
  rc=$?
  run_id="$(jq -r '.id // empty' "${TMP}/run.json" 2>/dev/null)"
  [[ -n "${run_id}" ]] || { fail "$1: no run id (exit ${rc}): $(tail -3 "${TMP}/run.err")"; return 1; }
  api "${MEMBER_TOKEN}" GET "/api/v1/runs/${run_id}" >/dev/null
  cp "${TMP}/resp.json" "${EVIDENCE_DIR}/run-$1.json"
  cp "${TMP}/run.err" "${EVIDENCE_DIR}/run-$1.err"
  if [[ "${rc}" -ne 0 ]] && jq -e --arg r "$3" '.state == "FAILED" and .runner_id == $r' "${TMP}/resp.json" >/dev/null \
    && grep -q 'reason: .*upgrade the organisation before its runners' "${TMP}/run.err"; then
    pass "$1: run ${run_id} FAILED with the refusal that names its fix: $(grep -m1 -o 'the proxy image is older[^;]*' "${TMP}/run.err")"
  else fail "$1: run ${run_id} exit ${rc}, $(jq -c '{state, runner_id}' "${TMP}/resp.json"); want FAILED naming the fix, got: $(tail -3 "${TMP}/run.err")"; return 1; fi
}

# spool_drained LABEL STATE: wait (serve still running) until every run spool in the runner's state
# directory is acknowledged through its last record, the condition docs/runner-registration.md sets for
# rolling a runner back. A spool already erased has nothing left to send.
spool_drained() {
  local f pending=()
  for _ in $(seq 1 30); do
    pending=()
    for f in "$2"/spools/*/state.json; do
      [[ -f "${f}" ]] || continue
      jq -e '.ack.Seq >= .last' "${f}" >/dev/null || pending+=("${f}")
    done
    [[ "${#pending[@]}" -eq 0 ]] && { pass "$1: audit spool acknowledged through its last record"; return 0; }
    sleep 2
  done
  fail "$1: audit spool still unacknowledged after 60 s: ${pending[*]} $(jq -c '{last, ack}' "${pending[@]}" 2>/dev/null | tr '\n' ' ')"
  return 1
}

revoke() {
  [[ "$(api "${MEMBER_TOKEN}" DELETE "/api/v1/me/runners/$1")" == 200 ]] || fail "revoking $1: $(cat "${TMP}/resp.json")"
}

# A version at or above 0.9.3 (X.Y.Z only).
from_093() { [[ "$(printf '%s\n0.9.3\n' "$1" | sort -V | head -1)" == 0.9.3 ]]; }

log "resolving the newest published ${SERIES}.Z release of ${REPO}"
REL_TAG="${WARDYN_COMPAT_RELEASED:-}"
if [[ -z "${REL_TAG}" ]]; then
  auth=()
  [[ -n "${GH_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer ${GH_TOKEN}")
  REL_TAG="$(curl -fsSL "${auth[@]}" "https://api.github.com/repos/${REPO}/releases?per_page=100" \
    | jq -r '.[] | select(.draft == false) | .tag_name' | grep -E "^v${SERIES//./\\.}\.[0-9]+$" | sort -V | tail -1)"
fi
[[ "${REL_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "no published ${SERIES}.Z release found (got '${REL_TAG}')"
REL_VER="${REL_TAG#v}"
AGENT_IMAGE="${REGISTRY}/agent-base:${REL_VER}"
REL_WARDYND="${REGISTRY}/wardynd:${REL_VER}"
REL_PROXY="${REGISTRY}/wardyn-proxy:${REL_VER}"
echo "released: ${REL_TAG}"

log "downloading the released CLI and wardyn-runnerd, checked against SHA256SUMS"
arch="$(go env GOARCH)"
mkdir -p "${TMP}/rel"
for f in SHA256SUMS "wardyn-linux-${arch}" "wardyn-runnerd-linux-${arch}"; do
  curl -fsSL -o "${TMP}/rel/${f}" "https://github.com/${REPO}/releases/download/${REL_TAG}/${f}" || die "downloading ${f} from ${REL_TAG} failed"
done
for f in "wardyn-linux-${arch}" "wardyn-runnerd-linux-${arch}"; do
  grep -q "[ /]${f}\$" "${TMP}/rel/SHA256SUMS" || die "${f} is not listed in ${REL_TAG}'s SHA256SUMS"
done
(cd "${TMP}/rel" && sha256sum -c --ignore-missing --quiet SHA256SUMS) || die "a released binary does not match SHA256SUMS"
chmod +x "${TMP}/rel/wardyn-linux-${arch}" "${TMP}/rel/wardyn-runnerd-linux-${arch}"
REL_CLI="${TMP}/rel/wardyn-linux-${arch}" REL_RUNNERD="${TMP}/rel/wardyn-runnerd-linux-${arch}"

log "pulling the released images"
for img in "${REL_WARDYND}" "${REL_PROXY}" "${AGENT_IMAGE}"; do
  docker pull -q "${img}" >/dev/null || die "docker pull ${img} failed"
done

log "building the candidate CLI, wardyn-runnerd and images from this tree"
go build -o "${TMP}/wardyn" ./cmd/wardyn || die "go build wardyn failed"
CGO_ENABLED=0 go build -tags docker -o "${TMP}/wardyn-runnerd" ./cmd/wardyn-runnerd || die "go build wardyn-runnerd failed"
CAND_CLI="${TMP}/wardyn" CAND_RUNNERD="${TMP}/wardyn-runnerd"
docker build -q -f deploy/compose/Dockerfile.wardynd -t "${CAND_WARDYND}" . >"${EVIDENCE_DIR}/build-wardynd.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/build-wardynd.log"; die "wardynd image build failed"; }
docker build -q -f deploy/compose/Dockerfile.proxy -t "${CAND_PROXY}" . >"${EVIDENCE_DIR}/build-proxy.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/build-proxy.log"; die "proxy image build failed"; }

log "minting the walk CA and the organisation's certificate for ${ORG_HOST}"
T="${TMP}/tls"
mkdir -p "${T}" || die "no tls dir"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=compat-walk CA" \
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
cat >"${TMP}/policy.json" <<'EOF'
{"allowed_domains": [], "denied_domains": [], "first_use_approval": "always_deny", "allowed_methods": [],
 "min_confinement_class": "CC1", "auto_stop_after_sec": 0}
EOF

log "STEP 1: candidate wardyn-runnerd against the released organisation (${REL_TAG})"
org_up released "${REL_WARDYND}" "${REL_PROXY}"
register cand-on-rel "${CAND_RUNNERD}" "${CAND_CLI}" "${TMP}/state-cand-on-rel"
CAND_RID="${RID}"
if from_093 "${REL_VER}"; then
  serve "${CAND_RUNNERD}" "${TMP}/state-cand-on-rel" cand-on-rel \
    && { smoke cand-on-rel "${REL_CLI}" "${CAND_RID}" && spool_drained cand-on-rel "${TMP}/state-cand-on-rel"; stop_serve; }
else
  echo "${REL_VER} predates decision_sink: the run must fail naming the fix (upgrade the organisation first)"
  serve "${CAND_RUNNERD}" "${TMP}/state-cand-on-rel" cand-on-rel && { smoke_refused cand-on-rel "${REL_CLI}" "${CAND_RID}"; stop_serve; }
fi

log "STEP 2: rollback, the released wardyn-runnerd serving a candidate runner's state directory"
if from_093 "${REL_VER}"; then
  ROLLBACK_STATE="${TMP}/state-cand-on-rel"
  echo "${REL_VER} takes back a runner that has retired a run: rolling back the runner from step 1"
else
  echo "${REL_VER} cannot take back a runner that has retired a run: rolling back a fresh candidate runner"
  revoke "${CAND_RID}"
  ROLLBACK_STATE="${TMP}/state-rollback"
  register rollback "${CAND_RUNNERD}" "${CAND_CLI}" "${ROLLBACK_STATE}"
  serve "${CAND_RUNNERD}" "${ROLLBACK_STATE}" rollback-candidate && stop_serve
fi
ROLLBACK_RID="$(jq -r .runner_id "${ROLLBACK_STATE}/runner.json")"
serve "${REL_RUNNERD}" "${ROLLBACK_STATE}" rollback-released && { smoke rollback "${REL_CLI}" "${ROLLBACK_RID}"; stop_serve; }
org_down released

log "STEP 3: released wardyn-runnerd (${REL_TAG}) against the candidate organisation"
org_up candidate "${CAND_WARDYND}" "${CAND_PROXY}"
register rel-on-cand "${REL_RUNNERD}" "${REL_CLI}" "${TMP}/state-rel-on-cand"
serve "${REL_RUNNERD}" "${TMP}/state-rel-on-cand" rel-on-cand && { smoke rel-on-cand "${CAND_CLI}" "${RID}"; stop_serve; }
org_down candidate

[[ "${FAILED}" -eq 0 ]] || die "runner-compat-walk: at least one step FAILED against ${REL_TAG} (evidence in ${EVIDENCE_DIR})"
echo "runner-compat-walk: PASS against ${REL_TAG} — evidence in ${EVIDENCE_DIR}"
