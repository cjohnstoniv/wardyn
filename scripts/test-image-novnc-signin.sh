#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Image gate for desktop sign-ins (issue #2972): a cookie set in the person's
# browser profile is invisible to the agent until the person shares that one
# site, and a share carries that site's cookies and no other's.
#
# It runs the desktop image with the runner's hardening (no capabilities,
# no-new-privileges, /tmp tmpfs, the person's store as a uid-1001 0700 tmpfs)
# and no network, then drives it the way wardynd does, one docker exec per
# step, each as the uid wardynd uses:
#   1. pid 1 is stopped from outside and pause-agent stops and proves the rest;
#   2. the person's display starts; its VNC and noVNC answer on store sockets;
#   3. a cookie for site.test and one for other.test are set in the person's browser;
#   4. the agent cannot read the store, signal the person's processes, run the
#      person's subcommands, or find either cookie in its own browser;
#   5. the person's proxy forwarder serves the person's uid only and adds the
#      lane credential, which the agent's attempt never reaches;
#   6. export --site site.test | import --site site.test puts site.test's cookie,
#      and not other.test's, into the agent's browser;
#   7. stop leaves no person process, and resume-agent continues the agent.
#
# Usage: scripts/test-image-novnc-signin.sh   (exit 0 = PASS, non-zero = FAIL)
#   IMAGE (default wardyn/agent-novnc:local), DOCKER_HOST (default the
#   wardyn-docker daemon, unix:///run/wardyn-docker.sock).
set -euo pipefail

IMAGE="${IMAGE:-wardyn/agent-novnc:local}"
export DOCKER_HOST="${DOCKER_HOST:-unix:///run/wardyn-docker.sock}"
C="wardyn-signin-gate-$$"
fail() { echo "test-image-novnc-signin: FAIL: $*" >&2; exit 1; }
pass() { echo "test-image-novnc-signin: ok: $*"; }
trap 'docker rm -f "$C" >/dev/null 2>&1 || true' EXIT

docker run -d --name "$C" --network none --memory 2g \
	--cap-drop ALL --security-opt no-new-privileges \
	--tmpfs /tmp:rw,nosuid,nodev,noexec,size=256m \
	--tmpfs /run/wardyn/signin:rw,nosuid,nodev,noexec,size=256m,uid=1001,gid=1001,mode=0700 \
	"$IMAGE" sh -c 'while :; do sleep 3600; done' >/dev/null
person() { docker exec -i -u 1001:1001 "$C" "$@"; }
agent() { docker exec -i -u 1000:1000 "$C" "$@"; }
root() { docker exec -i -u 0 "$C" "$@"; }
state_of() { agent ps -o stat= -p "$1" | tr -d ' ' | cut -c1; }

# A stand-in for the run proxy's sign-in listener on loopback: it records each
# request head and answers 200.
root sh -c 'cat > /run/fakeproxy.py' <<'PY'
import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 3129))
s.listen(8)
log = open("/run/fakeproxy.log", "a", buffering=1)
while True:
    c, _ = s.accept()
    data = b""
    while b"\r\n\r\n" not in data and len(data) < 65536:
        d = c.recv(4096)
        if not d:
            break
        data += d
    log.write(data.split(b"\r\n\r\n")[0].decode("latin1").replace("\r\n", " | ") + "\n")
    c.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
    c.close()
PY
root sh -c ': > /run/fakeproxy.log'
docker exec -d -u 0 "$C" python3 /run/fakeproxy.py

# 1. The agent's own process, then the pause.
docker exec -d -u 1000:1000 "$C" sleep 1000
for _ in $(seq 50); do agent_pid=$(agent pgrep -u agent -x sleep -n 2>/dev/null || true); [ -n "$agent_pid" ] && break; sleep 0.1; done
[ -n "${agent_pid:-}" ] || fail "the agent's process did not start"
docker kill --signal=STOP "$C" >/dev/null
pause_record=$(agent wardyn-signin pause-agent) || fail "pause-agent failed"
[ "$(state_of "$agent_pid")" = T ] || fail "the agent's process is not stopped after pause-agent"
[ "$(state_of 1)" = T ] || fail "pid 1 is not stopped"
pass "the agent is stopped (pid 1 from outside, the rest by pause-agent)"

