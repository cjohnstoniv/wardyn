#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-macos-vm-check.sh — what macos-vm-check.sh does, with a stub for every external command: no sudo,
# no Docker, no network, nothing installed. PATH holds only the stubs and symlinks to the plain text tools,
# so a command the script should not run cannot run. It proves: no consent means no action, an existing
# runner is refused, a planted secret leaves no report, an incomplete run exits 4, and a rerun skips the
# steps already finished. It proves the script's logic, not that anything works on a real Mac.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fails=0
fail() { echo "test-macos-vm-check: FAIL: $*" >&2; fails=$((fails + 1)); }
CALLS=$WORK/calls.log
STUB=$WORK/stub SAFE=$WORK/safe CO=$WORK/co
mkdir -p "$STUB" "$SAFE" "$CO/scripts" "$CO/deploy/compose" "$CO/test/conformance/macvm" "$CO/ui"
cp "$ROOT/scripts/macos-vm-check.sh" "$CO/scripts/"
: >"$CO/deploy/compose/docker-compose.yaml"

for t in awk sed grep tr od head tail cut date mkdir rm mv cat find sort mktemp id chmod ln tee env sh bash sleep python3 dirname basename wc uniq ls cp sha256sum; do
  p=$(command -v "$t") || { echo "test-macos-vm-check: no $t on this machine" >&2; exit 1; }
  ln -s "$p" "$SAFE/$t"
done

mk() { # mk <name> <body>: a stub that logs its argv, then runs <body>
  { echo '#!/bin/bash'; echo 'printf "%s %s\n" "$(basename "$0")" "$*" >>"$CALLS"'; echo "$2"; } >"$STUB/$1"
  chmod +x "$STUB/$1"
}
mk uname 'echo "${T_UNAME_M:-arm64}"'
mk sw_vers 'case "$1" in -productVersion) echo "${T_OS:-26.0.1}" ;; -buildVersion) echo 25A362 ;; esac'
mk sysctl 'case "$2" in kern.hv_support) echo 1 ;; kern.hv_vmm_present) echo 0 ;; hw.memsize) echo 34359738368 ;; hw.ncpu) echo 10 ;; machdep.cpu.brand_string) echo "Apple M2 Pro" ;; esac'
mk df 'printf "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/x 999999999 1 900000000 1%% /\n"'
mk pmset "echo \"Now drawing from 'AC Power'\""
mk stat 'echo "${T_CONSOLE_USER:-$(id -un)}"'
mk xcode-select 'echo /Library/Developer/CommandLineTools'
mk git 'case "$3" in describe) echo "${T_TAG:-v0.10.0-rc.10}" ;; status) echo "${T_DIRTY:-}" ;; rev-parse) echo abc123def456 ;; esac'
mk go '[ "$1" = version ] && exit ${T_GO_MISSING:-0}
if [ "$1" = test ]; then [ -z "${T_GO_FAIL:-}" ] || exit 1; while [ $# -gt 0 ]; do [ "$1" = -o ] && { echo bin >"$2"; chmod +x "$2"; }; shift; done; fi'
mk docker 'exit 0'
mk curl 'o=""; while [ $# -gt 0 ]; do [ "$1" = -o ] && o=$2; u=$1; shift; done
case "$o" in
  *.pkg) printf PKGBYTES >"$o" ;;
  *SHA256SUMS) printf "%s  wardyn-runnerd.pkg\n" "${T_BAD_SUM:-$(printf PKGBYTES | sha256sum | cut -d" " -f1)}" >"$o" ;;
