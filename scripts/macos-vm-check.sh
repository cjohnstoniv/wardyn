#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# macos-vm-check.sh — checks a release candidate's macOS VM sandboxes on THIS Apple silicon Mac and
# writes a report that is safe to paste into a public issue. docs/operations/macos-vm-rc-check.md is
# the page for the person who runs it; this header is the maintainer's contract.
#
#   scripts/macos-vm-check.sh --version 0.10.0-rc.10 --out ~/wardyn-check [--quick | --full]
#       [--server URL] [--base-ready] [--console] [--prompts] [--stop-at 2026-11-02T14:00:00Z]
#       [--allow-existing-runner] [--allow-org-writes] --i-understand-this-changes-this-mac
#   scripts/macos-vm-check.sh --preflight --version V      checks only; changes nothing
#   scripts/macos-vm-check.sh --cleanup --out DIR --i-understand-this-changes-this-mac
#
# Run it from a checkout at the release's tag: the suite is built from that checkout. Without the
# consent flag it runs the read-only preflight and stops. It uploads nothing and reads no credential
# from disk: the local stack's generated admin secret lives in a mode 600 file and reaches a command
# only through that command's environment; --server uses the WARDYN_TOKEN already in the environment.
#
# Steps (each one line in <out>/steps.tsv; a rerun with the same --out skips the ones recorded ok):
#   fetch, stack, install, register, base, build, suite, console (only with --console), then report.md
# Exit status, the same as test/conformance/macvm/run.sh: 0 every row PASS (or NOT_RUN on non-security
# rows only), 1 a FAIL, 2 a BLOCK or nothing could run (a summary that could not be written is 3 there
# and 2 here), 4 incomplete: no FAIL or BLOCK, but nothing passed, or a BLOCK row was not run or was cut
# off. Only 0 with `result: complete` reads as green. --quick omits BLOCK rows, so it can never read complete.
#
# ponytail: bash 3.2 (the macOS /bin/bash) — no associative arrays, no ${x,,}, no empty-array expansion.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASE_BASE=https://github.com/cjohnstoniv/wardyn/releases/download
IMAGE_BASE=ghcr.io/cjohnstoniv
STACK_PROJECT=wardyn-check
STACK_PORT=18080
# Seconds between base-readiness polls; a plain variable (not a WARDYN_ knob) so the stub test can zero it.
POLL=${CHECK_POLL_SECONDS:-30}
CONSENT_FLAG=--i-understand-this-changes-this-mac

version="" out="" mode=full server="" stop_at="" consent=0 preflight_only=0 cleanup=0
base_ready=0 console=0 prompts=0 allow_existing=0 allow_org_writes=0 checkout=$ROOT

say() { printf '%s\n' "$*"; }
usage_die() { printf 'macos-vm-check: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version|--out|--server|--stop-at|--checkout)
      [ $# -ge 2 ] || usage_die "$1 needs a value"
      case "$1" in
        --version) version=$2 ;; --out) out=$2 ;; --server) server=$2 ;;
        --stop-at) stop_at=$2 ;; --checkout) checkout=$2 ;;
      esac
      shift 2 ;;
    --quick) mode=quick; quick_given=1; shift ;;
    --full) mode=full; full_given=1; shift ;;
    --base-ready) base_ready=1; shift ;;
    --console) console=1; shift ;;
    --prompts) prompts=1; shift ;;
    --allow-existing-runner) allow_existing=1; shift ;;
    --allow-org-writes) allow_org_writes=1; shift ;;
    --preflight) preflight_only=1; shift ;;
    --cleanup) cleanup=1; shift ;;
    "$CONSENT_FLAG") consent=1; shift ;;
    -h|--help) sed -n 2,28p "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) usage_die "unknown argument: $1" ;;
  esac
done
[ -z "${quick_given:-}" ] || [ -z "${full_given:-}" ] || usage_die "--quick and --full are exclusive"
out=${out:-$HOME/wardyn-check}
STATE=$out/state
STEPS=$out/steps.tsv
SECRET=$STATE/admin.secret

# ---------- small helpers ----------

now_utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }
epoch_of() { date -u -d "$1" +%s 2>/dev/null || date -j -u -f '%Y-%m-%dT%H:%M:%SZ' "$1" +%s 2>/dev/null; }
past_stop() { [ -n "$stop_at" ] && [ "$(date +%s)" -ge "$(epoch_of "$stop_at")" ]; }

