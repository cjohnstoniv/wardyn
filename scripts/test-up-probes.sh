#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free, network-free regression coverage for HOW scripts/up.sh asks the
# running wardynd container a question — the post-boot probe block in cmd_up.
#
# THE regression this pins (ADV2-01 / ADV2-02): those probes ran a throwaway
# curl container on the compose network with no headers at all, so every
# request arrived at wardynd with `Host: wardynd:8080` and no credential. In
# local mode that is a 403 from the DNS-rebinding Host guard (isLoopbackHost,
# internal/api/http.go) and in token mode a 401 from humanOrAdminAuth — on
# EVERY `make setup`, in EVERY compose posture. So the "no-auth gate REJECTED a
# non-loopback peer" warning fired unconditionally (it could never say OK, and
# could therefore never detect the forwarder regression it exists to detect),
# and the post-boot LLM-ready policy re-pick behind /api/v1/setup/status was
# unreachable dead code.
#
# Both are ONE cause — a probe from a non-loopback peer against loopback-only
# gates — so there is now ONE way to ask: up.sh's wardynd_probe(). This file
# EXTRACTS that function straight from scripts/up.sh (same live-source pattern
# as scripts/test-up-policy.sh) and drives it against a `docker` stub on PATH
# that emulates wardynd's auth middleware for a request arriving from a
# non-loopback peer. No daemon, no network, no compose stack.
#
# Usage: scripts/test-up-probes.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UP_SH="${REPO_ROOT}/scripts/up.sh"

# shellcheck source=lib/common.sh
. "${REPO_ROOT}/scripts/lib/common.sh"  # env_get / env_set

fail() { echo "test-up-probes: FAIL: $*" >&2; exit 1; }

dir="$(mktemp -d)"; trap 'rm -rf "${dir}"' EXIT

# ── the `docker` stub: wardynd's auth middleware, from a non-loopback peer ───
# Mirrors internal/api/http.go's LocalMode branch (peer gate, then Host gate)
# and the humanOrAdminAuth group's bearer check, and speaks curl's
# `-w '\n%{http_code}'` output encoding (body, newline, status; no trailing
# newline). The peer is ALWAYS non-loopback here — that is the whole fixture:
# a container on the compose bridge, exactly like the docker gateway a real
# host UI/CLI request arrives as.
mkdir -p "${dir}/bin"
cat > "${dir}/bin/docker" <<'STUB'
#!/usr/bin/env bash
set -u
[[ "${1:-}" == "run" ]] || { echo "stub docker: unexpected subcommand: $*" >&2; exit 99; }

# ── alpine:3.20 -> doctor's socket-mountability probe ───────────────────────
# Emulates the ONE daemon behaviour that matters here: a bind-mount SOURCE that
# does not exist is MATERIALIZED as a (root-owned) directory before the
# container starts. The container command is then run on the host with the
# container path mapped back through the mount.
if printf '%s\n' "$@" | grep -qx 'alpine:3.20'; then
  src=""; dst=""; cargs=(); seen_img=0
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -v) spec="$2"; src="${spec%%:*}"; rest="${spec#*:}"; dst="${rest%%:*}"
          [[ -e "${src}" ]] || mkdir -p "${src}"   # <- docker materializes it
          shift 2 ;;
      alpine:3.20) seen_img=1; shift ;;
      *) if [[ ${seen_img} == 1 ]]; then cargs+=("${1/#${dst}//${src#/}}"); fi; shift ;;
    esac
  done
  [[ "${cargs[0]:-}" == "test" ]] || { echo "stub docker: unexpected probe cmd: ${cargs[*]:-}" >&2; exit 97; }
  exec "${cargs[@]}"
fi

host=""; auth=""; url=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -H) case "${2:-}" in
          Host:*)          host="${2#Host: }" ;;
          Authorization:*) auth="${2#Authorization: }" ;;
        esac
        shift 2 ;;
    http://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
[[ -n "${url}" ]] || { echo "stub docker: no URL in probe args" >&2; exit 98; }
# curl's default Host is the URL authority — what an unheadered probe sends.
rest="${url#http://}"
if [[ -z "${host}" ]]; then host="${rest%%/*}"; fi
path="/${rest#*/}"
emit() { printf '%s\n%s' "$1" "$2"; exit 0; }

