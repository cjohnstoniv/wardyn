#!/usr/bin/env bash
# Rootless Podman divergence probe (Doc-2 Gap 4).
#
# The docker runner speaks the Docker Engine API via client.FromEnv; Podman's
# Docker-compat REST API is a PARTIAL emulation, so rootless Podman is a SEPARATE
# proof from rootless Docker. This probe points the runner's API at a rootless
# Podman socket and checks the specific fields the runner depends on, so a
# divergence is a reported fact, not an assumption.
#
# Prereqs (need root to install; this box lacks them by default — see the [FAIL]
# hints): podman, the rootless helpers (uidmap: newuidmap/newgidmap), crun/runc,
# fuse-overlayfs, slirp4netns, and /etc/subuid+/etc/subgid for your user.
#
#   Enable the rootless Podman API socket, then run this:
#     systemctl --user enable --now podman.socket
#     DOCKER_HOST="unix://${XDG_RUNTIME_DIR}/podman/podman.sock" scripts/test-podman.sh
#
# Exit 0 = every runner-critical field is present/compatible; non-zero = at least
# one divergence to document before relying on rootless Podman.
set -uo pipefail

FAILED=0
note() { printf '  %s %s\n' "$1" "$2"; }
pass() { note "[pass]" "$1"; }
warn() { note "[warn]" "$1"; }
diverge() { note "[DIVERGE]" "$1"; FAILED=1; }
miss() { note "[MISSING]" "$1"; FAILED=1; }

command -v podman >/dev/null 2>&1 || { miss "podman not installed — sudo apt-get install -y podman uidmap crun fuse-overlayfs slirp4netns"; }
command -v newuidmap >/dev/null 2>&1 || miss "newuidmap absent (rootless UID mapping) — sudo apt-get install -y uidmap"
command -v newgidmap >/dev/null 2>&1 || miss "newgidmap absent (rootless GID mapping) — sudo apt-get install -y uidmap"
grep -q "^$(id -un):" /etc/subuid 2>/dev/null || miss "/etc/subuid has no entry for $(id -un) — sudo usermod --add-subuids 100000-165535 $(id -un)"
[ "${FAILED}" = 0 ] || { printf '\n[error] rootless Podman prerequisites missing — install them (see hints), then re-run.\n' >&2; exit 2; }

SOCK="${DOCKER_HOST:-unix://${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/podman/podman.sock}"
export DOCKER_HOST="${SOCK}"
echo "Probing rootless Podman via ${SOCK}"

# Use the docker CLI against the Podman socket (Podman ships the Docker-compat API).
D() { docker "$@"; }
D version >/dev/null 2>&1 || { echo "[error] cannot reach the Podman socket at ${SOCK}. Enable it: systemctl --user enable --now podman.socket" >&2; exit 2; }

# 1. rootless reported?
if D info --format '{{json .SecurityOptions}}' 2>/dev/null | grep -q rootless; then
  pass "docker info reports rootless"
else
  warn "docker info SecurityOptions does not list 'rootless' (Podman may format it differently — inspect: docker info --format '{{.SecurityOptions}}')"
fi

# 2. Runtimes map (CC2/CC3 gating reads this — hardening.go pickRuntime).
_rt="$(D info --format '{{json .Runtimes}}' 2>/dev/null || echo '{}')"
if [ "${_rt}" != "{}" ] && [ -n "${_rt}" ]; then
  pass "Runtimes map present: ${_rt} (CC-gating can read it)"
else
  diverge "Runtimes map empty/absent — CC2/CC3 gating (classToRuntime) can't see runsc/kata; expect CC1-only. Podman manages runtimes via containers.conf, not this map."
fi

# 3. Resource-cap enforceability — test ACTUAL enforcement, not `docker info`.
# Podman's Docker-compat `docker info` under-reports CpuCfsQuota (=false) even when
# the CPU quota actually binds, so the info booleans are NOT authoritative on
# Podman. Create a capped container and read its real cgroup v2 files: memory.max
# / pids.max / cpu.max reflect what the kernel enforces.
_info_caps="$(D info --format '{{.MemoryLimit}}/{{.PidsLimit}}/{{.CPUCfsQuota}}' 2>/dev/null || echo '?/?/?')"
_real="$(D run --rm --memory 256m --pids-limit 64 --cpus 1 alpine:3.20 sh -c \
  'printf "%s|%s|%s" "$(cat /sys/fs/cgroup/memory.max 2>/dev/null)" "$(cat /sys/fs/cgroup/pids.max 2>/dev/null)" "$(cat /sys/fs/cgroup/cpu.max 2>/dev/null)"' 2>/dev/null || echo '?|?|?')"
