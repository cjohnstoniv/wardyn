#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Pins scripts/release-commit.sh: builds a throwaway git repo from working-tree
# copies of every path the script edits or `git add`s, then drives the real
# script from the current version to a fake next one. Asserts that every edit
# lands and the commit is signed off under the caller's identity, that a
# pre-bumped pin passes, a missing pin exits 4, a missing ROADMAP row without
# --highlights refuses before anything is edited, that --expect-tip takes any
# unique prefix, and that an X.Y.Z-rc.N target is accepted.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/scripts/release-commit.sh"
[ -x "$SCRIPT" ] || { echo "FAIL: $SCRIPT is missing or not executable"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
export GIT_AUTHOR_NAME=Release-Test GIT_AUTHOR_EMAIL=release-test@example.invalid
export GIT_COMMITTER_NAME=Release-Test GIT_COMMITTER_EMAIL=release-test@example.invalid

FILES=(internal/version/version.go deploy/helm/wardyn/Chart.yaml ui/package.json README.md install.sh
  docs/ci/github-actions.yml docs/ci/azure-pipelines.yml threatmodel/THREAT-MODEL.md
  deploy/helm/wardyn/values.yaml docs/DESKTOP.md CHANGELOG.md ROADMAP.md
  internal/db/migrations_documented_test.go)

fail=0
ok()  { echo "ok:   $1"; }
bad() { echo "FAIL: $1"; fail=1; }
expect() {  # $1=label, rest = command that must succeed
  local label=$1; shift
  if "$@"; then ok "$label"; else bad "$label"; fi
}

# Drift guard: every path the script `git add`s must be in FILES.
for p in $(grep '^git add ' "$SCRIPT" | sed 's/^git add //'); do
  printf '%s\n' "${FILES[@]}" | grep -qx "$p" || bad "script git-adds $p, which this test does not copy"
done

CUR=$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$ROOT/internal/version/version.go")
IFS=. read -r MAJ MIN PAT <<<"$CUR"
NEXT="$MAJ.$MIN.$((PAT + 1))"
[ -n "$CUR" ] || { echo "FAIL: cannot read the current version"; exit 1; }

mkrepo() {  # $1=dir; leaves a committed copy of FILES at the current version
  local d=$1 f
  mkdir -p "$d"
  for f in "${FILES[@]}"; do mkdir -p "$d/$(dirname "$f")"; cp "$ROOT/$f" "$d/$f"; done
  git -C "$d" init -q
  git -C "$d" add -A
  git -C "$d" commit -q -m base
}

# ── 1. full apply, 7-char --expect-tip ───────────────────────────────────────
R1="$WORK/r1"; mkrepo "$R1"
TIP7=$(git -C "$R1" rev-parse --short=7 HEAD)
cp "$R1/CHANGELOG.md" "$WORK/cl1.before"
out=$("$SCRIPT" --apply --tree "$R1" --from "$CUR" --to "$NEXT" --date 2030-01-02 --highlights "Fake highlights" --expect-tip "$TIP7" 2>&1); rc=$?
[ "$rc" = 0 ] && ok "apply exits 0" || { bad "apply exit $rc"; echo "$out"; }
expect "version.go bumped"     grep -qF "const Version = \"$NEXT\"" "$R1/internal/version/version.go"
expect "Chart.yaml bumped"     grep -qF "appVersion: $NEXT" "$R1/deploy/helm/wardyn/Chart.yaml"
expect "ui/package.json bumped" grep -qF "\"version\": \"$NEXT\"" "$R1/ui/package.json"
expect "README pin bumped"     grep -qF "releases/download/v$NEXT/install.sh" "$R1/README.md"
expect "install.sh pin bumped" grep -qF "releases/download/v$NEXT/install.sh" "$R1/install.sh"
expect "github-actions pin"    grep -qF "ref: v$NEXT" "$R1/docs/ci/github-actions.yml"
expect "azure-pipelines pin"   grep -qF -- "--branch v$NEXT " "$R1/docs/ci/azure-pipelines.yml"
expect "threat-model line"     grep -qF "last reviewed at v$NEXT)" "$R1/threatmodel/THREAT-MODEL.md"
expect "values.yaml comment"   grep -qF "which is $NEXT " "$R1/deploy/helm/wardyn/values.yaml"
expect "DESKTOP wardynd pin"   grep -qF "wardynd:$NEXT|" "$R1/docs/DESKTOP.md"
expect "DESKTOP proxy pin"     grep -qF "wardyn-proxy:$NEXT|" "$R1/docs/DESKTOP.md"
expect "CHANGELOG renamed"     grep -qF "## [$NEXT] — 2030-01-02" "$R1/CHANGELOG.md"
expect "CHANGELOG keeps [Unreleased]" grep -qx "## \[Unreleased\]" "$R1/CHANGELOG.md"
expect "ROADMAP row has highlights" grep -qF "| **v$NEXT** | Fake highlights | **Shipped (pre-alpha)** — \`v$NEXT\`, 2030-01-02" "$R1/ROADMAP.md"
expect "commit subject"        test "$(git -C "$R1" log -1 --format=%s)" = "release: $NEXT"
expect "commit has Signed-off-by" grep -qx 'Signed-off-by: Release-Test <release-test@example.invalid>' <<<"$(git -C "$R1" log -1 --format=%B)"
expect "commit author is the caller" test "$(git -C "$R1" log -1 --format=%an)" = Release-Test
expect "tree clean after commit" test -z "$(git -C "$R1" status --porcelain)"
released_tail() { sed -n "/^## \\[$CUR\\]/,\$p" "$1"; }
expect "released CHANGELOG sections byte-identical" cmp <(released_tail "$WORK/cl1.before") <(released_tail "$R1/CHANGELOG.md")
expect "released tail is not empty" test -n "$(released_tail "$R1/CHANGELOG.md")"

# ── 1b. a second --apply over the finished release refuses and edits nothing ─
TIP_DONE=$(git -C "$R1" rev-parse HEAD)
out=$("$SCRIPT" --apply --tree "$R1" --from "$CUR" --to "$NEXT" --date 2030-01-03 --highlights "Fake highlights" 2>&1); rc=$?
expect "second apply exits 4" test "$rc" = 4
expect "second apply names the existing section" grep -q "## \[$NEXT\] already exists" <<<"$out"
expect "second apply commits nothing" test "$(git -C "$R1" rev-parse HEAD)" = "$TIP_DONE"
expect "second apply edits nothing" test -z "$(git -C "$R1" status --porcelain)"
expect "CHANGELOG still has one [$NEXT] section" test "$(grep -c "^## \[$NEXT\]" "$R1/CHANGELOG.md")" = 1

# ── 2. 40-char --expect-tip prefix; wrong prefix refuses ─────────────────────
R2="$WORK/r2"; mkrepo "$R2"
"$SCRIPT" --dry-run --tree "$R2" --from "$CUR" --to "$NEXT" --highlights x --expect-tip "$(git -C "$R2" rev-parse HEAD)" >/dev/null 2>&1
expect "40-char --expect-tip accepted" test $? = 0
"$SCRIPT" --dry-run --tree "$R2" --from "$CUR" --to "$NEXT" --highlights x --expect-tip deadbee >/dev/null 2>&1
expect "wrong --expect-tip refused (exit 3)" test $? = 3
"$SCRIPT" --dry-run --tree "$R2" --from "$CUR" --to "$NEXT" --highlights x --expect-tip HEAD >/dev/null 2>&1
expect "--expect-tip HEAD refused (exit 2)" test $? = 2
"$SCRIPT" --dry-run --tree "$R2" --from "$CUR" --to "$NEXT" --highlights x --expect-tip "$(git -C "$R2" rev-parse HEAD | cut -c1-6)" >/dev/null 2>&1
expect "--expect-tip under 7 characters refused (exit 2)" test $? = 2

# ── 3. a pre-bumped pin passes ───────────────────────────────────────────────
R3="$WORK/r3"; mkrepo "$R3"
sed -i "s/last reviewed at v$CUR)/last reviewed at v$NEXT)/" "$R3/threatmodel/THREAT-MODEL.md"
git -C "$R3" commit -q -am prebump
out=$("$SCRIPT" --apply --tree "$R3" --from "$CUR" --to "$NEXT" --highlights x 2>&1); rc=$?
expect "pre-bumped pin: apply exits 0" test "$rc" = 0
expect "pre-bumped pin reported as already bumped" grep -q "OK (already $NEXT)" <<<"$out"
expect "pre-bumped pin left at the new version" grep -qF "last reviewed at v$NEXT)" "$R3/threatmodel/THREAT-MODEL.md"

# ── 4. a missing pin exits 4 and edits nothing ───────────────────────────────
R4="$WORK/r4"; mkrepo "$R4"
sed -i "s/last reviewed at v$CUR)/last reviewed at v0.0.1)/" "$R4/threatmodel/THREAT-MODEL.md"
git -C "$R4" commit -q -am nopin
"$SCRIPT" --apply --tree "$R4" --from "$CUR" --to "$NEXT" --highlights x >/dev/null 2>&1
expect "missing pin exits 4" test $? = 4
expect "missing pin edits nothing" test -z "$(git -C "$R4" status --porcelain)"

# ── 5. a missing ROADMAP row without --highlights refuses, edits nothing ─────
R5="$WORK/r5"; mkrepo "$R5"
"$SCRIPT" --apply --tree "$R5" --from "$CUR" --to "$NEXT" >/dev/null 2>&1
expect "missing ROADMAP row without --highlights exits 3" test $? = 3
expect "refusal edits nothing" test -z "$(git -C "$R5" status --porcelain)"
expect "refusal commits nothing" test "$(git -C "$R5" log --format=%s | wc -l)" = 1

# ── 6. an X.Y.Z-rc.N target is accepted, with a notes block ──────────────────
R6="$WORK/r6"; mkrepo "$R6"
RC="$MAJ.$MIN.$((PAT + 1))-rc.1"
printf '## [%s] — 2030-01-01\n\nRehearsal notes.\n' "$RC" > "$WORK/notes.md"
cp "$R6/CHANGELOG.md" "$WORK/cl6.before"
out=$("$SCRIPT" --apply --tree "$R6" --from "$CUR" --to "$RC" --date 2030-01-02 --highlights x --notes "$WORK/notes.md" 2>&1); rc=$?
[ "$rc" = 0 ] && ok "rc apply exits 0" || { bad "rc apply exit $rc"; echo "$out"; }
expect "rc CHANGELOG heading" grep -qF "## [$RC] — 2030-01-02" "$R6/CHANGELOG.md"
expect "rc version.go"        grep -qF "const Version = \"$RC\"" "$R6/internal/version/version.go"
expect "rc: released CHANGELOG sections byte-identical" cmp <(released_tail "$WORK/cl6.before") <(released_tail "$R6/CHANGELOG.md")
# ── 7. prefix-safe pins: one version that is a prefix of another is not "already" ─
# 7a. a pre-bumped rc pin passes as already-bumped and is not bumped twice.
R7="$WORK/r7"; mkrepo "$R7"
sed -i "s/ref: v$CUR\$/ref: v$RC/" "$R7/docs/ci/github-actions.yml"
git -C "$R7" commit -q -am prebump-rc
out=$("$SCRIPT" --apply --tree "$R7" --from "$CUR" --to "$RC" --highlights x 2>&1); rc=$?
expect "pre-bumped rc pin: apply exits 0" test "$rc" = 0
expect "pre-bumped rc pin reported as already bumped" grep -q "github-actions.yml: OK (already $RC)" <<<"$out"
expect "pre-bumped rc pin not doubled" grep -qx "          ref: v$RC" "$R7/docs/ci/github-actions.yml"
# 7b. --from CUR where the tree carries CUR-rc.1 (old is a prefix of the pin): bumped once, not doubled.
R7B="$WORK/r7b"; mkrepo "$R7B"
sed -i "s/ref: v$CUR\$/ref: v$CUR-rc.1/" "$R7B/docs/ci/github-actions.yml"
git -C "$R7B" commit -q -am leftover-rc
"$SCRIPT" --apply --tree "$R7B" --from "$CUR" --to "$NEXT" --highlights x >/dev/null 2>&1
expect "old pin that prefixes a leftover rc pin exits 4" test $? = 4
# 7c. a leftover NEXT-rc.1 pin is not "already NEXT".
R7C="$WORK/r7c"; mkrepo "$R7C"
sed -i "s/ref: v$CUR\$/ref: v$NEXT-rc.1/" "$R7C/docs/ci/github-actions.yml"
git -C "$R7C" commit -q -am leftover-next-rc
out=$("$SCRIPT" --dry-run --tree "$R7C" --from "$CUR" --to "$NEXT" --highlights x 2>&1); rc=$?
expect "leftover X.Y.Z-rc.N pin is not read as already X.Y.Z (exit 4)" test "$rc" = 4
expect "leftover rc pin is not reported as already" bash -c '! grep -q "already" <<<"$1"' _ "$out"
# 7d. v<NEXT>0 (0.8.4 vs 0.8.40) is not "already NEXT", for both anchored pins.
R7D="$WORK/r7d"; mkrepo "$R7D"
sed -i "s/ref: v$CUR\$/ref: v${NEXT}0/" "$R7D/docs/ci/github-actions.yml"
git -C "$R7D" commit -q -am longer-pin
"$SCRIPT" --dry-run --tree "$R7D" --from "$CUR" --to "$NEXT" --highlights x >/dev/null 2>&1
expect "github-actions ref v${NEXT}0 is not read as already $NEXT (exit 4)" test $? = 4
R7E="$WORK/r7e"; mkrepo "$R7E"
sed -i "s/^version: $CUR\$/version: $NEXT/; s/^appVersion: $CUR\$/appVersion: ${NEXT}0/" "$R7E/deploy/helm/wardyn/Chart.yaml"
git -C "$R7E" commit -q -am longer-chart
"$SCRIPT" --dry-run --tree "$R7E" --from "$CUR" --to "$NEXT" --highlights x >/dev/null 2>&1
expect "Chart.yaml appVersion ${NEXT}0 is not read as already $NEXT (exit 4)" test $? = 4

"$SCRIPT" --dry-run --tree "$R6" --from "$CUR" --to "1.2" --highlights x >/dev/null 2>&1
expect "malformed --to refused (exit 2)" test $? = 2

[ "$fail" = 0 ] && echo "test-release-commit: all checks passed" || echo "test-release-commit: FAILED"
exit "$fail"
