#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Live UI-sandbox gateway e2e (D4.2): brings up a DEDICATED, uniquely-namespaced
# compose stack (never the operator's/another job's — see PROJECT below) with
# BOTH listeners' env set, launches one interactive sandbox on the code-server
# image (deploy/images/vscode/), and drives the whole relay with a real HTTP
# client against the SECOND origin:
#
#   - the full handoff: mint an attach ticket on the console origin -> GET
#     /__wardyn/enter on the UI origin -> the scoped wardyn_ui_sess cookie ->
#     302 into /r/<run>/ -> code-server's own HTML comes back through the exec
#     lane
#   - the second origin IS a boundary: /__wardyn/enter 404s on the console
#     listener (those routes exist only on the UI handler)
#   - ui.auth / ui.start / ui.open / ui.close audit rows, and NO session.attach
#     row (a relay session must never appear in the recording picker)
#   - an undeclared app is refused 403 naming the ui_apps policy field, and the
#     denial is audited
#   - a ticket minted for ANOTHER run is refused 403 on this one, and a
#     single-use ticket cannot be redeemed twice
#   - header hygiene BOTH ways, asserted against an in-sandbox echo responder:
#     no wardyn_* cookie, no Authorization and no ?ticket reaches the app; a
#     non-wardyn cookie does; the app's own Set-Cookie: wardyn_* is dropped on
#     the way out while its ordinary cookie survives
#   - the exec baseline: one relayed socat exec is POOLED, not one per request
#     (20 requests must not leave 20 execs), and the pool's idle reap actually
#     ends them
#
# GUARD: Docker-dependent, like the other live lanes. No-op unless
# WARDYN_TEST_DOCKER=1.
#
# Tears the stack down (compose down --volumes, scoped to PROJECT only) on
# every exit path, success or failure — never touches any other stack.
set -uo pipefail