esac; exit 0'
mk shasum 'sha256sum "$3"'
mk sudo '[ "$1" = -n ] && exit ${T_SUDO_N:-0}; [ "$1" = tee ] && cat >/dev/null; exit 0'
mk launchctl '[ "$1" = print ] && exit ${T_LOADED:-113}; exit 0'
mk hostname 'echo macmini-test'
mk scutil 'echo "Test Mac"'
mk wardyn 'case "$1 $2" in
  "runner token") echo REGTOKEN-STUB ;;
  "runner trust") if [ -n "${T_TRUST_PENDING:-}" ]; then echo "pending approval: 1 change(s) stored, not applied"; else echo "Runner x (1) trust class is managed_device"; fi ;;
  "runner list") n=$(cat "$WORK/runner_name" 2>/dev/null); c=$(( $(cat "$WORK/lists" 2>/dev/null || echo 0) + 1 )); echo $c >"$WORK/lists"
     t=managed_device; [ -z "${T_TRUST_PENDING:-}" ] || [ $c -ge 3 ] || t=personal
     printf "[{\"id\":\"11111111-2222-3333-4444-555555555555\",\"name\":\"%s\",\"trust_class\":\"%s\"}]\n" "$n" "$t" ;;
  "run --file") cat >/dev/null; echo "{\"id\":\"run-stub\"}" ;;
  "run get") echo "{\"state\":\"RUNNING\"}" ;;
esac; exit 0'
mk wardyn-runnerd 'case "$1 $2" in
  "setup "*) while [ $# -gt 0 ]; do case "$1" in --token-file) f=$2 ;; --name) echo "$2" >"$WORK/runner_name" ;; esac; shift; done
     echo "mode600=$(find "$f" -maxdepth 0 -perm 600 | wc -l) content=$(cat "$f")" >>"$WORK/setup.note" ;;
  "macos-vm status")
     c=$(( $(cat "$WORK/polls" 2>/dev/null || echo 0) + 1 )); echo $c >"$WORK/polls"
     if [ "${T_BASE:-ready}" = ready ] || [ $c -gt "${T_READY_AFTER:-0}" ]; then echo "base: ready, 26.0"; else
       echo "base: none"; echo "create the account wardyn-admin with the one-time code in /x/setup-code.json as its password"; fi
     [ "$3" != --json ] || [ -z "${T_LEAK_CODE:-}" ] || echo "{\"code\": \"000000\"}"
     echo "macOS VMs on this Mac (every login): ${T_RUNS:-0}; this runner offers 2 at once" ;;
esac; exit 0'
mk pnpm 'if [ "$1" = exec ]; then
  : "$WARDYN_MACVM_CONSOLE_URL"
  echo "tokenfile-mode600=$(find "$WARDYN_MACVM_CONSOLE_TOKEN_FILE" -maxdepth 0 -perm 600 | wc -l) file=${WARDYN_MACVM_CONSOLE_FILE_NAME:+set}" >>"$WORK/console.note"
  for c in viewer-frame no-watch-link sandbox-card files output; do
    s=PASS; [ "$c-${T_CONSOLE_FAIL:-}" != "files-1" ] || s=FAIL
    printf "{\"check\":\"%s\",\"status\":\"%s\",\"reason\":\"%s\",\"ms\":5,\"finished_at\":\"x\"}\n" "$c" "$s" "${T_CONSOLE_REASON:-}" >"$WARDYN_MACVM_CONSOLE_OUT/console-$c.json"
  done
fi; exit 0'
mk node 'exit 0'
cat >"$CO/test/conformance/macvm/run.sh" <<'RUNSH'
#!/bin/bash
printf 'run.sh %s | live=%s stop=%s prompts=%s runner=%s clockfor=%s token=%s\n' "$3" "${WARDYN_MACVM_LIVE:-}" "${WARDYN_MACVM_STOP_AT:-}" \
  "${WARDYN_MACVM_PROMPTS:-}" "${WARDYN_MACVM_RUNNER:-}" "${WARDYN_MACVM_CLOCK_FOR:-}" "${WARDYN_TOKEN:+set}" >>"$CALLS"
mkdir -p "$2"; cp "$T_SUMMARY" "$2/summary.tsv"
echo "result: ${T_SUITE_RESULT:-complete}"
[ -z "${T_BLOCK_NOT_RUN:-}" ] || echo "$T_BLOCK_NOT_RUN BLOCK NOT RUN"
exit "${T_SUITE_RC:-0}"
RUNSH
chmod +x "$CO/test/conformance/macvm/run.sh"

