#!/bin/sh
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

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
#
# Regression coverage for the pure host-side decisions below (default-policy
# auto-pick/re-pick, the CLI hand-off prefix) lives in scripts/test-up-policy.sh
# (no docker, no network — extracts and exercises the functions directly).
set -eu

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
COMPOSE_FILE="${REPO_ROOT}/deploy/compose/docker-compose.yaml"
ENV_FILE="${REPO_ROOT}/deploy/compose/.env"
ENV_EXAMPLE="${REPO_ROOT}/deploy/compose/.env.example"

. "${REPO_ROOT}/scripts/lib/common.sh"
# reset / reset-all machinery — split into its own sourced sibling to keep
# this file under the file-size gate (scripts/check-file-size.sh); see
# scripts/up-reset.sh for why it's safe to source before REPO_ROOT's other
# consumers (compose(), _confirm*(), cmd_up()) are defined below.
. "${REPO_ROOT}/scripts/up-reset.sh"

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
# wants_llm) upgrades to claude-llm.json, the shipped ceiling that admits the
# api_key grant + LLM egress an AGENT run needs. Without it BOTH demo.json and
# default.json carry only a github_token grant, so clampGrants strips the run's
# model grant and a first agent run boots, "completes", and 404s on its first
# model call. Kept off by default so a pure-Fence trial keeps the tight
# github-token-only ceiling.
pick_policy() {
  if [ "${2:-}" = "1" ]; then
    echo "/examples/policies/claude-llm.json"
    return
  fi
  case "$1" in
    *'"runsc"'*) echo "/examples/policies/default.json" ;;
    *)           echo "/examples/policies/demo.json" ;;
  esac
}

# host_llm_key_present ENV_FILE -> "1" | ""
# "1" when the operator has exported a host LLM API key, i.e. opted into a real
# model path before the stack is even up. THIS PROCESS'S env only — see
# llm_ready_from_status below for the lane this can never see.
#
# This used to also read WARDYN_COMPOSER_CONFIG (a non-"fake" backend counted as
# a real model path). No Go code has read that variable since the AI Run Composer
# was cut; up.sh was writing it into .env and reading it straight back, so the
# whole config branch was talking to itself. The ENV_FILE argument is kept —
# llm_ready_from_status's caller passes it and the key lookup may need it again.
host_llm_key_present() {
  [ -n "${ANTHROPIC_API_KEY:-}" ] && { echo 1; return; }
  [ -n "${OPENAI_API_KEY:-}" ]    && { echo 1; return; }
  echo ""
}

# llm_ready_from_status STATUS_JSON -> "1" | ""
# W1-S1-3: host_llm_key_present is blind to a managed subscription connected in a
# PRIOR `up` (no token re-supplied this run) and to a key added through the
# UI — neither ever touches this process's env. cmd_up instead asks the
# already-running daemon's own GET /api/v1/setup/status, whose llm_ready
# aggregates every lane (subscription, composer backend, secret-name
# heuristic, Bedrock, an AI-provider Integration) for the Getting-started
# readiness banner. Split out as a pure string check (matching
# host_llm_key_present's own case-pattern style) so test-up-policy.sh can pin
# the match on a canned body with no docker/network.
llm_ready_from_status() {
  case "$1" in
    *'"llm_ready":true'*) echo 1 ;;
    *) echo "" ;;
  esac
}

# llm_ready_from_probe STATUS_RAW -> "1" | ""
# STATUS_RAW is curl's "\n%{http_code}"-suffixed response for GET
# /api/v1/setup/status (the encoding cmd_up's post-boot probe emits via
# -w '\n%{http_code}': the body, a newline, then the status line). A non-200 —
# an unauthenticated probe 401ing under SSO because the WARDYN_OIDC_ISSUER
# guard around the call site was skipped or missing, a mid-restart 502, any
# daemon hiccup — must NOT be silently read the same as "no model path yet
# and everyone agrees the ceiling stays put". Only a confirmed 200 body
# reaches llm_ready_from_status; anything else is "no signal", same as an
# unreachable daemon.
llm_ready_from_probe() {
  _lrfp_code=$(printf '%s' "$1" | tail -n1)
  if [ "${_lrfp_code}" = "200" ]; then
    llm_ready_from_status "$(printf '%s' "$1" | sed '$d')"
  else
    echo ""
  fi
  unset _lrfp_code
}

