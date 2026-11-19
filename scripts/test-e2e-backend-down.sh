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
  for p in "${taker:-}" "${holder:-}"; do [[ -z "${p}" ]] || kill "${p}" 2>/dev/null || true; done  # (g), (h)
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
# The base-path proxy stub for (i): records its pid, binds -listen (exiting on a taken port), answers 200.
cat > "${tmp}/repo/.e2e-bin/basepathproxy" <<'PY'
#!/usr/bin/env python3
import http.server, os, sys
from pathlib import Path
(Path(os.environ["E2E_TEST_EVENTS"]) / f"proxy-{os.getpid()}").write_text(f"{os.getpid()}\n")
flags = dict(zip(sys.argv[1::2], sys.argv[2::2]))
host, port = flags["-listen"].rsplit(":", 1)
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"{}")
http.server.ThreadingHTTPServer((host, int(port)), H).serve_forever()
PY
cat > "${tmp}/bin/docker" <<'SH'
#!/usr/bin/env bash
if [[ "$1" == port ]]; then echo '127.0.0.1:55432'
elif [[ -n "${E2E_TEST_PSQL_FAIL:-}" && "$*" == *ON_ERROR_STOP* ]]; then cat >/dev/null; exit 1  # Postgres unreachable at seed time
elif [[ -n "${E2E_TEST_SEED_WAITS:-}" && "$*" == *ON_ERROR_STOP* ]]; then  # (g), (i): the seed waits for that stub's exit
  for _ in $(seq 1 100); do p="$(cat "${E2E_TEST_EVENTS}/${E2E_TEST_SEED_WAITS}"-* 2>/dev/null || true)"; [[ -n "${p}" ]] && ! kill -0 "${p}" 2>/dev/null && break; sleep 0.1; done
  cat >/dev/null
