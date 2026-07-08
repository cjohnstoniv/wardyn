#!/usr/bin/env bash
# Wardyn end-to-end validator.
#
# Stands up the real compose stack (postgres + dex + wardynd built with the
# docker runner), creates a governed run against a locally-built fixture agent
# image, and asserts the security invariants from ARCHITECTURE.md against LIVE
# containers and the LIVE control-plane API:
#
#   (a) the agent sandbox has NO default route (L0 structural egress)
#   (b) the metadata IP 169.254.169.254 is unreachable from the sandbox
#   (c) a disallowed domain + the metadata IP are DENIED by the proxy, and the
#       deny decisions surface as audit events via the API
#   (d) an allowed domain passes THROUGH the proxy (CONNECT tunnel established)
#   (e) kill cascade: kill -> agent container gone + run identity revoked
#       (the old run token now 401s on an internal endpoint) + run.kill audit
#   (f) a recording artifact uploaded via the run-token-gated internal endpoint
#       is served back by the admin-gated GET /runs/{id}/recording/{runID}
#   (g) the OIDC login flow against Dex completes and a session cookie (no admin
#       bearer) authenticates GET /api/v1/runs
#
# GUARD: this is a heavyweight, Docker-dependent test. It is a NO-OP unless
# WARDYN_TEST_DOCKER=1 is set (mirrors the integration-test convention so plain
# `go test ./...` / CI without Docker is unaffected).
#
# RE-RUNNABLE: every step is idempotent and the stack + fixture images are torn
# down on exit (success or failure) unless WARDYN_E2E_KEEP=1.
#
# Usage:
#   WARDYN_TEST_DOCKER=1 test/e2e/e2e.sh             # full run + teardown
#   WARDYN_TEST_DOCKER=1 WARDYN_E2E_KEEP=1 test/e2e/e2e.sh   # keep stack up
#   WARDYN_TEST_DOCKER=1 WARDYN_E2E_NO_BUILD=1 test/e2e/e2e.sh   # reuse images
set -uo pipefail