state_get() { [ -f "$STATE/state.tsv" ] && awk -F'\t' -v k="$1" '$1==k{v=$2} END{if(v!="")print v}' "$STATE/state.tsv"; }
state_put() { printf '%s\t%s\n' "$1" "$2" >>"$STATE/state.tsv"; }
step_done() { [ -f "$STEPS" ] && awk -F'\t' -v n="$1" '$1==n && $2=="ok"{f=1} END{exit !f}' "$STEPS"; }
record_step() { printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$(now_utc)" >>"$STEPS"; }

# step <name> <function>: run once, record it, skip one recorded ok; a step past --stop-at is recorded stopped.
step() {
  local name=$1 fn=$2 t0 rc
  if step_done "$name"; then say "step $name: finished earlier, skipped"; return 0; fi
  if past_stop; then record_step "$name" stopped 0; say "step $name: stopped, out of time"; return 1; fi
  t0=$(date +%s); say "step $name"
  "$fn"; rc=$?
  record_step "$name" "$([ $rc = 0 ] && echo ok || echo failed)" $(( $(date +%s) - t0 ))
  return $rc
}

# A command that needs the admin secret gets it in its environment only, never on argv.
with_token() {
  if [ -f "$SECRET" ]; then
    WARDYN_TOKEN="$(cat "$SECRET")" WARDYN_URL="$ORG_URL" "$@"
  else
    WARDYN_URL="$ORG_URL" "$@"
  fi
}

# ---------- preflight (read-only) ----------

MISSING=""
chk() { # chk <what> <how to get it> <predicate...>
  local what=$1 how=$2
  shift 2
  if "$@" >/dev/null 2>&1; then
    say "  ok       $what"
  else
    say "  MISSING  $what: $how"
    MISSING="$MISSING$what
"
  fi
}

is_arm64() { [ "$(uname -m)" = arm64 ]; }
os_major() { sw_vers -productVersion 2>/dev/null | cut -d. -f1; }
os_ok() { [ "$(os_major)" -ge 26 ] 2>/dev/null; }
hv_ok() { [ "$(sysctl -n kern.hv_support 2>/dev/null)" = 1 ] && [ "$(sysctl -n kern.hv_vmm_present 2>/dev/null)" = 0 ]; }
disk_ok() { [ $(( $(df -k "$HOME" | awk 'NR==2{print $4}') / 1048576 )) -ge 110 ]; }
mem_ok() { [ $(( $(sysctl -n hw.memsize) / 1073741824 )) -ge 16 ]; }
ac_ok() { local p; p=$(pmset -g batt 2>/dev/null); grep -q 'AC Power' <<<"$p"; }
have() { command -v "$1" >/dev/null 2>&1; }
docker_ok() { docker info >/dev/null 2>&1; }
go_ok() { go version >/dev/null 2>&1; }
tag_ok() { [ "$(git -C "$checkout" describe --tags --exact-match HEAD 2>/dev/null)" = "v$version" ]; }
tree_clean() { [ -z "$(git -C "$checkout" status --porcelain --untracked-files=no 2>/dev/null)" ]; }
suite_present() { [ -x "$checkout/test/conformance/macvm/run.sh" ]; }
token_env_set() { [ -n "${WARDYN_TOKEN:-}" ]; }
server_up() { curl -fsS -m 10 "$server/healthz" >/dev/null 2>&1; }
stop_valid() { [[ $stop_at =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] && epoch_of "$stop_at" >/dev/null; }

desktop_session() { [ "$(stat -f %Su /dev/console 2>/dev/null)" = "$(id -un)" ]; }

runner_registered() {
  [ -e "$HOME/Library/Application Support/wardyn/runnerd.json" ] ||
    [ -e "$HOME/Library/LaunchAgents/com.wardyn.runnerd.plist" ] ||
    launchctl print "gui/$(id -u)/com.wardyn.runnerd" >/dev/null 2>&1
}
no_other_runner() { [ -n "$(state_get runner_name)" ] || [ "$allow_existing" = 1 ] || ! runner_registered; }
live_runs() { wardyn-runnerd macos-vm status 2>/dev/null | sed -n 's/^macOS VMs on this Mac (every login): \([0-9]*\).*/\1/p'; }
no_live_runs() { [ -n "$(state_get runner_name)" ] || ! runner_registered || [ "$(live_runs)" = 0 ]; }

run_preflight() {
  say "Preflight (reads this Mac; changes nothing):"
  chk "Apple silicon" "this check needs an Apple silicon Mac" is_arm64
  chk "macOS 26 or later" "update macOS" os_ok
  chk "hypervisor available, and this Mac is not itself a VM" "macOS VM sandboxes need real Apple silicon" hv_ok
  chk "110 GiB free in your home volume" "free disk space; the base image needs about 85 GiB, a second VM about 107 GiB" disk_ok
  chk "16 GiB of memory or more" "use a Mac with more memory" mem_ok
  chk "on AC power" "plug the Mac in; preparing a base refuses on battery" ac_ok
  chk "git" "install the Xcode command line tools" have git
  chk "curl and shasum" "both ship with macOS" sh -c 'command -v curl && command -v shasum'
  chk "python3 (reads runner ids)" "install the Xcode command line tools" have python3
  chk "Go, to build the suite from this checkout" "install Go from go.dev/dl" go_ok
  chk "a user logged in at this Mac's desktop" "log in at the screen (macOS VMs run only in a login session); over SSH, use Screen Sharing" desktop_session
  chk "the Xcode command line tools (cgo builds the suite)" "run: xcode-select --install" xcode-select -p
  chk "Docker, running" "start Docker Desktop (the guests' network relay and proxy run in it)" docker_ok
  chk "sudo (installs the package)" "run it as an administrator" have sudo
  chk "this checkout is at the tag v$version" "git -C <checkout> checkout v$version" tag_ok
  chk "this checkout has no local changes" "git stash or commit them; the report names the commit it ran" tree_clean
  chk "the suite is in this checkout" "use a full checkout of the repository" suite_present
  chk "no other runner registered on this Mac" "remove it, or pass --allow-existing-runner and accept it will be used" no_other_runner
  chk "no live runs on this runner" "end them first" no_live_runs
  [ "$console" = 0 ] || chk "Node and pnpm, for --console" "install Node 20 or later and pnpm" sh -c 'command -v node && command -v pnpm'
  [ -z "$stop_at" ] || chk "--stop-at is an RFC 3339 UTC time like 2026-11-02T14:00:00Z" "fix the value" stop_valid
  if [ -n "$server" ]; then
    chk "--server is an http(s) URL" "give --server https://host" sh -c 'case "$1" in http://?*|https://?*) exit 0 ;; esac; exit 1' _ "$server"
    chk "WARDYN_TOKEN in the environment" "export your own personal API token; the script never reads one from a file" token_env_set
    chk "the server answers" "check the URL and that you can reach it" server_up
  fi
  say "  note     the organisation needs WARDYN_MACOS_VMS=1 and runners enabled; the script cannot read either"
  say "  note     a second admin must approve the trust change under four-eyes (rows that need two admin sessions are NOT_RUN without one)"
  for fx in ALLOWED_URL DENIED_HOST GIT_URL 1G_URL; do
    eval "[ -n \"\${WARDYN_MACVM_$fx:-}\" ]" || say "  note     WARDYN_MACVM_$fx is not set: the rows that need it are NOT_RUN"
  done
  have sudo && ! sudo -n true >/dev/null 2>&1 && say "  note     sudo will ask for your password once, at the install step"
  [ -z "$MISSING" ]
}

print_changes() {
  cat <<EOF

This script changes this Mac. With $CONSENT_FLAG it will:
  - install a runner: the release package (with sudo) and a runner registered on this Mac
  - restart that runner, as the tests need
  - fill a run's disk to its limit, and wedge a guest, to prove the limits hold
  - wait for a macOS base image: you start the prepare yourself in Terminal at this Mac's desktop (tens of GiB, a long wait)
  - write a root-owned posture file asserting this Mac is managed, and mark the runner managed_device, because macOS VM runs require both
EOF
  if [ -n "$server" ]; then
    cat <<EOF
It writes to the organisation at --server:
  - one single-use registration token and one runner, marked managed_device (the rows need that)
  - the runs the tests create, with their files and output; any run document you set for the suite is applied there
  - it restores by revoking the runner at --cleanup; the audit entries and run records stay in that organisation
  - P20 changes governance state it cannot undo, so it is NOT_RUN unless you pass --allow-org-writes
EOF
  else
    cat <<EOF
It starts a throwaway stack of containers on this Mac from the release's published images (a published RC only), loopback only, with a generated admin secret in a mode 600 file. It does not create the suite's network fixtures. --cleanup removes it.
EOF
  fi
  say "Use a Mac that is not in daily use. Preflight alone: add --preflight."
}

# ---------- steps ----------

init_out() {
  mkdir -p "$out" "$STATE" "$out/download" "$out/bin" || usage_die "cannot create $out"
  chmod 700 "$out" "$STATE"
  if [ -f "$STATE/state.tsv" ] && [ "$(state_get version)" != "$version" ]; then
    usage_die "$out holds a run for another version: give this run its own --out"
  fi
  [ -f "$STATE/state.tsv" ] || { ( umask 077; : >"$STATE/state.tsv" ); state_put version "$version"; state_put started "$(now_utc)"; }
  [ -f "$STEPS" ] || printf 'step\tstatus\tseconds\tended_utc\n' >"$STEPS"
  state_put server_kind "$([ -n "$server" ] && echo external || echo local)"
}

sha_of() { shasum -a 256 "$1" | awk '{print $1}'; }

step_fetch() {
  local pkg=$out/download/wardyn-runnerd.pkg sums=$out/download/SHA256SUMS want got
  curl -fsSL -o "$pkg" "$RELEASE_BASE/v$version/wardyn-runnerd.pkg" &&
    curl -fsSL -o "$sums" "$RELEASE_BASE/v$version/SHA256SUMS" || { say "download failed"; return 1; }
  want=$(awk '{n=$2; sub(/^\.\//,"",n); if (n=="wardyn-runnerd.pkg") print $1}' "$sums")
  got=$(sha_of "$pkg")
  [ -n "$want" ] && [ "$want" = "$got" ] || { say "the package does not match SHA256SUMS: refusing it"; return 1; }
  state_put pkg_sha "$got"
}

step_stack() {
  if [ -n "$server" ]; then ORG_URL=$server; return 0; fi
  local token
  token=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
  ( umask 077; printf '%s' "$token" >"$SECRET"; printf 'WARDYN_ADMIN_TOKEN=%s\nWARDYN_UP_PORT=%s\nWARDYN_PG_PORT=%s\nWARDYN_REGISTRY_PORT=%s\nWARDYN_WARDYND_IMAGE=%s\nWARDYN_PROXY_IMAGE=%s\n' \
      "$token" "$STACK_PORT" $((STACK_PORT + 1)) $((STACK_PORT + 2)) "$IMAGE_BASE/wardynd:$version" "$IMAGE_BASE/wardyn-proxy:$version" >"$STATE/stack.env" )
  chmod 600 "$SECRET" "$STATE/stack.env"
  state_put stack_project "$STACK_PROJECT"
  # The compose file does not pass WARDYN_MACOS_VMS to wardynd, so an override file does.
  printf 'services:\n  wardynd:\n    environment:\n      WARDYN_MACOS_VMS: "1"\n' >"$STATE/stack.override.yaml"
  docker compose -p "$STACK_PROJECT" -f "$checkout/deploy/compose/docker-compose.yaml" -f "$STATE/stack.override.yaml" --env-file "$STATE/stack.env" \
    up -d postgres wardynd >>"$out/check.log" 2>&1 || { say "the local stack did not start (see check.log)"; return 1; }
  local i=0
  until curl -fsS -m 3 "$ORG_URL/healthz" >/dev/null 2>&1; do
    i=$((i + 1)); past_stop && return 1
    [ "$i" -le 120 ] || { say "the local stack did not answer"; return 1; }
    sleep 5
  done
}

step_install() {
  sudo installer -pkg "$out/download/wardyn-runnerd.pkg" -target / >>"$out/check.log" 2>&1 || return 1
  state_put pkg_installed 1
}

# The registered runner's limits: the runbook's 12 GiB / 6 GiB, reduced on a Mac with less memory.
limits() {
  local gib cpus tot per
  gib=$(( $(sysctl -n hw.memsize) / 1073741824 )); cpus=$(sysctl -n hw.ncpu)
  tot=$(( gib / 2 > 12 ? 12 : gib / 2 )); per=$(( tot / 2 ))
  printf -- '--total-cpu %s --per-run-cpu %s --total-memory %s --per-run-memory %s' "$cpus" $(( cpus / 2 > 4 ? 4 : cpus / 2 )) "$tot" "$per"
}

trust_class() {
  with_token wardyn runner list --json | python3 -I -c 'import json,sys
for r in json.load(sys.stdin):
    if r.get("id") == sys.argv[1]:
        print(r.get("trust_class", ""))' "$1"
}

base_state() { wardyn-runnerd macos-vm status 2>/dev/null | sed -n 's/^base: \([a-z]*\).*/\1/p' | sed -n 1p; }

step_register() {
  local name tok rid
  [ "$(base_state)" != ready ] || [ "$base_ready" = 1 ] ||
    { say "a macOS base already exists here: pass --base-ready to reuse it, or run: wardyn-runnerd macos-vm remove"; return 1; }
  [ "$(base_state)" = ready ] || state_put base_ours 1
  name=$(state_get runner_name)
  if [ -z "$name" ]; then
    name=wardyn-check-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
    state_put runner_name "$name"
  fi
  tok=$STATE/registration.token
  ( umask 077; with_token wardyn runner token >"$tok" ) || { rm -f "$tok"; say "could not mint a registration token"; return 1; }
  # shellcheck disable=SC2046  # limits() prints separate words on purpose
  with_token wardyn-runnerd setup --org "$ORG_URL" --token-file "$tok" --name "$name" --claim --pool "$name" \
    --macos-vm on $(limits) >>"$out/check.log" 2>&1
  local rc=$?
  rm -f "$tok"
  [ $rc = 0 ] || { say "runner setup failed (see check.log)"; return 1; }
  state_put registered_by_script 1
  state_put runnerd_json "$HOME/Library/Application Support/wardyn/runnerd.json"
  state_put service_label com.wardyn.runnerd
  rid=$(with_token wardyn runner list --json | python3 -I -c 'import json,sys
for r in json.load(sys.stdin):
    if r.get("name") == sys.argv[1]:
        print(r["id"])' "$name")
  [ -n "$rid" ] || { say "the registered runner is not in your runner list"; return 1; }
  state_put runner_id "$rid"
  if [ ! -e "/Library/Application Support/Wardyn/posture.json" ]; then
    sudo mkdir -p "/Library/Application Support/Wardyn" &&
      printf '{"mdm_managed": true, "disk_encrypted": false}\n' | sudo tee "/Library/Application Support/Wardyn/posture.json" >/dev/null &&
      sudo chown root:wheel "/Library/Application Support/Wardyn/posture.json" &&
      sudo chmod 644 "/Library/Application Support/Wardyn/posture.json" && state_put posture_written 1
  fi
  local t
  t=$(with_token wardyn runner trust "$rid" managed_device --reason "macOS VM release check" 2>&1)
  case "$t" in
    *"pending approval"*)
      say "A second admin must approve the trust change now: wardyn governance changes list, then wardyn governance changes approve <id>"
      until [ "$(trust_class "$rid")" = managed_device ]; do
        past_stop && { say "stopped: out of time before the trust change was approved"; return 1; }
        sleep "$POLL"
      done ;;
    *"managed_device"*) ;;
    *) say "could not mark the runner managed_device"; return 1 ;;
  esac
}

