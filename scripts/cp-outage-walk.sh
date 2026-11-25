#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Control-plane outage walk (#4080): live runs ride out an outage of the database or the servers.
#
#   cp-outage-walk.sh kind     T1 and T2 on the kind quickstart cluster's server Kubernetes runs, with an
#                              owner SSH session open (WARDYN_TEST_K8S=1, WARDYN_ACK_SHARED_QUICKSTART=1).
#   cp-outage-walk.sh compose  T8, T6, T4 (10 min) and T5 (10 min) on a compose organisation on this engine: its
#                              server Docker runs and one Docker runner (WARDYN_TEST_DOCKER=1, passwordless sudo
#                              for iptables; scripts/lib/compose-org.sh). About 70 minutes.
#   cp-outage-walk.sh weekly   T3 with T12, T4 (70 min) and T5 (70 min) on the same topology. About 4 hours.
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
# T8, one server run's proxy removed while the rest are healthy: that headless run is torn down FAILED with a
#   run.lost row (reason outage, kept false) by 65 minutes plus a sweep tick after the kill, and no
#   run.lapse_sweep.hold row is written in the whole leg.
# T4, wardynd stopped 10 (compose) or 70 (weekly) minutes: no runner or server sandbox is deleted, the runner is
#   online within 60 s of wardynd answering, server and runner fetches carry on (10 min: no failed server fetch, no
#   gap over 7 s on the runner), and after 70 minutes the server's unattended run is held: kept with lost_reason
#   outage, its agent running, and a run.lapse_sweep.hold row written.
# T5, the runner's link to the organisation cut 10 or 70 minutes: no runner sandbox is deleted; a run killed during
#   the 10-minute cut has its sandbox gone within 60 s of reconnect; after 70 minutes, a run whose lease lapsed during
#   the cut gets no proxy and no successful fetch back in the 120 s after reconnect; the runner is online within 60 s
#   of reconnect.
# T3 and T12, Postgres stopped 70 minutes with an unattended task on the server's Docker: its sandbox kept, its agent
#   running, a file it wrote before the stop unchanged, a run.lapse_sweep.hold row written; then revived, the task
#   finishes and its run.complete row says COMPLETED.
#
# Both Postgres faults on kind freeze every postgres process (SIGSTOP, then SIGCONT) from the kind node, which sees
# them in an ancestor PID namespace: inside the container the postmaster is PID 1 and ignores SIGSTOP from its own
# namespace. The quickstart's Postgres has no volume, so it cannot be stopped without losing the database. Each
# freeze is proved by a fresh connection that must not complete within 5 s (the probe is first run unfrozen and
# must connect). T1 holds each freeze 20 s from the STOP, probe included, and freezes up to three times, 40 s
# apart, until a session check lands inside a freeze: a check runs every 30 s and gives up after 5 s, so one 20 s
# freeze can fall between two checks.
#
# NOT COVERED HERE, so #4080 is not closed by this walk alone: T1's retryable mint (no brokered credential on a
# hosted runner); the terminal, desktop and portal app sessions (a browser leg); T1 and T2 on server Docker; T6 and
# T5 on a Kubernetes runner (READY within 60 s, the owner's copy-out); T3 on server Kubernetes; T4's sessions (they
# are served by wardynd itself); the laptop and VM legs.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}" || exit 1
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/lib/walk-api.sh
source "${ROOT}/scripts/lib/walk-api.sh"

LEG="${1:-}"
case "${LEG}" in kind | compose | weekly) ;; *) die "usage: $0 kind|compose|weekly" ;; esac
FAILED=0
pass() { printf '  [pass] %s\n' "$*"; }
fail() { printf '  [FAIL] %s\n' "$*"; FAILED=1; }
step() { printf '\n==> %s\n' "$*"; }
ALLOWED_HOST="proxy.golang.org"
EVIDENCE_DIR="${WARDYN_OUTAGE_EVIDENCE:-${ROOT}/local/evidence/cp-outage-walk-${LEG}}"
mkdir -p "${EVIDENCE_DIR}"