# ── guard ────────────────────────────────────────────────────────────────────
if [[ "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  echo "e2e: set WARDYN_TEST_DOCKER=1 to run the Docker-dependent e2e suite (skipping)."
  exit 0
fi

# ── config ─────────────────────────────────────────────────────────────────--
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/deploy/compose/docker-compose.yaml"
FIXTURE_DIR="${REPO_ROOT}/test/e2e/fixtures"
COMPOSE=(docker compose -f "${COMPOSE_FILE}")

ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN:-demo-admin-token}"
BASE="http://localhost:8080"
INTERNAL_NET="wardyn-internal"           # compose network name (see compose `networks:`)
FIXTURE_AGENT="e2e-fixture"              # agent NAME -> image ghcr.io/.../agent-e2e-fixture
FIXTURE_IMAGE="ghcr.io/cjohnstoniv/agent-${FIXTURE_AGENT}:latest"
PROXY_IMAGE="wardyn/wardyn-proxy:demo"
CURL_HELPER="curlimages/curl:latest"

WORKDIR="$(mktemp -d /tmp/wardyn-e2e.XXXXXX)"
RUN_ID=""
CC_RUN_ID=""                             # the REAL-AGENT (claude-code) run id
PROXY_NAME=""
CC_AGENT="claude-code"                   # real agent NAME (image via WARDYN_AGENT_IMAGES)
CC_IMAGE="wardyn/agent-claude-code:demo" # the demo tag the compose stack maps to

pass=0; fail=0
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  PASS\033[0m %s\n' "$*"; pass=$((pass+1)); }
bad()  { printf '\033[1;31m  FAIL\033[0m %s\n' "$*"; fail=$((fail+1)); }
note() { printf '\033[1;33m  NOTE\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; teardown; exit 1; }

# ── teardown (always runs) ─────────────────────────────────────────────────--
teardown() {
  [[ "${WARDYN_E2E_KEEP:-}" == "1" ]] && { log "WARDYN_E2E_KEEP=1 set; leaving stack up"; return; }
  log "Tearing down"
  [[ -n "${PROXY_NAME}" ]] && docker rm -f "${PROXY_NAME}" >/dev/null 2>&1 || true
  for rid in "${RUN_ID}" "${CC_RUN_ID}"; do
    [[ -n "${rid}" ]] || continue
    # Best-effort: remove any per-run sandbox artifacts the kill cascade left.
    docker rm -f "wardyn-agent-${rid}" "wardyn-proxy-${rid}" >/dev/null 2>&1 || true
    docker network rm "wardyn-int-${rid}" >/dev/null 2>&1 || true
  done
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  docker rmi -f "${FIXTURE_IMAGE}" >/dev/null 2>&1 || true
  rm -rf "${WORKDIR}" >/dev/null 2>&1 || true
}
trap teardown EXIT

# curl run from inside a helper container ON the compose network, so it can
# resolve `wardynd` and `dex` exactly as the sidecars do. $@ is the curl argv.
nci() { docker run --rm --network "${INTERNAL_NET}" "${CURL_HELPER}" "$@"; }
# curl run with a mounted script (for the multi-step OIDC flow).
ncis() { docker run --rm --network "${INTERNAL_NET}" -v "$1":/flow.sh:ro "${CURL_HELPER}" sh /flow.sh; }
# host-side curl against the published port.
hc() { curl -sS "$@"; }

# ── 0. preflight ───────────────────────────────────────────────────────────--
command -v docker >/dev/null 2>&1 || die "docker not found"
docker compose version >/dev/null 2>&1 || die "docker compose v2 required"

# ── 1. build images + fixture, bring stack up ──────────────────────────────--
if [[ "${WARDYN_E2E_NO_BUILD:-}" != "1" ]]; then
  log "Building wardynd (-tags docker) + wardyn-proxy images"
  "${COMPOSE[@]}" build wardynd >/dev/null || die "build wardynd failed"
  "${COMPOSE[@]}" --profile build-only build proxy-image >/dev/null || die "build proxy failed"
  # The REAL-AGENT section needs the claude-code image (ships wardyn-rec,
  # wardyn-git-helper, agent-run, asciinema, claude CLI). Build it from the
  # repo-root context so the Go builder stage sees the module sources. This is
  # exactly `make agent-images` (claude-code half).
  log "Building real agent image (${CC_IMAGE})"
  docker build -q -f "${REPO_ROOT}/deploy/images/claude-code/Dockerfile" \
    -t "${CC_IMAGE}" "${REPO_ROOT}" >/dev/null || die "build claude-code agent image failed"
fi
log "Building fixture agent image (${FIXTURE_IMAGE})"
docker build -q -t "${FIXTURE_IMAGE}" "${FIXTURE_DIR}" >/dev/null || die "fixture build failed"

log "Starting postgres + dex + wardynd"
"${COMPOSE[@]}" up -d postgres dex wardynd >/dev/null || die "compose up failed"

log "Waiting for wardynd to become healthy"
tries=0
until [[ "$(docker inspect -f '{{.State.Health.Status}}' wardyn-api 2>/dev/null)" == "healthy" ]]; do
  tries=$((tries+1)); [[ ${tries} -gt 60 ]] && { "${COMPOSE[@]}" logs --tail 40 wardynd; die "wardynd unhealthy"; }
  sleep 2
done
ok "stack healthy ($(hc "${BASE}/healthz"))"

# ── 2. create a governed run against the fixture image ─────────────────────--
log "Creating a governed run (agent=${FIXTURE_AGENT})"
CREATE="$("${COMPOSE[@]}" exec -T -e WARDYN_URL="${BASE}" -e WARDYN_ADMIN_TOKEN="${ADMIN_TOKEN}" \
  wardynd /usr/local/bin/wardyn run --agent "${FIXTURE_AGENT}" --repo octocat/Hello-World --task "wardyn e2e")"
echo "${CREATE}"
RUN_ID="$(printf '%s\n' "${CREATE}" | awk '/^created run/{print $3; exit}')"
[[ -n "${RUN_ID}" ]] || die "could not parse created run id"
sleep 1
STATE="$(hc -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/api/v1/runs/${RUN_ID}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["state"])')"
if [[ "${STATE}" == "RUNNING" ]]; then ok "run dispatched to RUNNING (live sandbox created)"; else
  bad "run state=${STATE}, expected RUNNING (sandbox dispatch failed)"; fi
AGENT="wardyn-agent-${RUN_ID}"

# (h) GAP-1 closure: the auto-launched proxy sidecar must be RUNNING (it used
# to crash-loop on missing -config). The driver now delivers the full config —
# including this run's egress policy — via WARDYN_PROXY_CONFIG_JSON.
log "(h) auto-launched wardyn-proxy sidecar is healthy"
PROXY_STATE="$(docker inspect "wardyn-proxy-${RUN_ID}" --format '{{.State.Status}} restarts={{.RestartCount}}' 2>/dev/null || echo missing)"
if [[ "${PROXY_STATE}" == running* ]]; then ok "proxy sidecar ${PROXY_STATE}"; else
  bad "proxy sidecar not running: ${PROXY_STATE}"; fi
if docker logs "wardyn-proxy-${RUN_ID}" 2>&1 | grep -q "listening"; then
  ok "proxy logs show it is listening (config accepted, policy loaded)"; else
  bad "proxy logs missing 'listening': $(docker logs "wardyn-proxy-${RUN_ID}" 2>&1 | tail -3)"; fi

# Capture the run token from the proxy sidecar's config env (inside the
# WARDYN_PROXY_CONFIG_JSON payload). This is exactly how a real sidecar
# obtains it; we reuse it to exercise run-token-gated internal endpoints.
TOKEN="$(docker inspect "wardyn-proxy-${RUN_ID}" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
         | sed -n 's/^WARDYN_PROXY_CONFIG_JSON=//p' \
         | python3 -c 'import sys,json;print(json.load(sys.stdin)["run_token"])' 2>/dev/null || true)"
[[ -n "${TOKEN}" ]] || note "run token not found in proxy config env"

# ── 3a/b. L0 structural egress + metadata block (LIVE agent) ───────────────--
log "(a) agent has NO default route"
ROUTE="$(docker exec "${AGENT}" ip route 2>&1)"
if echo "${ROUTE}" | grep -qw default; then bad "agent HAS a default route (L0 violated): ${ROUTE}";
else ok "no default route (only on-link internal subnet): ${ROUTE//$'\n'/ ; }"; fi

log "(b) metadata IP 169.254.169.254 unreachable from sandbox"
MD="$(docker exec "${AGENT}" curl -sS -o /dev/null -m 6 --connect-timeout 5 -w '%{http_code}' \
       http://169.254.169.254/latest/meta-data/ 2>&1)"; MRC=$?
if [[ ${MRC} -ne 0 ]]; then ok "metadata IP unreachable (curl rc=${MRC}, no route)";
else bad "metadata IP REACHABLE (http_code=${MD}) — invariant 3 violated"; fi

# ── 4. allow/deny/pending/metadata through the AUTO-LAUNCHED sidecar ───────--
# GAP-1 is closed: the driver delivers the run's full proxy config (incl. the
# egress policy from the run's RunPolicy) via WARDYN_PROXY_CONFIG_JSON, so the
# probes below go through the sidecar wardynd launched — the real shipped path.
# The run's policy (demo.json) has first_use_approval=true, so the unknown
# domain yields a PENDING decision + a raised egress_domain ApprovalRequest
# rather than a hard deny; the metadata IP is an unconditional builtin deny.
log "(c/d) probing allow/pending/metadata through the auto-launched sidecar"
ALLOW="$(docker exec "${AGENT}" curl -sS -o /dev/null -m 20 --connect-timeout 10 -w '%{http_code}' \
          -x http://wardyn-proxy:3128 https://github.com/ 2>&1)"
if [[ "${ALLOW}" =~ ^(200|301|302)$ ]]; then ok "(d) allowed github.com passed via proxy (http_code=${ALLOW})";
else bad "(d) allowed github.com did NOT pass (got '${ALLOW}')"; fi

DENY="$(docker exec "${AGENT}" curl -sS -m 12 --connect-timeout 8 \
         -x http://wardyn-proxy:3128 https://evil.example.com/ 2>&1)"
if echo "${DENY}" | grep -q '403'; then ok "(c) unknown evil.example.com HELD by proxy (403, first-use approval)";
else bad "(c) unknown domain was not 403-held: ${DENY}"; fi

log "(c) first-use approval raised for the unknown domain"
PENDING_APPROVAL="$(hc -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/api/v1/approvals?state=PENDING" \
  | python3 -c 'import sys,json;print(sum(1 for a in json.load(sys.stdin) if a["kind"]=="egress_domain" and "evil.example.com" in json.dumps(a.get("requested_scope",{}))))')"
if [[ "${PENDING_APPROVAL}" -ge 1 ]]; then ok "(c) egress_domain ApprovalRequest raised for evil.example.com";
else bad "(c) no PENDING egress_domain approval found for evil.example.com"; fi

MDP="$(docker exec "${AGENT}" curl -sS -o /dev/null -m 8 --connect-timeout 6 -w '%{http_code}' \
        -x http://wardyn-proxy:3128 http://169.254.169.254/ 2>&1)"
if [[ "${MDP}" == "403" ]]; then ok "(c) metadata IP BLOCKED by proxy (403, builtin deny)";
else bad "(c) metadata IP not 403-blocked via proxy (got '${MDP}')"; fi

log "(c) egress decisions surfaced as audit events via the API"
sleep 2  # async decision sink flush
AUDIT_JSON="$(hc -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/api/v1/audit?run_id=${RUN_ID}")"
EGRESS_DENY="$(printf '%s' "${AUDIT_JSON}" \
  | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if e["action"]=="egress.deny" and e["outcome"]=="denied"))')"
