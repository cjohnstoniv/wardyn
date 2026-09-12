#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# teardown.sh — deletes the app registration (and its service principal),
# the 3 users, and the 2 groups this runbook created. Cluster/Secret teardown
# is printed, not run — this script only touches the throwaway Entra tenant,
# never kubectl/kind, so an operator supervising a live run stays in control
# of when the cluster actually goes away.
#
# SAFETY: lists every object it is about to delete before deleting anything,
# and never deletes without an explicit go-ahead — interactively that's a
# y/N prompt, non-interactively (or to skip the prompt) pass --yes. --dry-run
# prints the list and exits 0 without deleting anything, so you can review a
# tenant's actual state before committing.
#
# Usage: teardown.sh [--dry-run] [--yes]
set -euo pipefail

DRY_RUN=false
ASSUME_YES=false
for arg in "$@"; do
  case "${arg}" in
    --dry-run) DRY_RUN=true ;;
    --yes | -y) ASSUME_YES=true ;;
    *)
      echo "usage: $0 [--dry-run] [--yes]" >&2
      exit 1
      ;;
  esac
done

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

# Resolve what's actually there BEFORE printing or deleting anything, so the
# confirmation prompt below describes reality, not just what .env.local
# happens to still remember.
APP_ID="${CLIENT_ID:-}"
if [[ -n "${APP_ID}" ]]; then
  APP_LABEL="app registration ${APP_ID}"
else
  APP_ID="$(az ad app list --display-name wardyn-sso-validation --query "[0].appId" -o tsv 2>/dev/null || true)"
  [[ -n "${APP_ID}" && "${APP_ID}" != "null" ]] || APP_ID=""
  APP_LABEL="app registration wardyn-sso-validation (looked up: ${APP_ID:-not found})"
fi

USERS=()
for upn_var in WARDYN_ADMIN_UPN WARDYN_MEMBER_UPN WARDYN_OUTSIDER_UPN; do
  upn="${!upn_var:-}"
  [[ -n "${upn}" ]] && USERS+=("${upn}")
done
GROUP_NAMES=(wardyn-admins wardyn-eng)

echo "==> tenant ${TENANT_ID} — about to delete:"
if [[ -n "${APP_ID}" ]]; then
  echo "    - ${APP_LABEL} (and its service principal)"
else
  echo "    - ${APP_LABEL} — nothing to delete"
fi
for u in "${USERS[@]:-}"; do [[ -n "${u}" ]] && echo "    - user ${u}"; done
for g in "${GROUP_NAMES[@]}"; do echo "    - group ${g}"; done

if "${DRY_RUN}"; then
  echo "==> --dry-run: nothing deleted"
  exit 0
fi

if ! "${ASSUME_YES}"; then
  REPLY=""
  read -r -p "Delete these objects from tenant ${TENANT_ID}? [y/N] " REPLY || true
  case "${REPLY}" in
    y | Y | yes | YES) ;;
    *)
      echo "aborted — nothing deleted" >&2
      exit 1
      ;;
  esac
fi

if [[ -n "${APP_ID}" ]]; then
  echo "==> deleting ${APP_LABEL}"
  az ad app delete --id "${APP_ID}" || echo "    already gone"
else
  echo "==> no app registration found to delete"
fi

for upn in "${USERS[@]:-}"; do
  [[ -n "${upn}" ]] || continue
  echo "==> deleting user ${upn}"
  az ad user delete --id "${upn}" || echo "    already gone"
done

for grp in "${GROUP_NAMES[@]}"; do
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

  # local secret material this runbook wrote to disk (client secret, user
  # passwords, the rendered OIDC overlay) — not deleted automatically above,
  # since teardown.sh itself still needs .env.local's TENANT_ID/CLIENT_ID/UPNs
  # to run the Entra deletions you just saw:
  rm -f deploy/azure-entra-sso/{.env.local,values-entra.yaml}

  # the tenant itself: this leaves the throwaway directory behind. Delete it
  # in the portal — Azure Portal > Microsoft Entra ID > Manage tenants >
  # (select the throwaway tenant) > Delete. A tenant with no resources in it
  # (this one, once the above ran) is eligible immediately.
EOF
