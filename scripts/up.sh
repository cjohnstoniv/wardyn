#!/bin/sh
# scripts/up.sh — one-command launcher + doctor/preflight for local Wardyn,
# via the Docker COMPOSE path (deploy/compose/docker-compose.yaml). This is
# what `make setup` / `make doctor` / `make dev-pg` call.
#
# POSIX sh (set -eu) on purpose: this must run the same under macOS/Linux bash
# as /bin/sh, WSL, and dash/busybox ash. It wraps the EXISTING compose file +
# Makefile targets (scripts/demo.sh's healthz-wait shape, `make compose-down`,
# `make agent-images`) rather than reimplementing them.
#
# Usage:
#   scripts/up.sh [doctor|up|down|reset|pg]   (default: up)
#
#   doctor    Read-only preflight. Exits 2 if it finds a BLOCKing issue.
#   up        doctor, build wardynd, configure, start postgres+wardynd, open the
#             browser at the Getting-started page, THEN build the per-run images.
#   down      Tear down (delegates to `make compose-down`); KEEPS volumes/data.
#   reset     Wipe volumes (Postgres runs + append-only audit + recordings) then
#             `up` — the explicit clean-slate path. `down` keeps data on purpose
#             (audit is a system of record); `reset` is how you deliberately start
#             from an EMPTY Runs list on a machine that has run Wardyn before.
#   pg        Start/ensure the dockerized dev/e2e Postgres (wardyn-test-pg :55432).
set -eu

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
COMPOSE_FILE="${REPO_ROOT}/deploy/compose/docker-compose.yaml"
ENV_FILE="${REPO_ROOT}/deploy/compose/.env"
ENV_EXAMPLE="${REPO_ROOT}/deploy/compose/.env.example"

. "${REPO_ROOT}/scripts/lib/common.sh"

compose() { docker compose -f "${COMPOSE_FILE}" "$@"; }

# pick_policy RUNTIMES_JSON [WANTS_LLM] -> a WARDYN_DEFAULT_POLICY path.
# RUNTIMES_JSON is the output of `docker info --format '{{json .Runtimes}}'`.
# runc (CC1) is always assumed present (every Docker install ships it); a
# "runsc" key means gVisor (CC2) is available, so the stricter default.json
# (min_confinement_class CC2) can be used instead of the CC1 demo.json.
# WANTS_LLM="1" (the operator has opted into a real model path — see
# composer_wants_llm) upgrades to composer-dev.json, the only shipped ceiling that
# admits the api_key grant + LLM egress a COMPOSED run needs. Without it BOTH
# demo.json and default.json carry only a github_token grant, so clampGrants strips
# the composer's auto-minted model grant and a first composed run boots, "completes",
# and 404s on its first model call. Kept off by default so a pure-Fence trial keeps
# the tight github-token-only ceiling.
pick_policy() {
  if [ "${2:-}" = "1" ]; then
    echo "/examples/policies/composer-dev.json"
    return
  fi
  case "$1" in
    *'"runsc"'*) echo "/examples/policies/default.json" ;;
    *)           echo "/examples/policies/demo.json" ;;
  esac
}

# composer_wants_llm ENV_FILE -> "1" | ""
# "1" when the operator has opted into a real model path: a non-fake composer
# backend is configured, or a host LLM API key is exported.
composer_wants_llm() {
  _cwl_cfg=$(env_get "$1" WARDYN_COMPOSER_CONFIG)
  case "${_cwl_cfg}" in
    ''|*'"wire":"fake"'*|*'"wire": "fake"'*) : ;;  # unset or the fake default → no signal from config
    *) echo 1; return ;;                            # a real (non-fake) backend is configured
  esac
  [ -n "${ANTHROPIC_API_KEY:-}" ] && { echo 1; return; }
  [ -n "${OPENAI_API_KEY:-}" ]    && { echo 1; return; }
  echo ""
}

# os_kind -> windows | wsl | linux | darwin | unknown
os_kind() {
  _ok_u=$(uname -s 2>/dev/null || echo unknown)
  _ok_proc=""
  [ -r /proc/version ] && _ok_proc=$(cat /proc/version 2>/dev/null || true)
  # WSL must be checked before the generic Linux case: WSL's own `uname -s`
  # reports "Linux".
  case "${_ok_proc}" in *[Mm]icrosoft*) echo wsl; return ;; esac
  if [ -n "${WSL_DISTRO_NAME:-}" ]; then echo wsl; return; fi
  case "${_ok_u}" in
    MINGW*|MSYS*|CYGWIN*) echo windows; return ;;
  esac
  if [ "${OS:-}" = "Windows_NT" ]; then echo windows; return; fi
  case "${_ok_u}" in
    Linux)  echo linux ;;
    Darwin) echo darwin ;;
    *)      echo unknown ;;
  esac
}

