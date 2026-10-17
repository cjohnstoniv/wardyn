#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Execute both shipping scripts with fake Docker/Playwright and a real socket
# backend. Inspect getsockname(), not just the wrapper's exported environment.
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${REPO_ROOT}/scripts/lib/common.sh"
source "${REPO_ROOT}/scripts/lib/e2e-network.sh"
tmp="$(mktemp -d)"
cleanup() {
  local file
  for file in "${tmp}/repo/.e2e-bin/"*.pid; do
    [[ ! -f "${file}" ]] || kill "$(cat "${file}")" 2>/dev/null || true
  done
  rm -rf "${tmp}"
}
trap cleanup EXIT
mkdir -p "${tmp}/repo/"{scripts/lib,ui/e2e,.e2e-bin,test/reports/e2e} "${tmp}/bin" "${tmp}/events"
cp "${REPO_ROOT}/scripts/"{run-ui-e2e,e2e-backend}.sh "${tmp}/repo/scripts/"
cp "${REPO_ROOT}/scripts/lib/"{common,e2e-network,e2e-quarantine}.sh "${tmp}/repo/scripts/lib/"
: > "${tmp}/repo/ui/e2e/quarantine.txt"
touch "${tmp}/repo/ui/e2e/"{one,two,three,four,five,six}.spec.ts

cat > "${tmp}/repo/.e2e-bin/wardynd" <<'PY'
#!/usr/bin/env python3
import hashlib
import http.server
import json
import os
from pathlib import Path
import signal
import socket
import sys
import threading
import urllib.error
import urllib.request

args = sys.argv[1:]
if args == ["-gen-age-key"]:
    print("AGE-SECRET-KEY-TEST-FIXTURE")
    sys.exit(0)
flags = dict(zip(args[::2], args[1::2]))
proxy = "-target" in flags
token = os.environ.get("WARDYN_ADMIN_TOKEN", "")
if not proxy:
    assert token, "backend started without a token"

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        if proxy:
            if not self.path.startswith(flags["-prefix"] + "/"):
                self.send_error(404)
                return
            request = urllib.request.Request(flags["-target"] + self.path, headers=dict(self.headers))
            try:
                response = urllib.request.urlopen(request)
            except urllib.error.HTTPError as error:
                response = error
            self.send_response(response.status)
            self.end_headers()
            self.wfile.write(response.read())
            return
        authorized = self.headers.get("Authorization") == "Bearer " + token
        self.send_response(200 if self.path.endswith("/healthz") or authorized else 401)
        self.end_headers()
        self.wfile.write(b"{}")

    do_POST = do_GET
    do_PUT = do_GET

listeners = []
for name in ("-listen", "-ui-sandbox-listen", "-internal-listen"):
    if name not in flags:
        continue
    host, port = flags[name].rsplit(":", 1)
    host = host.strip("[]")
    # Never expose even this inert stub if the security regression returns.
    assert host in ("127.0.0.1", "127.0.0.2", "::1"), f"unsafe effective bind: {flags[name]}"
    class Server(http.server.ThreadingHTTPServer):
        address_family = socket.AF_INET6 if ":" in host else socket.AF_INET
    server = Server((host, int(port)), Handler)
    listeners.append(server.socket.getsockname()[:2])
    threading.Thread(target=server.serve_forever, daemon=True).start()
Path(os.environ["E2E_TEST_EVENTS"], f"bind-{os.getpid()}.json").write_text(json.dumps({
    "listeners": listeners, "proxy": proxy,
    "token_hash": hashlib.sha256(token.encode()).hexdigest() if token else "",
}))
signal.pause()
PY
cp "${tmp}/repo/.e2e-bin/wardynd" "${tmp}/repo/.e2e-bin/wardynd-tmux"
cp "${tmp}/repo/.e2e-bin/wardynd" "${tmp}/repo/.e2e-bin/basepathproxy"

cat > "${tmp}/bin/docker" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${E2E_TEST_EVENTS}/docker.log"
if [[ "$1" == port ]]; then
  echo '127.0.0.1:55432'
