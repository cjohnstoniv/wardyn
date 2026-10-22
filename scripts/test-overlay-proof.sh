#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-overlay-proof.sh — self-test for overlay-proof.sh and a text check of
# .github/workflows/overlay-proofs.yml.
#
# The proofs run against a tiny throwaway Go module (no network, no daemon).
# Every way a revert proof can be wrong must be a FAIL: a green revert, a revert
# that does not build, a want_fail that only matches part of a line, a manifest
# whose file list is not what changed. A checkout at the wrong sha is exit 2.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/scripts/overlay-proof.sh"
WF="$ROOT/.github/workflows/overlay-proofs.yml"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fails=0
fail() { echo "FAIL: $*" >&2; fails=$((fails + 1)); }

# ── the fixture module ──────────────────────────────────────────────────────
REPO="$TMP/repo"
mkdir -p "$REPO/internal/p"
cat >"$REPO/go.mod" <<'EOF'
module fx

go 1.21
EOF
cat >"$REPO/internal/p/p.go" <<'EOF'
package p

func Add(a, b int) int { return a + b }
func Bar(a int) int    { return a * 2 }
EOF
cat >"$REPO/internal/p/p_test.go" <<'EOF'
package p

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("add")
	}
}

func TestAddBar(t *testing.T) {
	if Bar(2) != 4 {
		t.Fatal("bar")
	}
}
EOF
export GIT_CONFIG_COUNT=3 GIT_CONFIG_KEY_0=maintenance.auto GIT_CONFIG_VALUE_0=false GIT_CONFIG_KEY_1=gc.auto GIT_CONFIG_VALUE_1=0 GIT_CONFIG_KEY_2=gc.autoDetach GIT_CONFIG_VALUE_2=false
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
# Two tracked links: one to a directory outside the checkout, one into .git.
mkdir "$TMP/outside"
ln -s "$TMP/outside" "$REPO/lnk"
ln -s .git "$REPO/glnk"
git -C "$REPO" init -q
git -C "$REPO" add -A
git -C "$REPO" commit -q -m fixture
SHA="$(git -C "$REPO" rev-parse HEAD)"

addbug() { sed 's/a + b/a - b/' "$REPO/internal/p/p.go"; }
barbug() { sed 's/a \* 2/a * 3/' "$REPO/internal/p/p.go"; }
green() { { cat "$REPO/internal/p/p.go"; echo "// a comment, no behaviour change"; }; }
panics() { sed 's/return a + b/var m map[int]int; m[0] = a; return a + b/' "$REPO/internal/p/p.go"; }
nobuild() { { cat "$REPO/internal/p/p.go"; echo "func Broken( {"; }; }

# put_file <bundle> <proof> <repo path>   (content on stdin)
put_file() { mkdir -p "$1/.proofs/files/$2/$(dirname "$3")"; cat >"$1/.proofs/files/$2/$3"; }
# pj <proof> <run> <want_fail> <repo path>...  -> one manifest proof object
pj() {
  local name=$1 run=$2 want=$3 files='' f
  shift 3
  for f in "$@"; do files="$files\"$f\":\".proofs/files/$name/$f\","; done
  printf '{"name":"%s","package":"./internal/p","run":"%s","tags":"","files":{%s},"want_fail":"%s"}' "$name" "$run" "${files%,}" "$want"
}
# manifest <bundle> <sha> <proof object>...
manifest() {
  local b=$1 sha=$2 list='' p
  shift 2
  for p in "$@"; do list="$list$p,"; done
  mkdir -p "$b/.proofs"
  printf '{"keyed_sha":"%s","proofs":[%s]}\n' "$sha" "${list%,}" >"$b/.proofs/manifest.json"
}

# expect <label> <want rc> <want text in output> <cmd>...
expect() {
  local label=$1 wantrc=$2 text=$3 out rc=0
  shift 3
  out=$("$@" 2>&1) || rc=$?
  if [ "$rc" != "$wantrc" ]; then fail "$label: rc $rc, want $wantrc"; printf '%s\n' "$out" | tail -n 12 >&2; return; fi
  if ! grep -Fq -- "$text" <<<"$out"; then fail "$label: output lacks: $text"; printf '%s\n' "$out" | tail -n 12 >&2; return; fi
  echo "ok   $label"
}
clean() { [ -z "$(git -C "$REPO" status --porcelain --untracked-files=all)" ] || fail "$1: the fixture tree is dirty afterwards"; }