# port_in_use PORT — best-effort; tries whatever's on PATH, defaults to "free"
# (0 = in use, 1 = free/unknown — this only ever drives a WARN, never a block).
port_in_use() {
  _piu_p=$1
  if command -v ss >/dev/null 2>&1; then
    ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[.:]${_piu_p}\$"
  elif command -v lsof >/dev/null 2>&1; then
    lsof -iTCP -sTCP:LISTEN -P 2>/dev/null | grep -q ":${_piu_p} "
  elif command -v nc >/dev/null 2>&1; then
    nc -z 127.0.0.1 "${_piu_p}" >/dev/null 2>&1
  else
    return 1
  fi
}

# open_url URL — best-effort browser opener. Honors WARDYN_UP_NO_BROWSER=1.
open_url() {
  if [ "${WARDYN_UP_NO_BROWSER:-0}" = "1" ]; then
    log "WARDYN_UP_NO_BROWSER=1 — open manually: $1"
    return 0
  fi
  case "$(os_kind)" in
    darwin)
      command -v open >/dev/null 2>&1 && { open "$1"; return 0; }
      ;;
    wsl)
      # wslview (wslu) opens the URL in the WINDOWS default browser; explorer.exe
      # does the same via a documented side effect. Prefer wslview if present.
      command -v wslview >/dev/null 2>&1 && { wslview "$1"; return 0; }
      command -v explorer.exe >/dev/null 2>&1 && { explorer.exe "$1" >/dev/null 2>&1 || true; return 0; }
      ;;
    linux)
      command -v xdg-open >/dev/null 2>&1 && { xdg-open "$1" >/dev/null 2>&1 & return 0; }
      ;;
  esac
  log "Open in your browser: $1"
}

# env_get FILE KEY -> current value (empty if unset/absent). Ignores comments
# (matches only an uncommented "KEY=" line start).
env_get() {
  [ -f "$1" ] || return 0
  grep -E "^$2=" "$1" 2>/dev/null | tail -1 | cut -d= -f2-
}

# env_set FILE KEY VALUE — idempotently set KEY=VALUE, replacing an existing
# uncommented line or appending. Plain awk (no sed -i) so it behaves the same
# under GNU and BSD userlands.
env_set() {
  _es_file=$1; _es_key=$2; _es_val=$3
  if [ -f "${_es_file}" ] && grep -qE "^${_es_key}=" "${_es_file}"; then
    awk -v k="${_es_key}=" -v line="${_es_key}=${_es_val}" \
      'index($0,k)==1{print line; next}{print}' "${_es_file}" > "${_es_file}.tmp"
    mv "${_es_file}.tmp" "${_es_file}"
  else
    printf '%s=%s\n' "${_es_key}" "${_es_val}" >> "${_es_file}"
  fi
}

# ── doctor ───────────────────────────────────────────────────────────────

DOCTOR_BLOCKED=0

report() {  # report LEVEL MESSAGE
  case "$1" in
    ok)    printf '  [ok]    %s\n' "$2" ;;
    warn)  printf '  [warn]  %s\n' "$2" ;;
    block) printf '  [BLOCK] %s\n' "$2"; DOCTOR_BLOCKED=1 ;;
  esac
}

