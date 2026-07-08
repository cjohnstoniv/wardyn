#!/bin/sh
# Wardyn e2e in-sandbox egress probe.
#
# Run INSIDE the agent sandbox (which has NO default route — its only path off
# the per-run internal network is the wardyn-proxy sidecar at wardyn-proxy:3128).
# It exercises the three egress classes the e2e assertions care about and prints
# a machine-greppable verdict per class:
#
#   ALLOWED  api.anthropic.com         (in the policy allowlist) -> expect PASS
#   DENIED   evil.example.com          (not in allowlist)        -> expect BLOCK (403 from proxy)
#   METADATA 169.254.169.254           (link-local metadata IP)  -> expect BLOCK (unconditional)
#
# Egress is attempted ONLY through the proxy (https_proxy/http_proxy point at
# the sidecar). We deliberately do NOT trust HTTP_PROXY for structural security
# (that is the documented Copilot bypass, invariant 3) — the structural L0 test
# (no default route) is asserted separately by the harness via `ip route`.
#
# Args (optional): $1 = proxy URL (default http://wardyn-proxy:3128)
set -u

PROXY="${1:-http://wardyn-proxy:3128}"
export http_proxy="$PROXY" https_proxy="$PROXY" HTTP_PROXY="$PROXY" HTTPS_PROXY="$PROXY"

# curl flags: silent, show only the proxy's HTTP status, short timeouts so a
# black-holed metadata IP fails fast instead of hanging the probe.
CURL="curl -sS -o /dev/null -m 12 --connect-timeout 8 -w %{http_code}"

probe() {
  label="$1"; url="$2"
  code="$($CURL "$url" 2>/dev/null)"
  rc=$?
  echo "PROBE ${label} url=${url} curl_rc=${rc} http_code=${code:-none}"
}

echo "=== wardyn e2e egress probe (proxy=${PROXY}) ==="
probe ALLOWED  "https://api.anthropic.com/v1/models"
probe DENIED   "https://evil.example.com/"
probe METADATA "http://169.254.169.254/latest/meta-data/"
echo "=== probe complete ==="
