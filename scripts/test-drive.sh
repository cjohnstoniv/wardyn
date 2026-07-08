#!/usr/bin/env bash
# scripts/test-drive.sh — Wardyn governance test-drive.
#
# Walks an operator through every major governance feature against a RUNNING
# compose stack (wardynd + postgres + dex).  Assumes `make demo` has already
# been executed, or pass --up to bring the stack up first.
#
# What each section proves
# ────────────────────────
# (1) Clone + governed egress  A run against octocat/Hello-World is created;
#     the sandbox actually clones the repo through the proxy egress path; docker
#     exec confirms the clone dir exists (proves clone wiring + governed egress
#     for git over HTTPS).
#
# (2) Egress deny              docker exec curl to an unlisted domain via the
#     proxy yields 403; the audit API shows the egress.deny or egress.pending
#     event for that host.
#
# (3) Metadata block           docker exec curl 169.254.169.254 through the
#     proxy yields 403; the audit shows the builtin deny (L3 invariant).
#
# (4) First-use approval       Probe an unknown domain (first_use_approval=true
#     policy), poll the approval queue, approve via the API, confirm state
#     transitions and audit.
#
# (5) Brokered git             docker exec wardyn-git-helper get for github.com
#     -> raises a credential ApprovalRequest; approve it; show the documented
#     fail-closed mint error (no GitHub App configured in the demo); assert no
#     token in sandbox env (invariant 1).
#
# (6) Kill cascade             wardyn kill -> container gone + run token 401 +
#     run.kill audit event.
#
# (7) Recording                GET the asciicast artifact for the run -> 200.
#
# Usage
# ─────
#   scripts/test-drive.sh               # assumes stack is up
#   scripts/test-drive.sh --up          # bring stack up first
#   scripts/test-drive.sh --keep        # skip teardown of test runs
#   scripts/test-drive.sh --section 3   # run only section 3
#   scripts/test-drive.sh --up --keep --section 1
#
# Environment
# ───────────
#   WARDYN_URL             default http://localhost:8080
#   WARDYN_ADMIN_TOKEN     default demo-admin-token
#   ANTHROPIC_API_KEY      if set, also runs a real claude task scenario
#
# The script is idempotent and re-runnable.  Sections that require a live
# sandbox create their own run and clean it up (unless --keep).

set -uo pipefail

# ── config ───────────────────────────────────────────────────────────────────
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/deploy/compose/docker-compose.yaml"
COMPOSE=(docker compose -f "${COMPOSE_FILE}")

WARDYN_URL="${WARDYN_URL:-http://localhost:8080}"
WARDYN_ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN:-demo-admin-token}"
ADMIN_HDR="Authorization: Bearer ${WARDYN_ADMIN_TOKEN}"
AGENT="claude-code"
PUBLIC_REPO="octocat/Hello-World"

# flags
OPT_UP=0
OPT_KEEP=0
OPT_SECTION=""

# ── argument parsing ─────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --up)        OPT_UP=1      ; shift ;;
    --keep)      OPT_KEEP=1    ; shift ;;
    --section)   OPT_SECTION="$2"; shift 2 ;;
    --section=*) OPT_SECTION="${1#*=}"; shift ;;
    -h|--help)
      sed -n '2,60p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,2\}//'
      exit 0 ;;
    *) printf 'unknown flag: %s (try --help)\n' "$1" >&2; exit 1 ;;
  esac
done

# ── colour helpers ────────────────────────────────────────────────────────────
source "${REPO_ROOT}/scripts/lib/common.sh"
ok()   { printf '\033[1;32m  PASS\033[0m %s\n' "$*"; pass=$((pass+1)); }
bad()  { printf '\033[1;31m  FAIL\033[0m %s\n' "$*"; fail=$((fail+1)); }
note() { printf '\033[1;33m  NOTE\033[0m %s\n' "$*"; }

pass=0; fail=0

# Run IDs created in section 1 are optionally reused by sections 2 and 3.
# Initialize here so ${S1_RUN_ID:-} is always defined even when section 1 is
# skipped via --section (bash set -u would reject an unset variable otherwise).
S1_RUN_ID=""

# ── run tracking (for teardown) ───────────────────────────────────────────────
# Accumulate all run IDs created by this script so teardown can remove them.
TD_RUN_IDS=()

teardown() {
  if [[ "${OPT_KEEP}" -eq 1 ]]; then
    note "teardown skipped (--keep)"
    return
  fi
  for rid in "${TD_RUN_IDS[@]+"${TD_RUN_IDS[@]}"}"; do
    docker rm -f "wardyn-agent-${rid}" "wardyn-proxy-${rid}" >/dev/null 2>&1 || true
    docker network rm "wardyn-int-${rid}" >/dev/null 2>&1 || true
  done
}
trap teardown EXIT

# ── helpers ───────────────────────────────────────────────────────────────────
# hc: host-side curl against the published port; no auth header unless given.
hc() { curl -sS "$@"; }

# api: call the wardyn admin API.
api() { hc -H "${ADMIN_HDR}" "${WARDYN_URL}$1" "${@:2}"; }

