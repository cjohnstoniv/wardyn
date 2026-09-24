#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# 06-kind-follow-main.sh — keep the Entra kind cluster on the newest main
# commit whose CI passed. Run it on a schedule: it deploys (05-kind-deploy.sh)
# only when that commit differs from the one last deployed, and records the
# deployed commit only after the rollout and /healthz succeed, so a failed
# deploy is retried on the next run instead of being marked done.
#
# Needs the same TENANT_ID / CLIENT_ID as 05-kind-deploy.sh, and gh signed in.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATE="${XDG_STATE_HOME:-${HOME}/.local/state}/wardyn-kind-entra/deployed"
mkdir -p "$(dirname "${STATE}")"

SHA="$(gh run list --repo cjohnstoniv/wardyn --branch main --event push --workflow CI \
  --status success --limit 1 --json headSha --jq '.[0].headSha')"
[[ -n "${SHA}" ]] || { echo "no green CI run on main" >&2; exit 1; }
if [[ "$(cat "${STATE}" 2>/dev/null)" == "${SHA}" ]]; then
  echo "==> ${SHA} (newest green main) is already deployed"
  exit 0
fi

git -C "${ROOT}" fetch -q origin main
"${ROOT}/05-kind-deploy.sh" "${SHA}"
echo "${SHA}" >"${STATE}"
