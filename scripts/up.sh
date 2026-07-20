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
#   scripts/up.sh [doctor|up|down|reset|reset-all|pg]   (default: up)
#
#   doctor    Read-only preflight. Exits 2 if it finds a BLOCKing issue.
#   up        doctor, build wardynd, configure, start postgres+wardynd, open the
#             browser at the Getting-started page, THEN build the per-run images.
#   down      Tear down (delegates to `make compose-down`); KEEPS volumes/data.
#   reset     Wipe volumes (Postgres runs + append-only audit + recordings) then
#             `up` — the explicit clean-slate path. `down` keeps data on purpose
#             (audit is a system of record); `reset` is how you deliberately start
#             from an EMPTY Runs list on a machine that has run Wardyn before.
#   reset-all FULL undo of local setup across BOTH modes: host daemon + compose
#             stack + ~/.wardyn install files. Leaves the machine clean (no
#             re-up). Flags: --dry-run --purge-images --purge-env.
#   pg        Start/ensure the dockerized dev/e2e Postgres (wardyn-test-pg :55432).
set -eu

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
COMPOSE_FILE="${REPO_ROOT}/deploy/compose/docker-compose.yaml"
ENV_FILE="${REPO_ROOT}/deploy/compose/.env"
ENV_EXAMPLE="${REPO_ROOT}/deploy/compose/.env.example"

. "${REPO_ROOT}/scripts/lib/common.sh"

# Every entry point (setup/up/doctor/reset/reset-all/pg) must operate on the
# SAME daemon — setup.sh exports its pick before delegating here, but direct
# `make compose-up` / `make reset-all` invocations need it too, or teardown
# inspects a different daemon than the one setup populated.
wardyn_pick_docker_host

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

