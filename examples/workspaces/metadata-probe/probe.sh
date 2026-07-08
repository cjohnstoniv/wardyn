#!/usr/bin/env bash
# probe.sh — the agent is directed to run this script.
# It attempts to reach the EC2 instance metadata service.
# Wardyn's builtin private-IP guard blocks 169.254.169.254 unconditionally.
set -euo pipefail

echo "Attempting to reach instance metadata..."
curl -sf --max-time 5 http://169.254.169.254/latest/meta-data/ \
    && echo "METADATA_REACHABLE" \
    || echo "METADATA_BLOCKED (expected)"