# The prepare cannot start from here: on macOS 26 it refuses over SSH, outside the signed-in desktop and with sudo.
# A person pastes one command in Terminal on the desktop; this waits for `ready` and, while it waits, checks the
# one-time code is named by file and never carried as a field. It never reads the code or its file.
step_base() {
  local log="$HOME/Library/Application Support/Wardyn/macos-vm.noindex/prepare.log" seen_wait=0 shape=ok st
  if [ "$(base_state)" != ready ]; then
    say "Start the base now, in Terminal on this Mac's own desktop (not over SSH, not with sudo):"
    say "    wardyn-runnerd macos-vm prepare"
    [ "$(os_major)" != 26 ] || say "macOS 26 then pauses for setup by hand once (about 10 minutes at this Mac): see 'Setup by hand' in docs/operations/macos-vms.md. The status names the one-time code's file; this script never reads or prints the code."
  fi
  until [ "$(base_state)" = ready ]; do
    st=$(wardyn-runnerd macos-vm status 2>&1; wardyn-runnerd macos-vm status --json 2>&1; tail -n 40 "$log" 2>/dev/null)
    if grep -q 'one-time code in .' <<<"$st"; then seen_wait=1; fi
    if grep -Eq '"code"[[:space:]]*:' <<<"$st"; then shape=fail; fi
    past_stop && { say "stopped: out of time before the base was ready"; state_put code_shape "$([ $seen_wait = 1 ] && echo $shape || echo not_run)"; return 1; }
    sleep "$POLL"
  done
  if [ "$seen_wait" = 1 ]; then state_put code_shape "$shape"; else state_put code_shape not_run; fi
  return 0
}

