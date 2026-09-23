#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-workflow-artifacts.sh — check-workflow-artifacts.sh must catch #374's
# exact shape (an upload-artifact step whose `path:` block scalar is empty)
# and must not flag an unrelated `uses:` step or a properly-pathed upload.
#
# Daemon-free, network-free: it runs the real gate against a throwaway tree.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/scripts" "$TMP/.github/workflows"
cp "$ROOT/scripts/check-workflow-artifacts.sh" "$TMP/scripts/"

fail() { echo "FAIL: $*" >&2; exit 1; }
run_gate() { (cd "$TMP" && ./scripts/check-workflow-artifacts.sh >/dev/null 2>&1); }

# 1. A non-empty path passes.
cat > "$TMP/.github/workflows/ci.yml" <<'EOF'
jobs:
  ui-e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - name: Upload Playwright report
        if: always()
        uses: actions/upload-artifact@v7
        with:
          name: playwright-report
          path: |
            ui/playwright-report/**
            ui/test-results/**
EOF
run_gate || fail "a non-empty path must PASS"
echo "ok  non-empty path passes"

# 2. #374's exact shape: `path: ''`, no failure.
cat > "$TMP/.github/workflows/ci.yml" <<'EOF'
jobs:
  ui-e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - name: Upload Playwright report
        if: always()
        uses: actions/upload-artifact@v7
        with:
          name: playwright-report
          path: ''
      - name: Say so if playwright-report did not upload
        run: |
          echo "::warning::playwright-report missing"
EOF
if run_gate; then fail "an upload-artifact with path: '' must FAIL — the gate went blind"; fi
echo "ok  empty path fails"

# 3. A `with:` block with no `path` key at all must also fail.
cat > "$TMP/.github/workflows/ci.yml" <<'EOF'
jobs:
  ui-e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/upload-artifact@v7
        with:
          name: playwright-report
EOF
if run_gate; then fail "an upload-artifact with no path key must FAIL"; fi
echo "ok  missing path key fails"

# 4. A step that merely NAMES upload-artifact in a run: line (not `uses:`)
#    must not be mistaken for the real step.
cat > "$TMP/.github/workflows/ci.yml" <<'EOF'
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - name: not a real upload
        run: echo "actions/upload-artifact@v7 mentioned in passing"
EOF
run_gate || fail "a run: line merely mentioning upload-artifact must PASS"
echo "ok  a run: line mentioning upload-artifact is not flagged"
