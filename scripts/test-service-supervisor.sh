#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Tests the service supervisor (run_service in
# deploy/images/common/agent-run-lib.sh) as agent-run runs it: a bash under
# `set -euo pipefail` that sourced the lib. No Docker or network: each case runs
# a supervisor in the background against a temp state file and reads the v1
# JSON wardynd mirrors. The HTTP and TCP pass cases need socat and are skipped
# without it; the failing-probe cases need nothing listening, so they always run.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="$ROOT/deploy/images/common/agent-run-lib.sh"

tmp="$(mktemp -d)"
sups=()
cleanup() {
    local p
    for p in "${sups[@]}"; do kill -TERM "$p" 2>/dev/null || true; done
    for p in "${sups[@]}"; do wait "$p" 2>/dev/null || true; done
    rm -rf "$tmp"
}
trap cleanup EXIT

fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

# start <name> <command> [VAR=value ...] — run a supervisor in the background with
# its state at $tmp/<name>.json and its stderr at $tmp/<name>.log; sets $sup.
start() {
    local name="$1" cmd="$2"
    shift 2
    # shellcheck disable=SC2016 # expanded by the inner bash
    env WARDYN_SERVICE_STATE_FILE="$tmp/$name.json" "$@" \
        bash -c 'set -euo pipefail; . "$1"; run_service "$2"' _ "$LIB" "$cmd" \
        >/dev/null 2>"$tmp/$name.log" &
    sup=$!
    sups+=("$sup")
}

# wait_state <name> <substring> <secs> — until the state file holds substring.
wait_state() {
    local i
    for ((i = 0; i < $3 * 10; i++)); do
        [[ "$(cat "$tmp/$1.json" 2>/dev/null)" == *"$2"* ]] && return 0
        sleep 0.1
    done
    return 1
}

# stop <pid> — TERM a supervisor and set rc to its exit code (no $(…): a
# subshell cannot wait for this shell's child).
stop() {
    rc=0
    kill -TERM "$1"
    wait "$1" || rc=$?
}

state() { cat "$tmp/$1.json" 2>/dev/null; }

free_port() { echo $((20000 + RANDOM % 20000)); }

# 1) exit 0 ends the service: the run completes, nothing restarts.
rc=0
# shellcheck disable=SC2016 # expanded by the inner bash
env WARDYN_SERVICE_STATE_FILE="$tmp/zero.json" WARDYN_TASK_MODE=exec WARDYN_SERVICE=1 \
    bash -c 'set -euo pipefail; . "$1"; maybe_exec_task_mode "exit 0"' _ "$LIB" 2>/dev/null || rc=$?
want='{"v":1,"health":"healthy","reason":"","restarts":0,"last_exit_code":0}'
if [[ $rc == 0 && "$(state zero)" == "$want" ]]; then ok "exit 0 ends the service (exit 0, $want)"
else bad "exit 0: rc=$rc state=$(state zero)"; fi

# 2) restart=never: the command's own code ends the run, no restart.
rc=0
# shellcheck disable=SC2016 # expanded by the inner bash
env WARDYN_SERVICE_STATE_FILE="$tmp/never.json" WARDYN_TASK_MODE=exec WARDYN_SERVICE=1 \
    WARDYN_SERVICE_RESTART=never \
    bash -c 'set -euo pipefail; . "$1"; maybe_exec_task_mode "exit 3"' _ "$LIB" 2>/dev/null || rc=$?
want='{"v":1,"health":"unhealthy","reason":"","restarts":0,"last_exit_code":3}'
if [[ $rc == 3 && "$(state never)" == "$want" ]]; then ok "restart=never exits with the command's code (3)"
else bad "restart=never: rc=$rc state=$(state never)"; fi

# 3) exit 1 restarts and counts, backing off 1 s then 2 s.
start crash 'exit 1'
if wait_state crash '"restarts":2' 8; then
    want='{"v":1,"health":"unhealthy","reason":"restarting","restarts":2,"last_exit_code":1}'
    if [[ "$(state crash)" == "$want" ]]; then ok "exit 1 restarts and counts ($want)"
    else bad "crash state: $(state crash)"; fi
else
    bad "exit 1 did not restart twice: $(state crash)"
fi
log="$(cat "$tmp/crash.log")"
if [[ "$log" == *"restart 1 in 1s"* && "$log" == *"restart 2 in 2s"* ]]; then ok "backoff 1 s then 2 s"
else bad "backoff log: $log"; fi
stop "$sup"
if [[ $rc == 143 ]]; then ok "TERM during backoff exits 143"; else bad "TERM during backoff exited $rc"; fi

