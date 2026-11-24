#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Image gate for the sandbox browser lockdown (issue #2970), run against the sign-in image
# (deploy/images/novnc-signin), the Wardyn image that ships Chromium. It installs the lockdown's
# policy as the runner delivers it (root-owned 0644, in a directory the image does not ship), with
# the runner's hardening and no network, and checks as the agent's uid:
#   I-0   the image ships no /etc/chromium/policies;
#   I-1   the agent cannot append to, remove, chmod or move aside the policy or its directory;
#   I-1b  the runner's archive form (one file, no directory entries) leaves every level root-owned;
#   I-2   a Chromium the agent starts with --load-extension loads no extension (I-2c: it does without the policy);
#   I-2d  nor through CDP Extensions.loadUnpacked over the pipe (I-2dc: it does without the policy);
#   I-3   wardyn-signin browser opens no port, refuses a port flag, and ignores CHROMIUM_FLAGS and CHROME_FLAGS;
#   I-3b  cdp --agent --session drives a page under the policy (I-3bc: and without it);
#   I-4   the sign-in gate passes with the policy installed;
#   I-5   records which policy keys leave the pipe and page sessions working (never fails);
#   I-6   the agent cannot mask the policy directory in a user namespace of its own.
# MEASURE lines are the measurements.
#
# Usage: scripts/test-image-browser-lockdown.sh   (exit 0 = PASS, non-zero = FAIL)
#   IMAGE (default wardyn/agent-novnc-signin:local), DOCKER_HOST (default the
#   wardyn-docker daemon, unix:///run/wardyn-docker.sock).
set -euo pipefail

IMAGE="${IMAGE:-wardyn/agent-novnc-signin:local}"
export DOCKER_HOST="${DOCKER_HOST:-unix:///run/wardyn-docker.sock}"
P="wardyn-lockdown-gate-$$"
fail() { echo "test-image-browser-lockdown: FAIL: $*" >&2; exit 1; }
pass() { echo "test-image-browser-lockdown: ok: $*"; }
trap 'docker rm -f "$P-on" "$P-off" "$P-tar" "$P-i5" "$P-i5b" >/dev/null 2>&1 || true' EXIT

# browserpolicy.Render(nil), byte for byte: TestRender_BaselineAlone pins the same bytes.
BASELINE='{"DeveloperToolsAvailability":2,"ExtensionInstallBlocklist":["*"]}'
POLICY=/etc/chromium/policies/managed/wardyn.json

