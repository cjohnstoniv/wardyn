#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-k6-rerun-rate.sh — k6-rerun-rate.sh counts each k6 leg's attempts and failures from the
# nightly's job history. A skipped or cancelled leg is not an attempt, and a job that is not a k6
# leg is not listed.
#
# Daemon-free, network-free: canned attempt JSON on stdin.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "FAIL: $*" >&2; exit 1; }

attempts='[
{"run":1,"attempt":1,"jobs":[
  {"name":"k6-kind-conformance (v1.31.4, org)","conclusion":"success"},
  {"name":"k6-kind-conformance (v1.31.4, inproc)","conclusion":"failure"},
  {"name":"fuzz","conclusion":"failure"}]},
{"run":1,"attempt":2,"jobs":[
  {"name":"k6-kind-conformance (v1.31.4, org)","conclusion":"skipped"},
  {"name":"k6-kind-conformance (v1.31.4, inproc)","conclusion":"success"}]},
{"run":2,"attempt":1,"jobs":[
  {"name":"k6-kind-conformance (v1.31.4, org)","conclusion":"success"},
  {"name":"k6-kind-conformance (v1.31.4, inproc)","conclusion":"cancelled"},
  {"name":"k6-kind-conformance (v1.35.0, org)","conclusion":"failure"}]},
{"run":3,"attempt":1,"jobs":[
  {"name":"k6-kind-conformance (v1.31.4, org)","conclusion":"success"},
  {"name":"k6-kind-conformance (v1.35.0, org)","conclusion":"success"},
  {"name":"k6-kind-conformance (v1.35.0, org)","conclusion":"success"}]}
]'

got="$("$ROOT/scripts/k6-rerun-rate.sh" --table <<<"$attempts")"
want='| leg | attempts | failed | failure rate |
|---|---|---|---|
| k6-kind-conformance (v1.31.4, inproc) | 2 | 1 | 50% |
| k6-kind-conformance (v1.31.4, org) | 3 | 0 | 0% |
| k6-kind-conformance (v1.35.0, org) | 3 | 1 | 33.3% |'
[ "$got" = "$want" ] || fail "table: got
$got
want
$want"

got="$("$ROOT/scripts/k6-rerun-rate.sh" --table <<<'[]')"
[ "$(wc -l <<<"$got")" = 2 ] || fail "no attempts: want the header only, got
$got"

echo "test-k6-rerun-rate: ok"