# 4) TERM stops the whole process group and never restarts.
start term "echo \$\$ > '$tmp/term.pid'; sleep 300; true"
for ((i = 0; i < 50; i++)); do [[ -s "$tmp/term.pid" ]] && break; sleep 0.1; done
pg="$(cat "$tmp/term.pid" 2>/dev/null || echo 0)"
stop "$sup"
want='{"v":1,"health":"healthy","reason":"","restarts":0,"last_exit_code":143}'
if [[ $rc == 143 && "$(state term)" == "$want" ]]; then ok "TERM exits 143 without a restart ($want)"
else bad "TERM: rc=$rc state=$(state term)"; fi
if [[ $pg != 0 ]] && ! kill -0 -- "-$pg" 2>/dev/null; then ok "TERM reached the command's process group"
else bad "process group $pg survived the stop"; fi

# 4b) a child that outlives its command's exit is killed with the group: before
# each restart, so a stop during the backoff finds nothing left, and before an
# exit under restart=never, which keeps the command's own code.
alive() { local p; for p in $(cat "$1" 2>/dev/null); do kill -0 "$p" 2>/dev/null && return 0; done; return 1; }
start orphan "sleep 300 & echo \$! >> '$tmp/orphan.pids'; exit 1"
if wait_state orphan '"restarts":1' 4 && ! alive "$tmp/orphan.pids"; then
    ok "a crashed command's child is killed before the restart"
else bad "child of the crashed command survived: $(state orphan) pids=$(cat "$tmp/orphan.pids" 2>/dev/null)"; fi
if wait_state orphan '"restarts":2' 6; then
    stop "$sup"
    if [[ $rc == 143 ]] && ! alive "$tmp/orphan.pids"; then ok "a stop during backoff leaves no child of any generation"
    else bad "stop during backoff: rc=$rc pids=$(cat "$tmp/orphan.pids")"; fi
else bad "orphan case did not restart twice: $(state orphan)"; fi
rc=0
# shellcheck disable=SC2016 # expanded by the inner bash
env WARDYN_SERVICE_STATE_FILE="$tmp/orphannever.json" WARDYN_SERVICE_RESTART=never \
    bash -c 'set -euo pipefail; . "$1"; run_service "$2"' _ "$LIB" \
    "sleep 300 & echo \$! > '$tmp/orphannever.pids'; exit 3" >/dev/null 2>&1 || rc=$?
if [[ $rc == 3 ]] && ! alive "$tmp/orphannever.pids"; then ok "restart=never kills the child and keeps the command's code (3)"
else bad "restart=never orphan: rc=$rc state=$(state orphannever) pids=$(cat "$tmp/orphannever.pids" 2>/dev/null)"; fi

# 5) failing probe past the threshold with restart=never: unhealthy, still running.
port="$(free_port)"
start probenever 'sleep 300' WARDYN_SERVICE_RESTART=never WARDYN_SERVICE_PORT="$port" \
    WARDYN_SERVICE_HEALTH_INTERVAL=1 WARDYN_SERVICE_HEALTH_THRESHOLD=2
if wait_state probenever '"health":"starting"' 2; then ok "a probed service starts as starting"
else bad "first state: $(state probenever)"; fi
if wait_state probenever '"reason":"probe_failing"' 6 && kill -0 "$sup" 2>/dev/null; then
    want='{"v":1,"health":"unhealthy","reason":"probe_failing","restarts":0,"last_exit_code":null}'
    if [[ "$(state probenever)" == "$want" ]]; then ok "threshold under restart=never: $want, still running"
    else bad "probe never state: $(state probenever)"; fi
else
    bad "restart=never probe: $(state probenever)"
fi
stop "$sup"

# 6) failing probe past the threshold with on_failure: TERM the group, restart.
port="$(free_port)"
start probe 'sleep 300' WARDYN_SERVICE_PORT="$port" \
    WARDYN_SERVICE_HEALTH_INTERVAL=1 WARDYN_SERVICE_HEALTH_THRESHOLD=2
if wait_state probe '"restarts":1' 8; then
    log="$(cat "$tmp/probe.log")"
    if [[ "$log" == *"failed 2 health checks in a row"* && "$(state probe)" == *'"last_exit_code":143'* ]]; then
        ok "threshold restarts the service (killed by TERM, 143)"
    else bad "probe restart: $(state probe) $log"; fi