# resolve_default_policy ENV_FILE OVERRIDE RUNTIMES_JSON WANTS_LLM -> policy path
# Decides + PERSISTS WARDYN_DEFAULT_POLICY into ENV_FILE (plus a
# WARDYN_DEFAULT_POLICY_AUTO marker) and echoes the resulting path. OVERRIDE
# is the process-env WARDYN_DEFAULT_POLICY for this invocation ("" if unset):
# an explicit override always wins and clears the marker (operator has now
# spoken) so a later plain `up` won't auto-pick over it. Otherwise ENV_FILE's
# own value is kept UNLESS it is unset or still carries the marker (meaning WE
# chose it last time, not the operator) — in which case pick_policy re-runs.
# That re-pick is what lets a managed subscription or an exported API key
# added AFTER the first `make setup` actually take effect on the next `up`:
# the old "only decide when .env has nothing" rule froze the pure-Fence
# demo.json/default.json ceiling forever once written, and a composed run kept
# 404ing on its first model call even after a real model path showed up.
# Pre-marker installs (a value with no marker at all) read as NOT-ours, same
# as a hand-set override.
resolve_default_policy() {
  _rdp_file=$1; _rdp_override=$2; _rdp_runtimes=$3; _rdp_wants_llm=$4
  if [ -n "${_rdp_override}" ]; then
    env_set "${_rdp_file}" WARDYN_DEFAULT_POLICY_AUTO 0
    env_set "${_rdp_file}" WARDYN_DEFAULT_POLICY "${_rdp_override}"
    echo "${_rdp_override}"
    unset _rdp_file _rdp_override _rdp_runtimes _rdp_wants_llm
    return
  fi
  _rdp_cur=$(env_get "${_rdp_file}" WARDYN_DEFAULT_POLICY)
  if [ -z "${_rdp_cur}" ] || [ "$(env_get "${_rdp_file}" WARDYN_DEFAULT_POLICY_AUTO)" = "1" ]; then
    _rdp_cur=$(pick_policy "${_rdp_runtimes}" "${_rdp_wants_llm}")
    env_set "${_rdp_file}" WARDYN_DEFAULT_POLICY_AUTO 1
  fi
  env_set "${_rdp_file}" WARDYN_DEFAULT_POLICY "${_rdp_cur}"
  echo "${_rdp_cur}"
  unset _rdp_file _rdp_override _rdp_runtimes _rdp_wants_llm _rdp_cur
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

# host_goarch — this host's GOARCH, for cross-compiling the host-native `wardyn`
# the image builds (see Dockerfile.wardynd). Defaults to amd64 on anything
# unrecognized: a wrong guess only costs host-side proxy detection, which is
# best-effort and degrades to the container's own honest "couldn't look there".
host_goarch() {
  case "$(uname -m 2>/dev/null || echo unknown)" in
    x86_64|amd64)  echo amd64 ;;
    arm64|aarch64) echo arm64 ;;
    *)             echo amd64 ;;
  esac
}

# seed_host_proxy — run the host-proxy detector ON THE HOST and stash the result
# for the containerized wardynd. Inside the container every tier is blind (the OS
# tier dispatches on the process's GOOS; git needs a binary distroless lacks; HOME
# is unset so no shell profile or tool config is reachable), so the wizard would
# otherwise assert a false negative on exactly the corp hosts that have a proxy.
#
# Wholly best-effort: every step tolerates failure and leaves the seed unset, in
# which case the wizard says "detection ran in a container and couldn't look at
# your host" instead of "nothing is configured". Rewritten on every `up`, so it
# can never go stale across a network change. base64 because compose interpolates
# `$` inside .env values; the payload is credential-masked before it is emitted.
#
# Also leaves a working host-native CLI at bin/wardyn (same convention as
# scripts/setup.sh's host-mode `go build -o bin/wardyn`) for cmd_up's hand-off
# to point at — the containerized path installs no `wardyn` on PATH otherwise.
seed_host_proxy() {
  _shp_bin="${REPO_ROOT}/bin/wardyn"
  mkdir -p "${REPO_ROOT}/bin" 2>/dev/null || return 0
  # distroless has no shell, so `docker run … cat` can't work — create, cp, rm.
  _shp_cid=$(docker create wardyn/wardynd:local 2>/dev/null) || return 0
  docker cp "${_shp_cid}:/host/wardyn" "${_shp_bin}" >/dev/null 2>&1 || true
  docker rm -f "${_shp_cid}" >/dev/null 2>&1 || true
  [ -x "${_shp_bin}" ] || { unset _shp_bin _shp_cid; return 0; }
  _shp_json=$("${_shp_bin}" setup detect-proxy 2>/dev/null) || _shp_json=""
  if [ -n "${_shp_json}" ]; then
    _shp_b64=$(printf '%s' "${_shp_json}" | base64 2>/dev/null | tr -d '\n') || _shp_b64=""
    if [ -n "${_shp_b64}" ]; then
      env_set "${ENV_FILE}" WARDYN_HOST_PROXY_B64 "${_shp_b64}"
      log "Host proxy detection seeded from this host (the container can't read your shell/OS/PAC settings)."
    fi
  fi
  unset _shp_bin _shp_cid _shp_json _shp_b64
}