EGRESS_PENDING="$(printf '%s' "${AUDIT_JSON}" \
  | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if e["action"]=="egress.pending"))')"
EGRESS_ALLOW="$(printf '%s' "${AUDIT_JSON}" \
  | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if e["action"]=="egress.allow"))')"
if [[ "${EGRESS_DENY}" -ge 1 ]]; then ok "(c) ${EGRESS_DENY} egress.deny audit event(s) (metadata builtin) via API";
else bad "(c) expected >=1 egress.deny audit event, got ${EGRESS_DENY}"; fi
if [[ "${EGRESS_PENDING}" -ge 1 ]]; then ok "(c) ${EGRESS_PENDING} egress.pending audit event(s) (first-use hold) via API";
else bad "(c) expected >=1 egress.pending audit event, got ${EGRESS_PENDING}"; fi
if [[ "${EGRESS_ALLOW}" -ge 1 ]]; then ok "(d) ${EGRESS_ALLOW} egress.allow audit event(s) visible via API";
else bad "(d) expected >=1 egress.allow audit event, got ${EGRESS_ALLOW}"; fi

# ── 5. recording artifact round-trip ───────────────────────────────────────--
log "(f) recording artifact upload (run-token) + serve (admin)"
cat > "${WORKDIR}/e2e.cast" <<'CAST'
{"version": 2, "width": 80, "height": 24, "timestamp": 1781291592, "title": "wardyn-e2e"}
[0.1, "o", "wardyn e2e recording artifact\r\n"]
CAST
UP="$(hc -o /dev/null -w '%{http_code}' -X PUT "${BASE}/api/v1/internal/recordings/${RUN_ID}" \
       -H "Authorization: Bearer ${TOKEN}" --data-binary @"${WORKDIR}/e2e.cast")"
