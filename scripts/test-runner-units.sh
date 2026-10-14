#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# No-docker, no-systemd, no-launchd checks for the runner packaging in
# deploy/runner/. install.sh runs against stub `id`, `uname`, `docker`,
# `systemctl` and `launchctl` on PATH and a throwaway HOME, so both the Linux
# and the macOS branch execute without touching this machine's service manager.
# The live start/stop proof is cmd/wardyn-runnerd/units_systemd_test.go in CI.
#
# Usage: scripts/test-runner-units.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="${REPO_ROOT}/deploy/runner"
INSTALL="${DIR}/install.sh"
UNIT="${DIR}/wardyn-runnerd.service"
PLIST="${DIR}/com.wardyn.runnerd.plist"

fail() { echo "test-runner-units: FAIL: $*" >&2; exit 1; }

unset DOCKER_HOST
WORK="$(mktemp -d)"
RT_TMP=""
trap 'rm -rf "$WORK" ${RT_TMP:+"$RT_TMP"}' EXIT

bash -n "$INSTALL" || fail "install.sh has a syntax error"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck "$INSTALL" || fail "shellcheck rejected install.sh"
fi

plist_get() { python3 -I -c 'import plistlib,sys; d=plistlib.load(open(sys.argv[1],"rb")); print(eval(sys.argv[2]))' "$1" "$2"; }
plist_get "$PLIST" 'd["Label"]' >/dev/null || fail "the committed plist does not parse"

# Every placeholder install.sh substitutes is still in a committed file.
for ph in $(grep -o '__[A-Z_]*__' "$INSTALL" | sort -u); do
  grep -q "$ph" "$UNIT" "$PLIST" || fail "install.sh substitutes ${ph}, which no committed unit or plist carries"
done

grep -q '^Restart=on-failure$' "$UNIT" || fail "the unit lost Restart=on-failure"
! grep -q '^User=' "$UNIT" || fail "the unit sets User=: it is a user unit"
grep -q '^ExecStart=__WARDYN_RUNNERD__ serve --org ' "$UNIT" || fail "the unit does not run 'serve --org'"

# ---- stubs -----------------------------------------------------------------
BIN="$WORK/bin"
mkdir -p "$BIN"
stub() { printf '#!/usr/bin/env bash\n%s\n' "$2" >"$BIN/$1"; chmod +x "$BIN/$1"; }
stub docker 'exit 0'
stub systemctl 'echo "$*" >>"$STUB_LOG"; case "$*" in *UnitPath*) echo "${STUB_UNITPATH-$HOME/.config/systemd/user /etc/systemd/user}" ;; esac'
stub launchctl 'echo "$*" >>"$STUB_LOG"'

# A runner binary that reports STUB_VERSION and "registers" by writing runner.json.
cat >"$WORK/wardyn-runnerd" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  --version) echo "wardyn-runnerd version ${STUB_VERSION:-0.9.0}" ;;
  register)
    shift; echo registered >>"$STUB_LOG"; echo "register $*" >>"$STUB_LOG"
    d="${STUB_STATE:-${XDG_CONFIG_HOME:-$HOME/.config}/wardyn/runner}"; prev=""
    for a in "$@"; do [ "$prev" = --state-dir ] && d="$a"; prev="$a"; done
    mkdir -p "$d"; : >"$d/runner.json" ;;
esac
STUB
chmod +x "$WORK/wardyn-runnerd"
echo "wdr_stub" >"$WORK/token"

# run_install NAME ARGS... — runs install.sh in a fresh HOME; exports its result in $RC and $OUT.
run_install() {
  local home="$WORK/home-$1"; shift
  mkdir -p "$home"
  export STUB_LOG="$home/stub.log" STUB_STATE
  : >"$STUB_LOG"
  set +e
  OUT="$(HOME="$home" PATH="$BIN:$PATH" env -u XDG_CONFIG_HOME -u XDG_DATA_HOME ${XDG_CFG:+"XDG_CONFIG_HOME=$XDG_CFG"} bash "$INSTALL" "$@" 2>&1)"
  RC=$?
  set -e
}