step_build() {
  ( cd "$checkout" && go test -c -tags macvm_live -o "$out/bin/macvm_live.test" ./test/conformance/macvm/ ) >>"$out/check.log" 2>&1
}

row_pattern() {
  local quick='^TestP(04|05|06|07|08|11|19|21|22|23|25|26|27|28)_' notp20='^TestP(0[4-9]|1[0-9]|2[1-9])[a-z]?_'
  if [ "$mode" = quick ]; then printf '%s' "$quick"
  elif [ -n "$server" ] && [ "$allow_org_writes" = 0 ]; then printf '%s' "$notp20"
  else printf '%s' '^TestP'; fi
}

step_suite() {
  local dir=$out/suite rc
  if [ -e "$dir" ]; then mv "$dir" "$dir.interrupted-$(date +%s)"; fi
  say "suite running; progress: tail -f $out/suite.log"
  # P23 and P24 and the rest read these; STOP_AT is the suite's own clock.
  ( [ -z "$stop_at" ] || export WARDYN_MACVM_STOP_AT=$stop_at
    [ "$prompts" = 0 ] || export WARDYN_MACVM_PROMPTS=1
    export WARDYN_MACVM_LIVE=1 WARDYN_MACVM_RUNNER=$(state_get runner_name)
    if [ "$mode" = quick ]; then
      export WARDYN_MACVM_CLOCK_FOR=${WARDYN_MACVM_CLOCK_FOR:-10m} WARDYN_MACVM_CLOCK_EVERY=${WARDYN_MACVM_CLOCK_EVERY:-2m} \
        WARDYN_MACVM_WARM_TRAVEL=${WARDYN_MACVM_WARM_TRAVEL:-2} WARDYN_MACVM_WARM_FRESH=${WARDYN_MACVM_WARM_FRESH:-1}
    fi
    with_token "$checkout/test/conformance/macvm/run.sh" "$out/bin/macvm_live.test" "$dir" "$(row_pattern)" ) >"$out/suite.log" 2>&1
  rc=$?
  state_put suite_rc "$rc"
  return 0
}

