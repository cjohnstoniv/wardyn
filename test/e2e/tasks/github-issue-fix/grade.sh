#!/bin/sh
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Grader for github-issue-fix.
#
#   docker run --rm -v <run-workspace>:/ws:ro -v <task-dir>:/task:ro \
#     python:3.12-alpine sh /task/grade.sh
#
# Overlays the HIDDEN acceptance test (which is not in the workspace at all) onto
# a temp copy of /ws and runs it with stdlib unittest (no network). Passing means
# slugify() matches the issue #42 spec including the unicode + dash edge cases.
set -u

G=/tmp/g
rm -rf "$G"
cp -r /ws "$G" || { echo "FAIL could not copy workspace"; exit 1; }

cp /task/heldout/test_slugify_hidden.py "$G/test_slugify_hidden.py"

if [ ! -f "$G/textutil.py" ]; then
    echo "FAIL textutil.py missing from workspace"
    exit 1
fi

cd "$G" || { echo "FAIL cannot cd $G"; exit 1; }
# Run isolated: the workspace is the agent's, so it must not be able to shadow the stdlib
# (a unittest.py or sitecustomize.py beside the code) or end the process before a test
# runs. -I keeps the working directory off sys.path; the workspace is appended AFTER the
# stdlib so only its own modules are importable from it. Exit 0 alone proves nothing (a
# module that exits 0 on import passes it), so the verdict is the run itself: at least
# one test ran and unittest said OK.
out=$(python3 -I -c 'import os, sys, unittest
sys.path.append(os.getcwd())
unittest.main(module=None, argv=["unittest", "-v"] + sys.argv[1:])' test_slugify_hidden 2>&1)
rc=$?
printf '%s\n' "$out"
ran=0
if printf '%s\n' "$out" | grep -Eq '^Ran [1-9][0-9]* tests? in ' && printf '%s\n' "$out" | grep -qx 'OK'; then
    ran=1
fi
if [ "$rc" -eq 0 ] && [ "$ran" -eq 1 ]; then
    echo "PASS github-issue-fix: slugify matches the issue #42 spec"
    exit 0
fi
echo "FAIL github-issue-fix: slugify does not match spec (unittest rc=$rc, ran-and-ok=$ran)"
exit 1