elif [[ "$*" == *' -i '* && "$*" != *' -c '* && "$*" != *' -tAc '* ]]; then
  cat >/dev/null
elif [[ "$*" == *' -tAc '* ]]; then
  echo 9
fi
SH
cat > "${tmp}/bin/tmux" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat > "${tmp}/bin/go" <<'SH'
#!/usr/bin/env bash
printf '%s %s\n' "${WARDYN_E2E_ADDR}" "$*" >> "${E2E_TEST_EVENTS}/go.log"
exit 1
SH
cat > "${tmp}/bin/pnpm" <<'PY'
#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import stat
import sys
import time
import urllib.error
import urllib.request

token = os.environ["WARDYN_E2E_TOKEN"]
url = os.environ["WARDYN_E2E_BASE_URL"].rstrip("/") + "/api/v1/runs"
request = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
assert urllib.request.urlopen(request).status == 200, "Playwright token did not reach the backend"
try:
    urllib.request.urlopen(urllib.request.Request(url, headers={"Authorization": "Bearer wardyn-e2e-token"}))
    raise AssertionError("backend still accepts the published token")
except urllib.error.HTTPError as error:
    assert error.code == 401
port = os.environ["WARDYN_E2E_ADDR"].rsplit(":", 1)[1]
token_file = Path("../.e2e-bin", "token-" + port)
assert stat.S_IMODE(token_file.stat().st_mode) == 0o600
spec = next(arg for arg in sys.argv if arg.endswith(".spec.ts"))
Path(os.environ["E2E_TEST_EVENTS"], f"client-{os.getpid()}.json").write_text(json.dumps({
    "token_hash": hashlib.sha256(token.encode()).hexdigest(),
    "db": os.environ["WARDYN_E2E_PG_DBNAME"], "url": url, "spec": spec,
}))
# E2E_TEST_FLAKY=<spec basename>[,<spec basename>...] marks those specs' single
# test flaky, the shape a pass-only-on-retry produces.
if Path(spec).name in os.environ.get("E2E_TEST_FLAKY", "").split(","):
    stats, suites = {"expected": 0, "unexpected": 0, "flaky": 1, "skipped": 0}, [
        {"title": "", "specs": [{"title": "flaky on a retry", "tests": [{"status": "flaky"}]}]}]
else:
    stats, suites = {"expected": 1, "unexpected": 0, "flaky": 0, "skipped": 0}, []
Path(os.environ["PLAYWRIGHT_JSON_OUTPUT_NAME"]).write_text(json.dumps({
    "stats": stats, "suites": suites,
}))
time.sleep(0.5)
PY
chmod +x "${tmp}/bin/"* "${tmp}/repo/.e2e-bin/"*
for name in ${!WARDYN_E2E_@}; do unset "${name}"; done
export PATH="${tmp}/bin:${PATH}" E2E_TEST_EVENTS="${tmp}/events"
export DOCKER_HOST=unix:///unused-e2e-test.sock WARDYN_DOCKER_SOCK=/unused-e2e-test.sock
export WARDYN_E2E_SKIP_BUILD=1 WARDYN_E2E_PG_CONTAINER=fake-e2e-pg
cd "${tmp}/repo"

(
  source ./scripts/e2e-backend.sh build >/dev/null
  [[ "${ADDR}" == 127.0.0.1:8088 && "${UI_ADDR}" == 127.0.0.1:8089 &&
     "${INTERNAL_ADDR}" == 127.0.0.1:8443 && "${PROXY_ADDR}" == 127.0.0.1:8090 ]]
) || die "standalone backend defaults are not all loopback"

