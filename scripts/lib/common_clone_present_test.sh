#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for W3-S1-3: test-drive.sh section 1 used to fall back to
# `test -d "$dir"`, which is true even for a clone that FAILED — because
# agent-run-lib.sh's clone_one() unconditionally `mkdir -p`s the dest before
# attempting `git clone`. clone_present() (scripts/lib/common.sh) must check
# for a populated .git instead, so a failed clone is reported as failed.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "${REPO_ROOT}/scripts/lib/common.sh"

# Stand in for `docker exec CTR ...`: run the rest of the argv locally
# against a plain directory instead of a container.
docker() { shift 2; "$@"; }

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

# Case 1: clone_one()'s mkdir -p ran but `git clone` failed (e.g. egress
# denied) — the dest dir exists but is empty. Must be reported as NOT cloned.
mkdir -p "${tmp}/failed-clone"
if clone_present anycontainer "${tmp}/failed-clone"; then
  echo "FAIL: clone_present true for a bare mkdir'd dir with no .git" >&2
  exit 1
fi

# Case 2: a real clone landed (.git present). Must be reported as cloned.
mkdir -p "${tmp}/real-clone/.git"
if ! clone_present anycontainer "${tmp}/real-clone"; then
  echo "FAIL: clone_present false despite .git present" >&2
  exit 1
fi

echo "ok - clone_present distinguishes a populated clone from a failed-clone's bare mkdir'd dir"
