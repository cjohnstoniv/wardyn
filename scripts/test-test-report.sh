#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free test for the two Go-suite controls in scripts/test-report.sh: the flaky-test quarantine
# (test/go-quarantine.txt, `go test -skip`) and the PG shard's test selection
# (scripts/pg-test-select.sh and its unit-run guard). Fixture modules under a temp dir; the checked-in
# quarantine file is validated against today's date, which is what turns every PR red the day an
# entry expires.
#
# Usage: scripts/test-test-report.sh   (exit 0 = PASS, non-zero = FAIL)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "${REPO_ROOT}/scripts/lib/e2e-quarantine.sh"

fail() { echo "test-test-report: FAIL: $*" >&2; exit 1; }
ok() { echo "  [pass] $*"; }

tmp="$(mktemp -d)"
report="tqfixture-$$"
trap 'rm -rf "${tmp}" "${REPO_ROOT}/test/reports/go/${report}"' EXIT
today="$(date -u +%F)"

out="$(quarantine_validate "${REPO_ROOT}/test/go-quarantine.txt" "${today}")" \
  || fail "test/go-quarantine.txt: ${out}"
ok "checked-in test/go-quarantine.txt passes against today"

# ── quarantine, through test-report.sh ────────────────────────────────────────
mkdir "${tmp}/q"
cat > "${tmp}/q/go.mod" <<'MOD'
module fixture

go 1.20
MOD
cat > "${tmp}/q/a_test.go" <<'GO'
package fixture

import "testing"

func TestKeep(t *testing.T)  {}
func TestFlaky(t *testing.T) { t.Fatal("flaky") }
func TestSub(t *testing.T) {
	t.Run("good", func(t *testing.T) {})
	t.Run("bad", func(t *testing.T) { t.Fatal("flaky subtest") })
}
GO
run_report() { # <quarantine file>: runs the fixture suite, prints its log, returns its exit code
  (cd "${tmp}/q" && WARDYN_GO_QUARANTINE="$1" WARDYN_TEST_REPORT_NAME="${report}" \
    WARDYN_TEST_REPORT_SKIP_FLOOR=1 WARDYN_TEST_REPORT_COVER=0 "${REPO_ROOT}/scripts/test-report.sh" fixture ./... 2>&1)
}
future="$(date -u -d "${today} + 3 days" +%F)"
past="$(date -u -d "${today} - 1 day" +%F)"
far="$(date -u -d "${today} + 30 days" +%F)"

: > "${tmp}/none.txt"
run_report "${tmp}/none.txt" >/dev/null && fail "control: the fixture's TestFlaky should fail an unquarantined run"
ok "control: an unquarantined flaky test fails the suite"

echo "fixture | TestFlaky | #12 | @cjohnstoniv | ${past}" > "${tmp}/expired.txt"
log="$(run_report "${tmp}/expired.txt")" && fail "an expired entry must fail the suite"
grep -q "expired on ${past}" <<<"${log}" || fail "expired entry: no 'expired' message in: ${log}"
grep -q "running suite" <<<"${log}" && fail "expired entry: the suite ran anyway"
ok "an expired entry fails before anything runs"

echo "fixture | TestFlaky | #12 | @cjohnstoniv | ${far}" > "${tmp}/far.txt"
run_report "${tmp}/far.txt" >/dev/null && fail "an entry past the 14-day cap must fail the suite"
echo "fixture | TestFlaky | cjohnstoniv | ${future}" > "${tmp}/bad.txt"
run_report "${tmp}/bad.txt" >/dev/null && fail "a malformed entry must fail the suite"
ok "a >14-day and a malformed entry fail the suite"

printf 'fixture | TestFlaky | #12 | @cjohnstoniv | %s\nfixture | TestSub/bad | #13 | @cjohnstoniv | %s\n' "${future}" "${future}" > "${tmp}/valid.txt"
log="$(run_report "${tmp}/valid.txt")" || fail "valid entries should let the suite pass, got: ${log}"
grep -q "QUARANTINED" <<<"${log}" || fail "the quarantined tests are not printed: ${log}"
grep -q "fixture TestFlaky #12" <<<"${log}" || fail "the summary does not name TestFlaky: ${log}"
json="${REPO_ROOT}/test/reports/go/${report}/test-output.json"
grep -q '"Test":"TestFlaky"' "${json}" && fail "TestFlaky ran although quarantined"
grep -q '"Action":"pass","Package":"fixture","Test":"TestKeep"' "${json}" || fail "TestKeep did not run"
grep -q '"Action":"pass","Package":"fixture","Test":"TestSub/good"' "${json}" || fail "TestSub/good did not run"
grep -q '"Test":"TestSub/bad"' "${json}" && fail "TestSub/bad ran although quarantined"
ok "a valid entry skips exactly the named test (and subtest), the rest run, the summary names them"