cmd_doctor() {
  DOCTOR_BLOCKED=0
  log "Wardyn doctor — read-only preflight"

  _kind=$(os_kind)
  case "${_kind}" in
    windows)
      report block "native Windows shell detected. Install WSL2 + Docker Desktop (enable WSL integration), then run \`make setup\` INSIDE your WSL distro — not from cmd.exe/PowerShell."
      ;;
    wsl)    report ok "WSL detected (${WSL_DISTRO_NAME:-distro unknown}) — \`make setup\` opens the UI in the Windows browser." ;;
    linux)  report ok "native Linux detected." ;;
    darwin) report ok "macOS detected." ;;
    *)      report warn "could not determine OS (uname -s = $(uname -s 2>/dev/null || echo '?')); proceeding anyway." ;;
  esac

  if ! command -v docker >/dev/null 2>&1; then
    report block "docker not found on PATH. Install Docker: https://docs.docker.com/get-docker/"
  elif ! docker info >/dev/null 2>&1; then
    report block "docker daemon not reachable. Start Docker Desktop (macOS/Windows) or dockerd (Linux), then re-run \`make doctor\`."
  else
    report ok "docker daemon reachable."
    if docker compose version >/dev/null 2>&1; then
      report ok "docker compose v2 available ($(docker compose version 2>/dev/null | head -1))."
    else
      report block "docker compose v2 required (standalone docker-compose v1 is not supported). Update Docker Desktop or install the compose plugin."
    fi

    _runtimes=$(docker info --format '{{json .Runtimes}}' 2>/dev/null || echo '{}')
    _classes="CC1 (runc, always)"
    case "${_runtimes}" in *'"runsc"'*) _classes="${_classes}, CC2 (gVisor/runsc)" ;; esac
    case "${_runtimes}" in *'"kata'*)   _classes="${_classes}, CC3 (kata)" ;; esac
    report ok "confinement classes available: ${_classes}"
  fi

  _port="${WARDYN_UP_PORT:-8080}"
  if port_in_use "${_port}"; then
    report warn "port ${_port} already in use — wardynd may fail to bind. Override with WARDYN_UP_PORT=<port>, or free the port. Never force-killed by this tool."
  else
    report ok "port ${_port} free."
  fi
  if port_in_use 5432; then
    report warn "port 5432 already in use — postgres may fail to bind (an existing wardyn-postgres container already holding it is fine)."
  else
    report ok "port 5432 free."
  fi

  if [ -e /dev/kvm ]; then
    report ok "/dev/kvm present (CC3/Kata-capable hardware)."
  else
    report warn "/dev/kvm not present — CC3 (Kata) confinement tier unavailable (optional; CC1/CC2 unaffected)."
  fi
  if [ -r /sys/kernel/btf/vmlinux ]; then
    report ok "/sys/kernel/btf/vmlinux present (eBPF ground-truth tier possible)."
  else
    report warn "/sys/kernel/btf/vmlinux not present — eBPF/Tetragon ground-truth tier unavailable (optional)."
  fi
  if command -v claude >/dev/null 2>&1; then
    report ok "claude CLI on PATH (host-mode composer backend, scripts/run-host.sh, available)."
  else
    report warn "claude CLI not on PATH — host-mode composer unavailable (optional; the compose path's Describe-mode uses the no-key fake backend by default)."
  fi

  if [ "${DOCTOR_BLOCKED}" -eq 1 ]; then
    printf '\n' >&2
    printf '\033[1;31m[error]\033[0m %s\n' "doctor found blocking issue(s) above — fix them, then re-run \`make doctor\`." >&2
    exit 2
  fi
  log "doctor: no blocking issues (see warnings above, if any)."
}

# ── up ───────────────────────────────────────────────────────────────────

