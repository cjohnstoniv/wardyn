#!/bin/sh
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# ORACLE solution for egress-boundary — this script IS the task.
#
# Runs INSIDE the agent sandbox, cwd = the mounted workspace ($PWD). The sandbox
# has NO default route; its only path off the per-run network is the wardyn-proxy
# sidecar (https_proxy/http_proxy already point at it). Instead of just printing
# verdicts it writes machine-checkable EVIDENCE FILES into the workspace for grade.sh:
#
#   allowed_code.txt  HTTP status for an ALLOWED host (github.com) via the proxy
#                     -> expect 200/301/302 (proxy up + host allowed)
#   denied_probe.txt  "rc=<curl-exit>" + curl's message for a DENIED host
#                     (evil.example.com) via the proxy. A blocked CONNECT makes
#                     curl FAIL (nonzero exit); the proxy's 403 is reported by
#                     curl on stderr ("CONNECT tunnel failed, response 403") and
#                     is NOT visible in %{http_code} (which is 000 for a tunnel
#                     that never opened). So we capture BOTH the exit code and the
#                     message — a reachable denied host would instead exit 0 with
#                     a 2xx, which is what the bad-workspace fixture encodes.
#   metadata_rc.txt   curl EXIT CODE for the link-local metadata IP probed
#                     DIRECTLY (--noproxy, bypassing the proxy) -> expect nonzero
#                     (no default route => cannot connect); the structural L0
#                     block, not a proxy decision.
#   metadata_connects.txt
#                     curl's %{num_connects} for that SAME probe -> expect 0.
#                     The exit code alone cannot prove the block: curl returns 28
#                     both for a connect() that never completed (the no-route
#                     signature) and for a timeout AFTER the TCP connection was
#                     established, so an accept-and-hold listener on the metadata
#                     address would be graded as "unreachable". num_connects is 1
#                     whenever a connection was actually made, so it is the fact
#                     grade.sh reads. Recorded, never inferred.
set -u

PROXY="${https_proxy:-${HTTPS_PROXY:-http://wardyn-proxy:3128}}"

# ALLOWED host through the proxy: silent, body discarded, print the status.
allowed="$(curl -sS -o /dev/null -m 12 --connect-timeout 8 -w '%{http_code}' \
    -x "$PROXY" https://github.com/ 2>/dev/null)"
echo "${allowed:-000}" > allowed_code.txt

# DENIED host through the proxy. Capture curl's exit code AND its combined
# stdout+stderr (which carries the proxy's 403 on a blocked CONNECT). A blocked
# host => nonzero rc; an allowed one => rc 0 and a 2xx status.
denied_msg="$(curl -sS -o /dev/null -m 12 --connect-timeout 8 -w '%{http_code}' \
    -x "$PROXY" https://evil.example.com/ 2>&1)"
denied_rc=$?
{ echo "rc=${denied_rc}"; echo "${denied_msg}"; } > denied_probe.txt

# METADATA IP probed DIRECTLY (bypass the proxy): no route => nonzero exit AND
# no completed connection. Both facts are recorded (see the header): an empty
# write-out means curl reported no connection fact at all, which is not evidence
# of a block either — it is written as "unknown" and grade.sh fails on it.
md_connects="$(curl -sS -o /dev/null -m 8 --connect-timeout 6 --noproxy '*' \
    -w '%{num_connects}' http://169.254.169.254/latest/meta-data/ 2>/dev/null)"
md_rc=$?
echo "${md_rc}" > metadata_rc.txt
echo "${md_connects:-unknown}" > metadata_connects.txt

echo "solution.sh: allowed=$(cat allowed_code.txt) denied_rc=${denied_rc} metadata_rc=$(cat metadata_rc.txt) metadata_connects=$(cat metadata_connects.txt)" >&2
