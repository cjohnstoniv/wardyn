#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Control-plane outage walk (#4080): live runs ride out an outage of the database or the servers.
#
#   cp-outage-walk.sh kind     T1 and T2 on the kind quickstart cluster's server Kubernetes runs, with an
#                              owner SSH session open (WARDYN_TEST_K8S=1, WARDYN_ACK_SHARED_QUICKSTART=1).
#   cp-outage-walk.sh compose  T6 on a Docker runner registered to a compose organisation on this engine
#                              (WARDYN_TEST_DOCKER=1; scripts/lib/compose-org.sh).
#
# T1, Postgres paused 20 s: no run ends, the SSH session stays open and writes one session.unconfirmed
#   rode_through row and no ended row, every allowlisted fetch succeeds, wardynd stays Ready (its readiness path
#   is /healthz), and after the drain the run's egress.allow rows equal its proxy's stdout allow lines.
# T2, Postgres down 5 min: no run ends, the SSH session ends 30 to 80 s after the outage starts (the 30 s check
#   clock plus the 30 s budget, plus a retry and a lookup bound) and one ended row is written, a new session opens
#   within 60 s of recovery, and no run.autostop row is written in the 24 minutes after recovery for a run that sat
#   idle through the outage with a 300 s idle limit (the recovery grace is 25 minutes).
# T6, a Docker runner's wardyn-runnerd restarted while the organisation is down: no two successful fetches are more
#   than 7 s apart (5 s of failure, the 1 s cadence, 1 s of stamp rounding), a run whose saved lapse time has passed
#   has its proxy stopped within 15 s of the restart, and the running run's lease is renewed within 60 s of the
#   organisation coming back.
#
# Both Postgres faults on kind freeze every process in its container (SIGSTOP, then SIGCONT): the quickstart's
# Postgres has no volume, so it cannot be stopped without losing the database. T1 freezes up to three times, 40 s
# apart, until a session check lands inside a freeze: a check runs every 30 s and gives up after 5 s, so one 20 s
# freeze can fall between two checks.
#
# NOT COVERED HERE, so #4080 is not closed by this walk alone: T1's retryable mint (no brokered credential on a
# hosted runner); the terminal, desktop and portal app sessions (a browser leg); T1 and T2 on server Docker; T6 on
# a Kubernetes runner; the laptop and VM legs.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}" || exit 1
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/lib/walk-api.sh
source "${ROOT}/scripts/lib/walk-api.sh"

LEG="${1:-}"
case "${LEG}" in kind | compose) ;; *) die "usage: $0 kind|compose" ;; esac
FAILED=0
pass() { printf '  [pass] %s\n' "$*"; }
fail() { printf '  [FAIL] %s\n' "$*"; FAILED=1; }
step() { printf '\n==> %s\n' "$*"; }
ALLOWED_HOST="proxy.golang.org"
EVIDENCE_DIR="${WARDYN_OUTAGE_EVIDENCE:-${ROOT}/local/evidence/cp-outage-walk-${LEG}}"
mkdir -p "${EVIDENCE_DIR}"

# The sandbox's traffic: one allowlisted fetch a second, logged as "<epoch> <CONNECT answer>", until a stop file
# appears or an hour passes.
FETCH_SCRIPT="$(cat <<'SH'
i=0
while [ "$i" -lt 3600 ] && [ ! -e /tmp/outage-fetch.stop ]; do
  printf '%s %s\n' "$(date +%s)" "$(curl -sS -o /dev/null -m 5 -w '%{http_connect}' https://proxy.golang.org/ 2>/dev/null)" >>/tmp/outage-fetch.log
  i=$((i + 1))
  sleep 1
done
SH
)"

# fetch_window LOG FROM TO: "<fetches> <failed> <longest gap between successes, s>" for the lines stamped FROM..TO.
fetch_window() {
  awk -v from="$2" -v to="$3" '$1 >= from && $1 <= to {
      n++
      if ($2 == "200") { if (prev && $1 - prev > gap) gap = $1 - prev; prev = $1 } else f++
    } END { printf "%d %d %d\n", n, f, gap }' "$1"
}

