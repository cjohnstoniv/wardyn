#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-dco.sh — pins `make dco` (Makefile:1110-1133): every commit in
# DCO_RANGE, merges included, must carry a well-formed Signed-off-by
# trailer, with exactly one exemption — a 2+-parent merge commit whose
# committer is GitHub <noreply@github.com>, and only when the caller opts
# in with DCO_ALLOW_GITHUB_MERGES=1. That exemption models push/merge_group,
# where GitHub itself makes the merge; PR ranges end at the PR head instead
# and never see one.
#
# Each case builds its own throwaway repo under mktemp, with local (not
# global) user.name/user.email, then runs the REAL repo Makefile's `dco`
# recipe against it — `make -f "$ROOT/Makefile" dco DCO_RANGE=...` invoked
# with cwd inside the throwaway repo, so `git log $(DCO_RANGE)` resolves
# against the throwaway history, not this repo's. The dco recipe reads no
# repo-relative path, so this works unmodified from any cwd.
#
# Cases:
#   1. signed commits plus a signed human merge                -> 0
#   2. an unsigned human merge                                  -> non-zero
#   3. an unsigned merge committed as GitHub <noreply@github.com>:
#        DCO_ALLOW_GITHUB_MERGES=0 -> non-zero, =1 -> 0
#   4. an unsigned NON-merge commit committed as GitHub, ALLOW=1 -> non-zero
#      (the exemption covers merges only)
#
# Daemon-free, network-free.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# run_dco <repo-dir> <base-sha> [make-args...]
# Runs the real Makefile's dco target with cwd inside <repo-dir>. Prints the
# recipe's output on failure (for debugging) and returns its exit code.
run_dco() {
	local repo="$1" base="$2" out rc=0
	shift 2
	out=$(cd "$repo" && make -f "$ROOT/Makefile" dco DCO_RANGE="$base..HEAD" "$@" 2>&1) || rc=$?
	[ "$rc" -eq 0 ] || printf '%s\n' "$out" >&2
	return "$rc"
}

# mkrepo <dir> — a fresh repo with local identity and one base commit,
# outside every case's DCO_RANGE.
mkrepo() {
	local dir="$1"
	mkdir -p "$dir"
	git -C "$dir" init -q -b main
	git -C "$dir" config user.name "Test Dev"
	git -C "$dir" config user.email "test@example.com"
	echo base >"$dir/f.txt"
	git -C "$dir" add f.txt
	git -C "$dir" commit -q -m "base"
}

# ── case 1: signed commits plus a signed human merge -> 0 ──────────────────
r1="$TMP/case1"
mkrepo "$r1"
base1="$(git -C "$r1" rev-parse HEAD)"
echo main1 >>"$r1/f.txt"
git -C "$r1" commit -q -am "main: signed change" -s
git -C "$r1" checkout -q -b feature
echo feat1 >"$r1/g.txt"
git -C "$r1" add g.txt
git -C "$r1" commit -q -m "feature: signed change" -s
git -C "$r1" checkout -q main
git -C "$r1" merge -q --no-ff --no-commit feature
git -C "$r1" commit -q -s --no-edit
if ! run_dco "$r1" "$base1"; then
	fail "case 1 (signed commits + signed human merge) expected exit 0, got non-zero"
fi

# ── case 2: an unsigned human merge -> non-zero ─────────────────────────────
r2="$TMP/case2"
mkrepo "$r2"
base2="$(git -C "$r2" rev-parse HEAD)"
echo main1 >>"$r2/f.txt"
git -C "$r2" commit -q -am "main: signed change" -s
git -C "$r2" checkout -q -b feature
echo feat1 >"$r2/g.txt"
git -C "$r2" add g.txt
git -C "$r2" commit -q -m "feature: signed change" -s
git -C "$r2" checkout -q main
git -C "$r2" merge -q --no-ff -m "Merge branch 'feature' (unsigned)" feature
if run_dco "$r2" "$base2"; then
	fail "case 2 (unsigned human merge) expected non-zero exit, got 0"
fi

# ── case 3: unsigned merge committed as GitHub <noreply@github.com> ────────
r3="$TMP/case3"
mkrepo "$r3"
base3="$(git -C "$r3" rev-parse HEAD)"
echo main1 >>"$r3/f.txt"
git -C "$r3" commit -q -am "main: signed change" -s
git -C "$r3" checkout -q -b feature
echo feat1 >"$r3/g.txt"
git -C "$r3" add g.txt
git -C "$r3" commit -q -m "feature: signed change" -s
git -C "$r3" checkout -q main
GIT_COMMITTER_NAME="GitHub" GIT_COMMITTER_EMAIL="noreply@github.com" \
	git -C "$r3" merge -q --no-ff -m "Merge pull request (unsigned, GitHub)" feature
if run_dco "$r3" "$base3" DCO_ALLOW_GITHUB_MERGES=0; then
	fail "case 3 (GitHub merge, ALLOW=0) expected non-zero exit, got 0"
fi
if ! run_dco "$r3" "$base3" DCO_ALLOW_GITHUB_MERGES=1; then
	fail "case 3 (GitHub merge, ALLOW=1) expected exit 0, got non-zero"
fi

# ── case 4: unsigned NON-merge commit committed as GitHub, ALLOW=1 ─────────
# The exemption is for merge commits only, so this must still fail closed.
r4="$TMP/case4"
mkrepo "$r4"
base4="$(git -C "$r4" rev-parse HEAD)"
echo main1 >>"$r4/f.txt"
GIT_COMMITTER_NAME="GitHub" GIT_COMMITTER_EMAIL="noreply@github.com" \
	git -C "$r4" commit -q -am "non-merge, committed as GitHub, unsigned"
if run_dco "$r4" "$base4" DCO_ALLOW_GITHUB_MERGES=1; then
	fail "case 4 (unsigned non-merge as GitHub, ALLOW=1) expected non-zero exit, got 0"
fi

echo "All DCO cases behaved as expected."
