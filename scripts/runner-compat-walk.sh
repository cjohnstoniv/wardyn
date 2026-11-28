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
#      to COMPLETED;
#   3b. only with WARDYN_COMPAT_REFUSAL=1 (DR-0436, #3648): on that runner, an
#      exit the candidate organisation refuses for good (its run's execution
#      generation is moved while it runs) is acknowledged once: one
#      runner.event.refuse row, the runner online for 30 s after it, no
#      runner.disconnect row, and a `refusal-acked-once: PASS` line.
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
# GUARD: self-skips (77) unless WARDYN_TEST_DOCKER=1. WARDYN_COMPAT_REFUSAL=1 adds step 3b. WARDYN_COMPAT_RELEASED
# pins the released tag instead of resolving it; WARDYN_COMPAT_DOCKER names the
# engine (default DOCKER_HOST, else /var/run/docker.sock). Removes everything it
# creates.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}" || exit 1
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/lib/walk-api.sh
source "${ROOT}/scripts/lib/walk-api.sh"

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
POOL=Compat
# shellcheck source=scripts/lib/compose-org.sh
source "${ROOT}/scripts/lib/compose-org.sh"

cleanup() {
  stop_serve
  org_down last
  docker network rm "${RUNNER_NET}" >/dev/null 2>&1
  [[ "${WARDYN_COMPAT_KEEP_IMAGES:-}" == "1" ]] || docker rmi "${CAND_WARDYND}" "${CAND_PROXY}" >/dev/null 2>&1
  rm -rf "${TMP}"
  echo "cleaned up ${PROJECT}"
}
trap cleanup EXIT

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

