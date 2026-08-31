#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# 03-people.sh — three cloud-only users (wardyn-admin, wardyn-member,
# wardyn-outsider) and two security groups (wardyn-admins, wardyn-eng) on the
# tenant's default domain, for the walk's three sign-in paths. Passwords are
# openssl-random and never printed — they land in .env.local (chmod 600)
# alongside everything the walk needs to reproduce each sign-in.
#
# admin  -> group wardyn-admins, App Role Wardyn.Admin (admin via the role map)
# member -> group wardyn-eng,    App Role Wardyn.Member (passes the
#           "assignment required" gate only — member's WARDYN ROLE comes from
#           the groups claim + the console People-step mapping, not the role
#           map; see README.md's walk)
# outsider -> nothing: no group, no app role. Stands for the deny demo.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ROOT}/.env.local"

# shellcheck disable=SC1090
[[ -f "${ENV_FILE}" ]] && source "${ENV_FILE}"
for v in TENANT_ID SP_OBJECT_ID ADMIN_ROLE_ID MEMBER_ROLE_ID; do
  [[ -n "${!v:-}" ]] || { echo "${v} not set — run 01-tenant-prep.sh and 02-app.sh first" >&2; exit 1; }
done

# shellcheck disable=SC1091
source "${ROOT}/lib.sh"

command -v az >/dev/null 2>&1 || { echo "az (Azure CLI) not found on PATH" >&2; exit 1; }

[[ "$(az account show --query tenantId -o tsv)" == "${TENANT_ID}" ]] || {
  echo "current az session is not on tenant ${TENANT_ID} — run: az login --tenant ${TENANT_ID} --allow-no-subscriptions" >&2
  exit 1
}

DOMAIN="$(az rest --method GET --url "https://graph.microsoft.com/v1.0/domains" \
  --query "value[?isDefault]|[0].id" -o tsv)"
[[ -n "${DOMAIN}" ]] || { echo "could not read the tenant's default domain" >&2; exit 1; }
echo "==> tenant default domain: ${DOMAIN}"

# create_user NICK DISPLAY -> writes ${NICK^^}_UPN / _OID / _PASSWORD to .env.local
create_user() {
  local nick="$1" display="$2"
  local upn="${nick}@${DOMAIN}"
  local var_prefix
  var_prefix="$(echo "${nick}" | tr 'a-z-' 'A-Z_')"
  local existing_oid
  existing_oid="$(az ad user show --id "${upn}" --query id -o tsv 2>/dev/null || true)"
  if [[ -n "${existing_oid}" ]]; then
    local pw_var="${var_prefix}_PASSWORD"
    if [[ -z "${!pw_var:-}" ]]; then
      echo "==> user ${upn} already exists (${existing_oid}) — no ${pw_var} recorded"
      echo "    (.env.local lost since it was created?) — minting a fresh password"
      local pw
      pw="$(openssl rand -base64 24)"
      az ad user update --id "${upn}" --password "${pw}" \
        --force-change-password-next-sign-in false
      set_var "${pw_var}" "${pw}"
    else
      echo "==> user ${upn} already exists (${existing_oid}) — reusing it, password unchanged"
    fi
  else
    local pw
    pw="$(openssl rand -base64 24)"
    echo "==> az ad user create ${upn}"
    existing_oid="$(az ad user create --display-name "${display}" \
      --user-principal-name "${upn}" \
      --mail-nickname "${nick}" \
      --password "${pw}" \
      --force-change-password-next-sign-in false \
      --query id -o tsv)"
    set_var "${var_prefix}_PASSWORD" "${pw}"
    # Best-effort: cloud-only users have no `mail` unless it's set explicitly
    # AND the tenant lets it be writable (mail is best-effort writable, not
    # guaranteed) — this is what the walk's ID-token email-claim assertion
    # step checks, with the admin-token-recovery fallback named there if it's
    # absent.
    az rest --method PATCH \
      --url "https://graph.microsoft.com/v1.0/users/${existing_oid}" \
      --headers "Content-Type=application/json" \
      --body "{\"mail\": \"${upn}\"}" \
      || echo "    WARN: could not set the mail attribute on ${upn} (best-effort, not fatal — see README's email-claim step)"
  fi
  set_var "${var_prefix}_UPN" "${upn}"
  set_var "${var_prefix}_OID" "${existing_oid}"
}

