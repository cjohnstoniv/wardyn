#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Live UI-sandbox gateway e2e (D4.2): brings up a DEDICATED, uniquely-namespaced
# compose stack (never the operator's/another job's — see PROJECT below) with
# BOTH listeners' env set, launches one interactive sandbox on the code-server
# image (deploy/images/vscode/), and drives the whole relay with a real HTTP
# client against the SECOND origin:
#
#   - the full handoff: mint an attach ticket on the console origin -> bind it
#     on the UI origin (a ticket this client never bound is refused) -> GET
#     /__wardyn/enter on the UI origin -> the scoped wardyn_ui_sess cookie ->
#     302 into /r/<run>/<app>/ -> code-server's own HTML comes back through the exec
#     lane
#   - the second origin IS a boundary: /__wardyn/enter 404s on the console
#     listener (those routes exist only on the UI handler)
#   - ui.authorize / ui.start / ui.open / ui.close audit rows, and NO session.attach
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
# Then the stack is recreated in HOST mode (WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE,
# a run-<id>.localhost origin per run) and the governed-inbound proof runs on it:
#
#   - an inbound token calls a BACKGROUND run's inbound app and gets the app's
#     answer; X-Wardyn-Caller reaches the app, a client-sent X-Wardyn-* and the
#     bearer do not; a non-inbound app, a caller the app does not name and an
#     API-kind token are each refused (404 inbound_not_found, 401
#     inbound_credential_invalid); each call is an inbound.allow / inbound.deny row
#   - a named person who is not the owner opens the app with a caller ticket and
#     lands in it; a person nobody named gets the foreign-run 404
#   - the host-mode cross-site rule on a cookie session, Origin: null included
#   - the VS Code app opens in host mode: the editor page loads and its WebSocket
#     upgrade crosses the gateway
#   - past the per-caller bucket the gateway answers 429 inbound_rate_limited
#
# GUARD: Docker-dependent, like the other live lanes. No-op unless
# WARDYN_TEST_DOCKER=1.
#
# Tears the stack down (compose down --volumes, scoped to PROJECT only) on
# every exit path, success or failure — never touches any other stack.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
source "${ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/lib/runner-limits.sh
source "${ROOT}/scripts/lib/runner-limits.sh"