cmd_up() {
  cmd_doctor

  log "Building the wardynd image (serves the REST API + embedded UI)"
  compose build

  if [ ! -f "${ENV_FILE}" ]; then
    log "Creating ${ENV_FILE} from .env.example"
    cp "${ENV_EXAMPLE}" "${ENV_FILE}"
  fi

  if ! grep -qE '^WARDYN_AGE_KEY=AGE-SECRET-KEY-' "${ENV_FILE}" 2>/dev/null; then
    log "Minting a persistent secret-store age key"
    _keyline=$(docker run --rm wardyn/wardynd:demo -gen-age-key 2>/dev/null | grep '^AGE-SECRET-KEY-' | head -1 || true)
    if [ -n "${_keyline}" ]; then
      env_set "${ENV_FILE}" WARDYN_AGE_KEY "${_keyline}"
      log "Persisted WARDYN_AGE_KEY to ${ENV_FILE} (secrets now survive restarts)"
    else
      warn "wardyn/wardynd:demo -gen-age-key produced no key (this wardynd build may predate the flag)."
      warn "Continuing with an ephemeral key — fine for now, but secrets won't survive a container restart."
    fi
  fi
  chmod 600 "${ENV_FILE}" 2>/dev/null || true

  env_set "${ENV_FILE}" WARDYN_LOCAL_MODE true
  # LocalMode no-auth requires a loopback request PEER; in compose the peer is the
  # docker gateway (port is published loopback-only, so LAN peers can't reach it).
  # Trust the forwarder so the host UI/CLI isn't 403'd. Safe only with the 127.0.0.1
  # publish this stack uses (see docker-compose.yaml).
  env_set "${ENV_FILE}" WARDYN_LOCAL_TRUST_FORWARDER true
  env_set "${ENV_FILE}" WARDYN_OIDC_ISSUER ""

  _policy="${WARDYN_DEFAULT_POLICY:-}"
  [ -z "${_policy}" ] && _policy=$(env_get "${ENV_FILE}" WARDYN_DEFAULT_POLICY)
  if [ -z "${_policy}" ]; then
    _runtimes=$(docker info --format '{{json .Runtimes}}' 2>/dev/null || echo '{}')
    _policy=$(pick_policy "${_runtimes}" "$(composer_wants_llm "${ENV_FILE}")")
    log "Auto-picked default policy: ${_policy}"
    case "${_policy}" in
      */composer-dev.json)
        log "  (composer-capable ceiling — a real model path is configured; a composed run can reach its LLM)" ;;
    esac
  fi
  env_set "${ENV_FILE}" WARDYN_DEFAULT_POLICY "${_policy}"

  if [ -z "$(env_get "${ENV_FILE}" WARDYN_COMPOSER_CONFIG)" ]; then
    env_set "${ENV_FILE}" WARDYN_COMPOSER_CONFIG '{"default":"dev","backends":[{"name":"dev","wire":"fake","model":"demo"}]}'
  fi
  chmod 600 "${ENV_FILE}" 2>/dev/null || true

  log "Starting postgres + wardynd (local mode, no SSO — see \`docker compose --profile sso up\` for Dex)"
  compose up -d postgres wardynd

  _url="http://localhost:${WARDYN_UP_PORT:-8080}"
  # Wait on the CONTAINER's own health (its healthcheck runs `wardyn runs list`
  # INSIDE the container), not a host curl to localhost:PORT. On Docker Desktop +
  # WSL2 in NAT mode a published 127.0.0.1:PORT is reachable from the Windows
  # browser but NOT from this WSL shell, so a host-curl health gate gives a false
  # "did not become healthy" even when the stack is fine. Container health is
  # network-topology-independent. Accept a host-curl too (covers Linux/mirrored).
  _cid=$(compose ps -q wardynd 2>/dev/null || true)
  log "Waiting for wardynd to become healthy"
  _tries=0
  until { [ -n "${_cid}" ] && [ "$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${_cid}" 2>/dev/null)" = "healthy" ]; } \
        || curl -fsS "${_url}/healthz" >/dev/null 2>&1; do
    _tries=$((_tries + 1))
    if [ "${_tries}" -gt 60 ]; then
      compose logs --tail 50 wardynd
      die "wardynd did not become healthy — see logs above (or: docker compose -f ${COMPOSE_FILE} logs wardynd)"
    fi
    sleep 2
  done
  log "wardynd is healthy"

  # Can THIS shell reach the published UI port? In WSL2 NAT mode it usually
  # cannot (only the Windows browser can) — an honest note, not a failure.
  if ! curl -fsS -m 3 "${_url}/healthz" >/dev/null 2>&1; then
    warn "the UI at ${_url} is reachable from your Windows browser but not from this WSL shell (Docker Desktop + WSL2 NAT). CLI calls from WSL won't hit it; enable WSL2 mirrored networking if you want shell access too."
  fi

  # Sandbox → control-plane reachability CONFIRMATION (the inverse of the
  # host-mode run-local warning). In this compose path wardynd runs as a
  # container on wardyn-internal, so a run's proxy sidecar reaches it at
  # http://wardynd:8080 over Docker DNS with NO host/NAT hop — which is what
  # lets workspace VERIFY report its result (the exact thing that can't work on
  # Docker Desktop + WSL2 when wardynd runs host-mode). Prove it with a
  # throwaway container on the same network; never fatal.
  if docker run --rm --network wardyn-internal curlimages/curl:latest \
       -s -m 5 -o /dev/null "http://wardynd:8080/healthz" >/dev/null 2>&1; then
    log "Sandbox → control-plane reachability: OK — workspace Verify and Record will complete on this instance."
  else
    warn "sandbox → control-plane probe failed (http://wardynd:8080 on wardyn-internal). Verify results may not report and Record captures will land empty (record_failed); check 'docker network inspect wardyn-internal'."
  fi

  # LOCAL-MODE no-auth GATE smoke (closes the masking class from the N1/forwarder
  # regression). /healthz is OUTSIDE the auth group, and the container healthcheck
  # runs on loopback INSIDE the container — neither exercises host→gated-API from a
  # NON-loopback peer, which is exactly what WARDYN_LOCAL_TRUST_FORWARDER must allow.
  # Hit a gated endpoint (/api/v1/me) from an in-network container (a non-loopback
  # peer, like the docker gateway a host request arrives as): 200 = forwarder OK;
  # 403 = the peer gate is wrongly rejecting compose traffic (a real regression).
  # WSL2 NAT can't do this from the host shell, so probe from the network instead.
  _me_code=$(docker run --rm --network wardyn-internal curlimages/curl:latest \
    -s -m 5 -o /dev/null -w '%{http_code}' "http://wardynd:8080/api/v1/me" 2>/dev/null || echo 000)
  case "${_me_code}" in
    200) log "Local-mode no-auth gate: OK (gated API reachable from a non-loopback peer — WARDYN_LOCAL_TRUST_FORWARDER effective)." ;;
    403) warn "Local-mode no-auth gate REJECTED a non-loopback peer (HTTP 403). The UI/CLI will be locked out — ensure WARDYN_LOCAL_TRUST_FORWARDER=true reached wardynd (docker compose config)." ;;
    *)   warn "Local-mode gate probe inconclusive (HTTP ${_me_code}); check 'docker compose -f ${COMPOSE_FILE} logs wardynd'." ;;
  esac

  open_url "${_url}"
  log "Wardyn is up: ${_url}  (local mode — no login) — the Getting-started page is ready NOW."

  # The per-run sandbox proxy + agent images are NOT needed to reach the UI or the
  # Getting-started page — only to LAUNCH a run. Build them AFTER the browser is
  # open so first light is as fast as possible; you read Getting-started while these
  # finish. Skip them for a pure UI look with WARDYN_UP_SKIP_RUN_IMAGES=1.
  if [ "${WARDYN_UP_SKIP_RUN_IMAGES:-0}" = "1" ]; then
    log "WARDYN_UP_SKIP_RUN_IMAGES=1 — skipping the run images (build later: make agent-images && docker compose -f \"${COMPOSE_FILE}\" --profile build-only build proxy-image)"
  else
    log "Finishing the run components so your first run is ready (sandbox proxy + agent images)…"
    compose --profile build-only build proxy-image
    make -C "${REPO_ROOT}" agent-images
    log "Run components ready — you can launch your first run."
  fi

  log "  Tear down: make compose-down   (or: scripts/up.sh down)"
}

