#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Run components on kind, end to end: one run carries two inline components,
# one delivering a stored secret as a header (the proxy injects it) and one
# delivering a stored secret as a file. Inside the sandbox it asserts that the
# header secret is in no process's environment or command line and in no file
# the agent can read; that the file secret is 0440 root:fsGroup (the agent's
# gid) and readable by the agent; and on the control plane that the attach and
# file-resolve audit rows exist and name no secret value. The docker half
# (`-r--------`, owned by the agent uid) is the real-daemon driver test,
# run here against the Wardyn test daemon.
#
# It creates its OWN cluster, wardyn-c17-<random>, with deploy/kind/quickstart.sh
# under a private KUBECONFIG and per-cluster image tag and ports, and deletes
# the cluster and its image tags on exit. It refuses to run if that name
# exists. Nothing else on the host is touched. WARDYN_KIND_WALK_KEEP=1 leaves
# the cluster up.
#
# GUARD: self-skips (exit 77) unless WARDYN_TEST_K8S=1.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  skip_lane "kind-components-walk: set WARDYN_TEST_K8S=1 to run the cluster-dependent components walk (skipping)."
fi

wardyn_pick_docker_host

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

for bin in docker kind kubectl helm curl jq go sha256sum; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

SUFFIX="$(head -c4 /dev/urandom | od -An -tx1 | tr -d ' \n')"
export WARDYN_QUICKSTART_CLUSTER="wardyn-c17-${SUFFIX}"
export WARDYN_QUICKSTART_IMAGE_TAG="c17-${SUFFIX}"
export WARDYN_QUICKSTART_HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-$((20000 + RANDOM % 5000))}"
export WARDYN_QUICKSTART_SSH_PORT="${WARDYN_QUICKSTART_SSH_PORT:-$((25000 + RANDOM % 5000))}"
CLUSTER="${WARDYN_QUICKSTART_CLUSTER}"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="wardyn"
BASE_URL="http://127.0.0.1:${WARDYN_QUICKSTART_HTTP_PORT}"
TMPDIR="$(mktemp -d /tmp/wardyn-kind-components-walk.XXXXXX)"
export KUBECONFIG="${TMPDIR}/kubeconfig"
# The header secret is split so the task text that carries it into the
# sandbox never holds it whole; only the shell's memory joins the halves.
HDR_A="c17hdr-$(head -c6 /dev/urandom | od -An -tx1 | tr -d ' \n')"
HDR_B="$(head -c6 /dev/urandom | od -An -tx1 | tr -d ' \n')-end"
HDR_VALUE="${HDR_A}${HDR_B}"
FILE_VALUE="c17file-$(head -c12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
FILE_TOKEN="c17-file"
CREATED=0

cleanup() {
  if [[ "${CREATED}" == "1" && "${WARDYN_KIND_WALK_KEEP:-}" != "1" ]]; then
    kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || echo "WARNING: could not delete cluster ${CLUSTER}" >&2
    docker rmi "wardyn/wardynd:${WARDYN_QUICKSTART_IMAGE_TAG}" "wardyn/wardyn-proxy:${WARDYN_QUICKSTART_IMAGE_TAG}" >/dev/null 2>&1 || true
  fi
  rm -rf "${TMPDIR}"
}
trap cleanup EXIT

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  die "cluster ${CLUSTER} already exists and this script did not create it"
fi

step "creating ${CLUSTER} (deploy/kind/quickstart.sh, private kubeconfig)"
CREATED=1
deploy/kind/quickstart.sh >"${TMPDIR}/quickstart.log" 2>&1 || { tail -40 "${TMPDIR}/quickstart.log" >&2; die "quickstart failed"; }

TOKEN="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get secret wardyn-auth -o jsonpath='{.data.admin-token}' | base64 -d)"
[[ -n "${TOKEN}" ]] || die "could not read the admin token"
api() { # METHOD PATH [BODY]
  curl -sS -X "$1" -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
    ${3:+--data-binary "$3"} -w '\n%{http_code}' "${BASE_URL}$2"
}
code_of() { tail -n1 <<<"$1"; }
body_of() { sed '$d' <<<"$1"; }

step "storing the two secrets"
put_secret() { # NAME VALUE
  local out
  out="$(api PUT "/api/v1/secrets/$1" "$(jq -nc --arg v "$2" '{value:$v}')")"
  [[ "$(code_of "${out}")" == "204" ]] || die "PUT /secrets/$1 = $(code_of "${out}") $(body_of "${out}")"
}
put_secret c17-header-secret "${HDR_VALUE}"
put_secret c17-file-secret "${FILE_VALUE}"