# The console checks need a RUNNING macOS VM run, so this starts one, plants a file for the Files check,
# runs the console checks' Playwright config against it, and ends the run. The token travels as a file path.
CONSOLE_CHECKS="viewer-frame no-watch-link sandbox-card files output"
step_console() {
  local doc rid tok=$SECRET planted i=0 state="" rc dir=$out/console
  rm -rf "$dir"; mkdir -p "$dir"
  if [ -n "$server" ]; then
    tok=$STATE/console.token
    ( umask 077; printf '%s' "$WARDYN_TOKEN" >"$tok" )
  fi
  doc=$(printf '{"api_version":"wardyn/v1","kind":"Run","intent":{"agent":"none","interactive":true,"machine_kind":"macos_vm","placement":"local","runner_id":"%s"}}' "$(state_get runner_id)")
  rid=$(printf '%s' "$doc" | with_token wardyn run --file - --json --title "macOS VM check" |
    python3 -I -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  [ -n "$rid" ] || { say "console: could not start a run"; return 1; }
  state_put console_run "$rid"
  until [ "$state" = RUNNING ]; do
    state=$(with_token wardyn run get "$rid" --json | python3 -I -c 'import json,sys; print(json.load(sys.stdin).get("state",""))')
    i=$((i + 1)); past_stop && break
    [ "$state" = RUNNING ] || [ "$i" -gt 60 ] || { sleep "$POLL"; continue; }
    break
  done
  if [ "$state" = RUNNING ]; then
    planted=macvm-check-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
    with_token wardyn run exec "$rid" -- sh -c "echo planted > /Users/agent/work/$planted" >>"$out/check.log" 2>&1
  fi
  ( cd "$checkout/ui" && WARDYN_MACVM_CONSOLE_URL=$ORG_URL WARDYN_MACVM_CONSOLE_RUN_ID=$rid \
      WARDYN_MACVM_CONSOLE_TOKEN_FILE=$tok WARDYN_MACVM_CONSOLE_OUT=$dir WARDYN_MACVM_CONSOLE_FILE_NAME=${planted:-} \
      pnpm exec playwright test --config playwright.macos-vm.config.ts ) >"$out/console.log" 2>&1
  rc=$?
  with_token wardyn run kill "$rid" >>"$out/check.log" 2>&1
  [ -z "$server" ] || rm -f "$tok"
  state_put console_rc "$rc"
  return 0
}

# console_rows: "check<TAB>status<TAB>reason" for each console check, from the checks' JSON, one per check.
console_rows() {
  local c f
  for c in $CONSOLE_CHECKS; do
    f=$out/console/console-$c.json
    if [ "$console" = 1 ] && [ -f "$f" ]; then
      python3 -I -c 'import json,sys
d = json.load(open(sys.argv[1]))
print("\t".join([str(d.get("check", "")), str(d.get("status", "")), str(d.get("reason", "")).replace("\t", " ").replace("\n", " ")]))' "$f" 2>/dev/null ||
        printf '%s\tNOT_RUN\tresult file unreadable\n' "$c"
    elif [ "$console" = 1 ]; then
      printf '%s\tNOT_RUN\tno result written\n' "$c"
    else
      printf '%s\tNOT_RUN\tnot requested (--console)\n' "$c"
    fi
  done
}

# ---------- report ----------

clean_field() { LC_ALL=C tr -c 'A-Za-z0-9 ._:;,()=/+-' ' ' <<<"$1" | tr -s ' ' | tr -d '\n' | sed 's/^ //;s/ $//' | cut -c1-200; }

suite_result() { sed -nE 's/^result: (complete|incomplete)$/\1/p' "$out/suite.log" 2>/dev/null | tail -n 1; }

# counts <outcome>: how many summary rows have it.
count_of() { [ -f "$out/suite/summary.tsv" ] && awk -F'\t' -v o="$1" 'NR>1 && $2==o{n++} END{print n+0}' "$out/suite/summary.tsv" || echo 0; }
# block_not_run: the number run.sh's summary prints as "N BLOCK NOT RUN", 0 when it prints none.
block_not_run() { sed -n 's/^\([0-9][0-9]*\) BLOCK NOT RUN$/\1/p' "$out/suite.log" 2>/dev/null | awk '{n=$1} END{print n+0}'; }

# Final exit code from the suite's own. Green only when it exited 0, says `result: complete`, something
# passed and no BLOCK row went unrun. A console FAIL or a code-shape FAIL is a FAIL; neither outranks a BLOCK.
final_code() {
  local rc res
  rc=$(state_get suite_rc)
  [ -n "$rc" ] || { echo 2; return; }
  res=$(suite_result)
  case "$rc" in
    0) if [ "$res" = complete ] && [ "$(count_of PASS)" -gt 0 ] && [ "$(block_not_run)" = 0 ]; then rc=0; else rc=4; fi ;;
    1|2|4) ;;
    *) rc=2 ;;
  esac
  if [ "$rc" != 2 ] && [ "$rc" != 1 ]; then
    if [ "$(state_get code_shape)" = fail ] || console_rows | awk -F'\t' '$2=="FAIL"{f=1} END{exit !f}'; then rc=1; fi
  fi
  echo "$rc"
}