# ── 1. a good bundle, built by pack from a lane's overlay.json ──────────────
LANE="$TMP/lane"
mkdir -p "$LANE/overlays/add-bug"
addbug >"$LANE/overlays/add-bug/p.go"
printf '{"Replace":{"/home/x/wt-010-FAKE/internal/p/p.go":"%s"}}' "$LANE/overlays/add-bug/p.go" >"$LANE/overlays/add-bug/overlay.json"
printf 'add-bug\tTestAdd\t--- FAIL: TestAdd\n' >"$LANE/proofs.tsv"
expect "pack" 0 "packed 1 proofs" "$SCRIPT" pack "$LANE" "$SHA" --out "$TMP/packed"
expect "packed bundle PASSes" 0 "result=PASS" "$SCRIPT" run "$TMP/packed" "$REPO"
clean "packed bundle"

# ── 2. hand-built bundles ───────────────────────────────────────────────────
B="$TMP/good"; put_file "$B" add-bug internal/p/p.go < <(addbug)
manifest "$B" "$SHA" "$(pj add-bug 'TestAdd$' '--- FAIL: TestAdd' internal/p/p.go)"
expect "good bundle PASSes" 0 "PROOF add-bug rc=1 want=--- FAIL: TestAdd result=PASS reason=ok" "$SCRIPT" run "$B" "$REPO"
clean "good"

B="$TMP/green"; put_file "$B" green internal/p/p.go < <(green)
manifest "$B" "$SHA" "$(pj green 'TestAdd' '--- FAIL: TestAdd' internal/p/p.go)"
expect "green revert FAILs" 1 "result=FAIL reason=overlay green" "$SCRIPT" run "$B" "$REPO"
clean "green"

B="$TMP/nobuild"; put_file "$B" nobuild internal/p/p.go < <(nobuild)
manifest "$B" "$SHA" "$(pj nobuild 'TestAdd' '--- FAIL: TestAdd' internal/p/p.go)"
expect "build error FAILs" 1 "result=FAIL reason=build failed" "$SCRIPT" run "$B" "$REPO"
clean "nobuild"

# The revert panics inside the named test: go prints "--- FAIL: TestAdd" and then "panic:".
B="$TMP/panic"; put_file "$B" panic internal/p/p.go < <(panics)
manifest "$B" "$SHA" "$(pj panic 'TestAdd$' '--- FAIL: TestAdd' internal/p/p.go)"
expect "a panic in the named test FAILs" 1 "result=FAIL reason=panic" "$SCRIPT" run "$B" "$REPO"
clean "panic"

# The named test is skipped while another test in the run fails, so rc is 1.
skiptest() { printf 'package p\n\nimport "testing"\n\nfunc TestAdd(t *testing.T)    { t.Skip("skipped") }\nfunc TestAddBar(t *testing.T) { t.Fatal("bar") }\n'; }
B="$TMP/skip"; put_file "$B" skip internal/p/p_test.go < <(skiptest)
manifest "$B" "$SHA" "$(pj skip 'TestAdd' '--- FAIL: TestAdd' internal/p/p_test.go)"
expect "a skipped named test FAILs" 1 "result=FAIL reason=skipped" "$SCRIPT" run "$B" "$REPO"
clean "skip"

# The -run regex matches nothing.
B="$TMP/notests"; put_file "$B" notests internal/p/p.go < <(addbug)
manifest "$B" "$SHA" "$(pj notests 'TestNoSuchTest' '--- FAIL: TestNoSuchTest' internal/p/p.go)"
expect "no tests to run FAILs" 1 "result=FAIL reason=no tests to run" "$SCRIPT" run "$B" "$REPO"
clean "notests"

# A target that resolves through a tracked link, outside the checkout or into .git.
cp "$REPO/.git/config" "$TMP/gitconfig.before"
B="$TMP/lnk"; put_file "$B" lnk lnk/p.go < <(addbug)
manifest "$B" "$SHA" "$(pj lnk 'TestAdd' '--- FAIL: TestAdd' lnk/p.go)"
expect "a link out of the repo FAILs" 1 "reason=path lnk/p.go resolves outside the repo" "$SCRIPT" run "$B" "$REPO"
[ ! -e "$TMP/outside/p.go" ] || fail "a file was written outside the checkout"
B="$TMP/glnk"; put_file "$B" glnk glnk/config < <(addbug)
manifest "$B" "$SHA" "$(pj glnk 'TestAdd' '--- FAIL: TestAdd' glnk/config)"
expect "a link into .git FAILs" 1 "reason=path glnk/config resolves into .git" "$SCRIPT" run "$B" "$REPO"
cmp -s "$REPO/.git/config" "$TMP/gitconfig.before" || fail "a file in .git was written"
clean "links"

