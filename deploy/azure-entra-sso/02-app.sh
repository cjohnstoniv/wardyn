#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# 02-app.sh — register the wardyn-sso-validation app: two App Roles
# (Wardyn.Admin / Wardyn.Member — free on Entra ID Free, per-USER assignment
# only; a GROUP-to-App-Role assignment needs P1 and is deliberately not
# scripted here), the groups claim (free: groupMembershipClaims=SecurityGroup),
# the optional email ID-token claim, a service principal, "assignment
# required" turned on, and a client secret. Nothing secret is ever printed —
# the client secret goes straight into .env.local (chmod 600).
#
# The Azure DevOps permissions follow ADO_TOKEN_MODE (docs/AZURE-DEVOPS.md, "The
# app registration"): minted_pat (the default) adds vso.pats and vso.pats_manage,
# which a per-run-token row needs; bearer adds the capability scopes an Entra
# sign-in row needs and never the two token permissions (a bearer row refuses an
# app that holds them). Both need this same app to be a confidential client on
# the Web platform, which the rest of this script already sets up.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ROOT}/.env.local"

# shellcheck disable=SC1090
[[ -f "${ENV_FILE}" ]] && source "${ENV_FILE}"
[[ -n "${TENANT_ID:-}" ]] || { echo "TENANT_ID not set — run 01-tenant-prep.sh first" >&2; exit 1; }

# shellcheck disable=SC1091
source "${ROOT}/lib.sh"

command -v az >/dev/null 2>&1 || { echo "az (Azure CLI) not found on PATH" >&2; exit 1; }
command -v uuidgen >/dev/null 2>&1 || { echo "uuidgen not found on PATH" >&2; exit 1; }

DISPLAY_NAME="wardyn-sso-validation"
ADO_TOKEN_MODE_GIVEN="${ADO_TOKEN_MODE:+1}"
ADO_TOKEN_MODE="${ADO_TOKEN_MODE:-minted_pat}"
case "${ADO_TOKEN_MODE}" in
  minted_pat | bearer) ;;
  *) echo "ADO_TOKEN_MODE must be minted_pat or bearer (got '${ADO_TOKEN_MODE}')" >&2; exit 1 ;;
esac
HTTP_PORT="${HTTP_PORT:-8480}"
REDIRECT_URI="http://localhost:${HTTP_PORT}/auth/callback"
# The per-user Azure DevOps sign-in (docs/adoption/azure-devops-entra.md)
# redirects to its own callback on the same app registration.
ADO_REDIRECT_URI="http://localhost:${HTTP_PORT}/api/v1/scm/azure-devops/callback"

[[ "$(az account show --query tenantId -o tsv)" == "${TENANT_ID}" ]] || {
  echo "current az session is not on tenant ${TENANT_ID} — run: az login --tenant ${TENANT_ID} --allow-no-subscriptions" >&2
  exit 1
}

EXISTING_APP_ID="$(az ad app list --display-name "${DISPLAY_NAME}" --query "[0].appId" -o tsv 2>/dev/null || true)"
APP_REUSED=0
if [[ -n "${EXISTING_APP_ID}" && "${EXISTING_APP_ID}" != "null" ]]; then
  echo "==> app '${DISPLAY_NAME}' already exists (${EXISTING_APP_ID}) — reusing it"
  CLIENT_ID="${EXISTING_APP_ID}"
  APP_REUSED=1
  echo "==> reading back the existing App Role GUIDs — a re-run must not mint fresh"
  echo "    uuidgen'd ones the reused app doesn't have, or 03-people.sh's"
  echo "    appRoleAssignedTo POST 400s against a role id that doesn't exist"
  ADMIN_ROLE_ID="$(az ad app show --id "${CLIENT_ID}" --query "appRoles[?value=='Wardyn.Admin']|[0].id" -o tsv)"
  MEMBER_ROLE_ID="$(az ad app show --id "${CLIENT_ID}" --query "appRoles[?value=='Wardyn.Member']|[0].id" -o tsv)"
  [[ -n "${ADMIN_ROLE_ID}" && "${ADMIN_ROLE_ID}" != "None" ]] || { echo "app '${DISPLAY_NAME}' (${CLIENT_ID}) has no Wardyn.Admin App Role — delete it in the portal and re-run, or add the role by hand" >&2; exit 1; }
  [[ -n "${MEMBER_ROLE_ID}" && "${MEMBER_ROLE_ID}" != "None" ]] || { echo "app '${DISPLAY_NAME}' (${CLIENT_ID}) has no Wardyn.Member App Role — delete it in the portal and re-run, or add the role by hand" >&2; exit 1; }
  echo "==> az ad app update --web-redirect-uris ${REDIRECT_URI} ${ADO_REDIRECT_URI} (HTTP_PORT may have"
  echo "    changed since this app was created — a stale redirect URI is AADSTS50011)"
  az ad app update --id "${CLIENT_ID}" --web-redirect-uris "${REDIRECT_URI}" "${ADO_REDIRECT_URI}"