# create_run: create a run and return its ID.  Prints noise to stderr.
# Args: [--task "task text"]  (defaults to empty task so no claude is invoked)
create_run() {
  local task=""
  while [[ $# -gt 0 ]]; do
    case "$1" in --task) task="$2"; shift 2 ;; *) shift ;; esac
  done
  local body
  body="$(printf '{"agent":"%s","repo":"%s","task":"%s"}' \
    "${AGENT}" "${PUBLIC_REPO}" "${task}")"
  local resp
  resp="$(hc -s -X POST "${WARDYN_URL}/api/v1/runs" \
           -H "${ADMIN_HDR}" \
           -H 'Content-Type: application/json' \
           -d "${body}")"
  printf '%s' "${resp}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])' 2>/dev/null
}

# wait_state: poll until the run reaches the expected state (or timeout).
# Args: run_id expected_state max_tries sleep_secs
wait_state() {
  local rid="$1" want="$2" tries="${3:-20}" slp="${4:-2}"
  local got
  for _ in $(seq 1 "${tries}"); do
    got="$(api "/api/v1/runs/${rid}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["state"])' 2>/dev/null || echo "")"
    [[ "${got}" == "${want}" ]] && return 0
    sleep "${slp}"
  done
  return 1
}

# run_section: returns 0 if we should run this section number.
run_section() {
  [[ -z "${OPT_SECTION}" || "${OPT_SECTION}" == "$1" ]]
}

# ── preflight ─────────────────────────────────────────────────────────────────
command -v docker     >/dev/null 2>&1 || die "docker not found"
command -v python3    >/dev/null 2>&1 || die "python3 not found (used for JSON parsing)"
docker compose version >/dev/null 2>&1 || die "docker compose v2 required"

# ── optional stack bring-up ───────────────────────────────────────────────────
if [[ "${OPT_UP}" -eq 1 ]]; then
  log "--up: bringing compose stack up"
  "${COMPOSE[@]}" up -d postgres dex wardynd >/dev/null \
    || die "compose up failed; check: docker compose -f ${COMPOSE_FILE} logs"
  log "Waiting for wardynd to become healthy"
  tries=0
  until [[ "$(docker inspect -f '{{.State.Health.Status}}' wardyn-api 2>/dev/null || echo starting)" == "healthy" ]]; do
    tries=$((tries+1))
    [[ ${tries} -gt 60 ]] && { "${COMPOSE[@]}" logs --tail 40 wardynd; die "wardynd unhealthy"; }
    sleep 2
  done
  ok "stack is healthy"
fi

# Sanity-check that wardynd is reachable. curl -w always prints the code (000
# on connection failure), so capture it directly — no fallback echo (which
# would double-print to "000000") and silence curl's own error line.
HEALTH="$(curl -s -o /dev/null -w '%{http_code}' "${WARDYN_URL}/healthz" 2>/dev/null)"
HEALTH="${HEALTH:-000}"
[[ "${HEALTH}" == "200" ]] || die "wardynd not reachable at ${WARDYN_URL} (http_code=${HEALTH}); run 'make test-drive' (it brings the stack up) or 'make demo' first"
note "wardynd at ${WARDYN_URL} (healthz=200)"

echo
printf '\033[1;36m━━━  Wardyn governance test-drive  ━━━\033[0m\n'
echo