printf 'row\toutcome\treason\tduration\nP04\tPASS\t\t120\nP05\tPASS\t\t30\nP06\tNOT_RUN\tneeds WARDYN_MACVM_ALLOWED_URL\t0\nP20\tNOT_RUN\textra\t0\n' >"$WORK/summary.good"
EXTRA='P07\tPASS\t\t5\tEXTRACOL-SENTINEL\n'

n=0
# go <args...>: a fresh home and out dir, one run of the script; its output is in $O, its status in $RC.
go_run() {
  n=$((n + 1)); H=$WORK/home-$n; OUT=$WORK/out-$n; mkdir -p "$H"
  : >"$CALLS"; rm -f "$WORK/runner_name" "$WORK/lists" "$WORK/polls" "$WORK/setup.note" "$WORK/console.note"
  O=$WORK/o-$n.txt
  env -i PATH="$STUB:$SAFE" HOME="$H" CALLS="$CALLS" WORK="$WORK" CHECK_POLL_SECONDS=0 \
    T_SUMMARY="${T_SUMMARY:-$WORK/summary.good}" "${ENVX[@]:-A=a}" \
    bash "$CO/scripts/macos-vm-check.sh" --version 0.10.0-rc.10 --out "$OUT" "$@" >"$O" 2>&1
  RC=$?
}
ENVX=()
CONSENT=--i-understand-this-changes-this-mac
OKFLAG="$CONSENT --base-ready"
with_env() { ENVX=("$@"); }
calls_have() { grep -Eq -e "$1" "$CALLS"; }
expect_rc() { [ "$RC" = "$1" ] || fail "$2: exit $RC, want $1 ($(tail -n 3 "$O" | tr '\n' ' '))"; }
expect_calls() { calls_have "$1" || fail "$2: no call matching $1"; }
expect_no_calls() { ! calls_have "$1" || fail "$2: a call matched $1: $(grep -E -e "$1" "$CALLS" | head -n 2 | tr '\n' ' ')"; }
expect_out() { grep -Eq -e "$1" "$O" || fail "$2: output lacks $1"; }
count_calls() { grep -Ec -e "$1" "$CALLS"; }

# 1. No consent: the preflight runs, the refusal lists what would change, and nothing changes.
with_env; go_run
expect_rc 2 "no consent"
for w in "install a runner" "restart that runner" "fill a run's disk" "wedge a guest"; do expect_out "$w" "no consent lists '$w'"; done
expect_no_calls '^(curl|wardyn|wardyn-runnerd|pnpm) ' "no consent runs nothing mutating"
expect_no_calls '^sudo (installer|tee|mkdir|chown|rm)' "no consent uses no sudo to change anything"
expect_no_calls '^docker compose' "no consent starts no stack"
[ ! -e "$OUT" ] || fail "no consent created the out dir"

# 2. --preflight alone passes, and names what is missing otherwise.
with_env; go_run --preflight; expect_rc 0 "preflight ok"; [ ! -e "$OUT" ] || fail "--preflight created the out dir"
with_env T_UNAME_M=x86_64 T_GO_MISSING=1 T_CONSOLE_USER=nobody; go_run --preflight
expect_rc 2 "preflight missing"; expect_out 'MISSING +Apple silicon' "names Apple silicon"; expect_out 'MISSING +Go' "names Go"
expect_out 'MISSING +a user logged in' "names the desktop session"
with_env T_TAG=v0.9.9; go_run --preflight; expect_rc 2 "tag mismatch"; expect_out 'MISSING +this checkout is at the tag v0.10.0-rc.10' "names the tag"
with_env T_TAG=v0.9.9; go_run $OKFLAG; expect_rc 2 "tag mismatch with consent"; expect_no_calls '^sudo installer' "tag mismatch installs nothing"

