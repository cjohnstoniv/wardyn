#!/bin/sh
# ORACLE solution for fix-failing-test.
#
# Runs INSIDE the sandbox as the agent, cwd = the mounted workspace ($PWD).
# The bug is one line: the discount is subtracted as a flat amount instead of
# applied as a percentage. Rewrite the return so the percentage form is used.
# We rewrite pricing.py wholesale via a heredoc — deterministic and robust
# regardless of the exact seed formatting. POSIX sh, no network, does not touch
# the tests.
set -u

cat > pricing.py <<'PY'
"""Tiny pricing helper.

total(items, discount_pct) returns the sum of item prices with a percentage
discount applied.
"""


def total(items, discount_pct):
    """Sum ``items`` and apply a ``discount_pct`` percent discount."""
    subtotal = sum(items)
    return subtotal * (1 - discount_pct / 100)
PY

echo "solution.sh: applied percentage-discount fix to pricing.py" >&2
