#!/bin/sh
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
python3 -m unittest -v test_slugify_hidden
rc=$?
if [ "$rc" -eq 0 ]; then
    echo "PASS github-issue-fix: slugify matches the issue #42 spec"
    exit 0
fi
echo "FAIL github-issue-fix: slugify does not match spec (unittest rc=$rc)"
exit 1
