#!/bin/sh
# ORACLE "solution" for interactive-repl — DOCUMENTATION ONLY, NOT EXECUTED.
#
# This task is interactive: the sandbox idles (Cmd=sleep infinity) and the
# ORCHESTRATOR drives the PTY over the WS-attach channel. There is no in-sandbox
# oracle execution. This file records the drive script the orchestrator performs,
# mirroring the `expects:` block in task.yaml, so a human can reproduce it by hand
# via `wardyn attach <run-id>`:
#
#   1. Prove the PTY is live and evaluates shell arithmetic:
#        echo wardyn-pty-$((6*7))
#      expect the stream to contain: wardyn-pty-42
#
#   2. In-PTY NEGATIVE egress probe — a denied host must be held by the proxy:
#        curl -sS -o /dev/null -m 12 -w '%{http_code}' \
#          -x http://wardyn-proxy:3128 https://evil.example.com/
#      expect the stream to contain: 403
#
#   3. Close the session:
#        exit
#      expect: EOF
#
# Grading is stream-based in the orchestrator (it matches the expectations above
# against the PTY output); grade.sh is a no-op placeholder.
echo "interactive-repl: this solution is documentation only; the orchestrator drives the PTY (see task.yaml expects:)." >&2
exit 0