deny_literals() {
  { hostname -s; hostname; scutil --get ComputerName; scutil --get LocalHostName; id -un
    printf '%s\n' "$HOME" "$out" "$checkout" "$(state_get runner_name)"
    [ -z "$server" ] || { sed -e 's|^[a-z]*://||' -e 's|[/:].*||' <<<"$server"; printf '%s\n' "$server"; }
  } 2>/dev/null | awk 'length($0) >= 3'
}

# deny_scan <file>: prints the class of each hit, never the matched text; returns 1 on any hit.
deny_scan() {
  local f=$1 hit=0 masked lits sha
  masked=$(mktemp "$STATE/mask.XXXXXX")
  sha=$(state_get pkg_sha); if [ -n "$sha" ]; then sed "s/$sha/SHA256/g" "$f" >"$masked"; else cp "$f" "$masked"; fi
  lits=$(mktemp "$STATE/lits.XXXXXX"); deny_literals >"$lits"
  hits() { grep -Eqi -e "$2" "$masked" && { say "  report refused: it contains $1"; hit=1; }; return 0; }
  hits "a URL" '[a-z][a-z0-9+.-]*://'
  hits "an email address or user@host" '[a-z0-9._%+-]+@[a-z0-9.-]+'
  hits "an IPv4 address" '(^|[^0-9.])[0-9]{1,3}(\.[0-9]{1,3}){3}([^0-9]|$)'
  hits "an IPv6 or MAC address" '([0-9a-f]{1,4}:){7}[0-9a-f]{1,4}|[0-9a-f:]*::[0-9a-f:]*|([0-9a-f]{2}:){5}[0-9a-f]{2}'
  hits "a hostname" '[a-z0-9-]+\.(local|lan|internal|home|corp|localdomain|intranet|com|net|org|io|dev)([^a-z]|$)'
  hits "a path under a home or temp folder" '/home/[a-z]|/var/folders|/private/(var|tmp)|~/'
  hits "a token or key" '(wdn_|wdd_|wde_|wdg_|wdp_|wdc_|ghp_|gho_|ghu_|ghs_|github_pat_|xox[abp]-|AKIA|ASIA|AGE-SECRET-KEY-|eyJ[a-z0-9_-]{8,}|sk-[a-z0-9_-]{16,}|-----BEGIN|PRIVATE KEY)|bearer +[a-z0-9._~+/=-]{8,}|(token|password|passwd|secret)[ =:]+[a-z0-9._~+/=-]{12,}'
  hits "a cloud account, host or resource id" '(^|[^0-9])[0-9]{12}([^0-9]|$)|arn:aws|(^|[^a-z])(i|h|ami|sg|vpc|subnet)-[0-9a-f]{8,17}|ec2-user|ec2-[0-9]'
  hits "a run id" '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|run[_-][0-9a-f]{8,}'
  hits "a long opaque string" '[a-z0-9+/_=-]{40,}'
  # /Users/<name> is a home path unless it is the guest's own account or the shared folder.
  if [ -n "$(grep -oE '/Users/[A-Za-z0-9._-]+' "$masked" | grep -vE '^/Users/(agent|Shared)$')" ]; then say "  report refused: it contains a path under a home folder"; hit=1; fi
  if [ -s "$lits" ] && grep -Fqif "$lits" "$masked"; then say "  report refused: it contains this Mac's host name, user name, a path of yours, or the server"; hit=1; fi
  if [ -f "$SECRET" ] && grep -Fqf "$SECRET" "$masked"; then say "  report refused: it contains the generated admin secret"; hit=1; fi
  rm -f "$masked" "$lits"
  return $hit
}