run_wrapper() {
  rm -f "${E2E_TEST_EVENTS}/"*
  ./scripts/run-ui-e2e.sh "$@" > "${tmp}/wrapper.log" 2>&1 || { cat "${tmp}/wrapper.log"; die "canonical wrapper failed"; }
  [[ -z "$(find .e2e-bin -name 'token-*' -print -quit)" ]] || die "down retained a bearer file"
}
check_events() {
  python3 - "$@" <<'PY'
import hashlib
import json
import os
from pathlib import Path
import sys

expected_clients, expected_dbs, prefix, host = sys.argv[1:]
events = Path(os.environ["E2E_TEST_EVENTS"])
clients = [json.loads(p.read_text()) for p in events.glob("client-*.json")]
binds = [json.loads(p.read_text()) for p in events.glob("bind-*.json")]
assert len(clients) == int(expected_clients), clients
assert len({c["db"] for c in clients}) == int(expected_dbs), clients
tokens = {c["token_hash"] for c in clients}
if os.environ.get("WARDYN_E2E_TOKEN"):
    assert tokens == {hashlib.sha256(os.environ["WARDYN_E2E_TOKEN"].encode()).hexdigest()}
else:
    assert len(tokens) == len(clients), "token reused across ups or lanes"
assert tokens == {b["token_hash"] for b in binds if not b["proxy"]}
assert all(c["url"].endswith(prefix + "/api/v1/runs") for c in clients), clients
assert len([b for b in binds if b["proxy"]]) == (len(clients) if prefix else 0)
assert all(addr[0] == host for b in binds for addr in b["listeners"]), binds
PY
}

touch ui/e2e/cockpit-terminal-tmux-probe.spec.ts
for setting in WARDYN_E2E_ADDR=0.0.0.0:8088 WARDYN_E2E_ADDR=example.com:8088 \
    WARDYN_E2E_UI_ADDR=192.0.2.1:8089 'WARDYN_E2E_INTERNAL_ADDR=[::]:8443' \
    WARDYN_E2E_PROXY_ADDR=0.0.0.0:8090; do
  for skip_build in 0 1; do
    rm -f "${E2E_TEST_EVENTS}/"*
    if env WARDYN_E2E_BASE_PATH=/wardyn WARDYN_E2E_SKIP_BUILD="${skip_build}" "${setting}" \
        ./scripts/run-ui-e2e.sh cockpit-terminal-tmux-probe > "${tmp}/refused.log" 2>&1; then
      die "wrapper accepted ${setting}"
    fi
    grep -q 'refuses non-loopback listener' "${tmp}/refused.log" || { cat "${tmp}/refused.log"; die "wrong wrapper refusal"; }
    [[ -z "$(find "${E2E_TEST_EVENTS}" -type f -print -quit)" ]] || die "unsafe wrapper performed build, Docker, startup or cleanup work"
  done
done
log "canonical wrapper refuses every unsafe tmux listener before build or cleanup, with and without skip-build"

for selection in mixed default tmux-shard; do
  spec_args=() shard_env=()
  [[ "${selection}" != mixed ]] || spec_args=(one cockpit-terminal-tmux-probe)
  [[ "${selection}" != tmux-shard ]] || shard_env=(WARDYN_E2E_SHARD=1/7)
  rm -f "${E2E_TEST_EVENTS}/"*
  if env WARDYN_E2E_ADDR=0.0.0.0:8088 WARDYN_E2E_LANES=3 "${shard_env[@]}" \
      ./scripts/run-ui-e2e.sh "${spec_args[@]}" > "${tmp}/refused.log" 2>&1; then
    die "wrapper accepted an unsafe ${selection} selection"
  fi
  grep -q 'refuses non-loopback listener' "${tmp}/refused.log" || { cat "${tmp}/refused.log"; die "wrong ${selection} refusal"; }
  [[ -z "$(find "${E2E_TEST_EVENTS}" -type f -print -quit)" ]] || die "${selection} performed work before refusing"
done
log "mixed lists, default three-lane runs and selected tmux shards refuse before any work"

