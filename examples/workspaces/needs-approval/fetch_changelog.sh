#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# fetch_changelog.sh — fetches a CHANGELOG from an unlisted domain.
# The agent is directed to run this; the domain triggers first-use approval.
set -euo pipefail

URL="${1:-https://example.com/CHANGELOG}"
# Under first_use_approval deny_with_review the first request is refused while the approval is
# PENDING; a retry after the decision is what carries the outcome, so retry for a bounded time.
ATTEMPTS="${FETCH_ATTEMPTS:-12}"
PAUSE="${FETCH_PAUSE_SEC:-15}"
echo "Fetching: $URL"
for ((i = 1; i <= ATTEMPTS; i++)); do
    if response=$(curl -sS --max-time 10 "$URL" 2>&1); then
        echo "--- response (first 20 lines) ---"
        printf '%s\n' "$response" | head -20
        exit 0
    fi
    echo "attempt $i/$ATTEMPTS failed: $response"
    ((i < ATTEMPTS)) && sleep "$PAUSE"
done
echo "FETCH_FAILED: $response"