if [[ "${GATE_LOCAL_MODE}" == "true" ]]; then
  [[ "${GATE_TRUST_FORWARDER}" == "true" ]] \
    || emit '{"error":"local mode: request peer is not loopback (bind wardynd to 127.0.0.1, set WARDYN_LOCAL_TRUST_FORWARDER when behind a loopback-only publish, or configure auth)"}' 403
  case "${host%%:*}" in
    127.0.0.1|localhost|'[::1]'|'::1') ;;
    *) emit '{"error":"local mode: request Host is not loopback (DNS-rebinding guard)"}' 403 ;;
  esac
elif [[ "${path}" != "/healthz" ]]; then
  # humanOrAdminAuth: /api/v1/** needs a credential when local mode is off.
  [[ "${auth}" == "Bearer ${GATE_TOKEN}" ]] || emit '{"error":"missing bearer token"}' 401
fi

case "${path}" in
  /healthz)              emit 'ok' 200 ;;
  /api/v1/setup/status)  emit '{"ready":true,"llm_ready":true,"providers":[]}' 200 ;;
  /api/v1/me)            emit '{"principal":"local:operator","role":"admin"}' 200 ;;
  *)                     emit '{"error":"not found"}' 404 ;;
esac
STUB
chmod +x "${dir}/bin/docker"
PATH="${dir}/bin:${PATH}"; export PATH

# ── 1) structural: ONE way to ask, and it sends a loopback Host ─────────────
# The two Highs were one cause, so the fix is one call path. Anything that
# reintroduces a bare in-network curl (or drops the Host override from the one
# that remains) puts the unconditional-403 bug straight back.
raw_probes="$(grep -n 'curlimages/curl' "${UP_SH}" || true)"
[ "$(printf '%s' "${raw_probes}" | grep -c . || true)" = "1" ] || {
  echo "--- in-network curl invocations found in scripts/up.sh ---" >&2
  printf '%s\n' "${raw_probes}" >&2
  fail "scripts/up.sh must reach wardynd through wardynd_probe() ONLY (found $(printf '%s' "${raw_probes}" | grep -c . || true) raw curlimages/curl invocations); each bare one asks from a non-loopback peer with no loopback Host and is answered 403/401 on every \`make setup\` (ADV2-01/ADV2-02)"
}

grep -qE '^wardynd_probe\(\) \{' "${UP_SH}" \
  || fail "scripts/up.sh has no wardynd_probe() — the post-boot probes must go through ONE helper that sends the loopback Host + bearer the gates require (ADV2-01/ADV2-02)"

probe_body="$(sed -n '/^wardynd_probe() {/,/^}/p' "${UP_SH}")"
printf '%s' "${probe_body}" | grep -q 'curlimages/curl' \
  || fail "the surviving curlimages/curl invocation is not inside wardynd_probe()"
printf '%s' "${probe_body}" | grep -qE -- '-H "Host: (127\.0\.0\.1|localhost|\[::1\])' \
  || fail "wardynd_probe() does not override Host with a loopback authority — local mode's DNS-rebinding guard 403s the Docker-DNS authority 'wardynd:8080' (ADV2-01)"
eval "${probe_body}"

# ── 2) local mode (the default compose posture): the probe must answer 200 ──
env_file="${dir}/.env"
: > "${env_file}"
export GATE_LOCAL_MODE=true GATE_TRUST_FORWARDER=true GATE_TOKEN=demo-admin-token

got="$(wardynd_probe "${env_file}" /api/v1/me | tail -n1)"
[ "${got}" = "200" ] || fail "local-mode no-auth gate probe got HTTP ${got}, want 200 — up.sh's 'gate REJECTED a non-loopback peer' warning fires on every \`make setup\` and the probe can never confirm a healthy gate (ADV2-01)"

got="$(wardynd_probe "${env_file}" /healthz | tail -n1)"
[ "${got}" = "200" ] || fail "sandbox -> control-plane /healthz probe got HTTP ${got}, want 200"

# ...and it must still DETECT the regression it exists to detect: with the
# forwarder off, the peer gate rejects and the probe reports that 403.
GATE_TRUST_FORWARDER=false
got="$(wardynd_probe "${env_file}" /api/v1/me | tail -n1)"
[ "${got}" = "403" ] || fail "with WARDYN_LOCAL_TRUST_FORWARDER off the probe got HTTP ${got}, want 403 — the loopback-Host override must not paper over the PEER gate this smoke exists to test"
GATE_TRUST_FORWARDER=true

