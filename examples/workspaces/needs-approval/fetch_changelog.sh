#!/usr/bin/env bash
# fetch_changelog.sh — fetches a CHANGELOG from an unlisted domain.
# The agent is directed to run this; the domain triggers first-use approval.
set -euo pipefail

URL="${1:-https://example.com/CHANGELOG}"
echo "Fetching: $URL"
response=$(curl -sS --max-time 10 "$URL" 2>&1) && {
    echo "--- response (first 20 lines) ---"
    printf '%s\n' "$response" | head -20
} || {
    echo "FETCH_FAILED: $response"
}