GET="$(hc -o /dev/null -w '%{http_code}' -H "Authorization: Bearer ${ADMIN_TOKEN}" \
        "${BASE}/api/v1/runs/${RUN_ID}/recording/${RUN_ID}")"
NOAUTH="$(hc -o /dev/null -w '%{http_code}' "${BASE}/api/v1/runs/${RUN_ID}/recording/${RUN_ID}")"
if [[ "${UP}" == "204" && "${GET}" == "200" && "${NOAUTH}" == "401" ]]; then
  ok "(f) recording store round-trip OK (upload=204 serve=200 noauth=401)"
else
  bad "(f) recording round-trip failed (upload=${UP} serve=${GET} noauth=${NOAUTH})"
fi
note "(f) this assertion exercises the endpoints directly; AUTO delivery via the"
note "(f)      brokered proxy upload route (no shared volume, cross-run 403) is"
note "(f)      exercised live by the real-agent assertion (i) below."

# ── 6. OIDC login flow against Dex ──────────────────────────────────────────--
log "(g) OIDC login flow against Dex -> session cookie authenticates GET /runs"
cat > "${WORKDIR}/oidc.sh" <<'SH'
set -u
JAR=/tmp/cj.txt; rm -f "$JAR"
BASE="http://wardynd:8080"; DEX="http://dex:5556"
C="curl -sS -c $JAR -b $JAR"
AU=$($C -D - -o /dev/null "$BASE/auth/login" | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')
echo "login_redirect_pkce_s256=$(echo "$AU" | grep -c 'code_challenge_method=S256') nonce=$(echo "$AU" | grep -c nonce) state=$(echo "$AU" | grep -c state)"
PAGE=$($C -L "$AU")
ACT=$(printf '%s' "$PAGE" | sed -n 's/.*<form[^>]*action="\([^"]*\)".*/\1/p' | head -1 | sed 's/&amp;/\&/g')
LOC=$($C -D - -o /dev/null --data-urlencode "login=demo@wardyn.local" --data-urlencode "password=password" "$DEX$ACT" | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')
CB=$(printf '%s' "$LOC" | sed 's#http://localhost:8080#http://wardynd:8080#')
$C -D /tmp/cb.txt -o /dev/null "$CB"
echo "session_cookie_set=$(grep -c wardyn_session $JAR)"
echo "runs_with_session=$($C -o /dev/null -w '%{http_code}' "$BASE/api/v1/runs")"
echo "runs_no_auth=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/api/v1/runs")"
SH
OIDC_OUT="$(ncis "${WORKDIR}/oidc.sh")"
echo "${OIDC_OUT}"
G_PKCE="$(echo "${OIDC_OUT}" | sed -n 's/.*login_redirect_pkce_s256=\([0-9]\).*/\1/p')"
G_COOKIE="$(echo "${OIDC_OUT}" | sed -n 's/^session_cookie_set=\([0-9]*\).*/\1/p')"
G_RUNS="$(echo "${OIDC_OUT}" | sed -n 's/^runs_with_session=\([0-9]*\).*/\1/p')"
G_NOAUTH="$(echo "${OIDC_OUT}" | sed -n 's/^runs_no_auth=\([0-9]*\).*/\1/p')"
if [[ "${G_PKCE}" == "1" && "${G_COOKIE}" -ge 1 && "${G_RUNS}" == "200" && "${G_NOAUTH}" == "401" ]]; then
  ok "(g) OIDC login completes; session cookie authenticates /runs (200), no-auth 401, PKCE S256"
else
  bad "(g) OIDC flow failed (pkce=${G_PKCE} cookie=${G_COOKIE} runs=${G_RUNS} noauth=${G_NOAUTH})"
fi

# ── 6b. REAL-AGENT run: full governed agent end-to-end ──────────────────────--
# Everything above proved the control-plane invariants against the synthetic
# e2e fixture (no agent process, no wardyn-rec). This section proves the REAL
# shipped path end to end against the claude-code agent image:
#
#   (i)   a governed run with a task -> live sandbox RUNNING + run.exec audited
#         + the recording cast AUTO-DELIVERED to the recording store (GAP-2 live
#         closure) with NO manual PUT. agent-run launches `claude -p <task>`;
#         with no api_key brokered it fails fast inside the sandbox, but the
#         recorder still captures + delivers the attempt.
#   (ii)  brokered git-credential chain, live: wardyn-git-helper get ->
#         credential ApprovalRequest -> approve -> the documented fail-closed
#         mint error ("no GitHubMinter configured"). Proves the FULL chain
#         sandbox -> proxy(token injected) -> control plane -> broker -> approval.
#   (iii) LLM route: curl $ANTHROPIC_BASE_URL/v1/messages with no api_key grant
#         -> the proxy's 404 no-brokered-credential JSON (chain + fail-closed).
#   (iv)  negative: an ABSOLUTE-URI request to the local-route path must NOT hit
#         the local route (it is forward-proxied + policy-denied, not minted).
log "REAL-AGENT: creating a governed run (agent=${CC_AGENT}, with a task)"
CC_TASK="print the word READY and exit"   # trivial; claude fails fast w/o a key
CC_CREATE="$("${COMPOSE[@]}" exec -T -e WARDYN_URL="${BASE}" -e WARDYN_ADMIN_TOKEN="${ADMIN_TOKEN}" \
  wardynd /usr/local/bin/wardyn run --agent "${CC_AGENT}" --repo octocat/Hello-World --task "${CC_TASK}")"
echo "${CC_CREATE}"
CC_RUN_ID="$(printf '%s\n' "${CC_CREATE}" | awk '/^created run/{print $3; exit}')"
[[ -n "${CC_RUN_ID}" ]] || die "could not parse claude-code run id"
CC_AGENT_CTR="wardyn-agent-${CC_RUN_ID}"

# (i) sandbox RUNNING. dispatch sets RUNNING before the fire-and-forget Exec, so
# even a fast-failing agent task leaves the run RUNNING (v0 has no completion
# watcher). The image resolved via WARDYN_AGENT_IMAGES must be the demo tag.
CC_STATE="$(hc -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/api/v1/runs/${CC_RUN_ID}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["state"])')"
if [[ "${CC_STATE}" == "RUNNING" ]]; then ok "(i) claude-code run dispatched to RUNNING (real agent sandbox up)";
else bad "(i) claude-code run state=${CC_STATE}, expected RUNNING"; fi

# (i) the run.exec audit event (the driver Exec'd /usr/local/bin/agent-run).
log "(i) run.exec audited for the real agent launch"
EXEC_AUDIT="$(hc -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/api/v1/audit?run_id=${CC_RUN_ID}" \
  | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if e["action"]=="run.exec"))')"
