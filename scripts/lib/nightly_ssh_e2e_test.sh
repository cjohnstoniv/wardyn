#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for W18-S1-2: the live SSH gateway e2e (`make test-e2e-ssh` /
# scripts/run-e2e-ssh.sh) ran in NO CI workflow — nightly.yml had jobs for the
# other live e2e lanes (test-e2e, test-e2e-live, test-drive, ci-mode-dogfood)
# but none of them, nor any other job, invoked `make test-e2e-ssh`. This
# statically checks that a nightly.yml job actually runs it, so a future
# removal of that step regresses loudly instead of silently.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORKFLOW="${REPO_ROOT}/.github/workflows/nightly.yml"

if ! command -v python3 >/dev/null 2>&1; then
  echo "SKIP: python3 not available to parse ${WORKFLOW}" >&2
  exit 0
fi

python3 - "${WORKFLOW}" <<'PYEOF'
import sys
import yaml

path = sys.argv[1]
with open(path) as f:
    doc = yaml.safe_load(f)

jobs = doc.get("jobs", {})
hit = None
for job_name, job in jobs.items():
    for step in job.get("steps", []):
        run = step.get("run", "") or ""
        if "make test-e2e-ssh" in run or "run-e2e-ssh.sh" in run:
            hit = job_name
            break
    if hit:
        break

if not hit:
    print(f"FAIL: no job in {path} runs `make test-e2e-ssh` / run-e2e-ssh.sh", file=sys.stderr)
    sys.exit(1)

print(f"ok - job {hit!r} in nightly.yml runs the live SSH gateway e2e")
PYEOF