create_user wardyn-admin    "Wardyn Admin"
create_user wardyn-member   "Wardyn Member"
create_user wardyn-outsider "Wardyn Outsider"

# shellcheck disable=SC1090
source "${ENV_FILE}"

# create_group NICK -> echoes its object id
create_group() {
  local nick="$1"
  local oid
  oid="$(az ad group show --group "${nick}" --query id -o tsv 2>/dev/null || true)"
  if [[ -n "${oid}" ]]; then
    echo "==> group ${nick} already exists (${oid}) — reusing it" >&2
  else
    echo "==> az ad group create ${nick}" >&2
    oid="$(az ad group create --display-name "${nick}" --mail-nickname "${nick}" --query id -o tsv)"
  fi
  echo "${oid}"
}

ADMIN_GROUP_OID="$(create_group wardyn-admins)"
ENG_GROUP_OID="$(create_group wardyn-eng)"

group_add() { # group_add GROUP_OID MEMBER_OID
  az ad group member check --group "$1" --member-id "$2" --query value -o tsv 2>/dev/null | grep -qx true \
    && echo "    already a member — skipping" \
    || az ad group member add --group "$1" --member-id "$2"
}
echo "==> wardyn-admins += ${WARDYN_ADMIN_UPN}"
group_add "${ADMIN_GROUP_OID}" "${WARDYN_ADMIN_OID}"
echo "==> wardyn-eng += ${WARDYN_MEMBER_UPN}"
group_add "${ENG_GROUP_OID}" "${WARDYN_MEMBER_OID}"

# assign_role USER_OID ROLE_ID -> POST appRoleAssignedTo, idempotent
assign_role() {
  local user_oid="$1" role_id="$2"
  local already
  already="$(az rest --method GET \
    --url "https://graph.microsoft.com/v1.0/servicePrincipals/${SP_OBJECT_ID}/appRoleAssignedTo" \
    --query "value[?principalId=='${user_oid}' && appRoleId=='${role_id}']|[0].id" -o tsv 2>/dev/null || true)"
  if [[ -n "${already}" && "${already}" != "None" ]]; then
    echo "    already assigned — skipping"
    return
  fi
  az rest --method POST \
    --url "https://graph.microsoft.com/v1.0/servicePrincipals/${SP_OBJECT_ID}/appRoleAssignedTo" \
    --headers "Content-Type=application/json" \
    --body "{\"principalId\": \"${user_oid}\", \"resourceId\": \"${SP_OBJECT_ID}\", \"appRoleId\": \"${role_id}\"}" \
    >/dev/null
}
echo "==> assigning App Role Wardyn.Admin to ${WARDYN_ADMIN_UPN}"
assign_role "${WARDYN_ADMIN_OID}" "${ADMIN_ROLE_ID}"
echo "==> assigning App Role Wardyn.Member to ${WARDYN_MEMBER_UPN}"
echo "    (passes 'assignment required' only — this user's WARDYN role comes"
echo "    from the eng group + the console People step, not this role map entry)"
assign_role "${WARDYN_MEMBER_OID}" "${MEMBER_ROLE_ID}"
echo "==> ${WARDYN_OUTSIDER_UPN} gets no group and no App Role assignment (deliberate)"

set_var ADMIN_GROUP_OID "${ADMIN_GROUP_OID}"
set_var ENG_GROUP_OID "${ENG_GROUP_OID}"
echo "==> wrote ADMIN_GROUP_OID / ENG_GROUP_OID and the three users' UPN/OID/PASSWORD to ${ENV_FILE}"