_mem="${_real%%|*}"; _rest="${_real#*|}"; _pids="${_rest%%|*}"; _cpu="${_rest##*|}"
_cap_ok=1
[ "${_mem}" = "268435456" ] || _cap_ok=0
[ "${_pids}" = "64" ] || _cap_ok=0
case "${_cpu}" in "100000 100000") : ;; *) _cap_ok=0 ;; esac
if [ "${_cap_ok}" = 1 ]; then
  pass "resource caps ACTUALLY enforce (memory.max=${_mem}, pids.max=${_pids}, cpu.max='${_cpu}') — despite docker info reporting ${_info_caps}"
  case "${_info_caps}" in
    true/true/true) : ;;
    *) warn "docker info under-reports caps as ${_info_caps} (Podman quirk). Wardyn's Phase-4 pre-flight probe reads docker info, so it will FALSE-POSITIVE fail-closed here — set WARDYN_ALLOW_UNENFORCEABLE_CAPS=1 (caps still enforce, verified above)." ;;
  esac
else
  diverge "resource caps do NOT all actually bind (memory.max=${_mem} want 268435456, pids.max=${_pids} want 64, cpu.max='${_cpu}' want '100000 100000') — an untrusted sandbox could run uncapped. Enable rootless cgroup v2 delegation for the missing controller."
fi

# 4. Internal:true bridge — the L0 no-default-route guarantee. Create one, assert
# a container on it has NO route off-host, then clean up.
_net="wardyn-podman-probe-$$"
if D network create --internal --driver bridge "${_net}" >/dev/null 2>&1; then
  pass "Internal bridge network created (--internal supported)"
  # A container on an internal network must NOT reach the public internet.
  if D run --rm --network "${_net}" alpine:3.20 sh -c 'wget -q -T 3 -O /dev/null http://1.1.1.1 2>/dev/null' >/dev/null 2>&1; then
    diverge "CRITICAL: container on an --internal network reached the public internet — Podman's Internal semantics differ from Moby's; the L0 no-default-route guarantee does NOT hold here."
  else
    pass "Internal network blocks off-host egress (L0 guarantee holds)"
  fi
  D network rm "${_net}" >/dev/null 2>&1 || true
else
  diverge "could not create an --internal bridge network — the L0 gatewayless-network primitive is unavailable on this Podman."
fi

# 5. host.docker.internal (proxy sidecar ExtraHosts uses host-gateway).
if D run --rm --add-host host.docker.internal:host-gateway alpine:3.20 getent hosts host.docker.internal >/dev/null 2>&1; then
  pass "host-gateway / host.docker.internal resolves"
else
  warn "host.docker.internal:host-gateway did not resolve — Podman's host-gateway handling differs; the proxy sidecar's ExtraHosts may need a Podman-specific value."
fi

# 6. Storage driver (disk-quota parsing reads .Driver + Backing Filesystem).
_drv="$(D info --format '{{.Driver}}' 2>/dev/null || echo '?')"
case "${_drv}" in
  overlay|overlay2|btrfs|zfs) pass "storage driver '${_drv}' (disk-quota parsing recognizes it)" ;;
  *) warn "storage driver '${_drv}' — disk-quota support (applyDiskQuota) may not recognize it; runs proceed WITHOUT a disk cap (already a soft path)." ;;
esac

echo
if [ "${FAILED}" = 0 ]; then
  echo "[ok] rootless Podman: no runner-critical divergence found on this host. Run 'make test-e2e' against this DOCKER_HOST for the full governed-run proof."
else
  echo "[error] rootless Podman: divergences found (see [DIVERGE] above) — document them; do NOT assume Docker-API parity." >&2
fi
exit "${FAILED}"