# The path normaliser used for the manifest and for git's changed names.
for bad in './x' 'a/../b' '.git/config' 'x/.git/y' '/abs' 'a//b' 'a/./b'; do
  out=0; printf 'internal/p/p.go\0%s\0' "$bad" | "$SCRIPT" paths >/dev/null 2>&1 || out=$?
  [ "$out" = 2 ] || fail "paths accepted $bad (rc $out)"
done
[ "$(printf 'b/y\0a/x\0b/y\0' | "$SCRIPT" paths)" = $'a/x\nb/y' ] || fail "paths did not sort and dedupe"
echo "ok   path normaliser"

# TestAddBar fails; "--- FAIL: TestAdd" is only a prefix of that line.
B="$TMP/substr"; put_file "$B" substr internal/p/p.go < <(barbug)
manifest "$B" "$SHA" "$(pj substr 'TestAdd' '--- FAIL: TestAdd' internal/p/p.go)"
expect "substring want_fail FAILs" 1 "result=FAIL reason=no output line equals the want_fail" "$SCRIPT" run "$B" "$REPO"
clean "substring"
manifest "$B" "$SHA" "$(pj substr 'TestAdd' '--- FAIL: TestAddBar' internal/p/p.go)"
expect "the full name PASSes" 0 "result=PASS" "$SCRIPT" run "$B" "$REPO"

# The manifest names p.go and p_test.go; only p.go differs from the tree.
B="$TMP/list"; put_file "$B" list internal/p/p.go < <(addbug); put_file "$B" list internal/p/p_test.go < "$REPO/internal/p/p_test.go"
manifest "$B" "$SHA" "$(pj list 'TestAdd' '--- FAIL: TestAdd' internal/p/p.go internal/p/p_test.go)"
expect "manifest list differs from git diff" 1 "reason=changed files differ from the manifest" "$SCRIPT" run "$B" "$REPO"
clean "list"
# The bundle file equals the tree: nothing is reverted.
B="$TMP/nodiff"; put_file "$B" nodiff internal/p/p.go < "$REPO/internal/p/p.go"
manifest "$B" "$SHA" "$(pj nodiff 'TestAdd' '--- FAIL: TestAdd' internal/p/p.go)"
expect "a no-op revert FAILs" 1 "reason=changed files differ from the manifest" "$SCRIPT" run "$B" "$REPO"

# Manifest paths that name .git, or spell a path another way, are refused before anything is copied.
for bad in './.git/config' '.git/config' 'internal/p/.git/hooks/x' 'internal/./p/p.go' 'internal/p/../p/p.go' '/etc/passwd' 'internal//p/p.go' '.GIT/config'; do
  B="$TMP/badpath"; rm -rf "$B"; put_file "$B" bad internal/p/p.go < <(addbug)
  printf '{"keyed_sha":"%s","proofs":[{"name":"bad","package":"./internal/p","run":"TestAdd","tags":"","files":{"%s":".proofs/files/bad/internal/p/p.go"},"want_fail":"--- FAIL: TestAdd"}]}\n' "$SHA" "$bad" >"$B/.proofs/manifest.json"
  expect "manifest path $bad is refused" 2 "unsafe path" "$SCRIPT" run "$B" "$REPO"
  clean "badpath $bad"
done

# ── 3. sequential, each in a clean tree; --only; the sha key ────────────────
B="$TMP/seq"
put_file "$B" first internal/p/p.go < <(addbug); put_file "$B" mid internal/p/p.go < <(green); put_file "$B" last internal/p/p.go < <(addbug)
manifest "$B" "$SHA" "$(pj first 'TestAdd$' '--- FAIL: TestAdd' internal/p/p.go)" "$(pj mid 'TestAdd$' '--- FAIL: TestAdd' internal/p/p.go)" "$(pj last 'TestAdd$' '--- FAIL: TestAdd' internal/p/p.go)"
if out="$("$SCRIPT" run "$B" "$REPO" 2>&1)"; then fail "sequence: exit 0 with a failing proof"; fi
for want in 'PROOF first rc=1 .*result=PASS' 'PROOF mid rc=0 .*result=FAIL reason=overlay green' 'PROOF last rc=1 .*result=PASS'; do
  grep -Eq "$want" <<<"$out" || fail "sequence: no line matches $want"