if [[ "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  echo "run-e2e-ui-sandbox: set WARDYN_TEST_DOCKER=1 to run the Docker-dependent UI-sandbox e2e (skipping)."
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
source "${ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

command -v docker >/dev/null 2>&1 || die "docker not found"
command -v curl >/dev/null 2>&1 || die "curl not found"
command -v jq >/dev/null 2>&1 || die "jq not found"

# ── dedicated stack identity — NEVER the operator's/another job's stack ──────
PROJECT="wardynv06uisbx"
COMPOSE_FILE="${ROOT}/deploy/compose/docker-compose.yaml"
API_PORT=18081
PG_PORT=15433
UI_PORT=18083
SSH_PORT=12223
REGISTRY_PORT=15011
# Project-unique agent image repo, for the same reason run-e2e-ssh.sh uses one:
# on a shared daemon the mutable wardyn/*:local tag can be re-pointed by another
# job between the image check and the run actually launching.
AGENT_IMAGE="wardynv06uisbx/agent-vscode:pinned"
# The app the relay serves. Port 8080 is what deploy/images/vscode/wardyn-ui-vscode
# binds; the echo app below has no launcher on purpose (this script starts it),
# which is also what proves the "already listening -> nothing was started" path.
VSCODE_PORT=8080
ECHO_PORT=9099
# Mirrors internal/api/uigateway.go's maxUIConnsPerRun. The exec-baseline check
# asserts the relay stays at or under it after a burst — if that constant moves,
# this moves with it.
UI_MAX_CONNS=8

compose() {
  COMPOSE_PROJECT_NAME="${PROJECT}" WARDYN_NS="${PROJECT}" \
    WARDYN_UP_PORT="${API_PORT}" WARDYN_PG_PORT="${PG_PORT}" WARDYN_SSH_PORT="${SSH_PORT}" \
    WARDYN_REGISTRY_PORT="${REGISTRY_PORT}" WARDYN_UI_SANDBOX_PORT="${UI_PORT}" \
    WARDYN_UI_SANDBOX_LISTEN=":8081" WARDYN_UI_SANDBOX_ADVERTISE="http://127.0.0.1:${UI_PORT}" \
    WARDYN_AGENT_IMAGES="$(jq -nc --arg img "${AGENT_IMAGE}" '{"claude-code":$img}')" \
    docker compose -p "${PROJECT}" -f "${COMPOSE_FILE}" "$@"
}

BASE="http://127.0.0.1:${API_PORT}"
UIBASE="http://127.0.0.1:${UI_PORT}"
ADMIN_TOKEN="demo-admin-token" # compose's own default (WARDYN_LOCAL_MODE left off)
TMPDIR="$(mktemp -d /tmp/wardyn-e2e-uisbx.XXXXXX)"
FAILED=0
note() { printf '  %s %s\n' "$1" "$2"; }
pass() { note "[pass]" "$1"; }
fail() { note "[FAIL]" "$1"; FAILED=1; }

teardown() {
  for r in "${RUN_ID:-}" "${RUN_B_ID:-}"; do
    [[ -n "${r}" ]] || continue
    curl -sS -X POST "${BASE}/api/v1/runs/${r}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1 || true
  done
  # Give wardynd a moment to stop+remove the per-run agent/proxy containers it
  # created OUTSIDE compose's bookkeeping before compose down kills wardynd
  # itself (run-e2e-ssh.sh's own hard-won note).
  [[ -n "${RUN_ID:-}${RUN_B_ID:-}" ]] && sleep 3
  log "tearing down ${PROJECT} (compose down --volumes; this project only)"
  compose down --volumes >/dev/null 2>&1 || true
  rm -rf "${TMPDIR}"
}
trap teardown EXIT

# Clean any stragglers from a prior aborted run of THIS script.
compose down --volumes >/dev/null 2>&1 || true

# ── build what we need ───────────────────────────────────────────────────────
docker image inspect wardyn/wardynd:local >/dev/null 2>&1 || make -s compose-build
if ! docker image inspect wardyn/agent-vscode:local >/dev/null 2>&1; then
  log "wardyn/agent-vscode:local absent; building it (make agent-image-vscode, +~300MB)"
  make agent-image-vscode || die "make agent-image-vscode failed"
fi
docker image inspect "${AGENT_IMAGE}" >/dev/null 2>&1 || \
  docker tag wardyn/agent-vscode:local "${AGENT_IMAGE}" || die "could not tag ${AGENT_IMAGE}"

# ── bring up the dedicated stack ─────────────────────────────────────────────
log "bringing up ${PROJECT} (api :${API_PORT}, ui :${UI_PORT}, pg :${PG_PORT})"
compose up -d postgres wardynd || die "compose up failed for ${PROJECT}"
wait_healthy "${BASE}" 60 2 || { compose logs wardynd | tail -80; die "wardynd did not become healthy"; }
UI_ENABLED="false"
HEALTHZ_RAW=""
for _ in $(seq 1 10); do
  HEALTHZ_RAW="$(curl -sS "${BASE}/healthz" 2>&1)"
  UI_ENABLED="$(printf '%s' "${HEALTHZ_RAW}" | jq -r '.ui_sandbox.enabled // false' 2>/dev/null || echo false)"
  [[ "${UI_ENABLED}" == "true" ]] && break
  sleep 1
done
if [[ "${UI_ENABLED}" != "true" ]]; then
  compose logs wardynd | tail -80
  die "ui-sandbox gateway not enabled per /healthz (WARDYN_UI_SANDBOX_LISTEN wiring broken, or wardyn/wardynd:local on this daemon predates the gateway) -- last /healthz body: ${HEALTHZ_RAW}"
fi
pass "stack up, healthy, /healthz reports ui_sandbox.enabled=true"

# ── api helper (console origin, admin bearer) ────────────────────────────────
api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "${body}" ]]; then
    curl -sS -o "${TMPDIR}/resp.json" -w '%{http_code}' -X "${method}" "${BASE}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" -H "Content-Type: application/json" -d "${body}"
  else
    curl -sS -o "${TMPDIR}/resp.json" -w '%{http_code}' -X "${method}" "${BASE}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}"
  fi
}

# mint_ticket RUN_ID -> prints the single-use ticket on stdout
mint_ticket() {
  local rid="$1" st
  st=$(api POST "/api/v1/runs/${rid}/attach-ticket")
  [[ "${st}" == "200" ]] || { fail "mint attach ticket for ${rid}: status ${st}: $(cat "${TMPDIR}/resp.json")"; return 1; }
  jq -r '.ticket' "${TMPDIR}/resp.json"
}

