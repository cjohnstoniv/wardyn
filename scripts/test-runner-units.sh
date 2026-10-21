#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# No-docker, no-systemd, no-launchd checks for the runner packaging in
# deploy/runner/. install.sh is a thin wrapper over `wardyn-runnerd setup` (rendering,
# idempotency and the claim are Go tests in cmd/wardyn-runnerd/setup_test.go), so here it runs
# against a stub wardyn-runnerd that records its arguments, stub `id` and `uname`, and a
# throwaway HOME. The live start/stop proof is cmd/wardyn-runnerd/units_systemd_test.go in CI.
#
# Usage: scripts/test-runner-units.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="${REPO_ROOT}/deploy/runner"
INSTALL="${DIR}/install.sh"
UNIT="${DIR}/wardyn-runnerd.service"
PLIST="${DIR}/com.wardyn.runnerd.plist"
SETUP_GO="${REPO_ROOT}/cmd/wardyn-runnerd/setup_host.go"

fail() { echo "test-runner-units: FAIL: $*" >&2; exit 1; }

unset DOCKER_HOST
WORK="$(mktemp -d)"
RT_TMP=""
trap 'rm -rf "$WORK" ${RT_TMP:+"$RT_TMP"}' EXIT

bash -n "$INSTALL" || fail "install.sh has a syntax error"
# Warnings and errors only: info and style findings differ between shellcheck releases and would turn a
# hosted runner's upgrade red. CI always has it (hosted runners ship it), so a missing one there fails
# rather than letting the check go quiet.
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -S warning "$INSTALL" || fail "shellcheck rejected install.sh"
elif [ -n "${CI:-}" ]; then
  fail "shellcheck is not installed on this CI runner, so install.sh would go unchecked"
fi

plist_get() { python3 -I -c 'import plistlib,sys; d=plistlib.load(open(sys.argv[1],"rb")); print(eval(sys.argv[2]))' "$1" "$2"; }
plist_get "$PLIST" 'd["Label"]' >/dev/null || fail "the committed plist does not parse"

# Every placeholder in a committed unit or plist is one the Go renderer replaces, and the reverse.
for ph in $(grep -oh '__[A-Z_]*__' "$UNIT" "$PLIST" | sort -u); do
  grep -q "\"$ph\"" "$SETUP_GO" || fail "${ph} is in a committed unit or plist, but setup never replaces it"
done

grep -q '^Restart=on-failure$' "$UNIT" || fail "the unit lost Restart=on-failure"
! grep -q '^User=' "$UNIT" || fail "the unit sets User=: it is a user unit"
grep -q '^ExecStart=__WARDYN_RUNNERD__ serve --config __WARDYN_CONFIG__$' "$UNIT" || fail "the unit does not run 'serve --config'"
! grep -q 'EnvironmentFile\|Environment=' "$UNIT" || fail "the unit carries settings: they are in runnerd.json"

# The unit is valid once its one placeholder is replaced.
if command -v systemd-analyze >/dev/null 2>&1; then
  # A hosted runner sets no XDG_RUNTIME_DIR and the user manager cannot start without one. Keep the path short: a long one breaks socket-unit parsing.
  RT="${XDG_RUNTIME_DIR:-}"
  [ -n "$RT" ] || { RT="$(mktemp -d /tmp/wardyn-rt.XXXXXX)"; RT_TMP="$RT"; }
  sed 's|__WARDYN_RUNNERD__|/bin/true|g; s|__WARDYN_CONFIG__|/nonexistent/runnerd.json|g' "$UNIT" >"$WORK/rendered.service"
  XDG_RUNTIME_DIR="$RT" systemd-analyze verify --user "$WORK/rendered.service" >"$WORK/verify.out" 2>&1 || fail "systemd-analyze rejected the unit: $(cat "$WORK/verify.out")"
fi

# ---- the wrapper ------------------------------------------------------------
BIN="$WORK/bin"
mkdir -p "$BIN"
stub() { printf '#!/usr/bin/env bash\n%s\n' "$2" >"$BIN/$1"; chmod +x "$BIN/$1"; }
stub systemctl 'echo "systemctl $*" >>"$STUB_LOG"'
stub launchctl 'echo "launchctl $*" >>"$STUB_LOG"'

