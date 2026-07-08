#!/bin/sh
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
python3 -m unittest -v test_pricing
rc=$?
if [ "$rc" -eq 0 ]; then
    echo "PASS fix-failing-test: all pricing tests green (fail-to-pass + pass-to-pass)"
    exit 0
fi
echo "FAIL fix-failing-test: pricing tests still failing (unittest rc=$rc)"
exit 1