# ─────────────────────────────────────────────────────────────────────────────
# SECTION 1: Clone + governed egress
# Prove that the --repo value is actually cloned into the sandbox workspace
# through the proxy egress path (HTTPS git clone via wardyn-proxy CONNECT
# tunnel; github.com is allowlisted in demo.json).
# ─────────────────────────────────────────────────────────────────────────────
if run_section 1; then
  log "Section 1: repo clone through governed egress"
  note "Creates a run with repo=${PUBLIC_REPO}; sandbox clones it via the proxy"

  S1_RUN_ID="$(create_run --task "wardyn test-drive section 1")"
  [[ -n "${S1_RUN_ID}" ]] || { bad "section 1: could not create run"; }

  if [[ -n "${S1_RUN_ID}" ]]; then
    TD_RUN_IDS+=("${S1_RUN_ID}")
    note "run ${S1_RUN_ID} created"

    if wait_state "${S1_RUN_ID}" "RUNNING" 25 2; then
      ok "section 1: sandbox RUNNING"
    else
      bad "section 1: sandbox did not reach RUNNING"
    fi

    S1_CTR="wardyn-agent-${S1_RUN_ID}"
    S1_REPO_DIR="/home/agent/work/Hello-World"

    # Give the clone a moment to complete (it happens during agent startup).
    sleep 4

    S1_LS="$(docker exec "${S1_CTR}" ls "${S1_REPO_DIR}" 2>&1 || echo "EXEC_FAILED")"
    if echo "${S1_LS}" | grep -v EXEC_FAILED | grep -qiE 'README|\.git|EXEC_FAILED' 2>/dev/null; then
      if echo "${S1_LS}" | grep -q EXEC_FAILED; then
        bad "section 1: docker exec failed — clone did not happen (dir absent)"
        note "section 1: ls output: ${S1_LS}"
      else
        ok "section 1: workspace cloned (ls ${S1_REPO_DIR}: ${S1_LS//$'\n'/ | })"
      fi
    else
      # Check whether the dir itself exists at all as a partial indicator
      if docker exec "${S1_CTR}" test -d "${S1_REPO_DIR}" 2>/dev/null; then
        ok "section 1: clone dir exists (${S1_REPO_DIR}); ls: ${S1_LS//$'\n'/ | }"
      else
        bad "section 1: clone dir absent (${S1_REPO_DIR}); check proxy egress + agent-run clone step"
        note "section 1: ls: ${S1_LS}"
        note "section 1: the current agent-run does not auto-clone; see docs/TRY-IT.md for the"
        note "section 1: wardyn-git-helper-based clone path (section 5 proves that chain)"
      fi
    fi

    # Show the sandbox workspace regardless so operators see what is there.
    WORK_LS="$(docker exec "${S1_CTR}" ls /home/agent/work 2>&1 || echo "<exec failed>")"
    note "section 1: workspace contents: ${WORK_LS//$'\n'/ | }"

    # Prove the proxy let git through: the network path is proxy-only (no default
    # route), so any successful outbound TCP implies the proxy allowed it.
    # Read the kernel route table from /proc/net/route directly: the slim agent
    # image ships no `ip`/`route` binary, so an `ip route` exec would error and a
    # naive grep for "default" would FALSELY report no default route. A default
    # route is a row whose Destination column (field 2) is all-zero ("00000000").
    ROUTE="$(docker exec "${S1_CTR}" cat /proc/net/route 2>&1)"
    if echo "${ROUTE}" | awk 'NR>1 && $2=="00000000"{found=1} END{exit !found}'; then
      bad "section 1: sandbox HAS a default route (L0 broken)"
    else
      ok "section 1: L0 enforced — no default route (only proxy egress exists)"
    fi

    note "section 1: /proc/net/route: ${ROUTE//$'\n'/ ; }"
  fi
  echo
fi

# ─────────────────────────────────────────────────────────────────────────────
# SECTION 2: Egress deny
# docker exec curl to a domain NOT in the allowlist via the proxy -> 403.
# The audit trail shows the egress decision.
# ─────────────────────────────────────────────────────────────────────────────
if run_section 2; then
  log "Section 2: egress deny — unlisted domain blocked by proxy"
  note "Probes evil.example.com through wardyn-proxy; expects 403 (or first-use hold)"

  S2_RUN_ID="${S1_RUN_ID:-}"
  S2_OWN=0
  if [[ -z "${S2_RUN_ID}" ]]; then
    S2_RUN_ID="$(create_run --task "wardyn test-drive section 2")"
    [[ -n "${S2_RUN_ID}" ]] || { bad "section 2: could not create run"; echo; }
    if [[ -n "${S2_RUN_ID}" ]]; then
      TD_RUN_IDS+=("${S2_RUN_ID}")
      S2_OWN=1
      wait_state "${S2_RUN_ID}" "RUNNING" 25 2 || bad "section 2: sandbox not RUNNING"
    fi
  fi

  if [[ -n "${S2_RUN_ID}" ]]; then
    S2_CTR="wardyn-agent-${S2_RUN_ID}"
    S2_DENY="$(docker exec "${S2_CTR}" curl -sS -m 10 --connect-timeout 8 \
                 -x http://wardyn-proxy:3128 https://evil.example.com/ 2>&1 || true)"
    if echo "${S2_DENY}" | grep -qE '403|PENDING|first.?use|approval'; then
      ok "section 2: unlisted domain blocked by proxy (403 / pending hold)"
    else
      bad "section 2: expected 403/pending for evil.example.com; got: ${S2_DENY}"
    fi
    note "section 2: proxy response: ${S2_DENY}"

    # Show the audit event for this run.
    sleep 2
    S2_AUDIT="$(api "/api/v1/audit?run_id=${S2_RUN_ID}")"
    S2_DENY_CNT="$(printf '%s' "${S2_AUDIT}" \
      | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if e.get("action") in ("egress.deny","egress.pending") and "evil" in json.dumps(e)))' 2>/dev/null || echo 0)"
    if [[ "${S2_DENY_CNT}" -ge 1 ]]; then
      ok "section 2: egress.deny/pending audit event visible for evil.example.com"
    else
      note "section 2: audit event not yet visible (async flush); check UI Audit tab"
    fi

    [[ "${S2_OWN}" -eq 1 && "${OPT_KEEP}" -eq 0 ]] && \
      { hc -o /dev/null -X POST "${WARDYN_URL}/api/v1/runs/${S2_RUN_ID}/kill" \
          -H "${ADMIN_HDR}" >/dev/null 2>&1 || true; }
  fi
  echo
fi

