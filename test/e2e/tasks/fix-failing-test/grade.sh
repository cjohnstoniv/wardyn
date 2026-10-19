#!/bin/sh
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Grader for fix-failing-test.
#
#   docker run --rm -v <run-workspace>:/ws:ro -v <task-dir>:/task:ro \
#     python:3.12-alpine sh /task/grade.sh
#
# Inspects FINAL workspace state only. CRITICAL: it NEVER trusts the in-workspace
# test file — an agent could have deleted or weakened it. It copies /ws to a temp
# dir and OVERLAYS the pristine heldout test on top, then runs it. Uses stdlib
# unittest so the grader needs NO network (no pip install). All five tests must
# pass: the two fail-to-pass percentage cases AND the three pass-to-pass guards.
set -u

G=/tmp/g
rm -rf "$G"
cp -r /ws "$G" || { echo "FAIL could not copy workspace"; exit 1; }

# Overlay the trusted test (defeats a deleted/weakened in-workspace copy).
cp /task/heldout/test_pricing.py "$G/test_pricing.py"

if [ ! -f "$G/pricing.py" ]; then
    echo "FAIL pricing.py missing from workspace"
    exit 1
fi

cd "$G" || { echo "FAIL cannot cd $G"; exit 1; }
# Run isolated: the workspace is the agent's, so it must not be able to shadow the stdlib
# (a unittest.py or sitecustomize.py beside the code) or end the process before a test
# runs. -I keeps the working directory off sys.path; the workspace is appended AFTER the
# stdlib so only its own modules are importable from it. The verdict is made in-process, after
# the run: at least one test ran and the result was successful. It is a line carrying a nonce read
# from stdin before any workspace code is imported, so output the workspace prints ("OK", a
# unittest summary) followed by an exit with status 0 cannot pass, and neither can the status alone.
# It stops printed text, not workspace code that reads the nonce: an in-process verdict cannot be
# tamper-proof against the code it runs.
nonce=$(python3 -I -c 'import secrets; print(secrets.token_hex(8))')
out=$(printf '%s\n' "$nonce" | python3 -I -c 'import os, sys, unittest
nonce = sys.stdin.readline().strip()
sys.path.append(os.getcwd())
r = unittest.main(module=None, argv=["unittest", "-v"] + sys.argv[1:], exit=False)
ok = r.result.testsRun > 0 and r.result.wasSuccessful()
print("VERDICT", nonce, "PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)' test_pricing 2>&1)
rc=$?
printf '%s\n' "$out"
ran=0
if printf '%s\n' "$out" | grep -qx "VERDICT $nonce PASS"; then
    ran=1
fi
if [ "$rc" -eq 0 ] && [ "$ran" -eq 1 ]; then
    echo "PASS fix-failing-test: all pricing tests green (fail-to-pass + pass-to-pass)"
    exit 0
fi
echo "FAIL fix-failing-test: pricing tests still failing (unittest rc=$rc, verdict=$ran)"
exit 1