done
clean "sequence"
echo "ok   sequential proofs, each in a clean tree"
expect "--only runs one" 0 "1 proofs" "$SCRIPT" run "$B" "$REPO" --only last
expect "--only an unknown name" 2 "no such proof" "$SCRIPT" run "$B" "$REPO" --only nope

manifest "$B" "0000000000000000000000000000000000000000" "$(pj first 'TestAdd$' '--- FAIL: TestAdd' internal/p/p.go)"
expect "wrong keyed_sha is exit 2" 2 "is keyed to" "$SCRIPT" run "$B" "$REPO"
clean "wrong sha"

# ── 4. plan groups by package and sizes the timeout ─────────────────────────
manifest "$B" "$SHA" "$(pj first 'TestAdd$' '--- FAIL: TestAdd' internal/p/p.go)" "$(pj last 'TestAdd$' '--- FAIL: TestAdd' internal/p/p.go)"
expect "plan" 0 '"only":"first last","n":2' "$SCRIPT" plan "$B" "$SHA"
expect "plan with the wrong sha" 2 "is not" "$SCRIPT" plan "$B" "1111111111111111111111111111111111111111"

# ── 5. the workflow file ────────────────────────────────────────────────────
if [ ! -f "$WF" ]; then
  fail "missing $WF"
else
  triggers="$(grep -E '^[[:space:]]*(push|pull_request|pull_request_target|schedule|workflow_run|workflow_call|merge_group|repository_dispatch):' "$WF" || true)"
  [ -z "$triggers" ] || fail "workflow: a trigger besides workflow_dispatch: $triggers"
  grep -Eq '^  workflow_dispatch:' "$WF" || fail "workflow: no workflow_dispatch trigger"
  [ "$(awk '/^on:/{f=1;next} /^[^ #]/{f=0} f && /^  [a-z_]+:/{n++} END{print n+0}' "$WF")" = 1 ] || fail "workflow: on: has more than one trigger"
  perm="$(grep -A1 '^permissions:' "$WF" || true)"
  [ "$perm" = $'permissions:\n  contents: read' ] || fail "workflow: top-level permissions are not exactly contents: read"
  [ "$(grep -cE '^[[:space:]]+permissions:' "$WF")" = 0 ] || fail "workflow: a job sets its own permissions"
  ! grep -Eq ':[[:space:]]*(write|admin)' "$WF" || fail "workflow: a write permission"
  ! grep -Fq 'secrets.' "$WF" || fail "workflow: reads a secret"
  ! grep -Fq 'id-token' "$WF" || fail "workflow: id-token"
  ! grep -Eq 'actions/cache@|actions/cache/save' "$WF" || fail "workflow: saves a cache (restore only)"
  grep -Fq 'actions/cache/restore@' "$WF" || fail "workflow: no cache restore step"
  unpinned="$(grep -E '^[[:space:]]*(-[[:space:]]+)?uses:' "$WF" | grep -Ev '@[0-9a-f]{40}( |$)' || true)"
  [ -z "$unpinned" ] || fail "workflow: a uses: not pinned by full sha: $unpinned"
  mp="$(grep -E '^[[:space:]]*max-parallel:' "$WF" | grep -Eo '[0-9]+' || true)"
  { [ -n "$mp" ] && [ "$mp" -le 4 ]; } || fail "workflow: max-parallel '$mp' is missing or over 4"
  jobs="$(awk '/^jobs:/{f=1;next} f && /^  [A-Za-z0-9_-]+:[[:space:]]*$/{n++} END{print n+0}' "$WF")"
  timeouts="$(grep -cE '^    timeout-minutes:' "$WF" || true)"
  [ "$jobs" -ge 2 ] || fail "workflow: only $jobs jobs"
  [ "$timeouts" = "$jobs" ] || fail "workflow: $jobs jobs but $timeouts timeout-minutes"
  gated="$(grep -cF "    if: github.ref == 'refs/heads/main'" "$WF" || true)"
  [ "$gated" = "$jobs" ] || fail "workflow: $gated of $jobs jobs are gated on github.ref == refs/heads/main"
  grep -Fq 'never dispatch while a train attempt 1 is queued' "$WF" || fail "workflow: the throttle comment is missing"
  echo "ok   workflow text checks"
fi

[ "$fails" = 0 ] || { echo "test-overlay-proof.sh: $fails failures" >&2; exit 1; }
echo "test-overlay-proof.sh: all checks passed"