# ── PG test selection ────────────────────────────────────────────────────────
mkdir "${tmp}/s"
cat > "${tmp}/s/go.mod" <<'MOD'
module fixture

go 1.20
MOD
cat > "${tmp}/s/gate_test.go" <<'GO'
package fixture

import (
	"os"
	"testing"

	"fixture/pgtestutil"
)

var testfloor = struct{ Mark func(*testing.T, string) }{func(*testing.T, string) {}}

func pgOpen(t *testing.T) {
	if os.Getenv("WARDYN_TEST_PG") == "" {
		t.Skip("WARDYN_TEST_PG not set")
	}
}

func helper(t *testing.T) { pgOpen(t) }

// An export_test.go alias: a var, not a func, that a test reaches the gate through.
var Alias = helper

var (
	GroupAlias = helper
)

type fx struct{}

func (fx) viaMethod(t *testing.T) { helper(t) }

// TestInComment mentions WARDYN_TEST_PG only here.
func TestInComment(t *testing.T) {}
func TestPlain(t *testing.T)     {}
func TestDirect(t *testing.T)    { pgOpen(t) }
func TestViaHelper(t *testing.T) { helper(t) }
func TestViaMethod(t *testing.T) { fx{}.viaMethod(t) }
func TestViaAlias(t *testing.T)  { Alias(t) }
func TestViaGroup(t *testing.T)  { GroupAlias(t) }
func TestCrossPkg(t *testing.T)  { pgtestutil.Open2(t) }
func TestProbe(t *testing.T) {
	testfloor.Mark(t, "pg")
}
GO
# A gate that lives in a helper package (no _test.go file), reached through a second helper.
mkdir "${tmp}/s/pgtestutil"
cat > "${tmp}/s/pgtestutil/pgtestutil.go" <<'GO'
package pgtestutil

import (
	"os"
	"testing"
)

func Open(t *testing.T) {
	if os.Getenv("WARDYN_TEST_PG") == "" {
		t.Skip("WARDYN_TEST_PG not set")
	}
}

func Open2(t *testing.T) { Open(t) }
GO
got="$(cd "${tmp}/s" && "${REPO_ROOT}/scripts/pg-test-select.sh" ./...)" || fail "selector failed on the fixture"
want='^(TestCrossPkg|TestDirect|TestProbe|TestViaAlias|TestViaGroup|TestViaHelper|TestViaMethod)$'
[ "${got}" = "${want}" ] || fail "selector picked ${got}, want ${want}"
ok "selector: direct, helper, method, alias (single and grouped), cross-package and probe tests selected; plain and comment-only not"

# The slices: the slow tests in a weights file are dealt out heaviest first to the lightest slice,
# the rest by hash; together the slices cover the selection and share only the probe.
printf '10\tTestViaHelper\n9\tTestDirect\n2\tTestViaMethod\n1\tTestViaAlias\n' > "${tmp}/weights.tsv"
slice() { (cd "${tmp}/s" && PG_TEST_WEIGHTS="${tmp}/weights.tsv" "${REPO_ROOT}/scripts/pg-test-select.sh" --part "$1" ./...); }
s1="$(slice 1/2)" || fail "slice 1/2 failed"; s2="$(slice 2/2)" || fail "slice 2/2 failed"
for t in TestViaHelper TestViaAlias; do grep -qw "${t}" <<<"${s1}" || fail "${t} not in slice 1: ${s1}"; grep -qw "${t}" <<<"${s2}" && fail "${t} also in slice 2"; done
for t in TestDirect TestViaMethod; do grep -qw "${t}" <<<"${s2}" || fail "${t} not in slice 2: ${s2}"; grep -qw "${t}" <<<"${s1}" && fail "${t} also in slice 1"; done
all="$({ tr -d '^()$' <<<"${s1}"; echo '|'; tr -d '^()$' <<<"${s2}"; } | tr -d '\n' | tr '|' '\n' | sort -u | paste -sd'|')"
[ "${all}" = "$(tr -d '^()$' <<<"${want}")" ] || fail "slices do not cover the selection: ${all}"
[ "$(grep -o TestProbe <<<"${s1}${s2}" | wc -l)" = 2 ] || fail "the probe is not in both slices"
ok "selector: --part packs the weighted tests by seconds, slices cover the selection, probe in every slice"