write_report() { # write_report <file> <code>
  local f=$1 code=$2 res rows sw chip mem
  res=$(suite_result)
  case "$code" in 0) res=complete ;; 4) res=incomplete ;; *) [ -n "$res" ] || res=incomplete ;; esac
  sw="$(sw_vers -productVersion 2>/dev/null) ($(sw_vers -buildVersion 2>/dev/null))"
  chip=$(sysctl -n machdep.cpu.brand_string 2>/dev/null); mem=$(( $(sysctl -n hw.memsize 2>/dev/null || echo 0) / 1073741824 ))
  {
    [ "$res" != incomplete ] || printf '# INCOMPLETE: this is not a pass\n\nNothing passed, a BLOCK row did not run, or the run was cut off. Read the counts and rows below.\n\n'
    printf '# macOS VM check report\n\nresult: %s\nexit: %s\n' "$res" "$code"
    printf 'counts: PASS %s, FAIL %s, BLOCK %s, NOT_RUN %s; BLOCK rows not run: %s\n\n' "$(count_of PASS)" "$(count_of FAIL)" \
      "$(count_of BLOCK)" "$(count_of NOT_RUN)" "$(block_not_run)"
    printf '| Field | Value |\n|---|---|\n'
    printf '| Release | %s |\n| Checkout commit | %s |\n| Package SHA-256 | %s |\n' "$(clean_field "$version")" \
      "$(clean_field "$(git -C "$checkout" rev-parse --short=12 HEAD 2>/dev/null)")" "$(clean_field "$(state_get pkg_sha)")"
    printf '| macOS | %s |\n| Chip | %s |\n| Memory | %s GiB |\n' "$(clean_field "$sw")" "$(clean_field "$chip")" "$mem"
    printf '| Mode | %s |\n| Stack | %s |\n| Started | %s |\n| Finished | %s |\n\n' "$mode" "$(state_get server_kind)" \
      "$(clean_field "$(state_get started)")" "$(now_utc)"
    printf '## Steps\n\n| Step | Status | Seconds |\n|---|---|---|\n'
    awk -F'\t' 'NR>1{print $1"\t"$2"\t"$3}' "$STEPS" | while IFS="$(printf '\t')" read -r a b c; do
      printf '| %s | %s | %s |\n' "$(clean_field "$a")" "$(clean_field "$b")" "$(clean_field "$c")"
    done
    printf '\n## Rows\n\n'
    if [ -f "$out/suite/summary.tsv" ]; then
      printf '| Row | Outcome | Seconds | Reason |\n|---|---|---|---|\n'
      awk -F'\t' 'NR==1{for(i=1;i<=NF;i++) if($i=="duration") di=i} NR>1{print $1"\t"$2"\t"(di?$di:"-")"\t"$3}' "$out/suite/summary.tsv" |
        while IFS="$(printf '\t')" read -r a b c d; do
          printf '| %s | %s | %s | %s |\n' "$(clean_field "$a")" "$(clean_field "$b")" "$(clean_field "$c")" "$(clean_field "$d")"
        done
    else
      printf 'The suite wrote no summary: no row ran.\n'
    fi
    printf '\n## Prepare shape check\n\nThe one-time code is named by file, never carried as a field, in the status and the prepare log: %s.\n' \
      "$(clean_field "$(state_get code_shape)")"
    printf '\n## Suite settings\n\n- Row set: %s\n' "$(clean_field "$mode")"
    [ "$mode" != quick ] || printf -- '- Time budgets shortened for quick: clock %s every %s, warm starts %s and %s.\n' "${WARDYN_MACVM_CLOCK_FOR:-10m}" "${WARDYN_MACVM_CLOCK_EVERY:-2m}" "${WARDYN_MACVM_WARM_TRAVEL:-2}" "${WARDYN_MACVM_WARM_FRESH:-1}"
    [ "$mode" = quick ] || printf -- '- Time budgets: the suite defaults.\n'
    printf -- '- Network fixtures provided:'
    for fx in ALLOWED_URL DENIED_HOST GIT_URL 1G_URL; do eval "[ -n \"\${WARDYN_MACVM_$fx:-}\" ]" && printf ' %s' "$fx"; done
    printf '. Rows that need a fixture not listed are NOT_RUN.\n'
    printf '\n## Console checks\n\n| Check | Status | Reason |\n|---|---|---|\n'
    console_rows | while IFS="$(printf '\t')" read -r a b c; do
      printf '| %s | %s | %s |\n' "$(clean_field "$a")" "$(clean_field "$b")" "$(clean_field "$c")"
    done
    printf '\n## Left out on purpose\n\n'
    [ -z "$server" ] || [ "$allow_org_writes" = 1 ] || printf -- '- P20 (managed gate): it changes governance state on an external server that this script cannot undo.\n'
    [ "$mode" != quick ] || printf -- '- Quick mode omits the BLOCK rows P9, P10, P12, P16, P17 and P20, so a quick run can never read complete.\n'
    printf '\nThis report is evidence from one Mac that someone else ran. It proves a security row only if we watched that run.\n'
  } >"$f"
}

