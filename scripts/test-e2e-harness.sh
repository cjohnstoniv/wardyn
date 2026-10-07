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
cat > "${tmp}/bin/pnpm" <<'PY'
#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import stat
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
Path(os.environ["E2E_TEST_EVENTS"], f"client-{os.getpid()}.json").write_text(json.dumps({
    "token_hash": hashlib.sha256(token.encode()).hexdigest(),
    "db": os.environ["WARDYN_E2E_PG_DBNAME"], "url": url,
}))
Path(os.environ["PLAYWRIGHT_JSON_OUTPUT_NAME"]).write_text(json.dumps({
    "stats": {"expected": 1, "unexpected": 0, "flaky": 0, "skipped": 0}, "suites": [],
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
log "test-e2e-harness: PASS"
