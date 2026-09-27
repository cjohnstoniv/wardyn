#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for W18-S1-2: the live SSH gateway e2e (`make test-e2e-ssh` /
# scripts/run-e2e-ssh.sh) ran in NO CI workflow — nightly.yml had jobs for the
# other live e2e lanes but none of them invoked it. This pins the invocation
# itself, not just a job name, so a future removal regresses loudly. Plain
# grep on purpose: the repo declares no YAML parser anywhere, and "does a
# non-comment line invoke the suite" does not need one — comment lines are
# stripped first because the job's own header comment names the target.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORKFLOW="${REPO_ROOT}/.github/workflows/nightly.yml"

# CAPTURE, THEN MATCH — never `grep | grep -q` under `pipefail` (the law
# kind-sso-walk.sh's own comment names for `kubectl logs | grep -q`): `grep -q`
# exits on its first match while the upstream grep is still writing, the
# upstream takes SIGPIPE, and pipefail reports the PIPELINE failed even though
# the match was found.
non_comment_lines="$(grep -Ev '^[[:space:]]*#' "${WORKFLOW}" || true)"
if grep -Eq 'make test-e2e-ssh|run-e2e-ssh\.sh' <<<"${non_comment_lines}"; then
  echo "ok - nightly.yml invokes the live SSH gateway e2e"
else
  echo "FAIL: no non-comment line in ${WORKFLOW} invokes 'make test-e2e-ssh' or run-e2e-ssh.sh (W18-S1-2)" >&2
  exit 1
fi
