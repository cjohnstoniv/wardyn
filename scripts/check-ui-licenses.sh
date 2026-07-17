#!/usr/bin/env bash
# Fail closed on a copyleft license in a SHIPPED (prod) UI dependency. GPL/AGPL/
# LGPL/MPL/EPL require source disclosure / relicensing on distribution; a
# dual-licensed "X OR Apache-2.0" still fails closed here — we don't assume the
# OR resolves in our favor without review. The known MPL/EPL hits (lightningcss,
# elkjs, dompurify) are devDependencies, excluded by --prod, so they don't trip.
# Extracted verbatim from ci.yml so the gate is single-sourced (Group F).
set -euo pipefail

cd "$(dirname "$0")/../ui"

pnpm licenses list --prod --json > /tmp/ui-prod-licenses.json
bad=$(jq -r 'keys[]' /tmp/ui-prod-licenses.json \
  | grep -Ei '(^|[^A-Za-z])(A?GPL|LGPL|MPL|EPL)([-0-9]|$)' || true)
if [ -n "$bad" ]; then
  echo "Copyleft license(s) found in PRODUCTION dependencies:"
  echo "$bad"
  jq . /tmp/ui-prod-licenses.json
  exit 1
fi
echo "No copyleft licenses in production dependencies."
