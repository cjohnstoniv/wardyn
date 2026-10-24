#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free regression test for scripts/e2e-backend.sh's process lifecycle (#3570): `down` must stop
# the wardynd that `up` started (recorded PID, then its process group), a failed or killed `up` must
# stop it too, and the gate wrapper's lock fds (8, 9) must not leak into it. A stub wardynd stands in
# for the real one; it binds -listen, answers 200, and forks a `sleep` child so the group kill is seen.
#
# E2E_BACKEND_SH=<path> runs the sections against another copy of the script (the revert proofs).
# Usage: scripts/test-e2e-backend-down.sh   (exit 0 = PASS, non-zero = FAIL by section name)
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${REPO_ROOT}/scripts/lib/common.sh"
tmp="$(mktemp -d)"
fail() { echo "test-e2e-backend-down: FAIL ${*}" >&2; exit 1; }
cleanup() {
  local f p
  for f in "${tmp}/repo/.e2e-bin/"*.pid "${tmp}/events/"stub-*; do
    p="$(cat "${f}" 2>/dev/null || true)"
    # Only a real pid above 1: an empty or unreadable file must never become `kill -- -0` (our own group).
    [[ "${p}" =~ ^[0-9]+$ && "${p}" -gt 1 ]] && kill -KILL -- "-${p}" 2>/dev/null || true
  done
  rm -rf "${tmp}"
}
trap cleanup EXIT
mkdir -p "${tmp}/repo/"{scripts/lib,.e2e-bin,ui/dist-e2e} "${tmp}/bin" "${tmp}/events"
cp "${E2E_BACKEND_SH:-${REPO_ROOT}/scripts/e2e-backend.sh}" "${tmp}/repo/scripts/e2e-backend.sh"
chmod +x "${tmp}/repo/scripts/e2e-backend.sh"
cp "${REPO_ROOT}/scripts/lib/"{common,e2e-network}.sh "${tmp}/repo/scripts/lib/"

cat > "${tmp}/repo/.e2e-bin/wardynd" <<'PY'
#!/usr/bin/env python3
import http.server, os, signal, subprocess, sys
from pathlib import Path
args = sys.argv[1:]
if args == ["-gen-age-key"]:
    print("AGE-SECRET-KEY-TEST-FIXTURE"); sys.exit(0)
ev = Path(os.environ["E2E_TEST_EVENTS"])
child = subprocess.Popen(["sleep", "300"])
(ev / f"stub-{os.getpid()}").write_text(f"{os.getpid()}\n")
(ev / f"child-{os.getpid()}").write_text(f"{child.pid}\n")
(ev / f"fds-{os.getpid()}").write_text("\n".join(os.readlink(f"/proc/self/fd/{f}") for f in os.listdir("/proc/self/fd") if os.path.exists(f"/proc/self/fd/{f}")))
if os.environ.get("E2E_TEST_NOBIND"):
    signal.pause()
flags = dict(zip(args[::2], args[1::2]))
host, port = flags["-listen"].rsplit(":", 1)
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"{}")
    do_POST = do_PUT = do_GET
http.server.ThreadingHTTPServer((host, int(port)), H).serve_forever()
PY
cat > "${tmp}/bin/docker" <<'SH'
#!/usr/bin/env bash
if [[ "$1" == port ]]; then echo '127.0.0.1:55432'
elif [[ -n "${E2E_TEST_PSQL_FAIL:-}" && "$*" == *ON_ERROR_STOP* ]]; then cat >/dev/null; exit 1  # Postgres unreachable at seed time
elif [[ "$*" == *' -i '* && "$*" != *' -c '* && "$*" != *' -tAc '* ]]; then cat >/dev/null
elif [[ "$*" == *' -tAc '* ]]; then echo 9
fi
SH
chmod +x "${tmp}/bin/docker" "${tmp}/repo/.e2e-bin/wardynd"
for name in ${!WARDYN_E2E_@}; do unset "${name}"; done
export PATH="${tmp}/bin:${PATH}" E2E_TEST_EVENTS="${tmp}/events"
export WARDYN_E2E_SKIP_BUILD=1 WARDYN_E2E_PG_CONTAINER=fake-e2e-pg
export WARDYN_E2E_ADDR="127.0.0.1:$(pick_free_port)" WARDYN_E2E_UI_ADDR="127.0.0.1:$(pick_free_port)" WARDYN_E2E_INTERNAL_ADDR="127.0.0.1:$(pick_free_port)"
cd "${tmp}/repo"
PIDF=".e2e-bin/wardynd-${WARDYN_E2E_ADDR##*:}.pid"

alive() { kill -0 "$1" 2>/dev/null; }
stub_pids() { cat "${tmp}/events/"stub-* 2>/dev/null || true; }
child_pids() { cat "${tmp}/events/"child-* 2>/dev/null || true; }
wait_dead() {  # wait_dead PID... : up to 5s
  local p i
  for p in "$@"; do for i in $(seq 1 50); do alive "${p}" || break; sleep 0.1; done; done
}
reset_events() { rm -f "${tmp}/events/"*; }