elif [[ "$*" == *' -i '* && "$*" != *' -c '* && "$*" != *' -tAc '* ]]; then cat >/dev/null
elif [[ "$*" == *' -tAc '* ]]; then echo 9
fi
SH
chmod +x "${tmp}/bin/docker" "${tmp}/repo/.e2e-bin/wardynd" "${tmp}/repo/.e2e-bin/basepathproxy"
for name in ${!WARDYN_E2E_@}; do unset "${name}"; done
export PATH="${tmp}/bin:${PATH}" E2E_TEST_EVENTS="${tmp}/events"
export WARDYN_E2E_SKIP_BUILD=1 WARDYN_E2E_PG_CONTAINER=fake-e2e-pg
export WARDYN_E2E_ADDR="127.0.0.1:$(pick_free_port)"
export WARDYN_E2E_UI_ADDR="127.0.0.1:$(pick_free_port "${WARDYN_E2E_ADDR##*:}")"
export WARDYN_E2E_INTERNAL_ADDR="127.0.0.1:$(pick_free_port "${WARDYN_E2E_ADDR##*:}" "${WARDYN_E2E_UI_ADDR##*:}")"
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
# (g) another process took the -listen port before wardynd bound it, and answers every request, so the
# health check passes while wardynd is still starting. Once wardynd has exited on its bind error, up
# fails and names that exit. The docker stub holds the seed until the stub wardynd has exited
# (E2E_TEST_SEED_WAITS): without that the seed and the exit race, and a wardynd still starting when
# the seed ends is not caught by this check.
reset_events
python3 -c 'import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"{}")
    do_POST = do_PUT = do_GET
srv = http.server.ThreadingHTTPServer((sys.argv[1], int(sys.argv[2])), H)
open(sys.argv[3], "w").close()
srv.serve_forever()' "${WARDYN_E2E_ADDR%:*}" "${WARDYN_E2E_ADDR##*:}" "${tmp}/taker-ready" & taker=$!
for _ in $(seq 1 50); do [[ -e "${tmp}/taker-ready" ]] && break; sleep 0.1; done
[[ -e "${tmp}/taker-ready" ]] || { kill "${taker}"; fail "(g) the taker never listened"; }
started=${SECONDS}
if E2E_TEST_SEED_WAITS=stub ./scripts/e2e-backend.sh up >"${tmp}/up.log" 2>&1; then kill "${taker}"; fail "(g) up succeeded while another process held ${WARDYN_E2E_ADDR}"; fi
took=$((SECONDS - started))
grep -q 'wardynd exited' "${tmp}/up.log" || { cat "${tmp}/up.log"; kill "${taker}"; fail "(g) up did not name wardynd's exit"; }
[[ ${took} -lt 15 ]] || { kill "${taker}"; fail "(g) up took ${took}s to notice wardynd had exited"; }
alive "${taker}" || fail "(g) up stopped the process that held the port"
[[ ! -e "${PIDF}" ]] || { kill "${taker}"; fail "(g) failed up left the pid file"; }
kill "${taker}"; wait "${taker}" 2>/dev/null || true; taker=""
echo "ok (g) a wardynd that exited on a taken port fails up, though another process answered for it"
# (h) the port is held but nothing answers on it: up fails as soon as wardynd exits, not after the
# whole health-check budget (30 s). A port no test above used: a plain bind cannot share one that still
# has connections in TIME_WAIT.
reset_events
export WARDYN_E2E_ADDR="127.0.0.1:$(pick_free_port "${WARDYN_E2E_ADDR##*:}" "${WARDYN_E2E_UI_ADDR##*:}" "${WARDYN_E2E_INTERNAL_ADDR##*:}")"
PIDF=".e2e-bin/wardynd-${WARDYN_E2E_ADDR##*:}.pid"
python3 -c 'import signal, socket, sys
s = socket.socket(); s.bind((sys.argv[1], int(sys.argv[2])))  # bound, never listening
open(sys.argv[3], "w").close()
signal.pause()' "${WARDYN_E2E_ADDR%:*}" "${WARDYN_E2E_ADDR##*:}" "${tmp}/holder-ready" & holder=$!
for _ in $(seq 1 50); do [[ -e "${tmp}/holder-ready" ]] && break; sleep 0.1; done
[[ -e "${tmp}/holder-ready" ]] || { kill "${holder}"; fail "(h) the holder never bound"; }
started=${SECONDS}
if ./scripts/e2e-backend.sh up >"${tmp}/up.log" 2>&1; then kill "${holder}"; fail "(h) up succeeded on a held port"; fi
took=$((SECONDS - started))
kill "${holder}"; wait "${holder}" 2>/dev/null || true; holder=""
grep -q 'wardynd exited before it served' "${tmp}/up.log" || { cat "${tmp}/up.log"; fail "(h) up did not name wardynd's exit"; }
[[ ${took} -lt 15 ]] || fail "(h) up took ${took}s to notice wardynd had exited"
echo "ok (h) a held port fails up as soon as wardynd exits"
# (i) the same for the base-path proxy: another process took its port and answers, so its health check
# passes. Once the proxy has exited on its bind error, up fails and names that exit.
reset_events
export WARDYN_E2E_BASE_PATH=/wardyn
export WARDYN_E2E_PROXY_ADDR="127.0.0.1:$(pick_free_port "${WARDYN_E2E_ADDR##*:}" "${WARDYN_E2E_UI_ADDR##*:}" "${WARDYN_E2E_INTERNAL_ADDR##*:}")"
python3 -c 'import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"{}")
srv = http.server.ThreadingHTTPServer((sys.argv[1], int(sys.argv[2])), H)
open(sys.argv[3], "w").close()
srv.serve_forever()' "${WARDYN_E2E_PROXY_ADDR%:*}" "${WARDYN_E2E_PROXY_ADDR##*:}" "${tmp}/ptaker-ready" & taker=$!
for _ in $(seq 1 50); do [[ -e "${tmp}/ptaker-ready" ]] && break; sleep 0.1; done
[[ -e "${tmp}/ptaker-ready" ]] || fail "(i) the taker never listened"
if E2E_TEST_SEED_WAITS=proxy ./scripts/e2e-backend.sh up >"${tmp}/up.log" 2>&1; then fail "(i) up succeeded while another process held ${WARDYN_E2E_PROXY_ADDR}"; fi
grep -q 'basepathproxy exited' "${tmp}/up.log" || { cat "${tmp}/up.log"; fail "(i) up did not name the proxy's exit"; }
alive "${taker}" || fail "(i) up stopped the process that held the port"
[[ ! -e "${PIDF}" ]] || fail "(i) failed up left the pid file"
kill "${taker}"; wait "${taker}" 2>/dev/null || true; taker=""
echo "ok (i) a base-path proxy that exited on a taken port fails up, though another process answered for it"
# (j) the proxy's port is held but nothing answers on it: up fails as soon as the proxy exits (the pid
# is passed to its wait), not after the whole health-check budget (10 s).
reset_events
export WARDYN_E2E_PROXY_ADDR="127.0.0.1:$(pick_free_port "${WARDYN_E2E_ADDR##*:}" "${WARDYN_E2E_UI_ADDR##*:}" "${WARDYN_E2E_INTERNAL_ADDR##*:}" "${WARDYN_E2E_PROXY_ADDR##*:}")"
python3 -c 'import signal, socket, sys
s = socket.socket(); s.bind((sys.argv[1], int(sys.argv[2])))  # bound, never listening
open(sys.argv[3], "w").close()
signal.pause()' "${WARDYN_E2E_PROXY_ADDR%:*}" "${WARDYN_E2E_PROXY_ADDR##*:}" "${tmp}/pholder-ready" & holder=$!
for _ in $(seq 1 50); do [[ -e "${tmp}/pholder-ready" ]] && break; sleep 0.1; done
[[ -e "${tmp}/pholder-ready" ]] || fail "(j) the holder never bound"
if ./scripts/e2e-backend.sh up >"${tmp}/up.log" 2>&1; then fail "(j) up succeeded on a held proxy port"; fi
kill "${holder}"; wait "${holder}" 2>/dev/null || true; holder=""
grep -q 'basepathproxy exited before it served' "${tmp}/up.log" || { cat "${tmp}/up.log"; fail "(j) up did not name the proxy's exit"; }
echo "ok (j) a held proxy port fails up as soon as the proxy exits"
echo "test-e2e-backend-down: PASS"