run_c() {
	docker run -d --name "$1" --network none --memory 2g \
		--cap-drop ALL --security-opt no-new-privileges \
		--tmpfs /tmp:rw,nosuid,nodev,noexec,size=256m \
		--tmpfs /run/wardyn/signin:rw,nosuid,nodev,noexec,size=256m,uid=1001,gid=1001,mode=0700 \
		"$IMAGE" sh -c 'while :; do sleep 3600; done' >/dev/null
}
agent() { local c=$1; shift; docker exec -i -u 1000:1000 "$c" "$@"; }
root() { local c=$1; shift; docker exec -i -u 0 "$c" "$@"; }
install_policy() { printf '%s' "$2" | root "$1" sh -c "mkdir -p ${POLICY%/*} && cat > $POLICY && chmod 0644 $POLICY"; }
# listeners C: the container's listening TCP ports, from /proc/net (no ss needed).
listeners() { agent "$1" cat /proc/net/tcp /proc/net/tcp6 2>/dev/null | awk '$4 == "0A" { split($2, a, ":"); print a[2] }' | while read -r h; do echo $((16#$h)); done | sort -un; }
stop_browsers() { agent "$1" pkill -u agent -x wardyn-signin || true; agent "$1" pkill -u agent chromium || true; sleep 1; }
# start_browser C ARGS...: wardyn-signin browser as the agent, until cdp --agent answers.
start_browser() {
	local c=$1; shift
	docker exec -d -u 1000:1000 "$c" "$@"
	for _ in $(seq 100); do agent "$c" wardyn-signin cdp --agent Browser.getVersion >/dev/null 2>&1 && return 0; sleep 0.2; done
	return 1
}
pyget() { agent "$1" python3 -I -c 'import json,sys; v=json.load(sys.stdin)
for k in sys.argv[1:]: v=v[k]
print(v)' "${@:2}"; }
# session_ok C: a page created and attached with flatten evaluates 1+1 to 2 through cdp --agent --session.
session_ok() {
	local c=$1 t s v
	t=$(agent "$c" wardyn-signin cdp --agent Target.createTarget '{"url":"about:blank"}' | pyget "$c" targetId) || return 1
	s=$(agent "$c" wardyn-signin cdp --agent Target.attachToTarget "{\"targetId\":\"$t\",\"flatten\":true}" | pyget "$c" sessionId) || return 1
	v=$(agent "$c" wardyn-signin cdp --agent --session "$s" Runtime.evaluate '{"expression":"1+1","returnByValue":true}' | pyget "$c" result value) || return 1
	[ "$v" = 2 ]
}
# port_opens C: a Chromium the agent starts with --remote-debugging-port=9222 listens within 5 s.
port_opens() {
	local c=$1
	docker exec -d -u 1000:1000 "$c" chromium --headless=new --no-sandbox --user-data-dir=/tmp/p5 --remote-debugging-port=9222
	for _ in $(seq 25); do listeners "$c" | grep -qx 9222 && return 0; sleep 0.2; done
	return 1
}
write_ext() {
	agent "$1" sh -c 'mkdir -p /tmp/ext && echo "self.addEventListener(\"install\", () => {});" > /tmp/ext/bg.js && cat > /tmp/ext/manifest.json' \
		<<<'{"manifest_version":3,"name":"lockdown-gate","version":"1","background":{"service_worker":"bg.js"}}'
}
# ext_loads C: a Chromium the agent starts itself with --load-extension shows a chrome-extension:// target within 10 s.
ext_loads() {
	local c=$1 port=""
	docker exec -d -u 1000:1000 "$c" chromium --headless=new --no-sandbox --user-data-dir=/tmp/p2 --remote-debugging-port=0 --load-extension=/tmp/ext
	for _ in $(seq 50); do port=$(agent "$c" sed -n 1p /tmp/p2/DevToolsActivePort 2>/dev/null || true); [ -n "$port" ] && break; sleep 0.2; done
	[ -n "$port" ] || fail "$c: the agent's Chromium wrote no DevToolsActivePort"
	agent "$c" python3 -I -c 'import json,sys,time,urllib.request
o = urllib.request.build_opener(urllib.request.ProxyHandler({}))
for _ in range(20):
    t = json.load(o.open("http://127.0.0.1:%s/json/list" % sys.argv[1], timeout=2))
    if any(x.get("url", "").startswith("chrome-extension://") for x in t): sys.exit(0)
    time.sleep(0.5)
sys.exit(1)' "$port"
}
# I2D: the agent's Chromium on the DevTools pipe with --enable-unsafe-extension-debugging; prints
# whether Extensions.loadUnpacked answered with a result and whether an extension target appeared.
read -r -d '' I2D <<'PY' || true
import json, os, select, subprocess, time
fds = [os.dup(f) for f in os.pipe() + os.pipe()]
r1, w1, r2, w2 = fds
os.dup2(r1, 3); os.dup2(w2, 4)
p = subprocess.Popen(["chromium", "--headless=new", "--no-sandbox", "--user-data-dir=/tmp/p2d", "--remote-debugging-pipe",
                      "--enable-unsafe-extension-debugging"], pass_fds=(3, 4), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
os.close(3); os.close(4); os.close(r1); os.close(w2)
buf = b""
def call(i, method, params):
    global buf
    os.write(w1, json.dumps({"id": i, "method": method, "params": params}).encode() + b"\0")
    end = time.time() + 20
    while time.time() < end:
        while b"\0" in buf:
            msg, buf = buf.split(b"\0", 1)
            m = json.loads(msg)
            if m.get("id") == i:
                return m
        if select.select([r2], [], [], 1)[0]:
            chunk = os.read(r2, 1 << 16)
            if not chunk:
                break
            buf += chunk
    return {}
load = call(1, "Extensions.loadUnpacked", {"path": "/tmp/ext"})
time.sleep(10)
infos = call(2, "Target.getTargets", {}).get("result", {}).get("targetInfos", [])
p.kill()
ext = any(t.get("url", "").startswith("chrome-extension://") for t in infos)
print("loaded=%s target=%s" % ("yes" if "result" in load else "no", "yes" if ext else "no"))
PY
# tar_one_file: a one-file archive with no directory entries, root 0644, the way the runner builds it.
tar_one_file() {
	python3 -I -c 'import io,sys,tarfile
data = sys.argv[1].encode()
ti = tarfile.TarInfo("etc/chromium/policies/managed/wardyn.json")
ti.size, ti.mode, ti.uid, ti.gid, ti.uname, ti.gname = len(data), 0o644, 0, 0, "", ""
with tarfile.open(fileobj=sys.stdout.buffer, mode="w|") as t:
    t.addfile(ti, io.BytesIO(data))' "$BASELINE"
}

ON="$P-on" OFF="$P-off"
run_c "$ON"
run_c "$OFF"

# I-0. The image ships no policy directory.
if root "$ON" ls -ld /etc/chromium/policies >/dev/null 2>&1; then
	echo "MEASURE I-0 policies-dir=present"
	fail "the image ships /etc/chromium/policies; remove it in deploy/images/novnc-signin/Dockerfile"
fi
echo "MEASURE I-0 policies-dir=absent"
install_policy "$ON" "$BASELINE"
[ "$(root "$ON" cat "$POLICY")" = "$BASELINE" ] || fail "the installed policy is not the baseline's bytes"

# I-1. The agent cannot change the policy.
agent "$ON" sh -c "echo x >> $POLICY" 2>/dev/null && fail "the agent appended to the policy"
agent "$ON" mv /etc/chromium/policies/managed /etc/chromium/policies/aside 2>/dev/null && fail "the agent moved the policy directory aside"
agent "$ON" mv /etc/chromium/policies /etc/chromium/aside 2>/dev/null && fail "the agent moved /etc/chromium/policies aside"
agent "$ON" rm -f "$POLICY" 2>/dev/null && fail "the agent removed the policy"
agent "$ON" chmod 666 "$POLICY" 2>/dev/null && fail "the agent chmodded the policy"
[ "$(root "$ON" cat "$POLICY")" = "$BASELINE" ] || fail "the policy changed under the agent"
pass "I-1 the agent cannot write, remove, chmod or move aside the policy"

# I-1b. The runner's archive form.
run_c "$P-tar"
tar_one_file | docker cp - "$P-tar":/
n=0
while read -r m step owner mode name; do
	echo "$m $step $owner $mode $name"
	n=$((n + 1))
	[ "$owner" = root:root ] || fail "I-1b $name is owned by $owner"
	(((8#$mode & 8#022) == 0)) || fail "I-1b $name has mode $mode, writable by group or others"
done < <(root "$P-tar" stat -c 'MEASURE I-1b %U:%G %a %n' /etc/chromium /etc/chromium/policies /etc/chromium/policies/managed "$POLICY")
[ "$n" = 4 ] || fail "I-1b stat read $n of the 4 levels"
[ "$(root "$P-tar" stat -c %a "$POLICY")" = 644 ] || fail "I-1b the file is not 0644"
pass "I-1b the archive form leaves every level root-owned and unwritable by others"

# I-2 and I-2c. An extension the agent loads itself.
write_ext "$ON"
write_ext "$OFF"
ext_loads "$ON" && fail "I-2 an extension loaded under the policy"
ext_loads "$OFF" || fail "I-2c control: the extension did not load without the policy, so I-2 proves nothing"
stop_browsers "$ON"
stop_browsers "$OFF"
pass "I-2 --load-extension loads nothing under the policy, and does without it"

# I-2d and I-2dc. Extensions.loadUnpacked over the pipe.
on=$(agent "$ON" python3 -I -c "$I2D")
off=$(agent "$OFF" python3 -I -c "$I2D")
echo "MEASURE I-2d $on"
echo "MEASURE I-2dc $off"
[ "$on" != "loaded=yes target=yes" ] || fail "I-2d Extensions.loadUnpacked loaded an extension under the policy"
[ "$off" = "loaded=yes target=yes" ] || fail "I-2dc control: Extensions.loadUnpacked did not load without the policy, so I-2d proves nothing"
pass "I-2d Extensions.loadUnpacked loads nothing under the policy, and does without it"

# I-3. Wardyn's launcher opens no port.
before=$(listeners "$ON")
start_browser "$ON" wardyn-signin browser --headless || fail "I-3 wardyn-signin browser did not answer under the policy"
[ "$(listeners "$ON")" = "$before" ] || fail "I-3 wardyn-signin browser opened a listener"
agent "$ON" wardyn-signin browser --remote-debugging-port=9222 >/dev/null 2>&1 && fail "I-3 wardyn-signin browser took --remote-debugging-port"
stop_browsers "$ON"
start_browser "$ON" env CHROMIUM_FLAGS=--remote-debugging-port=9222 CHROME_FLAGS=--remote-debugging-port=9223 wardyn-signin browser --headless ||
	fail "I-3 wardyn-signin browser did not answer with CHROMIUM_FLAGS set"
listeners "$ON" | grep -qxE '9222|9223' && fail "I-3 CHROMIUM_FLAGS or CHROME_FLAGS opened a port"
pass "I-3 wardyn-signin browser opens no port, by argument or by environment"

# I-3b and I-3bc. A page session through the broker.
if session_ok "$ON"; then i3b=ok; else i3b=refused; fi
echo "MEASURE I-3b session=$i3b"
[ "$i3b" = ok ] || fail "I-3b cdp --agent --session does not drive a page under the policy"
start_browser "$OFF" wardyn-signin browser --headless || fail "I-3bc wardyn-signin browser did not answer without the policy"
session_ok "$OFF" || fail "I-3bc control: cdp --agent --session does not drive a page without the policy"
pass "I-3b cdp --agent --session evaluates 1+1 under the policy, and without it"

# I-5. Which keys keep the pipe and sessions: recorded, never failed.
if port_opens "$ON"; then port=listening; else port=none; fi
echo "MEASURE I-5a pipe=ok session=$i3b port=$port"
measure() {
	local c=$1 policy=$2 label=$3 pipe=refused session=refused port=none
	run_c "$c"
	install_policy "$c" "$policy"
	if start_browser "$c" wardyn-signin browser --headless; then
		pipe=ok
		if session_ok "$c"; then session=ok; fi
	fi
	if port_opens "$c"; then port=listening; fi
	echo "MEASURE $label pipe=$pipe session=$session port=$port"
}
measure "$P-i5" '{"DeveloperToolsAvailability":2,"ExtensionInstallBlocklist":["*"],"RemoteDebuggingAllowed":false}' I-5
measure "$P-i5b" '{"ExtensionInstallBlocklist":["*"]}' I-5b

# I-6. No user-namespace mask over the policy.
if agent "$ON" unshare -Urm sh -c "mount -t tmpfs none ${POLICY%/*} && ls ${POLICY%/*}" >/dev/null 2>&1; then
	echo "MEASURE I-6 userns-mask=possible"
	fail "I-6 the agent masked the policy directory in a user namespace"
fi
echo "MEASURE I-6 userns-mask=refused"
pass "I-6 the agent cannot mask the policy directory"

# I-4. The sign-in gate with the policy installed.
BROWSER_POLICY_PRESEED=1 "$(dirname "$0")/test-image-novnc-signin.sh" || fail "I-4 the sign-in gate fails with the policy installed"
pass "I-4 the sign-in gate passes with the policy installed"
echo "test-image-browser-lockdown: PASS"
