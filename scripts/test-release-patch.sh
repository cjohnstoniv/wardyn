#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Pins scripts/release-patch.sh. Every case builds a throwaway repo (a bare
# "origin" plus a clone) that carries a copy of the real script next to STUB
# release-commit / green-by-tree / verify-release / claims scripts, then runs the
# command with a PATH-shim `gh` (canned answers from a fixture dir, every call
# logged) and a PATH-shim `git` that logs each `git push` before running the real
# one against the local bare origin. Nothing leaves the machine. The cases:
#   A. a full run: release commit, checks, branch + PR + nightly, wait, publish
#      (branch fast-forward, tag, run watch, release edit, verify), the table
#   B. a re-run of A repeats nothing it already did
#   C. refuses a dirty tree        D. refuses V <= the newest vX.Y.* tag
#   E. refuses a BRANCH that lacks origin/release/X.Y (DRY_RUN only warns)
#   F. DRY_RUN=1: no release commit, PR, release/* push, tag or Release; the
#      nightly dispatch and a promote dry-run dispatch run, and the table prints
#   G. refuses publish when green-by-tree exits 1 (and 2)   H. release/X.Y moved
#   I. the wait loop keeps waiting while a ci/nightly run is active
#   J. the wait loop stops on a red run and PRINTS the rerun command, never runs it
#   K-T. gh auth, release-commit refusal, numeric tag order, a missing draft,
#      verify failure, an oversized BODY, bad ISSUES / PHASE, the script text
# and, over every push any case made: no --force, no `+` refspec.
# Daemon-free, network-free.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/scripts/release-patch.sh"
[ -x "$SCRIPT" ] || { echo "FAIL: $SCRIPT is missing or not executable"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
REAL_GIT="$(command -v git)"
g() { "$REAL_GIT" "$@"; }
mkdir -p "$WORK/bin"

export GIT_AUTHOR_NAME=Patch-Test GIT_AUTHOR_EMAIL=patch-test@example.invalid
export GIT_COMMITTER_NAME=Patch-Test GIT_COMMITTER_EMAIL=patch-test@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_TERMINAL_PROMPT=0

# ── shims ────────────────────────────────────────────────────────────────────
cat >"$WORK/bin/git" <<SHIM
#!/usr/bin/env bash
[ "\${1:-}" = push ] && echo "\$*" >>"\$FIX/push.log"
exec "$REAL_GIT" "\$@"
SHIM

# gh: every call is appended to \$FIX/gh.log. Answers come from files in \$FIX.
cat >"$WORK/bin/gh" <<'SHIM'
#!/usr/bin/env bash
echo "$*" >>"$FIX/gh.log"
args=("$@"); jqexpr=""; wf=""; status=""; bodyfile=""; notesfile=""
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in
    --jq|-q) jqexpr="${args[i+1]}" ;;
    --workflow) wf="${args[i+1]}" ;;
    --status) status="${args[i+1]}" ;;
    --body-file) bodyfile="${args[i+1]}" ;;
    --notes-file) notesfile="${args[i+1]}" ;;
  esac