# The sandbox's traffic: one allowlisted fetch a second, logged as "<epoch> <CONNECT answer>", until a stop file
# appears or four hours pass.
FETCH_SCRIPT="$(cat <<'SH'
i=0
while [ "$i" -lt 14400 ] && [ ! -e /tmp/outage-fetch.stop ]; do
  printf '%s %s\n' "$(date +%s)" "$(curl -sS -o /dev/null -m 5 -w '%{http_connect}' https://proxy.golang.org/ 2>/dev/null)" >>/tmp/outage-fetch.log
  i=$((i + 1))
  sleep 1
done
SH
)"

# fetch_window LOG FROM TO: "<fetches> <failed> <longest gap without a success, s>" for the lines stamped FROM..TO.
# The gap counts the stretch from FROM to the first success and from the last success to TO, so failures at the
# window's start or end show; a window with no success at all is one gap, TO - FROM.
fetch_window() {
  awk -v from="$2" -v to="$3" '$1 >= from && $1 <= to {
      n++
      if ($2 == "200") { if ($1 - (ok ? prev : from) > gap) gap = $1 - (ok ? prev : from); prev = $1; ok = 1 } else f++
    } END { if (!ok) gap = to - from; else if (to - prev > gap) gap = to - prev; printf "%d %d %d\n", n, f, gap }' "$1"
}

# ── kind leg: T1 and T2 ─────────────────────────────────────────────────────
kind_leg() {
  [[ "${WARDYN_TEST_K8S:-}" == "1" ]] || skip_lane "cp-outage-walk (kind): set WARDYN_TEST_K8S=1 to run it (skipping)."
  [[ "${WARDYN_ACK_SHARED_QUICKSTART:-}" == "1" ]] || skip_lane "cp-outage-walk (kind): set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm the wardyn-quickstart cluster may have its database frozen (skipping)."
  for bin in kubectl docker curl jq ssh ssh-keygen; do
    command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
  done
  TMP="$(mktemp -d /tmp/wardyn-cp-outage.XXXXXX)"
  CONTEXT="kind-${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
  NAMESPACE="wardyn"
  RUNS_NAMESPACE="wardyn-runs"
  RELEASE="wardyn"
  KIND_NODE="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}-control-plane"
  BASE="http://127.0.0.1:${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
  k() { kubectl --context "${CONTEXT}" "$@"; }
  ADMIN_TOKEN="$(k -n "${NAMESPACE}" get secret wardyn-auth -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null)"
  [[ -n "${ADMIN_TOKEN}" ]] || die "could not read the admin token from Secret wardyn-auth"
  api() { walk_api "${BASE}" "${ADMIN_TOKEN}" "${TMP}/resp.json" 10 "$@"; }
  psql1() { k -n "${NAMESPACE}" exec deployment/postgres -- psql -U wardyn -d wardyn -tAc "$1" 2>&1; }
  # pg_signal STOP|CONT: every postgres process on the kind node (-x: the exact process name, so nothing else).
  pg_signal() { docker exec "${KIND_NODE}" pkill "-$1" -x postgres; }
  # pg_probe: a fresh connection to Postgres from inside its pod, given 5 s. rc 0 = connected, 124 = timed out.
  pg_probe() {
    timeout 5 kubectl --context "${CONTEXT}" -n "${NAMESPACE}" exec deployment/postgres -- \
      psql -U wardyn -d wardyn -h 127.0.0.1 -tAc 'select 1' >/dev/null 2>&1
  }
  # pg_freeze: prove the probe works, freeze Postgres, then prove it: a fresh connection must not complete within
  # 5 s. FREEZE_T0 is the clock second of the STOP, so a caller can hold the freeze for a measured time.
  pg_freeze() {
    local rc
    pg_probe; rc=$?
    [[ "${rc}" -eq 0 ]] || die "freeze check cannot reach Postgres before the freeze (rc=${rc})"
    pg_signal STOP || die "could not freeze Postgres on ${KIND_NODE} (no postgres process, or no pkill there)"
    FREEZE_T0="$(date +%s)"
    pg_probe; rc=$?
    case "${rc}" in
      124) ;;
      0) die "the Postgres freeze did not take effect" ;;
      *) die "freeze check inconclusive (rc=${rc})" ;;
    esac
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
    pg_freeze
    while [[ $(($(date +%s) - FREEZE_T0)) -lt 20 ]]; do
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
  pg_freeze
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