EXEC_OK="$(hc -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/api/v1/audit?run_id=${CC_RUN_ID}" \
  | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if e["action"]=="run.exec" and e["outcome"]=="success"))')"
if [[ "${EXEC_AUDIT}" -ge 1 ]]; then ok "(i) run.exec audit event present (${EXEC_AUDIT}; success=${EXEC_OK})";
else bad "(i) no run.exec audit event for the claude-code run"; fi

# (i) recording AUTO-DELIVERED: wardyn-rec wraps the agent argv, asciinema
# records the (fast-failing) session, and -out-dir delivers <run>.cast to the
# shared wardyn-recordings volume that wardynd's recording store reads. No
# manual PUT here — this is the live GAP-2 closure. Poll up to ~40s because the
# cast lands only after the agent process exits and wardyn-rec copies it.
log "(i) recording cast auto-delivered (no manual PUT) -> GET serves 200"
CC_REC=""
for _ in $(seq 1 20); do
  CC_REC="$(hc -o /dev/null -w '%{http_code}' -H "Authorization: Bearer ${ADMIN_TOKEN}" \
            "${BASE}/api/v1/runs/${CC_RUN_ID}/recording/${CC_RUN_ID}")"
  [[ "${CC_REC}" == "200" ]] && break
  sleep 2
done
if [[ "${CC_REC}" == "200" ]]; then
  CT="$(hc -D - -o /dev/null -H "Authorization: Bearer ${ADMIN_TOKEN}" \
        "${BASE}/api/v1/runs/${CC_RUN_ID}/recording/${CC_RUN_ID}" | tr -d '\r' | sed -n 's/^[Cc]ontent-[Tt]ype: //p')"
  ok "(i) recording AUTO-DELIVERED + served (200, Content-Type: ${CT:-<none>}) — GAP-2 closed live"