done
emit() {  # emit <fixture file> <default body>
  local body
  body=$(cat "$FIX/$1" 2>/dev/null) || body="$2"
  if [ -n "$jqexpr" ]; then printf '%s' "$body" | jq -r "$jqexpr"; else printf '%s\n' "$body"; fi
}
rc_of() { cat "$FIX/$1" 2>/dev/null || echo 0; }
case "$1 ${2:-}" in
  "auth status") exit "$(rc_of auth.rc)" ;;
  "repo view") echo acme/wardyn ;;
  "pr list") emit pr_list.json '[]' ;;
  "pr create")
    cp "$bodyfile" "$FIX/pr_body.txt"
    echo '[{"number":7}]' >"$FIX/pr_list.json"
    echo https://github.com/acme/wardyn/pull/7 ;;
  "workflow run")
    if [ "$3" = release.yml ]; then
      printf '[{"databaseId":950,"headBranch":"dry","status":"queued","event":"workflow_dispatch"}]' >"$FIX/run_list_release.yml.json"
    fi ;;
  "run list")
    if [ -n "$status" ]; then emit "run_list_$status.json" '[]'; else emit "run_list_$wf.json" '[]'; fi ;;
  "run watch") exit "$(rc_of watch.rc)" ;;
  "run view") emit "run_view_$3.json" '{"jobs":[]}' ;;
  "run rerun") echo "UNEXPECTED rerun" >&2; exit 0 ;;
  "release view")
    [ -f "$FIX/release_view.json" ] || { echo "release not found" >&2; exit 1; }
    emit release_view.json '{}' ;;
  "release edit")
    rc=$(rc_of release_edit.rc)
    [ "$rc" = 0 ] || exit "$rc"
    [ -n "$notesfile" ] && cp "$notesfile" "$FIX/release_body.txt"
    echo '{"isDraft":false}' >"$FIX/release_view.json" ;;
  "api "*)
    shift
    ep=""
    while [ $# -gt 0 ]; do
      case "$1" in -X|-f|-F|--jq|-q) shift 2 ;; --*) shift ;; *) ep="$1"; shift ;; esac
    done
    key="api_$(printf '%s' "$ep" | tr '/' '_').json"
    case "$ep" in
      */nightly.yml/runs) emit "$key" '{"workflow_runs":[]}' ;;
      */releases) emit "$key" '[]' ;;
      */actions/runs/*/jobs) [ -f "$FIX/$key" ] && emit "$key" '{}' || { echo "gh: no fixture for $ep (HTTP 404)" >&2; exit 1; } ;;
      */actions/runs/*) emit "$key" "{\"updated_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" ;;
      *) [ -f "$FIX/$key" ] && emit "$key" '{}' || { echo "gh: no fixture for $ep (HTTP 404)" >&2; exit 1; } ;;
    esac ;;
  *) echo "gh shim: unhandled: $*" >&2; exit 1 ;;
esac
SHIM

cat >"$WORK/bin/go" <<'SHIM'
#!/usr/bin/env bash
echo "$*" >>"$FIX/go.log"
exit "$(cat "$FIX/go.rc" 2>/dev/null || echo 0)"
SHIM
chmod +x "$WORK/bin/git" "$WORK/bin/gh" "$WORK/bin/go"

# ── stubs that live inside each fixture repo ─────────────────────────────────
write_stubs() {  # run inside the fixture work tree
  cat >scripts/release-commit.sh <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$FIX/rc.log"
case "$1" in
  --dry-run) exit "$(cat "$FIX/rc_dry.rc" 2>/dev/null || echo 0)" ;;
  --apply) [ -f "$FIX/rc.rc" ] && exit "$(cat "$FIX/rc.rc")" ;;
esac
to=""; while [ $# -gt 0 ]; do [ "$1" = --to ] && to="$2"; shift; done
printf '\n## [%s] — 2030-01-02\n\n- notes for %s\n' "$to" "$to" >>CHANGELOG.md
git add CHANGELOG.md
git commit -q -s -m "release: $to"
STUB
  cat >scripts/green-by-tree.sh <<'STUB'
#!/usr/bin/env bash
echo "$1 WATCHED=[${WATCHED:-}] NEED_STAGING=${NEED_STAGING:-} REPO=${GITHUB_REPOSITORY:-}" >>"$FIX/green.log"
if [ -s "$FIX/green.seq" ]; then rc=$(head -1 "$FIX/green.seq"); sed -i 1d "$FIX/green.seq"
else rc=$(cat "$FIX/green.rc" 2>/dev/null || echo 0); fi
echo "green-by-tree: stub verdict $rc" >&2
[ "$rc" = 0 ] && printf 'tree=t\nci_run=1\nnightly_run=2\nnightly_sha=n\n'
exit "$rc"
STUB
  cat >scripts/verify-release.sh <<'STUB'
#!/usr/bin/env bash
echo "V=${V:-}" >>"$FIX/verify.log"
cat "$FIX/verify.out" 2>/dev/null || echo "fails=0"
exit "$(cat "$FIX/verify.rc" 2>/dev/null || echo 0)"
STUB
  cat >scripts/test-claims-match-code.sh <<'STUB'
#!/usr/bin/env bash
echo run >>"$FIX/claims.log"
exit "$(cat "$FIX/claims.rc" 2>/dev/null || echo 0)"
STUB
  chmod +x scripts/*.sh
  printf 'dco:\n\t@echo "$(DCO_RANGE)" >>"$(FIX)/dco.log"\n' >Makefile
  printf 'name: release\n# a\n          watched="w1 w2 w3"\n' >.github/workflows/release.yml
  printf '# Changelog\n\n## [Unreleased]\n\n## [0.8.3] — 2030-01-01\n\n- old\n' >CHANGELOG.md
}

mkfix() {  # mkfix <name>: sets FIX WK OR; origin has main, release/0.8 (+tags v0.8.2, v0.8.3)
  local d="$WORK/$1"
  FIX="$d/fix"; WK="$d/work"; OR="$d/origin.git"; export FIX
  mkdir -p "$FIX"
  g init -q --bare -b main "$OR"
  g init -q -b main "$WK"
  (
    cd "$WK" || exit 1
    mkdir -p scripts .github/workflows
    cp "$SCRIPT" scripts/release-patch.sh
    write_stubs
    g add -A && g commit -q -s -m base
    g remote add origin "$OR"
    g push -q origin main
    g checkout -q -b release/0.8
    echo rel >RELONLY && g add RELONLY && g commit -q -s -m "release-line only"
    g tag v0.8.2 main
    g tag -a v0.8.3 -m "Wardyn v0.8.3"
    g push -q origin release/0.8 --tags
    g checkout -q main
    echo m >MAINWORK && g add MAINWORK && g commit -q -s -m "main work"
    g push -q origin main
    g fetch -q origin --tags
  ) || { echo "FAIL: fixture $1 did not build"; exit 1; }
  # the Release draft that release-assets would have made, and the tag's run
  echo '{"isDraft":true}' >"$FIX/release_view.json"
  printf '[{"databaseId":901,"headBranch":"v0.8.4","status":"completed","event":"push"}]' >"$FIX/run_list_release.yml.json"
}

# rp <ENV=VAL>...: run the command in the fixture work tree; leaves RC and the
# combined output in $FIX/out.txt
rp() {
  # GITHUB_REPOSITORY is pinned: on a hosted runner it names the real repository, and the
  # script (rightly) honours it, which would miss every canned acme/wardyn answer.
  (cd "$WK" && env -u GH_TOKEN -u GITHUB_TOKEN GITHUB_REPOSITORY=acme/wardyn PATH="$WORK/bin:$PATH" WAIT_INTERVAL=0 RUN_POLL_INTERVAL=0 "$@" ./scripts/release-patch.sh) >"$FIX/out.txt" 2>&1
  RC=$?
}
lines() { [ -f "$1" ] && wc -l <"$1" | tr -d ' ' || echo 0; }

fail=0
ok()  { echo "ok:   $1"; }
bad() { echo "FAIL: $1"; fail=1; }
check() { local label=$1; shift; if "$@"; then ok "$label"; else bad "$label"; fi; }
out_has() { grep -qE -- "$1" "$FIX/out.txt"; }
log_has() { grep -qE -- "$2" "$FIX/$1" 2>/dev/null; }
log_lacks() { ! grep -qE -- "$2" "$FIX/$1" 2>/dev/null; }
refused() {  # refused <label> <output regexp>: RC != 0 and nothing was pushed or dispatched
  local label=$1 re=$2
  if [ "$RC" != 0 ] && out_has "$re" && [ "$(lines "$FIX/push.log")" = 0 ] && log_lacks gh.log 'workflow run|pr create|release edit'; then
    ok "$label"
  else bad "$label (rc=$RC)"; sed 's/^/      /' "$FIX/out.txt"; fi
}
ALLPUSH="$WORK/allpush.log"; : >"$ALLPUSH"
keep_pushes() { [ -f "$FIX/push.log" ] && cat "$FIX/push.log" >>"$ALLPUSH"; return 0; }

# ── A. a full run ────────────────────────────────────────────────────────────
mkfix A
printf 'Release notes paste\n' >"$WORK/notes.md"
rp V=0.8.4 MERGE=origin/main ISSUES="12 15" HIGHLIGHTS="Fixes things" NOTES="$WORK/notes.md"
[ "$RC" = 0 ] && ok "A: full run exits 0" || { bad "A: full run exit $RC"; sed 's/^/      /' "$FIX/out.txt"; }
TIP=$(g -C "$OR" rev-parse refs/heads/chore/release-0.8.4 2>/dev/null)
check "A: release-commit --apply with FROM, TO, notes, highlights" \
  log_has rc.log "^--apply --tree \. --from 0\.8\.3 --to 0\.8\.4 --notes $WORK/notes.md --highlights Fixes things\$"
check "A: main merged --no-ff with a Signed-off-by" \
  bash -c "g() { '$REAL_GIT' \"\$@\"; }; g -C '$OR' log --merges --format=%B refs/heads/chore/release-0.8.4 | grep -q '^Signed-off-by: '"
check "A: release commit sits on the candidate" \
  bash -c "'$REAL_GIT' -C '$OR' log -1 --format=%s refs/heads/chore/release-0.8.4 | grep -qx 'release: 0.8.4'"
check "A: make dco ran over the release's own range" log_has dco.log '^HEAD \^origin/main \^origin/release/0\.8$'
check "A: version go test ran" log_has go.log "^test -count=1 \./cmd/wardyn/ -run TestVersionMatchesChangelog\|TestShippedVersionStringsAgree\$"
check "A: claims script ran" log_has claims.log '^run$'
check "A: PR into release/0.8 titled release: 0.8.4" log_has gh.log '^pr create --base release/0\.8 --head chore/release-0\.8\.4 --title release: 0\.8\.4 '
check "A: PR body: one Closes per line" bash -c "grep -cx 'Closes #12' '$FIX/pr_body.txt' | grep -qx 1 && grep -cx 'Closes #15' '$FIX/pr_body.txt' | grep -qx 1 && [ \"\$(grep -c 'Closes' '$FIX/pr_body.txt')\" = 2 ]"
check "A: nightly dispatched on the branch" log_has gh.log '^workflow run nightly\.yml --ref chore/release-0\.8\.4$'
check "A: pushed the candidate branch" log_has push.log '^push origin HEAD:refs/heads/chore/release-0\.8\.4$'
check "A: fast-forwarded release/0.8 to the candidate" bash -c "[ -n '$TIP' ] && [ \"\$('$REAL_GIT' -C '$OR' rev-parse refs/heads/release/0.8)\" = '$TIP' ]"
check "A: pushed the tag, and it peels to the candidate" bash -c "[ \"\$('$REAL_GIT' -C '$OR' rev-parse 'refs/tags/v0.8.4^{commit}')\" = '$TIP' ]"
check "A: green-by-tree got NEED_STAGING=1, the watched list, the repo" log_has green.log 'WATCHED=\[w1 w2 w3\] NEED_STAGING=1 REPO=acme/wardyn$'
check "A: green-by-tree ran in wait and again in publish" bash -c "[ \"\$(grep -c . '$FIX/green.log')\" -ge 2 ]"
check "A: watched the tag's release.yml run" log_has gh.log '^run watch 901 --exit-status$'
check "A: published the draft as a pre-release" log_has gh.log '^release edit v0\.8\.4 --draft=false --prerelease --title Wardyn v0\.8\.4 --notes-file '
check "A: release body is the new CHANGELOG section only" \
  bash -c "grep -q 'notes for 0.8.4' '$FIX/release_body.txt' && ! grep -q '0.8.3' '$FIX/release_body.txt'"
check "A: release edit came after the run watch" bash -c "[ \$(grep -n '^run watch 901' '$FIX/gh.log' | cut -d: -f1 | head -1) -lt \$(grep -n '^release edit' '$FIX/gh.log' | cut -d: -f1 | head -1) ]"
check "A: verify-release ran for 0.8.4" log_has verify.log '^V=0\.8\.4$'
check "A: prints the table with the total from T0" out_has 'total from T0'
check "A: prints the forward-port commands without running them" out_has 'forward-port'
check "A: no push to main" log_lacks push.log 'refs/heads/main'
keep_pushes

# ── B. a re-run does nothing twice ───────────────────────────────────────────
TREE=$(g -C "$WK" rev-parse 'HEAD^{tree}')
printf '{"workflow_runs":[{"id":9,"event":"workflow_dispatch","status":"in_progress","conclusion":null,"head_commit":{"tree_id":"%s"}}]}' "$TREE" \
  >"$FIX/api_repos_acme_wardyn_actions_workflows_nightly.yml_runs.json"
P0=$(lines "$FIX/push.log"); R0=$(lines "$FIX/rc.log"); N0=$(grep -c '^pr create' "$FIX/gh.log"); E0=$(grep -c '^release edit' "$FIX/gh.log"); W0=$(grep -c '^workflow run' "$FIX/gh.log")
rp V=0.8.4 MERGE=origin/main ISSUES="12 15" HIGHLIGHTS="Fixes things" NOTES="$WORK/notes.md"
[ "$RC" = 0 ] && ok "B: re-run exits 0" || { bad "B: re-run exit $RC"; sed 's/^/      /' "$FIX/out.txt"; }
check "B: no new push" bash -c "[ \"\$(wc -l <'$FIX/push.log')\" = $P0 ]"
check "B: release commit not made again" bash -c "[ \"\$(wc -l <'$FIX/rc.log')\" = $R0 ]"
check "B: no second PR" bash -c "[ \"\$(grep -c '^pr create' '$FIX/gh.log')\" = $N0 ]"
check "B: no second release edit" bash -c "[ \"\$(grep -c '^release edit' '$FIX/gh.log')\" = $E0 ]"
check "B: no second workflow dispatch" bash -c "[ \"\$(grep -c '^workflow run' '$FIX/gh.log')\" = $W0 ]"
check "B: says what it skipped" out_has '[Ss]kip'
keep_pushes

# ── C. dirty tree ────────────────────────────────────────────────────────────
mkfix C
echo scratch >"$WK/dirty.txt"
rp V=0.8.4
refused "C: refuses a dirty worktree" 'not clean'

# ── D. V must exceed the newest vX.Y.* tag ───────────────────────────────────
mkfix D
for v in 0.8.3 0.8.2 0.8.3-rc.1; do
  rp V=$v
  refused "D: refuses V=$v" 'greater'
done
g -C "$WK" tag v0.8.4 main   # a tag that is not the candidate's
rp V=0.8.4
refused "D: refuses V whose tag exists elsewhere" 'exists'
rp V=0.8
refused "D: refuses a malformed V" 'X\.Y\.Z'
rp V=
refused "D: refuses a missing V" 'X\.Y\.Z'

# ── E. BRANCH must contain origin/release/X.Y ────────────────────────────────
mkfix E
g -C "$WK" branch --no-track feat-x origin/main
rp V=0.8.4 BRANCH=feat-x
refused "E: refuses a BRANCH without origin/release/0.8" 'does not contain'
check "E: no release commit attempted" log_lacks rc.log '.'
rp V=0.8.4 BRANCH=release/0.8
refused "E: refuses BRANCH=release/0.8 itself" 'release/'

# ── F. DRY_RUN ───────────────────────────────────────────────────────────────
mkfix F
g -C "$WK" branch --no-track feat-x origin/main
echo 3 >"$FIX/rc_dry.rc"
rp V=0.8.4 BRANCH=feat-x DRY_RUN=1 HIGHLIGHTS=x
[ "$RC" = 0 ] && ok "F: dry run exits 0" || { bad "F: dry run exit $RC"; sed 's/^/      /' "$FIX/out.txt"; }
check "F: no push to refs/heads/release/*" log_lacks push.log 'refs/heads/release/'
check "F: no push of a tag" log_lacks push.log 'refs/tags/|v0\.8\.4'
check "F: no release commit (only a --dry-run)" bash -c "grep -q '^--dry-run ' '$FIX/rc.log' && ! grep -q -- '--apply' '$FIX/rc.log'"
check "F: a refused dry-run release commit only warns" out_has 'would be refused'
check "F: no PR" log_lacks gh.log '^pr create'
check "F: no Release edit" log_lacks gh.log '^release edit'
check "F: nightly dispatched as usual" log_has gh.log '^workflow run nightly\.yml --ref feat-x$'
check "F: promote dry-run dispatched" log_has gh.log '^workflow run release\.yml --ref feat-x -f path=promote -f dry_run=true$'
check "F: watched the dry run" log_has gh.log '^run watch 950 --exit-status$'
check "F: no verify-release" log_lacks verify.log '.'
check "F: prints the table" out_has 'total from T0'
check "F: origin has no release/0.8 change or tag v0.8.4" bash -c "[ -z \"\$('$REAL_GIT' -C '$OR' tag -l v0.8.4)\" ] && [ \"\$('$REAL_GIT' -C '$OR' rev-parse refs/heads/release/0.8)\" = \"\$('$REAL_GIT' -C '$WK' rev-parse origin/release/0.8)\" ]"
keep_pushes

# ── G. publish needs green-by-tree = 0 ───────────────────────────────────────
for rcv in 1 2; do
  mkfix G$rcv
  rp V=0.8.4 PHASE=prepare
  P1=$(lines "$FIX/push.log")
  echo "$rcv" >"$FIX/green.rc"
  rp V=0.8.4 PHASE=publish
  if [ "$RC" != 0 ] && out_has 'not green|green-by-tree' && log_lacks push.log 'release/0\.8$|v0\.8\.4' && [ "$(lines "$FIX/push.log")" = "$P1" ] && log_lacks gh.log 'release edit'; then
    ok "G: publish refused on green-by-tree exit $rcv"
  else bad "G: publish refused on green-by-tree exit $rcv (rc=$RC)"; sed 's/^/      /' "$FIX/out.txt"; fi
  keep_pushes
done

# ── H. release/0.8 moved after prepare ───────────────────────────────────────
mkfix H
rp V=0.8.4 PHASE=prepare
g clone -q "$OR" "$WORK/H/other" && (cd "$WORK/H/other" && g checkout -q release/0.8 && echo x >MOVED && g add MOVED && g commit -q -s -m moved && g push -q origin release/0.8)
rp V=0.8.4 PHASE=publish
if [ "$RC" != 0 ] && out_has 'release/0\.8' && log_lacks push.log 'v0\.8\.4' && log_lacks gh.log 'release edit'; then ok "H: stops when release/0.8 moved, no tag pushed"
else bad "H: stops when release/0.8 moved (rc=$RC)"; sed 's/^/      /' "$FIX/out.txt"; fi
keep_pushes

# ── I. wait keeps waiting while a run is active ──────────────────────────────
mkfix I
printf '1\n1\n0\n' >"$FIX/green.seq"
printf '[{"databaseId":55,"status":"in_progress","conclusion":null}]' >"$FIX/run_list_ci.yml.json"
rp V=0.8.4
[ "$RC" = 0 ] && ok "I: finishes once green-by-tree turns 0" || { bad "I: exit $RC"; sed 's/^/      /' "$FIX/out.txt"; }
check "I: green-by-tree polled 3 times in wait, once more in publish" bash -c "[ \"\$(grep -c . '$FIX/green.log')\" = 4 ]"
check "I: said it was waiting" out_has '[Ww]aiting'
keep_pushes

# ── J. a red run: stop, print the rerun command, never run it ────────────────
mkfix J
echo 1 >"$FIX/green.rc"
printf '[{"databaseId":66,"status":"completed","conclusion":"failure"}]' >"$FIX/run_list_nightly.yml.json"
printf '{"jobs":[{"name":"kind-sso-walk","conclusion":"failure"},{"name":"fuzz","conclusion":"success"}]}' >"$FIX/run_view_66.json"
rp V=0.8.4
if [ "$RC" != 0 ] && out_has 'kind-sso-walk' && out_has 'gh run rerun 66 --failed' && ! out_has 'fuzz' && log_lacks gh.log '^run rerun' && log_lacks push.log 'release/0\.8$|v0\.8\.4'; then
  ok "J: red run stops the wait, names the job, only prints the rerun"
else bad "J: red run (rc=$RC)"; sed 's/^/      /' "$FIX/out.txt"; fi
keep_pushes
mkfix J2
echo 2 >"$FIX/green.rc"
rp V=0.8.4
check "J: green-by-tree exit 2 stops the wait" bash -c "[ '$RC' != 0 ] && grep -q 'exit 2' '$FIX/out.txt'"
keep_pushes

# ── K. gh auth ───────────────────────────────────────────────────────────────
mkfix K
echo 1 >"$FIX/auth.rc"
rp V=0.8.4
refused "K: refuses when gh auth status fails" 'gh auth'

# ── L. a refused release commit stops before any push ────────────────────────
mkfix L
echo 3 >"$FIX/rc.rc"
rp V=0.8.4
refused "L: release-commit refusal stops the run and names HIGHLIGHTS" 'HIGHLIGHTS'

# ── M. tag order is numeric ──────────────────────────────────────────────────
mkfix M
g -C "$WK" tag v0.8.9 origin/release/0.8
rp V=0.8.10 PHASE=prepare
[ "$RC" = 0 ] && ok "M: 0.8.10 is greater than 0.8.9" || { bad "M: exit $RC"; sed 's/^/      /' "$FIX/out.txt"; }
check "M: FROM is the newest tag, 0.8.9" log_has rc.log '--from 0\.8\.9 --to 0\.8\.10'
keep_pushes

# ── N. a missing draft stops publish and prints the releases listing ─────────
mkfix N
rm -f "$FIX/release_view.json"
rp V=0.8.4
if [ "$RC" != 0 ] && log_has gh.log '^api -X GET repos/acme/wardyn/releases' && log_lacks verify.log '.'; then ok "N: a missing draft stops and lists the releases"
else bad "N: a missing draft (rc=$RC)"; sed 's/^/      /' "$FIX/out.txt"; fi
keep_pushes

# ── O. verify-release must pass ──────────────────────────────────────────────
mkfix O
echo 2 >"$FIX/verify.rc"
rp V=0.8.4
check "O: a failing verify-release fails the command" bash -c "[ '$RC' != 0 ] && grep -q 'verify-release' '$FIX/out.txt'"
keep_pushes

# ── P. inputs ────────────────────────────────────────────────────────────────
mkfix P
head -c 125001 /dev/zero | tr '\0' x >"$WORK/big.md"
rp V=0.8.4 BODY="$WORK/big.md"
refused "P: refuses a BODY over GitHub's 125000-character limit" '125000'
rp V=0.8.4 ISSUES='12;x'
refused "P: refuses a non-numeric ISSUES" 'ISSUES'
rp V=0.8.4 PHASE=bogus
refused "P: refuses an unknown PHASE" 'PHASE'
rp V=0.8.4 DRY_RUN=maybe
refused "P: refuses a DRY_RUN other than 0 or 1" 'DRY_RUN'

# ── R. F1: a local tag left at an older head is never pushed ────────────────
mkfix R
rp V=0.8.4 PHASE=prepare MERGE=origin/main
g -C "$WK" tag -a v0.8.4 -m old
OLD=$(g -C "$WK" rev-parse HEAD)
g clone -q "$OR" "$WORK/R/other" && (cd "$WORK/R/other" && echo more >MORE && g add MORE && g commit -q -s -m "more main" && g push -q origin main)
rp V=0.8.4 MERGE=origin/main
if [ "$RC" != 0 ] && [ "$(g -C "$WK" rev-parse HEAD)" != "$OLD" ] && out_has 'local tag v0\.8\.4 is at' \
  && [ -z "$(g -C "$OR" tag -l v0.8.4)" ] && log_lacks push.log 'refs/tags|v0\.8\.4$' && log_lacks push.log 'refs/heads/release/' && log_lacks gh.log 'release edit'; then
  ok "R: a stale local tag is refused by name before release/0.8 or the tag moves"
else bad "R: stale local tag (rc=$RC)"; sed 's/^/      /' "$FIX/out.txt"; fi
keep_pushes
check "R: the tag is pushed by its full ref when it is right" bash -c "grep -q 'refs/tags/v0.8.4' '$ALLPUSH'"

# ── S. F2: a nightly older than 24 hours is absent ───────────────────────────
mkfix S
rp V=0.8.4 PHASE=prepare
STREE=$(g -C "$WK" rev-parse 'HEAD^{tree}')
OLDTS=$(date -u -d '30 hours ago' +%Y-%m-%dT%H:%M:%SZ); NEWTS=$(date -u +%Y-%m-%dT%H:%M:%SZ)
runs_json() { printf '{"workflow_runs":[{"id":9,"event":"workflow_dispatch","status":"completed","conclusion":"success","created_at":"%s","updated_at":"%s","head_commit":{"tree_id":"%s"}}]}' "$1" "$1" "$STREE"; }
printf '{"jobs":[{"name":"w1","conclusion":"success"},{"name":"w2","conclusion":"success"},{"name":"w3","conclusion":"success"},{"name":"multi-arch build (x)","conclusion":"success"}]}' >"$FIX/api_repos_acme_wardyn_actions_runs_9_jobs.json"
W1=$(grep -c '^workflow run nightly' "$FIX/gh.log")
runs_json "$NEWTS" >"$FIX/api_repos_acme_wardyn_actions_workflows_nightly.yml_runs.json"
rp V=0.8.4 PHASE=prepare
check "S: a green nightly from now is not dispatched again" bash -c "[ \"\$(grep -c '^workflow run nightly' '$FIX/gh.log')\" = $W1 ]"
runs_json "$OLDTS" >"$FIX/api_repos_acme_wardyn_actions_workflows_nightly.yml_runs.json"
rp V=0.8.4 PHASE=prepare
check "S: a green nightly 30 hours old is dispatched afresh" bash -c "[ \"\$(grep -c '^workflow run nightly' '$FIX/gh.log')\" = $((W1 + 1)) ]"
# wait and publish: green-by-tree passes on the old run (nightly_run=2), the run is stale
printf '{"updated_at":"%s"}' "$OLDTS" >"$FIX/api_repos_acme_wardyn_actions_runs_2.json"
rp V=0.8.4 PHASE=publish
if [ "$RC" != 0 ] && out_has 'more than 24 hours' && [ "$(lines "$FIX/push.log")" = 1 ] && log_lacks push.log 'release/0\.8$|v0\.8\.4'; then ok "S: publish refuses a stale nightly before any push"
else bad "S: publish with a stale nightly (rc=$RC)"; sed 's/^/      /' "$FIX/out.txt"; fi
rp V=0.8.4 PHASE=wait
if [ "$RC" != 0 ] && out_has 'more than 24 hours'; then ok "S: wait does not accept a stale nightly with nothing running"
else bad "S: wait with a stale nightly (rc=$RC)"; fi
keep_pushes

# ── T. an rc never moves release/X.Y ─────────────────────────────────────────
mkfix T
printf '[{"databaseId":902,"headBranch":"v0.8.4-rc.1","status":"completed","event":"push"}]' >"$FIX/run_list_release.yml.json"
rp V=0.8.4-rc.1
[ "$RC" = 0 ] && ok "T: an rc run exits 0" || { bad "T: rc run exit $RC"; sed 's/^/      /' "$FIX/out.txt"; }
check "T: release/0.8 on origin is untouched" bash -c "[ \"\$('$REAL_GIT' -C '$OR' rev-parse refs/heads/release/0.8)\" = \"\$('$REAL_GIT' -C '$WK' rev-parse origin/release/0.8)\" ]"
check "T: no push to release/*" log_lacks push.log 'refs/heads/release/'
check "T: the rc tag is pushed from the candidate" bash -c "[ \"\$('$REAL_GIT' -C '$OR' rev-parse 'refs/tags/v0.8.4-rc.1^{commit}')\" = \"\$('$REAL_GIT' -C '$OR' rev-parse refs/heads/chore/release-0.8.4-rc.1)\" ]"
check "T: published as a pre-release" log_has gh.log '^release edit v0\.8\.4-rc\.1 --draft=false --prerelease '
keep_pushes
# an rc already on a candidate branch does not become the next FROM
mkfix T2
g -C "$WK" tag v0.8.4-rc.1 main
rp V=0.8.4 PHASE=prepare
check "T: FROM ignores an rc tag that release/0.8 does not contain" log_has rc.log -- '--from 0\.8\.3 --to 0\.8\.4'
keep_pushes

# ── U. F4: a dry run on a branch that DOES hold release/0.8 pushes nothing there
mkfix U
g -C "$WK" branch --no-track feat-y origin/release/0.8
RELBEFORE=$(g -C "$OR" rev-parse refs/heads/release/0.8)
rp V=0.8.4 BRANCH=feat-y DRY_RUN=1 HIGHLIGHTS=x
[ "$RC" = 0 ] && ok "U: dry run exits 0" || { bad "U: dry run exit $RC"; sed 's/^/      /' "$FIX/out.txt"; }
check "U: release/0.8 unchanged and no tag" bash -c "[ \"\$('$REAL_GIT' -C '$OR' rev-parse refs/heads/release/0.8)\" = '$RELBEFORE' ] && [ -z \"\$('$REAL_GIT' -C '$OR' tag -l v0.8.4)\" ]"
check "U: no push but the candidate branch" bash -c "[ \"\$(grep -vc '^push origin HEAD:refs/heads/feat-y\$' '$FIX/push.log')\" = 0 ]"
check "U: no release edit, no verify, no PR" bash -c "! grep -qE '^release edit|^pr create' '$FIX/gh.log' && [ ! -s '$FIX/verify.log' ]"
keep_pushes

# ── V. F4: verify-release that does not end fails=0 fails the command ─────────
mkfix V
echo "fails=3" >"$FIX/verify.out"
rp V=0.8.4
check "V: exit 0 from verify-release without fails=0 still fails" bash -c "[ '$RC' != 0 ] && grep -q 'did not end fails=0' '$FIX/out.txt'"
keep_pushes

# ── W. the repository comes from GITHUB_REPOSITORY, else from gh repo view ──
mkfix W
rp V=0.8.4 PHASE=prepare GITHUB_REPOSITORY=other/repo
check "W: GITHUB_REPOSITORY is honoured" log_has gh.log 'repos/other/repo/actions/workflows/nightly\.yml/runs'
rp V=0.8.4 PHASE=prepare GITHUB_REPOSITORY=
check "W: without it, gh repo view names the repository" log_has gh.log '^repo view '
keep_pushes

# ── Q. the script text and every recorded push ───────────────────────────────
check "Q: the script text never forces a push" bash -c "! grep -nE 'push[^#]*(--force|-f |--mirror| \\+)' '$SCRIPT'"
check "Q: no case pushed with --force, -f, --mirror or a + refspec" bash -c "! grep -qE -- '--force|--mirror| -f |\\+refs|\\+[A-Za-z0-9]' '$ALLPUSH'"

exit "$fail"
