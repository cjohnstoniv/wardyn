#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# 06-kind-follow-main.sh — keep the Entra kind cluster on the newest main
# commit whose CI passed. Run it on a schedule: it deploys (05-kind-deploy.sh)
# only when that commit is NEWER than the one last deployed, and records the
# deployed commit only after the rollout and /healthz succeed.
#
# "Newest" is main's own first-parent history, never the order CI runs
# finished in: an old commit's run that completes late would otherwise look
# newest and DOWNGRADE the cluster, and an older wardynd cannot read secrets a
# newer one has already re-encrypted. A commit that is an ancestor of the one
# deployed is never deployed.
#
# A failed deploy rolls the release back to the revision it replaced and is
# recorded, so the next run waits for a newer green commit instead of breaking
# the cluster again every tick.
#
# Needs the same TENANT_ID / CLIENT_ID as 05-kind-deploy.sh, and gh signed in.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATE_DIR="${XDG_STATE_HOME:-${HOME}/.local/state}/wardyn-kind-entra"
DEPLOYED_FILE="${STATE_DIR}/deployed"
FAILED_FILE="${STATE_DIR}/failed"
CTX="kind-${WARDYN_QUICKSTART_CLUSTER:-wardyn-entra}"
mkdir -p "${STATE_DIR}"
touch "${FAILED_FILE}"

git -C "${ROOT}" fetch -q origin main
GREEN="$(gh run list --repo cjohnstoniv/wardyn --branch main --event push --workflow CI \
  --status success --limit 100 --json headSha --jq '.[].headSha')"
DEPLOYED="$(cat "${DEPLOYED_FILE}" 2>/dev/null || true)"

SHA=""
while read -r c; do
  [[ -n "${DEPLOYED}" && "${c}" == "${DEPLOYED}" ]] && break
  if grep -qx "${c}" <<<"${GREEN}"; then SHA="${c}"; break; fi
done < <(git -C "${ROOT}" rev-list --first-parent -n 200 origin/main)

if [[ -z "${SHA}" ]]; then
  echo "==> ${DEPLOYED:-nothing} is already deployed; no newer green commit on main"
  exit 0
fi
if [[ -n "${DEPLOYED}" ]] && ! git -C "${ROOT}" merge-base --is-ancestor "${DEPLOYED}" "${SHA}"; then
  echo "==> refusing ${SHA}: it does not descend from the deployed ${DEPLOYED}" >&2
  exit 1
fi
if grep -qx "${SHA}" "${FAILED_FILE}"; then
  echo "==> ${SHA} (newest green main) failed to deploy before; waiting for a newer one"
  exit 0
fi

REV="$(helm --kube-context "${CTX}" -n wardyn history wardyn --max 1 -o json | jq -r '.[0].revision')"
if ! "${ROOT}/05-kind-deploy.sh" "${SHA}"; then
  echo "==> deploy of ${SHA} failed; rolling back to revision ${REV}" >&2
  echo "${SHA}" >>"${FAILED_FILE}"
  helm --kube-context "${CTX}" -n wardyn rollback wardyn "${REV}" --wait --timeout 5m
  exit 1
fi
echo "${SHA}" >"${DEPLOYED_FILE}"