# _confirm PROMPT — shared consent gate for destructive commands (same
# convention as setup.sh's stale-store recovery): WARDYN_FORCE_RESET=1 is the
# headless yes; otherwise an interactive prompt defaulting to No; non-interactive
# without the env var refuses. Callers decide the exit code on refusal (reset's
# contract: interactive decline exits 0, non-interactive refusal exits 2).
_confirm() {
  [ "${WARDYN_FORCE_RESET:-}" = 1 ] && return 0
  if [ -t 0 ]; then
    printf '  %s [y/N] ' "$1"
    read -r _c_a || _c_a=""
    case "${_c_a}" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
  fi
  warn "Non-interactive: set WARDYN_FORCE_RESET=1 to proceed."
  return 1
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

    # Resource-cap enforceability HINT (pre-boot; advisory). The authoritative gate
    # is post-create: a governed run refuses only if the daemon actually DISCARDS a
    # requested limit (create-response warning). These docker-info booleans are just
    # an early heads-up and are UNRELIABLE on Podman's compat API (it under-reports
    # CpuCfsQuota=false even when the quota binds), so treat a warn here as "check",
    # not "will fail".
    _caps=$(docker info --format '{{.MemoryLimit}}/{{.PidsLimit}}/{{.CPUCfsQuota}}' 2>/dev/null || echo '?/?/?')
    case "${_caps}" in
      true/true/true) report ok "resource caps look enforceable (memory + pids + cpu; cgroup v$(docker info --format '{{.CgroupVersion}}' 2>/dev/null || echo '?'))." ;;
      *) report warn "docker info hints some resource limits may not enforce (memory/pids/cpu = ${_caps}). If a real run is refused (daemon discarded a limit), delegate the cgroup v2 controllers (systemd: Delegate=yes; rootless: enable cgroup v2 delegation) or set WARDYN_ALLOW_UNENFORCEABLE_CAPS=1. On Podman this is often a false alarm (compat API under-reports; caps still bind)." ;;
    esac
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

  # ── corporate-network preflight ───────────────────────────────────────────
  # Turn the two classic 3-minute-build / silent-bring-up failures into a warning
  # BEFORE anything is built: (1) a TLS-MITM proxy present but no corp CA staged,
  # (2) the chosen docker socket not actually bind-mountable by the daemon.
  # Signal on an explicit forward proxy only — a low-false-positive predictor of a
  # build-breaking TLS-MITM. (Custom CAs in the trust store are too noisy: mkcert
  # and other local-dev CAs live there too.) A transparent MITM with no proxy env
  # won't trip this, but the build's own x509 error then points here.
  _corp_signal=0
  for _pv in HTTPS_PROXY https_proxy HTTP_PROXY http_proxy; do
    eval "_pval=\${${_pv}:-}"; [ -n "${_pval}" ] && _corp_signal=1
  done
  if [ "${_corp_signal}" = 1 ]; then
    if [ -f "${REPO_ROOT}/deploy/images/corp-ca.pem" ]; then
      report ok "forward proxy set and deploy/images/corp-ca.pem is staged — image builds will trust your corp CA."
    else
      report warn "a forward proxy is set (HTTP(S)_PROXY) but deploy/images/corp-ca.pem is NOT staged. If a build fails with 'x509: certificate signed by unknown authority', copy your corp root CA to deploy/images/corp-ca.pem (gitignored) and rebuild — see deploy/images/README.md."
    fi
  fi
  unset _corp_signal _pv _pval

  # The compose wardynd bind-mounts WARDYN_DOCKER_SOCK to drive the daemon. Assert
  # it is actually bind-mountable (not merely that the CLI can reach the daemon):
  # on Rancher Desktop the host ~/.rd/docker.sock is NOT mountable while the in-VM
  # /var/run/docker.sock is. wardyn_pick_docker_host (sourced at top) resolves it.
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    wardyn_pick_docker_host 2>/dev/null || true
    _sock="${WARDYN_DOCKER_SOCK:-/var/run/docker.sock}"
    if [ -S "${_sock}" ]; then
      report ok "docker socket ${_sock} present on host (bind-mountable)."
    elif docker run --rm -v "${_sock}:/probe.sock" alpine:3.20 test -S /probe.sock >/dev/null 2>&1; then
      report ok "docker socket ${_sock} is bind-mountable by the daemon (in-VM path, e.g. Rancher Desktop)."
    else
      report warn "chosen docker socket ${_sock} is neither present on the host nor bind-mountable by the daemon — the compose wardynd can't create sandboxes. On Rancher Desktop set WARDYN_DOCKER_SOCK=/var/run/docker.sock (the in-VM path); otherwise check the path and permissions."
    fi
    unset _sock
  fi

  # Bedrock model-auth preflight (host vs container): which credential source is
  # usable here. Region/model come from env or deploy/compose/.env; the actual
  # secrets (bedrock-api-key / static keys) live in the store and are reported by
  # `wardyn setup status` after boot. This is a pre-boot heads-up only.
  _br_region="${WARDYN_BEDROCK_REGION:-$(env_get "${ENV_FILE}" WARDYN_BEDROCK_REGION 2>/dev/null || true)}"
  _br_dir="${WARDYN_BEDROCK_AWS_DIR:-$(env_get "${ENV_FILE}" WARDYN_BEDROCK_AWS_DIR 2>/dev/null || true)}"
  if [ -n "${_br_region}" ]; then
    if [ -n "${_br_dir}" ] && [ -d "${_br_dir}" ]; then
      report ok "Bedrock configured with an ~/.aws mount (${_br_dir}) — SSO/temp creds auto-rotate; grant uid 1000 read (setfacl -R -m u:1000:rX '${_br_dir}') if runs can't auth."
    else
      report ok "Bedrock region set (${_br_region}). Prefer a bedrock-api-key bearer (never resident) or an ~/.aws mount for SSO; add credentials in the UI or via 'wardyn secret set' — 'wardyn setup status' shows which path is live after boot."
    fi
  fi
  unset _br_region _br_dir

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
    _keyline=$(docker run --rm wardyn/wardynd:local -gen-age-key 2>/dev/null | grep '^AGE-SECRET-KEY-' | head -1 || true)
    if [ -n "${_keyline}" ]; then
      env_set "${ENV_FILE}" WARDYN_AGE_KEY "${_keyline}"
      log "Persisted WARDYN_AGE_KEY to ${ENV_FILE} (secrets now survive restarts)"
    else
      warn "wardyn/wardynd:local -gen-age-key produced no key (this wardynd build may predate the flag)."
      warn "Continuing with an ephemeral key — fine for now, but secrets won't survive a container restart."
    fi
  fi
  chmod 600 "${ENV_FILE}" 2>/dev/null || true

  # Persist the daemon choice wardyn_pick_docker_host derived. It is otherwise
  # env-only, so a bare `docker compose up -d wardynd` (outside this script)
  # falls back to compose's /var/run/docker.sock default and SILENTLY collapses
  # confinement to Fence on a dual-daemon box — runsc/kata live on the other
  # daemon and simply go invisible. Writing it to .env makes every later compose
  # invocation, from any shell, drive the same daemon.
  [ -n "${WARDYN_DOCKER_SOCK:-}" ] && env_set "${ENV_FILE}" WARDYN_DOCKER_SOCK "${WARDYN_DOCKER_SOCK}"

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

  # A stale WARDYN_AGENT_IMAGES override in .env silently breaks every run at
  # pull time ("registry: denied" — locally-built tags exist in no registry),
  # and .env survives reset by design. Check the referenced images actually
  # exist on the daemon we're about to use, and say how to fix it if not.
  _ai_json=$(env_get "${ENV_FILE}" WARDYN_AGENT_IMAGES)
  if [ -n "${_ai_json}" ]; then
    for _ai_img in $(printf '%s' "${_ai_json}" | tr ',{}' '\n\n\n' | sed -n 's/.*:"\([^"]*\)".*/\1/p'); do
      docker image inspect "${_ai_img}" >/dev/null 2>&1 \
        || warn ".env overrides WARDYN_AGENT_IMAGES with '${_ai_img}' — NOT present on this daemon; runs naming that agent fail at pull time. Rebuild it, fix the override in ${ENV_FILE}, or start clean (make reset-all ARGS=--purge-env)."
    done
    unset _ai_json _ai_img
  fi

  # Bedrock auto-wire (container path): persist operator-provided Bedrock config
  # into .env so the compose wardynd reads it at boot — closing the gap where the
  # container path (unlike host-mode setup.sh) required hand-editing .env. Triggers
  # only on an EXPLICIT Bedrock signal (CLAUDE_CODE_USE_BEDROCK, or region+model in
  # env); never guesses from a bare ~/.aws (many machines have one for unrelated AWS
  # work). Idempotent: never overwrites a key already in .env. Credentials are NOT
  # imported here — they're added in the UI after launch (the wizard now surfaces
  # the bearer/session-token/static-key options) or via 'wardyn secret set'.
  _br_on=0
  case "${CLAUDE_CODE_USE_BEDROCK:-}" in 1|true|TRUE|yes) _br_on=1 ;; esac
  [ -n "${WARDYN_BEDROCK_REGION:-}" ] && [ -n "${WARDYN_BEDROCK_MODEL:-}" ] && _br_on=1
  if [ "${_br_on}" = 1 ]; then
    _br_region="${WARDYN_BEDROCK_REGION:-${AWS_REGION:-${AWS_DEFAULT_REGION:-}}}"
    if [ -n "${_br_region}" ] && [ -z "$(env_get "${ENV_FILE}" WARDYN_BEDROCK_REGION)" ]; then
      env_set "${ENV_FILE}" WARDYN_BEDROCK_REGION "${_br_region}"
      log "Bedrock: wired region ${_br_region} into ${ENV_FILE}."
    fi
    if [ -n "${WARDYN_BEDROCK_MODEL:-}" ] && [ -z "$(env_get "${ENV_FILE}" WARDYN_BEDROCK_MODEL)" ]; then
      env_set "${ENV_FILE}" WARDYN_BEDROCK_MODEL "${WARDYN_BEDROCK_MODEL}"
    fi
    [ -n "${WARDYN_BEDROCK_AWS_PROFILE:-}" ] && [ -z "$(env_get "${ENV_FILE}" WARDYN_BEDROCK_AWS_PROFILE)" ] \
      && env_set "${ENV_FILE}" WARDYN_BEDROCK_AWS_PROFILE "${WARDYN_BEDROCK_AWS_PROFILE}"
    # SSO/temp-cred safe path: bind the operator's ~/.aws read-only (nothing stored,
    # SSO auto-rotates). Only when it exists and no dir was preset.
    if [ -z "$(env_get "${ENV_FILE}" WARDYN_BEDROCK_AWS_DIR)" ]; then
      if [ -n "${WARDYN_BEDROCK_AWS_DIR:-}" ]; then
        env_set "${ENV_FILE}" WARDYN_BEDROCK_AWS_DIR "${WARDYN_BEDROCK_AWS_DIR}"
      elif [ -d "${HOME}/.aws" ]; then
        env_set "${ENV_FILE}" WARDYN_BEDROCK_AWS_DIR "${HOME}/.aws"
        log "Bedrock: wired ~/.aws read-only mount (SSO auto-rotates; nothing stored)."
        [ "$(id -u)" = "1000" ] || warn "Bedrock ~/.aws mount: host uid $(id -u) != sandbox agent uid 1000. If a run can't read your 0600 AWS files, grant the sandbox uid: setfacl -R -m u:1000:rX \"${HOME}/.aws\"."
      fi
    fi
    log "Bedrock: add the API key (preferred, never resident), a session token, or static keys in the UI after launch."
    chmod 600 "${ENV_FILE}" 2>/dev/null || true
  fi
  unset _br_on _br_region

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

  # Headless model-access seed: a Claude subscription token supplied via env is
  # connected through the IN-CONTAINER CLI (loopback → local-mode no-auth, so a
  # host→bridge non-loopback peer never hits the auth gate). Piped on stdin so it
  # never lands in argv/ps, deploy/compose/.env, or the wardynd container env — it
  # lives ONLY in the age-encrypted store. Interactive setup uses
  # `wardyn subscription connect` after launch (or `wardyn setup status`).
  if [ -n "${WARDYN_SUBSCRIPTION_TOKEN:-}" ]; then
    log "Connecting the Wardyn-managed Claude subscription from WARDYN_SUBSCRIPTION_TOKEN…"
    if printf '%s' "${WARDYN_SUBSCRIPTION_TOKEN}" | compose exec -T wardynd /usr/local/bin/wardyn subscription connect --token-stdin; then
      log "Managed Claude subscription connected (injected proxy-side; never resident in the sandbox)."
    else
      warn "subscription connect failed — check the token (from 'claude setup-token', starts with sk-ant-oat)."
    fi
  fi

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
  if docker run --rm --network "${WARDYN_NS:-wardyn}-internal" curlimages/curl:latest \
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
  _me_code=$(docker run --rm --network "${WARDYN_NS:-wardyn}-internal" curlimages/curl:latest \
    -s -m 5 -o /dev/null -w '%{http_code}' "http://wardynd:8080/api/v1/me" 2>/dev/null || echo 000)
  case "${_me_code}" in
    200) log "Local-mode no-auth gate: OK (gated API reachable from a non-loopback peer — WARDYN_LOCAL_TRUST_FORWARDER effective)." ;;
    403) warn "Local-mode no-auth gate REJECTED a non-loopback peer (HTTP 403). The UI/CLI will be locked out — ensure WARDYN_LOCAL_TRUST_FORWARDER=true reached wardynd (docker compose config)." ;;
    *)   warn "Local-mode gate probe inconclusive (HTTP ${_me_code}); check 'docker compose -f ${COMPOSE_FILE} logs wardynd'." ;;
  esac

  # Route hand-off. CI/headless (no browser or no TTY): no browser, NO demos —
  # just the launch command. Interactive (UI/CLI): open Getting-started + point at
  # the keyless demo and a one-command governed run.
  if [ "${WARDYN_UP_NO_BROWSER:-0}" = "1" ] || [ ! -t 1 ]; then
    log "Wardyn is up (headless): ${_url}"
    log "  Ready — launch a governed run:"
    log "    wardyn run --agent claude-code --image ubuntu:24.04 --task-mode exec --task 'echo hi' --policy-file examples/policies/sandbox.yaml --wait"
  else
    open_url "${_url}"
    log "Wardyn is up: ${_url}  (local mode — no login) — the Getting-started page is ready NOW."
    log "  Prove the sandbox boundary from the CLI (keyless):"
    log "    wardyn run --agent claude-code --interactive --policy-file examples/policies/sandbox.yaml"
    log "  Give it a real Claude:  wardyn subscription connect   (then run with sandbox-claude.yaml)"
    log "  Or click the /demos screen in the UI."
  fi

  # The per-run sandbox proxy + agent images are NOT needed to reach the UI or the
  # Getting-started page — only to LAUNCH a run. Build them AFTER the browser is
  # open so first light is as fast as possible; you read Getting-started while these
  # finish. Skip them for a pure UI look with WARDYN_UP_SKIP_RUN_IMAGES=1.
  if [ "${WARDYN_UP_SKIP_RUN_IMAGES:-0}" = "1" ]; then
    log "WARDYN_UP_SKIP_RUN_IMAGES=1 — skipping the run images (build later: make agent-images-core && docker compose -f \"${COMPOSE_FILE}\" --profile build-only build proxy-image)"
  else
    log "Finishing the run components so your first run is ready (sandbox proxy + agent images)…"
    # The proxy sidecar is the SOLE egress path for every run — if it can't build,
    # no run can work, so it stays fatal under set -e.
    compose --profile build-only build proxy-image
    # Agent images are PER-AGENT: one blocked image (e.g. a corp mirror missing an
    # agent's package) must not abort the stack or the other agents. Build each
    # independently, continue on error, and summarize — mirroring the host-mode
    # loop in scripts/setup.sh. The control plane is already up and healthy above,
    # so a failed agent image is a warning, not a teardown. Building via compose so
    # the corp-build args (NPM_REGISTRY/HTTP(S)_PROXY) wired into these stanzas apply.
    _agent_img_warn=0
    for _svc in agent-claude-code agent-codex-cli agent-aws-sso; do
      log "Building ${_svc}…"
      if compose --profile build-only build "${_svc}"; then
        log "  built ${_svc}"
      else
        warn "build failed for ${_svc} — runs naming this agent fail until you rebuild it (docker compose -f ${COMPOSE_FILE} --profile build-only build ${_svc}). Other agents are unaffected."
        _agent_img_warn=1
      fi
    done
    if [ "${_agent_img_warn}" = 1 ]; then
      warn "one or more agent images did not build (see above). The stack is UP; fix the image and rerun its build before launching that agent."
    else
      log "Run components ready — you can launch your first run."
    fi
    unset _agent_img_warn _svc
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
# has run Wardyn before. Irreversible, so it CONFIRMS (default No; headless needs
# WARDYN_FORCE_RESET=1). A live HOST-mode (`make setup`) daemon is offered a stop
# first: the containerized wardynd this brings up binds the same 127.0.0.1:8080,
# so leaving the host one running guarantees a port collision, not a second UI.
# For the FULL undo (host daemon + rundir + compose, no re-up) use reset-all.
cmd_reset() {
  warn "reset REMOVES the compose volumes: Postgres (ALL runs + the append-only audit log) and recordings."
  warn "This is irreversible. Plain \`make compose-down\` keeps them; use that if you only want to stop the stack."
  _host_pid="$(cat "${HOME}/.wardyn/host-wardynd.pid" 2>/dev/null || true)"
  if [ -n "${_host_pid}" ] && kill -0 "${_host_pid}" 2>/dev/null; then
    warn "Host-mode wardynd is running (PID ${_host_pid}) — the containerized wardynd this brings up needs its :8080."
    if _confirm "Stop the host daemon first (make stop-host)?"; then
      make -C "${REPO_ROOT}" stop-host
    else
      warn "Left the host daemon up — the fresh containerized wardynd will fail to bind :8080."
    fi
  fi
  if ! _confirm "Wipe the compose volumes and re-up?"; then
    [ -t 0 ] && { log "Aborted — nothing was removed."; exit 0; }
    exit 2
  fi
  compose down -v --remove-orphans
  log "Volumes removed — bringing up a fresh, empty Wardyn"
  cmd_up
}

