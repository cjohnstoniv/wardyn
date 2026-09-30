# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/lib/up-doctor.sh — `scripts/up.sh doctor`: the read-only host
# preflight (report/DOCTOR_BLOCKED, the socket-mountability probe, cmd_doctor)
# and nothing else. Split out of up.sh purely to keep that file under the
# repo's file-size gate (scripts/check-file-size.sh), exactly as
# scripts/up-reset.sh was — sourced by up.sh, never run standalone.
#
# Depends on names defined in up.sh itself (sourced before any function here is
# CALLED, which is all a POSIX-sourced file needs): REPO_ROOT, COMPOSE_FILE,
# ENV_FILE, port_in_use(), host_llm_key_present(), pick_policy(), plus
# log/warn/die/os_kind/env_get from scripts/lib/common.sh.
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

  # Model access lives under Settings -> Model providers (each person connects
  # their own credential there). The boot-time model-lane variables were retired
  # in 0.8.2 and wardynd REFUSES them; flag any still set here, in the
  # environment or in deploy/compose/.env, before they surface as a boot refusal.
  _retired=$( { env; [ -f "${ENV_FILE}" ] && cat "${ENV_FILE}"; } 2>/dev/null \
    | grep -E '^WARDYN_((ANTHROPIC|OPENAI|BEDROCK)_[A-Z_]+|AGENT_ANTHROPIC_MODEL|SUBSCRIPTION_INJECT|ALLOW_SHARED_SUBSCRIPTION)=' \
    | grep -Eiv '=(false|off)?$' | cut -d= -f1 | sort -u | tr '\n' ' ' || true)
  if [ -n "${_retired}" ]; then
    report warn "retired model-access variable(s) set: ${_retired}— remove them; wardynd refuses them at boot. Configure model access under Settings -> Model providers instead."
  fi
  unset _retired

  if [ "${DOCTOR_BLOCKED}" -eq 1 ]; then
    printf '\n' >&2
    printf '\033[1;31m[error]\033[0m %s\n' "doctor found blocking issue(s) above — fix them, then re-run \`make doctor\`." >&2
    exit 2
  fi
  log "doctor: no blocking issues (see warnings above, if any)."
}