# ---- refusals ---------------------------------------------------------------
stub id 'echo 0'
STUB_STATE=/dev/null run_install root --org https://wardyn.example --binary "$WORK/wardyn-runnerd" --token-file "$WORK/token" --proxy-image p:1
[ "$RC" -ne 0 ] && grep -q 'run as the person who owns this runner' <<<"$OUT" || fail "install.sh did not refuse to run as root (rc=$RC): $OUT"
rm -f "$BIN/id"

STUB_STATE=/dev/null run_install noorg --binary "$WORK/wardyn-runnerd" --token-file "$WORK/token"
[ "$RC" -ne 0 ] && grep -q -- '--org is required' <<<"$OUT" || fail "install.sh without --org did not refuse (rc=$RC): $OUT"

STUB_STATE=/dev/null STUB_VERSION=0.9.0+abc run_install dev --org https://wardyn.example --binary "$WORK/wardyn-runnerd" --token-file "$WORK/token"
[ "$RC" -ne 0 ] && grep -q 'development build' <<<"$OUT" || fail "a +commit version without --proxy-image did not refuse (rc=$RC): $OUT"

STUB_STATE=/dev/null run_install notoken --org https://wardyn.example --binary "$WORK/wardyn-runnerd" --proxy-image p:1
[ "$RC" -ne 0 ] && grep -q 'register first' <<<"$OUT" || fail "no identity and no token did not refuse (rc=$RC): $OUT"

# ---- Linux: install, reinstall keeps the identity, uninstall ---------------
stub uname 'echo Linux'
H="$WORK/home-linux"
STUB_STATE="$H/.config/wardyn/runner" run_install linux --org https://wardyn.example --binary "$WORK/wardyn-runnerd" --token-file "$WORK/token"
[ "$RC" -eq 0 ] || fail "Linux install failed (rc=$RC): $OUT"
[ -x "$H/.local/share/wardyn/bin/wardyn-runnerd" ] || fail "the binary was not installed"
RENDERED="$H/.config/systemd/user/wardyn-runnerd.service"
grep -q "^ExecStart=$H/.local/share/wardyn/bin/wardyn-runnerd serve --org " "$RENDERED" || fail "ExecStart not rendered"
! grep -q '__[A-Z_]*__' "$RENDERED" || fail "a placeholder survived in the rendered unit"
[ "$(stat -c %a "$H/.config/wardyn/runnerd.env" 2>/dev/null || stat -f %Lp "$H/.config/wardyn/runnerd.env")" = 600 ] || fail "env file is not 0600"
grep -qx 'WARDYN_RUNNER_ORG_URL=https://wardyn.example' "$H/.config/wardyn/runnerd.env" || fail "env file lost the org URL"
grep -qx 'WARDYN_PROXY_IMAGE=ghcr.io/cjohnstoniv/wardyn-proxy:0.9.0' "$H/.config/wardyn/runnerd.env" || fail "env file lost the default proxy image"
grep -qx 'WARDYN_INTERNAL_NETWORK=wardyn-internal' "$H/.config/wardyn/runnerd.env" || fail "env file lost the network"
grep -q 'enable --now wardyn-runnerd.service' "$H/stub.log" || fail "the unit was not enabled and started"
grep -q '^registered$' "$H/stub.log" || fail "the first install did not register"
if command -v systemd-analyze >/dev/null 2>&1; then
  # A hosted runner sets no XDG_RUNTIME_DIR and the user manager cannot start without one. Keep the path short: a long one breaks socket-unit parsing.
  RT="${XDG_RUNTIME_DIR:-}"
  [ -n "$RT" ] || { RT="$(mktemp -d /tmp/wardyn-rt.XXXXXX)"; RT_TMP="$RT"; }
  XDG_RUNTIME_DIR="$RT" systemd-analyze verify --user "$RENDERED" >"$WORK/verify.out" 2>&1 || fail "systemd-analyze rejected the rendered unit: $(cat "$WORK/verify.out")"