# The guard: a unit run (DSN unset) where TestHidden skipped naming WARDYN_TEST_PG, but the selector
# cannot see how it reaches the gate, must fail; one the selector does see must pass.
ev() { printf '{"Action":"%s","Package":"fixture","Test":"%s"%s}\n' "$1" "$2" "${3:-}"; }
{
  ev output TestDirect ',"Output":"    gate_test.go:1: WARDYN_TEST_PG not set\\n"'; ev skip TestDirect
  ev pass TestPlain
} > "${tmp}/unit-ok.json"
(cd "${tmp}/s" && "${REPO_ROOT}/scripts/pg-test-select.sh" --guard "${tmp}/unit-ok.json" ./... >/dev/null 2>&1) \
  || fail "guard rejected a selected PG test"
{
  cat "${tmp}/unit-ok.json"
  ev output TestHidden ',"Output":"    x_test.go:9: WARDYN_TEST_PG not set\\n"'; ev skip TestHidden
} > "${tmp}/unit-bad.json"
glog="$(cd "${tmp}/s" && "${REPO_ROOT}/scripts/pg-test-select.sh" --guard "${tmp}/unit-bad.json" ./... 2>&1)" \
  && fail "guard passed a PG-skipping test outside the selection"
grep -q "fixture TestHidden" <<<"${glog}" || fail "guard did not name TestHidden: ${glog}"
ok "guard: a test that skips for WARDYN_TEST_PG outside the selection fails, naming it"
# The shard package lists: api by default, store on request, anything else refused; the store shard
# is the old whole-package list, so a package dropped from it would silently lose its PG tests.
sel="${REPO_ROOT}/scripts/pg-test-select.sh"
[ "$("${sel}" --packages)" = "$("${sel}" --packages api)" ] || fail "--packages without a set is not api"
grep -q 'test/apie2e' <<<"$("${sel}" --packages api)" || fail "api set lost test/apie2e"
for p in internal/store internal/db internal/secretstore internal/broker internal/recording cmd/wardynd internal/notify internal/livebus; do
  grep -qF "./${p}/..." <<<"$("${sel}" --packages store)" || fail "store set lost ${p}"
done
"${sel}" --packages nope >/dev/null 2>&1 && fail "--packages accepted an unknown set"
ok "selector: --packages names the api and store shard lists, refuses others"

# ── must-pass floors, through test-report.sh ─────────────────────────────────
# A stand-in `go` replays a canned event stream, so each name-list floor is exercised without a daemon:
# every floor's names passing is green; any one of them skipped, or absent, is red and names it. The names are
# written out here on purpose, so a name dropped from test-report.sh's chain fails its case.
stub="${tmp}/stub"
mkdir "${stub}"
cat > "${stub}/go" <<'SH'
#!/usr/bin/env bash
[ "$1" = test ] && [ "$2" = -json ] && exec cat "${STUB_EVENTS}"
echo "stub go: unexpected call: $*" >&2
exit 2
SH
chmod +x "${stub}/go"
floor_run() { # <suite> <events file> [VAR=val ...]: runs test-report.sh on the replayed events, prints its log
  local suite="$1" events="$2"; shift 2
  (cd "${tmp}/q" && env -u WARDYN_TEST_DOCKER -u WARDYN_TEST_K8S -u WARDYN_TEST_K8S_AGENT_SANDBOX -u WARDYN_TEST_PG \
    -u WARDYN_TEST_REPORT_SKIP_FLOOR -u GITHUB_ACTIONS \
    PATH="${stub}:${PATH}" STUB_EVENTS="${events}" WARDYN_GO_QUARANTINE="${tmp}/none.txt" \
    WARDYN_TEST_REPORT_NAME="${report}" WARDYN_TEST_REPORT_COVER=0 "$@" "${REPO_ROOT}/scripts/test-report.sh" "${suite}" ./... 2>&1)
}
floor_events() { # <file> <suite> <skip|-> <names...>: every name passes, the one named in $3 skips; plus the suite's marked probe
  local f="$1" suite="$2" skipped="$3"; shift 3
  : > "${f}"
  ev output TestMarked ",\"Output\":\"WARDYN_FLOOR_PROBE:${suite}\\n\"" >> "${f}"
  ev pass TestMarked >> "${f}"
  local n
  for n in "$@"; do
    if [ "${n}" = "${skipped}" ]; then ev skip "${n}" >> "${f}"; else ev pass "${n}" >> "${f}"; fi
  done
}
# floor_case <label> <suite> <target> <env> <names...>: all pass is green; <target> skipped, or not run, is red.
floor_case() {
  local label="$1" suite="$2" target="$3" envs="$4" log n others=()
  shift 4
  for n in "$@"; do [ "${n}" = "${target}" ] || others+=("${n}"); done
  floor_events "${tmp}/floor.json" "${suite}" - "$@"
  # shellcheck disable=SC2086 # envs is a short list of VAR=val words
  log="$(floor_run "${suite}" "${tmp}/floor.json" ${envs})" || fail "${label}: every name passing should be green, got: ${log}"
  grep -q "all passed" <<<"${log}" || fail "${label}: no 'all passed' line in: ${log}"
  floor_events "${tmp}/floor.json" "${suite}" "${target}" "$@"
  # shellcheck disable=SC2086
  log="$(floor_run "${suite}" "${tmp}/floor.json" ${envs})" && fail "${label}: ${target} skipped must be red, got: ${log}"
  grep -q "SKIPPED" <<<"${log}" && grep -q "${target}" <<<"${log}" || fail "${label}: the skip of ${target} is not named in: ${log}"
  floor_events "${tmp}/floor.json" "${suite}" - "${others[@]}"
  # shellcheck disable=SC2086
  log="$(floor_run "${suite}" "${tmp}/floor.json" ${envs})" && fail "${label}: ${target} absent must be red, got: ${log}"
  grep -qE "did not pass|no test matching" <<<"${log}" && grep -q "${target}" <<<"${log}" || fail "${label}: the absence of ${target} is not named in: ${log}"
}
# floor_each <label> <suite> <env> <names...>: floor_case for every name in the list.
floor_each() {
  local label="$1" suite="$2" envs="$3" t
  shift 3
  for t in "$@"; do floor_case "${label}" "${suite}" "${t}" "${envs}" "$@"; done
  ok "floor ${label}: each of its $# names must pass (skipped and absent both fail)"
}

