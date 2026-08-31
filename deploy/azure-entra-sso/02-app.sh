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
HTTP_PORT="${HTTP_PORT:-8480}"
REDIRECT_URI="http://localhost:${HTTP_PORT}/auth/callback"

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
  echo "==> az ad app update --web-redirect-uris ${REDIRECT_URI} (HTTP_PORT may have"
  echo "    changed since this app was created — a stale redirect URI is AADSTS50011)"
  az ad app update --id "${CLIENT_ID}" --web-redirect-uris "${REDIRECT_URI}"
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
    --web-redirect-uris "${REDIRECT_URI}" \
    --app-roles @"${ROLES_JSON}" \
    --query appId -o tsv)"
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
echo "==> wrote CLIENT_ID / CLIENT_SECRET / APP_OBJECT_ID / SP_OBJECT_ID /"
echo "    ADMIN_ROLE_ID / MEMBER_ROLE_ID / HTTP_PORT to ${ENV_FILE}"
