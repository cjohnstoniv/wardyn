#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# walk_api (scripts/lib/walk-api.sh) is the one admin API call eleven walk and e2e scripts make. Each script's
# one-line api() passes its own URL, token, body file and time limit; this pins what walk_api hands curl and that
# curl's exit status reaches the caller.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "${REPO_ROOT}/scripts/lib/walk-api.sh"

# Stand in for curl: record the argv one word per line, print a status code, exit with STUB_RC.
STUB_RC=0
curl() { printf '%s\n' "$@" >"${tmp}/argv"; printf 200; return "${STUB_RC}"; }

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

code="$(walk_api http://h/api/v1 tok /r.json 10 POST /runs '{"a":1}')"
[[ "${code}" == 200 ]] || fail "status code ${code}, want 200"
want='-sS
-o
/r.json
-w
%{http_code}
-X
POST
http://h/api/v1/runs
-H
Authorization: Bearer tok
--max-time
10
-H
Content-Type: application/json
-d
{"a":1}'
[[ "$(cat "${tmp}/argv")" == "${want}" ]] || fail "a call with a time limit and a body passed: $(cat "${tmp}/argv")"

walk_api http://h tok /r.json "" GET /healthz >/dev/null
want='-sS
-o
/r.json
-w
%{http_code}
-X
GET
http://h/healthz
-H
Authorization: Bearer tok'
[[ "$(cat "${tmp}/argv")" == "${want}" ]] || fail "a call with no time limit and no body passed: $(cat "${tmp}/argv")"

STUB_RC=7
rc=0
walk_api http://h tok /r.json "" GET /healthz >/dev/null || rc=$?
[[ "${rc}" == 7 ]] || fail "curl exited 7 and walk_api returned ${rc}"

echo "ok - walk_api passes curl the caller's URL, token, body file and limit, and returns curl's exit status"
