#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# License gate for SHIPPED (prod) UI dependencies — an ALLOWLIST, not a denylist.
# The old denylist (A?GPL|LGPL|MPL|EPL) failed OPEN on everything it did not name:
# SSPL, BUSL, CC-BY-NC and any "unknown" license — the source-available terms legal
# fears most — sailed straight through. Instead, every prod license must be one of
# the known-permissive terms below; anything else fails the build. A dual-licensed
# expression passes only when EVERY token in it is allowlisted (an "X OR AGPL" is
# rejected — we don't assume the OR resolves in our favor without review).
# Extracted from ci.yml so the gate is single-sourced (Group F).
set -euo pipefail

cd "$(dirname "$0")/../ui"

# Permissive, distribution-safe SPDX ids. BSD-* covers the versioned BSD family
# (BSD-2-Clause/BSD-3-Clause/...). Extend deliberately, with review.
allow='0BSD|Apache-2\.0|BSD|BSD-[0-9A-Za-z.-]+|ISC|MIT|MIT-0|OFL-1\.1|CC0-1\.0|Unlicense|Python-2\.0|BlueOak-1\.0\.0'

pnpm licenses list --prod --json > /tmp/ui-prod-licenses.json
# For each license key, strip out every allowlisted token plus the SPDX glue
# (parentheses, AND/OR/WITH, whitespace). Whatever remains is a term we do NOT
# vouch for -> the key is reported and the build fails.
bad=$(jq -r 'keys[]' /tmp/ui-prod-licenses.json | while IFS= read -r lic; do
  rest=$(printf '%s' "$lic" \
    | sed -E "s/\b($allow)\b//g; s/\b(OR|AND|WITH)\b//g; s/[()]//g; s/[[:space:]]+//g")
  [ -n "$rest" ] && printf '%s\n' "$lic"
done || true)
if [ -n "$bad" ]; then
  echo "Non-allowlisted license(s) found in PRODUCTION dependencies:"
  echo "$bad"
  jq . /tmp/ui-prod-licenses.json
  exit 1
fi
echo "All production dependency licenses are on the allowlist."