step_report() {
  local code draft=$STATE/report.draft
  code=$(final_code)
  rm -f "$out/report.md"
  write_report "$draft" "$code"
  if deny_scan "$draft"; then
    mv "$draft" "$out/report.md"; say "report: $out/report.md"
  else
    rm -f "$draft"; say "No report written. Share summary.tsv after you have read it yourself: $out/suite/summary.tsv"
    [ "$code" = 1 ] || code=2
  fi
  record_step report ok 0
  return "$code"
}

# ---------- cleanup: only what state.tsv records ----------

do_cleanup() {
  [ -f "$STATE/state.tsv" ] || { say "nothing to clean: $out has no state file"; return 0; }
  ORG_URL=$(state_get org_url); [ -n "$ORG_URL" ] || ORG_URL=${server:-http://localhost:$STACK_PORT}
  # Without the marker the runner is not ours to deregister; with runs present the base is in use.
  [ "$(state_get registered_by_script)" = 1 ] || { say "Refused: $out does not record that this script registered the runner on this Mac. Nothing was removed."; return 2; }
  [ "$(live_runs)" = 0 ] || { say "Refused: runs exist on this runner. End them, then run --cleanup again."; return 2; }
  local bad=0 name
  name=$(state_get runner_name)
  [ -z "$name" ] || { with_token wardyn runner revoke "$name" >/dev/null 2>&1 && say "revoked runner" || { say "could not revoke the runner (the stack may be down)"; bad=1; }; }
  [ -z "$(state_get service_label)" ] || { launchctl bootout "gui/$(id -u)/$(state_get service_label)" >/dev/null 2>&1; rm -f "$HOME/Library/LaunchAgents/$(state_get service_label).plist"; }
  [ -z "$(state_get runnerd_json)" ] || rm -f "$(state_get runnerd_json)"
  [ "$(state_get base_ours)" != 1 ] || { wardyn-runnerd macos-vm remove >/dev/null 2>&1 && say "removed the base this run prepared" || bad=1; }
  [ "$(state_get posture_written)" != 1 ] || sudo rm -f "/Library/Application Support/Wardyn/posture.json"
  [ -z "$(state_get stack_project)" ] || { docker compose -p "$(state_get stack_project)" -f "$checkout/deploy/compose/docker-compose.yaml" -f "$STATE/stack.override.yaml" --env-file "$STATE/stack.env" down -v >/dev/null 2>&1 && say "removed the local stack" || bad=1; }
  rm -f "$SECRET" "$STATE/stack.env"
  [ "$(state_get pkg_installed)" != 1 ] || say "left in place: the installed package under /usr/local/wardyn (macOS has no uninstaller for a package) and $out"
  return $bad
}

# ---------- main ----------

[ -n "$version" ] || usage_die "--version is required (the release, like 0.10.0-rc.10)"
ORG_URL=${server:-http://localhost:$STACK_PORT}

if [ "$cleanup" = 1 ]; then
  [ "$consent" = 1 ] || { print_changes; say "--cleanup changes this Mac too; add $CONSENT_FLAG."; exit 2; }
  do_cleanup; exit $?
fi

run_preflight; pre=$?
if [ "$preflight_only" = 1 ]; then exit $(( pre == 0 ? 0 : 2 )); fi
if [ "$consent" != 1 ]; then print_changes; say "Refused: nothing was changed. Add $CONSENT_FLAG to go on."; exit 2; fi
[ "$pre" = 0 ] || { say "Preflight found what is missing above: nothing was changed."; exit 2; }

init_out
print_changes | tee -a "$out/check.log"
state_put org_url "$ORG_URL"
failed=0
for s in fetch stack install register base build suite console; do
  [ "$s" != console ] || [ "$console" = 1 ] || continue
  if ! step "$s" "step_$s"; then failed=1; break; fi
done
step_report
exit $?