else
  ADMIN_ROLE_ID="$(uuidgen)"
  MEMBER_ROLE_ID="$(uuidgen)"
  ROLES_JSON="$(mktemp)"
  trap 'rm -f "${ROLES_JSON}"' EXIT
  cat >"${ROLES_JSON}" <<EOF
[
  {
    "allowedMemberTypes": ["User"],
    "description": "Full Wardyn control-plane access",
    "displayName": "Wardyn Admin",
    "id": "${ADMIN_ROLE_ID}",
    "isEnabled": true,
    "value": "Wardyn.Admin"
  },
  {
    "allowedMemberTypes": ["User"],
    "description": "Launch and use Wardyn runs",
    "displayName": "Wardyn Member",
    "id": "${MEMBER_ROLE_ID}",
    "isEnabled": true,
    "value": "Wardyn.Member"
  }
]
EOF
  echo "==> az ad app create --display-name ${DISPLAY_NAME}"
  CLIENT_ID="$(az ad app create --display-name "${DISPLAY_NAME}" \
    --web-redirect-uris "${REDIRECT_URI}" "${ADO_REDIRECT_URI}" \
    --app-roles @"${ROLES_JSON}" \
    --query appId -o tsv)"
fi

if [[ "${APP_REUSED}" == "1" && -z "${ADO_TOKEN_MODE_GIVEN}" ]]; then
  echo "ADO_TOKEN_MODE is not recorded for the existing app '${DISPLAY_NAME}'. An app set up before 0.8.2 serves an" >&2
  echo "Entra sign-in (bearer) row, which refuses a token that can create tokens. Re-run with ADO_TOKEN_MODE=bearer to" >&2
  echo "keep that row working, or ADO_TOKEN_MODE=minted_pat once you switch the row to token_mode minted_pat." >&2
  exit 1
fi

APP_OBJECT_ID="$(az ad app show --id "${CLIENT_ID}" --query id -o tsv)"

echo "==> groupMembershipClaims=SecurityGroup + optional email ID-token claim"
# Whole-object PATCH on optionalClaims (Graph does not merge partial arrays
# inside it) — accessToken/saml2Token stay empty, we only add idToken.email.
az rest --method PATCH \
  --url "https://graph.microsoft.com/v1.0/applications/${APP_OBJECT_ID}" \
  --headers "Content-Type=application/json" \
  --body '{
    "groupMembershipClaims": "SecurityGroup",
    "optionalClaims": {
      "idToken": [{"name": "email", "essential": false}],
      "accessToken": [],
      "saml2Token": []
    }
  }'

# Azure DevOps delegated permissions for an `entra` provider row (see the header
# for ADO_TOKEN_MODE). The Azure DevOps service principal exists in a tenant only
# once an Azure DevOps organisation is connected to it; without one this step
# is skipped and the Azure DevOps lane cannot be tested on this tenant.
ADO_API="499b84ac-1321-427f-aa17-267ca6975798"
ADO_TOKEN_SCOPES=(vso.pats vso.pats_manage)
if [[ "${ADO_TOKEN_MODE}" == "minted_pat" ]]; then
  # A per-run-token row chooses its own PAT scopes; the app needs only these two.
  ADO_SCOPES=("${ADO_TOKEN_SCOPES[@]}")
else
  # An Entra sign-in row whose ceiling is every per-area read + code_write + pr.
  ADO_SCOPES=(vso.analytics vso.build vso.code vso.graph vso.identity vso.memberentitlementmanagement
    vso.packaging vso.profile vso.project vso.release vso.securefiles_read vso.serviceendpoint vso.test
    vso.variablegroups_read vso.wiki vso.work vso.code_write)
