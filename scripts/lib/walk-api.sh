# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash

# scripts/lib/walk-api.sh: the admin API call the walk and e2e scripts share.
#
#   walk_api URL TOKEN RESP MAX_TIME METHOD PATH [BODY]
#
# Sends METHOD to URL followed by PATH, with TOKEN as the bearer and BODY, when it is not empty, as JSON. It prints
# the HTTP status code and writes the response body to RESP. MAX_TIME is curl's --max-time in seconds; empty means
# no limit. Its exit status is curl's.
#
# A script binds its own settings in a one-line api(), which reads them at each call, so a value set after the
# script sources this file (a token read from a Secret) or changed later (a port picked again) is the one sent:
#
#   api() { walk_api "${BASE}" "${ADMIN_TOKEN}" "${TMPDIR}/resp.json" "${CURL_MAX_TIME}" "$@"; }

walk_api() {
  local url="$1" token="$2" resp="$3" max_time="$4" method="$5" path="$6" body="${7:-}"
  local args=(-sS -o "${resp}" -w '%{http_code}' -X "${method}" "${url}${path}" -H "Authorization: Bearer ${token}")
  if [[ -n "${max_time}" ]]; then args+=(--max-time "${max_time}"); fi
  if [[ -n "${body}" ]]; then args+=(-H "Content-Type: application/json" -d "${body}"); fi
  curl "${args[@]}"
}