# ─────────────────────────────────────────────────────────────────────────────
# SECTION 3: Metadata block
# docker exec curl 169.254.169.254 through the proxy -> unconditional 403.
# Proves builtin deny for link-local / cloud metadata IPs (invariant 3).
# ─────────────────────────────────────────────────────────────────────────────
if run_section 3; then
  log "Section 3: metadata IP block (169.254.169.254 -> builtin deny)"
  note "The proxy unconditionally blocks link-local/metadata IPs regardless of policy"

  S3_RUN_ID="${S1_RUN_ID:-}"
  S3_OWN=0
  if [[ -z "${S3_RUN_ID}" ]]; then
    S3_RUN_ID="$(create_run --task "wardyn test-drive section 3")"
    [[ -n "${S3_RUN_ID}" ]] || { bad "section 3: could not create run"; echo; }
    if [[ -n "${S3_RUN_ID}" ]]; then
      TD_RUN_IDS+=("${S3_RUN_ID}")
      S3_OWN=1
      wait_state "${S3_RUN_ID}" "RUNNING" 25 2 || bad "section 3: sandbox not RUNNING"
    fi
  fi

  if [[ -n "${S3_RUN_ID}" ]]; then
    S3_CTR="wardyn-agent-${S3_RUN_ID}"

    # Probe 1: direct connection attempt (no default route -> should fail to
    # connect). Capture ONLY the clean http_code on stdout (stderr -> /dev/null);
    # a connection that never establishes yields http_code "000". Mixing curl's
    # stderr into the captured value (2>&1) would corrupt the comparison.
    S3_DIRECT="$(docker exec "${S3_CTR}" curl -s -o /dev/null -m 6 --connect-timeout 5 \
                   -w '%{http_code}' http://169.254.169.254/ 2>/dev/null || true)"
    if [[ "${S3_DIRECT}" == "000" || "${S3_DIRECT}" == "" ]]; then
      ok "section 3: 169.254.169.254 unreachable directly (L0 — no default route)"
    else
      bad "section 3: direct connection to 169.254.169.254 got http_code=${S3_DIRECT} (L0 concern)"
    fi

    # Probe 2: via proxy -> must be 403 (builtin deny, not even checked against
    # allowlist). The proxy returns a clean 403, so stdout carries the code;
    # suppress stderr so a transient curl warning can never corrupt the value.
    S3_VIA_PROXY="$(docker exec "${S3_CTR}" curl -s -o /dev/null -m 8 --connect-timeout 6 \
                      -w '%{http_code}' -x http://wardyn-proxy:3128 http://169.254.169.254/ 2>/dev/null || echo "000")"
    if [[ "${S3_VIA_PROXY}" == "403" ]]; then
      ok "section 3: 169.254.169.254 blocked by proxy (403 builtin deny)"
    else
      bad "section 3: metadata IP via proxy returned ${S3_VIA_PROXY} (expected 403)"
    fi

    # Audit evidence.
    sleep 2
    S3_AUDIT="$(api "/api/v1/audit?run_id=${S3_RUN_ID}")"
    S3_DENY_CNT="$(printf '%s' "${S3_AUDIT}" \
      | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if e.get("action")=="egress.deny"))' 2>/dev/null || echo 0)"
    if [[ "${S3_DENY_CNT}" -ge 1 ]]; then
      ok "section 3: egress.deny audit event(s) visible for this run (${S3_DENY_CNT})"
    else
      note "section 3: egress.deny not yet in audit (async); check UI Audit tab"
    fi

    [[ "${S3_OWN}" -eq 1 && "${OPT_KEEP}" -eq 0 ]] && \
      { hc -o /dev/null -X POST "${WARDYN_URL}/api/v1/runs/${S3_RUN_ID}/kill" \
          -H "${ADMIN_HDR}" >/dev/null 2>&1 || true; }
  fi
  echo
fi

# ─────────────────────────────────────────────────────────────────────────────
# SECTION 4: First-use approval
# The demo policy has first_use_approval=true.  Probing an unknown domain
# raises a PENDING egress_domain ApprovalRequest.  We approve it via the API
# and confirm the state change and audit trail.
# ─────────────────────────────────────────────────────────────────────────────
if run_section 4; then
  log "Section 4: first-use approval workflow"
  note "Probes an unknown domain; first_use_approval raises a PENDING approval"
  note "Then approves it via the API and shows the state change + audit"

  S4_RUN_ID="$(create_run --task "wardyn test-drive section 4")"
  [[ -n "${S4_RUN_ID}" ]] || { bad "section 4: could not create run"; echo; }

  if [[ -n "${S4_RUN_ID}" ]]; then
    TD_RUN_IDS+=("${S4_RUN_ID}")
    wait_state "${S4_RUN_ID}" "RUNNING" 25 2 || bad "section 4: sandbox not RUNNING"

    S4_CTR="wardyn-agent-${S4_RUN_ID}"
    S4_DOMAIN="td-s4-$(date +%s).example.com"
    note "section 4: probing domain ${S4_DOMAIN} (will trigger first-use approval)"

    # Trigger the first-use hold (background, because it may block until approved).
    docker exec "${S4_CTR}" curl -sS -m 20 --connect-timeout 10 \
      -x http://wardyn-proxy:3128 "https://${S4_DOMAIN}/" >/dev/null 2>&1 &
    S4_CURL_PID=$!

    # Poll for the PENDING approval.
    S4_AP_ID=""
    for _ in $(seq 1 15); do
      S4_AP_ID="$(api "/api/v1/approvals?state=PENDING" \
        | python3 -c "