# 3. An existing runner is refused unless allowed.
n=$((n + 1)); H=$WORK/home-$n; mkdir -p "$H/Library/Application Support/wardyn"; : >"$H/Library/Application Support/wardyn/runnerd.json"
: >"$CALLS"; O=$WORK/o-$n.txt
env -i PATH="$STUB:$SAFE" HOME="$H" CALLS="$CALLS" WORK="$WORK" T_SUMMARY="$WORK/summary.good" \
  bash "$CO/scripts/macos-vm-check.sh" --version 0.10.0-rc.10 --out "$WORK/out-x" $OKFLAG >"$O" 2>&1; RC=$?
expect_rc 2 "existing runner"; expect_out 'MISSING +no other runner' "names the existing runner"; expect_no_calls '^sudo installer' "existing runner: no install"
: >"$CALLS"
env -i PATH="$STUB:$SAFE" HOME="$H" CALLS="$CALLS" WORK="$WORK" T_SUMMARY="$WORK/summary.good" T_RUNS=2 \
  bash "$CO/scripts/macos-vm-check.sh" --version 0.10.0-rc.10 --out "$WORK/out-y" --allow-existing-runner --preflight >"$O" 2>&1; RC=$?
expect_rc 2 "existing runner with live runs"; expect_out 'MISSING +no live runs' "refuses live runs"