# wardyn_cli_prefix REPO_ROOT_DIR -> invocation prefix for example commands.
# seed_host_proxy extracts a working host-native `wardyn` to REPO_ROOT_DIR/bin
# (same convention as scripts/setup.sh's host-mode `go build -o bin/wardyn`);
# use it when present. Otherwise fall back to a bare `wardyn` (assumed on
# PATH) — the caller is expected to also print the `go install` recovery line.
wardyn_cli_prefix() {
  if [ -x "$1/bin/wardyn" ]; then
    echo "./bin/wardyn"
  else
    echo "wardyn"
  fi
}

# wardynd_probe ENV_FILE PATH -> "<body>\n<http_code>" (llm_ready_from_probe's
# STATUS_RAW encoding). THE one way cmd_up asks the running wardynd a question.
#
# wardynd is distroless (no curl, no shell), so the question goes through a
# throwaway curl container on the compose network — which makes it arrive from
# a NON-loopback peer, exactly like a real host UI/CLI request arriving via the
# docker bridge gateway (docker-compose.yaml's WARDYN_LOCAL_TRUST_FORWARDER
# note). Two headers are what make such a request answerable at all:
#
#   Host: 127.0.0.1:8080 — local mode's DNS-rebinding guard (isLoopbackHost,
#     internal/api/http.go) 403s any non-loopback Host, and Docker DNS forces
#     the URL authority to "wardynd:8080". A real host request carries the
#     browser's/CLI's own loopback Host, so this REPRODUCES the real shape
#     rather than faking a pass — the PEER gate, the only thing these probes
#     exist to test, is still exercised in full and a 403 still means what the
#     call sites say. Without it every probe 403'd on EVERY `make setup`.
#   Authorization: Bearer — /api/v1/me and /api/v1/setup/status are in the
#     humanOrAdminAuth group, which 401s an unauthenticated call whenever local
#     mode is off; local mode returns before the bearer check, so ONE shape
#     answers both postures. Fallback mirrors compose's :-demo-admin-token.
#
# NOT `2>/dev/null`: `-m 5` bounds the curl, not the image pull docker does first
# and reports ONLY on stderr — discarded, a slow or failed acquisition reached the
# call sites as a bare `000` they then blamed wardynd for. curl is `-s` anyway.
# Pinned by scripts/test-up-probes.sh + cmd/wardynd/up_sh_oidc_probe_guard_test.go.
wardynd_probe() {
  _wp_env=$1; _wp_path=$2
  _wp_tok=$(env_get "${_wp_env}" WARDYN_ADMIN_TOKEN)
  docker run --rm --network "${WARDYN_NS:-wardyn}-internal" curlimages/curl:latest \
    -s -m 5 -w '\n%{http_code}' \
    -H "Host: 127.0.0.1:8080" \
    -H "Authorization: Bearer ${_wp_tok:-demo-admin-token}" \
    "http://wardynd:8080${_wp_path}" || echo 000
  unset _wp_env _wp_path _wp_tok
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

# env_get/env_set moved to scripts/lib/common.sh (contracts documented there) —
# setup.sh's front-door workspaces-root prompt persists through the same helpers.

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

# _confirm_host_stop PROMPT — like _confirm, but gated on the SEPARATE
# WARDYN_FORCE_STOP_HOST flag, never WARDYN_FORCE_RESET: `reset` only wipes the
# compose volumes by contract (TRY-IT.md's "does not touch a host-mode
# daemon"), so a headless `WARDYN_FORCE_RESET=1 make reset` confirming the
# (unrelated) volume wipe must not ALSO silently kill a live host-mode wardynd
# — that needs its own explicit opt-in. Interactive behavior is identical to
# _confirm (prompt, default No).
_confirm_host_stop() {
  [ "${WARDYN_FORCE_STOP_HOST:-}" = 1 ] && return 0
  if [ -t 0 ]; then
    printf '  %s [y/N] ' "$1"
    read -r _c_a || _c_a=""
    case "${_c_a}" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
  fi
  warn "Non-interactive: set WARDYN_FORCE_STOP_HOST=1 to also stop it headlessly."
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

# sock_mountable SOCK — true iff the DAEMON can see a socket at SOCK (the in-VM
# path case, e.g. Rancher Desktop, where the host has no such file).
# Mounts SOCK's PARENT read-only and tests the leaf inside it. Naming a missing
# SOCK as the bind source instead — what this probe used to do — makes docker
# materialize it as a root-owned DIRECTORY there, breaking doctor's
# "creates/changes nothing" contract. Skipped when the parent is missing too.
sock_mountable() {
  _sm_dir=$(dirname "$1")
  [ -d "${_sm_dir}" ] && docker run --rm --pull=never -v "${_sm_dir}:/probe:ro" \
    alpine:3.20 test -S "/probe/$(basename "$1")" >/dev/null 2>&1
}

cmd_doctor() {
  DOCTOR_BLOCKED=0
  # "read-only" means doctor changes nothing about this host's Wardyn setup —
  # no volume, container, image or config of yours is created or touched. The
  # ONE thing it runs is the socket-mountability probe below: a throwaway
  # `alpine:3.20 test -S`, --pull=never so it can never reach the network or
  # write to your image store, skipped entirely when that image isn't already
  # local. Keep that flag: without it, `make doctor` on a fresh box silently
  # pulled an image, which is exactly the surprise the claim rules out. And see
  # sock_mountable: never name a MISSING path as a bind source — docker
  # materializes an absent source as a root-owned directory.
  log "Wardyn doctor — read-only preflight (nothing on this host is created or changed)"

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
  _pg_port="${WARDYN_PG_PORT:-5432}"
  if port_in_use "${_pg_port}"; then
    report warn "port ${_pg_port} already in use — postgres may fail to bind (an existing wardyn-postgres container already holding it is fine). Override with WARDYN_PG_PORT=<port>, or free the port."
  else
    report ok "port ${_pg_port} free."
  fi
  # registry: auto-started via postgres/dex/wardynd's depends_on, so it binds
  # even on a plain `make setup` — not opt-in like the SSO/groundtruth profiles.
  _registry_port="${WARDYN_REGISTRY_PORT:-5010}"
  if port_in_use "${_registry_port}"; then
    report warn "port ${_registry_port} already in use — the devcontainer-build registry may fail to bind. Override with WARDYN_REGISTRY_PORT=<port>, or free the port."
  else
    report ok "port ${_registry_port} free."
  fi
  # wardynd's SSH gateway mapping is always published in compose, whether or
  # not WARDYN_SSH_LISTEN is set to actually enable the gateway.
  _ssh_port="${WARDYN_SSH_PORT:-2222}"
  if port_in_use "${_ssh_port}"; then
    report warn "port ${_ssh_port} already in use — wardynd's SSH gateway mapping may fail to bind. Override with WARDYN_SSH_PORT=<port>, or free the port."
  else
    report ok "port ${_ssh_port} free."
  fi
  # Same story for the UI-sandbox gateway mapping (docs/UI-SANDBOXES.md).
  _ui_sandbox_port="${WARDYN_UI_SANDBOX_PORT:-8081}"
  if port_in_use "${_ui_sandbox_port}"; then
    report warn "port ${_ui_sandbox_port} already in use — wardynd's UI-sandbox gateway mapping may fail to bind. Override with WARDYN_UI_SANDBOX_PORT=<port>, or free the port."
  else
    report ok "port ${_ui_sandbox_port} free."
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
    elif docker image inspect alpine:3.20 >/dev/null 2>&1 && sock_mountable "${_sock}"; then
      report ok "docker socket ${_sock} is bind-mountable by the daemon (in-VM path, e.g. Rancher Desktop)."
    else
      report warn "chosen docker socket ${_sock} is not present on the host, and the in-VM mountability probe did not confirm it (it is skipped rather than pulled when alpine:3.20 isn't already local — \`docker pull alpine:3.20\` and re-run to test it). The compose wardynd may not be able to create sandboxes. On Rancher Desktop set WARDYN_DOCKER_SOCK=/var/run/docker.sock (the in-VM path); otherwise check the path and permissions."
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

  # Target the host-native `wardyn` the image also builds, for host-side proxy
  # detection after boot (seed_host_proxy). Must be set BEFORE the build.
  WARDYN_HOST_GOARCH="${WARDYN_HOST_GOARCH:-$(host_goarch)}"
  case "$(os_kind)" in
    darwin) WARDYN_HOST_GOOS="${WARDYN_HOST_GOOS:-darwin}" ;;
    *)      WARDYN_HOST_GOOS="${WARDYN_HOST_GOOS:-linux}" ;;   # wsl -> linux
  esac
  export WARDYN_HOST_GOOS WARDYN_HOST_GOARCH

  # PULL FIRST. Wardyn publishes signed, SBOM-attested images for every release
  # (docs/VERIFY.md), so a fresh install should not have to compile Go and build a
  # UI bundle to see the product. We pull them and retag to the :local names the
  # compose file already uses, which leaves every build path below untouched.
  #
  # Fallback rather than a flag day: if ANY image is missing — a release whose
  # images have not been pushed yet, an air-gapped host, a registry an operator
  # cannot reach, or a working tree ahead of the last tag — we build exactly as
  # before. That is why this is a pull-first path and not a pull-only default: a
  # default that breaks between the version bump and the image push would be a
  # worse trade than a slower first run.
  #
  # WARDYN_BUILD_LOCAL=1 skips the pull outright (contributors testing their own
  # changes must never silently run a published binary instead).
  pulled_all=false
  if [ "${WARDYN_BUILD_LOCAL:-}" != 1 ] && [ "${WARDYN_BUILD_LOCAL:-}" != true ]; then
    ver="$(grep -oE 'Version = "[^"]+"' "${REPO_ROOT}/internal/version/version.go" | head -1 | cut -d'"' -f2 || true)"
    if [ -n "$ver" ]; then
      log "Pulling published images for ${ver} (set WARDYN_BUILD_LOCAL=1 to build from source instead)"
      pulled_all=true
      for pair in "wardynd:wardyn/wardynd:local" \
                  "wardyn-proxy:wardyn/wardyn-proxy:local" \
                  "agent-base:wardyn/agent-base:local" \
                  "agent-codex-cli:wardyn/agent-codex-cli:local" \
                  "agent-aws-sso:wardyn/agent-aws-sso:local"; do
        remote="ghcr.io/cjohnstoniv/${pair%%:*}:${ver}"
        localref="${pair#*:}"
        if docker pull -q "$remote" >/dev/null 2>&1 && docker tag "$remote" "$localref"; then
          continue
        fi
        warn "could not pull ${remote} — building from source instead."
        pulled_all=false
        break
      done
    fi
  fi
  if $pulled_all; then
    log "Using published images (cosign-signed, SBOM-attested — see docs/VERIFY.md). Skipped the local build."
  else

  log "Building the wardynd image (serves the REST API + embedded UI)"
  # ponytail: retry, don't predict. A corp allowlist mirror 404s the pnpm TARBALL,
  # which is only observable from inside the build — a registry probe hits the
  # metadata path, gets 200, and the build dies anyway (deploy/images/README.md).
  # Retrying with a host-built ui/dist recovers it with no probe and covers every
  # ui-build failure mode. The OSS path never enters this branch.
  if ! compose build; then
    [ -z "${WARDYN_UI_STAGE:-}" ] \
      || die "wardynd image build failed with WARDYN_UI_STAGE=${WARDYN_UI_STAGE} (the failing step is above)."
    command -v pnpm >/dev/null 2>&1 \
      || die "wardynd image build failed (the failing step is above). If it died at 'npm install -g pnpm' with a 404/403, your registry can't serve pnpm: build the UI where it can, then re-run with 'WARDYN_UI_STAGE=ui-prebuilt make setup' — see deploy/images/README.md."
    warn "wardynd image build failed — retrying with the UI built on THIS host (WARDYN_UI_STAGE=ui-prebuilt), the documented recovery for a registry that can't serve pnpm."
    # Reuse an already-complete node_modules instead of reinstalling. A mirror
    # that serves ordinary packages can still 403 the PLATFORM-specific binaries
    # a reinstall's postinstall scripts fetch (this lockfile fans out
    # @tailwindcss/oxide-* and @esbuild/* per platform), which killed this very
    # fallback for a 0.4.1 adopter even though node_modules was already good and
    # `pnpm build` alone succeeds.
    #
    # The guard compares the lockfile against pnpm's own copy of the one it
    # installed from, so it fails safe: a drifted lockfile OR an absent
    # node_modules both fall through to the full install (today's behavior).
    # vite's emptyOutDir means `pnpm build` cannot ship a stale bundle.
    #
    # ponytail: this only helps the SECOND `make setup` — a fresh clone has no
    # node_modules and still takes the install path.
    if cmp -s "${REPO_ROOT}/ui/pnpm-lock.yaml" "${REPO_ROOT}/ui/node_modules/.pnpm/lock.yaml" \
       && ( cd "${REPO_ROOT}/ui" && pnpm build ); then
      log "Rebuilt ui/dist from the existing node_modules (skipped the reinstall)."
    else
      make -C "${REPO_ROOT}" ui \
        || die "the host UI build failed too. If it died fetching a platform-specific package (e.g. '@scope/<os>-<arch>' 403/404), your mirror serves the package but not its per-platform binaries — install those, or copy a populated ui/node_modules from a machine that can, then re-run: pnpm -C ui build && WARDYN_UI_STAGE=ui-prebuilt make setup. Please report the exact package + URL so we can close this properly."
    fi
    WARDYN_UI_STAGE=ui-prebuilt; export WARDYN_UI_STAGE
    compose build \
      || die "still failing with WARDYN_UI_STAGE=ui-prebuilt (the failing step is above). If the error is x509, stage your corp root CA at deploy/images/corp-ca.pem — see deploy/images/README.md."
    log "Recovered: built the UI on this host and reused it (WARDYN_UI_STAGE=ui-prebuilt)."
  fi
  fi  # end: pulled_all fallback

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

  # OIDC-clobber guard: env_set is a hard overwrite, so unconditionally forcing
  # WARDYN_LOCAL_MODE=true + WARDYN_OIDC_ISSUER="" here would silently disable an
  # operator's OIDC operator/viewer split on EVERY routine `make setup` re-run. Only
  # force local no-auth on a fresh install (no issuer set); an existing SSO config is
  # preserved untouched. (A commented example line reads as unset — env_get ignores it.)
  if [ -n "$(env_get "${ENV_FILE}" WARDYN_OIDC_ISSUER)" ]; then
    log "Preserving existing OIDC config (WARDYN_OIDC_ISSUER is set) — leaving auth in SSO mode, not forcing local no-auth."
    # An issuer alone is not enough: a pre-SSO `make setup` wrote LOCAL_MODE=true
    # into this same .env, and local mode SHORT-CIRCUITS auth entirely (no login,
    # for anyone) regardless of the issuer. Warn — don't auto-flip, which could
    # lock the operator out if the IdP is down.
    if [ "$(env_get "${ENV_FILE}" WARDYN_LOCAL_MODE)" = "true" ]; then
      warn "WARDYN_LOCAL_MODE=true is still set in ${ENV_FILE} — it bypasses SSO entirely (no login required). Set it false to actually enforce the OIDC operator/viewer split."
    fi
  else
    env_set "${ENV_FILE}" WARDYN_LOCAL_MODE true
    env_set "${ENV_FILE}" WARDYN_OIDC_ISSUER ""
  fi
  # LocalMode no-auth requires a loopback request PEER; in compose the peer is the
  # docker gateway (port is published loopback-only, so LAN peers can't reach it).
  # Trust the forwarder so the host UI/CLI isn't 403'd. Safe only with the 127.0.0.1
  # publish this stack uses (see docker-compose.yaml). Inert under SSO (local-mode-only).
  env_set "${ENV_FILE}" WARDYN_LOCAL_TRUST_FORWARDER true

  _prev_policy=$(env_get "${ENV_FILE}" WARDYN_DEFAULT_POLICY)
  _runtimes=$(docker info --format '{{json .Runtimes}}' 2>/dev/null || echo '{}')
  _policy=$(resolve_default_policy "${ENV_FILE}" "${WARDYN_DEFAULT_POLICY:-}" "${_runtimes}" "$(host_llm_key_present "${ENV_FILE}")")
  if [ "${_policy}" != "${_prev_policy}" ]; then
    log "Auto-picked default policy: ${_policy}"
    case "${_policy}" in
      */claude-llm.json)
        log "  (LLM-capable ceiling — a real model path is configured; an agent run can reach its model)" ;;
    esac
  fi
  unset _prev_policy

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

  # Must precede `compose up`: the seed is read from the wardynd container's env
  # at boot (docker-compose.yaml WARDYN_HOST_PROXY_B64).
  seed_host_proxy
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
  # -m 3 (the shape the WSL-reachability probe below uses): a peer that ACCEPTS
  # and never replies hangs an unbounded curl forever, so the 60-try budget
  # never expires and `make setup` waits with no ceiling at all.
  until { [ -n "${_cid}" ] && [ "$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${_cid}" 2>/dev/null)" = "healthy" ]; } \
        || curl -fsS -m 3 "${_url}/healthz" >/dev/null 2>&1; do
    _tries=$((_tries + 1))
    if [ "${_tries}" -gt 60 ]; then
      compose logs --tail 50 wardynd
      die "wardynd did not become healthy — see logs above (or: docker compose -f ${COMPOSE_FILE} logs wardynd)"
    fi
    sleep 2
    # The capture above ran right after `compose up -d`; on a slow daemon it can
    # come back empty, wedging this loop on the host-curl arm. Re-read until it
    # lands.
    [ -n "${_cid}" ] || _cid=$(compose ps -q wardynd 2>/dev/null || true)
  done
  log "wardynd is healthy"

  # Headless model-access seed: a Claude subscription token supplied via env is
  # connected through the IN-CONTAINER CLI (loopback → local-mode no-auth, so a
  # host→bridge non-loopback peer never hits the auth gate). Piped on stdin so it
  # never lands in argv/ps, deploy/compose/.env, or the wardynd container env — it
  # lives ONLY in the age-encrypted store. Interactive setup uses
  # `wardyn subscription connect` after launch (or `wardyn setup status`).
  # Connecting a shared subscription from an env var during `up` is the
  # pre-provisioning path the posture rule exists to remove: the daemon only
  # resolves such a credential in a single-user posture, and this stack runs a
  # shared admin token. Say so instead of connecting something that will not
  # resolve — a silent no-op reads as "model access is configured" until the
  # first run fails.
  if [ -n "${WARDYN_SUBSCRIPTION_TOKEN:-}" ]; then
    warn "WARDYN_SUBSCRIPTION_TOKEN is ignored: a shared subscription credential is limited to single-user desktop deployments."
    warn "  Connect it in the console instead (Settings -> Model provider -> Claude subscription), or use an API key / Bedrock."
    warn "  Genuinely a single-user box? Set WARDYN_ALLOW_SHARED_SUBSCRIPTION=true in deploy/compose/.env and re-run."
  fi

  # W1-S1-3: the pick above ran BEFORE wardynd existed — host_llm_key_present can
  # only see THIS process's env (*_API_KEY), never a
  # managed subscription (just connected above, OR left over from a PRIOR `up`
  # with no token re-supplied this time) or a key added through the UI in a
  # browser session up.sh never sees. Ask the daemon itself instead, reachable
  # the same way the local-mode gate smoke below already proves: an in-network
  # peer under WARDYN_LOCAL_TRUST_FORWARDER, no token needed in local mode.
  #
  # SSO/OIDC guard (same masking class as the local-mode gate smoke below):
  # /api/v1/setup/status sits in the humanOrAdminAuth group, so under SSO
  # (WARDYN_OIDC_ISSUER set, see the OIDC-clobber guard above) an unauthenticated
  # in-network curl 401s. Uncaptured, that 401 body fails llm_ready_from_status's
  # match same as an empty one — the re-pick silently no-ops with no signal that
  # anything went wrong. Skip it and say why instead, and warn on a non-200 in
  # local mode so a real daemon problem doesn't masquerade as "no model path yet".
  if [ -n "$(env_get "${ENV_FILE}" WARDYN_OIDC_ISSUER)" ]; then
    log "Post-boot LLM-ready re-pick: skipped (WARDYN_OIDC_ISSUER is set — SSO mode, /api/v1/setup/status correctly requires a real session)."
  else
    # wardynd_probe yields the body AND the status in one round trip
    # (llm_ready_from_probe's STATUS_RAW encoding), so "no model path yet" is
    # distinguishable from "couldn't ask" — and, because it sends the loopback
    # Host + bearer the gates require, a healthy stack now answers 200 instead
    # of the 403/401 that made this re-pick dead code in every posture.
    _status_raw=$(wardynd_probe "${ENV_FILE}" /api/v1/setup/status)
    _status_code=$(printf '%s' "${_status_raw}" | tail -n1)
    [ "${_status_code}" = "200" ] \
      || warn "post-boot LLM-ready probe got HTTP ${_status_code} from /api/v1/setup/status — skipping the policy re-pick this run (a managed subscription or UI-added key won't take effect until the next \`up\`); check 'docker compose -f ${COMPOSE_FILE} logs wardynd'."
    _llm_ready=$(llm_ready_from_probe "${_status_raw}")
    unset _status_raw _status_code
    if [ -n "${_llm_ready}" ]; then
      _prev_policy=$(env_get "${ENV_FILE}" WARDYN_DEFAULT_POLICY)
      _post_runtimes=$(docker info --format '{{json .Runtimes}}' 2>/dev/null || echo '{}')
      _post_policy=$(resolve_default_policy "${ENV_FILE}" "" "${_post_runtimes}" 1)
      if [ "${_post_policy}" != "${_prev_policy}" ]; then
        log "Re-picked default policy now that a model path is live: ${_post_policy} — restarting wardynd"
        compose up -d wardynd
      fi
      unset _prev_policy _post_runtimes _post_policy
    fi
    unset _llm_ready
  fi

  # Can THIS shell reach the published UI port? In WSL2 NAT mode it usually
  # cannot (only the Windows browser can) — an honest note, not a failure.
  if ! curl -fsS -m 3 "${_url}/healthz" >/dev/null 2>&1; then
    warn "the UI at ${_url} is reachable from your Windows browser but not from this WSL shell (Docker Desktop + WSL2 NAT). CLI calls from WSL won't hit it; enable WSL2 mirrored networking if you want shell access too."
  fi

  # Sandbox → control-plane reachability CONFIRMATION (the inverse of the
  # host-mode warning setup.sh prints). In this compose path wardynd runs as a
  # container on wardyn-internal, so a run's proxy sidecar reaches it at
  # http://wardynd:8080 over Docker DNS with NO host/NAT hop — which is what
  # lets workspace VERIFY report its result (the exact thing that can't work on
  # Docker Desktop + WSL2 when wardynd runs host-mode). Prove it with a
  # throwaway container on the same network; never fatal.
  if [ "$(wardynd_probe "${ENV_FILE}" /healthz | tail -n1)" = "200" ]; then
    log "Sandbox → control-plane reachability: OK — workspace recordings and confined replays will complete on this instance."
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
  #
  # SSO/OIDC guard: this probe only means something in LOCAL_MODE — under SSO
  # (WARDYN_OIDC_ISSUER set, see the OIDC-clobber guard above) /api/v1/me needs a
  # real OIDC session, so an unauthenticated in-network curl 401s regardless of
  # WARDYN_LOCAL_TRUST_FORWARDER. Left unguarded, that 401 fell into the generic
  # "inconclusive, check logs" branch below and sent an SSO operator chasing a
  # forwarder problem that doesn't exist. Skip it and say why instead.
  if [ -n "$(env_get "${ENV_FILE}" WARDYN_OIDC_ISSUER)" ]; then
    log "Local-mode no-auth gate: skipped (WARDYN_OIDC_ISSUER is set — SSO mode, not local-mode no-auth; /api/v1/me correctly requires a real session)."
  else
    _me_code=$(wardynd_probe "${ENV_FILE}" /api/v1/me | tail -n1)
    case "${_me_code}" in
      200) log "Local-mode no-auth gate: OK (gated API reachable from a non-loopback peer — WARDYN_LOCAL_TRUST_FORWARDER effective)." ;;
      403) warn "Local-mode no-auth gate REJECTED a non-loopback peer (HTTP 403). The UI/CLI will be locked out — ensure WARDYN_LOCAL_TRUST_FORWARDER=true reached wardynd (docker compose config)." ;;
      401) warn "Local-mode no-auth gate probe was asked for a credential (HTTP 401): wardynd booted with WARDYN_LOCAL_MODE off, and the WARDYN_ADMIN_TOKEN in ${ENV_FILE} is not the one it accepted. Re-run \`make setup\` (or reconcile the token) — the UI/CLI will be asked to log in." ;;
      *)   warn "Local-mode gate probe inconclusive (HTTP ${_me_code}); check 'docker compose -f ${COMPOSE_FILE} logs wardynd'." ;;
    esac
  fi

  _cli=$(wardyn_cli_prefix "${REPO_ROOT}")

  # Route hand-off. CI/headless (no browser or no TTY): no browser, NO demos —
  # just the launch command. Interactive (UI/CLI): open Getting-started + point at
  # the keyless demo and a one-command governed run.
  if [ "${WARDYN_UP_NO_BROWSER:-0}" = "1" ] || [ ! -t 1 ]; then
    log "Wardyn is up (headless): ${_url}"
    log "  Ready — launch a governed run:"
    log "    ${_cli} run --agent claude-code --image ubuntu:24.04 --task-mode exec --task 'echo hi' --policy-file examples/policies/sandbox.yaml --wait"
  else
    open_url "${_url}"
    log "Wardyn is up: ${_url}  (local mode — no login) — the Getting-started page is ready NOW."
    log "  Prove the sandbox boundary from the CLI (keyless):"
    log "    ${_cli} run --agent claude-code --interactive --policy-file examples/policies/sandbox.yaml"
    log "  Give it a real Claude:  ${_cli} subscription connect   (then run with sandbox-claude.yaml)"
    log "  Or open Getting Started in the UI — the demo steps live there."
  fi
  [ "${_cli}" = "wardyn" ] && log "  (bin/wardyn wasn't extracted — build one: go install github.com/cjohnstoniv/wardyn/cmd/wardyn@latest)"
  unset _cli

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
    for _svc in agent-base agent-claude-code agent-codex-cli agent-aws-sso; do
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