import sys, json
aps = json.load(sys.stdin)
dom = '${S4_DOMAIN}'
print(next((a['id'] for a in aps
            if a.get('kind') == 'egress_domain'
               and dom in json.dumps(a.get('requested_scope', {}))), ''))
" 2>/dev/null || echo "")"
      [[ -n "${S4_AP_ID}" ]] && break
      sleep 2
    done

    if [[ -n "${S4_AP_ID}" ]]; then
      ok "section 4: PENDING egress_domain ApprovalRequest raised (id=${S4_AP_ID})"
    else
      bad "section 4: no PENDING egress_domain approval surfaced for ${S4_DOMAIN}"
      note "section 4: current PENDING approvals:"
      api "/api/v1/approvals?state=PENDING" | python3 -c \
        'import sys,json;[print(" ",a["id"],a.get("kind"),json.dumps(a.get("requested_scope",{}))[:80]) for a in json.load(sys.stdin)]' 2>/dev/null || true
    fi

    if [[ -n "${S4_AP_ID}" ]]; then
      # Approve via the API.
      S4_APP_CODE="$(hc -o /dev/null -w '%{http_code}' \
        -X POST "${WARDYN_URL}/api/v1/approvals/${S4_AP_ID}/approve" \
        -H "${ADMIN_HDR}")"
      if [[ "${S4_APP_CODE}" == "200" || "${S4_APP_CODE}" == "202" ]]; then
        ok "section 4: approval APPROVED via API (http_code=${S4_APP_CODE})"
      else
        bad "section 4: approve returned ${S4_APP_CODE}"
      fi

      # Confirm state change. The admin API exposes only the LIST surface
      # (GET /api/v1/approvals?state=) plus approve/deny — there is no
      # single-approval GET route — so confirm by finding our id in the
      # APPROVED list rather than fetching it directly (a direct GET would 404).
      sleep 1
      S4_STATE="$(api "/api/v1/approvals?state=APPROVED" \
        | python3 -c "
import sys, json
aid = '${S4_AP_ID}'
aps = json.load(sys.stdin)
print(next((a.get('state','') for a in aps if a.get('id') == aid), ''))
" 2>/dev/null || echo "")"
      if [[ "${S4_STATE}" == "APPROVED" ]]; then
        ok "section 4: approval state is now APPROVED"
      else
        bad "section 4: approval state=${S4_STATE:-<not found in APPROVED list>} (expected APPROVED)"
      fi

      # Show audit event for the approval.
      sleep 2
      S4_AUDIT="$(api "/api/v1/audit?run_id=${S4_RUN_ID}")"
      S4_AP_AUDIT="$(printf '%s' "${S4_AUDIT}" \
        | python3 -c "
import sys, json
eid = '${S4_AP_ID}'
print(sum(1 for e in json.load(sys.stdin)
          if 'approval' in e.get('action','') or eid in json.dumps(e)))
" 2>/dev/null || echo 0)"
      if [[ "${S4_AP_AUDIT}" -ge 1 ]]; then
        ok "section 4: approval-related audit event visible"
      else
        note "section 4: approval audit event not yet flushed (async)"
      fi
    fi

    # Clean up the background curl regardless.
    kill "${S4_CURL_PID}" 2>/dev/null || true; wait "${S4_CURL_PID}" 2>/dev/null || true

    [[ "${OPT_KEEP}" -eq 0 ]] && \
      { hc -o /dev/null -X POST "${WARDYN_URL}/api/v1/runs/${S4_RUN_ID}/kill" \
          -H "${ADMIN_HDR}" >/dev/null 2>&1 || true; }
  fi
  echo
fi