# 2. The person's display.
cred=$(head -c32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')
printf '{"credential":"%s","proxy":"127.0.0.1:3129"}' "$cred" | person wardyn-signin start || fail "start failed"
# Read whole answers before matching: under pipefail a reader that stops early
# fails the writer with SIGPIPE.
banner=$(person timeout 5 socat -T2 - UNIX-CONNECT:/run/wardyn/signin/vnc.sock </dev/null || true)
[ "${banner:0:7}" = "RFB 003" ] || fail "no RFB banner on the person's VNC socket"
page=$(printf 'GET /vnc.html HTTP/1.0\r\n\r\n' | person timeout 5 socat - UNIX-CONNECT:/run/wardyn/signin/ws.sock || true)
[[ "$page" == "HTTP/1.1 200 "* ]] || fail "noVNC is not served on the person's websocket socket"
pass "the person's display answers on its store sockets (VNC and noVNC)"

# 3. Two sites' cookies in the person's browser.
person wardyn-signin cdp Storage.setCookies '{"cookies":[{"name":"sid","value":"PERSON-SITE-SECRET","domain":".site.test","path":"/","secure":true,"httpOnly":true},{"name":"oid","value":"PERSON-OTHER-SECRET","domain":"other.test","path":"/"}]}' >/dev/null ||
	fail "setting the person's cookies failed"
jar=$(person wardyn-signin cdp Storage.getCookies)
[[ "$jar" == *PERSON-SITE-SECRET* ]] || fail "the person's browser does not hold the cookie"

# 4. What the agent cannot do.
agent ls /run/wardyn/signin >/dev/null 2>&1 && fail "the agent can list the person's store"
agent cat /run/wardyn/signin/cred >/dev/null 2>&1 && fail "the agent can read the lane credential"
root cat /run/wardyn/signin/cred >/dev/null 2>&1 && fail "uid 0 without capabilities can read the lane credential"
agent wardyn-signin export --site site.test >/dev/null 2>&1 && fail "the agent ran the person's export"
agent wardyn-signin cdp Storage.getCookies >/dev/null 2>&1 && fail "the agent reached the person's browser control"
browser_pid=$(agent pgrep -u person -x chromium -o) || fail "no person browser process"
agent kill -0 "$browser_pid" 2>/dev/null && fail "the agent can signal the person's browser"
agent sh -c "cat /proc/$browser_pid/environ" >/dev/null 2>&1 && fail "the agent can read the person's browser environment"
agent sh -c "grep -rls PERSON-SITE-SECRET /tmp /home/agent /dev/shm 2>/dev/null" && fail "the person's cookie is readable by the agent"
docker exec -d -u 1000:1000 "$C" wardyn-signin browser --headless about:blank
for _ in $(seq 100); do agent wardyn-signin cdp --agent Browser.getVersion >/dev/null 2>&1 && break; sleep 0.2; done
jar=$(agent wardyn-signin cdp --agent Storage.getCookies) || fail "the agent's browser did not start"
case "$jar" in *PERSON-*) fail "the agent's browser holds a person cookie before any share";; esac
pass "before a share the agent sees no person cookie, no store file, no person process"

# 5. The forwarder: the person's uid only, with the credential added.
args=$(agent ps -u person -o args=)
port=$(sed -n 's/.*--proxy-server=http:\/\/127\.0\.0\.1:\([0-9]*\).*/\1/p' <<<"$args" | sed -n 1p)
[ -n "$port" ] || fail "no forwarder port on the person's browser"
# The person's browser makes requests of its own through the lane, so count
# site.test's lines rather than expecting an empty log.
site_lines() { root cat /run/fakeproxy.log | grep -c '^CONNECT site.test:443 ' || true; }
agent curl -s -m 5 -o /dev/null -x "http://127.0.0.1:$port" https://site.test/ && fail "the agent's curl was served by the forwarder"
[ "$(site_lines)" -eq 0 ] || fail "the agent's attempt reached the sign-in listener"
person curl -s -m 5 -o /dev/null -x "http://127.0.0.1:$port" https://site.test/ || true
want="Proxy-Authorization: Basic $(printf 'wardyn-signin:%s' "$cred" | base64 -w0)"
seen=$(root cat /run/fakeproxy.log)
grep -qF "CONNECT site.test:443 HTTP/1.1 | Host: site.test:443 | $want" <<<"$seen" || fail "the person's CONNECT did not carry the lane credential"
[ "$(grep -c . <<<"$seen")" -eq "$(grep -c "$want" <<<"$seen")" ] || fail "a request reached the sign-in listener without the credential"
pass "the forwarder serves the person's uid only and adds the credential"

# 6. The share of one site.
shared=$(person wardyn-signin export --site site.test) || fail "export failed"
case "$shared" in *PERSON-SITE-SECRET*) ;; *) fail "the export lacks site.test's cookie";; esac
case "$shared" in *PERSON-OTHER-SECRET*) fail "the export carries other.test's cookie";; esac
printf '%s' "$shared" | agent wardyn-signin import --site site.test >/dev/null || fail "import failed"
for _ in $(seq 50); do jar=$(agent wardyn-signin cdp --agent Storage.getCookies); case "$jar" in *PERSON-SITE-SECRET*) break;; esac; sleep 0.2; done
case "$jar" in *PERSON-SITE-SECRET*) ;; *) fail "the agent's browser did not load the shared login";; esac
case "$jar" in *PERSON-OTHER-SECRET*) fail "the agent's browser holds the unshared site's cookie";; esac
pass "after a share of site.test the agent holds site.test's cookie and not other.test's"

# 7. End of the sign-in.
person wardyn-signin stop || fail "stop failed"
# Zombies wait for pid 1 to reap them (it is stopped until the resume); they hold nothing.
left=$(agent ps -u person -o stat= | grep -vc '^Z' || true)
[ "$left" -eq 0 ] || fail "$left person process(es) left after stop"
printf '%s' "$pause_record" | agent wardyn-signin resume-agent || fail "resume-agent failed"
docker kill --signal=CONT "$C" >/dev/null
[ "$(state_of "$agent_pid")" != T ] || fail "the agent's process is still stopped after resume"
pass "stop leaves no person process; the agent is resumed"
echo "test-image-novnc-signin: PASS"
