#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Drives the git_credential probe in examples/workspaces/probes.sh against a `docker` stub on PATH, so the
# probe's pass and fail arms are checked with no daemon, no network and no running sandbox. The stub answers
# `docker inspect -f {{.State.Running}}` from a file of lines (one consumed per call, the last repeating) and
# `docker exec` with a canned exit code and output.
#
# Usage: scripts/test-workspace-probes.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROBES="${REPO_ROOT}/examples/workspaces/probes.sh"
HELPER_GO="${REPO_ROOT}/cmd/wardyn-git-helper/main.go"

fail() { echo "test-workspace-probes: FAIL: $*" >&2; exit 1; }
pass() { echo "PASS  $*"; }

dir="$(mktemp -d)"; trap 'rm -rf "${dir}"' EXIT
mkdir -p "${dir}/bin"
export STUB="${dir}/stub"

cat > "${dir}/bin/docker" <<'STUBSH'
#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >> "${STUB}/calls"
case "${1:-}" in
  inspect)
    if [[ "$(cat "${STUB}/running")" == missing ]]; then
      echo "Error: No such object: ${*: -1}" >&2
      exit 1
    fi
    n=$(( $(cat "${STUB}/inspect.n" 2>/dev/null || echo 0) + 1 ))
    echo "${n}" > "${STUB}/inspect.n"
    total="$(wc -l < "${STUB}/running")"
    sed -n "$(( n > total ? total : n ))p" "${STUB}/running"
    ;;
  exec)
    cat "${STUB}/exec.out"
    exit "$(cat "${STUB}/exec.rc")"
    ;;
  *) echo "stub docker: unexpected subcommand: $*" >&2; exit 99 ;;
esac
STUBSH
chmod +x "${dir}/bin/docker"

# The helper's own lines, read from its source so a change to either one fails here.
line_brokered="$(grep -o 'wardyn-git-helper: this run.s GitHub access is brokered through wardyn-proxy[^"]*' "${HELPER_GO}" | head -n1)"
line_nogrant="$(grep -o 'wardyn-git-helper: WARDYN_GITHUB_GRANT_ID not set and no git_pat grant for this host; skipping broker mint (git will prompt)' "${HELPER_GO}" | head -n1)"
[[ -n "${line_brokered}" ]] || fail "the helper's brokered-refusal line is not in ${HELPER_GO}"
[[ -n "${line_nogrant}" ]] || fail "the helper's no-grant line is not in ${HELPER_GO}"

rc=0; out=""
# run_case <name> <running lines, \n separated> <exec rc> <exec output>
run_case() {
  local name="$1" running="$2" exec_rc="$3" exec_out="$4"
  rm -rf "${STUB}"; mkdir -p "${STUB}"
  printf '%s\n' "${running}" > "${STUB}/running"
  printf '%s' "${exec_rc}" > "${STUB}/exec.rc"
  printf '%s\n' "${exec_out}" > "${STUB}/exec.out"
  : > "${STUB}/calls"
  rc=0
  out="$(PATH="${dir}/bin:${PATH}" SANDBOX_REF=wardyn-agent-test timeout 60 bash "${PROBES}" git_credential 2>&1)" || rc=$?
  CASE="${name}"
}
want_rc0()  { [[ "${rc}" -eq 0 ]] || fail "${CASE}: rc ${rc}, want 0: ${out}"; }
want_rcne() { [[ "${rc}" -ne 0 ]] || fail "${CASE}: rc 0, want non-zero: ${out}"; }
want_text() { grep -qF -- "$1" <<<"${out}" || fail "${CASE}: output lacks '$1': ${out}"; }
want_pass() { grep -q 'PASS' <<<"${out}" || fail "${CASE}: no PASS line: ${out}"; want_text "$1"; }
want_nopass() { ! grep -q 'PASS' <<<"${out}" || fail "${CASE}: unexpected PASS line: ${out}"; }

run_case "P1 stopped container" "false" 0 "unused"
[[ "${rc}" -eq 1 ]] || fail "P1: rc ${rc}, want 1: ${out}"
want_text "not found or not running"; want_nopass
! grep -q '^exec ' "${STUB}/calls" || fail "P1: the probe reached docker exec on a stopped container"
pass "P1 a stopped container fails before any exec"

run_case "P2 stopped between check and exec" $'true\nfalse' 1 "Error response from daemon: container abc is not running"
want_rcne; want_text "docker exec could not run wardyn-git-helper (exit 1)"; want_nopass
pass "P2 a container that stops before the exec is not a pass"

run_case "P3 docker's own exit code" "true" 126 "OCI runtime exec failed: exec failed: unable to start container process"
want_rcne; want_nopass
pass "P3 docker's own exit code is not a pass"

run_case "P4 unrecognised error text" "true" 0 "wardyn-git-helper: unexpected failure talking to the proxy"
want_rcne; want_text "FAIL"; want_text "Could not confirm credential chain behavior"; want_nopass
pass "P4 unrecognised error text is a failure"

run_case "P5 brokered refusal" "true" 0 "${line_brokered}"
want_rc0; want_pass "Helper refused to mint"
pass "P5 the brokered refusal passes"

run_case "P6 no grant" "true" 0 "${line_nogrant}"
want_rc0; want_pass "No GitHub grant on this run"
pass "P6 no GitHub grant passes"

run_case "P7 approval raised" "true" 0 "wardyn-git-helper: credential approval pending (a1); waiting up to 5m0s ..."
want_rc0; want_pass "Credential approval request raised"
pass "P7 a raised approval passes"

run_case "P8 minted" "true" 0 "password=x"
want_rc0; want_pass "Credential minted"
pass "P8 a minted credential passes"

echo "test-workspace-probes: PASS"