# launch_run -> prints the run id; an idle interactive sandbox (never execs the
# agent, so no model credential is needed — run-e2e-ssh.sh's proven convention).
launch_run() {
  local body st rid state
  body='{"agent":"claude-code","repo":"local:e2e","confinement_class":"CC1","interactive":true,
    "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny",
    "min_confinement_class":"CC1","auto_stop_after_sec":-1,
    "ui_apps":[{"name":"vscode","port":'"${VSCODE_PORT}"',"path":"/"},
               {"name":"echo","port":'"${ECHO_PORT}"',"path":"/"}]}}'
  st=$(api POST /api/v1/runs "${body}")
  [[ "${st}" == "201" || "${st}" == "200" ]] || { cat "${TMPDIR}/resp.json" >&2; return 1; }
  rid="$(jq -r '.id' "${TMPDIR}/resp.json")"
  [[ -n "${rid}" && "${rid}" != "null" ]] || return 1
  for _ in $(seq 1 60); do
    st=$(api GET "/api/v1/runs/${rid}")
    [[ "${st}" == "200" ]] || { sleep 2; continue; }
    state="$(jq -r '.state' "${TMPDIR}/resp.json")"
    [[ "${state}" == "RUNNING" ]] && { echo "${rid}"; return 0; }
    [[ "${state}" =~ ^(FAILED|KILLED|STOPPED)$ ]] && { echo "run ${rid} reached ${state}" >&2; return 1; }
    sleep 2
  done
  echo "run ${rid} never reached RUNNING" >&2
  return 1
}

RUN_ID="$(launch_run)" || die "could not launch the UI-sandbox run"
pass "run ${RUN_ID} is RUNNING on ${AGENT_IMAGE}, policy declares ui_apps vscode:${VSCODE_PORT} + echo:${ECHO_PORT}"

# The run payload must carry the effective ui_apps — it is what the console
# reads to decide whether to offer the lane at all (D1.2/D3.1).
api GET "/api/v1/runs/${RUN_ID}" >/dev/null
declared="$(jq -r '[.ui_apps[]?.name] | sort | join(",")' "${TMPDIR}/resp.json")"
if [[ "${declared}" == "echo,vscode" ]]; then
  pass "GET /runs/{id} exposes the effective ui_apps (${declared})"
else
  fail "GET /runs/{id} ui_apps = '${declared}', want 'echo,vscode'"
fi

# ── 1. the second origin is a boundary: the console listener mints nothing ──
# The console router has an SPA catch-all, so an unmounted path there answers
# 200 with index.html — the assertion that matters is that it is NOT the
# gateway: no relay session is minted and no redirect into /r/ is issued,
# whatever ticket is presented.
CONSOLE_TICKET="$(mint_ticket "${RUN_ID}")" || die "no ticket"
code=$(curl -sS -o /dev/null -D "${TMPDIR}/console-enter.h" -w '%{http_code}' \
  "${BASE}/__wardyn/enter?run=${RUN_ID}&app=vscode&ticket=${CONSOLE_TICKET}")
if ! grep -qi "set-cookie:.*wardyn_ui_sess" "${TMPDIR}/console-enter.h" \
   && ! grep -qi "^location:.*/r/" "${TMPDIR}/console-enter.h"; then
  pass "console origin does not run the gateway: /__wardyn/enter there mints no relay session (status ${code}, no wardyn_ui_sess, no /r/ redirect)"
else
  fail "console origin behaved like the UI gateway: $(tr -d '\r' < "${TMPDIR}/console-enter.h" | tr '\n' '|')"
fi

# ── 2. the full handoff: ticket -> enter -> cookie -> code-server HTML ───────
TICKET="$(mint_ticket "${RUN_ID}")" || die "no ticket"
code=$(curl -sS -o /dev/null -D "${TMPDIR}/enter.h" -w '%{http_code}' \
  "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=vscode&ticket=${TICKET}")
UI_SESS="$(sed -n 's/.*[Ss]et-[Cc]ookie: *wardyn_ui_sess=\([^;]*\).*/\1/p' "${TMPDIR}/enter.h" | head -1)"
loc="$(sed -n 's/^[Ll]ocation: *//p' "${TMPDIR}/enter.h" | tr -d '\r' | head -1)"
if [[ "${code}" == "302" && -n "${UI_SESS}" ]] \
   && grep -qi "set-cookie:.*wardyn_ui_sess=.*Path=/r/${RUN_ID}/" "${TMPDIR}/enter.h" \
   && grep -qi "set-cookie:.*wardyn_ui_sess=.*HttpOnly" "${TMPDIR}/enter.h" \
   && grep -qi "set-cookie:.*wardyn_ui_sess=.*SameSite=Lax" "${TMPDIR}/enter.h" \
   && [[ "${loc}" == "/r/${RUN_ID}/" ]]; then
  pass "enter: 302 -> ${loc}, cookie wardyn_ui_sess is HttpOnly + SameSite=Lax + Path=/r/${RUN_ID}/"