if [[ "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  skip_lane "run-e2e-ui-sandbox: set WARDYN_TEST_DOCKER=1 to run the Docker-dependent UI-sandbox e2e (skipping)."
fi
wardyn_pick_docker_host

command -v docker >/dev/null 2>&1 || die "docker not found"
command -v curl >/dev/null 2>&1 || die "curl not found"
command -v jq >/dev/null 2>&1 || die "jq not found"

# ── dedicated stack identity — NEVER the operator's/another job's stack ──────
PROJECT="wardynv06uisbx"
# Fixed project name and ports, and an unconditional `down --volumes` at the end: a second concurrent run would
# tear down the first. Hold a per-host lock for the whole run (fd 9 closes on exit).
if command -v flock >/dev/null 2>&1; then
  exec 9>"/tmp/wardyn-run-e2e-ui-sandbox.lock"
  flock -n 9 || die "another run-e2e-ui-sandbox.sh is already running on this host (project ${PROJECT}); wait for it"
fi
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
# Same reason, for the two images the stack itself runs: reusing wardyn/*:local
# meant this lane graded whatever another job on the daemon last built.
# docker-compose.yaml takes these overrides and defaults to the :local names,
# so no other caller changes.
WARDYND_IMAGE="wardyn/wardynd:${PROJECT}"
PROXY_IMAGE="wardyn/wardyn-proxy:${PROJECT}"
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
    WARDYN_WARDYND_IMAGE="${WARDYND_IMAGE}" WARDYN_PROXY_IMAGE="${PROXY_IMAGE}" \
    WARDYN_UI_SANDBOX_LISTEN=":8081" WARDYN_UI_SANDBOX_ADVERTISE="http://127.0.0.1:${UI_PORT}" \
    WARDYN_AGENT_IMAGES="$(jq -nc --arg img "${AGENT_IMAGE}" '{"claude-code":$img}')" \
    WARDYN_AGE_KEY="${AGE_KEY:-}" \
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
  for r in "${RUN_ID:-}" "${RUN_B_ID:-}" "${RUN_BG_ID:-}" "${RUN_HM_ID:-}"; do
    [[ -n "${r}" ]] || continue
    curl -sS -X POST "${BASE}/api/v1/runs/${r}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1 || true
  done
  # Give wardynd a moment to stop+remove the per-run agent/proxy containers it
  # created OUTSIDE compose's bookkeeping before compose down kills wardynd
  # itself (run-e2e-ssh.sh's own hard-won note).
  [[ -n "${RUN_ID:-}${RUN_B_ID:-}${RUN_BG_ID:-}${RUN_HM_ID:-}" ]] && sleep 3
  log "tearing down ${PROJECT} (compose down --volumes; this project only)"
  compose down --volumes >/dev/null 2>&1 || true
  rm -rf "${TMPDIR}"
}
trap teardown EXIT

# Clean any stragglers from a prior aborted run of THIS script.
compose down --volumes >/dev/null 2>&1 || true

# ── build what we need ───────────────────────────────────────────────────────
# wardynd/wardyn-proxy: always built, to this project's OWN tags — never the
# shared, mutable wardyn/*:local. The layer cache makes the rebuild near-free.
compose build wardynd >/dev/null || die "build ${WARDYND_IMAGE} failed"
compose --profile build-only build proxy-image >/dev/null || die "build ${PROXY_IMAGE} failed"
if ! docker image inspect wardyn/agent-vscode:local >/dev/null 2>&1; then
  log "wardyn/agent-vscode:local absent; building it (make agent-image-vscode, +~228 MiB)"
  make agent-image-vscode || die "make agent-image-vscode failed"
fi
# Re-tag EVERY run, not just when the tag is missing: the tag is a pointer, and
# a `make agent-image-vscode` rebuild moves :local to a new image id while the
# old :pinned tag keeps this stack on the code-server image from the first run
# that ever created it.
docker tag wardyn/agent-vscode:local "${AGENT_IMAGE}" || die "could not tag ${AGENT_IMAGE}"
# One age key for the stack's whole life: the host-mode walk recreates wardynd, and with a fresh ephemeral key it
# refuses to start over the secrets the path-mode walk sealed.
AGE_KEY="$(docker run --rm --pull=never --entrypoint /wardynd "${WARDYND_IMAGE}" -gen-age-key | grep -E '^AGE-SECRET-KEY-' | head -1)"
[[ -n "${AGE_KEY}" ]] || die "wardynd -gen-age-key produced no key"

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
  die "ui-sandbox gateway not enabled per /healthz (WARDYN_UI_SANDBOX_LISTEN wiring broken, or ${WARDYND_IMAGE} was built from a tree without it) -- last /healthz body: ${HEALTHZ_RAW}"
fi
pass "stack up, healthy, /healthz reports ui_sandbox.enabled=true"
# The stack's Docker runner takes no run until it has limits (scripts/lib/runner-limits.sh); this
# walk declares its own, sized for its few runs.
# shellcheck disable=SC2317 # called by wardyn_ensure_runner_limits
wardyn() { compose exec -T -e WARDYN_URL=http://localhost:8080 wardynd /usr/local/bin/wardyn "$@"; }
WARDYN_SETUP_RUNNER_LIMITS="${WARDYN_SETUP_RUNNER_LIMITS:---max-runs 4 --total-cpu 8 --total-memory 16 --total-disk 80 --per-run-cpu 2 --per-run-memory 4 --per-run-disk 20}" \
  wardyn_ensure_runner_limits </dev/null || die "could not give the ui-sandbox walk's runner its limits"

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
  st=$(api POST "/api/v1/runs/${rid}/attach/ticket")
  [[ "${st}" == "200" ]] || { fail "mint attach ticket for ${rid}: status ${st}: $(cat "${TMPDIR}/resp.json")"; return 1; }
  jq -r '.ticket' "${TMPDIR}/resp.json"
}

# bind_cookie TICKET -> prints the "name=value" binding cookie the gateway
# sets for TICKET, sending what the console's own pre-enter fetch sends. Enter
# refuses a ticket presented without it (#1241).
bind_cookie() {
  curl -sS -o /dev/null -D "${TMPDIR}/bind.h" -X POST "${UIBASE}/__wardyn/bind" \
    -H "Sec-Fetch-Site: same-site" -H "Origin: ${BASE}" --data-urlencode "ticket=$1"
  sed -n 's/^[Ss]et-[Cc]ookie: *\(wardyn_ui_bind_[^;]*\).*/\1/p' "${TMPDIR}/bind.h" | tr -d '\r' | head -1
}

# launch_json BODY -> prints the run id once the run is RUNNING.
launch_json() {
  local st rid state
  st=$(api POST /api/v1/runs "$1")
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

# launch_run -> prints the run id; an idle interactive sandbox (never execs the
# agent, so no model credential is needed — run-e2e-ssh.sh's proven convention).
launch_run() {
  launch_json '{"agent":"claude-code","repo":"local:e2e","confinement_class":"CC1","interactive":true,
    "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny",
    "min_confinement_class":"CC1","auto_stop_after_sec":-1,
    "ui_apps":[{"name":"vscode","port":'"${VSCODE_PORT}"',"path":"/"},
               {"name":"echo","port":'"${ECHO_PORT}"',"path":"/"}]}}'
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
code=$(curl -sS -o /dev/null -w '%{http_code}' \
  "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=vscode&ticket=${TICKET}")
if [[ "${code}" == "403" ]]; then
  pass "unbound ticket: enter refuses a ticket this client never bound (403), without spending it"
else
  fail "unbound ticket: status ${code}, want 403"
fi
code=$(curl -sS -o /dev/null -D "${TMPDIR}/enter.h" -w '%{http_code}' -H "Cookie: $(bind_cookie "${TICKET}")" \
  "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=vscode&ticket=${TICKET}")
UI_SESS="$(sed -n 's/.*[Ss]et-[Cc]ookie: *wardyn_ui_sess=\([^;]*\).*/\1/p' "${TMPDIR}/enter.h" | head -1)"
loc="$(sed -n 's/^[Ll]ocation: *//p' "${TMPDIR}/enter.h" | tr -d '\r' | head -1)"
if [[ "${code}" == "302" && -n "${UI_SESS}" ]] \
   && grep -qi "set-cookie:.*wardyn_ui_sess=.*Path=/r/${RUN_ID}/vscode/" "${TMPDIR}/enter.h" \
   && grep -qi "set-cookie:.*wardyn_ui_sess=.*HttpOnly" "${TMPDIR}/enter.h" \
   && grep -qi "set-cookie:.*wardyn_ui_sess=.*SameSite=Lax" "${TMPDIR}/enter.h" \
   && [[ "${loc}" == "/r/${RUN_ID}/vscode/" ]]; then
  pass "enter: 302 -> ${loc}, cookie wardyn_ui_sess is HttpOnly + SameSite=Lax + Path=/r/${RUN_ID}/vscode/"
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
    -H "Cookie: wardyn_ui_sess=${UI_SESS}" "${UIBASE}/r/${RUN_ID}/vscode/")
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
code=$(curl -sS -o "${TMPDIR}/undeclared.json" -w '%{http_code}' -H "Cookie: $(bind_cookie "${TICKET}")" \
  "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=grafana&ticket=${TICKET}")