# ─────────────────────────────────────────────────────────────────────────────
# SECTION 5: Brokered git credential
# docker exec wardyn-git-helper get for github.com -> raises a credential
# ApprovalRequest (requires_approval=true in demo.json) -> we approve it ->
# the broker fails closed (no GitHub App configured in demo) -> no token in
# sandbox env (invariant 1: secrets never in sandbox).
# ─────────────────────────────────────────────────────────────────────────────
if run_section 5; then
  log "Section 5: brokered git credential chain"
  note "Proves the full chain: sandbox -> proxy(token inject) -> broker -> approval"
  note "Demo has no GitHub App, so the broker fails closed; no token leaks to env"

  S5_RUN_ID="$(create_run --task "wardyn test-drive section 5")"
  [[ -n "${S5_RUN_ID}" ]] || { bad "section 5: could not create run"; echo; }

  if [[ -n "${S5_RUN_ID}" ]]; then
    TD_RUN_IDS+=("${S5_RUN_ID}")
    wait_state "${S5_RUN_ID}" "RUNNING" 25 2 || bad "section 5: sandbox not RUNNING"

    S5_CTR="wardyn-agent-${S5_RUN_ID}"
    TMPOUT="/tmp/wardyn-td-s5-${S5_RUN_ID}.out"
    TMPERR="/tmp/wardyn-td-s5-${S5_RUN_ID}.err"

    note "section 5: running wardyn-git-helper get (background, 60s approval timeout)"
    docker exec -e WARDYN_APPROVAL_TIMEOUT=60s "${S5_CTR}" sh -c \
      'printf "protocol=https\nhost=github.com\n\n" | /usr/local/bin/wardyn-git-helper get' \
      >"${TMPOUT}" 2>"${TMPERR}" &
    S5_PID=$!

    # Poll for the credential ApprovalRequest.
    S5_CRED_AP=""
    for _ in $(seq 1 20); do
      S5_CRED_AP="$(api "/api/v1/approvals?state=PENDING" \
        | python3 -c "
import sys, json
rid = '${S5_RUN_ID}'
aps = json.load(sys.stdin)
print(next((a['id'] for a in aps
            if a.get('kind') == 'credential' and a.get('run_id') == rid), ''))
" 2>/dev/null || echo "")"
      [[ -n "${S5_CRED_AP}" ]] && break
      sleep 2
    done

    if [[ -n "${S5_CRED_AP}" ]]; then
      ok "section 5: credential ApprovalRequest raised (id=${S5_CRED_AP})"
    else
      bad "section 5: no PENDING credential approval for run ${S5_RUN_ID}"
    fi

    # Approve -> triggers the broker mint path (fails closed, no GitHub App).
    if [[ -n "${S5_CRED_AP}" ]]; then
      S5_APP_CODE="$(hc -o /dev/null -w '%{http_code}' \
        -X POST "${WARDYN_URL}/api/v1/approvals/${S5_CRED_AP}/approve" \
        -H "${ADMIN_HDR}")"
      if [[ "${S5_APP_CODE}" == "200" || "${S5_APP_CODE}" == "202" ]]; then
        ok "section 5: credential approval APPROVED (${S5_APP_CODE})"
      else
        bad "section 5: approve returned ${S5_APP_CODE}"
      fi
    fi

    # Wait for the helper to exit.
    for _ in $(seq 1 30); do kill -0 "${S5_PID}" 2>/dev/null || break; sleep 2; done
    if kill -0 "${S5_PID}" 2>/dev/null; then
      note "section 5: git-helper still running after 60s; killing"
      kill "${S5_PID}" 2>/dev/null || true
    fi
    wait "${S5_PID}" 2>/dev/null || true

    S5_STDOUT="$(cat "${TMPOUT}" 2>/dev/null || echo "")"
    S5_STDERR="$(cat "${TMPERR}" 2>/dev/null || echo "")"

    note "section 5: git-helper stdout: ${S5_STDOUT:-<empty>}"
    note "section 5: git-helper stderr: ${S5_STDERR:-<empty>}"

    # Fail-closed proof: no token emitted + the documented error message appears.
    if echo "${S5_STDOUT}" | grep -q '^password='; then
      bad "section 5: git-helper emitted a token (fail-closed broken; token leaked)"
    elif echo "${S5_STDERR}" | grep -qiE 'no GitHubMinter configured|mint status 500|fail.?closed|github_token grant but no'; then
      ok "section 5: broker failed closed (no GitHub App); full chain proven; no token"
    else
      note "section 5: fail-closed error message not matched; may be expected if approval timed out"
    fi

    # Invariant 1: no token in sandbox env.
    S5_ENV="$(docker exec "${S5_CTR}" env 2>/dev/null || echo "")"
    if echo "${S5_ENV}" | grep -iE 'token|ghp_|github.*key' | grep -v 'GRANT_ID'; then
      bad "section 5: token-like value found in sandbox env (invariant 1 violated)"
    else
      ok "section 5: sandbox env contains no token/key values (invariant 1 preserved)"
    fi

    rm -f "${TMPOUT}" "${TMPERR}" 2>/dev/null || true

    [[ "${OPT_KEEP}" -eq 0 ]] && \
      { hc -o /dev/null -X POST "${WARDYN_URL}/api/v1/runs/${S5_RUN_ID}/kill" \
          -H "${ADMIN_HDR}" >/dev/null 2>&1 || true; }
  fi
  echo
fi

