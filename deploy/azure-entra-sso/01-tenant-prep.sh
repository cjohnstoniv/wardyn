#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# 01-tenant-prep.sh — sign into the throwaway Entra tenant and VERIFY security
# defaults are off (they otherwise block the interactive flow used by every
# later step and force per-user MFA before you've even created the users). See
# README.md's Prelude for the explicit throwaway-tenant trade-off this is, and
# for the portal step this script only checks, never performs: Microsoft Graph
# refuses a PATCH to identitySecurityDefaultsEnforcementPolicy on current
# tenants (AADSTS65002) — disabling it is portal-only now.
#
# Usage: 01-tenant-prep.sh <tenant-id>
#
# Order matters: this FIRST `az login` happens BEFORE you disable security
# defaults in the portal, so it must be the normal interactive browser flow,
# never `--use-device-code` — device-code sign-in is exactly what security
# defaults block on a tenant created 2026-07 or later.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ROOT}/.env.local"

# shellcheck disable=SC1090
[[ -f "${ENV_FILE}" ]] && source "${ENV_FILE}"

# shellcheck disable=SC1091
source "${ROOT}/lib.sh"

TENANT_ID="${1:-${TENANT_ID:-}}"
[[ -n "${TENANT_ID}" ]] || {
  echo "usage: $0 <tenant-id>   (the GUID from Entra ID > Overview)" >&2
  exit 1
}

command -v az >/dev/null 2>&1 || { echo "az (Azure CLI) not found on PATH" >&2; exit 1; }

echo "==> az login --tenant ${TENANT_ID} (interactive browser — a subscription-less"
echo "    tenant is fine with --allow-no-subscriptions; do NOT add --use-device-code"
echo "    here, security defaults still block it at this point)"
az login --tenant "${TENANT_ID}" --allow-no-subscriptions >/dev/null

echo "==> checking security defaults (identitySecurityDefaultsEnforcementPolicy)"
echo "    Microsoft Graph now refuses a PATCH to this policy (AADSTS65002) —"
echo "    disabling it is PORTAL-ONLY. Trade-off, spelled out: security defaults"
echo "    force MFA on every user and block legacy/device-code auth tenant-wide;"
echo "    turning them off is what makes 'az login --use-device-code' and"
echo "    unattended user creation possible in the steps that follow. YOU turn"
echo "    them off, in the portal, and only because this tenant is throwaway"
echo "    (README.md's Prelude; teardown.sh prints the portal tenant-delete step"
echo "    when you're done) — never do this on a tenant with real users in it."
STATE="$(az rest --method GET \
  --url "https://graph.microsoft.com/v1.0/policies/identitySecurityDefaultsEnforcementPolicy" \
  --query isEnabled -o tsv)"
if [[ "${STATE}" != "false" ]]; then
  echo "security defaults report isEnabled=${STATE}. Disable them yourself, then re-run this script:" >&2
  echo "  Azure Portal > Microsoft Entra ID > Properties >" >&2
  echo "  Manage Security defaults (link at the bottom of the page) >" >&2
  echo "  Security defaults: Disabled > give any justification > Save." >&2
  exit 1
fi
echo "    security defaults: disabled (confirmed via Graph GET)"

set_var TENANT_ID "${TENANT_ID}"
echo "==> wrote TENANT_ID to ${ENV_FILE}"