if [[ "${code}" == "403" ]] && grep -q "ui_apps" "${TMPDIR}/undeclared.json"; then
  pass "undeclared app: 403 naming ui_apps ($(jq -r '.error // .message // .' "${TMPDIR}/undeclared.json" 2>/dev/null | head -c 120))"
else
  fail "undeclared app: status ${code}, body $(cat "${TMPDIR}/undeclared.json")"
fi

# ── 4. a foreign run's ticket, and a reused one ─────────────────────────────
RUN_B_ID="$(launch_run)" || die "could not launch the second run"
FOREIGN_TICKET="$(mint_ticket "${RUN_B_ID}")" || die "no foreign ticket"
code=$(curl -sS -o "${TMPDIR}/foreign.json" -w '%{http_code}' -H "Cookie: $(bind_cookie "${FOREIGN_TICKET}")" \
  "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=vscode&ticket=${FOREIGN_TICKET}")
if [[ "${code}" == "403" ]]; then
  pass "foreign ticket: a ticket minted for run ${RUN_B_ID} is refused 403 on run ${RUN_ID}"
else
  fail "foreign ticket: status ${code}, body $(cat "${TMPDIR}/foreign.json")"
fi
REUSE_TICKET="$(mint_ticket "${RUN_ID}")" || die "no ticket"
REUSE_BIND="$(bind_cookie "${REUSE_TICKET}")"
curl -sS -o /dev/null -H "Cookie: ${REUSE_BIND}" "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=vscode&ticket=${REUSE_TICKET}"
code=$(curl -sS -o "${TMPDIR}/reuse.json" -w '%{http_code}' -H "Cookie: ${REUSE_BIND}" \
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
      | grep ECHO-END >/dev/null; then
    echo_up=1; break
  fi
  sleep 1
done
[[ "${echo_up}" -eq 1 ]] || fail "the in-sandbox echo responder never answered on 127.0.0.1:${ECHO_PORT}"

TICKET="$(mint_ticket "${RUN_ID}")" || die "no ticket"
curl -sS -o /dev/null -D "${TMPDIR}/enter-echo.h" -H "Cookie: $(bind_cookie "${TICKET}")" "${UIBASE}/__wardyn/enter?run=${RUN_ID}&app=echo&ticket=${TICKET}"
ECHO_SESS="$(sed -n 's/.*[Ss]et-[Cc]ookie: *wardyn_ui_sess=\([^;]*\).*/\1/p' "${TMPDIR}/enter-echo.h" | head -1)"
[[ -n "${ECHO_SESS}" ]] || fail "no relay session cookie for the echo app"
code=$(curl -sS -m 60 -o "${TMPDIR}/echo.txt" -D "${TMPDIR}/echo.h" -w '%{http_code}' \
  -H "Cookie: wardyn_ui_sess=${ECHO_SESS}; wardyn_admin_token=MUST-NOT-LEAK; app_cookie=keepme" \
  -H "Authorization: Bearer MUST-NOT-LEAK" \
  "${UIBASE}/r/${RUN_ID}/echo/probe?ticket=MUST-NOT-LEAK&keep=yes")
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
  curl -sS -m 60 -o /dev/null -H "Cookie: wardyn_ui_sess=${UI_SESS}" "${UIBASE}/r/${RUN_ID}/vscode/" || true
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
for action in ui.authorize ui.start ui.open ui.close; do
  count="$(jq --arg a "${action}" '[.[] | select(.action==$a and .outcome=="success")] | length' "${TMPDIR}/audit.json")"
  if [[ "${count}" -ge 1 ]]; then
    pass "audit: ${count} successful ${action} row(s)"
  else
    fail "audit: no successful ${action} row found"
  fi
done
denied="$(jq '[.[] | select(.action=="ui.authorize" and .outcome=="denied" and (.data.reason | test("not declared")))] | length' "${TMPDIR}/audit.json")"
if [[ "${denied}" -ge 1 ]]; then
  pass "audit: the undeclared-app denial is recorded as ui.authorize/denied"
else
  fail "audit: no ui.authorize denial row for the undeclared app"
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

# ═══ governed inbound, in host mode ═════════════════════════════════════════
# Everything above ran on the shared origin. Inbound needs one origin per run for a person who is not the
# owner (the gateway refuses that person on a shared host), so the path-mode runs end here and wardynd is
# recreated with WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE. A *.localhost name resolves to loopback, so the walk
# needs no DNS.
kill_run() {
  local r="$1" state
  [[ -n "${r}" ]] || return 0
  curl -sS -o /dev/null -X POST "${BASE}/api/v1/runs/${r}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" || true
  for _ in $(seq 1 30); do
    api GET "/api/v1/runs/${r}" >/dev/null
    state="$(jq -r '.state // empty' "${TMPDIR}/resp.json" 2>/dev/null)"
    [[ -z "${state}" || "${state}" =~ ^(KILLED|STOPPED|FAILED|COMPLETED)$ ]] && return 0
    sleep 2
  done
  return 1
}
kill_run "${RUN_ID}" || fail "host mode: run ${RUN_ID} did not end before the restart"
kill_run "${RUN_B_ID}" || fail "host mode: run ${RUN_B_ID} did not end before the restart"

HOST_ORIGIN_TEMPLATE="http://run-{run}.localhost:${UI_PORT}"
HM_OVERRIDE="${TMPDIR}/host-mode.compose.yaml"
cat >"${HM_OVERRIDE}" <<YAML
services:
  wardynd:
    environment:
      WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE: "${HOST_ORIGIN_TEMPLATE}"
YAML
compose -f "${HM_OVERRIDE}" up -d --no-deps --force-recreate wardynd >/dev/null 2>&1 || die "could not recreate wardynd in host mode"
wait_healthy "${BASE}" 60 2 || { compose logs wardynd | tail -80; die "wardynd did not become healthy in host mode"; }
HOST_MODE="false"
INBOUND_TEMPLATE=""
for _ in $(seq 1 10); do
  curl -sS "${BASE}/healthz" >"${TMPDIR}/healthz-host.json" 2>/dev/null || true
  HOST_MODE="$(jq -r '.ui_sandbox.host_mode // false' "${TMPDIR}/healthz-host.json" 2>/dev/null || echo false)"
  [[ "${HOST_MODE}" == "true" ]] && break
  sleep 1
done
INBOUND_TEMPLATE="$(jq -r '.ui_sandbox.inbound_url_template // empty' "${TMPDIR}/healthz-host.json" 2>/dev/null)"
if [[ "${HOST_MODE}" == "true" && "${INBOUND_TEMPLATE}" == "${HOST_ORIGIN_TEMPLATE}/r/{run}/{app}/" ]]; then
  pass "host mode: /healthz reports host_mode=true and inbound_url_template ${INBOUND_TEMPLATE}"
else
  compose logs wardynd | tail -40
  die "host mode did not come up: host_mode=${HOST_MODE}, inbound_url_template='${INBOUND_TEMPLATE}'"
fi

# The people. The stack has one admin bearer and no second human, so the other principals are registered
# the way run-e2e-ssh.sh registers its second key: a row in this project's own Postgres, hashed the way
# GetAPITokenByRaw reads it. e2e-caller is named by the apps below; e2e-stranger is named by nothing. Each
# holds a console (api) token and an inbound token, the two kinds the gateway tells apart.
seed_token() { # PRINCIPAL KIND -> prints the raw token once its row is in
  local principal="$1" kind="$2" raw sha
  raw="wdn_$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
  sha="$(printf '%s' "${raw}" | sha256sum | cut -d' ' -f1)"
  compose exec -T postgres psql -U wardyn -d wardyn -v ON_ERROR_STOP=1 -c \
    "INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, kind, expires_at)
     VALUES (gen_random_uuid(), '${principal}', '${principal}@e2e.wardyn.invalid', 'user', 'standard', '[]', false, 'e2e ${kind}', '${sha}', '${kind}', now() + interval '30 days');" \
    >"${TMPDIR}/seed-token.log" 2>&1 || { echo "seed ${principal}/${kind}: $(cat "${TMPDIR}/seed-token.log")" >&2; return 1; }
  printf '%s' "${raw}"
}
CALLER_API="$(seed_token e2e-caller api)" || die "could not register e2e-caller"
CALLER_INBOUND="$(seed_token e2e-caller inbound)" || die "could not register e2e-caller"
STRANGER_API="$(seed_token e2e-stranger api)" || die "could not register e2e-stranger"
STRANGER_INBOUND="$(seed_token e2e-stranger inbound)" || die "could not register e2e-stranger"
code=$(curl -sS -o "${TMPDIR}/me.json" -w '%{http_code}' "${BASE}/api/v1/me" -H "Authorization: Bearer ${CALLER_API}")
if [[ "${code}" == "200" && "$(jq -r '.principal' "${TMPDIR}/me.json")" == "e2e-caller" ]]; then
  pass "setup: e2e-caller's console token signs in as e2e-caller"
else
  die "setup: e2e-caller's console token: status ${code}, body $(cat "${TMPDIR}/me.json")"
fi
code=$(curl -sS -o /dev/null -w '%{http_code}' "${BASE}/api/v1/me" -H "Authorization: Bearer ${CALLER_INBOUND}")
if [[ "${code}" == "401" ]]; then
  pass "an inbound token is refused on the console API (401)"
else
  fail "an inbound token on the console API: status ${code}, want 401"
fi

# Two runs. RUN_BG is a BACKGROUND command run (no terminal, no desktop): its only door is the inbound app
# `api`, answered by the echo responder the command starts. RUN_HM is an interactive run with the editor
# (a non-inbound app) beside an inbound `api` app, so "a non-inbound app on a run that has an inbound one"
# is a real case.
BG_CMD="mkdir -p /home/agent && printf %s '${ECHO_APP_B64}' | base64 -d > ${ECHO_APP_PATH} && chmod +x ${ECHO_APP_PATH} && exec socat TCP-LISTEN:${ECHO_PORT},reuseaddr,fork EXEC:${ECHO_APP_PATH}"
RUN_BG_ID="$(launch_json "$(jq -nc --arg cmd "${BG_CMD}" --argjson port "${ECHO_PORT}" \
  '{repo:"local:e2e", confinement_class:"CC1", experience:"background", workload:{kind:"command", command:$cmd},
    inline_policy:{allowed_domains:[], first_use_approval:"always_deny", min_confinement_class:"CC1",
      ui_apps:[{name:"api", port:$port, path:"/", inbound:true, callers:["user:e2e-caller"]}]}}')")" \
  || die "could not launch the background run with an inbound app"
RUN_HM_ID="$(launch_json "$(jq -nc --argjson port "${ECHO_PORT}" --argjson editor "${VSCODE_PORT}" \
  '{agent:"claude-code", repo:"local:e2e", confinement_class:"CC1", interactive:true,
    inline_policy:{allowed_domains:[], first_use_approval:"always_deny", min_confinement_class:"CC1", auto_stop_after_sec:-1,
      ui_apps:[{name:"vscode", port:$editor, path:"/"},
               {name:"api", port:$port, path:"/", inbound:true, callers:["user:e2e-caller"]}]}}')")" \
  || die "could not launch the interactive run with the editor"
pass "host mode: background run ${RUN_BG_ID} (inbound api) and interactive run ${RUN_HM_ID} (vscode + inbound api) are RUNNING"

# ── host-mode request helpers ───────────────────────────────────────────────
uihost() { printf 'run-%s.localhost:%s' "$1" "${UI_PORT}"; }
# hreq RUN OUT CURL_ARGS... -> one request to RUN's own origin; prints the status, writes OUT.body and OUT.h
hreq() {
  local run="$1" out="$2"
  shift 2
  curl -sS -m 60 --resolve "$(uihost "${run}"):127.0.0.1" -o "${out}.body" -D "${out}.h" -w '%{http_code}' "$@"
}
iurl() { printf 'http://%s/r/%s/%s/%s' "$(uihost "$1")" "$1" "$2" "${3:-}"; }
reason_of() { jq -r '.reason // empty' "$1" 2>/dev/null; }
# want_refusal LABEL GOT_STATUS WANT_STATUS WANT_REASON BODY_FILE
want_refusal() {
  local got_reason
  got_reason="$(reason_of "$5")"
  if [[ "$2" == "$3" && "${got_reason}" == "$4" ]]; then
    pass "$1: $3 $4"
  else
    fail "$1: status $2 reason '${got_reason}', want $3 $4 (body: $(head -c 200 "$5"))"
  fi
}
# open_in_host RUN APP TICKET -> binds the ticket and POSTs enter on the run's own origin, as the console's
# /open page does; sets OPEN_STATUS, OPEN_LOCATION and OPEN_SESS.
open_in_host() {
  local run="$1" app="$2" ticket="$3" bind
  hreq "${run}" "${TMPDIR}/hbind" -X POST -H "Sec-Fetch-Site: same-site" -H "Origin: ${BASE}" \
    --data-urlencode "ticket=${ticket}" "http://$(uihost "${run}")/__wardyn/bind" >/dev/null
  bind="$(sed -n 's/^[Ss]et-[Cc]ookie: *\(wardyn_ui_bind_[^;]*\).*/\1/p' "${TMPDIR}/hbind.h" | tr -d '\r' | head -1)"
  OPEN_STATUS="$(hreq "${run}" "${TMPDIR}/henter" -X POST -H "Cookie: ${bind}" \
    --data-urlencode "run=${run}" --data-urlencode "app=${app}" --data-urlencode "ticket=${ticket}" \
    "http://$(uihost "${run}")/__wardyn/enter")"
  OPEN_LOCATION="$(sed -n 's/^[Ll]ocation: *//p' "${TMPDIR}/henter.h" | tr -d '\r' | head -1)"
  OPEN_SESS="$(sed -n 's/.*[Ss]et-[Cc]ookie: *wardyn_ui_sess=\([^;]*\).*/\1/p' "${TMPDIR}/henter.h" | head -1)"
}
# app_ticket TOKEN RUN APP -> the status of POST /runs/{run}/apps/{app}/ticket as TOKEN's holder; the body is resp.json
app_ticket() {
  curl -sS -o "${TMPDIR}/resp.json" -w '%{http_code}' -X POST "${BASE}/api/v1/runs/$2/apps/$3/ticket" -H "Authorization: Bearer $1"
}
# The denial rows share auth.fail's bound (1/s, burst 5, process-wide): a pause before each request whose
# inbound.deny row is asserted keeps the bucket from eating it.
refill() { sleep 2; }

# ── token path ──────────────────────────────────────────────────────────────
# The first call waits out the command's start: the responder is not listening the instant the run is RUNNING.
# The client sends a forged X-Wardyn-Caller, an X-Wardyn-Admin and (it is the credential) a bearer; none of
# them may reach the app as sent.
code="000"
for _ in $(seq 1 12); do
  code=$(hreq "${RUN_BG_ID}" "${TMPDIR}/tok" -H "Authorization: Bearer ${CALLER_INBOUND}" \
    -H "X-Wardyn-Caller: spoofed-admin" -H "X-Wardyn-Admin: yes" "$(iurl "${RUN_BG_ID}" api "probe?secret=querysecret")")
  [[ "${code}" == "200" ]] && grep -q "ECHO-END" "${TMPDIR}/tok.body" && break
  sleep 5
done
if [[ "${code}" == "200" ]] && grep -q "ECHO-END" "${TMPDIR}/tok.body"; then
  pass "token path: an inbound token called the background run's inbound app and got the app's answer"
else
  fail "token path: status ${code}, body head: $(head -c 300 "${TMPDIR}/tok.body")"
fi
if [[ "$(grep -ic '^x-wardyn-caller:' "${TMPDIR}/tok.body")" == "1" ]] \
   && grep -qi '^x-wardyn-caller: e2e-caller' "${TMPDIR}/tok.body" \
   && grep -qi '^x-wardyn-caller-email: e2e-caller@e2e.wardyn.invalid' "${TMPDIR}/tok.body"; then
  pass "token path: X-Wardyn-Caller (and -Email) reached the app, set by the gateway"
else
  fail "token path: caller headers at the app: $(grep -i 'x-wardyn' "${TMPDIR}/tok.body" | tr '\n' '|')"
fi
if ! grep -qiE 'spoofed-admin|x-wardyn-admin|^authorization:' "${TMPDIR}/tok.body" && ! grep -q "${CALLER_INBOUND}" "${TMPDIR}/tok.body"; then
  pass "token path: a client-sent X-Wardyn-* header and the bearer did not reach the app"
else
  fail "token path: the app saw something it must not:
$(cat "${TMPDIR}/tok.body")"
fi

refill
code=$(hreq "${RUN_BG_ID}" "${TMPDIR}/apitok" -H "Authorization: Bearer ${CALLER_API}" "$(iurl "${RUN_BG_ID}" api probe)")
want_refusal "token path: an API-kind token" "${code}" 401 inbound_credential_invalid "${TMPDIR}/apitok.body"
if grep -qi '^www-authenticate: *Bearer' "${TMPDIR}/apitok.h"; then
  pass "token path: the 401 carries WWW-Authenticate: Bearer"
else
  fail "token path: no WWW-Authenticate on the 401: $(tr -d '\r' < "${TMPDIR}/apitok.h" | tr '\n' '|')"
fi
code=$(hreq "${RUN_BG_ID}" "${TMPDIR}/admintok" -H "Authorization: Bearer ${ADMIN_TOKEN}" "$(iurl "${RUN_BG_ID}" api probe)")
want_refusal "token path: the admin bearer is no credential here" "${code}" 403 ui_gateway_no_session "${TMPDIR}/admintok.body"
refill
code=$(hreq "${RUN_BG_ID}" "${TMPDIR}/stranger" -H "Authorization: Bearer ${STRANGER_INBOUND}" "$(iurl "${RUN_BG_ID}" api probe)")
want_refusal "token path: a live inbound token of someone the app does not name" "${code}" 404 inbound_not_found "${TMPDIR}/stranger.body"
refill
code=$(hreq "${RUN_HM_ID}" "${TMPDIR}/noninbound" -H "Authorization: Bearer ${CALLER_INBOUND}" "$(iurl "${RUN_HM_ID}" vscode)")
want_refusal "token path: a non-inbound app on a run that has an inbound one" "${code}" 404 inbound_not_found "${TMPDIR}/noninbound.body"
# A bearer is never ambient, so the browser rules that refuse a cookie session do not apply to it.
code=$(hreq "${RUN_BG_ID}" "${TMPDIR}/tok-null" -H "Authorization: Bearer ${CALLER_INBOUND}" -H "Origin: null" "$(iurl "${RUN_BG_ID}" api probe)")
if [[ "${code}" == "200" ]]; then
  pass "token path: Origin: null does not stop a bearer (the cross-site rule is for cookie sessions)"
else
  fail "token path with Origin: null: status ${code}, want 200"
fi

# ── people path ─────────────────────────────────────────────────────────────
# A named person who is not the owner: the console's /open page mints their ticket for the one app, binds it
# and enters, on the run's own host. This is that sequence, request for request.
code=$(app_ticket "${CALLER_API}" "${RUN_BG_ID}" api)
if [[ "${code}" == "200" ]]; then
  PEOPLE_TICKET="$(jq -r '.ticket' "${TMPDIR}/resp.json")"
  open_in_host "${RUN_BG_ID}" api "${PEOPLE_TICKET}"
  if [[ "${OPEN_STATUS}" == "303" && "${OPEN_LOCATION}" == "/r/${RUN_BG_ID}/api/" && -n "${OPEN_SESS}" ]]; then
    pass "people path: the named caller's ticket entered on the run's own host (303 -> ${OPEN_LOCATION})"
  else
    fail "people path: enter status ${OPEN_STATUS}, location '${OPEN_LOCATION}', headers: $(tr -d '\r' < "${TMPDIR}/henter.h" | tr '\n' '|')"
  fi
  PEOPLE_SESS="${OPEN_SESS}"
  code=$(hreq "${RUN_BG_ID}" "${TMPDIR}/landed" -H "Cookie: wardyn_ui_sess=${PEOPLE_SESS}" "http://$(uihost "${RUN_BG_ID}")${OPEN_LOCATION}who")
  if [[ "${code}" == "200" ]] && grep -qi '^x-wardyn-caller: e2e-caller' "${TMPDIR}/landed.body"; then
    pass "people path: the named caller landed in the app, as e2e-caller"
  else
    fail "people path: landing status ${code}, body head: $(head -c 300 "${TMPDIR}/landed.body")"
  fi
else
  fail "people path: the named caller was refused a ticket: status ${code}, $(cat "${TMPDIR}/resp.json")"
  PEOPLE_SESS=""
fi
refill
code=$(app_ticket "${STRANGER_API}" "${RUN_BG_ID}" api)
want_refusal "people path: a person nobody named" "${code}" 404 run_not_found "${TMPDIR}/resp.json"
code=$(app_ticket "${CALLER_API}" "${RUN_HM_ID}" vscode)
want_refusal "people path: a named caller asking for a non-inbound app" "${code}" 404 run_not_found "${TMPDIR}/resp.json"

# The host-mode cross-site rule on that cookie session (uiCrossSite). A request names its own origin or none,
# or it is a top-level GET navigation; a foreign Origin, "null" (a sandboxed frame's, a webview's) and any
# other cross-site request is refused 403 ui_gateway_cross_site before the app is reached.
if [[ -n "${PEOPLE_SESS}" ]]; then
  BG_ORIGIN="http://$(uihost "${RUN_BG_ID}")"
  SESS_COOKIE="Cookie: wardyn_ui_sess=${PEOPLE_SESS}"
  refill
  code=$(hreq "${RUN_BG_ID}" "${TMPDIR}/xs-null" -H "${SESS_COOKIE}" -H "Origin: null" "$(iurl "${RUN_BG_ID}" api probe)")
  want_refusal "cross-site rule: a cookie request with Origin: null" "${code}" 403 ui_gateway_cross_site "${TMPDIR}/xs-null.body"
  code=$(hreq "${RUN_BG_ID}" "${TMPDIR}/xs-fetch" -H "${SESS_COOKIE}" -H "Sec-Fetch-Site: cross-site" -H "Sec-Fetch-Mode: cors" "$(iurl "${RUN_BG_ID}" api probe)")
  want_refusal "cross-site rule: a cross-site fetch" "${code}" 403 ui_gateway_cross_site "${TMPDIR}/xs-fetch.body"
  code=$(hreq "${RUN_BG_ID}" "${TMPDIR}/xs-nav" -H "${SESS_COOKIE}" -H "Sec-Fetch-Site: cross-site" -H "Sec-Fetch-Mode: navigate" -H "Sec-Fetch-Dest: document" "$(iurl "${RUN_BG_ID}" api probe)")
  if [[ "${code}" == "200" ]]; then
    pass "cross-site rule: a cross-site top-level GET navigation (a link) is let through"
  else
    fail "cross-site rule: a cross-site navigation: status ${code}, want 200"
  fi
  code=$(hreq "${RUN_BG_ID}" "${TMPDIR}/xs-own" -H "${SESS_COOKIE}" -H "Origin: ${BG_ORIGIN}" -H "Sec-Fetch-Site: same-origin" "$(iurl "${RUN_BG_ID}" api probe)")
  if [[ "${code}" == "200" ]]; then
    pass "cross-site rule: a request naming the run's own origin is let through"
  else
    fail "cross-site rule: a same-origin request: status ${code}, want 200"
  fi
fi

# ── VS Code in host mode ────────────────────────────────────────────────────
# The editor is a non-inbound app, so its owner opens it with the ordinary ticket. The page must load, and the
# WebSocket the workbench opens next must get through the gateway: the upgrade carries the browser's Origin,
# which for the run's own page is the run's own origin.
HM_TICKET="$(mint_ticket "${RUN_HM_ID}")" || die "no ticket for the editor run"
open_in_host "${RUN_HM_ID}" vscode "${HM_TICKET}"
if [[ "${OPEN_STATUS}" == "303" && "${OPEN_LOCATION}" == "/r/${RUN_HM_ID}/vscode/" && -n "${OPEN_SESS}" ]]; then
  pass "vscode: the owner entered on the run's own host (303 -> ${OPEN_LOCATION})"
else
  fail "vscode: enter status ${OPEN_STATUS}, location '${OPEN_LOCATION}', headers: $(tr -d '\r' < "${TMPDIR}/henter.h" | tr '\n' '|')"
fi
HM_SESS="${OPEN_SESS}"
HM_ORIGIN="http://$(uihost "${RUN_HM_ID}")"
got_html=0
for _ in $(seq 1 6); do
  code=$(hreq "${RUN_HM_ID}" "${TMPDIR}/hm-editor" -L -H "Cookie: wardyn_ui_sess=${HM_SESS}" "$(iurl "${RUN_HM_ID}" vscode)")
  [[ "${code}" == "200" ]] && grep -qi "code-server" "${TMPDIR}/hm-editor.body" && { got_html=1; break; }
  sleep 5
done
if [[ "${got_html}" -eq 1 ]]; then
  pass "vscode: the editor page loaded in host mode ($(wc -c < "${TMPDIR}/hm-editor.body") bytes)"
else
  fail "vscode: editor page status ${code}, body head: $(head -c 300 "${TMPDIR}/hm-editor.body")"
fi
# ws_status RUN SESS ORIGIN OUT -> the status line code of a WebSocket upgrade to the editor (101 when the
# server accepts it; curl then holds the socket open until -m ends it, which is not a failure here).
ws_status() {
  local run="$1" sess="$2" origin="$3" out="$4" token
  token="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  curl -sS -m 6 --http1.1 --resolve "$(uihost "${run}"):127.0.0.1" -o /dev/null -D "${out}.h" \
    -H "Connection: Upgrade" -H "Upgrade: websocket" -H "Sec-WebSocket-Version: 13" \
    -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" -H "Origin: ${origin}" -H "Cookie: wardyn_ui_sess=${sess}" \
    "$(iurl "${run}" vscode "?reconnectionToken=${token}&reconnection=false&skipWebSocketFrames=true")" >/dev/null 2>&1 || true
  sed -n '1s/^HTTP\/[0-9.]* \([0-9]*\).*/\1/p' "${out}.h" | tr -d '\r'
}
code="$(ws_status "${RUN_HM_ID}" "${HM_SESS}" "${HM_ORIGIN}" "${TMPDIR}/hm-ws")"
if [[ "${code}" == "101" ]]; then
  pass "vscode: the editor's WebSocket upgrade (Origin ${HM_ORIGIN}) got through the gateway (101)"
else
  fail "vscode: the WebSocket upgrade with the run's own Origin answered '${code}', want 101: $(tr -d '\r' < "${TMPDIR}/hm-ws.h" | head -8 | tr '\n' '|')"
fi
# What the cross-site rule does with a webview's "null" origin: it refuses it, on a plain request and on an
# upgrade alike, because an Origin that is not the request's own is the rule's definition of cross-site.
refill
code=$(hreq "${RUN_HM_ID}" "${TMPDIR}/hm-null" -H "Cookie: wardyn_ui_sess=${HM_SESS}" -H "Origin: null" "$(iurl "${RUN_HM_ID}" vscode)")
want_refusal "vscode: a cookie request with Origin: null" "${code}" 403 ui_gateway_cross_site "${TMPDIR}/hm-null.body"
code="$(ws_status "${RUN_HM_ID}" "${HM_SESS}" "null" "${TMPDIR}/hm-ws-null")"
if [[ "${code}" == "403" ]]; then
  pass "vscode: a WebSocket upgrade with Origin: null is refused 403 by the cross-site rule"
else
  fail "vscode: the WebSocket upgrade with Origin: null answered '${code}', want 403: $(tr -d '\r' < "${TMPDIR}/hm-ws-null.h" | head -8 | tr '\n' '|')"
fi
code="$(ws_status "${RUN_HM_ID}" "${HM_SESS}" "https://evil.example" "${TMPDIR}/hm-ws-evil")"
if [[ "${code}" == "403" ]]; then
  pass "vscode: a WebSocket upgrade from another origin is refused 403"
else
  fail "vscode: the WebSocket upgrade from another origin answered '${code}', want 403"
fi

# ── the rate bound ──────────────────────────────────────────────────────────
# 400 calls from one caller, 300 of them in flight at once: the bucket holds 100 and refills at 50 a second,
# and the relay answers far slower than that (about 12 a second here), so the calls must arrive together, not
# one per answer, to outrun the refill.
sleep 3
mkdir -p "${TMPDIR}/rl"
# One call first, so the token's session is cached and the 300 take their verdicts at once, not each after a
# token lookup of its own.
hreq "${RUN_BG_ID}" "${TMPDIR}/rl/warm" -H "Authorization: Bearer ${CALLER_INBOUND}" "$(iurl "${RUN_BG_ID}" api warm)" >/dev/null || true
curl -sS -m 120 --parallel --parallel-immediate --parallel-max 300 --resolve "$(uihost "${RUN_BG_ID}"):127.0.0.1" \
  -H "Authorization: Bearer ${CALLER_INBOUND}" -o "${TMPDIR}/rl/#1.body" -D "${TMPDIR}/rl/#1.h" -w '%{http_code}\n' \
  "$(iurl "${RUN_BG_ID}" api "rl[1-400]")" >"${TMPDIR}/rl/codes.txt" 2>/dev/null || true
n200="$(grep -c '^200$' "${TMPDIR}/rl/codes.txt" || true)"
n429="$(grep -c '^429$' "${TMPDIR}/rl/codes.txt" || true)"
n503="$(grep -c '^503$' "${TMPDIR}/rl/codes.txt" || true)" # admitted, then no relay connection within the wait
first429="$(grep -l '^HTTP/[0-9.]* 429' "${TMPDIR}"/rl/*.h 2>/dev/null | head -1 || true)"
if [[ "${n429:-0}" -ge 1 && "${n200:-0}" -ge 1 && -n "${first429}" ]] \
   && [[ "$(reason_of "${first429%.h}.body")" == "inbound_rate_limited" ]] && grep -qi '^retry-after: *1' "${first429}"; then
  pass "rate bound: of 400 calls from one caller ${n200} were answered, ${n429} refused 429 inbound_rate_limited with Retry-After: 1 and ${n503:-0} admitted but not relayed (503)"
else
  fail "rate bound: ${n200:-0} answered, ${n429:-0} refused, ${n503:-0} not relayed (503); first 429 file '${first429}' (reason '$(reason_of "${first429%.h}.body")')"
fi

# ── the inbound audit trail ─────────────────────────────────────────────────
inbound_rows() { # RUN JQ_FILTER -> the number of the run's inbound.* rows the filter keeps
  jq "[.[] | select($2)] | length" "${TMPDIR}/inbound-$1.json"
}
for r in "${RUN_BG_ID}" "${RUN_HM_ID}"; do
  api GET "/api/v1/audit?run_id=${r}&action_prefix=inbound.&limit=1000" >/dev/null
  cp "${TMPDIR}/resp.json" "${TMPDIR}/inbound-${r}.json"
done
check_rows() { # LABEL RUN JQ_FILTER
  local n
  n="$(inbound_rows "$2" "$3")"
  if [[ "${n:-0}" -ge 1 ]]; then pass "audit: $1 (${n} row(s))"; else fail "audit: no row for: $1"; fi
}
check_rows "inbound.allow by token, naming e2e-caller, the entry that admitted it and the token id" "${RUN_BG_ID}" \
  '.action=="inbound.allow" and .outcome=="success" and .actor=="e2e-caller" and .target=="api" and .data.via=="token" and .data.caller=="user:e2e-caller" and .data.method=="GET" and .data.path=="/probe" and (.data.token_id|length)>0'
check_rows "inbound.allow by session for the person who opened the app" "${RUN_BG_ID}" \
  '.action=="inbound.allow" and .actor=="e2e-caller" and .data.via=="session" and .data.caller=="user:e2e-caller"'
check_rows "inbound.deny credential_invalid for the API-kind token" "${RUN_BG_ID}" \
  '.action=="inbound.deny" and .outcome=="denied" and .data.reason=="credential_invalid" and .data.via=="token"'
check_rows "inbound.deny not_found for the caller nobody named" "${RUN_BG_ID}" \
  '.action=="inbound.deny" and .actor=="e2e-stranger" and .data.reason=="not_found"'
check_rows "inbound.deny cross_site for the Origin: null cookie request" "${RUN_BG_ID}" \
  '.action=="inbound.deny" and .data.reason=="cross_site"'
check_rows "inbound.deny rate_limited" "${RUN_BG_ID}" \
  '.action=="inbound.deny" and .data.reason=="rate_limited"'
check_rows "inbound.deny not_found for the non-inbound editor app" "${RUN_HM_ID}" \
  '.action=="inbound.deny" and .target=="vscode" and .data.reason=="not_found"'
if [[ "$(inbound_rows "${RUN_BG_ID}" '(.data.path // "") | test("secret")')" == "0" ]]; then
  pass "audit: no inbound row records a query string"
else
  fail "audit: an inbound row carries the request's query"
fi

# ── summary ──────────────────────────────────────────────────────────────────
if [[ "${FAILED}" -eq 0 ]]; then
  log "UI-SANDBOX E2E: PASS (all checks above passed)"
else
  printf '\033[1;31m[error]\033[0m %s\n' "UI-SANDBOX E2E: FAIL (see [FAIL] lines above)" >&2
fi
exit "${FAILED}"
