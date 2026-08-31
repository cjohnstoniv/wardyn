#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# teardown.sh — deletes the app registration (and its service principal),
# the 3 users, and the 2 groups this runbook created. Cluster/Secret teardown
# is printed, not run — this script only touches the throwaway Entra tenant,
# never kubectl/kind, so an operator supervising a live run stays in control
# of when the cluster actually goes away.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ROOT}/.env.local"

# shellcheck disable=SC1090
[[ -f "${ENV_FILE}" ]] && source "${ENV_FILE}"
[[ -n "${TENANT_ID:-}" ]] || { echo "TENANT_ID not set — nothing recorded in ${ENV_FILE} to tear down" >&2; exit 1; }

command -v az >/dev/null 2>&1 || { echo "az (Azure CLI) not found on PATH" >&2; exit 1; }

[[ "$(az account show --query tenantId -o tsv)" == "${TENANT_ID}" ]] || {
  echo "current az session is not on tenant ${TENANT_ID} — run: az login --tenant ${TENANT_ID} --allow-no-subscriptions" >&2
  exit 1
}

if [[ -n "${CLIENT_ID:-}" ]]; then
  echo "==> deleting app registration ${CLIENT_ID} (wardyn-sso-validation, deletes its SP too)"
  az ad app delete --id "${CLIENT_ID}" || echo "    already gone"
else
  echo "==> no CLIENT_ID recorded — looking up wardyn-sso-validation by display name"
  APP_ID="$(az ad app list --display-name wardyn-sso-validation --query "[0].appId" -o tsv 2>/dev/null || true)"
  [[ -n "${APP_ID}" && "${APP_ID}" != "null" ]] && az ad app delete --id "${APP_ID}" || echo "    none found"
fi

for upn_var in WARDYN_ADMIN_UPN WARDYN_MEMBER_UPN WARDYN_OUTSIDER_UPN; do
  upn="${!upn_var:-}"
  [[ -n "${upn}" ]] || continue
  echo "==> deleting user ${upn}"
  az ad user delete --id "${upn}" || echo "    already gone"
done

for grp in wardyn-admins wardyn-eng; do
  echo "==> deleting group ${grp}"
  gid="$(az ad group show --group "${grp}" --query id -o tsv 2>/dev/null || true)"
  [[ -n "${gid}" ]] && az ad group delete --group "${grp}" || echo "    already gone"
done

cat <<'EOF'

==> Entra objects deleted (app, service principal, 3 users, 2 groups).

Still yours to do, deliberately not scripted here:

  # kind cluster (matches whatever WARDYN_QUICKSTART_CLUSTER you brought it up with):
  WARDYN_QUICKSTART_CLUSTER=wardyn-entra deploy/kind/quickstart.sh --down

  # the OIDC client-secret Secret, if the cluster above is staying up longer:
  kubectl --context kind-wardyn-entra -n wardyn delete secret wardyn-entra-oidc

  # the tenant itself: this leaves the throwaway directory behind. Delete it
  # in the portal — Azure Portal > Microsoft Entra ID > Manage tenants >
  # (select the throwaway tenant) > Delete. A tenant with no resources in it
  # (this one, once the above ran) is eligible immediately.
EOF