else
  bad "(i) recording not auto-delivered (GET=${CC_REC}); agent-run/wardyn-rec/-out-dir chain"
  note "(i) last cast dir listing on the agent: $(docker exec "${CC_AGENT_CTR}" ls -la /wardyn/recordings 2>&1 | tr '\n' ' ' || echo '<exec failed>')"
fi

# (ii) brokered git-credential chain, LIVE, from inside the real sandbox.
# wardyn-git-helper reads WARDYN_GITHUB_GRANT_ID + WARDYN_PROXY_URL from env and
# POSTs the local mint route; the demo policy's github_token grant has
# requires_approval=true, so the broker first returns 409 pending and raises a
# credential ApprovalRequest. The helper then polls. We approve out of band,
# the helper re-mints, and the broker fails closed (no GitHub App key in the
# demo). We run the helper with a SHORT approval timeout in the background so
# the script controls the approve step deterministically.
log "(ii) brokered git-credential chain: helper -> mint -> credential approval"
GH_OUT="${WORKDIR}/githelper.out"; GH_ERR="${WORKDIR}/githelper.err"
# Short timeout so the helper does not hang the suite if approval never lands.
docker exec -e WARDYN_APPROVAL_TIMEOUT=60s "${CC_AGENT_CTR}" sh -c \
  'printf "protocol=https\nhost=github.com\n\n" | /usr/local/bin/wardyn-git-helper get' \
  >"${GH_OUT}" 2>"${GH_ERR}" &
GH_PID=$!

# Poll for the credential ApprovalRequest the mint raised (kind=credential).
log "(ii) credential ApprovalRequest raised (kind=credential, PENDING)"
CRED_APID=""
for _ in $(seq 1 15); do
  CRED_APID="$(hc -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/api/v1/approvals?state=PENDING" \
    | python3 -c 'import sys,json
rid=sys.argv[1]
aps=json.load(sys.stdin)
print(next((a["id"] for a in aps if a.get("kind")=="credential" and a.get("run_id")==rid), ""))' "${CC_RUN_ID}")"
  [[ -n "${CRED_APID}" ]] && break
  sleep 2
done
if [[ -n "${CRED_APID}" ]]; then ok "(ii) credential ApprovalRequest raised (id=${CRED_APID})";
else bad "(ii) no PENDING credential approval surfaced for the run"; fi

# Approve it via the API (the broker's mint gate consumes APPROVED in the same
# tx that verifies scope; the helper's poll then re-mints).
if [[ -n "${CRED_APID}" ]]; then
  APP_CODE="$(hc -o /dev/null -w '%{http_code}' -X POST "${BASE}/api/v1/approvals/${CRED_APID}/approve" \
              -H "Authorization: Bearer ${ADMIN_TOKEN}")"
  if [[ "${APP_CODE}" == "200" || "${APP_CODE}" == "202" ]]; then ok "(ii) credential approval APPROVED via API (${APP_CODE})";
  else bad "(ii) approve call failed (${APP_CODE})"; fi
fi

# Wait for the helper to finish (it re-mints, broker fails closed). Bounded wait
# so a hang cannot wedge the suite.
GH_RC=0
for _ in $(seq 1 30); do kill -0 "${GH_PID}" 2>/dev/null || break; sleep 2; done
if kill -0 "${GH_PID}" 2>/dev/null; then
  note "(ii) git-helper still running after wait; killing"
  kill "${GH_PID}" >/dev/null 2>&1 || true
fi
wait "${GH_PID}" 2>/dev/null; GH_RC=$?
GH_STDOUT="$(cat "${GH_OUT}" 2>/dev/null || true)"
GH_STDERR="$(cat "${GH_ERR}" 2>/dev/null || true)"
echo "--- wardyn-git-helper rc=${GH_RC} ---"
echo "stdout: ${GH_STDOUT:-<empty>}"
echo "stderr: ${GH_STDERR}"
# Fail-closed proof: the helper must NOT have emitted a password (no token was
# minted — the demo has no GitHubMinter), and the documented fail-closed mint
# error must appear. The broker returns "no GitHubMinter configured (fail
# closed)"; the proxy passes the 500 verbatim and the helper surfaces it.
if echo "${GH_STDOUT}" | grep -q '^password='; then
  bad "(ii) git-helper emitted a token — fail-closed broken (a credential was minted with no GitHubMinter)"
elif echo "${GH_STDERR}" | grep -qiE 'no GitHubMinter configured|mint status 500|github_token grant but no'; then
  ok "(ii) full brokered chain proven; broker failed closed (no GitHubMinter) — no token leaked"
else
  bad "(ii) chain did not reach the documented fail-closed mint error: ${GH_STDERR}"
fi