docker_names=(TestConformanceDocker/L0StructuralEgress TestConformanceDocker/DNSAudit
  TestConformanceDocker/CreateStatusStop TestConformanceDocker/WaitExitCode TestConformanceDocker/ExecStream
  TestConformanceDocker/ManagedFiles TestConformanceDocker/UserDrives TestBootEgress_NoFirstUseApproval
  TestConformanceDocker/PlacementRouting/UnregisteredRunnerIsRefusedNeverSubstituted
  TestConformanceDocker/PlacementRouting/UnadvertisedClassIsRefusedNeverDowngraded
  TestConformanceDocker/PlacementRouting/AskedPlacementIsHonoured
  TestConformanceDockerThroughRemote/PlacementRouting/OfflineRunnerIsRefusedNotRoutedLocally
  TestConformanceDocker/InteractiveAttach TestConformanceDocker/ExecStreamLoopbackRelay
  TestRunLifetimeDocker/EndKeepsAgentAndCutsEgress TestRunLifetimeDocker/LostProxyCutsEgress
  TestRunLifetimeDocker/ReviveReusesAddressAndCA TestMemberDriveDocker TestRecordingDocker
  TestConformanceDocker/KillMissingRef TestConformanceDocker/NoNetwork
  TestRunLifetimeDocker/FreezeAgentOnlyAndInspectExec)
k8s_names=(TestConformanceK8s/AgentCannotReachAPIServer TestConformanceK8s/AgentCannotReachAnythingButTheProxy
  TestConformanceK8s/CreateStatusStop TestConformanceK8s/WaitExitCode TestConformanceK8s/UserDrives
  TestConformanceK8s/PlacementRouting/UnregisteredRunnerIsRefusedNeverSubstituted
  TestConformanceK8s/PlacementRouting/UnadvertisedClassIsRefusedNeverDowngraded
  TestConformanceK8s/PlacementRouting/AskedPlacementIsHonoured TestConformanceK8s/LiveProxyNarrow
  TestConformanceK8s/ClosedOffAfterTeardown TestConformanceK8s/ManagedFiles TestConformanceK8s/ExecStream
  TestConformanceK8s/InteractiveAttach TestConformanceK8s/ExecStreamLoopbackRelay TestConformanceK8s/NoNetwork
  TestConformanceK8s/EphemeralDiskLimit TestRecordingK8s TestConformanceK8s/NoNetworkProxyPodReachesOnlyWardyn)