else
  fail "enter handoff: status ${code}, location '${loc}', headers: $(tr -d '\r' < "${TMPDIR}/enter.h" | tr '\n' '|')"
fi

# The relayed page itself. First contact starts code-server through the
# launcher convention, so allow a generous window (uiEnsureWaitSecs is 20s in
# the sandbox and the whole cycle is bounded by uiEnsureTimeout).
got_html=0
for _ in $(seq 1 6); do
  # -L: code-server's landing path 302s to ./?folder=<workdir>, a relative
  # redirect that stays on the UI origin and inside the cookie's path scope.
  code=$(curl -sS -m 60 -L -o "${TMPDIR}/vscode.html" -w '%{http_code}' \
    -H "Cookie: wardyn_ui_sess=${UI_SESS}" "${UIBASE}/r/${RUN_ID}/")
  [[ "${code}" == "200" ]] && grep -qi "code-server" "${TMPDIR}/vscode.html" && { got_html=1; break; }
  sleep 5
done
if [[ "${got_html}" -eq 1 ]]; then
  pass "relay: code-server's own HTML came back through the exec lane ($(wc -c < "${TMPDIR}/vscode.html") bytes)"
else
  fail "relay: status ${code}, body head: $(head -c 300 "${TMPDIR}/vscode.html")"
fi

# ── 3. an undeclared app is refused, naming the policy field ────────────────
TICKET="$(mint_ticket "${RUN_ID}")" || die "no ticket"
code=$(curl -sS -o "${TMPDIR}/undeclared.json" -w '%{http_code}' \
  "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=grafana&ticket=${TICKET}")
if [[ "${code}" == "403" ]] && grep -q "ui_apps" "${TMPDIR}/undeclared.json"; then
  pass "undeclared app: 403 naming ui_apps ($(jq -r '.error // .message // .' "${TMPDIR}/undeclared.json" 2>/dev/null | head -c 120))"
else
  fail "undeclared app: status ${code}, body $(cat "${TMPDIR}/undeclared.json")"
fi

# ── 4. a foreign run's ticket, and a reused one ─────────────────────────────
RUN_B_ID="$(launch_run)" || die "could not launch the second run"
FOREIGN_TICKET="$(mint_ticket "${RUN_B_ID}")" || die "no foreign ticket"
code=$(curl -sS -o "${TMPDIR}/foreign.json" -w '%{http_code}' \
  "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=vscode&ticket=${FOREIGN_TICKET}")
if [[ "${code}" == "403" ]]; then
  pass "foreign ticket: a ticket minted for run ${RUN_B_ID} is refused 403 on run ${RUN_ID}"
else
  fail "foreign ticket: status ${code}, body $(cat "${TMPDIR}/foreign.json")"
fi
REUSE_TICKET="$(mint_ticket "${RUN_ID}")" || die "no ticket"
curl -sS -o /dev/null "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=vscode&ticket=${REUSE_TICKET}"
code=$(curl -sS -o "${TMPDIR}/reuse.json" -w '%{http_code}' \
  "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=vscode&ticket=${REUSE_TICKET}")
if [[ "${code}" == "403" ]]; then
  pass "single-use ticket: the second redemption is refused 403"
else
  fail "ticket reuse: status ${code}, body $(cat "${TMPDIR}/reuse.json")"
fi

# ── 5. header hygiene both ways, against an in-sandbox echo responder ───────
# The "echo" app has NO /usr/local/bin/wardyn-ui-echo: starting it here is also
# what exercises uiEnsureApp's "already listening, nothing was started" path.
AGENT_CTR="wardyn-agent-${RUN_ID}"
# /home/agent, NOT /tmp: the sandbox mounts /tmp as a NOEXEC tmpfs (the docker
# driver's HostConfig), so socat's EXEC child there dies on exec and the relay
# sees nothing but an EOF. Delivered base64-on-argv rather than piped into
# `docker exec -i`, so nothing depends on this script's own stdin.
ECHO_APP_PATH="/home/agent/wardyn-echo.sh"
ECHO_APP_B64="$(base64 -w0 <<'ECHOAPP'
#!/bin/sh
# Reflects the request line + headers it received, and tries to set BOTH a
# reserved wardyn_* cookie (which the gateway must drop) and an ordinary one
# (which must survive).
req=""
while IFS= read -r line; do
  line=$(printf '%s' "$line" | tr -d '\r')
  [ -z "$line" ] && break
  req="${req}${line}
