#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-dco.sh — pins `make dco` (Makefile:1111-1136): every commit in
# DCO_RANGE, merges included, must carry a well-formed Signed-off-by
# trailer, with exactly one exemption — a 2+-parent merge commit whose
# committer is EXACTLY GitHub <noreply@github.com> AND that carries GitHub's
# web-flow signature (key id B5690EEEBB952194), and only when the
# caller opts in with DCO_ALLOW_GITHUB_MERGES=1. That exemption models
# push/merge_group only, where GitHub itself makes such merges — PR ranges
# end at the PR head and never pass this flag, so every commit in a PR's
# own range, merges included, must carry Signed-off-by, even a
# GitHub-committed merge (e.g. from "Update branch") landed on the branch
# itself (#1070).
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
#   3. an unsigned merge committed as GitHub <noreply@github.com>, which is
#      all a local `GIT_COMMITTER_EMAIL=` forges:
#        ALLOW=0 -> non-zero, ALLOW=1 -> non-zero (no web-flow signature)
#      the same merge carrying the web-flow signature block:
#        ALLOW=0 -> non-zero, ALLOW=1 -> 0
#   4. an unsigned NON-merge commit committed as GitHub, ALLOW=1 -> non-zero
#      (the exemption covers merges only)
#   5. ALLOW=1 exempts an exact committer match ONLY, not "any merge":
#        (a) an unsigned merge committed as an impersonating identity,
#            Mallory <evil-noreply@github.com>              -> non-zero
#        (b) an unsigned plain human merge                   -> non-zero
#   6. ci.yml's own PR-range invocation (extracted from the workflow file,
#      not reimplemented) run against an unsigned GitHub-committed merge on
#      the branch (e.g. from "Update branch")                -> non-zero
#      (pins that the PR-range branch of ci.yml never carries
#      DCO_ALLOW_GITHUB_MERGES — that exemption is push/merge_group only)
#   7. ci.yml's PR range, run against a release PR that merges main, where
#      main holds an unsigned GitHub-committed merge (from a merged PR):
#        the range as ci.yml writes it (^origin/main)        -> 0
#        the same range without ^origin/main                  -> non-zero
#      (pins that commits already on main are not re-judged on a PR)
#
# Daemon-free, network-free.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
# The cases have passed or failed by the time this runs, so it must not decide
# the verdict: under `set -e` a failing `rm` in the trap turned a green run red
# (`rm: cannot remove '.../case7/.git': Directory not empty`, after "All DCO
# cases behaved as expected."). One retry covers a file written while rm walks
# the tree; a directory that still will not go is named and left behind.
cleanup() {
	rm -rf "$TMP" 2>/dev/null && return 0
	sleep 1
	rm -rf "$TMP" || echo "test-dco: could not remove $TMP" >&2
}
trap cleanup EXIT

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

# A real signature block from a GitHub-made merge commit (PR #1341). webflow_sign
# copies it onto another commit: git reads the key id from the block without
# the key, which is all the exemption checks, so the copy stands in for a
# genuine GitHub merge. It needs gpg, as the dco recipe does.
WEBFLOW_SIG='-----BEGIN PGP SIGNATURE-----

wsFcBAABCAAQBQJquqhZCRC1aQ7uu5UhlAAAnyMQAGBpg5XEFBqPqoamyDQRlHef
cerQnLtXIOq8i6ZAmqwFcvJ/C+eHx7Aa9j1+jmv0FyDn9VNaMyYPqwJbjqG47bg6
KGt6bhJ6qbtOWVb82g2AQZsN4MwPx0m8Ridg5MzsmldK0F9QX54Ub0SOA2CGmoyA
KRnLXA1MugFaACPkSePKVL6zdm3NAAI2zrno2IVSiGbY5D0jSjnAxAsONAiwGNK+
/k3DyRqYyZoSlEe3gUM+alUBDj+ekrcir/+ysG7IOz3AiomQVRn/sOttDFXkWmN+
6fPZmuHpwaxNUT+/QSvh83KEqpq/kg8vKfu/WeDK32crjtcm2JsfbCipe1zAcydq
WM59GnQBMW1xNqyRRslRy2NQ37nHVJE9aGNu6CBjdNC1v1kSjtvHSQoqlenxDksW
kO8yW39GDP8jgxbsAbyEN8KFP6tYvCXJcR/6RumRuMx4s0ZXed9DLTeNWwEb1Y+S
utAgods0M+j7/QJK3a2f7Viyo52k+S+Domz+cWC67Nqjo17cIYnyoJKChw8V4Q3D
oor/BFG8DTbYTyxVjWyfFLZpmwfhFrWuGjtIhUqcVSJdWBnz86r3sfM09l1wMY1O
UAtcEKH1c+e2wjCvaHqo1uaaHPAju7eeoH/CTol4WQWXemlNEjLY+JN66rWKVBqW
3IPbeKXjT5QHecALbVl3
=gEak
-----END PGP SIGNATURE-----'