# Stop at the build boundary: a non-tmux host must survive selection, but this
# test must never actually open even an inert wildcard listener.
for selection in explicit non-tmux-shard; do
  spec_args=() shard_env=()
  [[ "${selection}" != explicit ]] || spec_args=(one)
  [[ "${selection}" != non-tmux-shard ]] || shard_env=(WARDYN_E2E_SHARD=2/7)
  rm -f "${E2E_TEST_EVENTS}/"*
  if env WARDYN_E2E_ADDR=0.0.0.0:8088 WARDYN_E2E_SKIP_BUILD=0 "${shard_env[@]}" \
      ./scripts/run-ui-e2e.sh "${spec_args[@]}" > "${tmp}/build-stopped.log" 2>&1; then
    die "build stub unexpectedly succeeded"
  fi
  grep -q '^0.0.0.0:8088 build ' "${E2E_TEST_EVENTS}/go.log" || die "${selection} lost its explicit non-tmux host"
  [[ ! -e "${E2E_TEST_EVENTS}/docker.log" ]] || die "failed build triggered database cleanup"
done
rm ui/e2e/cockpit-terminal-tmux-probe.spec.ts
log "explicit non-tmux selections and shards preserve host overrides"

# The specs one run actually executed, sorted. The shard split is pinned by
# these sets, not by the log line the script prints about it.
ran_specs() {
  python3 - "${E2E_TEST_EVENTS}" <<'PY'
import json, pathlib, sys
print(" ".join(sorted(json.loads(p.read_text())["spec"] for p in pathlib.Path(sys.argv[1]).glob("client-*.json"))))
PY
}