# reset-all — the TRUE fresh-install undo: everything `make setup` / `make
# compose-up` created, across BOTH modes (host daemon + compose stack), so an
# iteration loop can start each round from a genuinely clean box. Unlike
# `reset` it does NOT re-up — it leaves the machine clean and stops.
#
# ~/.wardyn is shared real estate (other tools keep source trees and scratch
# there), so removal is a NAMED ALLOWLIST of the files setup.sh /
# stage-claude-creds.sh create — never `rm -rf ~/.wardyn`. Everything else
# found there is reported as PRESERVED.
#
# Kept by default (flags to purge):
#   deploy/compose/.env  (--purge-env)     the persisted age key. Safe to keep:
#                                          `down -v` destroyed every secret
#                                          sealed under it, and the same key
#                                          just seals the next ones. Purge only
#                                          for a pristine first-contact baseline.
#   built :local images  (--purge-images)  the minutes-long rebuild set.
# Built binaries (bin/, ui/dist) are `make clean`'s job — not duplicated here.
_ra_mark() {  # _ra_mark 0|1 LABEL — one manifest line
  if [ "$1" = 1 ]; then printf '  [present] %s\n' "$2"; else printf '  [absent]  %s\n' "$2"; fi
}

cmd_reset_all() {
  _ra_dry=0; _ra_purge_images=0; _ra_purge_env=0
  for _ra_a in "$@"; do
    case "${_ra_a}" in
      --dry-run)      _ra_dry=1 ;;
      --purge-images) _ra_purge_images=1 ;;
      --purge-env)    _ra_purge_env=1 ;;
      *) die "reset-all: unknown flag '${_ra_a}' (known: --dry-run --purge-images --purge-env)" ;;
    esac
  done
  _ra_rundir="${HOME}/.wardyn"
  # :local is the current locally-built tag; the :demo variants are the
  # pre-rename generation still present on boxes that set up before it.
  _ra_images="wardyn/wardynd:local wardyn/wardyn-proxy:local wardyn/agent-claude-code:local wardyn/agent-codex-cli:local wardyn/agent-oracle:local wardyn/wardyn-tetragon-ingest:local wardyn/wardynd:demo wardyn/wardyn-proxy:demo wardyn/agent-claude-code:demo wardyn/agent-codex-cli:demo wardyn/agent-oracle:demo wardyn/wardyn-tetragon-ingest:demo"

  # ── gather facts (read-only) ─────────────────────────────────────────
  _ra_host_pid=$(cat "${_ra_rundir}/host-wardynd.pid" 2>/dev/null || true)
  _ra_host_live=0
  [ -n "${_ra_host_pid}" ] && kill -0 "${_ra_host_pid}" 2>/dev/null && _ra_host_live=1

  _ra_proj=$(compose config 2>/dev/null | awk '/^name:/{print $2; exit}')
  [ -n "${_ra_proj}" ] || _ra_proj=compose
  _ra_containers=$(compose ps -aq 2>/dev/null | grep -c . || true)

  # Enable every profile the file declares (sso, groundtruth, build-only, …) so
  # profile-gated volumes like tetragon_export are seen AND torn down too.
  _ra_profiles=""
  for _ra_p in $(compose config --profiles 2>/dev/null); do
    _ra_profiles="${_ra_profiles} --profile ${_ra_p}"
  done

  # Resolve the EXACT docker volume names this compose file owns (explicit
  # `name:` when set, else <project>_<logical>) — the same set `down -v`
  # removes. A project-label filter is NOT safe here: the label is just the
  # directory name ("compose"), which this repo's pre-rename eras (warden-*/
  # writ-*) share, and reset-all must never claim volumes that aren't its own.
  # `docker compose config --format json` already RESOLVES each volume's real
  # docker name (explicit `name:` when set, else <project>_<logical>, and the
  # bare external name for `external: true`), so read that structured output
  # instead of hand-parsing the YAML render — the old awk scan assumed a fixed
  # 2-space block style and silently mis-derived names for any flow-style or
  # external-volume shape. jq is the shared JSON tool across scripts/ (ci-run.sh,
  # run-e2e-byoi.sh); if jq or `config` is unavailable the preview lists no
  # volumes, but the real `down -v` teardown below is unaffected.
  _ra_volnames=$(compose ${_ra_profiles} config --format json 2>/dev/null \
    | jq -r --arg proj "${_ra_proj}" '(.volumes // {}) | to_entries[] | .value.name // ($proj + "_" + .key)' 2>/dev/null \
    | tr '\n' ' ')
  _ra_volumes=""
  for _ra_v in ${_ra_volnames}; do
    docker volume inspect "${_ra_v}" >/dev/null 2>&1 && _ra_volumes="${_ra_volumes}${_ra_v} "
  done

  _ra_net=0; _ra_net_attached=0
  if docker network inspect wardyn-internal >/dev/null 2>&1; then
    _ra_net=1
    _ra_net_attached=$(docker network inspect -f '{{len .Containers}}' wardyn-internal 2>/dev/null || echo 0)
  fi

  _ra_testpg=0
  docker inspect wardyn-test-pg >/dev/null 2>&1 && _ra_testpg=1

  # ~/.wardyn: split into install files (ours to delete) vs preserved (not ours)
  _ra_install=""; _ra_preserved=""
  if [ -d "${_ra_rundir}" ]; then
    for _ra_e in "${_ra_rundir}"/* "${_ra_rundir}"/.[!.]*; do
      [ -e "${_ra_e}" ] || continue
      case "$(basename "${_ra_e}")" in
        host-wardynd.pid|host-wardynd.log|claude-creds|composer-dev-subscription.json)
          _ra_install="${_ra_install}$(basename "${_ra_e}") " ;;
        *)
          _ra_preserved="${_ra_preserved}$(basename "${_ra_e}") " ;;
      esac
    done
  fi

  _ra_env_present=0
  [ -f "${ENV_FILE}" ] && _ra_env_present=1

  _ra_images_present=""
  for _ra_img in ${_ra_images}; do
    docker image inspect "${_ra_img}" >/dev/null 2>&1 && _ra_images_present="${_ra_images_present}${_ra_img} "
  done

  # ── manifest ─────────────────────────────────────────────────────────
  log "reset-all — full undo of local Wardyn setup (daemon: ${DOCKER_HOST:-default socket}). Manifest:"
  if [ "${_ra_host_live}" = 1 ]; then
    _ra_mark 1 "host-mode wardynd (PID ${_ra_host_pid}, ~/.wardyn/host-wardynd.pid) — will be stopped"
  else
    _ra_mark 0 "host-mode wardynd (no live PID)"
  fi
  _ra_mark "$([ "${_ra_containers:-0}" -gt 0 ] && echo 1 || echo 0)" "compose containers: ${_ra_containers:-0} (project '${_ra_proj}')"
  _ra_mark "$([ -n "${_ra_volumes}" ] && echo 1 || echo 0)" "compose volumes: ${_ra_volumes:-none }(runs + audit + recordings — IRREVERSIBLE)"
  if [ "${_ra_net}" = 1 ]; then
    _ra_mark 1 "docker network wardyn-internal (${_ra_net_attached} attached — removed only if 0 remain after teardown)"
  else
    _ra_mark 0 "docker network wardyn-internal"
  fi
  _ra_mark "${_ra_testpg}" "dev/e2e postgres container wardyn-test-pg (:55432)"
  _ra_mark "$([ -n "${_ra_install}" ] && echo 1 || echo 0)" "~/.wardyn install files: ${_ra_install:-none }(includes STAGED CLAUDE CREDS — re-stage after next setup)"
  [ -n "${_ra_preserved}" ] && printf '  [keep]    ~/.wardyn PRESERVED (not Wardyn setup'\''s): %s\n' "${_ra_preserved}"
  if [ "${_ra_purge_env}" = 1 ]; then
    _ra_mark "${_ra_env_present}" "deploy/compose/.env (age key) — --purge-env"
  else
    printf '  [keep]    deploy/compose/.env (age key; sealed secrets die with the volume, so keeping it is safe — --purge-env for a pristine baseline)\n'
  fi
  if [ "${_ra_purge_images}" = 1 ]; then
    _ra_mark "$([ -n "${_ra_images_present}" ] && echo 1 || echo 0)" "built images: ${_ra_images_present:-none}— --purge-images (minutes to rebuild)"
  else
    printf '  [keep]    built images (%s) — --purge-images to remove; rebuilds take minutes\n' "${_ra_images_present:-none present}"
  fi
  printf '  [note]    built binaries (bin/, ui/dist): use `make clean`\n'

  if [ "${_ra_dry}" = 1 ]; then
    log "Dry run — nothing was touched. After a real reset-all every line above reads [absent]."
    exit 0
  fi

  # ── consent, then act on the facts above ─────────────────────────────
  warn "This removes everything marked [present]: all runs, the audit log, recordings, and staged Claude creds."
  if ! _confirm "Proceed with reset-all?"; then
    [ -t 0 ] && { log "Aborted — nothing was removed."; exit 0; }
    exit 2
  fi

  [ "${_ra_host_live}" = 1 ] && make -C "${REPO_ROOT}" stop-host

  # shellcheck disable=SC2086 — _ra_profiles is a flat flag list by construction
  compose ${_ra_profiles} down -v --remove-orphans \
    || warn "compose down failed (docker unreachable?) — continuing with filesystem cleanup"

  # run-host.sh creates wardyn-internal OUTSIDE compose ownership (the source of
  # setup.sh's "incorrect label" recovery dance) — remove it when nothing is
  # attached so the next setup recreates it cleanly; a busy network is left alone.
  if docker network inspect wardyn-internal >/dev/null 2>&1; then
    docker network rm wardyn-internal >/dev/null 2>&1 \
      || warn "wardyn-internal still has attached containers — left in place"
  fi

  # -v: also drop the anonymous pgdata volume docker auto-created for it
  [ "${_ra_testpg}" = 1 ] && docker rm -f -v wardyn-test-pg >/dev/null 2>&1

  # Allowlist only — never `rm -rf ~/.wardyn` (see comment above).
  rm -f  "${_ra_rundir}/host-wardynd.pid" "${_ra_rundir}/host-wardynd.log"
  rm -rf "${_ra_rundir}/claude-creds"
  rm -f  "${_ra_rundir}/composer-dev-subscription.json"

  [ "${_ra_purge_env}" = 1 ] && rm -f "${ENV_FILE}"

  if [ "${_ra_purge_images}" = 1 ]; then
    for _ra_img in ${_ra_images_present}; do
      docker rmi "${_ra_img}" >/dev/null 2>&1 || warn "could not remove ${_ra_img} (in use?)"
    done
  fi

  log "Clean. Verify: scripts/up.sh reset-all --dry-run   (every line should read [absent])"
  log "Next: make setup (asks; Enter = containerized)  or  make compose-up (containerized, no prompt)"
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
[ $# -gt 0 ] && shift
case "${cmd}" in
  doctor)    cmd_doctor ;;
  up)        cmd_up ;;
  down)      cmd_down ;;
  reset)     cmd_reset ;;
  reset-all) cmd_reset_all "$@" ;;
  pg)        cmd_pg ;;
  *) die "usage: $0 {doctor|up|down|reset|reset-all|pg}" ;;
esac