fi

STUB_STATE="$H/.config/wardyn/runner" run_install linux --org https://wardyn.example --binary "$WORK/wardyn-runnerd"
[ "$RC" -eq 0 ] || fail "Linux reinstall failed (rc=$RC): $OUT"
! grep -q '^registered$' "$H/stub.log" || fail "an existing identity was registered again"

STUB_STATE="$H/.config/wardyn/runner" run_install linux --uninstall
[ "$RC" -eq 0 ] || fail "Linux uninstall failed (rc=$RC): $OUT"
for gone in "$RENDERED" "$H/.config/wardyn/runnerd.env" "$H/.local/share/wardyn/bin/wardyn-runnerd"; do
  [ ! -e "$gone" ] || fail "uninstall left ${gone#"$H"/}"
done
[ -e "$H/.config/wardyn/runner/runner.json" ] || fail "uninstall removed the runner identity"

# A login shell that sets XDG_CONFIG_HOME: register and serve get the same explicit --state-dir,
# the unit stays where the user manager (which never sees that variable) looks, and nothing lands
# under the XDG directory.
XH="$WORK/home-xdgcfg"
XDG_CFG="$XH/cfg" STUB_STATE='' run_install xdgcfg --org https://wardyn.example --binary "$WORK/wardyn-runnerd" --token-file "$WORK/token"
[ "$RC" -eq 0 ] || fail "an install with XDG_CONFIG_HOME set failed (rc=$RC): $OUT"
XSTATE="$XH/.config/wardyn/runner"
grep -qx "register --org https://wardyn.example --token-file $WORK/token --state-dir $XSTATE" "$XH/stub.log" || fail "register did not get --state-dir $XSTATE: $(cat "$XH/stub.log")"
grep -qx "WARDYN_RUNNER_STATE_DIR=$XSTATE" "$XH/.config/wardyn/runnerd.env" || fail "the env file does not carry the state dir serve gets"
grep -q '^ExecStart=.* serve --org ${WARDYN_RUNNER_ORG_URL} --state-dir ${WARDYN_RUNNER_STATE_DIR}$' "$XH/.config/systemd/user/wardyn-runnerd.service" || fail "the unit does not pass --state-dir to serve"
[ -f "$XSTATE/runner.json" ] || fail "the identity is not in the state dir"
[ ! -e "$XH/cfg" ] || fail "XDG_CONFIG_HOME split the install: $(find "$XH/cfg" -type f)"

# An upgrade: the identity an earlier install registered under XDG_CONFIG_HOME is kept in place,
# passed to serve, and not registered again.
UH="$WORK/home-xdgup"
mkdir -p "$UH/cfg/wardyn/runner"; : >"$UH/cfg/wardyn/runner/runner.json"
XDG_CFG="$UH/cfg" STUB_STATE='' run_install xdgup --org https://wardyn.example --binary "$WORK/wardyn-runnerd"
[ "$RC" -eq 0 ] || fail "an upgrade with the identity under XDG_CONFIG_HOME failed (rc=$RC): $OUT"
! grep -q '^registered$' "$UH/stub.log" || fail "an upgrade registered again"
grep -qx "WARDYN_RUNNER_STATE_DIR=$UH/cfg/wardyn/runner" "$UH/.config/wardyn/runnerd.env" || fail "the upgrade did not keep the identity directory"
[ ! -e "$UH/.config/wardyn/runner" ] || fail "the upgrade reset the identity to a new root"

# The user manager cannot find the unit: refuse before register spends the token.
DH="$WORK/home-nodiscover"
STUB_UNITPATH=/etc/systemd/user STUB_STATE='' run_install nodiscover --org https://wardyn.example --binary "$WORK/wardyn-runnerd" --token-file "$WORK/token"
[ "$RC" -ne 0 ] && grep -q 'no token was spent' <<<"$OUT" || fail "an undiscoverable unit did not refuse (rc=$RC): $OUT"
! grep -q '^registered$' "$DH/stub.log" || fail "register ran although the unit could not be found"