# A runner binary that records the arguments it was run with.
cat >"$WORK/wardyn-runnerd" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$STUB_LOG"
exit "${STUB_EXIT:-0}"
STUB
chmod +x "$WORK/wardyn-runnerd"

# run_install NAME ARGS... — runs install.sh in a fresh HOME; exports its result in $RC and $OUT.
run_install() {
  local home="$WORK/home-$1"; shift
  mkdir -p "$home"
  export STUB_LOG="$home/stub.log"
  : >"$STUB_LOG"
  set +e
  OUT="$(HOME="$home" PATH="$BIN:$PATH" env -u XDG_CONFIG_HOME -u XDG_DATA_HOME bash "$INSTALL" "$@" 2>&1)"
  RC=$?
  set -e
}

stub id 'echo 0'
run_install root --org https://wardyn.example --binary "$WORK/wardyn-runnerd"
[ "$RC" -ne 0 ] && grep -q 'not as root' <<<"$OUT" || fail "install.sh did not refuse to run as root (rc=$RC): $OUT"
[ ! -s "$WORK/home-root/stub.log" ] || fail "install.sh ran setup as root"
rm -f "$BIN/id"

run_install missing --org https://wardyn.example --binary "$WORK/absent"
[ "$RC" -ne 0 ] && grep -q 'not found' <<<"$OUT" || fail "a missing --binary did not say not found (rc=$RC): $OUT"

# Every other argument reaches setup unchanged; a release download has no exec bit and still runs.
cp "$WORK/wardyn-runnerd" "$WORK/wardyn-runnerd-linux-amd64"
chmod 0644 "$WORK/wardyn-runnerd-linux-amd64"
run_install pass --org https://wardyn.example --binary "$WORK/wardyn-runnerd-linux-amd64" --token-file "$WORK/token" --relay-listen 127.0.0.1 --claim --pool Laptop --default --max-runs 2
[ "$RC" -eq 0 ] || fail "install.sh failed (rc=$RC): $OUT"
[ "$(cat "$WORK/home-pass/stub.log")" = "setup --org https://wardyn.example --token-file $WORK/token --relay-listen 127.0.0.1 --claim --pool Laptop --default --max-runs 2" ] \
  || fail "setup did not get the arguments: $(cat "$WORK/home-pass/stub.log")"

STUB_EXIT=3 run_install exit --org https://wardyn.example --binary "$WORK/wardyn-runnerd"
[ "$RC" -eq 3 ] || fail "setup's exit status was lost (rc=$RC): $OUT"

# ---- uninstall: the service and binary go, the identity stays -----------------
stub uname 'echo Linux'
H="$WORK/home-linux"
mkdir -p "$H/.config/systemd/user" "$H/.config/wardyn/runner" "$H/.local/share/wardyn/bin"
: >"$H/.config/systemd/user/wardyn-runnerd.service"; : >"$H/.config/wardyn/runnerd.env"; : >"$H/.config/wardyn/runnerd.json"
: >"$H/.local/share/wardyn/bin/wardyn-runnerd"; : >"$H/.config/wardyn/runner/runner.json"
: >"$H/.local/share/wardyn/bin/wardyn"; : >"$H/.config/wardyn/cli.json"
: >"$H/.local/share/wardyn/bin/wardyn-runnerd.previous"
run_install linux --uninstall
[ "$RC" -eq 0 ] || fail "Linux uninstall failed (rc=$RC): $OUT"
for gone in .config/systemd/user/wardyn-runnerd.service .config/wardyn/runnerd.env .config/wardyn/runnerd.json .local/share/wardyn/bin/wardyn-runnerd .local/share/wardyn/bin/wardyn-runnerd.previous; do
  [ ! -e "$H/$gone" ] || fail "uninstall left $gone"