F="/run/wardyn/secrets/${FILE_TOKEN}"
# shellcheck disable=SC2016 # expanded inside the sandbox
TASK='a='"${HDR_A}"'; b='"${HDR_B}"'; v="$a$b"; f='"${F}"'
echo "C17 uid=$(id -u)"
echo "C17 gid=$(id -g)"
echo "C17 filestat=$(stat -L -c "%a %u %g" "$f")"
echo "C17 filesha=$(sha256sum < "$f" | cut -c1-64)"
echo "C17 filecontrol=$(printf "%s\n" "$(cat "$f")" | grep -rlF -f - /run 2>/dev/null | grep -c .)"
echo "C17 hdr_environ=$(printf "%s\n" "$v" | grep -lF -f - /proc/[0-9]*/environ 2>/dev/null | grep -c .)"
echo "C17 hdr_cmdline=$(printf "%s\n" "$v" | grep -lF -f - /proc/[0-9]*/cmdline 2>/dev/null | grep -c .)"
echo "C17 hdr_files=$(printf "%s\n" "$v" | grep -rlF -f - /run /tmp /etc /home /workspace "$HOME" 2>/dev/null | grep -c .)"
echo "C17 hdr_curl=$(curl -s -o /dev/null -m 20 -w "%{http_code}" https://example.com/ 2>/dev/null)"
echo C17 done'
BODY="$(jq -nc --arg task "${TASK}" --arg file "${FILE_TOKEN}" '{
  agent: "claude-code", task_mode: "exec", task: $task,
  inline_policy: {allowed_domains: [], denied_domains: [], first_use_approval: "deny_with_review",
    allowed_methods: [], min_confinement_class: "CC1", eligible_grants: [], auto_stop_after_sec: 0},
  components: [
    {name: "c17 header", inline: {hosts: ["example.com"],
      secrets: [{secret_name: "c17-header-secret", delivery: {mode: "header", host: "example.com", header: "X-C17-Token", format: "%s"}}]}},
    {name: "c17 file", inline: {hosts: [],
      secrets: [{secret_name: "c17-file-secret", delivery: {mode: "file", file: $file}}]}}
  ]}')"

step "launching an exec run with a header component and a file component"
out="$(api POST /api/v1/runs "${BODY}")"
[[ "$(code_of "${out}")" == "201" ]] || die "POST /runs = $(code_of "${out}") $(body_of "${out}")"
RUN_ID="$(body_of "${out}" | jq -r .id)"
echo "run ${RUN_ID}"

state=""
for _ in $(seq 1 240); do
  state="$(body_of "$(api GET "/api/v1/runs/${RUN_ID}")" | jq -r .state)"
  case "${state}" in COMPLETED|FAILED|KILLED|STOPPED) break ;; esac
  sleep 2
done
echo "run state: ${state}"
OUTPUT=""
for _ in $(seq 1 30); do
  o="$(body_of "$(api GET "/api/v1/runs/${RUN_ID}/output")")"
  OUTPUT="$(jq -r '.output // empty' <<<"${o}")"
  [[ "$(jq -r '.complete // false' <<<"${o}")" == "true" ]] && break
  sleep 2
done
grep '^C17 ' <<<"${OUTPUT}" | tee "${TMPDIR}/sandbox.txt"
[[ "${state}" == "COMPLETED" ]] || { kubectl --context "${CONTEXT}" -n "${NAMESPACE}" logs deployment/wardyn --tail=80 >&2; die "the run did not complete (${state})"; }
grep -q '^C17 done' "${TMPDIR}/sandbox.txt" || die "the sandbox check did not finish"
val() { sed -n "s/^C17 $1=//p" "${TMPDIR}/sandbox.txt"; }

step "file secret: 0440 root:fsGroup, readable by the agent"
gid="$(val gid)"
[[ "$(val filestat)" == "440 0 ${gid}" ]] || die "file stat '$(val filestat)', want '440 0 ${gid}' (root-owned, the agent's gid as fsGroup)"
[[ "$(val filesha)" == "$(printf '%s' "${FILE_VALUE}" | sha256sum | cut -c1-64)" ]] || die "the agent read a different file content"
[[ "$(val filecontrol)" -ge 1 ]] || die "the scanner did not find the file secret under /run, so its zero for the header secret proves nothing"

step "header secret: in no environment, command line or readable file"
for k in hdr_environ hdr_cmdline hdr_files; do
  [[ "$(val "${k}")" == "0" ]] || die "${k} = '$(val "${k}")', want 0: the header secret is visible inside the sandbox"
done
echo "proxied https://example.com answered $(val hdr_curl) (informational)"
grep -qF "${HDR_VALUE}" <<<"${OUTPUT}" && die "the header secret is in the run output"

step "audit rows"
AUDIT="$(body_of "$(api GET "/api/v1/audit?run_id=${RUN_ID}")")"
actions="$(jq -r '[.. | objects | select(has("action")) | .action] | unique | join(" ")' <<<"${AUDIT}")"
echo "actions: ${actions}"
for a in run.component.attach run.file_secret.resolve run.egress.add; do
  grep -qw "${a}" <<<"${actions}" || die "no ${a} row for the run"
done
grep -qF -e "${HDR_VALUE}" -e "${FILE_VALUE}" <<<"${AUDIT}" && die "an audit row carries a secret value"

step "docker half: the driver delivers the file -r-------- owned by the agent uid"
WARDYN_TEST_DOCKER=1 go test -tags docker -count=1 -v \
  -run 'TestManagedFiles_ASecretIsReadableByTheAgentAlone_RealDocker' ./internal/runner/docker/ >"${TMPDIR}/docker.log" 2>&1
rc=$?
grep -E '^(--- |ok|FAIL|PASS)' "${TMPDIR}/docker.log"
[[ "${rc}" == "0" ]] && grep -q -- '--- PASS: TestManagedFiles_ASecretIsReadableByTheAgentAlone_RealDocker' "${TMPDIR}/docker.log" \
  || { tail -30 "${TMPDIR}/docker.log" >&2; die "docker half failed or skipped (exit ${rc})"; }

echo "kind-components-walk: PASS"