# A release download has no exec bit: a readable --binary still installs, executable in place.
cp "$WORK/wardyn-runnerd" "$WORK/wardyn-runnerd-linux-amd64"
chmod 0644 "$WORK/wardyn-runnerd-linux-amd64"
NX="$WORK/home-nonexec"
STUB_STATE="$NX/.config/wardyn/runner" run_install nonexec --org https://wardyn.example --binary "$WORK/wardyn-runnerd-linux-amd64" --token-file "$WORK/token"
[ "$RC" -eq 0 ] || fail "a non-executable --binary did not install (rc=$RC): $OUT"
[ -x "$NX/.local/share/wardyn/bin/wardyn-runnerd" ] || fail "the installed binary is not executable"
STUB_STATE=/dev/null run_install missing --org https://wardyn.example --binary "$WORK/absent" --token-file "$WORK/token"
[ "$RC" -ne 0 ] && grep -q 'not found' <<<"$OUT" || fail "a missing --binary did not say not found (rc=$RC): $OUT"

# A release candidate is a release: its proxy image tag is the candidate version.
STUB_STATE="$WORK/home-rc/.config/wardyn/runner" STUB_VERSION=0.9.0-rc.1 run_install rc --org https://wardyn.example --binary "$WORK/wardyn-runnerd" --token-file "$WORK/token"
[ "$RC" -eq 0 ] || fail "a release-candidate version was refused (rc=$RC): $OUT"
grep -qx 'WARDYN_PROXY_IMAGE=ghcr.io/cjohnstoniv/wardyn-proxy:0.9.0-rc.1' "$WORK/home-rc/.config/wardyn/runnerd.env" || fail "the candidate version is not the proxy image tag"

# ---- macOS: LaunchAgent rendering ------------------------------------------
stub uname 'echo Darwin'
M="$WORK/home-mac"
STUB_STATE="$M/Library/Application Support/wardyn/runner" run_install mac --org https://wardyn.example --binary "$WORK/wardyn-runnerd" --token-file "$WORK/token" --proxy-image 'p:1'
[ "$RC" -eq 0 ] || fail "macOS install failed (rc=$RC): $OUT"
AGENT="$M/Library/LaunchAgents/com.wardyn.runnerd.plist"
! grep -q '__[A-Z_]*__' "$AGENT" || fail "a placeholder survived in the rendered plist"
[ "$(plist_get "$AGENT" 'd["ProgramArguments"]')" = "['$M/Library/Application Support/wardyn/bin/wardyn-runnerd', 'serve', '--org', 'https://wardyn.example']" ] || fail "ProgramArguments not rendered"
[ "$(plist_get "$AGENT" 'd["EnvironmentVariables"]')" = "{'WARDYN_PROXY_IMAGE': 'p:1', 'WARDYN_INTERNAL_NETWORK': 'wardyn-internal'}" ] || fail "EnvironmentVariables not rendered or DOCKER_HOST not omitted"
[ "$(plist_get "$AGENT" 'd["KeepAlive"]')" = True ] || fail "KeepAlive is off"
grep -q "bootstrap gui/.* $AGENT" "$M/stub.log" || fail "the agent was not bootstrapped"
DOCKER_HOST=unix:///var/run/docker.sock STUB_STATE="$M/Library/Application Support/wardyn/runner" run_install mac --org https://wardyn.example --binary "$WORK/wardyn-runnerd" --proxy-image 'p:1'
[ "$RC" -eq 0 ] || fail "macOS reinstall failed (rc=$RC): $OUT"
[ "$(plist_get "$AGENT" 'd["EnvironmentVariables"]["DOCKER_HOST"]')" = unix:///var/run/docker.sock ] || fail "DOCKER_HOST not carried into the plist"
STUB_STATE=/dev/null run_install mac --uninstall
[ "$RC" -eq 0 ] && [ ! -e "$AGENT" ] || fail "macOS uninstall left the agent (rc=$RC): $OUT"

echo "test-runner-units: PASS"