# ── kind leg: T1 and T2 ─────────────────────────────────────────────────────
kind_leg() {
  [[ "${WARDYN_TEST_K8S:-}" == "1" ]] || skip_lane "cp-outage-walk (kind): set WARDYN_TEST_K8S=1 to run it (skipping)."
  [[ "${WARDYN_ACK_SHARED_QUICKSTART:-}" == "1" ]] || skip_lane "cp-outage-walk (kind): set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm the wardyn-quickstart cluster may have its database frozen (skipping)."
  for bin in kubectl curl jq ssh ssh-keygen; do
    command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
  done
  TMP="$(mktemp -d /tmp/wardyn-cp-outage.XXXXXX)"
  CONTEXT="kind-${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
  NAMESPACE="wardyn"
  RUNS_NAMESPACE="wardyn-runs"
  RELEASE="wardyn"
  BASE="http://127.0.0.1:${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
  k() { kubectl --context "${CONTEXT}" "$@"; }
  ADMIN_TOKEN="$(k -n "${NAMESPACE}" get secret wardyn-auth -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null)"
  [[ -n "${ADMIN_TOKEN}" ]] || die "could not read the admin token from Secret wardyn-auth"
  api() { walk_api "${BASE}" "${ADMIN_TOKEN}" "${TMP}/resp.json" 10 "$@"; }
  psql1() { k -n "${NAMESPACE}" exec deployment/postgres -- psql -U wardyn -d wardyn -tAc "$1" 2>&1; }
  # pg_signal STOP|CONT: every process in the Postgres container but this shell.
  pg_signal() {
    k -n "${NAMESPACE}" exec deployment/postgres -- sh -c \
      'for p in /proc/[0-9]*; do n="${p#/proc/}"; [ "$n" = "$$" ] || kill -s "$1" "$n" 2>/dev/null; done; true' _ "$1"
  }
  run_state() { api GET "/api/v1/runs/$1" >/dev/null; jq -r '.state // empty' "${TMP}/resp.json"; }
  audit_count() { psql1 "SELECT count(*) FROM audit_events WHERE run_id = '$1' AND action = '$2' $3"; }
  ssh_rows() { audit_count "${RUN_A}" session.unconfirmed "AND data->>'surface' = 'ssh' AND data->>'result' = '$1'"; }
  ready_now() {
    [[ "$(k -n "${NAMESPACE}" get deployment "${RELEASE}" -o jsonpath='{.status.readyReplicas}')" == \
      "$(k -n "${NAMESPACE}" get deployment "${RELEASE}" -o jsonpath='{.spec.replicas}')" ]]
  }
  proxy_allow_lines() {
    k -n "${RUNS_NAMESPACE}" logs "wardyn-proxy-$1" 2>/dev/null \
      | jq -Rr --arg h "${ALLOWED_HOST}" 'fromjson? | select(.decision == "allow" and .request.Host == $h) | 1' | wc -l
  }
  # create_run IDLE_LIMIT: an interactive run allowed ALLOWED_HOST, RUNNING, with its pod; prints its id.
  create_run() {
    local code rid state=""
    code="$(api POST /api/v1/runs "$(jq -nc --arg h "${ALLOWED_HOST}" --argjson idle "$1" '{agent:"claude-code",
      repo:"local:cp-outage", interactive:true, inline_policy:{allowed_domains:[$h], first_use_approval:"always_deny",
      min_confinement_class:"CC1", auto_stop_after_sec:$idle, resources:{cpu_millis:250, memory_mib:256}}}')")"
    [[ "${code}" == 20[01] ]] || { cat "${TMP}/resp.json" >&2; die "POST /runs answered ${code}"; }
    rid="$(jq -r '.id' "${TMP}/resp.json")"
    for _ in $(seq 1 90); do
      state="$(run_state "${rid}")"
      [[ "${state}" == RUNNING ]] && break
      sleep 2
    done
    [[ "${state}" == RUNNING ]] || die "run ${rid} never reached RUNNING (last ${state})"
    k -n "${RUNS_NAMESPACE}" wait --for=condition=Ready "pod/wardyn-agent-${rid}" --timeout=120s >/dev/null \
      || die "run ${rid}'s agent pod never became Ready"
    echo "${rid}"
  }

  RUN_A="" RUN_B="" SSH_PID="" OWNER_FP="" FROZEN=""
  cleanup_kind() {
    [[ -z "${FROZEN}" ]] || pg_signal CONT
    [[ -z "${SSH_PID}" ]] || kill "${SSH_PID}" 2>/dev/null
    local rid
    for rid in ${RUN_A} ${RUN_B}; do api POST "/api/v1/runs/${rid}/kill" >/dev/null 2>&1; done
    [[ -z "${OWNER_FP}" ]] || api DELETE "/api/v1/me/ssh-keys/$(printf '%s' "${OWNER_FP}" | jq -sRr @uri)" >/dev/null 2>&1
    rm -rf "${TMP}"
  }
  trap cleanup_kind EXIT

  step "the cluster: wardynd answers as the Kubernetes runner, SSH is on, readiness reads /healthz"
  local healthz advertise ssh_host ssh_port path
  healthz="$(curl -fsS -m 10 "${BASE}/healthz")" || die "${BASE}/healthz did not answer"
  [[ "$(jq -r '.runner // empty' <<<"${healthz}")" == k8s ]] || die "${BASE} is not the Kubernetes install: ${healthz}"
  advertise="$(jq -r '.ssh.advertise_addr // empty' <<<"${healthz}")"
  ssh_host="${advertise%:*}" ssh_port="${advertise##*:}"
  [[ -n "${ssh_host}" && -n "${ssh_port}" ]] || die "no ssh.advertise_addr in /healthz"
  path="$(k -n "${NAMESPACE}" get deployment "${RELEASE}" -o jsonpath='{.spec.template.spec.containers[0].readinessProbe.httpGet.path}')"
  [[ "${path}" == /healthz ]] && pass "wardynd's readiness probe reads /healthz" || fail "wardynd's readiness probe reads '${path}', not /healthz"
  api GET /api/v1/site-config >/dev/null
  [[ "$(jq -r '.sessions.unconfirmed_budget_s // 30' "${TMP}/resp.json")" == 30 ]] || die "the session budget is not 30 s: $(jq -c '.sessions' "${TMP}/resp.json")"

  step "run A (traffic and an owner SSH session)"
  RUN_A="$(create_run -1)" || exit 1
  printf '%s\n' "${FETCH_SCRIPT}" | k -n "${RUNS_NAMESPACE}" exec -i "wardyn-agent-${RUN_A}" -- sh -c 'cat >/tmp/outage-fetch.sh' \
    || die "could not write the fetch loop into run ${RUN_A}"
  k -n "${RUNS_NAMESPACE}" exec "wardyn-agent-${RUN_A}" -- sh -c 'nohup sh /tmp/outage-fetch.sh >/dev/null 2>&1 &' \
    || die "could not start the fetch loop in run ${RUN_A}"
  ssh-keygen -t ed25519 -N "" -q -f "${TMP}/owner_key" -C "cp-outage-walk"
  [[ "$(api POST /api/v1/me/ssh-keys "$(jq -nc --arg pk "$(cat "${TMP}/owner_key.pub")" '{name:"cp-outage-walk",public_key:$pk}')")" == 201 ]] \
    || die "registering the owner key failed: $(cat "${TMP}/resp.json")"
  OWNER_FP="$(ssh-keygen -lf "${TMP}/owner_key.pub" | awk '{print $2}')"
  ssh_a() {
    ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8 \
      -i "${TMP}/owner_key" -p "${ssh_port}" "${RUN_A}@${ssh_host}" "$@"
  }
  ssh_a -T 'sleep 3000' </dev/null >>"${EVIDENCE_DIR}/ssh-session.log" 2>&1 &
  SSH_PID=$!
  for _ in $(seq 1 10); do sleep 1; done
  kill -0 "${SSH_PID}" 2>/dev/null || die "the owner SSH session did not stay open: $(tail -5 "${EVIDENCE_DIR}/ssh-session.log")"
  pass "run ${RUN_A} RUNNING, fetching ${ALLOWED_HOST} every second, owner SSH session open"

  step "T1: Postgres paused 20 s"
  local t1_from cycle ready_lost="" rode=0 ended
  t1_from="$(date +%s)"
  for cycle in 1 2 3; do
    FROZEN=1
    pg_signal STOP || die "could not freeze Postgres"
    for _ in $(seq 1 10); do
      ready_now || ready_lost=1
      sleep 2
    done
    pg_signal CONT || die "could not resume Postgres"
    FROZEN=""
    for _ in $(seq 1 10); do
      rode="$(ssh_rows rode_through)"
      [[ "${rode}" =~ ^[1-9] ]] && break
      sleep 2
    done
    [[ "${rode}" =~ ^[1-9] ]] && break
    echo "  freeze ${cycle}: no session check landed inside it"
  done
  [[ "${rode}" =~ ^[1-9] ]] && pass "T1: the SSH session wrote ${rode} rode_through row(s)" \
    || fail "T1: no session.unconfirmed rode_through row for the SSH session after three freezes (is its check every 30 s?)"
  ended="$(ssh_rows ended)"
  [[ "${ended}" == 0 ]] && pass "T1: no ended row" || fail "T1: ${ended} ended row(s): a 20 s pause ended the session"
  kill -0 "${SSH_PID}" 2>/dev/null && pass "T1: the SSH session is still open" || fail "T1: the SSH session closed"
  [[ "$(run_state "${RUN_A}")" == RUNNING ]] && pass "T1: run ${RUN_A} still RUNNING" || fail "T1: run ${RUN_A} is $(run_state "${RUN_A}")"
  [[ -z "${ready_lost}" ]] && pass "T1: wardynd stayed Ready through every freeze" || fail "T1: wardynd lost a Ready replica during a freeze"
  k -n "${RUNS_NAMESPACE}" exec "wardyn-agent-${RUN_A}" -- touch /tmp/outage-fetch.stop
  for _ in $(seq 1 8); do sleep 1; done
  k -n "${RUNS_NAMESPACE}" exec "wardyn-agent-${RUN_A}" -- cat /tmp/outage-fetch.log >"${EVIDENCE_DIR}/fetch-a.log"
  local n f gap
  read -r n f gap < <(fetch_window "${EVIDENCE_DIR}/fetch-a.log" "${t1_from}" "$(date +%s)")
  [[ "${n}" -ge 20 && "${f}" -eq 0 ]] && pass "T1: ${n} of ${n} allowlisted fetches succeeded" \
    || fail "T1: ${f} of ${n} allowlisted fetches failed (want 0 of at least 20; ${EVIDENCE_DIR}/fetch-a.log)"
  local want="" got=""
  for _ in $(seq 1 45); do
    want="$(proxy_allow_lines "${RUN_A}")"
    got="$(audit_count "${RUN_A}" egress.allow "AND target = '${ALLOWED_HOST}'")"
    [[ "${want}" == "${got}" ]] && break
    sleep 2
  done
  [[ "${want}" -gt 0 && "${want}" == "${got}" ]] && pass "T1: ${got} egress.allow rows equal the proxy's ${want} allow lines" \
    || fail "T1: ${got} egress.allow rows, but the proxy printed ${want} allow lines"

  step "run B (idle, 300 s idle limit) for T2's autostop check"
  RUN_B="$(create_run 300)" || exit 1
  pass "run ${RUN_B} RUNNING"

  step "T2: Postgres down 5 minutes"
  local fs fe ssh_end="" back=""
  fs="$(date +%s)"
  FROZEN=1
  pg_signal STOP || die "could not freeze Postgres"
  for _ in $(seq 1 150); do
    if [[ -z "${ssh_end}" ]] && ! kill -0 "${SSH_PID}" 2>/dev/null; then ssh_end="$(date +%s)"; fi
    sleep 2
  done
  pg_signal CONT || die "could not resume Postgres"
  FROZEN=""
  fe="$(date +%s)"
  SSH_PID=""
  if [[ -n "${ssh_end}" && $((ssh_end - fs)) -ge 30 && $((ssh_end - fs)) -le 80 ]]; then
    pass "T2: the SSH session ended $((ssh_end - fs)) s after the outage began"
  else
    fail "T2: the SSH session ended ${ssh_end:+$((ssh_end - fs)) s after the outage began}${ssh_end:-never}, want 30 to 80 s"
  fi
  while [[ $(($(date +%s) - fe)) -le 60 ]]; do
    if ssh_a true </dev/null >/dev/null 2>&1; then back="$(($(date +%s) - fe))"; break; fi
    sleep 3
  done
  [[ -n "${back}" ]] && pass "T2: a new SSH session opened ${back} s after recovery" || fail "T2: no SSH session opened within 60 s of recovery"
  for _ in $(seq 1 15); do
    ended="$(ssh_rows ended)"
    [[ "${ended}" == 1 ]] && break
    sleep 2
  done
  [[ "${ended}" == 1 ]] && pass "T2: one session.unconfirmed ended row" || fail "T2: ${ended} ended rows for the SSH session, want 1"
  local rid
  for rid in "${RUN_A}" "${RUN_B}"; do
    [[ "$(run_state "${rid}")" == RUNNING ]] && pass "T2: run ${rid} RUNNING after recovery" || fail "T2: run ${rid} is $(run_state "${rid}") after recovery"
  done
  log "T2: watching for run.autostop for 24 minutes after recovery"
  local stops=0
  while [[ $(($(date +%s) - fe)) -lt 1440 ]]; do
    stops="$(psql1 "SELECT count(*) FROM audit_events WHERE action = 'run.autostop' AND run_id IN ('${RUN_A}', '${RUN_B}')")"
    [[ "${stops}" == 0 ]] || break
    sleep 30
  done
  [[ "${stops}" == 0 ]] && pass "T2: no run.autostop in the 24 minutes after recovery" \
    || fail "T2: ${stops} run.autostop row(s) $(( $(date +%s) - fe )) s after recovery: the recovery grace did not hold"
  [[ "$(run_state "${RUN_B}")" == RUNNING ]] && pass "T2: the idle run ${RUN_B} is still RUNNING" || fail "T2: the idle run ${RUN_B} is $(run_state "${RUN_B}")"
}