# webflow_sign <repo-dir> — re-seal HEAD (on branch main) with the signature
# block above, keeping every other byte of the commit.
webflow_sign() {
	local repo="$1" raw new
	raw="$(git -C "$repo" cat-file commit HEAD)"
	new="$({
		printf '%s\n' "$raw" | sed '/^$/q' | sed '$d'
		printf 'gpgsig %s\n' "$(printf '%s\n' "$WEBFLOW_SIG" | sed '2,$s/^/ /')"
		printf '\n%s\n' "$(printf '%s\n' "$raw" | sed '1,/^$/d')"
	} | git -C "$repo" hash-object -t commit -w --stdin)"
	git -C "$repo" update-ref refs/heads/main "$new"
	[ "$(git -C "$repo" log -1 --format=%GK)" = "B5690EEEBB952194" ] \
		|| fail "webflow_sign: git does not read the web-flow key id from the re-sealed commit (is gpg installed?)"
}

# mkrepo <dir> — a fresh repo with local identity and one base commit,
# outside every case's DCO_RANGE.
mkrepo() {
	local dir="$1"
	mkdir -p "$dir"
	git -C "$dir" init -q -b main
	git -C "$dir" config user.name "Test Dev"
	git -C "$dir" config user.email "test@example.com"
	# No background git in a repo the EXIT trap is about to delete: a commit
	# or merge may leave `git maintenance run --auto` (or gc) writing under
	# .git after the command has returned. Local config, so the git that
	# `make dco` runs in this repo obeys it too.
	git -C "$dir" config gc.auto 0
	git -C "$dir" config maintenance.auto false
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
if run_dco "$r3" "$base3" DCO_ALLOW_GITHUB_MERGES=1 2>/dev/null; then
	fail "case 3 (merge committed as GitHub but carrying no web-flow signature, ALLOW=1) expected non-zero exit, got 0"
fi
webflow_sign "$r3"
if run_dco "$r3" "$base3" DCO_ALLOW_GITHUB_MERGES=0 2>/dev/null; then
	fail "case 3 (web-flow signed merge, ALLOW=0) expected non-zero exit, got 0"
fi
if ! run_dco "$r3" "$base3" DCO_ALLOW_GITHUB_MERGES=1; then
	fail "case 3 (web-flow signed merge, ALLOW=1) expected exit 0, got non-zero"
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

# ── case 5a: unsigned merge committed as an impersonating identity ─────────
# Mallory <evil-noreply@github.com> is not GitHub <noreply@github.com> — the
# committer email must match exactly, not just look GitHub-ish.
r5a="$TMP/case5a"
mkrepo "$r5a"
base5a="$(git -C "$r5a" rev-parse HEAD)"
echo main1 >>"$r5a/f.txt"
git -C "$r5a" commit -q -am "main: signed change" -s
git -C "$r5a" checkout -q -b feature
echo feat1 >"$r5a/g.txt"
git -C "$r5a" add g.txt
git -C "$r5a" commit -q -m "feature: signed change" -s
git -C "$r5a" checkout -q main
GIT_COMMITTER_NAME="Mallory" GIT_COMMITTER_EMAIL="evil-noreply@github.com" \
	git -C "$r5a" merge -q --no-ff -m "Merge pull request (unsigned, impersonating GitHub)" feature
if run_dco "$r5a" "$base5a" DCO_ALLOW_GITHUB_MERGES=1; then
	fail "case 5a (unsigned merge committed as Mallory <evil-noreply@github.com>, ALLOW=1) expected non-zero exit, got 0"
fi

# ── case 5b: unsigned plain human merge, ALLOW=1 ────────────────────────────
# ALLOW=1 is not "any merge is exempt" — only GitHub's own exact identity.
r5b="$TMP/case5b"
mkrepo "$r5b"
base5b="$(git -C "$r5b" rev-parse HEAD)"
echo main1 >>"$r5b/f.txt"
git -C "$r5b" commit -q -am "main: signed change" -s
git -C "$r5b" checkout -q -b feature
echo feat1 >"$r5b/g.txt"
git -C "$r5b" add g.txt
git -C "$r5b" commit -q -m "feature: signed change" -s
git -C "$r5b" checkout -q main
git -C "$r5b" merge -q --no-ff -m "Merge branch 'feature' (unsigned, plain human)" feature
if run_dco "$r5b" "$base5b" DCO_ALLOW_GITHUB_MERGES=1; then
	fail "case 5b (unsigned plain human merge, ALLOW=1) expected non-zero exit, got 0"
fi

# ── case 6: ci.yml's PR-range dco invocation must not carry the exemption ──
# Extracts the literal `make dco ...` line ci.yml runs inside the
# `if [ -n "$PR_HEAD" ]; then ... elif` branch (the PR-range branch) and
# asserts it carries no DCO_ALLOW_GITHUB_MERGES flag — that exemption models
# push/merge_group only, where GitHub itself makes the merge (#1070). Then
# runs the same shape of call (no ALLOW flag) against a throwaway repo whose
# HEAD is an unsigned merge committed by GitHub <noreply@github.com>, as
# "Update branch" would land on a PR branch under branch protection — it
# must still fail closed.
CI_YML="$ROOT/.github/workflows/ci.yml"
pr_branch="$(awk '/if \[ -n "\$PR_HEAD" \]; then/{f=1;next} f && /elif \[ -n "\$BASE" \]; then/{exit} f' "$CI_YML")"
[ -n "$pr_branch" ] || fail "case 6: could not find ci.yml's PR-range branch (if [ -n \"\$PR_HEAD\" ]; then ... elif)"
pr_cmd="$(printf '%s\n' "$pr_branch" | grep 'make dco')"
[ -n "$pr_cmd" ] || fail "case 6: no 'make dco' invocation found in ci.yml's PR-range branch"
case "$pr_cmd" in
	*DCO_ALLOW_GITHUB_MERGES*)
		fail "case 6: ci.yml's PR-range dco invocation carries DCO_ALLOW_GITHUB_MERGES — that exemption must apply only on push/merge_group (#1070): $pr_cmd" ;;