# ── 3) token mode (local mode off, no OIDC): the bearer is what answers ─────
export GATE_LOCAL_MODE=false GATE_TOKEN=s3cret-admin-token
env_set "${env_file}" WARDYN_ADMIN_TOKEN s3cret-admin-token
got="$(wardynd_probe "${env_file}" /api/v1/setup/status | tail -n1)"
[ "${got}" = "200" ] || fail "token-mode probe got HTTP ${got}, want 200 — /api/v1/setup/status is in the humanOrAdminAuth group and needs the WARDYN_ADMIN_TOKEN from .env (ADV2-02)"

# A .env with no token at all must still answer: compose boots wardynd with
# "${WARDYN_ADMIN_TOKEN:-demo-admin-token}", so the helper's fallback has to be
# that same default or the probe 401s on a stock stack.
env_file2="${dir}/.env2"; : > "${env_file2}"
GATE_TOKEN=demo-admin-token
got="$(wardynd_probe "${env_file2}" /api/v1/setup/status | tail -n1)"
[ "${got}" = "200" ] || fail "token-mode probe with no WARDYN_ADMIN_TOKEN in .env got HTTP ${got}, want 200 — the fallback must mirror docker-compose.yaml's own :-demo-admin-token default"

# A token that does NOT match what wardynd booted with is a real, reportable
# problem, not a silent one.
GATE_TOKEN=some-other-token
got="$(wardynd_probe "${env_file}" /api/v1/me | tail -n1)"
[ "${got}" = "401" ] || fail "a mismatched admin token got HTTP ${got}, want 401"
grep -q '      401)' "${UP_SH}" \
  || fail "scripts/up.sh's local-mode gate smoke has no 401 arm — a token-mode mismatch falls into the generic 'inconclusive, check logs' branch (ADV2-01: the branches must distinguish healthy from broken)"

# ── 4) end to end: the LLM-ready re-pick is reachable again ────────────────
# ADV2-02's actual consequence. llm_ready_from_probe only trusts a 200, so
# before the fix this fed on a 403/401 body forever and the ceiling stayed
# wedged on demo.json no matter what model path the operator configured.
eval "$(sed -n '/^llm_ready_from_status() {/,/^}/p' "${UP_SH}")"
eval "$(sed -n '/^llm_ready_from_probe() {/,/^}/p' "${UP_SH}")"
export GATE_LOCAL_MODE=true GATE_TRUST_FORWARDER=true GATE_TOKEN=demo-admin-token
got="$(llm_ready_from_probe "$(wardynd_probe "${env_file}" /api/v1/setup/status)")"
[ "${got}" = "1" ] || fail "a healthy stack reporting llm_ready:true did not drive the post-boot policy re-pick (llm_ready_from_probe gave '${got}') — the re-pick is still unreachable (ADV2-02)"

# ── 5) every host-side health probe is TIME-BOUNDED (perf-2:perf2-C1) ──────
# cmd_up's health wait advertises a ceiling of 60 tries x 2s, and common.sh's
# wait_healthy/wait_down carry their own TRIES budgets — but an unbounded curl
# against a peer that ACCEPTS the connection and never replies blocks forever,
# so the loop never reaches its second attempt and the budget is fiction.
# `make setup` hangs with no ceiling at all.
unbounded=""
for f in "${UP_SH}" "${REPO_ROOT}/scripts/lib/common.sh"; do
  while IFS= read -r line; do
    case "${line}" in
      *'-m '*|*'--max-time'*|*'#'*) ;;
      *) unbounded="${unbounded}${f}:${line}"$'\n' ;;
    esac
  done < <(grep -n 'curl -fsS' "${f}" | grep -v '^[0-9]*:[[:space:]]*#' || true)
done
[ -z "${unbounded}" ] || {
  echo "--- unbounded curl probes ---" >&2
  printf '%s' "${unbounded}" >&2
  fail "every host-side curl probe must carry a max-time bound (-m); a stalled-but-accepting peer otherwise blocks the poll loop past its advertised ceiling forever (perf2-C1)"
}

grep -q '_cid=$(compose ps -q wardynd 2>/dev/null || true)$' "${UP_SH}" || fail "cmd_up no longer reads the wardynd container id the way this guard expects"
awk '/^  until \{ \[ -n "\$\{_cid\}"/,/^  done$/' "${UP_SH}" | grep -q 'compose ps -q wardynd'   || fail "cmd_up's health-wait loop never re-reads _cid — an empty first capture (slow daemon, right after \`compose up -d\`) wedges the wait on the host-curl arm for its whole budget (perf2-C1)"