# reset / reset-all: see scripts/up-reset.sh (sourced above).

cmd_pg() {
  # THE dev/e2e Postgres bring-up: .github/workflows/ci.yml's "Start Postgres"
  # step runs `make dev-pg`, i.e. this function, so CI and the dev loop cannot
  # drift (they did — 16 here vs 17 there — for a month). Keep the image at the
  # bare `postgres:17` deploy/compose ships; this container is a throwaway
  # (`reset-all` rm -f's it), so it does not carry compose's digest pin.
  # Adds idempotent reuse and the wardyn_e2e database e2e-backend.sh expects, so
  # the dev/e2e loop can self-provision instead of dying on a missing container.
  if docker inspect wardyn-test-pg >/dev/null 2>&1; then
    log "wardyn-test-pg already exists — ensuring it's running"
    docker start wardyn-test-pg >/dev/null 2>&1 || true
  else
    log "Starting wardyn-test-pg (dev/e2e Postgres) on :55432"
    docker run -d --name wardyn-test-pg \
      -e POSTGRES_PASSWORD=wardyn -e POSTGRES_USER=wardyn -e POSTGRES_DB=wardyn \
      -p 55432:5432 postgres:17
  fi

  _tries=0
  until docker exec wardyn-test-pg pg_isready -U wardyn >/dev/null 2>&1; do
    _tries=$((_tries + 1))
    [ "${_tries}" -gt 30 ] && die "wardyn-test-pg did not become ready on :55432"
    sleep 1
  done

  # scripts/e2e-backend.sh's default DB, precreated so `make dev-pg` alone is
  # enough for CI's seeder. A non-default WARDYN_E2E_PG_DBNAME is created by
  # e2e-backend.sh cmd_up itself.
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