# (a) up then down leaves no wardynd (and no child of its group) from that run.
reset_events
./scripts/e2e-backend.sh up >"${tmp}/up.log" 2>&1 || { cat "${tmp}/up.log"; fail "(a) up failed"; }
pid="$(cat "${PIDF}")"
[[ "$(stub_pids)" == "${pid}" ]] || fail "(a) pid file ${pid} is not the stub wardynd [$(stub_pids)]"
alive "${pid}" || fail "(a) wardynd not running after a successful up"
[[ "$(ps -o pgid= -p "${pid}" | tr -d ' ')" == "${pid}" ]] || fail "(a) wardynd is not its own process-group leader"
child="$(child_pids)"
alive "${child}" || fail "(a) stub child not running"
./scripts/e2e-backend.sh down >"${tmp}/down.log" 2>&1 || { cat "${tmp}/down.log"; fail "(a) down failed"; }
wait_dead "${pid}" "${child}"
! alive "${pid}" || fail "(a) down left wardynd ${pid} running"
! alive "${child}" || fail "(a) down left wardynd's child ${child} running (group not stopped)"
[[ ! -e "${PIDF}" ]] || fail "(a) down left the pid file"
echo "ok (a) up then down leaves no wardynd or child"

# (b) a fresh up after down binds the same port again with a new process.
reset_events
./scripts/e2e-backend.sh up >"${tmp}/up.log" 2>&1 || { cat "${tmp}/up.log"; fail "(b) second up failed"; }
pid2="$(cat "${PIDF}")"
[[ "${pid2}" != "${pid}" ]] || fail "(b) second up reused the old pid"
(exec 3<>"/dev/tcp/${WARDYN_E2E_ADDR%:*}/${WARDYN_E2E_ADDR##*:}") 2>/dev/null || fail "(b) nothing listens on ${WARDYN_E2E_ADDR}"
./scripts/e2e-backend.sh down >/dev/null 2>&1 || fail "(b) down failed"
wait_dead "${pid2}"
! alive "${pid2}" || fail "(b) down left wardynd ${pid2} running"
echo "ok (b) fresh up binds the port after down"

# (c) up whose Postgres is unreachable after wardynd started exits non-zero and leaves no wardynd.
reset_events
if E2E_TEST_PSQL_FAIL=1 ./scripts/e2e-backend.sh up >"${tmp}/up.log" 2>&1; then fail "(c) up succeeded with Postgres unreachable"; fi
pid="$(stub_pids)"
[[ -n "${pid}" ]] || { cat "${tmp}/up.log"; fail "(c) wardynd never started, so the failure path was not exercised"; }
wait_dead ${pid} "$(child_pids)"
! alive "${pid}" || fail "(c) failed up left wardynd ${pid} running"
! alive "$(child_pids)" || fail "(c) failed up left wardynd's child running"
[[ ! -e "${PIDF}" ]] || fail "(c) failed up left the pid file"
echo "ok (c) failed up stops its wardynd"

# (d) down with no pid file, and with a dead pid, succeeds.
rm -f "${PIDF}"
./scripts/e2e-backend.sh down >/dev/null 2>&1 || fail "(d) down with no pid file returned non-zero"
echo 2147483646 > "${PIDF}"
./scripts/e2e-backend.sh down >/dev/null 2>&1 || fail "(d) down with a dead pid returned non-zero"
sleep 300 & other=$!
echo "${other}" > "${PIDF}"
./scripts/e2e-backend.sh down >/dev/null 2>&1 || fail "(d) down with a reused pid returned non-zero"
alive "${other}" || fail "(d) down killed an unrelated process whose pid was in the pid file"
kill "${other}"
echo "ok (d) down is a no-op without a live wardynd"

# (e) killing the script mid-up (wardynd up, never healthy) leaves no wardynd.
reset_events
E2E_TEST_NOBIND=1 ./scripts/e2e-backend.sh up >"${tmp}/up.log" 2>&1 & upid=$!
for _ in $(seq 1 100); do [[ -n "$(stub_pids)" && -s "${PIDF}" ]] && break; sleep 0.1; done
pid="$(stub_pids)"; [[ -n "${pid}" ]] || fail "(e) wardynd never started"
kill -TERM "${upid}"
wait "${upid}" 2>/dev/null || true
wait_dead "${pid}" "$(child_pids)"
! alive "${pid}" || fail "(e) killing up left wardynd ${pid} running"
! alive "$(child_pids)" || fail "(e) killing up left wardynd's child running"
echo "ok (e) a killed up leaves no wardynd"

# (f) the gate lock fds 8 and 9 do not reach wardynd.
reset_events
./scripts/e2e-backend.sh up 8>"${tmp}/lock8" 9>"${tmp}/lock9" >"${tmp}/up.log" 2>&1 || { cat "${tmp}/up.log"; fail "(f) up failed"; }
pid="$(cat "${PIDF}")"
! grep -E 'lock[89]' "${tmp}/events/fds-${pid}" >/dev/null || fail "(f) wardynd inherited a gate lock fd: $(grep -E 'lock[89]' "${tmp}/events/fds-${pid}")"
./scripts/e2e-backend.sh down >/dev/null 2>&1 || fail "(f) down failed"
echo "ok (f) lock fds 8 and 9 are closed in wardynd"
echo "test-e2e-backend-down: PASS"