# 4. A full local run, start to finish. A base that already exists is refused unless --base-ready says to reuse it.
with_env; go_run $CONSENT; expect_rc 2 "a ready base without --base-ready"; expect_no_calls '^wardyn-runnerd setup' "a ready base is not set up over"
with_env; go_run $OKFLAG --full
expect_rc 0 "full run"
[ "$(awk -F'\t' 'NR>1 && $2!="ok"' "$OUT/steps.tsv" | wc -l)" = 0 ] || fail "a step is not ok: $(cat "$OUT/steps.tsv")"
for s in fetch stack install register base build suite; do grep -q "^$s	ok" "$OUT/steps.tsv" || fail "step $s missing from steps.tsv"; done
[ "$(find "$OUT/state/admin.secret" -perm 600 | wc -l)" = 1 ] || fail "admin secret is not mode 600"
[ "$(find "$OUT/state/stack.env" -perm 600 | wc -l)" = 1 ] || fail "stack.env is not mode 600"
SECRET=$(cat "$OUT/state/admin.secret"); [ ${#SECRET} -ge 32 ] || fail "the generated secret is short"
grep -Fq "$SECRET" "$CALLS" && fail "the admin secret reached a command's argv"
grep -rFq "$SECRET" "$OUT" --exclude=admin.secret --exclude=stack.env && fail "the admin secret is in the out dir outside its two files"
grep -q 'mode600=1 content=REGTOKEN-STUB' "$WORK/setup.note" || fail "setup did not get the registration token as a mode 600 file ($(cat "$WORK/setup.note" 2>/dev/null))"
[ ! -e "$OUT/state/registration.token" ] || fail "the registration token file was left behind"
grep -q 'REGTOKEN' "$CALLS" && fail "the registration token reached argv"
expect_calls '^sudo installer -pkg .*wardyn-runnerd.pkg -target /' "installs the package"
expect_calls '^sudo chown root:wheel /Library/Application Support/Wardyn/posture.json' "posture is root's"
expect_calls '^wardyn runner trust 11111111-2222-3333-4444-555555555555 managed_device' "trusts the runner"
expect_calls 'docker compose -p wardyn-check -f .*docker-compose.yaml -f .*stack.override.yaml --env-file' "starts the local stack with its override"
grep -q 'WARDYN_WARDYND_IMAGE=ghcr.io/cjohnstoniv/wardynd:0.10.0-rc.10' "$OUT/state/stack.env" || fail "the stack does not use the release's image"
grep -q 'WARDYN_MACOS_VMS: "1"' "$OUT/state/stack.override.yaml" || fail "the stack override does not turn macOS VMs on"
expect_no_calls 'macos-vm prepare' "the script never starts prepare"
expect_calls '^run.sh \^TestP \| live=1 stop= prompts= runner=wardyn-check-.* clockfor= token=set' "run.sh gets the live flag, runner and token"
[ -f "$OUT/report.md" ] || fail "no report.md"
[ "$(sed -n 1p "$OUT/report.md")" = "# macOS VM check report" ] || fail "report title"
grep -q '^result: complete' "$OUT/report.md" || fail "report does not say result: complete"
grep -q '^counts: PASS 2, FAIL 0, BLOCK 0, NOT_RUN 2' "$OUT/report.md" || fail "report counts: $(grep '^counts' "$OUT/report.md")"
grep -q 'P06 | NOT_RUN | 0 | needs WARDYN_MACVM_ALLOWED_URL' "$OUT/report.md" || fail "report row table"
grep -Eq 'http|macmini|'"$USER" "$OUT/report.md" && fail "the report names a URL, a host or a user"

# 5. Exit codes: 4 for incomplete, never green.
with_env T_SUITE_RC=4 T_SUITE_RESULT=incomplete; go_run $OKFLAG
expect_rc 4 "suite incomplete"; [ "$(sed -n 1p "$OUT/report.md")" = "# INCOMPLETE: this is not a pass" ] || fail "incomplete is not prominent"; grep -q '^result: incomplete' "$OUT/report.md" || fail "report result"
with_env T_SUITE_RC=0 T_SUITE_RESULT=incomplete; go_run $OKFLAG; expect_rc 4 "exit 0 but result incomplete"
with_env T_SUITE_RC=0 T_SUITE_RESULT=nothing; go_run $OKFLAG; expect_rc 4 "exit 0 with no readable result"
with_env T_BLOCK_NOT_RUN=2; go_run $OKFLAG; expect_rc 4 "a BLOCK row not run"; grep -q 'BLOCK rows not run: 2' "$OUT/report.md" || fail "report lacks the BLOCK NOT RUN count"
printf 'row\toutcome\treason\tduration\nP04\tNOT_RUN\tx\t0\n' >"$WORK/summary.none"
with_env T_SUMMARY="$WORK/summary.none"; go_run $OKFLAG; expect_rc 4 "nothing passed"
with_env T_SUITE_RC=1; go_run $OKFLAG; expect_rc 1 "a FAIL"
with_env T_SUITE_RC=2; go_run $OKFLAG; expect_rc 2 "a BLOCK"
with_env T_SUITE_RC=3; go_run $OKFLAG; expect_rc 2 "a summary write error is not incomplete"

# 6. The report is an allowlist, then a deny scan; a hit writes no report.
printf 'row\toutcome\treason\tduration\nP04\tPASS\t\t120\tEXTRACOL-SENTINEL\n' >"$WORK/summary.extra"
with_env T_SUMMARY="$WORK/summary.extra"; go_run $OKFLAG; expect_rc 0 "extra column"
grep -q EXTRACOL "$OUT/report.md" && fail "a column outside the allowlist reached the report"
printf 'row\toutcome\treason\tduration\nP04\tFAIL\ta `cmd` $(x) | b\t1\n' >"$WORK/summary.meta"
with_env T_SUMMARY="$WORK/summary.meta" T_SUITE_RC=1; go_run $OKFLAG
grep -q '| P04 | FAIL | 1 | a cmd (x) b |' "$OUT/report.md" || fail "the field filter: $(grep P04 "$OUT/report.md")"
plant() { # plant <label> <text>: the text in a row's reason must leave no report
  printf 'row\toutcome\treason\tduration\nP04\tPASS\t%s\t1\n' "$2" >"$WORK/summary.plant"
  with_env T_SUMMARY="$WORK/summary.plant"; go_run $OKFLAG
  [ ! -e "$OUT/report.md" ] || fail "planted $1: a report was written"
  expect_out 'No report written' "planted $1 says so"; [ "$RC" != 0 ] || fail "planted $1: exit 0"
  grep -Fq -- "$2" "$O" && fail "planted $1: the output repeats the planted text"
}
plant "a URL" "see https://org.example/x"
plant "an IPv4 address" "peer 10.1.2.3 refused"
plant "a hostname" "host build.corp refused"
plant "a Mac host name" "talked to macmini-test"
plant "a home path" "wrote /Users/alice/notes"
plant "a wdn_ token" "wdn_abcdefghijklmnop"
plant "a wdg_ token" "key wdg_abcd1234"
plant "an AWS key" "AKI""AIOSFODNN7EXAMPLE"
plant "a private key" "-----BEGIN PRIVATE KEY"
plant "ec2-user" "login as ec2-user"
plant "an AWS account id" "account 123456789012"
plant "a run id" "run 3f2b8c1e-1111-2222-3333-444455556666"
SECRET_PLANTED=$(cat "$OUT/state/admin.secret"); plant "the admin secret" "x $SECRET_PLANTED"
# 6b. A clean report still goes through after those.
with_env; go_run $OKFLAG; [ -f "$OUT/report.md" ] || fail "a clean run wrote no report"
# The guest's own paths are not a home path.
printf 'row\toutcome\treason\tduration\nP26\tPASS\tfiles in /Users/agent/work\t1\n' >"$WORK/summary.guest"
with_env T_SUMMARY="$WORK/summary.guest"; go_run $OKFLAG; [ -f "$OUT/report.md" ] || fail "the guest's /Users/agent path was refused"

# 7. Resume: a crash after the install leaves finished steps, and a rerun skips them.
with_env T_GO_FAIL=1; go_run $OKFLAG
expect_rc 2 "build fails"; grep -q '^build	failed' "$OUT/steps.tsv" || fail "build not recorded failed"
installs=$(count_calls '^sudo installer'); curls=$(count_calls '^curl .*wardyn-runnerd.pkg'); setups=$(count_calls '^wardyn-runnerd setup'); ups=$(count_calls 'compose .* up ')
O1=$OUT; H1=$H
: >"$CALLS"
env -i PATH="$STUB:$SAFE" HOME="$H1" CALLS="$CALLS" WORK="$WORK" CHECK_POLL_SECONDS=0 T_SUMMARY="$WORK/summary.good" \
  bash "$CO/scripts/macos-vm-check.sh" --version 0.10.0-rc.10 --out "$O1" $OKFLAG >"$WORK/o-resume.txt" 2>&1; RC=$?
[ "$RC" = 0 ] || fail "resume exit $RC: $(tail -n 3 "$WORK/o-resume.txt")"
[ "$installs" = 1 ] && [ "$(count_calls '^sudo installer')" = 0 ] || fail "resume reinstalled (first $installs, again $(count_calls '^sudo installer'))"
[ "$(count_calls '^curl ')" = 0 ] && [ "$(count_calls '^wardyn-runnerd setup')" = 0 ] && [ "$(count_calls 'compose .* up ')" = 0 ] || fail "resume repeated the download, the setup or the stack"
grep -q 'finished earlier, skipped' "$WORK/o-resume.txt" || fail "resume does not say what it skipped"
grep -q '^suite	ok' "$O1/steps.tsv" || fail "resume did not run the suite"
env -i PATH="$STUB:$SAFE" HOME="$H1" CALLS="$CALLS" WORK="$WORK" T_SUMMARY="$WORK/summary.good" \
  bash "$CO/scripts/macos-vm-check.sh" --version 0.9.0 --out "$O1" $OKFLAG >"$WORK/o-ver.txt" 2>&1
[ $? = 2 ] || fail "an out dir for another version was reused"

# 8. --stop-at, the quick set, and the budgets.
with_env; go_run $OKFLAG --stop-at 2000-01-01T00:00:00Z
expect_rc 2 "stop-at in the past"; expect_no_calls '^sudo installer' "stop-at past: no install"; grep -q '^fetch	stopped' "$OUT/steps.tsv" || fail "stopped step not recorded"
with_env; go_run $OKFLAG --stop-at 2999-01-01T00:00:00Z; expect_calls 'stop=2999-01-01T00:00:00Z' "run.sh gets the stop time"
with_env; go_run $OKFLAG --stop-at tomorrow; expect_rc 2 "bad stop-at"
with_env; go_run $OKFLAG --quick
expect_calls 'clockfor=10m' "quick shortens the time budgets"; grep -q 'quick run can never read complete' "$OUT/report.md" || fail "the quick report does not say it cannot read complete"
pat=$(grep -o '^run.sh [^ ]*' "$CALLS" | cut -d' ' -f2)
for r in TestP05_AgentSession TestP23_X; do grep -Eq "$pat" <<<"$r" || fail "quick lacks $r"; done
for r in TestP12_CapSlotsAndLogins TestP20_ManagedGate TestP09_Desktop; do ! grep -Eq "$pat" <<<"$r" || fail "quick includes the BLOCK row $r"; done
with_env; go_run $OKFLAG --quick --full; expect_rc 2 "quick and full"
with_env; go_run; : # (no version flag path below)
n=$((n + 1)); env -i PATH="$STUB:$SAFE" HOME="$WORK" bash "$CO/scripts/macos-vm-check.sh" $OKFLAG >/dev/null 2>&1; [ $? = 2 ] || fail "no --version was not a usage error"

# 9. The base: the script prints the command, never runs it, and waits.
with_env T_BASE=none T_READY_AFTER=12; go_run $CONSENT
expect_rc 0 "base wait"; expect_out 'wardyn-runnerd macos-vm prepare' "prints the prepare command"; expect_no_calls '^wardyn-runnerd macos-vm prepare' "never runs prepare"
grep -q 'code_shape	ok' "$OUT/state/state.tsv" || fail "the code shape check did not pass: $(grep code_shape "$OUT/state/state.tsv")"
grep -q 'base_ours	1' "$OUT/state/state.tsv" || fail "base_ours not recorded"
with_env T_BASE=none T_READY_AFTER=12 T_LEAK_CODE=1; go_run $CONSENT
expect_rc 1 "a code field in the status"; grep -q 'code_shape	fail' "$OUT/state/state.tsv" || fail "code_shape fail not recorded"
with_env; go_run $OKFLAG; grep -q 'code_shape	not_run' "$OUT/state/state.tsv" || fail "a ready base should leave the shape check not_run"
with_env; go_run --preflight; : ; 
# a ready base is refused unless reused
n=$((n + 1)); : >"$CALLS"

# 10. Trust under four-eyes waits for the second admin.
with_env T_TRUST_PENDING=1; go_run $OKFLAG
expect_rc 0 "four-eyes trust"; expect_out 'second admin must approve' "tells the second admin what to run"

# 11. --server: names what it writes, creates no stack, keeps P20 out by default, leaves the URL out of the report.
with_env; go_run $OKFLAG --server https://org.example.test
expect_rc 2 "server without a token"; expect_out 'MISSING +WARDYN_TOKEN' "names the token"
with_env WARDYN_TOKEN=personal-token-value-xyz; go_run $OKFLAG --server https://org.example.test
expect_rc 0 "server run"; expect_out 'writes to the organisation' "states what it writes"; expect_out 'P20 changes governance state' "states the P20 limit"
expect_no_calls '^docker compose' "server mode starts no stack"
grep -Fq personal-token-value-xyz "$CALLS" && fail "the server token reached argv"
grep -Fq org.example "$OUT/report.md" && fail "the server URL is in the report"
grep -q 'P20' "$OUT/report.md" || fail "the report does not say P20 was left out"
pat=$(grep -o '^run.sh [^ ]*' "$CALLS" | cut -d' ' -f2)
! grep -Eq "$pat" <<<"TestP20_ManagedGate" && grep -Eq "$pat" <<<"TestP12_CapSlotsAndLogins" || fail "server pattern: P20 out, P12 in"
with_env WARDYN_TOKEN=personal-token-value-xyz; go_run $OKFLAG --server https://org.example.test --allow-org-writes
expect_calls '^run.sh \^TestP \|' "--allow-org-writes runs every row"

# 12. --console runs the console checks' config with a token FILE, ends its run, and reports status and reason only.
with_env T_CONSOLE_REASON=viewer; go_run $OKFLAG --console
expect_rc 0 "console run"; expect_calls '^pnpm exec playwright test --config playwright.macos-vm.config.ts' "runs the console config"
grep -q 'tokenfile-mode600=1 file=set' "$WORK/console.note" || fail "console env: $(cat "$WORK/console.note" 2>/dev/null)"
expect_calls '^wardyn run kill run-stub' "ends the console run"
grep -q '^| files | PASS' "$OUT/report.md" || fail "console rows not in the report"
with_env T_CONSOLE_FAIL=1; go_run $OKFLAG --console; expect_rc 1 "a console FAIL"
with_env; go_run $OKFLAG; grep -q 'viewer-frame | NOT_RUN | not requested' "$OUT/report.md" || fail "without --console the rows are not NOT_RUN"
with_env T_CONSOLE_REASON="saw http://x.test/a" ; go_run $OKFLAG --console; [ ! -e "$OUT/report.md" ] || fail "a URL in a console reason reached the report"

# 13. --cleanup: consent, the marker, no runs, and only what the state file records.
with_env; go_run $OKFLAG; OUTC=$OUT; HC=$H
cl() { : >"$CALLS"; env -i PATH="$STUB:$SAFE" HOME="$HC" CALLS="$CALLS" WORK="$WORK" "$@" bash "$CO/scripts/macos-vm-check.sh" --version 0.10.0-rc.10 --out "$OUTC" --cleanup $OKFLAG2 >"$WORK/o-clean.txt" 2>&1; RC=$?; }
OKFLAG2=""; cl; [ "$RC" = 2 ] && ! grep -q '^wardyn runner revoke' "$CALLS" || fail "cleanup without consent acted"
OKFLAG2=$OKFLAG; cl T_RUNS=1; [ "$RC" = 2 ] && ! grep -q '^wardyn runner revoke' "$CALLS" || fail "cleanup went ahead with runs present"
cl; [ "$RC" = 0 ] || fail "cleanup exit $RC: $(tail -n 3 "$WORK/o-clean.txt")"
expect_calls '^wardyn runner revoke wardyn-check-' "cleanup revokes the recorded runner"
expect_calls '^launchctl bootout' "cleanup stops the recorded service"
expect_calls '^docker compose -p wardyn-check .* down -v' "cleanup removes the recorded stack"
expect_calls '^sudo rm -f /Library/Application Support/Wardyn/posture.json' "cleanup removes the posture file it wrote"
expect_no_calls 'macos-vm remove' "cleanup keeps a base it did not prepare"
[ ! -e "$OUTC/state/admin.secret" ] || fail "cleanup left the admin secret"
grep -q 'left in place' "$WORK/o-clean.txt" || fail "cleanup does not say it leaves the package"
# a run that prepared its base removes it; an out dir with no marker is refused.
with_env T_BASE=none T_READY_AFTER=12; go_run $CONSENT; OUTC=$OUT; HC=$H; cl; expect_calls 'macos-vm remove' "cleanup removes the base this run prepared"
mkdir -p "$WORK/nomark/state"; printf 'version\t0.10.0-rc.10\nrunner_name\tsomeone-else\n' >"$WORK/nomark/state/state.tsv"
OUTC=$WORK/nomark; cl; [ "$RC" = 2 ] && ! grep -q '^wardyn runner revoke' "$CALLS" || fail "cleanup deregistered a runner with no marker"

[ "$fails" = 0 ] || { echo "test-macos-vm-check: $fails FAILED" >&2; exit 1; }
echo "test-macos-vm-check: PASS"