done
[ -e "$H/.config/wardyn/runner/runner.json" ] || fail "uninstall removed the runner identity"
# The CLI and its profile are the person's, not the runner's: uninstall leaves both.
[ -e "$H/.local/share/wardyn/bin/wardyn" ] && [ -e "$H/.config/wardyn/cli.json" ] || fail "uninstall removed the person's wardyn CLI or cli.json"
grep -qF "Kept the runner identity in: $H/.config/wardyn/runner" <<<"$OUT" || fail "uninstall did not name the default identity directory: $OUT"

# The identity setup kept under a custom XDG_CONFIG_HOME is the one the message names.
X="$WORK/home-xdgid"
mkdir -p "$X/.config/wardyn"
printf 'WARDYN_RUNNER_STATE_DIR=%s\n' "$X/xdg/wardyn/runner" >"$X/.config/wardyn/runnerd.env"
run_install xdgid --uninstall
[ "$RC" -eq 0 ] && grep -qF "Kept the runner identity in: $X/xdg/wardyn/runner" <<<"$OUT" || fail "uninstall did not name the identity directory from runnerd.env (rc=$RC): $OUT"

# runnerd.json names the identity directory too, and outranks the env file an earlier setup left.
X="$WORK/home-jsonid"
mkdir -p "$X/.config/wardyn"
printf '{\n  "org_url": "https://wardyn.example",\n  "state_dir": "%s",\n  "internal_network": "wardyn-internal"\n}\n' "$X/xdg/wardyn/runner" >"$X/.config/wardyn/runnerd.json"
run_install jsonid --uninstall
[ "$RC" -eq 0 ] && grep -qF "Kept the runner identity in: $X/xdg/wardyn/runner" <<<"$OUT" && [ ! -e "$X/.config/wardyn/runnerd.json" ] || fail "uninstall did not name the identity directory from runnerd.json or left it (rc=$RC): $OUT"

# A unit setup put under a custom XDG_CONFIG_HOME, because the user manager searches it, goes too.
X="$WORK/home-xdg"
mkdir -p "$X/xdg/systemd/user" "$X/.local/share/wardyn/bin"
: >"$X/xdg/systemd/user/wardyn-runnerd.service"; : >"$X/.local/share/wardyn/bin/wardyn-runnerd"
export STUB_LOG="$X/stub.log"
set +e
OUT="$(HOME="$X" XDG_CONFIG_HOME="$X/xdg" PATH="$BIN:$PATH" env -u XDG_DATA_HOME bash "$INSTALL" --uninstall 2>&1)"
RC=$?
set -e
[ "$RC" -eq 0 ] && [ ! -e "$X/xdg/systemd/user/wardyn-runnerd.service" ] || fail "uninstall left the unit under XDG_CONFIG_HOME (rc=$RC): $OUT"

stub uname 'echo Darwin'
M="$WORK/home-mac"
mkdir -p "$M/Library/LaunchAgents" "$M/Library/Application Support/wardyn/bin"
: >"$M/Library/LaunchAgents/com.wardyn.runnerd.plist"; : >"$M/Library/Application Support/wardyn/runnerd.json"; : >"$M/Library/Application Support/wardyn/bin/wardyn-runnerd"
: >"$M/Library/Application Support/wardyn/bin/wardyn"; : >"$M/Library/Application Support/wardyn/cli.json"
: >"$M/Library/Application Support/wardyn/bin/wardyn-runnerd.previous"
run_install mac --uninstall
[ "$RC" -eq 0 ] && [ ! -e "$M/Library/LaunchAgents/com.wardyn.runnerd.plist" ] && [ ! -e "$M/Library/Application Support/wardyn/bin/wardyn-runnerd" ] && [ ! -e "$M/Library/Application Support/wardyn/bin/wardyn-runnerd.previous" ] && [ ! -e "$M/Library/Application Support/wardyn/runnerd.json" ] || fail "macOS uninstall left the agent, binary, rollback copy or runnerd.json (rc=$RC): $OUT"
[ -e "$M/Library/Application Support/wardyn/bin/wardyn" ] && [ -e "$M/Library/Application Support/wardyn/cli.json" ] || fail "macOS uninstall removed the person's wardyn CLI or cli.json"

echo "test-runner-units: PASS"