else
    bad "failing probe did not restart: $(state probe)"
fi
stop "$sup"

# 7) passing probes: TCP connect, and HTTP 2xx on the health path.
if command -v socat >/dev/null 2>&1; then
    port="$(free_port)"
    start tcp "exec socat TCP-LISTEN:$port,bind=127.0.0.1,reuseaddr,fork EXEC:/bin/true" \
        WARDYN_SERVICE_PORT="$port" WARDYN_SERVICE_HEALTH_INTERVAL=1 WARDYN_SERVICE_HEALTH_THRESHOLD=2
    if wait_state tcp '"health":"healthy"' 6; then ok "TCP probe passes: healthy"
    else bad "TCP probe: $(state tcp)"; fi
    stop "$sup"

    printf '#!/bin/sh\nread -r l\nprintf "HTTP/1.0 %%s\\r\\n\\r\\n" "$1"\n' > "$tmp/http.sh"
    chmod +x "$tmp/http.sh"
    port="$(free_port)"
    start http200 "exec socat TCP-LISTEN:$port,bind=127.0.0.1,reuseaddr,fork SYSTEM:'$tmp/http.sh \"204 No Content\"'" \
        WARDYN_SERVICE_PORT="$port" WARDYN_SERVICE_HEALTH_PATH=/healthz \
        WARDYN_SERVICE_HEALTH_INTERVAL=1 WARDYN_SERVICE_HEALTH_THRESHOLD=2
    if wait_state http200 '"health":"healthy"' 6; then ok "HTTP probe 2xx passes: healthy"
    else bad "HTTP 2xx probe: $(state http200) $(cat "$tmp/http200.log")"; fi
    stop "$sup"

    port="$(free_port)"
    start http500 "exec socat TCP-LISTEN:$port,bind=127.0.0.1,reuseaddr,fork SYSTEM:'$tmp/http.sh \"500 Oops\"'" \
        WARDYN_SERVICE_PORT="$port" WARDYN_SERVICE_HEALTH_PATH=/healthz WARDYN_SERVICE_RESTART=never \
        WARDYN_SERVICE_HEALTH_INTERVAL=1 WARDYN_SERVICE_HEALTH_THRESHOLD=2
    if wait_state http500 '"reason":"probe_failing"' 6; then ok "HTTP probe 5xx fails: probe_failing"
    else bad "HTTP 5xx probe: $(state http500)"; fi
    stop "$sup"
else
    echo "skip: socat not installed; TCP and HTTP pass cases not run"
fi

# 8) the whole probe is bounded to 5 s: a connect a full backlog never answers,
# and an HTTP status line a silent listener never sends, each fail in time. The
# listeners need python3; skipped without it.
if command -v python3 >/dev/null 2>&1; then
    python3 -I -c '
import socket
full = socket.socket(); full.bind(("127.0.0.1", 0)); full.listen(0)
fill = [socket.socket() for _ in range(4)]
for c in fill:
    c.setblocking(False); c.connect_ex(full.getsockname())
silent = socket.socket(); silent.bind(("127.0.0.1", 0)); silent.listen(8)
print(full.getsockname()[1], silent.getsockname()[1], flush=True)
held = []
while True:
    held.append(silent.accept()[0])
' > "$tmp/hung.ports" &
    sups+=("$!")
    for ((i = 0; i < 50; i++)); do [[ -s "$tmp/hung.ports" ]] && break; sleep 0.1; done
    read -r full_port silent_port < "$tmp/hung.ports"
    # probe_bounded <what> <port> [path] — the probe fails, within 7 s.
    probe_bounded() {
        local s=$SECONDS prc=0
        # shellcheck disable=SC2016 # expanded by the inner bash
        timeout 20 bash -c '. "$1"; service_probe "$2" "$3"' _ "$LIB" "$2" "${3:-}" || prc=$?
        if (( prc != 0 && SECONDS - s <= 7 )); then ok "a probe whose $1 never answers fails in $((SECONDS - s))s"
        else bad "$1 probe: rc=$prc after $((SECONDS - s))s"; fi
    }
    probe_bounded "TCP connect" "$full_port"
    probe_bounded "HTTP read" "$silent_port" /healthz
else
    echo "skip: python3 not installed; the hung-probe cases not run"
fi

echo "--- test-service-supervisor: $([[ $fail -eq 0 ]] && echo PASS || echo FAIL) ---"
exit "$fail"