eph_names=(TestConformanceK8s/EphemeralDiskLimit/OverTheLimitTheRunIsEvicted/Tmp
  TestConformanceK8s/EphemeralDiskLimit/OverTheLimitTheRunIsEvicted/Workdir
  TestConformanceK8s/EphemeralDiskLimit/OverTheLimitTheRunIsEvicted/Cache
  TestConformanceK8s/EphemeralDiskLimit/AnOversizedLimitStillSchedules)
build_names=(TestBuild_SmokeDockerd TestBuildFromDevcontainerFiles_BakesAgentCLI
  TestFinalizeBase_IDEqualsTheDaemonsAuxID)
aws_names=(TestDeviceCodeLoginRealCLI TestParseRealAWSCLICacheFile TestAWSSSOConfigAcceptedByRealBotocore)
unit_names=(TestSSHGateway_DirectTCPIPLoopbackRestriction TestSSHGateway_ForwardAuditSurvivesKill
  TestSSHGateway_VMRunDoors TestSSHGateway_ContainerRunForwardStaysSocat
  TestSSHGateway_RemoteSSHChannelsOnOneConnection TestUIGateway_MacOSVMScreenSharingPortRefused
  TestUIRelay_AnotherAppsIdlePoolDoesNotRefuseAnApp TestUIRelay_ActiveConnectionsStillRefuseAnApp
  TestForwardRefusesMacScreenSharingPorts TestForwardDialsLoopbackOnly)
l0_names=(TestL0_MetadataUnreachable TestL0_ProxyIsSoleEgressPath TestL0_NoDNSExfil TestL0_DNSQueryIsRecorded TestL0_HostAddressUnreachable
  TestReplaceProxy_Live_AgentReachesTheNewProxyAtOnce)
vm_names=(TestLinuxVMSandbox TestLinuxVMSandbox/ForwardDialsGuestLoopbackOnly TestLinuxVMSandbox/HostRulesJudgedInTheGuest TestLinuxVMDrives)
sandbox_names=(TestAgentSandboxGoneAfterTeardown TestAgentSandboxNetworkPolicyFirst)

floor_each "conformance-docker" conformance-docker WARDYN_TEST_DOCKER=1 "${docker_names[@]}"
floor_each "conformance-k8s" conformance-k8s WARDYN_TEST_K8S=1 "${k8s_names[@]}" "${eph_names[@]}"
# The agent-sandbox pair is required only where the lane declares it.
for t in "${sandbox_names[@]}"; do
  floor_case "AGENT_SANDBOX" conformance-k8s "${t}" "WARDYN_TEST_K8S=1 WARDYN_TEST_K8S_AGENT_SANDBOX=1" "${k8s_names[@]}" "${eph_names[@]}" "${sandbox_names[@]}"
done
ok "floor AGENT_SANDBOX: each of its ${#sandbox_names[@]} names must pass (skipped and absent both fail)"
floor_events "${tmp}/floor.json" conformance-k8s - "${k8s_names[@]}" "${eph_names[@]}"
log="$(floor_run conformance-k8s "${tmp}/floor.json" WARDYN_TEST_K8S=1)" || fail "AGENT_SANDBOX unset: the pair must not be required, got: ${log}"
ok "floor AGENT_SANDBOX: the pair is not required while WARDYN_TEST_K8S_AGENT_SANDBOX is unset"
floor_each "envbuild" envbuild WARDYN_TEST_DOCKER=1 "${build_names[@]}"
# The docker suite's list: the AWS three where Docker is declared, the gateway recipes only on the hosted runner,
# and the revive case always.
floor_each "docker" docker "WARDYN_TEST_DOCKER=1 GITHUB_ACTIONS=true" "${aws_names[@]}" TestGatewayRecipes_Live TestStartSandbox_OnlyBehindARunningProxy
floor_each "docker revive" docker "" TestStartSandbox_OnlyBehindARunningProxy
# The gateway recipes are required only on the hosted runner: with Docker declared and no GITHUB_ACTIONS they are not.
floor_events "${tmp}/floor.json" docker - "${aws_names[@]}" TestStartSandbox_OnlyBehindARunningProxy
log="$(floor_run docker "${tmp}/floor.json" WARDYN_TEST_DOCKER=1)" || fail "GITHUB_ACTIONS unset: TestGatewayRecipes_Live must not be required, got: ${log}"
ok "floor docker: TestGatewayRecipes_Live is not required while GITHUB_ACTIONS is unset"
floor_each "l0-docker" l0-docker WARDYN_TEST_DOCKER=1 "${l0_names[@]}"
floor_each "conformance-vm" conformance-vm "" "${vm_names[@]}"
floor_each "unit" unit "" "${unit_names[@]}"
echo "test-test-report: PASS"
