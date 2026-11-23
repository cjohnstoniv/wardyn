#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Tests the clone state file (dispatch_repo_clones in
# deploy/images/common/agent-run-lib.sh) as agent-run writes it: a bash under
# `set -euo pipefail` that sourced the lib. No Docker or network: the
# repositories are local file:// ones, and the state goes to a temp file through
# WARDYN_CLONE_STATE_FILE. Each case reads the v1 JSON wardynd reads and checks
# that the stderr lines are still printed.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="$ROOT/deploy/images/common/agent-run-lib.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export HOME="$tmp/home" GIT_CONFIG_GLOBAL="$tmp/gitconfig" GIT_CONFIG_NOSYSTEM=1
unset WARDYN_RUN_ID WARDYN_REPO_URL
mkdir -p "$HOME"

fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

# run_clones <name> <repos> — dispatch_repo_clones over <repos> in a fresh bash;
# the state lands in $tmp/<name>.json and the stderr in $tmp/<name>.log.
run_clones() {
    local name="$1"
    rm -f "$tmp/$name.json"
    WARDYN_CLONE_STATE_FILE="$tmp/$name.json" WARDYN_REPOS="$2" workdir="$tmp" \
        bash -c 'set -euo pipefail; . "$1"; dispatch_repo_clones' _ "$LIB" 2>"$tmp/$name.log" || bad "$name: dispatch_repo_clones exited non-zero"
}

# repos_are <name> <json> — the state file is done:true with exactly these repos (dest, outcome, cause), in order.
repos_are() {
    python3 -I -c '
import json, sys
st = json.load(open(sys.argv[1]))
want = json.loads(sys.argv[2])
got = [[r["dest"], r["outcome"], r["cause"]] for r in st["repos"]]
sys.exit(0 if st["v"] == 1 and st["done"] is True and got == want else 1)
' "$tmp/$1.json" "$2"
}

# A bare repository with one commit, for the clone that succeeds.
git init -q --bare "$tmp/good.git"
git init -q "$tmp/seed"
echo hi > "$tmp/seed/file"
git -C "$tmp/seed" add file
git -C "$tmp/seed" -c user.name=t -c user.email=t@example.invalid commit -q -m seed
git -C "$tmp/seed" push -q "file://$tmp/good.git" HEAD:refs/heads/main

held="$tmp/held" missing="$tmp/missing" good="$tmp/good"
mkdir -p "$held"
echo keep > "$held/mine"
tab=$'\t'
repos="file://$tmp/held.git${tab}$held${tab}org/held${tab}"$'\n'"file://$tmp/nope.git${tab}$missing${tab}org/nope${tab}"$'\n'"file://$tmp/good.git${tab}$good${tab}org/good${tab}"

# 1-3: a target holding a file, a repository that does not exist, one that clones.
run_clones three "$repos"
if repos_are three "[[\"$held\",\"skipped\",\"not_a_checkout\"],[\"$missing\",\"failed\",\"clone_failed\"],[\"$good\",\"ok\",\"\"]]"; then
    ok "three clones: skipped, failed and ok are in the state file"
else
    bad "three clones: state file $(cat "$tmp/three.json" 2>/dev/null)"
fi
# The stderr lines are word for word what they were.
for want in "agent-run: clone SKIPPED for $held — it already holds files that are not a checkout of org/held" \
    "agent-run: clone FAILED for $missing — the egress policy or credential broker may have blocked it" \
    "agent-run: clone OK ($good)"; do
    if grep -qF -- "$want" "$tmp/three.log"; then ok "stderr: ${want%% —*}"; else bad "stderr line missing: $want"; fi
done
[[ -f "$held/mine" ]] && ok "the held folder was left as it was" || bad "the held folder lost its file"

# 4: a dest holding a quote and a backslash still gives valid JSON, and the dest reads back.
odd="$tmp/we\"ird\\dir"
mkdir -p "$odd"
echo keep > "$odd/mine"
run_clones odd "file://$tmp/held.git${tab}$odd${tab}org/odd${tab}"
if python3 -I -c '
import json, sys
st = json.load(open(sys.argv[1]))
sys.exit(0 if [r["dest"] for r in st["repos"]] == [sys.argv[2]] and st["repos"][0]["outcome"] == "skipped" else 1)
' "$tmp/odd.json" "$odd"; then ok "a dest with a quote and a backslash is valid JSON"; else bad "odd dest: $(cat "$tmp/odd.json" 2>/dev/null)"; fi

# 5: the in-progress marker alone, and a run with nothing to clone (done, no repos).
(WARDYN_CLONE_STATE_FILE="$tmp/marker.json"; export WARDYN_CLONE_STATE_FILE; . "$LIB"; _clone_state_write false)
if [[ "$(cat "$tmp/marker.json")" == '{"v":1,"done":false,"repos":[]}' ]]; then ok "the marker alone"; else bad "marker: $(cat "$tmp/marker.json" 2>/dev/null)"; fi
run_clones none ""
if [[ "$(cat "$tmp/none.json")" == '{"v":1,"done":true,"repos":[]}' ]]; then ok "nothing to clone writes done with no repos"; else bad "none: $(cat "$tmp/none.json" 2>/dev/null)"; fi

echo "--- test-clone-state: $([[ $fail -eq 0 ]] && echo PASS || echo FAIL) ---"
exit "$fail"
