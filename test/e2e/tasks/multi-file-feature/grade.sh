#!/bin/sh
# Grader for multi-file-feature.
#
#   docker run --rm -v <run-workspace>:/ws:ro -v <task-dir>:/task:ro \
#     python:3.12-alpine sh /task/grade.sh
#
# Copies /ws to a temp dir and OVERLAYS the pristine heldout acceptance test AND
# the heldout regression test (never trusts in-workspace copies). Runs both with
# stdlib unittest (no network). The feature (search across cli.py + storage.py)
# must be present AND the existing add/list behaviour intact.
set -u

G=/tmp/g
rm -rf "$G"
cp -r /ws "$G" || { echo "FAIL could not copy workspace"; exit 1; }

cp /task/heldout/test_search.py   "$G/test_search.py"
cp /task/heldout/test_existing.py "$G/test_existing.py"

for f in cli.py storage.py; do
    if [ ! -f "$G/$f" ]; then
        echo "FAIL $f missing from workspace"
        exit 1
    fi
done

cd "$G" || { echo "FAIL cannot cd $G"; exit 1; }
python3 -m unittest -v test_search test_existing
rc=$?
if [ "$rc" -eq 0 ]; then
    echo "PASS multi-file-feature: search feature works and add/list intact"
    exit 0
fi
echo "FAIL multi-file-feature: acceptance/regression tests failing (unittest rc=$rc)"
exit 1