# refusal_acked_once LABEL CLI RID: step 3b. One exec run starts on RID; once it is RUNNING and bound, its
# execution generation is moved, so the sandbox's exit names a generation the organisation never issued.
refusal_acked_once() {
  local cli_pid run_id="" rows=0 down=0 disconnects
  WARDYN_URL="${ORG_URL}" WARDYN_TOKEN="${MEMBER_TOKEN}" "$2" run --agent claude-code --task-mode exec --placement local \
    --policy-file "${TMP}/policy.json" --wait --timeout 8m --json --task "sleep 20; echo compat-walk-refusal" \
    >"${TMP}/refusal-run.json" 2>"${TMP}/refusal-run.err" &
  cli_pid=$!
  for _ in $(seq 1 60); do
    run_id="$(psql1 "SELECT r.id FROM agent_runs r JOIN runner_executions e ON e.run_id = r.id
      WHERE r.runner_id = '$3' AND r.state = 'RUNNING' AND e.exec_id <> '' LIMIT 1")"
    [[ -n "${run_id}" ]] && break
    sleep 2
  done
  if [[ -z "${run_id}" ]] || ! psql1 "UPDATE runner_executions SET generation = gen_random_uuid() WHERE run_id = '${run_id}'" >/dev/null; then
    kill "${cli_pid}" 2>/dev/null
    fail "$1: no RUNNING, bound exec run on runner $3 whose generation could be moved"
    return 1
  fi
  for _ in $(seq 1 45); do
    rows="$(psql1 "SELECT count(*) FROM audit_events WHERE action = 'runner.event.refuse' AND actor = 'runner:$3'")"
    [[ "${rows}" -ge 1 ]] && break
    sleep 2
  done
  for _ in $(seq 1 15); do
    api "${MEMBER_TOKEN}" GET "/api/v1/me/runners/$3" >/dev/null
    jq -e '.online == true' "${TMP}/resp.json" >/dev/null || down=$((down + 1))
    sleep 2
  done
  rows="$(psql1 "SELECT count(*) FROM audit_events WHERE action = 'runner.event.refuse' AND actor = 'runner:$3'")"
  disconnects="$(psql1 "SELECT count(*) FROM audit_events WHERE action = 'runner.disconnect' AND target = '$3'")"
  psql1 "SELECT outcome, data FROM audit_events WHERE action = 'runner.event.refuse' AND actor = 'runner:$3'" >"${EVIDENCE_DIR}/refusal-$1.txt"
  kill "${cli_pid}" 2>/dev/null
  wait "${cli_pid}" 2>/dev/null
  if [[ "${rows}" == 1 && "${down}" -eq 0 && "${disconnects}" == 0 ]] && kill -0 "${SERVE_PID}" 2>/dev/null; then
    pass "$1: run ${run_id}'s refused exit was acknowledged once and the runner stayed online"
    echo "refusal-acked-once: PASS"
  else
    fail "$1: ${rows} runner.event.refuse rows (want 1), offline in ${down} of 15 reads over 30 s, ${disconnects} runner.disconnect rows (want 0)"
    return 1
  fi
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
  # The read is retried: once (nightly run 39800484102) it came back with no
  # matching tag and no curl error, and a later read of the same API found v0.9.8.
  # The cause is unknown, so a read with no matching tag says what it got.
  resolve_release() {
    local hdr="${TMP}/releases.headers" body="${TMP}/releases.json"
    curl -fsSL -D "${hdr}" -o "${body}" "${auth[@]}" "https://api.github.com/repos/${REPO}/releases?per_page=100" || return 1
    REL_TAG="$(jq -r '.[] | select(.draft == false) | .tag_name' "${body}" | grep -E "^v${SERIES//./\\.}\.[0-9]+$" | sort -V | tail -1)"
    [[ "${REL_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] && return 0
    warn "the releases read held no ${SERIES}.Z tag: $(grep -E '^HTTP/' "${hdr}" | tail -1 | sed 's/[[:space:]]*$//')," \
      "$(jq length "${body}" 2>&1) items, first tags $(jq -r '[.[:3][].tag_name] | join(",")' "${body}" 2>&1)," \
      "request id $(grep -i '^x-github-request-id:' "${hdr}" | tail -1 | cut -d' ' -f2 | tr -d '\r')"
    return 1
  }
  retry resolve_release
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
  retry curl -fsSL -o "${TMP}/rel/${f}" "https://github.com/${REPO}/releases/download/${REL_TAG}/${f}" || die "downloading ${f} from ${REL_TAG} failed"
done
for f in "wardyn-linux-${arch}" "wardyn-runnerd-linux-${arch}"; do
  grep -q "[ /]${f}\$" "${TMP}/rel/SHA256SUMS" || die "${f} is not listed in ${REL_TAG}'s SHA256SUMS"
done
(cd "${TMP}/rel" && sha256sum -c --ignore-missing --quiet SHA256SUMS) || die "a released binary does not match SHA256SUMS"
chmod +x "${TMP}/rel/wardyn-linux-${arch}" "${TMP}/rel/wardyn-runnerd-linux-${arch}"
REL_CLI="${TMP}/rel/wardyn-linux-${arch}" REL_RUNNERD="${TMP}/rel/wardyn-runnerd-linux-${arch}"

log "pulling the released images"
for img in "${REL_WARDYND}" "${REL_PROXY}" "${AGENT_IMAGE}"; do
  retry docker pull -q "${img}" >/dev/null || die "docker pull ${img} failed"
done

log "building the candidate CLI, wardyn-runnerd and images from this tree"
go build -o "${TMP}/wardyn" ./cmd/wardyn || die "go build wardyn failed"
CGO_ENABLED=0 go build -tags docker -o "${TMP}/wardyn-runnerd" ./cmd/wardyn-runnerd || die "go build wardyn-runnerd failed"
CAND_CLI="${TMP}/wardyn" CAND_RUNNERD="${TMP}/wardyn-runnerd"
docker build -q -f deploy/compose/Dockerfile.wardynd -t "${CAND_WARDYND}" . >"${EVIDENCE_DIR}/build-wardynd.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/build-wardynd.log"; die "wardynd image build failed"; }
docker build -q -f deploy/compose/Dockerfile.proxy -t "${CAND_PROXY}" . >"${EVIDENCE_DIR}/build-proxy.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/build-proxy.log"; die "proxy image build failed"; }

org_tls
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
if serve "${REL_RUNNERD}" "${TMP}/state-rel-on-cand" rel-on-cand; then
  smoke rel-on-cand "${CAND_CLI}" "${RID}"
  if [[ "${WARDYN_COMPAT_REFUSAL:-}" == "1" ]]; then
    log "STEP 3b: an exit the candidate organisation refuses for good is acknowledged once (DR-0436)"
    refusal_acked_once rel-on-cand "${CAND_CLI}" "${RID}"
  fi
  stop_serve
fi
org_down candidate

[[ "${FAILED}" -eq 0 ]] || die "runner-compat-walk: at least one step FAILED against ${REL_TAG} (evidence in ${EVIDENCE_DIR})"
echo "runner-compat-walk: PASS against ${REL_TAG} — evidence in ${EVIDENCE_DIR}"
