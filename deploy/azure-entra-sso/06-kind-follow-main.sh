#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# 06-kind-follow-main.sh — keep the Entra kind cluster on the newest main
# commit whose required checks passed. Run it on a schedule: it deploys (05-kind-deploy.sh)
# only when that commit is NEWER than the one the cluster is running.
#
# "Newest" is main's own first-parent history, never the order CI runs
# finished in: an old commit's run that completes late would otherwise look
# newest and DOWNGRADE the cluster, and an older wardynd cannot read secrets a
# newer one has already re-encrypted. What is running is read off the cluster
# (the c-<sha> image tag 05 deploys), not from a file, so a commit deployed by
# hand with 05 is judged too; a commit that is not a descendant of it is
# never deployed.
#
# A deploy that fails after helm changed the release is rolled back to the
# revision it replaced and recorded, so the next run waits for a newer green
# commit instead of breaking the cluster again every tick. A failure before
# helm was touched (a build, the daemon, gh) is neither rolled back nor
# recorded: the next run simply tries again.
#
# Needs the same TENANT_ID / CLIENT_ID as 05-kind-deploy.sh, and gh signed in.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATE_DIR="${XDG_STATE_HOME:-${HOME}/.local/state}/wardyn-kind-entra"
FAILED_FILE="${STATE_DIR}/failed"
CTX="kind-${WARDYN_QUICKSTART_CLUSTER:-wardyn-entra}"
mkdir -p "${STATE_DIR}"
touch "${FAILED_FILE}"

# One run at a time: two overlapping runs would roll back each other's deploy.
exec 9>"${STATE_DIR}/lock"
flock -n 9 || { echo "==> another run is deploying; skipping"; exit 0; }

git -C "${ROOT}" fetch -q origin main
IMAGE="$(kubectl --context "${CTX}" -n wardyn get deploy wardyn \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="wardynd")].image}')"
[[ "${IMAGE##*:}" =~ ^c-([0-9a-f]{12})$ ]] \
  || { echo "==> ${CTX} runs ${IMAGE}, not a commit 05 deployed; run 05-kind-deploy.sh once by hand" >&2; exit 1; }
DEPLOYED="$(git -C "${ROOT}" rev-parse --verify "${BASH_REMATCH[1]}^{commit}")"

# "Green" is the bar a merge uses: every REQUIRED status check succeeded on the
# commit. Judging by the whole run's conclusion let one flaky non-required job
# (ui-e2e) keep the cluster behind a main that branch protection accepted.
REQUIRED="$(gh api repos/cjohnstoniv/wardyn/branches/main/protection/required_status_checks \
  --jq '.contexts[]')"
[[ -n "${REQUIRED}" ]] || { echo "==> could not read main's required checks" >&2; exit 1; }
required_green() {
  local ok ctx
  ok="$(gh api "repos/cjohnstoniv/wardyn/commits/$1/check-runs?per_page=100" \
    --jq '.check_runs[] | select(.conclusion == "success") | .name')" || return 1
  while read -r ctx; do
    grep -qxF "${ctx}" <<<"${ok}" || return 1
  done <<<"${REQUIRED}"
}
SHA=""
while read -r c; do
  [[ "${c}" == "${DEPLOYED}" ]] && break
  if required_green "${c}"; then SHA="${c}"; break; fi
done < <(git -C "${ROOT}" rev-list --first-parent -n 30 origin/main)

if [[ -z "${SHA}" ]]; then
  echo "==> ${DEPLOYED} is already deployed; no newer green commit on main"
  exit 0
fi
if ! git -C "${ROOT}" merge-base --is-ancestor "${DEPLOYED}" "${SHA}"; then
  echo "==> refusing ${SHA}: it does not descend from the deployed ${DEPLOYED}" >&2
  exit 1
fi
if grep -qx "${SHA}" "${FAILED_FILE}"; then
  echo "==> ${SHA} (newest green main) failed to deploy before; waiting for a newer one"
  exit 0
fi

rev() { helm --kube-context "${CTX}" -n wardyn history wardyn --max 1 -o json | jq -r '.[0].revision'; }
REV="$(rev)"
if ! "${ROOT}/05-kind-deploy.sh" "${SHA}"; then
  if [[ "$(rev)" != "${REV}" ]]; then
    echo "==> deploy of ${SHA} failed after changing the release; rolling back to revision ${REV}" >&2
    echo "${SHA}" >>"${FAILED_FILE}"
    helm --kube-context "${CTX}" -n wardyn rollback wardyn "${REV}" --wait --timeout 5m
  else
    echo "==> deploy of ${SHA} failed before changing the release; the next run retries it" >&2
  fi
  exit 1
fi