# Behavioural proof against a real stalled peer: a listener that ACCEPTS and
# never replies. wait_healthy must return within its own budget, not hang.
port_file="$(mktemp)"
python3 - "${port_file}" <<'PYEOF' &
import socket, sys, time
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("127.0.0.1", 0))
with open(sys.argv[1], "w") as f:
    f.write(str(s.getsockname()[1]))
s.listen(8)
conns = []
deadline = time.time() + 120
while time.time() < deadline:
    try:
        s.settimeout(max(0.1, deadline - time.time()))
        conns.append(s.accept()[0])   # accepted, never answered
    except OSError:
        break
PYEOF
stall_pid=$!
trap 'kill "${stall_pid}" >/dev/null 2>&1 || true; rm -rf "${dir}" "${port_file}"' EXIT
for _ in $(seq 1 50); do [ -s "${port_file}" ] && break; sleep 0.1; done
stall_port="$(cat "${port_file}")"
[ -n "${stall_port}" ] || fail "test harness could not bind a stalled listener"

# PATH still carries the docker stub; wait_healthy uses curl only.
start="$(date +%s)"
rc=0
timeout 20 bash -c ". '${REPO_ROOT}/scripts/lib/common.sh'; wait_healthy 'http://127.0.0.1:${stall_port}' 1 1" || rc=$?
elapsed=$(( $(date +%s) - start ))
[ "${rc}" != "0" ]   || fail "wait_healthy reported a stalled-but-accepting peer as healthy"
[ "${rc}" != "124" ] || fail "wait_healthy did not return against a stalled-but-accepting peer — its curl is unbounded, so TRIES never expires and \`make setup\` waits forever (perf2-C1)"
[ "${elapsed}" -lt 15 ] || fail "wait_healthy took ${elapsed}s for a 1-try budget against a stalled peer — the curl bound is not effective (perf2-C1)"

kill "${stall_pid}" >/dev/null 2>&1 || true

# ── 6) doctor's socket probe MATERIALIZES nothing (correctness-1:C5) ───────
# `make doctor` advertises itself as a read-only preflight that "creates/changes
# nothing". Its socket-mountability probe named the socket path itself as the
# bind-mount SOURCE, and docker materializes a missing source as a root-owned
# DIRECTORY — so doctor left a directory sitting exactly where the socket is
# supposed to appear, on the Rancher-Desktop-shaped hosts the probe exists for.
eval "$(sed -n '/^sock_mountable() {/,/^}/p' "${UP_SH}")"
[ "$(type -t sock_mountable)" = "function" ] || fail "scripts/up.sh has no sock_mountable() — doctor's socket-mountability probe must be extractable so its no-create contract can be pinned (C5)"

sockdir="${dir}/rd"; mkdir -p "${sockdir}"
missing="${sockdir}/docker.sock"
rc=0; sock_mountable "${missing}" || rc=$?
[ "${rc}" != "0" ] || fail "sock_mountable said a nonexistent socket is mountable"
[ ! -e "${missing}" ] || fail "doctor's socket probe CREATED $(ls -ld "${missing}" | awk '{print $1}') at ${missing} — it must never name a missing path as a bind-mount source (C5: 'read-only preflight — creates/changes nothing')"

# A parent that does not exist either is skipped outright, not conjured.
absent="${dir}/no-such-dir/docker.sock"
rc=0; sock_mountable "${absent}" || rc=$?
[ "${rc}" != "0" ] || fail "sock_mountable said a socket under a nonexistent parent is mountable"
[ ! -e "${dir}/no-such-dir" ] || fail "doctor's socket probe created the parent directory ${dir}/no-such-dir (C5)"

# ...and it must still say YES to a socket the daemon can actually see.
python3 - "${sockdir}/docker.sock" <<'PYEOF'
import socket, sys
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.bind(sys.argv[1])
PYEOF
sock_mountable "${sockdir}/docker.sock" || fail "sock_mountable did not recognise a real unix socket the daemon can see — the probe lost its whole purpose (C5)"
rm -f "${sockdir}/docker.sock"

echo "test-up-probes: self-test PASS"