# WARDYN_E2E_SHARD=i/n keeps every n-th spec of the byte-sorted list, so a spec
# runs in exactly one shard whatever the runner's locale — a spec that ran in
# both, or in neither, is a coverage hole no later job sees.
for shard in 1/2 2/2 1/3; do
  rm -f "${E2E_TEST_EVENTS}"/*
  env WARDYN_E2E_SHARD="${shard}" ./scripts/run-ui-e2e.sh > "${tmp}/shard.log" 2>&1 \
    || { cat "${tmp}/shard.log"; die "shard ${shard} failed"; }
  case "${shard}" in
    1/2) want="e2e/five.spec.ts e2e/one.spec.ts e2e/three.spec.ts" ;;
    2/2) want="e2e/four.spec.ts e2e/six.spec.ts e2e/two.spec.ts" ;;
    *) want="e2e/five.spec.ts e2e/six.spec.ts" ;;
  esac
  [[ "$(ran_specs)" == "${want}" ]] || die "shard ${shard} ran [$(ran_specs)], want [${want}]"
  log "shard ${shard} runs its own specs and no other"
done

# A shard that holds no spec must refuse, not report a green run that ran
# nothing — and must refuse before any build, database or backend work.
for bad in 7/7 1 0/2 2/2x; do
  rm -f "${E2E_TEST_EVENTS}"/*
  if env WARDYN_E2E_SHARD="${bad}" ./scripts/run-ui-e2e.sh > "${tmp}/refused.log" 2>&1; then
    die "wrapper accepted WARDYN_E2E_SHARD=${bad}"
  fi
  grep -qE 'holds no spec|must be i/n|needs 1 <= i <= n' "${tmp}/refused.log" \
    || { cat "${tmp}/refused.log"; die "wrong WARDYN_E2E_SHARD=${bad} refusal"; }
  [[ -z "$(find "${E2E_TEST_EVENTS}" -type f -print -quit)" ]] || die "WARDYN_E2E_SHARD=${bad} worked before refusing"
done
log "an empty, malformed or out-of-range shard refuses before any work"

run_wrapper one two
check_events 2 1 '' 127.0.0.1
log "per-up rotation, private token handoff and effective default loopback binds"

export WARDYN_E2E_ADDR="127.0.0.2:$(pick_free_port)"
export WARDYN_E2E_UI_ADDR="127.0.0.2:$(pick_free_port)"
export WARDYN_E2E_INTERNAL_ADDR="127.0.0.2:$(pick_free_port)"
export WARDYN_E2E_TOKEN=explicit-private-test-token
run_wrapper one two
check_events 2 1 '' 127.0.0.2
unset WARDYN_E2E_ADDR WARDYN_E2E_UI_ADDR WARDYN_E2E_INTERNAL_ADDR WARDYN_E2E_TOKEN
log "canonical wrapper preserves an explicit bind host and token override"

export WARDYN_E2E_LANES=3 WARDYN_E2E_BASE_PATH=/wardyn
run_wrapper
check_events 6 3 /wardyn 127.0.0.1
unset WARDYN_E2E_LANES WARDYN_E2E_BASE_PATH
log "three concurrent lanes have isolated tokens, databases and base-path proxies"

for setting in WARDYN_E2E_ADDR=0.0.0.0:8088 WARDYN_E2E_ADDR=example.com:8088 \
    WARDYN_E2E_UI_ADDR=192.0.2.1:8089 'WARDYN_E2E_INTERNAL_ADDR=[::]:8443' \
    WARDYN_E2E_PROXY_ADDR=0.0.0.0:8090; do
  rm -f "${E2E_TEST_EVENTS}/"*
  if env WARDYN_E2E_TMUX=1 WARDYN_E2E_BASE_PATH=/wardyn "${setting}" \
      ./scripts/e2e-backend.sh up > "${tmp}/refused.log" 2>&1; then
    die "tmux accepted ${setting}"
  fi
  grep -q 'refuses non-loopback listener' "${tmp}/refused.log" || { cat "${tmp}/refused.log"; die "wrong refusal"; }
  [[ -z "$(find "${E2E_TEST_EVENTS}" -type f -print -quit)" ]] || die "unsafe up performed work before refusing"
done
log "tmux rejects every non-loopback entry point before Docker, build, reset or startup"

touch ui/e2e/cockpit-terminal-tmux-probe.spec.ts
export WARDYN_E2E_ADDR=":$(pick_free_port)" WARDYN_E2E_UI_ADDR="localhost:$(pick_free_port)"
run_wrapper cockpit-terminal-tmux-probe
check_events 1 1 '' 127.0.0.1
log "port-only and localhost overrides stay loopback in the real-tmux lane"

[[ "$(e2e_listen_addr '192.0.2.1:8088')" == '192.0.2.1:8088' ]] || die "explicit non-tmux host lost"
e2e_require_loopback '[::1]:8088' 127.0.0.2:8089

# One checkout runs this wrapper twice: ci.yml's ui-e2e job runs its shard and
# then the governance four-eyes spec as a second call. The flake list is
# APPENDED to, so the second call cannot truncate the shard's own flakes out of
# the file notify-flaky reads (#1880).
FLAKY_TSV=test/reports/e2e/flaky.tsv
flaky_calls=0
for flaky in one three; do
  flaky_calls=$((flaky_calls + 1))
  rm -f "${E2E_TEST_EVENTS}"/*
  rc=0
  env E2E_TEST_FLAKY="${flaky}.spec.ts" ./scripts/run-ui-e2e.sh one three > "${tmp}/flake.log" 2>&1 || rc=$?
  [[ ${rc} -eq 1 ]] || { cat "${tmp}/flake.log"; die "a new flake must fail the gate, got rc=${rc}"; }
  grep -q "^${flaky}.spec.ts"$'\t'"flaky on a retry"$'\t''new$' "${FLAKY_TSV}" \
    || { cat "${FLAKY_TSV}"; die "call on ${flaky} left no classified flake"; }
  [[ "$(wc -l < "${FLAKY_TSV}")" -eq "${flaky_calls}" ]] \
    || { cat "${FLAKY_TSV}"; die "call on ${flaky} dropped an earlier call's flake"; }
done
# A run where nothing flaked leaves the earlier lists in place.
rm -f "${E2E_TEST_EVENTS}"/*
run_wrapper two four
[[ "$(wc -l < "${FLAKY_TSV}")" -eq 2 ]] || { cat "${FLAKY_TSV}"; die "a clean call truncated the flake list"; }
log "consecutive calls append to the flake list; a clean call leaves it alone"

log "test-e2e-harness: PASS"