# (iii) LLM route fail-closed: with no api_key grant configured the proxy's
# /wardyn/llm/anthropic/* route returns 404 + the no-brokered-credential JSON.
# Run from INSIDE the sandbox so the env wiring (ANTHROPIC_BASE_URL) is used.
log "(iii) LLM route returns the no-brokered-credential 404 (no api_key grant)"
LLM_BODY="$(docker exec "${CC_AGENT_CTR}" sh -c \
  'curl -sS -m 20 "${ANTHROPIC_BASE_URL}/v1/messages" -H "content-type: application/json" -d "{\"model\":\"x\",\"max_tokens\":1,\"messages\":[]}"' 2>&1 || true)"
LLM_CODE="$(docker exec "${CC_AGENT_CTR}" sh -c \
  'curl -sS -o /dev/null -m 20 -w "%{http_code}" "${ANTHROPIC_BASE_URL}/v1/messages" -H "content-type: application/json" -d "{\"model\":\"x\",\"max_tokens\":1,\"messages\":[]}"' 2>&1 || true)"
echo "llm http_code=${LLM_CODE} body=${LLM_BODY}"
if [[ "${LLM_CODE}" == "404" ]] && echo "${LLM_BODY}" | grep -q 'no_llm_credential'; then
  ok "(iii) LLM route fail-closed: 404 no_llm_credential JSON (proxy holds/injects, none brokered)"
else
  bad "(iii) LLM route did not fail closed (code=${LLM_CODE} body=${LLM_BODY})"
fi

# (iii, optional) Real LLM path. The full real path (real api_key secret + an
# api_key grant whose injection rule targets api.anthropic.com + a live claude
# task) requires provisioning the secret store and a custom policy at wardynd
# boot — neither is exposed by the running compose stack's REST surface (there
# is no secret-create or policy-create route; the demo policy brokers no
# api_key). So this validator does NOT auto-wire it; it is documented and left
# OPTIONAL per the assignment. When WARDYN_E2E_ANTHROPIC_KEY is set we record
# that the operator must supply a boot-time api_key grant to exercise it.
if [[ -n "${WARDYN_E2E_ANTHROPIC_KEY:-}" ]]; then
  note "(iii) WARDYN_E2E_ANTHROPIC_KEY is set: the real-LLM path needs a boot-time"
  note "(iii)   api_key grant (injection rule host=api.anthropic.com) + the secret in"
  note "(iii)   wardynd's secret store. This minimal validator does not provision"
  note "(iii)   those via the API (no such route); rerun the stack with a policy that"
  note "(iii)   brokers the key to exercise the live ANTHROPIC_BASE_URL passthrough."
fi

# (iv) negative: an ABSOLUTE-URI forward-proxy request MUST be treated as a
# forward-proxy request, NOT the origin-form local route — and critically the
# proxy MUST NOT inject the run token on this path (token injection happens ONLY
# on the origin-form local mint route). We target the control-plane host DIRECTLY
# (http://wardynd:8080/api/v1/internal/credentials/mint) through the proxy, so
# there is NO loopback to a local route. wardynd is NOT in the demo allowlist, so
# the forward-proxy path policy-DENIES it (403/CONNECT-style failure). The agent
# therefore cannot reach the internal mint endpoint with the run token via an
# absolute-URI — the run token is injected only on the gated origin-form route.
# A reached-internal-endpoint would 400 "grant_id is required" (token injected)
# or 401 (token missing); a policy deny proves neither: the request never left
# the proxy's allowlist gate.
log "(iv) negative: absolute-URI to the internal mint endpoint is policy-denied (token NOT injected)"
ABS_BODY="$(docker exec "${CC_AGENT_CTR}" sh -c \
  'curl -sS -m 15 -x http://wardyn-proxy:3128 \
     -X POST http://wardynd:8080/api/v1/internal/credentials/mint \
     -H "content-type: application/json" -d "{\"grant_id\":\"00000000-0000-0000-0000-000000000000\"}"' 2>&1 || true)"
echo "absolute-uri internal-mint attempt -> ${ABS_BODY}"
# A reached internal endpoint would return a JSON error from wardynd
# ("grant_id is required" / "missing run claims"); a mint result would carry
# token/jti/kind. The forward-proxy path must instead yield a policy deny (403)
# or a connection/tunnel failure — proving the absolute-URI never hit the local
# route and the run token was never injected onto it.
if echo "${ABS_BODY}" | grep -qE '"token"|"jti"|"kind"|"expires_at"'; then
  bad "(iv) absolute-URI request was MINTED (origin-form gating / no-inject broken): ${ABS_BODY}"
elif echo "${ABS_BODY}" | grep -qiE 'grant_id is required|missing run claims|invalid decision'; then
  bad "(iv) absolute-URI reached the internal mint endpoint (run token injected on forward path): ${ABS_BODY}"
elif echo "${ABS_BODY}" | grep -qiE 'approval_pending|first.?use'; then
  # The unknown control-plane host is HELD by the proxy's egress allowlist as a
  # first-use approval (demo policy first_use_approval=true) — the SAME hold as
  # an unknown domain (assertion c). It never reached the internal mint and no
  # run token was injected: the local route + token injection are origin-form
  # only, exactly as the fixed convention requires.
  ok "(iv) absolute-URI HELD by egress allowlist (first-use), never minted, run token NOT injected"
elif echo "${ABS_BODY}" | grep -qiE '403|denied|Forbidden|refused|not allowed|could not|failed|error'; then
  ok "(iv) absolute-URI policy-denied, never reached internal mint, run token NOT injected"
else
  # No recognized signal (no held/first-use, no policy-deny, no mint). Do NOT
  # auto-pass: an unrecognized output may hide a regression or a real bypass.
  # Fail loudly and record the raw output so a human inspects it.
  bad "(iv) absolute-URI produced an UNRECOGNIZED proxy response; cannot assert it was held/denied"
  note "(iv) raw proxy response: ${ABS_BODY}"
fi

# The brokered local-route + LLM decisions must surface in audit (DecisionLog ->
# decisions pipeline -> audit), proving they flow through the same fanout.
log "(ii/iii) brokered DecisionLogs surfaced in audit (mint/approvals/llm)"
# The proxy decision sink posts each record async; poll briefly for both to land.
BROK_MINT=0; BROK_LLM=0
for _ in $(seq 1 8); do
  CC_AUDIT="$(hc -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/api/v1/audit?run_id=${CC_RUN_ID}")"
  BROK_MINT="$(printf '%s' "${CC_AUDIT}" | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if "brokered:mint" in json.dumps(e)))')"
  BROK_LLM="$(printf '%s' "${CC_AUDIT}" | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if "brokered:llm" in json.dumps(e)))')"
  [[ "${BROK_MINT}" -ge 1 && "${BROK_LLM}" -ge 1 ]] && break
  sleep 2
done
if [[ "${BROK_MINT}" -ge 1 ]]; then ok "(ii) brokered:mint DecisionLog(s) in audit (${BROK_MINT})";
else bad "(ii) no brokered:mint decision audit event for the run"; fi
if [[ "${BROK_LLM}" -ge 1 ]]; then ok "(iii) brokered:llm DecisionLog(s) in audit (${BROK_LLM})";
else bad "(iii) no brokered:llm decision audit event for the run"; fi

# ── 7. kill cascade ─────────────────────────────────────────────────────────--
log "(e) kill cascade"
# Remove our manually-launched proxy first so the per-run network has no active
# endpoint blocking its teardown (otherwise the runner logs a benign net-remove
# error; the agent + sidecar are still torn down and identity is still revoked).
docker rm -f "${PROXY_NAME}" >/dev/null 2>&1 || true
PROXY_NAME=""
PRE="$(hc -o /dev/null -w '%{http_code}' -X POST "${BASE}/api/v1/internal/decisions" \
        -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
        -d '{"request":{"host":"prekill.example","port":443,"method":"CONNECT"},"decision":"deny","rule_source":"e2e:prekill"}')"
KILL="$(hc -o /dev/null -w '%{http_code}' -X POST "${BASE}/api/v1/runs/${RUN_ID}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}")"
sleep 2
GONE="$(docker ps -a --filter "name=${AGENT}" --format '{{.Names}}')"
POST="$(hc -o /dev/null -w '%{http_code}' -X POST "${BASE}/api/v1/internal/decisions" \
         -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
         -d '{"request":{"host":"postkill.example","port":443,"method":"CONNECT"},"decision":"deny","rule_source":"e2e:postkill"}')"
