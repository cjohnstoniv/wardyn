#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-migration-numbers.sh — check-migration-numbers.sh actually reads
# origin/main, not just the working tree.
#
# The gate's whole point is comparing THIS branch's new migration numbers
# against a remote it did not pick them from, so the fixture builds a real
# throwaway git repo with a real 'origin' remote rather than a bare directory
# of files — a bug that made the gate silently fall back to a same-repo
# comparison (always "no collision") would still pass a fixture with no
# origin to diverge from.
#
# Daemon-free, network-free: 'origin' is a local bare repo on disk.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# ── build a bare 'origin' whose main has migrations 0001..0003 ──────────────
BARE="$TMP/origin.git"
git init --quiet --bare -b main "$BARE"

SEED="$TMP/seed"
git init --quiet -b main "$SEED"
git -C "$SEED" config user.email "test@example.com"
git -C "$SEED" config user.name "test"
mkdir -p "$SEED/internal/db/migrations"
for n in 0001 0002 0003; do
  echo "-- migration $n" > "$SEED/internal/db/migrations/${n}_seed.sql"
done
git -C "$SEED" add -A
git -C "$SEED" commit --quiet -m seed
git -C "$SEED" remote add origin "$BARE"
git -C "$SEED" push --quiet origin main

# ── the working repo: a clone, with the gate script copied in ───────────────
WORK="$TMP/work"
git clone --quiet "$BARE" "$WORK"
git -C "$WORK" checkout --quiet main
git -C "$WORK" config user.email "test@example.com"
git -C "$WORK" config user.name "test"
mkdir -p "$WORK/scripts"
cp "$ROOT/scripts/check-migration-numbers.sh" "$WORK/scripts/"

run_gate() { (cd "$WORK" && ./scripts/check-migration-numbers.sh >/dev/null 2>&1); }
gate_says() { (cd "$WORK" && ./scripts/check-migration-numbers.sh 2>&1 >/dev/null); }

# 1. a new migration numbered past origin/main's max => pass.
echo "-- ok" > "$WORK/internal/db/migrations/0004_new.sql"
run_gate || fail "0004 after a 0003 max must PASS"
echo "ok  new prefix past the remote max passes"
rm "$WORK/internal/db/migrations/0004_new.sql"

# 2. a new migration REUSING origin/main's max prefix => fail. This is the
#    regression the gate exists for: two branches off the same base each
#    independently pick "the next number" and each looks clean in isolation.
echo "-- collide" > "$WORK/internal/db/migrations/0003_collide.sql"
if run_gate; then fail "reusing the remote's max prefix (0003) must FAIL"; fi
# gate_says exits 1 (the gate correctly failed); under pipefail a pipe into
# grep would report THAT exit rather than grep's own, so capture first (the
# test-image-pins.sh convention) instead of piping straight into grep.
out="$(gate_says || true)"
printf '%s' "$out" | grep -q "0003" || fail "the failure message must name the colliding file: $out"
echo "ok  prefix <= remote max fails, naming the file"
rm "$WORK/internal/db/migrations/0003_collide.sql"

# 3. a new migration numbered BELOW origin/main's max => fail (not just
#    equal — the gate must reject the whole range, not only a tie).
echo "-- behind" > "$WORK/internal/db/migrations/0002_behind.sql"
if run_gate; then fail "a prefix BELOW the remote max (0002) must FAIL"; fi
echo "ok  prefix below the remote max fails"
rm "$WORK/internal/db/migrations/0002_behind.sql"

# 4. an UNCHANGED pre-existing migration (already on origin/main, untouched
#    on this branch) must never be flagged — only NEW files are this gate's
#    business.
run_gate || fail "an unmodified pre-existing migration set must PASS"
echo "ok  unmodified tree passes"

# 5. no 'origin' remote at all => skip (exit 0), never a hard failure — this
#    gate degrades gracefully rather than blocking `make lint` offline.
NOORIGIN="$TMP/noorigin"
git init --quiet -b main "$NOORIGIN"
git -C "$NOORIGIN" config user.email "test@example.com"
git -C "$NOORIGIN" config user.name "test"
mkdir -p "$NOORIGIN/scripts" "$NOORIGIN/internal/db/migrations"
cp "$ROOT/scripts/check-migration-numbers.sh" "$NOORIGIN/scripts/"
echo "-- x" > "$NOORIGIN/internal/db/migrations/0001_x.sql"
git -C "$NOORIGIN" add -A
git -C "$NOORIGIN" commit --quiet -m seed
(cd "$NOORIGIN" && ./scripts/check-migration-numbers.sh >/dev/null 2>&1) \
  || fail "no origin remote must SKIP (exit 0), not fail"
echo "ok  no origin remote skips instead of failing"

echo "test-migration-numbers: all cases passed"