# ── compose legs: an organisation on this engine, its server Docker runs and one Docker runner ───────────────
STATE="" STATE_FILE="" CUT=""

# aapi TOKEN METHOD PATH [BODY]: a call to wardynd through its loopback port, which a runner cut leaves alone.
aapi() { walk_api "${ADMIN_URL}" "$1" "${TMP}/resp.json" 15 "${@:2}"; }
running() { [[ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" == true ]]; }
gone() { ! docker inspect "$1" >/dev/null 2>&1; }
run_field() { aapi "${ADMIN_TOKEN}" GET "/api/v1/runs/$1" >/dev/null; jq -r "$2 // empty" "${TMP}/resp.json"; }
audit_n() { psql1 "SELECT count(*) FROM audit_events WHERE $1"; }
runner_online() {
  aapi "${MEMBER_TOKEN}" GET "/api/v1/me/runners/${RID}" >/dev/null && jq -e '.online == true' "${TMP}/resp.json" >/dev/null
}
# until_true SECONDS CMD...: run CMD once a second until it succeeds and print how many seconds that took; fail
# after SECONDS.
until_true() {
  local lim="$1" t0
  shift
  t0="$(date +%s)"
  while [[ $(($(date +%s) - t0)) -le "${lim}" ]]; do
    if "$@"; then echo "$(($(date +%s) - t0))"; return 0; fi
    sleep 1
  done
  return 1
}
# fetch_log RUN: copy the run's fetch log into the evidence and print its path.
fetch_log() {
  docker exec "wardyn-agent-$1" cat /tmp/outage-fetch.log >"${EVIDENCE_DIR}/fetch-$1.log" 2>&1
  echo "${EVIDENCE_DIR}/fetch-$1.log"
}
# wait_out SECONDS WHAT RUN...: sleep through an outage, checking every 30 s that each RUN's agent container is
# still running; one FAIL per run that stops.
wait_out() {
  local lim="$1" what="$2" t0 rid lost=""
  shift 2
  t0="$(date +%s)"
  while [[ $(($(date +%s) - t0)) -lt "${lim}" ]]; do
    for rid in "$@"; do
      if [[ " ${lost} " != *" ${rid} "* ]] && ! running "wardyn-agent-${rid}"; then
        lost="${lost} ${rid}"
        fail "${what}: run ${rid}'s agent container stopped $(($(date +%s) - t0)) s into the outage"
      fi
    done
    sleep 30
  done
  [[ -n "${lost}" ]] || pass "${what}: every agent container ran through the outage ($*)"
}

# runner_link down|up: cut, or restore, every connection to the organisation's runner address ORG_HOST:ORG_PORT,
# from this host's processes (wardyn-runnerd) and from its containers, by original destination, so Docker's DNAT
# and its userland proxy are both matched. The walk's own calls go to ADMIN_URL, which the cut does not match.
runner_link() {
  local chain rc=0
  if [[ "$1" == down ]]; then
    CUT=1 # before the first insert, so a failure part-way is still undone by the cleanup
    for chain in OUTPUT DOCKER-USER; do
      sudo -n iptables -I "${chain}" -p tcp -m conntrack --ctorigdst "${ORG_HOST}" --ctorigdstport "${ORG_PORT}" \
        -j REJECT --reject-with tcp-reset || return 1
    done
    return 0
  fi
  # Best effort on both chains: a delete that fails on one never stops the other.
  for chain in OUTPUT DOCKER-USER; do
    sudo -n iptables -D "${chain}" -p tcp -m conntrack --ctorigdst "${ORG_HOST}" --ctorigdstport "${ORG_PORT}" \
      -j REJECT --reject-with tcp-reset || rc=1
  done
  [[ "${rc}" -ne 0 ]] || CUT=""
  return "${rc}"
}

cleanup_compose() {
  [[ -z "${CUT}" ]] || runner_link up
  stop_serve
  org_down last
  docker network rm "${RUNNER_NET}" >/dev/null 2>&1
  docker rmi "${CAND_WARDYND}" "${CAND_PROXY}" >/dev/null 2>&1
  rm -rf "${TMP}"
}

compose_setup() {
  [[ "${WARDYN_TEST_DOCKER:-}" == "1" ]] || skip_lane "cp-outage-walk (${LEG}): set WARDYN_TEST_DOCKER=1 to run it (skipping)."
  for bin in docker curl jq go openssl ss iptables; do
    command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
  done
  sudo -n iptables -S OUTPUT >/dev/null 2>&1 || die "this walk cuts a runner off with iptables and needs passwordless sudo"
  TMP="$(mktemp -d /tmp/wardyn-cp-outage.XXXXXX)"
  export DOCKER_HOST="${DOCKER_HOST:-unix:///var/run/docker.sock}"
  PROJECT="wardyn-outage"
  ORG_HOST="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
  ORG_PORT="${WARDYN_OUTAGE_ORG_PORT:-18743}"
  ORG_URL="https://${ORG_HOST}:${ORG_PORT}"
  ORG_ADMIN_PORT="${WARDYN_OUTAGE_ADMIN_PORT:-18744}"
  ADMIN_URL="https://localhost:${ORG_ADMIN_PORT}"
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
  wait_healthy "${ADMIN_URL}" 30 2 || die "wardynd does not answer on its loopback port ${ADMIN_URL}"
  STATE="${TMP}/state"
  register outage "${TMP}/wardyn-runnerd" "${TMP}/wardyn" "${STATE}"
  serve "${TMP}/wardyn-runnerd" "${STATE}" first || die "the runner never came online"
  STATE_FILE="$(find "${STATE}" -maxdepth 2 -name state.json -print -quit)"
  [[ -n "${STATE_FILE}" ]] || die "no state.json under ${STATE}"
}

# start_run NAME TOKEN WHERE TASK: an exec run, RUNNING with its agent container. WHERE is "runner" (placement
# local: the member's default pool, which is this walk's runner) or "server" (the admin's run, on the server's own
# Docker). Prints its id.
start_run() {
  local rid s="" flags=()
  [[ "$3" == runner ]] && flags=(--placement local)
  WARDYN_URL="${ADMIN_URL}" WARDYN_TOKEN="$2" "${TMP}/wardyn" run --agent claude-code --task-mode exec "${flags[@]}" \
    --policy-file "${TMP}/policy.json" --json --task "$4" >"${TMP}/run-$1.json" 2>"${TMP}/run-$1.err"
  rid="$(jq -r '.id // empty' "${TMP}/run-$1.json" 2>/dev/null)"
  [[ -n "${rid}" ]] || die "$1: no run id: $(tail -3 "${TMP}/run-$1.err")"
  for _ in $(seq 1 60); do
    s="$(run_field "${rid}" .state)"
    [[ "${s}" == RUNNING ]] && running "wardyn-agent-${rid}" && break
    sleep 2
  done
  [[ "${s}" == RUNNING ]] || die "$1: run ${rid} never reached RUNNING with its container (last ${s})"
  if [[ "$3" == runner ]]; then
    [[ "$(run_field "${rid}" .runner_id)" == "${RID}" ]] || die "$1: run ${rid} is not on runner ${RID}"
  else
    [[ -z "$(run_field "${rid}" .runner_id)" ]] || die "$1: run ${rid} landed on a runner, not the server's Docker"
  fi
  echo "${rid}"
}

# t6 RUN_A RUN_C: wardyn-runnerd restarted while the organisation is down (RUN_A fetching, RUN_C to be aged).
t6() {
  local run_a="$1" run_c="$2" before after t6_from i stopped="" renewed="" n f gap
  before="$(jq -r --arg id "${run_a}" '.runs[$id].lease.token_renewed_at // empty' "${STATE_FILE}")"
  [[ -n "${before}" ]] || die "run ${run_a} has no lease in ${STATE_FILE}"
  step "T6: the organisation goes down, then wardyn-runnerd restarts"
  t6_from="$(date +%s)"
  compose stop wardynd >/dev/null 2>&1 || die "could not stop wardynd"
  for _ in $(seq 1 15); do sleep 1; done
  stop_serve
  jq --arg id "${run_c}" --arg t "$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ)" '.runs[$id].lapse_by = $t' \
    "${STATE_FILE}" >"${STATE_FILE}.walk" && mv "${STATE_FILE}.walk" "${STATE_FILE}" || die "could not age run ${run_c}'s lapse time"
  "${TMP}/wardyn-runnerd" serve --org "${ORG_URL}" --state-dir "${STATE}" --proxy-image "${ORG_PROXY}" \
    --internal-network "${RUNNER_NET}" >"${EVIDENCE_DIR}/serve-restart.log" 2>&1 &
  SERVE_PID=$!
  for i in $(seq 1 15); do
    running "wardyn-proxy-${run_c}" || { stopped="${i}"; break; }
    sleep 1
  done
  [[ -n "${stopped}" ]] && pass "T6: run ${run_c}'s proxy, past its lapse time, stopped within ${stopped} s of the restart" \
    || fail "T6: run ${run_c}'s proxy, past its lapse time, still runs 15 s after the restart"
  running "wardyn-proxy-${run_a}" && pass "T6: run ${run_a}'s proxy kept running through the restart" \
    || fail "T6: run ${run_a}'s proxy is not running after the restart"
  for _ in $(seq 1 20); do sleep 1; done
  compose start wardynd >/dev/null 2>&1 || die "could not start wardynd"
  wait_healthy "${ADMIN_URL}" 90 2 || die "the organisation did not come back"
  for i in $(seq 1 30); do
    after="$(jq -r --arg id "${run_a}" '.runs[$id].lease.token_renewed_at // empty' "${STATE_FILE}")"
    [[ -n "${after}" && "${after}" != "${before}" ]] && { renewed=$((i * 2)); break; }
    sleep 2
  done
  [[ -n "${renewed}" ]] && pass "T6: run ${run_a}'s lease renewed within ${renewed} s of the organisation coming back" \
    || fail "T6: run ${run_a}'s lease was not renewed within 60 s of the organisation coming back (still ${before})"
  read -r n f gap < <(fetch_window "$(fetch_log "${run_a}")" "${t6_from}" "$(date +%s)")
  [[ "${n}" -ge 30 && "${gap}" -le 7 ]] && pass "T6: ${n} fetches, ${f} failed, longest gap without a success ${gap} s" \
    || fail "T6: ${n} fetches, ${f} failed, longest gap without a success ${gap} s (want at least 30 fetches and at most 7 s)"
  [[ "$(run_field "${run_a}" .state)" == RUNNING ]] && pass "T6: run ${run_a} still RUNNING" || fail "T6: run ${run_a} is $(run_field "${run_a}" .state)"
}

# t4 MINUTES RUNNER_RUN SERVER_RUN [SERVER_HELD]: every wardynd replica stopped for MINUTES. With SERVER_HELD (an
# unattended server run, 70 minutes), it must come back held.
t4() {
  local mins="$1" rrun="$2" srun="$3" held="${4:-}" t_from t_back took n f gap
  step "T4: wardynd stopped ${mins} minutes"
  t_from="$(date +%s)"
  compose stop wardynd >/dev/null 2>&1 || die "could not stop wardynd"
  wait_out $((mins * 60)) "T4 (${mins} min)" "${rrun}" "${srun}" ${held}
  compose start wardynd >/dev/null 2>&1 || die "could not start wardynd"
  wait_healthy "${ADMIN_URL}" 90 2 || die "wardynd did not come back"
  t_back="$(date +%s)"
  took="$(until_true 60 runner_online)" && pass "T4 (${mins} min): the runner was online ${took} s after wardynd answered" \
    || fail "T4 (${mins} min): the runner was not online within 60 s of wardynd answering"
  if [[ "${mins}" -le 10 ]]; then
    read -r n f gap < <(fetch_window "$(fetch_log "${srun}")" "${t_from}" "${t_back}")
    [[ "${n}" -ge 60 && "${f}" -eq 0 ]] && pass "T4: ${n} server-run fetches through the outage, none failed" \
      || fail "T4: ${f} of ${n} server-run fetches failed (want 0 of at least 60)"
    read -r n f gap < <(fetch_window "$(fetch_log "${rrun}")" "${t_from}" "${t_back}")
    [[ "${n}" -ge 60 && "${gap}" -le 7 ]] && pass "T4: ${n} runner-run fetches through the outage, longest gap ${gap} s" \
      || fail "T4: ${n} runner-run fetches, longest gap without a success ${gap} s (want at least 60 and at most 7 s)"
    local rid
    for rid in "${rrun}" "${srun}"; do
      [[ "$(run_field "${rid}" .state)" == RUNNING ]] && pass "T4: run ${rid} RUNNING" || fail "T4: run ${rid} is $(run_field "${rid}" .state)"
    done
  fi
  if [[ -n "${held}" ]]; then
    t_held "T4 (70 min)" "${held}" "${t_from}"
    running "wardyn-agent-${rrun}" && pass "T4 (70 min): runner run ${rrun}'s sandbox kept" \
      || fail "T4 (70 min): runner run ${rrun}'s agent container is gone or stopped"
  fi
}

# t_held WHAT RUN SINCE: within 5 minutes, RUN (an unattended server run) is kept with lost_reason outage and its
# agent running, and a run.lapse_sweep.hold row has been written since SINCE.
t_held() {
  local what="$1" rid="$2" since="$3" holds
  for _ in $(seq 1 60); do
    [[ "$(run_field "${rid}" .lost_reason)" == outage ]] && break
    sleep 5
  done
  [[ "$(run_field "${rid}" .lost_reason)" == outage && "$(run_field "${rid}" .state)" == RUNNING ]] \
    && pass "${what}: unattended server run ${rid} kept (RUNNING, lost_reason outage)" \
    || fail "${what}: unattended server run ${rid} is $(run_field "${rid}" .state) with lost_reason '$(run_field "${rid}" .lost_reason)', want RUNNING and outage"
  running "wardyn-agent-${rid}" && pass "${what}: its agent container is running" || fail "${what}: its agent container is gone or stopped"
  holds="$(audit_n "action = 'run.lapse_sweep.hold' AND time >= to_timestamp(${since}) AND (data->>'held')::int >= 1")"
  [[ "${holds}" =~ ^[1-9] ]] && pass "${what}: ${holds} run.lapse_sweep.hold row(s)" || fail "${what}: no run.lapse_sweep.hold row since the outage began"
}

# t5 MINUTES RUN [KILL_RUN]: the runner's link cut for MINUTES. KILL_RUN (10 minutes) is killed during the cut. After
# 70 minutes RUN's lease has lapsed, and it must not get its network back.
t5() {
  local mins="$1" rid="$2" krun="${3:-}" code t_up took ok
  step "T5: the runner's link cut ${mins} minutes"
  runner_link down || die "could not cut the runner's link (iptables)"
  if [[ -n "${krun}" ]]; then
    for _ in $(seq 1 30); do sleep 1; done
    code="$(aapi "${ADMIN_TOKEN}" POST "/api/v1/runs/${krun}/kill")"
    [[ "${code}" == 200 || "${code}" == 202 ]] || fail "T5: killing run ${krun} during the cut answered ${code}: $(cat "${TMP}/resp.json")"
  fi
  wait_out $((mins * 60 - 30)) "T5 (${mins} min)" "${rid}" ${krun}
  runner_link up || die "could not restore the runner's link (iptables)"
  t_up="$(date +%s)"
  took="$(until_true 60 runner_online)" && pass "T5 (${mins} min): the runner was online ${took} s after reconnect" \
    || fail "T5 (${mins} min): the runner was not online within 60 s of reconnect"
  if [[ -n "${krun}" ]]; then
    took="$(until_true 60 gone "wardyn-agent-${krun}")" && pass "T5: the kill sent during the cut landed ${took} s after reconnect" \
      || fail "T5: run ${krun}, killed during the cut, still has its agent container 60 s after reconnect"
  fi
  if [[ "${mins}" -ge 70 ]]; then
    ok=1
    for _ in $(seq 1 120); do
      running "wardyn-proxy-${rid}" && ok=""
      sleep 1
    done
    [[ -n "${ok}" ]] && pass "T5 (70 min): run ${rid}, lapsed during the cut, had no proxy in the 120 s after reconnect" \
      || fail "T5 (70 min): run ${rid}, lapsed during the cut, has a proxy again after reconnect"
    [[ "$(awk -v t="${t_up}" '$1 > t && $2 == "200"' "$(fetch_log "${rid}")" | wc -l)" -eq 0 ]] \
      && pass "T5 (70 min): run ${rid} made no successful fetch after reconnect" \
      || fail "T5 (70 min): run ${rid} made a successful fetch after reconnect"
  fi
  running "wardyn-agent-${rid}" && pass "T5 (${mins} min): runner run ${rid}'s sandbox kept" \
    || fail "T5 (${mins} min): runner run ${rid}'s agent container is gone or stopped"
}

# ── compose leg (nightly): T8, T6, T4 (10 min), T5 (10 min) ─────────────────────────────────────────────────
compose_leg() {
  compose_setup
  local leg_from run_a run_c run_k run_s run_p t8_from t8_due took state lost
  leg_from="$(date +%s)"
  run_p="$(start_run p "${ADMIN_TOKEN}" server 'sleep 14400')" || exit 1
  run_a="$(start_run a "${MEMBER_TOKEN}" runner "${FETCH_SCRIPT}")" || exit 1
  run_c="$(start_run c "${MEMBER_TOKEN}" runner 'sleep 14400')" || exit 1
  run_k="$(start_run k "${MEMBER_TOKEN}" runner 'sleep 14400')" || exit 1
  run_s="$(start_run s "${ADMIN_TOKEN}" server "${FETCH_SCRIPT}")" || exit 1
  pass "server runs ${run_p} and ${run_s}, runner runs ${run_a}, ${run_c} and ${run_k} RUNNING"

  step "T8: run ${run_p}'s proxy removed while every other proxy is healthy"
  t8_from="$(date +%s)"
  docker rm -f "wardyn-proxy-${run_p}" >/dev/null || die "could not remove run ${run_p}'s proxy"
  # The token lapses at most 65 minutes after the kill (its last renewal was before it), then one 60 s tick, plus
  # 2 minutes for the sweep's own work.
  t8_due=$((t8_from + 65 * 60 + 60 + 120))

  t6 "${run_a}" "${run_c}"
  t4 10 "${run_a}" "${run_s}"
  t5 10 "${run_a}" "${run_k}"

  step "T8: waiting for run ${run_p}'s lapse (until $(date -u -d "@${t8_due}" +%H:%M:%SZ))"
  state=""
  while [[ "$(date +%s)" -le "${t8_due}" ]]; do
    state="$(run_field "${run_p}" .state)"
    [[ "${state}" == FAILED ]] && break
    sleep 30
  done
  took=$(($(date +%s) - t8_from))
  [[ "${state}" == FAILED ]] && pass "T8: run ${run_p} FAILED ${took} s after its proxy was removed" \
    || fail "T8: run ${run_p} is ${state} $((took / 60)) minutes after its proxy was removed, want FAILED by 68"
  lost="$(audit_n "run_id = '${run_p}' AND action = 'run.lost' AND data->>'reason' = 'outage' AND data->>'kept' = 'false'")"
  [[ "${lost}" == 1 ]] && pass "T8: one run.lost row (outage, not kept)" || fail "T8: ${lost} run.lost (outage, not kept) rows for run ${run_p}, want 1"
  [[ "$(audit_n "action = 'run.lapse_sweep.hold' AND time >= to_timestamp(${leg_from})")" == 0 ]] \
    && pass "T8: no run.lapse_sweep.hold row in the whole leg" || fail "T8: a run.lapse_sweep.hold row was written: one dead proxy was taken for an outage"
  [[ "$(run_field "${run_s}" .state)" == RUNNING ]] && pass "T8: server run ${run_s} still RUNNING" || fail "T8: server run ${run_s} is $(run_field "${run_s}" .state)"
}

# ── weekly leg: T3 with T12, T4 (70 min), T5 (70 min) ───────────────────────────────────────────────────────
weekly_leg() {
  compose_setup
  local run_u run_w run_r run_r2 ends sum_before sum_after t_from code took
  # The unattended task: write a file, then wait for the walk's go file, then finish.
  local task='head -c 64 /dev/urandom | od -An -tx1 >/tmp/cp-outage-pre; while [ ! -e /tmp/cp-outage-go ]; do sleep 5; done; echo cp-outage-done'

  step "T3 and T12: Postgres stopped 70 minutes with an unattended task on the server's Docker"
  run_u="$(start_run u "${ADMIN_TOKEN}" server "${task}")" || exit 1
  ends="$(run_field "${run_u}" .ends_at)"
  [[ -z "${ends}" || "$(date -d "${ends}" +%s)" -gt $(($(date +%s) + 3 * 3600)) ]] \
    || die "run ${run_u} ends at ${ends}, inside this walk: its end, not the outage, would decide what happens to it"
  for _ in $(seq 1 10); do sleep 1; done
  sum_before="$(docker exec "wardyn-agent-${run_u}" sha256sum /tmp/cp-outage-pre | cut -d' ' -f1)"
  [[ -n "${sum_before}" ]] || die "run ${run_u} wrote no /tmp/cp-outage-pre"
  t_from="$(date +%s)"
  compose stop postgres >/dev/null 2>&1 || die "could not stop Postgres"
  wait_out 4200 "T3" "${run_u}"
  compose start postgres >/dev/null 2>&1 || die "could not start Postgres"
  wait_healthy "${ADMIN_URL}" 90 2 || die "wardynd did not answer after Postgres came back"
  t_held "T3" "${run_u}" "${t_from}"
  sum_after="$(docker exec "wardyn-agent-${run_u}" sha256sum /tmp/cp-outage-pre | cut -d' ' -f1)"
  [[ "${sum_after}" == "${sum_before}" ]] && pass "T3: the file written before the stop is unchanged" \
    || fail "T3: the file written before the stop changed or is gone (${sum_before} then '${sum_after}')"
  code="$(aapi "${ADMIN_TOKEN}" POST "/api/v1/runs/${run_u}/revive" '{}')"
  [[ "${code}" == 200 ]] || fail "T12: revive answered ${code}: $(cat "${TMP}/resp.json")"
  took="$(until_true 120 running "wardyn-proxy-${run_u}")" && pass "T12: run ${run_u} revived, a new proxy up in ${took} s" \
    || fail "T12: run ${run_u} has no running proxy 120 s after its revive"
  docker exec "wardyn-agent-${run_u}" touch /tmp/cp-outage-go || fail "T12: could not signal run ${run_u}'s task"
  for _ in $(seq 1 24); do
    [[ "$(run_field "${run_u}" .state)" == COMPLETED ]] && break
    sleep 5
  done
  [[ "$(run_field "${run_u}" .state)" == COMPLETED ]] && pass "T12: the task finished: run ${run_u} COMPLETED" \
    || fail "T12: run ${run_u} is $(run_field "${run_u}" .state) 2 minutes after its task was released"
  [[ "$(audit_n "run_id = '${run_u}' AND action = 'run.complete' AND data->>'state' = 'COMPLETED'")" == 1 ]] \
    && pass "T12: its run.complete row says COMPLETED" || fail "T12: no run.complete row saying COMPLETED for run ${run_u}"

  run_w="$(start_run w "${ADMIN_TOKEN}" server "${task}")" || exit 1
  run_r="$(start_run r "${MEMBER_TOKEN}" runner "${FETCH_SCRIPT}")" || exit 1
  t4 70 "${run_r}" "${run_w}" "${run_w}"
  aapi "${ADMIN_TOKEN}" POST "/api/v1/runs/${run_w}/kill" >/dev/null
  aapi "${ADMIN_TOKEN}" POST "/api/v1/runs/${run_r}/kill" >/dev/null

  run_r2="$(start_run r2 "${MEMBER_TOKEN}" runner "${FETCH_SCRIPT}")" || exit 1
  t5 70 "${run_r2}"
}

"${LEG}_leg"
[[ "${FAILED}" -eq 0 ]] || die "cp-outage-walk (${LEG}): at least one check FAILED (evidence in ${EVIDENCE_DIR})"
echo "cp-outage-walk (${LEG}): PASS"