# ── down / pg ────────────────────────────────────────────────────────────

cmd_down() {
  log "Delegating teardown to \`make compose-down\`"
  make -C "${REPO_ROOT}" compose-down
}

# reset — deliberate clean slate. `down` keeps the named volumes (postgres_data,
# recordings, audit) so runs + the append-only audit log survive a restart, which
# is what you want normally. reset REMOVES them, so the following `up` starts with
# an EMPTY Runs list — the honest "fresh like a new clone" state on a machine that
# has run Wardyn before. Irreversible on purpose; the `reset` verb IS the consent.
cmd_reset() {
  warn "reset REMOVES the compose volumes: Postgres (ALL runs + the append-only audit log) and recordings."
  warn "This is irreversible. Plain \`make compose-down\` keeps them; use that if you only want to stop the stack."
  compose down -v --remove-orphans
  log "Volumes removed — bringing up a fresh, empty Wardyn"
  cmd_up
}

cmd_pg() {
  # The exact CI incantation (.github/workflows/ci.yml "Start Postgres" step),
  # reused verbatim as the single source of truth — plus idempotent reuse and
  # the wardyn_e2e database e2e-backend.sh/run-ui-e2e.sh expect, so the dev/e2e
  # loop can self-provision instead of dying on a missing container.
  if docker inspect wardyn-test-pg >/dev/null 2>&1; then
    log "wardyn-test-pg already exists — ensuring it's running"
    docker start wardyn-test-pg >/dev/null 2>&1 || true
  else
    log "Starting wardyn-test-pg (dev/e2e Postgres) on :55432"
    docker run -d --name wardyn-test-pg \
      -e POSTGRES_PASSWORD=wardyn -e POSTGRES_USER=wardyn -e POSTGRES_DB=wardyn \
      -p 55432:5432 postgres:16-alpine
  fi

  _tries=0
  until docker exec wardyn-test-pg pg_isready -U wardyn >/dev/null 2>&1; do
    _tries=$((_tries + 1))
    [ "${_tries}" -gt 30 ] && die "wardyn-test-pg did not become ready on :55432"
    sleep 1
  done

  # scripts/e2e-backend.sh's default DB (run-local.sh creates/drops its own
  # wardyn_local on demand, so it needs nothing precreated here).
  docker exec wardyn-test-pg psql -U wardyn -d wardyn -c "CREATE DATABASE wardyn_e2e" >/dev/null 2>&1 || true
  log "wardyn-test-pg ready on :55432 (databases: wardyn, wardyn_e2e)"
}

# ── dispatch ─────────────────────────────────────────────────────────────

cmd="${1:-up}"
case "${cmd}" in
  doctor) cmd_doctor ;;
  up)     cmd_up ;;
  down)   cmd_down ;;
  reset)  cmd_reset ;;
  pg)     cmd_pg ;;
  *) die "usage: $0 {doctor|up|down|reset|pg}" ;;
esac