# ── compose leg: T6 ─────────────────────────────────────────────────────────
compose_leg() {
  [[ "${WARDYN_TEST_DOCKER:-}" == "1" ]] || skip_lane "cp-outage-walk (compose): set WARDYN_TEST_DOCKER=1 to run it (skipping)."
  for bin in docker curl jq go openssl ss; do
    command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
  done
  TMP="$(mktemp -d /tmp/wardyn-cp-outage.XXXXXX)"
  export DOCKER_HOST="${DOCKER_HOST:-unix:///var/run/docker.sock}"
  PROJECT="wardyn-outage"
  ORG_HOST="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
  ORG_PORT="${WARDYN_OUTAGE_ORG_PORT:-18743}"
  ORG_URL="https://${ORG_HOST}:${ORG_PORT}"
  [[ -n "${ORG_HOST}" && "${ORG_HOST}" != 127.* ]] || die "the bridge gateway '${ORG_HOST}' is not a non-loopback address"
  if ss -Hltn "sport = :8443" | grep . >/dev/null; then die "something already listens on :8443, the relay's port"; fi
  ADMIN_TOKEN="demo-admin-token" # compose's own default
  MEMBER="outage-walk-member"
  MEMBER_TOKEN="wdn_$(openssl rand -hex 32)"
  RUNNER_NET="${PROJECT}-runner-internal"
  PG_CONTAINER="${PROJECT}-postgres"
  POOL=Outage
  SERVE_PID="" ORG_WARDYND="" ORG_PROXY="" RIDS="" RID=""
  AGENT_IMAGE="${WARDYN_OUTAGE_AGENT_IMAGE:-wardyn/agent-base:local}"
  # shellcheck source=scripts/lib/compose-org.sh
  source "${ROOT}/scripts/lib/compose-org.sh"
  CAND_WARDYND="wardyn/wardynd:${PROJECT}"
  CAND_PROXY="wardyn/wardyn-proxy:${PROJECT}"
  cleanup_compose() {
    stop_serve
    org_down last
    docker network rm "${RUNNER_NET}" >/dev/null 2>&1
    docker rmi "${CAND_WARDYND}" "${CAND_PROXY}" >/dev/null 2>&1
    rm -rf "${TMP}"
  }
  trap cleanup_compose EXIT

  image_missing "${AGENT_IMAGE}" && die "${AGENT_IMAGE} is not on this engine (make agent-image-base)"
  log "building the CLI, wardyn-runnerd and the organisation's images from this tree"
  go build -o "${TMP}/wardyn" ./cmd/wardyn || die "go build wardyn failed"
  CGO_ENABLED=0 go build -tags docker -o "${TMP}/wardyn-runnerd" ./cmd/wardyn-runnerd || die "go build wardyn-runnerd failed"
  docker build -q -f deploy/compose/Dockerfile.wardynd -t "${CAND_WARDYND}" . >"${EVIDENCE_DIR}/build-wardynd.log" 2>&1 || die "wardynd image build failed"
  docker build -q -f deploy/compose/Dockerfile.proxy -t "${CAND_PROXY}" . >"${EVIDENCE_DIR}/build-proxy.log" 2>&1 || die "proxy image build failed"
  org_tls
  jq -n --arg h "${ALLOWED_HOST}" '{allowed_domains:[$h], denied_domains:[], first_use_approval:"always_deny",
    allowed_methods:[], min_confinement_class:"CC1", auto_stop_after_sec:0}' >"${TMP}/policy.json"
  org_up candidate "${CAND_WARDYND}" "${CAND_PROXY}"
  local state="${TMP}/state"
  register outage "${TMP}/wardyn-runnerd" "${TMP}/wardyn" "${state}"
  serve "${TMP}/wardyn-runnerd" "${state}" first || die "the runner never came online"

  # start_run NAME TASK: an exec run on the runner, RUNNING, with its agent container; prints its id.
  start_run() {
    local rid s=""
    WARDYN_URL="${ORG_URL}" WARDYN_TOKEN="${MEMBER_TOKEN}" "${TMP}/wardyn" run --agent claude-code --task-mode exec \
      --placement local --policy-file "${TMP}/policy.json" --json --task "$2" >"${TMP}/run-$1.json" 2>"${TMP}/run-$1.err"
    rid="$(jq -r '.id // empty' "${TMP}/run-$1.json" 2>/dev/null)"
    [[ -n "${rid}" ]] || die "$1: no run id: $(tail -3 "${TMP}/run-$1.err")"
    for _ in $(seq 1 60); do
      api "${MEMBER_TOKEN}" GET "/api/v1/runs/${rid}" >/dev/null
      s="$(jq -r '.state // empty' "${TMP}/resp.json")"
      [[ "${s}" == RUNNING && "$(docker inspect -f '{{.State.Running}}' "wardyn-agent-${rid}" 2>/dev/null)" == true ]] && break
      sleep 2
    done
    [[ "${s}" == RUNNING ]] || die "$1: run ${rid} never reached RUNNING with its container (last ${s})"
    echo "${rid}"
  }
  local run_a run_c state_file before after t6_from i stopped="" renewed=""
  run_a="$(start_run a "${FETCH_SCRIPT}")" || exit 1
  run_c="$(start_run c 'sleep 3600')" || exit 1
  pass "runs ${run_a} (fetching ${ALLOWED_HOST} every second) and ${run_c} RUNNING on runner ${RID}"
  state_file="$(find "${state}" -maxdepth 2 -name state.json -print -quit)"
  [[ -n "${state_file}" ]] || die "no state.json under ${state}"
  before="$(jq -r --arg id "${run_a}" '.runs[$id].lease.token_renewed_at // empty' "${state_file}")"
  [[ -n "${before}" ]] || die "run ${run_a} has no lease in ${state_file}"

  step "T6: the organisation goes down, then wardyn-runnerd restarts"
  t6_from="$(date +%s)"
  compose stop wardynd >/dev/null 2>&1 || die "could not stop wardynd"
  for _ in $(seq 1 15); do sleep 1; done
  stop_serve
  jq --arg id "${run_c}" --arg t "$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ)" '.runs[$id].lapse_by = $t' \
    "${state_file}" >"${state_file}.walk" && mv "${state_file}.walk" "${state_file}" || die "could not age run ${run_c}'s lapse time"
  "${TMP}/wardyn-runnerd" serve --org "${ORG_URL}" --state-dir "${state}" --proxy-image "${ORG_PROXY}" \
    --internal-network "${RUNNER_NET}" >"${EVIDENCE_DIR}/serve-restart.log" 2>&1 &
  SERVE_PID=$!
  for i in $(seq 1 15); do
    [[ "$(docker inspect -f '{{.State.Running}}' "wardyn-proxy-${run_c}" 2>/dev/null)" == true ]] || { stopped="${i}"; break; }
    sleep 1
  done
  [[ -n "${stopped}" ]] && pass "T6: run ${run_c}'s proxy, past its lapse time, stopped within ${stopped} s of the restart" \
    || fail "T6: run ${run_c}'s proxy, past its lapse time, still runs 15 s after the restart"
  [[ "$(docker inspect -f '{{.State.Running}}' "wardyn-proxy-${run_a}" 2>/dev/null)" == true ]] \
    && pass "T6: run ${run_a}'s proxy kept running through the restart" || fail "T6: run ${run_a}'s proxy is not running after the restart"
  for _ in $(seq 1 20); do sleep 1; done
  compose start wardynd >/dev/null 2>&1 || die "could not start wardynd"
  wait_healthy "${ORG_URL}" 90 2 || die "the organisation did not come back"
  for i in $(seq 1 30); do
    after="$(jq -r --arg id "${run_a}" '.runs[$id].lease.token_renewed_at // empty' "${state_file}")"
    [[ -n "${after}" && "${after}" != "${before}" ]] && { renewed=$((i * 2)); break; }
    sleep 2
  done
  [[ -n "${renewed}" ]] && pass "T6: run ${run_a}'s lease renewed within ${renewed} s of the organisation coming back" \
    || fail "T6: run ${run_a}'s lease was not renewed within 60 s of the organisation coming back (still ${before})"
  docker exec "wardyn-agent-${run_a}" cat /tmp/outage-fetch.log >"${EVIDENCE_DIR}/fetch-a.log" 2>&1
  local n f gap
  read -r n f gap < <(fetch_window "${EVIDENCE_DIR}/fetch-a.log" "${t6_from}" "$(date +%s)")
  [[ "${n}" -ge 30 && "${gap}" -le 7 ]] && pass "T6: ${n} fetches, ${f} failed, longest gap between successes ${gap} s" \
    || fail "T6: ${n} fetches, ${f} failed, longest gap between successes ${gap} s (want at least 30 fetches and at most 7 s)"
  api "${MEMBER_TOKEN}" GET "/api/v1/runs/${run_a}" >/dev/null
  [[ "$(jq -r '.state' "${TMP}/resp.json")" == RUNNING ]] && pass "T6: run ${run_a} still RUNNING" || fail "T6: run ${run_a} is $(jq -r '.state' "${TMP}/resp.json")"
}

"${LEG}_leg"
[[ "${FAILED}" -eq 0 ]] || die "cp-outage-walk (${LEG}): at least one check FAILED (evidence in ${EVIDENCE_DIR})"
echo "cp-outage-walk (${LEG}): PASS"