fi
if az ad sp show --id "${ADO_API}" >/dev/null 2>&1; then
  # `az ad app permission add` appends without de-duplicating, so a re-run on
  # a reused app adds only the scopes the app does not already hold.
  HELD="$(az ad app show --id "${CLIENT_ID}" \
    --query "requiredResourceAccess[?resourceAppId=='${ADO_API}'].resourceAccess[].id" -o tsv)"
  ADO_PERMS=()
  for s in "${ADO_SCOPES[@]}"; do
    id="$(az ad sp show --id "${ADO_API}" --query "oauth2PermissionScopes[?value=='${s}'].id | [0]" -o tsv)"
    [[ -n "${id}" && "${id}" != "None" ]] || { echo "Azure DevOps publishes no delegated scope ${s}" >&2; exit 1; }
    grep -qx "${id}" <<<"${HELD}" || ADO_PERMS+=("${id}=Scope")
  done
  if ((${#ADO_PERMS[@]})); then
    echo "==> az ad app permission add (Azure DevOps, ${ADO_TOKEN_MODE}: ${#ADO_PERMS[@]} of ${#ADO_SCOPES[@]} scopes not yet held)"
    az ad app permission add --id "${CLIENT_ID}" --api "${ADO_API}" --api-permissions "${ADO_PERMS[@]}"
  else
    echo "==> the app already holds every Azure DevOps scope ${ADO_TOKEN_MODE} needs"
  fi
  if [[ "${ADO_TOKEN_MODE}" == "minted_pat" ]]; then
    echo "    Grant admin consent — required, so each person's connection is captured silently at sign-in:"
  else
    echo "    Grant admin consent once (or let each person consent at their first Azure DevOps sign-in):"
  fi
  echo "    az ad app permission admin-consent --id ${CLIENT_ID}"
  if [[ "${ADO_TOKEN_MODE}" == "bearer" ]]; then
    for s in "${ADO_TOKEN_SCOPES[@]}"; do
      id="$(az ad sp show --id "${ADO_API}" --query "oauth2PermissionScopes[?value=='${s}'].id | [0]" -o tsv)"
      if [[ -n "${id}" && "${id}" != "None" ]] && grep -qx "${id}" <<<"${HELD}"; then
        echo "    WARNING: the app holds ${s}; a bearer row refuses to inject a token that can create tokens." >&2
        echo "    Remove it (az ad app permission delete --id ${CLIENT_ID} --api ${ADO_API} --api-permissions ${id}) or use ADO_TOKEN_MODE=minted_pat." >&2
      fi
    done
  fi
else
  echo "==> no Azure DevOps service principal in this tenant — connect an Azure DevOps organisation"
  echo "    to it (Organization settings -> Microsoft Entra) and re-run to add the Azure DevOps permissions"
fi

if az ad sp show --id "${CLIENT_ID}" >/dev/null 2>&1; then
  echo "==> service principal already exists — reusing it"
else
  echo "==> az ad sp create --id ${CLIENT_ID}  (app create alone makes no SP)"
  az ad sp create --id "${CLIENT_ID}" >/dev/null
fi
SP_OBJECT_ID="$(az ad sp show --id "${CLIENT_ID}" --query id -o tsv)"

echo "==> appRoleAssignmentRequired=true on the Enterprise Application"
echo "    (until 03-people.sh + the walk's third-user demo toggles it off,"
echo "    an unassigned user hits Entra's AADSTS50105 before Wardyn ever sees them)"
az rest --method PATCH \
  --url "https://graph.microsoft.com/v1.0/servicePrincipals/${SP_OBJECT_ID}" \
  --headers "Content-Type=application/json" \
  --body '{"appRoleAssignmentRequired": true}'

if [[ "${APP_REUSED}" == "1" && -n "${CLIENT_SECRET:-}" ]]; then
  echo "==> app reused and CLIENT_SECRET already recorded in ${ENV_FILE} — skipping credential reset"
else
  echo "==> resetting the client secret (not printed — written straight to .env.local)"
  CLIENT_SECRET="$(az ad app credential reset --id "${CLIENT_ID}" --append \
    --query password -o tsv)"
fi

set_var CLIENT_ID "${CLIENT_ID}"
set_var CLIENT_SECRET "${CLIENT_SECRET}"
set_var APP_OBJECT_ID "${APP_OBJECT_ID}"
set_var SP_OBJECT_ID "${SP_OBJECT_ID}"
set_var ADMIN_ROLE_ID "${ADMIN_ROLE_ID}"
set_var MEMBER_ROLE_ID "${MEMBER_ROLE_ID}"
set_var HTTP_PORT "${HTTP_PORT}"
set_var ADO_TOKEN_MODE "${ADO_TOKEN_MODE}"
echo "==> wrote CLIENT_ID / CLIENT_SECRET / APP_OBJECT_ID / SP_OBJECT_ID /"
echo "    ADMIN_ROLE_ID / MEMBER_ROLE_ID / HTTP_PORT / ADO_TOKEN_MODE to ${ENV_FILE}"