KSTATE="$(hc -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/api/v1/runs/${RUN_ID}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["state"])')"
KILL_AUDIT="$(hc -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE}/api/v1/audit?run_id=${RUN_ID}" \
  | python3 -c 'import sys,json;print(sum(1 for e in json.load(sys.stdin) if e["action"]=="run.kill" and e["actor_type"]=="human"))')"
echo "pre-kill token=${PRE} kill=${KILL} agent_gone=$([[ -z "${GONE}" ]] && echo yes || echo no) post-kill token=${POST} state=${KSTATE} run.kill_audit=${KILL_AUDIT}"
if [[ "${PRE}" == "202" && "${KILL}" == "202" && -z "${GONE}" && "${POST}" == "401" && "${KSTATE}" == "KILLED" && "${KILL_AUDIT}" -ge 1 ]]; then
  ok "(e) kill cascade: container gone + run token revoked (401) + state KILLED + run.kill audit"
else
  bad "(e) kill cascade incomplete (pre=${PRE} kill=${KILL} gone='${GONE}' post=${POST} state=${KSTATE} audit=${KILL_AUDIT})"
fi

# ── summary ─────────────────────────────────────────────────────────────────--
echo
log "e2e summary: ${pass} passed, ${fail} failed"
[[ ${fail} -eq 0 ]] || exit 1
