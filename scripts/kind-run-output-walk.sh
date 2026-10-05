#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Run-output walk on kind: a headless exec run prints a registered secret and a
# marker, completes, wardynd restarts, and `wardyn run output` returns the same
# bytes it did before the restart. The registered secret is absent from
# run_outputs.output read with psql, and the marker is present (so the row is
# not simply empty).
#
# It creates its OWN cluster (default name wardyn-out-o4, ports 8291/2291) with
# deploy/kind/quickstart.sh and deletes it on exit; it refuses a cluster that
# already exists, because it would otherwise have to delete one it did not
# create. WARDYN_KIND_WALK_KEEP=1 leaves its own cluster up.
#
# GUARD: self-skips (exit 77) unless WARDYN_TEST_K8S=1.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  skip_lane "kind-run-output-walk: set WARDYN_TEST_K8S=1 to run the cluster-dependent run-output walk (skipping)."
fi

wardyn_pick_docker_host

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

for bin in docker kind kubectl helm curl jq go cmp; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

export WARDYN_QUICKSTART_CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-out-o4}"
export WARDYN_QUICKSTART_HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8291}"
export WARDYN_QUICKSTART_SSH_PORT="${WARDYN_QUICKSTART_SSH_PORT:-2291}"
CLUSTER="${WARDYN_QUICKSTART_CLUSTER}"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="wardyn"
BASE_URL="http://127.0.0.1:${WARDYN_QUICKSTART_HTTP_PORT}"
SECRET_NAME="walk-secret"
SECRET_VALUE="walk-secret-value-$(date +%s)-$$"
MARKER="out-o4-marker-$$"
TMPDIR="$(mktemp -d /tmp/wardyn-kind-run-output-walk.XXXXXX)"
CREATED=0

cleanup() {
  rm -rf "${TMPDIR}"
  if [[ "${CREATED}" == "1" && "${WARDYN_KIND_WALK_KEEP:-}" != "1" ]]; then
    kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || echo "WARNING: could not delete cluster ${CLUSTER}" >&2
  fi
}
trap cleanup EXIT

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  die "cluster ${CLUSTER} already exists and this script did not create it; pick another WARDYN_QUICKSTART_CLUSTER"
fi

step "creating ${CLUSTER} (deploy/kind/quickstart.sh)"
CREATED=1
deploy/kind/quickstart.sh >"${TMPDIR}/quickstart.log" 2>&1 || { tail -30 "${TMPDIR}/quickstart.log" >&2; die "quickstart failed"; }

TOKEN="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get secret wardyn-auth -o jsonpath='{.data.admin-token}' | base64 -d)"
[[ -n "${TOKEN}" ]] || die "could not read the admin token"

step "building the wardyn CLI"
go build -o "${TMPDIR}/wardyn" ./cmd/wardyn || die "go build failed"
w() { WARDYN_URL="${BASE_URL}" WARDYN_ADMIN_TOKEN="${TOKEN}" "${TMPDIR}/wardyn" "$@"; }

step "storing ${SECRET_NAME} and launching an exec run that prints it"
printf '%s' "${SECRET_VALUE}" | w secret set "${SECRET_NAME}" || die "secret set failed"
cat >"${TMPDIR}/policy.json" <<EOF
{
  "allowed_domains": [],
  "denied_domains": [],
  "first_use_approval": "deny_with_review",
  "allowed_methods": [],
  "min_confinement_class": "CC1",
  "eligible_grants": [
    {"kind": "env_secret", "scope": {"name": "WALK_SECRET", "secret_name": "${SECRET_NAME}"}, "ttl_seconds": 3600}
  ],
  "auto_stop_after_sec": 0
}
EOF
w run --agent claude-code --task-mode exec --policy-file "${TMPDIR}/policy.json" --wait --timeout 8m \
  --task "echo ${MARKER}; echo \"\$WALK_SECRET\"" --json >"${TMPDIR}/run.json" 2>"${TMPDIR}/run.err"
rc=$?
RUN_ID="$(jq -r '.id // empty' "${TMPDIR}/run.json" 2>/dev/null)"
[[ -n "${RUN_ID}" ]] || { cat "${TMPDIR}/run.err" >&2; die "no run id (run exit ${rc})"; }
[[ "${rc}" -eq 0 ]] || { cat "${TMPDIR}/run.err" >&2; die "the run did not complete (exit ${rc}, run ${RUN_ID})"; }
echo "run ${RUN_ID} completed"

read_output() { # FILE: a final capture, retried while the row is still being written
  local i
  for i in $(seq 1 30); do
    w run output "${RUN_ID}" >"$1" 2>"$1.err" && [[ "$(w run output "${RUN_ID}" --json | jq -r .complete)" == "true" ]] && return 0
    sleep 2
  done
  cat "$1.err" >&2
  return 1
}
read_output "${TMPDIR}/before.txt" || die "no final output before the restart"
grep -q "${MARKER}" "${TMPDIR}/before.txt" || die "the marker is not in the output"
echo "before restart: $(wc -c <"${TMPDIR}/before.txt") bytes"

step "restarting wardynd"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout restart deployment/wardyn >/dev/null || die "rollout restart failed"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status deployment/wardyn --timeout=240s || die "wardynd did not come back"
for _ in $(seq 1 60); do curl -sf "${BASE_URL}/healthz" >/dev/null 2>&1 && break; sleep 2; done

step "reading the output again"
w run output "${RUN_ID}" >"${TMPDIR}/after.txt" 2>"${TMPDIR}/after.err" || { cat "${TMPDIR}/after.err" >&2; die "wardyn run output failed after the restart"; }
cmp "${TMPDIR}/before.txt" "${TMPDIR}/after.txt" || die "the output changed across the restart"
echo "ok: same $(wc -c <"${TMPDIR}/after.txt") bytes after the restart"

step "psql: run_outputs.output for the run"
PG_POD="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pod -l app=postgres -o jsonpath='{.items[0].metadata.name}')"
[[ -n "${PG_POD}" ]] || die "no postgres pod"
psql_q() { kubectl --context "${CONTEXT}" -n "${NAMESPACE}" exec "${PG_POD}" -- psql -U wardyn -d wardyn -tA -c "$1"; }
psql_q "select run_id, source, incomplete, capture_gap, mask_scope, length(output) as bytes, captured_at is not null as final from run_outputs where run_id = '${RUN_ID}'" | tee "${TMPDIR}/row.txt"
[[ -s "${TMPDIR}/row.txt" ]] || die "no run_outputs row for the run"
leaked="$(psql_q "select count(*) from run_outputs where run_id = '${RUN_ID}' and position(convert_to('${SECRET_VALUE}', 'UTF8') in output) > 0")"
marked="$(psql_q "select count(*) from run_outputs where run_id = '${RUN_ID}' and position(convert_to('${MARKER}', 'UTF8') in output) > 0")"
echo "rows holding the secret: ${leaked}; rows holding the marker: ${marked}"
[[ "${marked}" == "1" ]] || die "the stored row does not hold the marker, so the secret check proves nothing"
[[ "${leaked}" == "0" ]] || die "the registered secret is in run_outputs.output"
grep -q "${SECRET_VALUE}" "${TMPDIR}/after.txt" && die "the registered secret is in the CLI output"

echo "kind-run-output-walk: PASS"