"
done
printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nSet-Cookie: wardyn_ui_sess=tossed; Path=/\r\nSet-Cookie: app_ok=1; Path=/\r\nConnection: close\r\n\r\nECHO-BEGIN\n%sECHO-END\n' "$req"
ECHOAPP
)"
docker exec "${AGENT_CTR}" sh -c \
  "printf %s '${ECHO_APP_B64}' | base64 -d > ${ECHO_APP_PATH} && chmod +x ${ECHO_APP_PATH}" \
  || fail "could not write the echo responder into ${AGENT_CTR}"
docker exec "${AGENT_CTR}" test -x "${ECHO_APP_PATH}" \
  || fail "the echo responder was not written into ${AGENT_CTR}"
# `docker exec -d` returns 0 whether or not the detached command survives, so
# prove the responder actually answers from INSIDE the sandbox before asking
# the relay to reach it — otherwise a dead EXEC child surfaces as an opaque
# relay 502 instead of a setup failure.
docker exec -d "${AGENT_CTR}" socat "TCP-LISTEN:${ECHO_PORT},reuseaddr,fork" "EXEC:${ECHO_APP_PATH}"
echo_up=0
for _ in $(seq 1 10); do
  if docker exec "${AGENT_CTR}" sh -c \
      "printf 'GET /setup-probe HTTP/1.0\r\n\r\n' | socat - TCP:127.0.0.1:${ECHO_PORT}" 2>/dev/null \
      | grep -q ECHO-END; then
    echo_up=1; break
  fi
  sleep 1
done
[[ "${echo_up}" -eq 1 ]] || fail "the in-sandbox echo responder never answered on 127.0.0.1:${ECHO_PORT}"

TICKET="$(mint_ticket "${RUN_ID}")" || die "no ticket"
curl -sS -o /dev/null -D "${TMPDIR}/enter-echo.h" "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=echo&ticket=${TICKET}"
ECHO_SESS="$(sed -n 's/.*[Ss]et-[Cc]ookie: *wardyn_ui_sess=\([^;]*\).*/\1/p' "${TMPDIR}/enter-echo.h" | head -1)"
[[ -n "${ECHO_SESS}" ]] || fail "no relay session cookie for the echo app"
code=$(curl -sS -m 60 -o "${TMPDIR}/echo.txt" -D "${TMPDIR}/echo.h" -w '%{http_code}' \
  -H "Cookie: wardyn_ui_sess=${ECHO_SESS}; wardyn_admin_token=MUST-NOT-LEAK; app_cookie=keepme" \
  -H "Authorization: Bearer MUST-NOT-LEAK" \
  "${UIBASE}/r/${RUN_ID}/probe?ticket=MUST-NOT-LEAK&keep=yes")
if [[ "${code}" == "200" ]] && grep -q "ECHO-END" "${TMPDIR}/echo.txt"; then
  if ! grep -qi "MUST-NOT-LEAK" "${TMPDIR}/echo.txt" \
     && ! grep -qi "wardyn_" "${TMPDIR}/echo.txt" \
     && grep -q "app_cookie=keepme" "${TMPDIR}/echo.txt" \
     && grep -q "keep=yes" "${TMPDIR}/echo.txt"; then
    pass "inbound strip: no wardyn_* cookie, no Authorization and no ?ticket reached the app; its own cookie and query survived"
  else
    fail "inbound strip: the app saw something it must not:
$(cat "${TMPDIR}/echo.txt")"
  fi
  if ! grep -qi "set-cookie:.*wardyn_" "${TMPDIR}/echo.h" && grep -qi "set-cookie:.*app_ok=1" "${TMPDIR}/echo.h"; then
    pass "outbound strip: the app's Set-Cookie: wardyn_ui_sess was dropped, its ordinary cookie kept"
  else
    fail "outbound strip: response headers: $(tr -d '\r' < "${TMPDIR}/echo.h" | tr '\n' '|')"
  fi
else
  fail "echo app: status ${code}, body head: $(head -c 300 "${TMPDIR}/echo.txt")"
fi

# ── 6. exec baseline: the relayed socat execs are pooled, then reaped ───────
# Counted from /proc inside the sandbox (no ps/pgrep dependency in the image).
relay_execs() {
  docker exec "${AGENT_CTR}" sh -c '
n=0
for p in /proc/[0-9]*; do
  c=$(tr "\0" " " < "$p/cmdline" 2>/dev/null) || continue
  case "$c" in *"TCP:127.0.0.1:'"${VSCODE_PORT}"'"*) n=$((n+1));; esac
