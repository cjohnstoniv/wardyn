#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# 01-tenant-prep.sh — sign into the throwaway Entra tenant and disable
# security defaults, which otherwise block the interactive flow used by every
# later step and force per-user MFA before you've even created the users. See
# README.md's Prelude for the explicit throwaway-tenant trade-off this is.
#
# Usage: 01-tenant-prep.sh <tenant-id>
#
# Order matters: this FIRST `az login` happens BEFORE security defaults are
# disabled, so it must be the normal interactive browser flow, never
# `--use-device-code` — device-code sign-in is exactly what security defaults
# block on a tenant created 2026-07 or later.
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

echo "==> disabling security defaults (identitySecurityDefaultsEnforcementPolicy)"
echo "    Trade-off, spelled out: security defaults force MFA on every user and"
echo "    block legacy/device-code auth tenant-wide. Turning them off is what makes"
echo "    'az login --use-device-code' and unattended user creation possible in"
echo "    the steps that follow. This is acceptable ONLY because this tenant is"
echo "    throwaway (README.md's Prelude + teardown.sh delete it when you're done)"
echo "    — never do this on a tenant with real users in it."
az rest --method PATCH \
  --url "https://graph.microsoft.com/v1.0/policies/identitySecurityDefaultsEnforcementPolicy" \
  --headers "Content-Type=application/json" \
  --body '{"isEnabled": false}'

echo "==> verifying"
STATE="$(az rest --method GET \
  --url "https://graph.microsoft.com/v1.0/policies/identitySecurityDefaultsEnforcementPolicy" \
  --query isEnabled -o tsv)"
[[ "${STATE}" == "false" ]] || {
  echo "security defaults still report isEnabled=${STATE} — PATCH did not take. Re-run this script." >&2
  exit 1
}
echo "    security defaults: disabled"

set_var TENANT_ID "${TENANT_ID}"
echo "==> wrote TENANT_ID to ${ENV_FILE}"
