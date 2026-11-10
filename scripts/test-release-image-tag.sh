#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-release-image-tag.sh — release.yml turns github.ref_name into an image
# tag. A vX.Y.Z tag push gives X.Y.Z; a dry-run dispatch on a branch such as
# train/0.10-rc9-w1 must still give a valid tag (`/` is not allowed in one), or
# every image build fails with "invalid reference format".
#
# It also holds the other release-workflow checks that need no daemon (the base-image ref, publish-image's tags,
# no tree-built binary in a privileged job).
#
# Daemon-free, network-free: each "Compute the release tag" step's run script
# is pulled out of the workflow and run with a chosen REF_NAME.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WF="$ROOT/.github/workflows/release.yml"
fail() { echo "FAIL: $*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# The run block of every step named "Compute the release tag".
awk '
  /^      - name: Compute the release tag$/ { n++; want = 1; next }
  want && /^        run: \|$/ { grab = 1; next }
  want && /^        run: / { sub(/^        run: /, ""); print > (dir "/step" n ".sh"); want = 0; next }
  grab && /^          / { sub(/^          /, ""); print > (dir "/step" n ".sh"); next }
  grab { grab = 0; want = 0 }
' dir="$WORK" "$WF"
steps="$(find "$WORK" -name 'step*.sh' | wc -l)"
[ "$steps" -eq 2 ] || fail "want 2 'Compute the release tag' steps in release.yml, found $steps"

for s in "$WORK"/step*.sh; do
  while IFS='|' read -r ref want; do
    out="$WORK/out"; : >"$out"
    REF_NAME="$ref" GITHUB_OUTPUT="$out" bash "$s"
    [ "$(cat "$out")" = "tag=$want" ] || fail "$(basename "$s"): ref '$ref': got [$(cat "$out")], want [tag=$want]"
  done <<'CASES'
v0.10.0|0.10.0
v0.10.0-rc.9|0.10.0-rc.9
train/0.10-rc9-w1|train-0.10-rc9-w1
main|main
CASES
done

# The promote job derives the same tag from REF_NAME.
grep -q 'RELEASE_TAG="${RELEASE_TAG//\\//-}"' "$WF" || fail "the promote step does not replace '/' in the tag"

# The UI-sandbox images build on the base DIGEST, verified against this run's workflow identity, never on its tag.
awk '
  /^      - name: Compute the base-image ref$/ { n++; want = 1; next }
  want && /^        run: \|$/ { grab = 1; next }
  grab && /^          / { sub(/^          /, ""); print > (dir "/base.sh"); next }
  grab { grab = 0; want = 0 }
' dir="$WORK" "$WF"
[ -s "$WORK/base.sh" ] || fail "no 'Compute the base-image ref' step in release.yml"
n="$(grep -c '^      - name: Compute the base-image ref$' "$WF")"
[ "$n" -eq 1 ] || fail "want 1 'Compute the base-image ref' step (the two UI-sandbox jobs share it), found $n"
grep -q 'BASE_IMAGE=${{ steps.base.outputs.ref }}' "$WF" || fail "the UI-sandbox build no longer takes BASE_IMAGE from the base step"
mkdir -p "$WORK/stub"
cat > "$WORK/stub/docker" <<'STUB'
#!/usr/bin/env bash
echo "docker $*" >> "$STUBLOG"
echo '"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
STUB
cat > "$WORK/stub/cosign" <<'STUB'
#!/usr/bin/env bash
echo "cosign $*" >> "$STUBLOG"
[ -z "${COSIGN_FAIL:-}" ]
STUB
chmod +x "$WORK/stub/docker" "$WORK/stub/cosign"
runbase() { # DRY_RUN -> sets out, rc
  out="$WORK/out"; : >"$out"; : >"$WORK/stublog"; rc=0
  PATH="$WORK/stub:$PATH" STUBLOG="$WORK/stublog" GITHUB_OUTPUT="$out" DRY_RUN="$1" RELEASE_TAG=0.10.0 BASE_NAME=agent-base \
    WORKFLOW_REF=cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/v0.10.0 bash "$WORK/base.sh" || rc=$?
}
digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
runbase false
[ "$rc" -eq 0 ] || fail "base step failed on a real run"
[ "$(cat "$WORK/out")" = "ref=ghcr.io/cjohnstoniv/agent-base@$digest" ] || fail "base ref is not the digest: $(cat "$WORK/out")"
grep -q "^cosign verify ghcr.io/cjohnstoniv/agent-base@$digest --certificate-identity https://github.com/cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/v0.10.0 --certificate-oidc-issuer https://token.actions.githubusercontent.com\$" "$WORK/stublog" \
  || fail "the base digest was not cosign-verified against this run's identity: $(cat "$WORK/stublog")"
COSIGN_FAIL=1 runbase false
[ "$rc" -ne 0 ] || fail "an unsigned base digest was accepted"
runbase true
[ "$(cat "$WORK/out")" = "ref=ghcr.io/cjohnstoniv/agent-base:latest" ] || fail "dry run base ref: $(cat "$WORK/out")"
! grep -q cosign "$WORK/stublog" || fail "a dry run verified a signature it cannot have"

# publish-image.yml: :latest moves forward only, and extra_tag never writes a v* tag.
PI="$ROOT/.github/workflows/publish-image.yml"
awk '
  /^      - name: Compute image tags$/ { want = 1; next }
  want && /^        run: \|$/ { grab = 1; next }
  grab && /^          / { sub(/^          /, ""); print > (dir "/tags.sh"); next }
  grab { grab = 0; want = 0 }
' dir="$WORK" "$PI"
[ -s "$WORK/tags.sh" ] || fail "no 'Compute image tags' step in publish-image.yml"
grep -Eq '^    concurrency:$' "$PI" && grep -q 'group: publish-image$' "$PI" && grep -q 'cancel-in-progress: false' "$PI" \
  || fail "publish-image.yml has no non-cancelling concurrency group on the publish job"
cat > "$WORK/stub/git" <<'STUB'
#!/usr/bin/env bash
[ "$1 $2 $3" = "ls-remote origin refs/heads/main" ] || { echo "unexpected git $*" >&2; exit 99; }
[ -z "${TIP:-}" ] || printf '%s\trefs/heads/main\n' "$TIP"
STUB
chmod +x "$WORK/stub/git"
sha=1111111111111111111111111111111111111111
newer=2222222222222222222222222222222222222222
runtags() { # TIP EXTRA_TAG [REF_TYPE] -> sets out, rc
  out="$WORK/out"; : >"$out"; rc=0
  PATH="$WORK/stub:$PATH" TIP="$1" EXTRA_TAG="$2" REF_TYPE="${3:-branch}" REF_NAME="${REFN:-main}" SHA="$sha" GITHUB_OUTPUT="$out" bash "$WORK/tags.sh" >"$WORK/stdout" 2>&1 || rc=$?
}
runtags "$sha" ""
[ "$rc" -eq 0 ] && grep -q 'wardynd:latest$' "$out" || fail "main's tip did not get :latest (rc $rc): $(cat "$WORK/stdout")"
runtags "$newer" ""
[ "$rc" -eq 0 ] || fail "a stale commit failed the step instead of skipping :latest (rc $rc)"
! grep -q 'wardynd:latest$' "$out" || fail "a commit that is not main's tip was given :latest"
grep -q "wardynd:sha-1111111$" "$out" || fail "the sha tag was dropped for a stale commit"
runtags "" ""
[ "$rc" -ne 0 ] || fail "an unreadable main tip still published"
# extra_tag is an allow-list (0.0.x to 0.4.x). Every other spelling is refused, whatever the ref.
for bad in 0.10.0 0.10.0-rc.18 0.5.0 v0.4.4 latest sha-1111111 main "x y"; do
  runtags "$sha" "$bad"
  [ "$rc" -ne 0 ] || fail "extra_tag='$bad' was accepted"
done
# `latest` on a stale (non-tip) ref is refused as an extra_tag, not just skipped as the computed tag.
runtags "$newer" latest
[ "$rc" -ne 0 ] || fail "extra_tag=latest on a ref that is not main's tip was accepted"
runtags "$newer" 0.10.0
[ "$rc" -ne 0 ] || fail "extra_tag=0.10.0 on a stale ref was accepted"
for ok in 0.4.4 0.0.1 0.3.12; do
  runtags "$sha" "$ok"
  [ "$rc" -eq 0 ] && grep -q "wardynd:$ok\$" "$out" || fail "the $ok backfill tag was refused (rc $rc): $(cat "$WORK/stdout")"
done
# A tag ref: only a 0.0.x to 0.4.x one publishes; release.yml owns the rest (v0.10.0, v0.10.0-rc.18 would write 0.10.0 / 0.10.0-rc.18).
for badref in v0.10.0 v0.10.0-rc.18 v0.5.1 v1.0.0 nightly; do
  REFN="$badref" runtags "$sha" "" tag
  [ "$rc" -ne 0 ] || fail "tag ref $badref was published by publish-image.yml"
  ! grep -q . "$out" || fail "tag ref $badref wrote tags before refusing: $(cat "$out")"
done
REFN=v0.4.4 runtags "$sha" "" tag
[ "$rc" -eq 0 ] && grep -q 'wardynd:0.4.4$' "$out" || fail "tag ref v0.4.4 was refused (rc $rc)"
! grep -q 'wardynd:latest$' "$out" || fail "a tag ref was given :latest"

# release.yml: no job that can write to the repository or mint the signing identity executes a binary built from
# the tree, and no step whose env holds the Apple secrets executes one either.
awk '
  function flush() { if (job != "" && priv && bad != "") { print job ": " bad; found = 1 } }
  /^  [A-Za-z0-9_-]+:$/ { flush(); job = $1; priv = 0; bad = ""; next }
  /^      (contents|packages|id-token): write/ { priv = 1 }
  /^[[:space:]]*#/ { next }
  /(^|[[:space:]($"`])(\.\/|bin\/)wardyn[A-Za-z0-9._-]*[[:space:]]+(version|--)/ { if (bad == "") bad = $0 }
  /\$\("?\$helper"?[[:space:]]+version/ { if (bad == "") bad = $0 }
  END { flush(); exit found }
' "$WF" || fail "a job holding contents/packages/id-token write executes a tree-built binary (lines above)"
awk '
  function flush() { if (secret && exec) { print "step with Apple secrets runs: " exec; found = 1 } }
  /^      - / { flush(); secret = 0; exec = "" }
  /secrets\.APPLE_/ { secret = 1 }
  /^[[:space:]]*#/ { next }
  /\$\("?\$(helper|agent)"?[[:space:]]/ || /"\$RUNNER_TEMP\/pkg\/[^"]*wardyn[^"]*"[[:space:]]+version/ { if (exec == "") exec = $0 }
  END { flush(); exit found }
' "$WF" || fail "a step whose env holds the Apple secrets executes a tree-built binary"

echo "test-release-image-tag: ok"