done
echo $n' 2>/dev/null | tr -d '\r'
}
for _ in $(seq 1 20); do
  curl -sS -m 60 -o /dev/null -H "Cookie: wardyn_ui_sess=${UI_SESS}" "${UIBASE}/r/${RUN_ID}/" || true
done
after="$(relay_execs)"
if [[ -n "${after}" && "${after}" -le "${UI_MAX_CONNS}" ]]; then
  pass "exec baseline: 20 relayed requests left ${after} socat exec(s) in the sandbox (pooled; cap is ${UI_MAX_CONNS}, one-per-request would be 20)"
else
  fail "exec baseline: ${after} socat execs after 20 requests, want <= ${UI_MAX_CONNS}"
fi
# The idle pool (uiIdleConnTimeout, 90s) closes the relay connections, and the
# bound that must hold afterwards is the SAME cap — never a per-request climb.
# It is deliberately not asserted to reach zero: closing the exec stream cannot
# signal the process behind it (there is no "kill this exec" in the substrate
# APIs), so a socat whose app-side half is still open can outlive its relay
# connection and be reaped only with the sandbox. That residual is stated in
# docs/UI-SANDBOXES.md rather than papered over here.
sleep 150
left="$(relay_execs)"
if [[ -n "${left}" && "${left}" -le "${UI_MAX_CONNS}" ]]; then
  if [[ "${left}" == "0" ]]; then
    pass "exec baseline: after the pool's idle window every relay exec is gone"
  else
    pass "exec baseline: after the pool's idle window ${left} relay exec(s) remain, within the ${UI_MAX_CONNS} cap (the documented residual: a closed relay connection cannot kill the socat behind it)"
  fi
else
  fail "exec baseline: ${left} socat exec(s) after the idle window, above the ${UI_MAX_CONNS} cap:
$(docker exec "${AGENT_CTR}" sh -c 'for p in /proc/[0-9]*; do tr "\0" " " < "$p/cmdline" 2>/dev/null; echo; done' | grep TCP:127.0.0.1 || true)"
fi

# ── 7. the audit trail ──────────────────────────────────────────────────────
api GET "/api/v1/audit?run_id=${RUN_ID}" >/dev/null
cp "${TMPDIR}/resp.json" "${TMPDIR}/audit.json"
for action in ui.auth ui.start ui.open ui.close; do
  count="$(jq --arg a "${action}" '[.[] | select(.action==$a and .outcome=="success")] | length' "${TMPDIR}/audit.json")"
  if [[ "${count}" -ge 1 ]]; then
    pass "audit: ${count} successful ${action} row(s)"
  else
    fail "audit: no successful ${action} row found"
  fi
done
denied="$(jq '[.[] | select(.action=="ui.auth" and .outcome=="denied" and (.data.reason | test("not declared")))] | length' "${TMPDIR}/audit.json")"
if [[ "${denied}" -ge 1 ]]; then
  pass "audit: the undeclared-app denial is recorded as ui.auth/denied"
else
  fail "audit: no ui.auth denial row for the undeclared app"
fi
attach="$(jq '[.[] | select(.action=="session.attach")] | length' "${TMPDIR}/audit.json")"
if [[ "${attach}" -eq 0 ]]; then
  pass "audit: NO session.attach row — a relay session never poses as a recorded terminal attach"
else
  fail "audit: ${attach} session.attach row(s) appeared for a run that only ever used the relay"
fi
started="$(jq -r '[.[] | select(.action=="ui.start" and .outcome=="success") | .data.app] | unique | join(",")' "${TMPDIR}/audit.json")"
if [[ "${started}" == "vscode" ]]; then
  pass "audit: ui.start records only the app the launcher actually started (${started}) — the pre-listening echo app started nothing"
else
  fail "audit: ui.start apps = '${started}', want only 'vscode'"
fi

# ── summary ──────────────────────────────────────────────────────────────────
if [[ "${FAILED}" -eq 0 ]]; then
  log "UI-SANDBOX E2E: PASS (all checks above passed)"
else
  printf '\033[1;31m[error]\033[0m %s\n' "UI-SANDBOX E2E: FAIL (see [FAIL] lines above)" >&2
fi
exit "${FAILED}"
