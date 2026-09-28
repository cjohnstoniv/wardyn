#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-workflow-artifacts.sh — every actions/upload-artifact step in
# .github/workflows/*.yml must have a non-empty `path`.
#
# #374 shipped `upload_playwright_report` with `path: ''`: a later step's
# `path:` block was inserted directly below the upload step instead of above
# it, so the upload step's own path lines became the FOLLOWING step's `run:`
# argument and the upload step's block scalar was left empty. With
# `continue-on-error: true` on that step every run printed a warning and
# never attached a report — exactly the artifact needed to read WHY a red
# `ui-e2e` job failed (see scripts/test-report.sh's own G11 fix). YAML alone
# can't catch this: it's valid YAML, just the wrong step's content. Assert
# the shape directly instead of depending on actionlint being installed.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v python3 >/dev/null 2>&1 || { echo "check-workflow-artifacts.sh: python3 not found" >&2; exit 1; }

python3 - <<'PYEOF'
import glob
import sys

try:
    import yaml
except ImportError:
    print("check-workflow-artifacts.sh: PyYAML not available (pip install pyyaml)", file=sys.stderr)
    sys.exit(1)

bad = []
for wf in sorted(glob.glob(".github/workflows/*.yml")) + sorted(glob.glob(".github/workflows/*.yaml")):
    with open(wf) as f:
        doc = yaml.safe_load(f)
    jobs = (doc or {}).get("jobs") or {}
    for job_id, job in jobs.items():
        for step in (job or {}).get("steps") or []:
            uses = (step or {}).get("uses", "")
            if not uses.startswith("actions/upload-artifact@"):
                continue
            path = ((step or {}).get("with") or {}).get("path")
            name = step.get("name", uses)
            if not path or not str(path).strip():
                bad.append(f"{wf}: job '{job_id}' step '{name}' — upload-artifact with an empty path")

if bad:
    print("check-workflow-artifacts.sh: FAILED", file=sys.stderr)
    for b in bad:
        print(f"  {b}", file=sys.stderr)
    sys.exit(1)

print("check-workflow-artifacts.sh: every upload-artifact step has a non-empty path")
PYEOF