esac

r6="$TMP/case6"
mkrepo "$r6"
base6="$(git -C "$r6" rev-parse HEAD)"
echo main1 >>"$r6/f.txt"
git -C "$r6" commit -q -am "main: signed change" -s
git -C "$r6" checkout -q -b feature
echo feat1 >"$r6/g.txt"
git -C "$r6" add g.txt
git -C "$r6" commit -q -m "feature: signed change" -s
git -C "$r6" checkout -q main
GIT_COMMITTER_NAME="GitHub" GIT_COMMITTER_EMAIL="noreply@github.com" \
	git -C "$r6" merge -q --no-ff -m "Merge pull request (unsigned, GitHub, landed on the PR branch)" feature
if run_dco "$r6" "$base6"; then
	fail "case 6 (PR-range GitHub merge, no ALLOW flag, as ci.yml runs it) expected non-zero exit, got 0"
fi

# ── case 7: a release PR that merges main; main holds a GitHub merge ───────
# main carries an unsigned merge committed as GitHub <noreply@github.com>; the
# release branch forked before it; the PR branch merges main with a signed
# merge. The DCO_RANGE is taken from ci.yml's own PR-range line, with BASE,
# PR_HEAD and origin/main replaced by this repo's shas and local ref. With
# ^origin/main the range is just the signed merge (0); without it the range
# re-judges main's GitHub merge (non-zero).
r7="$TMP/case7"
mkrepo "$r7"
git -C "$r7" branch release
echo main1 >>"$r7/f.txt"
git -C "$r7" commit -q -am "main: signed change" -s
git -C "$r7" checkout -q -b landed
echo feat1 >"$r7/g.txt"
git -C "$r7" add g.txt
git -C "$r7" commit -q -m "landed: signed change" -s
git -C "$r7" checkout -q main
GIT_COMMITTER_NAME="GitHub" GIT_COMMITTER_EMAIL="noreply@github.com" \
	git -C "$r7" merge -q --no-ff -m "Merge pull request (unsigned, GitHub, on main)" landed
git -C "$r7" checkout -q -b pr release
echo rel >"$r7/h.txt"
git -C "$r7" add h.txt
git -C "$r7" commit -q -m "release: signed change" -s
pr7_base="$(git -C "$r7" rev-parse release)"
git -C "$r7" merge -q --no-ff --no-commit main
git -C "$r7" commit -q -s --no-edit
pr7_head="$(git -C "$r7" rev-parse HEAD)"
main7="$(git -C "$r7" rev-parse main)"

range7="$(printf '%s\n' "$pr_cmd" | sed -n 's/.*DCO_RANGE="\([^"]*\)".*/\1/p')"
[ -n "$range7" ] || fail "case 7: could not read DCO_RANGE from ci.yml's PR-range line: $pr_cmd"
case "$range7" in
	*'^origin/main'*) ;;
	*) fail "case 7: ci.yml's PR range does not exclude origin/main: $range7" ;;
esac
range7="${range7//\$BASE/$pr7_base}"
range7="${range7//\$PR_HEAD/$pr7_head}"
range7="${range7//origin\/main/$main7}"
if ! (cd "$r7" && make -f "$ROOT/Makefile" dco DCO_RANGE="$range7" >/dev/null 2>&1); then
	(cd "$r7" && make -f "$ROOT/Makefile" dco DCO_RANGE="$range7" >&2) || true
	fail "case 7 (release PR merging main, range '$range7') expected exit 0, got non-zero"
fi
if (cd "$r7" && make -f "$ROOT/Makefile" dco DCO_RANGE="$pr7_base..$pr7_head" >/dev/null 2>&1); then
	fail "case 7 (same range without ^origin/main) expected non-zero exit, got 0"
fi

echo "All DCO cases behaved as expected."