# ─────────────────────────────────────────────────────────────────────────────
# SECTION 6: Kill cascade
# wardyn kill -> container gone + run token 401 + run state KILLED + run.kill
# audit event (actor_type=human).
# ─────────────────────────────────────────────────────────────────────────────
if run_section 6; then
  log "Section 6: kill cascade"
  note "Kills the run; asserts container gone + token revoked + audit event"

  S6_RUN_ID="$(create_run --task "wardyn test-drive section 6")"
  [[ -n "${S6_RUN_ID}" ]] || { bad "section 6: could not create run"; echo; }

  if [[ -n "${S6_RUN_ID}" ]]; then
    TD_RUN_IDS+=("${S6_RUN_ID}")
    wait_state "${S6_RUN_ID}" "RUNNING" 25 2 || bad "section 6: sandbox not RUNNING"

    S6_CTR="wardyn-agent-${S6_RUN_ID}"
    S6_PROXY_CTR="wardyn-proxy-${S6_RUN_ID}"

    # Capture the run token from the proxy config env so we can test 401 after kill.
    S6_TOKEN="$(docker inspect "${S6_PROXY_CTR}" --format \
      '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
      | sed -n 's/^WARDYN_PROXY_CONFIG_JSON=//p' \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["run_token"])' 2>/dev/null || echo "")"

    [[ -n "${S6_TOKEN}" ]] \
      && note "section 6: run token captured from proxy config" \
      || note "section 6: run token not found; post-kill 401 check will be skipped"

    # Verify the token works before kill (via the internal decisions endpoint).
    if [[ -n "${S6_TOKEN}" ]]; then
      S6_PRE="$(hc -o /dev/null -w '%{http_code}' \
        -X POST "${WARDYN_URL}/api/v1/internal/decisions" \
        -H "Authorization: Bearer ${S6_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{"request":{"host":"prekill.example","port":443,"method":"CONNECT"},"decision":"deny","rule_source":"test-drive:s6"}' \
        2>/dev/null || echo "000")"
      if [[ "${S6_PRE}" == "202" ]]; then
        ok "section 6: pre-kill token is valid (202)"
      else
        note "section 6: pre-kill token returned ${S6_PRE} (expected 202)"
      fi
    fi

    # Issue the kill.
    S6_KILL_CODE="$(hc -o /dev/null -w '%{http_code}' \
      -X POST "${WARDYN_URL}/api/v1/runs/${S6_RUN_ID}/kill" \
      -H "${ADMIN_HDR}" 2>/dev/null || echo "000")"
    if [[ "${S6_KILL_CODE}" == "202" ]]; then
      ok "section 6: kill accepted (202)"
    else
      bad "section 6: kill returned ${S6_KILL_CODE} (expected 202)"
    fi

    sleep 2

    # Container gone.
    S6_GONE="$(docker ps -a --filter "name=${S6_CTR}" --format '{{.Names}}' 2>/dev/null || echo "")"
    if [[ -z "${S6_GONE}" ]]; then
      ok "section 6: agent container is gone"
    else
      bad "section 6: agent container still present: ${S6_GONE}"
    fi

    # Run token revoked -> 401.
    if [[ -n "${S6_TOKEN}" ]]; then
      S6_POST="$(hc -o /dev/null -w '%{http_code}' \
        -X POST "${WARDYN_URL}/api/v1/internal/decisions" \
        -H "Authorization: Bearer ${S6_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{"request":{"host":"postkill.example","port":443,"method":"CONNECT"},"decision":"deny","rule_source":"test-drive:s6"}' \
        2>/dev/null || echo "000")"
      if [[ "${S6_POST}" == "401" ]]; then
        ok "section 6: run token revoked (401 post-kill)"
      else
        bad "section 6: post-kill token returned ${S6_POST} (expected 401)"
      fi
    fi

    # Run state.
    S6_STATE="$(api "/api/v1/runs/${S6_RUN_ID}" \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["state"])' 2>/dev/null || echo "")"
    if [[ "${S6_STATE}" == "KILLED" ]]; then
      ok "section 6: run state is KILLED"
    else
      bad "section 6: run state=${S6_STATE} (expected KILLED)"
    fi

    # run.kill audit event.
    S6_AUDIT="$(api "/api/v1/audit?run_id=${S6_RUN_ID}")"
    S6_KILL_AUDIT="$(printf '%s' "${S6_AUDIT}" \
      | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if e.get("action")=="run.kill" and e.get("actor_type")=="human"))' \
      2>/dev/null || echo 0)"
    if [[ "${S6_KILL_AUDIT}" -ge 1 ]]; then
      ok "section 6: run.kill audit event with actor_type=human present"
    else
      bad "section 6: no run.kill audit event (actor_type=human)"
    fi
  fi
  echo
fi

# ─────────────────────────────────────────────────────────────────────────────
# SECTION 7: Recording
# GET the asciicast recording artifact for a completed or killed run -> 200.
# Uses the same manual-upload path exercised by the e2e suite to ensure there
# is an artifact to retrieve; then confirms the admin-gated endpoint serves it.
# ─────────────────────────────────────────────────────────────────────────────
if run_section 7; then
  log "Section 7: recording artifact"
  note "Uploads a synthetic asciicast then retrieves it via the admin API"

  S7_RUN_ID="$(create_run --task "wardyn test-drive section 7")"
  [[ -n "${S7_RUN_ID}" ]] || { bad "section 7: could not create run"; echo; }

  if [[ -n "${S7_RUN_ID}" ]]; then
    TD_RUN_IDS+=("${S7_RUN_ID}")
    wait_state "${S7_RUN_ID}" "RUNNING" 25 2 || bad "section 7: sandbox not RUNNING"

    S7_PROXY_CTR="wardyn-proxy-${S7_RUN_ID}"

    # Capture the run token so we can upload via the run-token-gated endpoint.
    S7_TOKEN="$(docker inspect "${S7_PROXY_CTR}" --format \
      '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
      | sed -n 's/^WARDYN_PROXY_CONFIG_JSON=//p' \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["run_token"])' 2>/dev/null || echo "")"

    if [[ -z "${S7_TOKEN}" ]]; then
      bad "section 7: could not capture run token from proxy config; skipping upload"
    else
      # Upload a minimal asciicast.
      S7_CAST_TMP="$(mktemp /tmp/wardyn-td-s7.XXXXXX.cast)"
      printf '{"version": 2, "width": 80, "height": 24, "timestamp": %s, "title": "wardyn-test-drive"}\n' \
        "$(date +%s)" > "${S7_CAST_TMP}"
      printf '[0.1, "o", "wardyn test-drive section 7\\r\\n"]\n' >> "${S7_CAST_TMP}"

      S7_UP="$(hc -o /dev/null -w '%{http_code}' \
        -X PUT "${WARDYN_URL}/api/v1/internal/recordings/${S7_RUN_ID}" \
        -H "Authorization: Bearer ${S7_TOKEN}" \
        --data-binary "@${S7_CAST_TMP}" 2>/dev/null || echo "000")"
      rm -f "${S7_CAST_TMP}" 2>/dev/null || true

      if [[ "${S7_UP}" == "204" ]]; then
        ok "section 7: recording uploaded via run token (204)"
      else
        bad "section 7: recording upload returned ${S7_UP} (expected 204)"
      fi

      # Retrieve via admin token.
      S7_GET="$(hc -o /dev/null -w '%{http_code}' \
        -H "${ADMIN_HDR}" \
        "${WARDYN_URL}/api/v1/runs/${S7_RUN_ID}/recording/${S7_RUN_ID}" \
        2>/dev/null || echo "000")"
      if [[ "${S7_GET}" == "200" ]]; then
        ok "section 7: recording served by admin endpoint (200)"
      else
        bad "section 7: GET recording returned ${S7_GET} (expected 200)"
      fi

      # No-auth must be 401.
      S7_NOAUTH="$(hc -o /dev/null -w '%{http_code}' \
        "${WARDYN_URL}/api/v1/runs/${S7_RUN_ID}/recording/${S7_RUN_ID}" \
        2>/dev/null || echo "000")"
      if [[ "${S7_NOAUTH}" == "401" ]]; then
        ok "section 7: unauthenticated request correctly rejected (401)"
      else
        bad "section 7: unauthenticated request returned ${S7_NOAUTH} (expected 401)"
      fi
    fi

    [[ "${OPT_KEEP}" -eq 0 ]] && \
      { hc -o /dev/null -X POST "${WARDYN_URL}/api/v1/runs/${S7_RUN_ID}/kill" \
          -H "${ADMIN_HDR}" >/dev/null 2>&1 || true; }
  fi
  echo
fi

# ─────────────────────────────────────────────────────────────────────────────
# OPTIONAL: Real Claude task scenario (requires ANTHROPIC_API_KEY)
# ─────────────────────────────────────────────────────────────────────────────
if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
  log "Optional: real Claude task scenario (ANTHROPIC_API_KEY is set)"
  note "Requires the LLM-enabled policy (claude-llm.json) and the secret pre-loaded."
  note "Restart wardynd with WARDYN_DEFAULT_POLICY=/examples/policies/claude-llm.json"
  note "and run: echo \"\$ANTHROPIC_API_KEY\" | wardyn secret set anthropic-api-key"
  note "Then re-run with ANTHROPIC_API_KEY set to trigger a real claude task."
  note "See docs/TRY-IT.md Level 2 for the full procedure."
  pass=$((pass+0))  # no assertions, just guidance
else
  note "ANTHROPIC_API_KEY not set — real Claude task scenario skipped"
  note "Set it and see docs/TRY-IT.md Level 2 for a guided LLM-enabled run"
fi
echo

# ─────────────────────────────────────────────────────────────────────────────
# Summary
# ─────────────────────────────────────────────────────────────────────────────
printf '\033[1;36m━━━  test-drive summary  ━━━\033[0m\n'
if [[ ${fail} -eq 0 ]]; then
  printf '\033[1;32m  %d passed, 0 failed\033[0m\n' "${pass}"
else
  printf '\033[1;31m  %d passed, %d failed\033[0m\n' "${pass}" "${fail}"
fi
echo
note "For agent-task adversarial scenarios see: examples/workspaces/"
note "UI:        ${WARDYN_URL}  (SSO: demo@wardyn.local / password)"
note "Admin API: Authorization: Bearer ${WARDYN_ADMIN_TOKEN}"
note "Tear down: docker compose -f ${COMPOSE_FILE} down -v"
echo

[[ ${fail} -eq 0 ]] || exit 1
