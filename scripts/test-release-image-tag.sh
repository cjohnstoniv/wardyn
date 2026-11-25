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

# The per-commit image carries the commit it was built from: the tags step outputs it (empty on a tag ref, which keeps
# the bare version), the build passes it as the REVISION build arg, and a later step runs the pushed image and checks it.
runtags "$sha" ""
grep -qx "revision=$sha" "$out" || fail "a branch build did not output revision=$sha: $(cat "$out")"
REFN=v0.4.4 runtags "$sha" "" tag
grep -qx 'revision=' "$out" || fail "a tag ref did not output an empty revision: $(cat "$out")"
grep -Eq '^            REVISION=\$\{\{ steps\.tags\.outputs\.revision \}\}$' "$PI" || fail "publish-image.yml does not pass REVISION from the tags step to the build"
grep -q '^      - name: Check the image reports its revision$' "$PI" || fail "publish-image.yml has no step that checks the built image's revision"
grep -q 'grep -aqF "\$REVISION"' "$PI" || fail "the image check does not look for the stamped revision in the binaries"
df="$ROOT/deploy/compose/Dockerfile.wardynd"
# wardynd, the in-container wardyn, the fallback host CLI and the per-platform host CLIs: four go build commands.
[ "$(grep -c 'internal/version.buildRevision=\${REVISION}' "$df")" -eq 4 ] || fail "Dockerfile.wardynd does not stamp buildRevision into wardynd and every wardyn build"
[ "$(grep -c 'internal/version.releaseBuild=\${RELEASE_BUILD}' "$df")" -eq 4 ] || fail "Dockerfile.wardynd does not stamp releaseBuild into wardynd and every wardyn build"
grep -q 'run_in_image /host/wardyn-linux-amd64' "$PI" || fail "the image check does not run the linux/amd64 host CLI from the image"

# Moving tags: the build pushes only the per-commit tag; :latest and the other names are applied by a later step, after
# the image has been checked and signed, and each must resolve to the signed digest.
runtags "$sha" ""
grep -qx 'first_tag=ghcr.io/cjohnstoniv/wardynd:sha-1111111' "$out" || fail "the tags step did not output the per-commit tag as first_tag: $(cat "$out")"
later="$(awk '/^later_tags<<TAGEOF$/ { on = 1; next } /^TAGEOF$/ { on = 0 } on' "$out")"
grep -qx 'ghcr.io/cjohnstoniv/wardynd:latest' <<<"$later" || fail "later_tags does not hold :latest for main's tip"
! grep -q 'sha-1111111' <<<"$later" || fail "later_tags repeats the per-commit tag"
step_line() { grep -n "^      - name: $1\$" "$PI" | head -1 | cut -d: -f1 || true; }
b_line="$(grep -n '^        id: build$' "$PI" | head -1 | cut -d: -f1 || true)"
c_line="$(step_line 'Check the image reports its revision')"
g_line="$(step_line 'Sign the pushed image (keyless, by digest)')"
r_line="$(step_line 'Point the remaining tags at the signed image')"
[ -n "$b_line" ] && [ -n "$c_line" ] && [ -n "$g_line" ] && [ -n "$r_line" ] || fail "publish-image.yml lacks the build, check, sign or retag step"
[ "$b_line" -lt "$c_line" ] && [ "$c_line" -lt "$g_line" ] && [ "$g_line" -lt "$r_line" ] || fail "publish-image.yml must build, check, sign, then retag, in that order"
shallow='^ {0,6}[^ ]' # the next line leaves the tags value: no second tag under it
grep -A1 '^          tags: ' "$PI" | sed -n '1,2p' | { IFS= read -r l1; IFS= read -r l2; [ "$l1" = '          tags: ${{ steps.tags.outputs.first_tag }}' ] && [[ "$l2" =~ $shallow ]]; } || fail "the build pushes more than the per-commit tag"
[ "$(grep -c 'imagetools create' "$PI")" -eq 1 ] && [ "$(grep -n 'imagetools create' "$PI" | cut -d: -f1)" -gt "$g_line" ] || fail "a tag is applied outside the retag step that follows the signature"
grep -q 'imagetools create --tag "\$t" "\$IMAGE_REF"' "$PI" || fail "the retag step does not copy the signed digest"
grep -q 'want the signed' "$PI" || fail "the retag step does not check each tag's digest against the signed one"

# Publisher guard: a job that holds packages or id-token write (its own permissions block, or the workflow's when it has
# none) must not execute a file taken out of the image on the runner. The image is unsigned until that job signs it, so
# a binary it carries runs only inside a container (run_in_image), never as a host process there. Prints each offending
# line and exits 1; reads the workflow on stdin.
publisher_host_exec_guard() {
  awk '
    function flush() { if (job != "" && priv && bad != "") { print job ": " bad; found = 1 } }
    /^permissions:/ { top = 1; next }
    /^[A-Za-z]/ { top = 0 }
    top && /^  (packages|id-token): write/ { wfwrite = 1 }
    /^jobs:/ { injobs = 1; next }
    injobs && /^  [A-Za-z0-9_-]+:$/ { flush(); job = $1; own = 0; ownwrite = 0; priv = 0; bad = ""; next }
    injobs && /^    permissions:/ { own = 1; next }
    injobs && own && /^      (packages|id-token): write/ { ownwrite = 1 }
    injobs { priv = ownwrite || (!own && wfwrite) }
    /^[[:space:]]*#/ { next }
    # A command word that is a variable ("$f" version, $(...$f...), a copied-out file run through a loop variable), an
    # interpreter or wrapper run on a file, a chmod, or a path under RUNNER_TEMP run directly.
    injobs && (/chmod[[:space:]]+\+x/ || /\$RUNNER_TEMP\/[A-Za-z0-9._\/-]*"?[[:space:]]+(version|--)/ || /\$\("?\$RUNNER_TEMP\// \
      || /(^|;|&&|\|\||\||\$\(|`)[[:space:]]*"?\$[{]?[A-Za-z_][A-Za-z0-9_]*[}]?"?([[:space:]]+[^=[:space:]]|[[:space:]]*($|;|&|\||\)))/ \
      || /(^|;|&&|\|\||\||\$\(|`)[[:space:]]*(bash|sh|env|exec|xargs|nohup|timeout|setsid|sudo)[[:space:]]/) { if (bad == "") bad = $0 }
    END { flush(); exit found }
  '
}
# The publishing job's handling of the unsigned image is pinned by equality, not by a list of shapes to refuse:
# a deny-list always has one more spelling. In a job that holds packages or id-token write (as above):
#   1. the body of run_in_image is exactly the one expected command, so no flag, mount or option can change;
#   2. every line that runs docker is one of the known lines: the run inside run_in_image, the one create, the two
#      cp, the rm and the two imagetools lines, so no other subcommand (run elsewhere, container, start, exec,
#      pull, push, tag) can appear;
#   3. every line that names the copied-out files (RUNNER_TEMP, the loop variable f, the container id cid) is one
#      of the known lines, so nothing new can act on them.
# A change to the step therefore fails here until this list is edited with it. Prints each offending line and
# exits 1; reads the workflow on stdin.
PIN_BODY='docker run --rm --platform linux/amd64 --network none --read-only --cap-drop ALL \
--security-opt no-new-privileges --entrypoint "$1" "$IMAGE_REF" version'
PIN_DOCKER='cid="$(docker create --platform "linux/$arch" "$IMAGE_REF")"
docker cp "$cid:/host/." "$RUNNER_TEMP/$arch/host"
docker cp "$cid:/wardynd" "$RUNNER_TEMP/$arch/wardynd"
docker rm "$cid" >/dev/null
docker buildx imagetools create --tag "$t" "$IMAGE_REF"
now="$(docker buildx imagetools inspect "$t" --format '"'"'{{.Manifest.Digest}}'"'"')"'
PIN_FILES='for arch in amd64 arm64; do
mkdir -p "$RUNNER_TEMP/$arch/host"
expected=("$RUNNER_TEMP/$arch/wardynd" "$RUNNER_TEMP/$arch/host/wardyn")
expected+=("$RUNNER_TEMP/$arch/host/wardyn-$platform")
for f in "${expected[@]}"; do
if [ -L "$f" ] || [ ! -f "$f" ]; then
echo "::error::$(basename "$f") is missing from the linux/$arch image or is not a regular file"; exit 1
grep -aqF "$REVISION" "$f" \
|| { echo "::error::$(basename "$f") in the linux/$arch image was not stamped with revision ${REVISION}"; exit 1; }'
publisher_pin_guard() { # PIN_REQUIRE=1 also demands every known line
  PIN_BODY="$PIN_BODY" PIN_DOCKER="$PIN_DOCKER" PIN_FILES="$PIN_FILES" awk '
    function trim(x) { sub(/^[[:space:]]+/, "", x); sub(/[[:space:]]+$/, "", x); return x }
    function flush(  k) {
      if (job == "" || !priv) return
      if (defs != 1) { print job ": run_in_image is defined " defs " times, want once"; found = 1 }
      if (require) { for (k in okd) if (!(k in hit)) { print job ": a known line is missing: " k; found = 1 }; for (k in okf) if (!(k in hit)) { print job ": a known line is missing: " k; found = 1 } }
    }
    BEGIN { body = ENVIRON["PIN_BODY"]; require = (ENVIRON["PIN_REQUIRE"] == "1"); n = split(ENVIRON["PIN_DOCKER"], d, "\n"); for (i = 1; i <= n; i++) okd[d[i]] = 1; n = split(ENVIRON["PIN_FILES"], fl, "\n"); for (i = 1; i <= n; i++) okf[fl[i]] = 1 }
    /^permissions:/ { top = 1; next }
    /^[A-Za-z]/ { top = 0 }
    top && /^  (packages|id-token): write/ { wfwrite = 1 }
    /^jobs:/ { injobs = 1; next }
    injobs && /^  [A-Za-z0-9_-]+:$/ { flush(); job = $1; own = 0; ownwrite = 0; priv = 0; defs = 0; inbody = 0; got = ""; delete hit; next }
    injobs && /^    permissions:/ { own = 1; next }
    injobs && own && /^      (packages|id-token): write/ { ownwrite = 1 }
    injobs { priv = ownwrite || (!own && wfwrite) }
    !(injobs && priv) || /^[[:space:]]*#/ { next }
    { t = trim($0) }
    t ~ /^run_in_image\(\)[[:space:]]*\{$/ { inbody = 1; defs++; got = ""; next }
    inbody && t == "}" { inbody = 0; if (got != body) { print job ": run_in_image is not the expected command: " got; found = 1 }; next }
    inbody { got = (got == "" ? t : got "\n" t); next }
    (t in okd) || (t in okf) { hit[t] = 1 }
    t ~ /(^|[^A-Za-z0-9_.\/-])docker[[:space:]]/ && !(t in okd) { print job ": a docker command that is not one of the known lines: " t; found = 1 }
    t ~ /\$\{?(RUNNER_TEMP|f|cid)\}?([^A-Za-z0-9_]|$)/ || t ~ /(^|[[:space:];(])(f|cid)=/ || t ~ /(^|[[:space:];])for[[:space:]]+(f|cid|arch)[[:space:]]/ {
      if (!(t in okf) && !(t in okd)) { print job ": a line that names the copied-out files and is not one of the known lines: " t; found = 1 }
    }
    END { flush(); exit found }
  '
}
publisher_host_exec_guard < "$PI" || fail "publish-image.yml has a publishing job that executes a file taken from the image on the runner (lines above)"
# The guard has to see the shape it exists for: the image check once chmod-ed the extracted host CLI and ran it on the runner.
cat > "$WORK/old-shape.yml" <<'OLD'
permissions:
  contents: read
  packages: write
  id-token: write
jobs:
  publish-image:
    runs-on: ubuntu-26.04
    steps:
      - name: Check the image reports its revision
        run: |
          docker cp "$cid:/host/." "$RUNNER_TEMP/host"
          chmod +x "$RUNNER_TEMP/host/wardyn-linux-amd64"
          got="$("$RUNNER_TEMP/host/wardyn-linux-amd64" version)"
OLD
if publisher_host_exec_guard < "$WORK/old-shape.yml" >/dev/null; then fail "the publisher guard accepts a runner-side run of an extracted CLI"; fi
sed -i '/^    runs-on/a\    permissions:\n      contents: read' "$WORK/old-shape.yml"
publisher_host_exec_guard < "$WORK/old-shape.yml" >/dev/null || fail "the publisher guard refuses a job that holds no write permission"
PIN_REQUIRE=1 publisher_pin_guard < "$PI" || fail "the publishing job in publish-image.yml handles the image outside the pinned lines (lines above)"

# The publishing job's steps and what they run are pinned whole, so a line added to a step or a step added to the job
# fails until this test is edited with it. The list of step starts drops the ref after an action's name (a version
# bump is not a change), and the whole job is compared line by line with its comments and blank lines dropped and every
# line trimmed, so a line added to any step (a shell, an env that a shell reads) is a difference. The pin guard above
# names the lines that touch the copied-out files; this is the check that nothing else was added next to them.
publisher_step_starts() { awk '/^  publish-image:$/ { on = 1; next } /^  [A-Za-z0-9_-]+:$/ { on = 0 } on && /^      - /' | sed -E 's/@[^ ]+( #.*)?$//'; }
publisher_job_lines() { # the publishing job whole, from stdin: every line but comments and blank ones, each trimmed, an action's ref dropped
  awk '
function trim(x) { sub(/^[[:space:]]+/, "", x); sub(/[[:space:]]+$/, "", x); return x }
/^  [A-Za-z0-9_-]+:$/ { on = ($1 == "publish-image:"); next }
!on { next }
{ t = trim($0); if (t == "" || t ~ /^#/) next; if (t ~ /(^|[[:space:]])uses: /) sub(/@[^ ]+( #.*)?$/, "", t); print t }
'
}
PIN_STEPS='      - uses: actions/checkout
      - uses: docker/setup-qemu-action
      - uses: docker/setup-buildx-action
      - uses: docker/login-action
      - name: Log in to Docker Hub (skipped when the secret is absent, e.g. a fork)
      - name: Compute image tags
      - uses: sigstore/cosign-installer
      - uses: docker/build-push-action
      - name: Check the image reports its revision
      - name: Sign the pushed image (keyless, by digest)
      - name: Point the remaining tags at the signed image'
PIN_JOB="$(cat <<'PIN_JOB_END'
needs: gate
if: needs.gate.outputs.green == 'true'
concurrency:
group: publish-image
cancel-in-progress: false
runs-on: ubuntu-26.04
timeout-minutes: 20
env:
DOCKERHUB_USERNAME: ${{ secrets.DOCKERHUB_USERNAME }}
steps:
- uses: actions/checkout
with:
ref: ${{ needs.gate.outputs.sha }}
- uses: docker/setup-qemu-action
- uses: docker/setup-buildx-action
- uses: docker/login-action
with:
registry: ghcr.io
username: ${{ github.actor }}
password: ${{ secrets.GITHUB_TOKEN }}
- name: Log in to Docker Hub (skipped when the secret is absent, e.g. a fork)
uses: docker/login-action
if: env.DOCKERHUB_USERNAME != ''
with:
username: ${{ env.DOCKERHUB_USERNAME }}
password: ${{ secrets.DOCKERHUB_TOKEN }}
- name: Compute image tags
id: tags
env:
REF_TYPE: ${{ github.ref_type }}
REF_NAME: ${{ github.ref_name }}
EXTRA_TAG: ${{ inputs.extra_tag }}
SHA: ${{ needs.gate.outputs.sha }}
run: |
set -euo pipefail
backfill='^0\.[0-4]\.[0-9]+$'
if [ -n "${EXTRA_TAG:-}" ] && ! [[ "$EXTRA_TAG" =~ $backfill ]]; then
echo "::error::extra_tag must be a 0.0.x to 0.4.x backfill tag (got ${EXTRA_TAG}); release tags come from release.yml"; exit 1
fi
if [ "$REF_TYPE" = "tag" ] && ! [[ "${REF_NAME#v}" =~ $backfill ]]; then
echo "::error::refusing to publish tag ref ${REF_NAME}: release.yml owns release image tags; only 0.0.x to 0.4.x backfills run here"; exit 1
fi
tags=("ghcr.io/cjohnstoniv/wardynd:sha-${SHA::7}")
if [ "$REF_TYPE" = "branch" ] && [ "$REF_NAME" = "main" ]; then
tip="$(git ls-remote origin refs/heads/main | cut -f1)"
[ -n "$tip" ] || { echo "::error::could not read main's tip"; exit 1; }
if [ "$tip" = "$SHA" ]; then
tags+=("ghcr.io/cjohnstoniv/wardynd:latest")
else
echo "::notice::$SHA is not main's tip ($tip); not moving :latest"
fi
fi
if [ "$REF_TYPE" = "tag" ]; then
tags+=("ghcr.io/cjohnstoniv/wardynd:${REF_NAME#v}")
fi
if [ -n "${EXTRA_TAG:-}" ]; then
tags+=("ghcr.io/cjohnstoniv/wardynd:${EXTRA_TAG}")
fi
{
echo "first_tag=${tags[0]}"
echo "later_tags<<TAGEOF"
printf '%s\n' "${tags[@]:1}"
echo "TAGEOF"
if [ "$REF_TYPE" = "tag" ]; then echo "revision="; else echo "revision=${SHA}"; fi
} >> "$GITHUB_OUTPUT"
- uses: sigstore/cosign-installer
- uses: docker/build-push-action
id: build
with:
context: .
file: deploy/compose/Dockerfile.wardynd
push: true
platforms: linux/amd64,linux/arm64
provenance: false
build-args: |
REVISION=${{ steps.tags.outputs.revision }}
tags: ${{ steps.tags.outputs.first_tag }}
- name: Check the image reports its revision
env:
IMAGE_REF: ghcr.io/cjohnstoniv/wardynd@${{ steps.build.outputs.digest }}
REVISION: ${{ steps.tags.outputs.revision }}
REF_NAME: ${{ github.ref_name }}
run: |
set -euo pipefail
reports() {
if [ -n "$REVISION" ]; then
case "$2" in *"+${REVISION::9}") ;; *) echo "::error::$1 printed '$2', want it to end in +${REVISION::9}"; exit 1;; esac
elif [ "$2" != "${REF_NAME#v}" ]; then
echo "::error::$1 printed '$2', want the release version ${REF_NAME#v}"; exit 1
fi
}
run_in_image() {
docker run --rm --platform linux/amd64 --network none --read-only --cap-drop ALL \
--security-opt no-new-privileges --entrypoint "$1" "$IMAGE_REF" version
}
cli="$(run_in_image /usr/local/bin/wardyn)" || { echo "::error::wardyn version failed in the image"; exit 1; }
got="$(run_in_image /host/wardyn-linux-amd64)" || { echo "::error::the host CLI's version command failed in the image"; exit 1; }
reports "wardyn in the image" "$cli"
reports "the linux/amd64 host CLI in the image" "$got"
for arch in amd64 arm64; do
cid="$(docker create --platform "linux/$arch" "$IMAGE_REF")"
mkdir -p "$RUNNER_TEMP/$arch/host"
docker cp "$cid:/host/." "$RUNNER_TEMP/$arch/host"
docker cp "$cid:/wardynd" "$RUNNER_TEMP/$arch/wardynd"
docker rm "$cid" >/dev/null
expected=("$RUNNER_TEMP/$arch/wardynd" "$RUNNER_TEMP/$arch/host/wardyn")
for platform in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
expected+=("$RUNNER_TEMP/$arch/host/wardyn-$platform")
done
for f in "${expected[@]}"; do
if [ -L "$f" ] || [ ! -f "$f" ]; then
echo "::error::$(basename "$f") is missing from the linux/$arch image or is not a regular file"; exit 1
fi
if [ -n "$REVISION" ]; then
grep -aqF "$REVISION" "$f" \
|| { echo "::error::$(basename "$f") in the linux/$arch image was not stamped with revision ${REVISION}"; exit 1; }
fi
done
done
echo "image reports: $got"
- name: Sign the pushed image (keyless, by digest)
env:
IMAGE_REF: ghcr.io/cjohnstoniv/wardynd@${{ steps.build.outputs.digest }}
run: cosign sign --yes "${IMAGE_REF}"
- name: Point the remaining tags at the signed image
env:
IMAGE_REF: ghcr.io/cjohnstoniv/wardynd@${{ steps.build.outputs.digest }}
DIGEST: ${{ steps.build.outputs.digest }}
LATER_TAGS: ${{ steps.tags.outputs.later_tags }}
run: |
set -euo pipefail
while IFS= read -r t; do
[ -n "$t" ] || continue
docker buildx imagetools create --tag "$t" "$IMAGE_REF"
now="$(docker buildx imagetools inspect "$t" --format '{{.Manifest.Digest}}')"
[ "$now" = "$DIGEST" ] || { echo "::error::$t points at $now, want the signed $DIGEST"; exit 1; }
done <<< "$LATER_TAGS"
PIN_JOB_END
)"
publisher_whole_pin() { # reads the workflow on stdin; prints each difference, exits 1 on any
  local wf rc=0 got
  wf="$(cat)"
  got="$(publisher_step_starts <<<"$wf")"
  [ "$got" = "$PIN_STEPS" ] || { echo "the publishing job's steps are not the pinned list: $got"; rc=1; }
  got="$(publisher_job_lines <<<"$wf")"
  [ "$got" = "$PIN_JOB" ] || { echo "the publishing job's lines are not the pinned text:"; diff <(printf '%s\n' "$PIN_JOB") <(printf '%s\n' "$got") | head -20; rc=1; }
  return $rc
}
publisher_whole_pin < "$PI" || fail "publish-image.yml's publishing job differs from the pinned steps and run blocks (lines above)"
# Lines that run a copied-out file by a spelling the pin guard does not name, and a step added to the job, each
# added to a copy of the real workflow: each must fail, and a comment or a version bump of an action must not.
plant() { # NAME AFTER LINE...: the workflow with LINE added after the first line that contains AFTER
  local name="$1" after="$2"; shift 2
  ADD="$(printf '%s\n' "$@")" AFTER="$after" awk '{ print } !done && index($0, ENVIRON["AFTER"]) { print ENVIRON["ADD"]; done = 1 }' "$PI" > "$WORK/plant-$name.yml"
  cmp -s "$PI" "$WORK/plant-$name.yml" && fail "the plant $name did not change the workflow"
  return 0
}
plant p1 'for f in "${expected[@]}"; do' '              "${expected[0]}" version >/dev/null 2>&1 || true'
plant p2 'docker rm "$cid"' '            for g in "${expected[@]}"; do if "$g" version >/dev/null 2>&1; then :; fi; done'
plant p3 'docker rm "$cid"' '            /home/runner/work/_temp/amd64/wardynd version >/dev/null 2>&1 || true'
plant p4 'docker rm "$cid"' '            (cd /home/runner/work/_temp/amd64 && ./wardynd version) || true'
plant p5 'cosign sign --yes' '      - run: /home/runner/work/_temp/arm64/host/wardyn-linux-amd64 version || true'
plant p6 'cosign sign --yes' '      - name: Look' '        run: echo hi'
for n in p1 p2 p3 p4 p5 p6; do
  if publisher_whole_pin < "$WORK/plant-$n.yml" >/dev/null; then fail "the whole-block pin accepts the plant $n"; fi
done
plant s1 'cosign sign --yes' '        shell: bash -e {0}'
plant s2 'cosign sign --yes' '        env:' '          BASH_ENV: /tmp/evil.sh'
for n in s1 s2; do
  if publisher_whole_pin < "$WORK/plant-$n.yml" >/dev/null; then fail "the whole-job pin accepts the plant $n (a shell line or a BASH_ENV env added to a step)"; fi
done
plant c1 'docker rm "$cid"' '            # a comment'
publisher_whole_pin < "$WORK/plant-c1.yml" >/dev/null || fail "the whole-block pin refuses an added comment"
sed -E 's|(uses: docker/setup-qemu-action)@[0-9a-f]+ # v4|\1@0123456789abcdef0123456789abcdef01234567 # v5|' "$PI" > "$WORK/plant-c2.yml"
cmp -s "$PI" "$WORK/plant-c2.yml" && fail "the version-bump control did not change the workflow"
publisher_whole_pin < "$WORK/plant-c2.yml" >/dev/null || fail "the whole-block pin refuses a version bump of an action"
# One fixture per way the unsigned image's bytes could reach the runner again; each must fail its guard, and the
# workflow as it stands must pass both (above).
mkfix() { # NAME BODY: a workflow whose publishing job runs BODY
  { printf 'permissions:\n  contents: read\n  packages: write\n  id-token: write\njobs:\n  publish-image:\n    runs-on: ubuntu-26.04\n    steps:\n      - name: Check the image reports its revision\n        run: |\n'; cat; } > "$WORK/fix-$1.yml"
}
GOOD_RUN='          run_in_image() {
            docker run --rm --platform linux/amd64 --network none --read-only --cap-drop ALL \
              --security-opt no-new-privileges --entrypoint "$1" "$IMAGE_REF" version
          }'
mkfix good <<<"$GOOD_RUN"
publisher_pin_guard < "$WORK/fix-good.yml" >/dev/null && publisher_host_exec_guard < "$WORK/fix-good.yml" >/dev/null || fail "the guards refuse the safe shape"
mkfix loopvar <<<"$GOOD_RUN
          for f in \"\$RUNNER_TEMP\"/host/wardyn*; do
            \"\$f\" version >/dev/null 2>&1 || true
          done"
if publisher_host_exec_guard < "$WORK/fix-loopvar.yml" >/dev/null; then fail "the exec guard accepts a copied-out file run through a loop variable"; fi
mkfix interp <<<"$GOOD_RUN
          bash \"\$f\""
if publisher_host_exec_guard < "$WORK/fix-interp.yml" >/dev/null; then fail "the exec guard accepts an interpreter run on a copied-out file"; fi
for shape in 'mount|-v /var/run/docker.sock:/var/run/docker.sock' 'home|-v "$HOME:/h"' 'volumeflag|--volume /tmp:/t' 'env|-e GITHUB_TOKEN' 'privileged|--privileged' 'hostnet|--network host' 'capadd|--cap-add SYS_ADMIN'; do
  name="${shape%%|*}"; flag="${shape#*|}"
  mkfix "$name" <<<"${GOOD_RUN/--network none/--network none $flag}"
  if publisher_pin_guard < "$WORK/fix-$name.yml" >/dev/null; then fail "the docker run guard accepts run_in_image with: $flag"; fi
done
for dropped in '--read-only ' '--cap-drop ALL ' '--network none '; do
  mkfix dropped <<<"${GOOD_RUN/$dropped/}"
  if publisher_pin_guard < "$WORK/fix-dropped.yml" >/dev/null; then fail "the docker run guard accepts run_in_image without: $dropped"; fi
done
mkfix nonew <<<"${GOOD_RUN/--security-opt no-new-privileges /}"
if publisher_pin_guard < "$WORK/fix-nonew.yml" >/dev/null; then fail "the docker run guard accepts run_in_image without no-new-privileges"; fi
mkfix second <<<"$GOOD_RUN
          docker run --rm -v /:/host \"\$IMAGE_REF\""
if publisher_pin_guard < "$WORK/fix-second.yml" >/dev/null; then fail "the docker run guard accepts a second docker run outside run_in_image"; fi
# A command taken out is as much a change as one put in: with the real workflow, every known line is removed in turn
# and each removal must fail, and run_in_image defined a second time (even identically) must fail too.
while IFS= read -r known; do
  [ -n "$known" ] || continue
  KNOWN="$known" awk '{ t = $0; sub(/^[[:space:]]+/, "", t); sub(/[[:space:]]+$/, "", t); if (t != ENVIRON["KNOWN"]) print }' "$PI" > "$WORK/fix-missing.yml"
  cmp -s "$PI" "$WORK/fix-missing.yml" && fail "the missing-command control did not remove: $known"
  if PIN_REQUIRE=1 publisher_pin_guard < "$WORK/fix-missing.yml" >/dev/null; then fail "the pin guard accepts the publishing job without: $known"; fi
done <<< "$PIN_DOCKER
$PIN_FILES"
mkfix twice <<<"$GOOD_RUN
$GOOD_RUN"
if publisher_pin_guard < "$WORK/fix-twice.yml" >/dev/null; then fail "the pin guard accepts run_in_image defined twice"; fi
mkfix twice-other <<<"$GOOD_RUN
${GOOD_RUN/--network none /}"
if publisher_pin_guard < "$WORK/fix-twice-other.yml" >/dev/null; then fail "the pin guard accepts a second, different run_in_image"; fi

# The image check itself, run with a stub docker: a failing version command and an empty answer each fail it, on a branch
# and on a tag, and a tag must report exactly its release version.
awk '
  /^      - name: Check the image reports its revision$/ { want = 1; next }
  want && /^        run: \|$/ { grab = 1; next }
  grab && /^          / { sub(/^          /, ""); print > (dir "/check.sh"); next }
  grab { grab = 0; want = 0 }
' dir="$WORK" "$PI"
[ -s "$WORK/check.sh" ] || fail "no 'Check the image reports its revision' step in publish-image.yml"
mkdir -p "$WORK/chk/bin"
cat > "$WORK/chk/bin/docker" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  run)
    entry=""; prev=""
    for a in "$@"; do [ "$prev" = "--entrypoint" ] && entry="$a"; prev="$a"; done
    printf '%s' "$STUB_VERSION"
    # A failing command still printed a plausible version: only its exit status can fail the check.
    case "$entry" in
      *"$STUB_FAIL"*) [ -z "$STUB_FAIL" ] || exit 3;;
    esac;;
  create) # the container id names the platform it was created for, so a copy knows which child image it reads
    plat=""; prev=""
    for a in "$@"; do [ "$prev" = "--platform" ] && plat="$a"; prev="$a"; done
    echo "cid-${plat#linux/}";;
  cp) arch="${2%%:*}"; arch="${arch#cid-}"
      # STUB_OMIT and STUB_LINK are ARCH:FILE: the file is left out of, or made a link in, that platform'"'"'s image only.
      case "$2" in
        */.) for f in wardyn wardyn-linux-amd64 wardyn-linux-arm64 wardyn-darwin-amd64 wardyn-darwin-arm64; do
               [ "$arch:$f" = "$STUB_OMIT" ] || printf '%s' "$STUB_BYTES" > "$3/$f"
             done
             # A name made a link to another file is present but not a copied-out regular file.
             [ "${STUB_LINK%%:*}" != "$arch" ] || { rm -f "$3/${STUB_LINK#*:}"; ln -s wardyn "$3/${STUB_LINK#*:}"; };;
        *) [ "$arch:wardynd" = "$STUB_OMIT" ] || printf '%s' "$STUB_BYTES" > "$3";;
      esac;;
esac
STUB
chmod +x "$WORK/chk/bin/docker"
runcheck() { # WANT(0|1) REVISION REF_NAME VERSION FAIL-ENTRYPOINT-SUBSTRING NAME [BYTES-OF-THE-COPIED-FILES]; STUB_OMIT / STUB_LINK from the caller
  rm -rf "$WORK/chk/tmp"; mkdir "$WORK/chk/tmp"; local rc=0
  PATH="$WORK/chk/bin:$PATH" RUNNER_TEMP="$WORK/chk/tmp" IMAGE_REF=img@sha256:x REVISION="$2" REF_NAME="$3" \
    STUB_VERSION="$4" STUB_FAIL="$5" STUB_BYTES="${7-stamped $2}" STUB_OMIT="${STUB_OMIT:-}" STUB_LINK="${STUB_LINK:-}" bash "$WORK/check.sh" >"$WORK/chk/out" 2>&1 || rc=$?
  if [ "$1" = 0 ] && [ "$rc" -ne 0 ]; then fail "image check refused a good case ($6): $(cat "$WORK/chk/out")"; fi
  if [ "$1" = 1 ] && [ "$rc" -eq 0 ]; then fail "image check accepted a bad case: $6"; fi
}
runcheck 0 "$sha" main "0.10.1+111111111" "" "branch, reports the commit"
runcheck 1 "$sha" main "0.10.1+111111111" "/usr/local/bin/wardyn" "branch, the in-container CLI fails"
runcheck 1 "$sha" main "0.10.1+111111111" "/host/wardyn" "branch, the host CLI fails"
runcheck 1 "$sha" main "" "" "branch, empty output"
runcheck 1 "$sha" main "0.10.1" "" "branch, bare version"
runcheck 1 "$sha" main "0.10.1+222222222" "" "branch, another commit"
runcheck 1 "$sha" main "0.10.1+111111111" "" "branch, a copied-out binary without the commit" "no stamp in these bytes"
# Every file the image is built to carry must be there, each a regular file: one missing from an otherwise valid
# set, or a link in its place, fails even though every file that is there carries the commit.
# The two Linux children of the index are each read, so the same holds of the arm64 child, which nothing runs, and of a
# tag ref, which has no commit to look for but is built to carry the same files.
for arch in amd64 arm64; do
  for omitted in wardynd wardyn wardyn-linux-amd64 wardyn-linux-arm64 wardyn-darwin-amd64 wardyn-darwin-arm64; do
    STUB_OMIT="$arch:$omitted" runcheck 1 "$sha" main "0.10.1+111111111" "" "branch, $omitted missing from the $arch image"
    STUB_OMIT="$arch:$omitted" runcheck 1 "" v0.4.4 "0.4.4" "" "tag, $omitted missing from the $arch image"
  done
  STUB_LINK="$arch:wardyn-darwin-arm64" runcheck 1 "$sha" main "0.10.1+111111111" "" "branch, a host CLI that is a link in the $arch image"
  STUB_LINK="$arch:wardyn-darwin-arm64" runcheck 1 "" v0.4.4 "0.4.4" "" "tag, a host CLI that is a link in the $arch image"
done
runcheck 0 "" v0.4.4 "0.4.4" "" "tag, binaries that carry no commit are not looked for one" "no stamp in these bytes"
runcheck 0 "" v0.4.4 "0.4.4" "" "tag, the release version"
runcheck 1 "" v0.4.4 "0.4.4" "/usr/local/bin/wardyn" "tag, the in-container CLI fails"
runcheck 1 "" v0.4.4 "0.4.4" "/host/wardyn" "tag, the host CLI fails"
runcheck 1 "" v0.4.4 "" "" "tag, empty output"
runcheck 1 "" v0.4.4 "0.4.4+111111111" "" "tag, a commit suffix"
runcheck 1 "" v0.4.4 "0.4.3" "" "tag, another version"

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

# scripts/float-latest.sh and its wiring in release.yml: the one place `:latest` on the float-latest rows is written.
# git, curl and cosign are stubs on PATH, so nothing here touches a network or a registry.
command -v yq >/dev/null && command -v jq >/dev/null || { echo "test-release-image-tag: yq and jq are required" >&2; exit 1; }
FL="$WORK/fl"
mkdir -p "$FL/bin"
cat >"$FL/bin/git" <<'STUB'
#!/usr/bin/env bash
echo "git $*" >>"$FL/calls"
[ "$1" = ls-remote ] || exit 99
while read -r t; do printf '0000000000000000000000000000000000000000\trefs/tags/%s\n' "$t"; done <"$FL/tags"
STUB
cat >"$FL/bin/curl" <<'STUB'
#!/usr/bin/env bash
echo "curl $*" >>"$FL/calls"
url=""
for a in "$@"; do case "$a" in https://*) url="$a" ;; esac; done
case "$url" in
  https://ghcr.io/token\?*) echo '{"token":"t"}'; exit 0 ;;
  https://ghcr.io/v2/cjohnstoniv/*/manifests/*)
    r="${url#https://ghcr.io/v2/cjohnstoniv/}"; img="${r%%/*}"; ref="${r##*/}"
    code=404; digest=""
    if [ -f "$FL/reg/$img/$ref" ]; then code=200; digest="$(cat "$FL/reg/$img/$ref")"; fi
    [ -f "$FL/reg/$img/$ref.code" ] && code="$(cat "$FL/reg/$img/$ref.code")"
    if [ -n "$digest" ]; then printf 'HTTP/2 %s\r\ndocker-content-digest: %s\r\n\r\n\n%s' "$code" "$digest" "$code"
    else printf 'HTTP/2 %s\r\n\r\n\n%s' "$code" "$code"; fi
    exit 0 ;;
esac
exit 99
STUB
cat >"$FL/bin/cosign" <<'STUB'
#!/usr/bin/env bash
echo "cosign $*" >>"$FL/calls"
case "$1" in
  verify)
    [ "$3" = --certificate-identity ] && [ "$5" = --certificate-oidc-issuer ] || exit 99
    [ "$6" = https://token.actions.githubusercontent.com ] || exit 1
    tag="${4##*refs/tags/}"
    [ -f "$FL/signed/${2#*@}" ] && grep -qxF "$tag" "$FL/signed/${2#*@}" ;;
  copy)
    [ "$2" = --force ] && [ "$#" -eq 4 ] || exit 99
    echo "$*" >>"$FL/copies"
    dest="${4%:latest}"; mkdir -p "$FL/reg/${dest##*/}"
    if [ -n "${COPY_WRONG:-}" ]; then echo "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" >"$FL/reg/${dest##*/}/latest"
    else echo "${3#*@}" >"$FL/reg/${dest##*/}/latest"; fi ;;
  *) exit 99 ;;
esac
STUB
chmod +x "$FL/bin/git" "$FL/bin/curl" "$FL/bin/cosign"

d() { printf 'sha256:%063d%s' 0 "$1"; }
D1="$(d 1)"; D2="$(d 2)"; D3="$(d 3)"; D4="$(d 4)"; D5="$(d 5)"
IMG=ghcr.io/cjohnstoniv
fl_reset() { rm -rf "$FL/reg" "$FL/signed" "$FL/tags" "$FL/copies" "$FL/calls" "$FL/out" "$FL/err"; mkdir -p "$FL/reg" "$FL/signed"; : >"$FL/tags"; unset FL_COPY_WRONG; }
fl_tag() { printf '%s\n' "$@" >>"$FL/tags"; }
fl_image() { # IMAGE VERSION DIGEST: the version's tag in the registry, signed by the run for v<VERSION>
  mkdir -p "$FL/reg/$1"; echo "$3" >"$FL/reg/$1/$2"; echo "v$2" >>"$FL/signed/$3"
}
fl_latest() { mkdir -p "$FL/reg/$1"; echo "$2" >"$FL/reg/$1/latest"; }
fl_run() { # DRY (empty: unset) REF IMAGE...
  local dry="$1"; shift
  FL_RC=0
  if [ -n "$dry" ]; then
    env PATH="$FL/bin:$PATH" FL="$FL" COPY_WRONG="${FL_COPY_WRONG:-}" DRY_RUN="$dry" bash "$ROOT/scripts/float-latest.sh" "$@" >"$FL/out" 2>"$FL/err" || FL_RC=$?
  else
    env -u DRY_RUN PATH="$FL/bin:$PATH" FL="$FL" bash "$ROOT/scripts/float-latest.sh" "$@" >"$FL/out" 2>"$FL/err" || FL_RC=$?
  fi
}
fl_rc() { [ "$FL_RC" -eq "$2" ] || fail "float-latest $1: rc $FL_RC, want $2: $(cat "$FL/out" "$FL/err")"; }
fl_out() { grep -qF -- "$2" "$FL/out" || fail "float-latest $1: stdout lacks [$2]: $(cat "$FL/out")"; }
fl_err() { grep -qF -- "$2" "$FL/err" || fail "float-latest $1: stderr lacks [$2]: $(cat "$FL/err")"; }
fl_copies() { # NAME EXPECTED_LINE...: $FL/copies holds exactly these lines (none: no copy)
  local n="$1" got want; shift
  got="$(cat "$FL/copies" 2>/dev/null || true)"; want="$(printf '%s\n' "$@")"; [ "$#" -gt 0 ] || want=""
  [ "$got" = "$want" ] || fail "float-latest $n: copies [$got], want [$want]"
}
fl_copy_line() { echo "copy --force $IMG/$1@$2 $IMG/$1:latest"; }
fl_case1() { fl_reset; fl_tag v0.10.9 v0.11.0; fl_image agent-base 0.10.9 "$D1"; fl_image agent-base 0.11.0 "$D2"; fl_latest agent-base "$D2"; }
fl_case2() { fl_case1; fl_tag v0.11.1; fl_image agent-base 0.11.1 "$D3"; }

# 1 an older-line final does not float
fl_case1; fl_run false v0.10.9 agent-base; fl_rc 1 0
fl_out 1 "PLAN agent-base latest=$D2 target=v0.11.0 $D2 action=keep"; fl_copies 1
[ "$(cat "$FL/reg/agent-base/latest")" = "$D2" ] || fail "float-latest 1: :latest moved"
# 2 the newest final floats
fl_case2; fl_run false v0.11.1 agent-base; fl_rc 2 0
fl_out 2 "PLAN agent-base latest=$D2 target=v0.11.1 $D3 action=move"; fl_out 2 "moved $IMG/agent-base:latest -> $D3"
fl_copies 2 "$(fl_copy_line agent-base "$D3")"
# 3 inverted completion: the lower final's job finishes after the higher's
fl_case2; fl_latest agent-base "$D3"; fl_run false v0.10.9 agent-base; fl_rc 3 0
fl_out 3 "target=v0.11.1 $D3 action=keep"; fl_copies 3
# 4 repair: a lower run finds :latest below the highest published final and moves it up
fl_case2; fl_tag v0.10.10; fl_image agent-base 0.10.10 "$D4"; fl_run false v0.10.10 agent-base; fl_rc 4 0
fl_out 4 "target=v0.11.1 $D3 action=move"; fl_copies 4 "$(fl_copy_line agent-base "$D3")"
# 4b numeric order: v0.10.10 is above v0.10.9
fl_reset; fl_tag v0.10.9 v0.10.10; fl_image agent-base 0.10.9 "$D1"; fl_image agent-base 0.10.10 "$D4"; fl_latest agent-base "$D1"
fl_run false v0.10.9 agent-base; fl_rc 4b 0
fl_out 4b "PLAN agent-base latest=$D1 target=v0.10.10 $D4 action=move"; fl_copies 4b "$(fl_copy_line agent-base "$D4")"
# 5 a tag whose image is not published yet is skipped
fl_case2; fl_tag v0.11.2; fl_run false v0.11.1 agent-base; fl_rc 5 0
fl_out 5 "skip agent-base:0.11.2: not published"; fl_copies 5 "$(fl_copy_line agent-base "$D3")"
# 6 an RC ref skips before any read
fl_case2; fl_run false v0.11.2-rc.1 agent-base; fl_rc 6 0
[ "$(cat "$FL/out")" = "float-latest: v0.11.2-rc.1 is a prerelease tag: skipped, no write" ] || fail "float-latest 6: stdout [$(cat "$FL/out")]"
[ ! -s "$FL/calls" ] || fail "float-latest 6: an RC read something: $(cat "$FL/calls")"; fl_copies 6
# 6b a branch ref prints the plan and writes nothing
fl_case2; fl_run true chore/release-0.11.2 agent-base; fl_rc 6b 0
fl_out 6b "PLAN agent-base latest=$D2 target=v0.11.1 $D3 action=move"; fl_out 6b "not a final release tag: no write"; fl_copies 6b
# 7 a dry run writes nothing
fl_case2; fl_run true v0.11.1 agent-base; fl_rc 7 0
fl_out 7 "action=move"; fl_out 7 "DRY RUN: no write"; fl_copies 7
# 8 a candidate whose digest is signed by another tag's run fails closed, with nothing written for any image
fl_case2; rm -f "$FL/signed/$D3"; echo v0.11.0 >"$FL/signed/$D3"
fl_image agent-codex-cli 0.11.1 "$D5"; fl_latest agent-codex-cli "$D2"
fl_run false v0.10.9 agent-codex-cli agent-base; fl_rc 8 1
fl_err 8 "agent-base:0.11.1"; fl_copies 8
# 9 a registry answer other than 200 or 404 fails closed
fl_case2; echo 500 >"$FL/reg/agent-base/0.11.1.code"; fl_run false v0.10.9 agent-base; fl_rc 9 1; fl_copies 9
# 10 nothing published at or above the run's tag
fl_reset; fl_tag v0.11.0 v0.11.1; fl_image agent-base 0.11.0 "$D2"; fl_run false v0.11.1 agent-base; fl_rc 10 1
fl_err 10 "no published final at or above v0.11.1"; fl_copies 10
# 11 a copy that lands on another digest fails
fl_case2; FL_COPY_WRONG=1; fl_run false v0.11.1 agent-base; fl_rc 11 1; fl_err 11 "after the copy, expected $D3"
# 12 a final ref that is not a tag on origin
fl_case1; fl_run false v0.12.0 agent-base; fl_rc 12 1; fl_err 12 "v0.12.0 is not a tag on origin"; fl_copies 12
# 13 each image is decided on its own
fl_case2; fl_image agent-codex-cli 0.11.1 "$D5"; fl_latest agent-codex-cli "$D5"
fl_run false v0.11.1 agent-base agent-codex-cli; fl_rc 13 0
fl_out 13 "PLAN agent-base latest=$D2 target=v0.11.1 $D3 action=move"; fl_out 13 "PLAN agent-codex-cli latest=$D5 target=v0.11.1 $D5 action=keep"
fl_copies 13 "$(fl_copy_line agent-base "$D3")"
# 14 no DRY_RUN is a usage error
fl_case2; fl_run "" v0.11.1 agent-base; fl_rc 14 2; fl_copies 14

# The wiring in release.yml. (The two ui-sandbox jobs share their steps by a YAML anchor, so the one allowed line is read twice: sort -u.)
[ "$(yq -r '.jobs["float-latest"].concurrency.group' "$WF")" = release-floating-aliases ] || fail "float-latest job is not in the release-floating-aliases group"
[ "$(yq -r '.jobs["float-latest"].concurrency["cancel-in-progress"]' "$WF")" = false ] || fail "the float-latest group cancels in-progress runs"
for j in preflight-green images promote; do
  [ "$(yq -r '.jobs["float-latest"].needs | contains(["'"$j"'"])' "$WF")" = true ] || fail "float-latest does not need $j"
done
[ "$(yq -r '.jobs["float-latest"].steps[-1].env.DRY_RUN' "$WF")" = "\${{ inputs.dry_run == true || !startsWith(github.ref, 'refs/tags/v') }}" ] || fail "the float-latest step's DRY_RUN is not the tag-ref and dry_run guard"
yq -r '.jobs["float-latest"].steps[-1].run' "$WF" >"$FL/step.sh"
grep -qF 'scripts/float-latest.sh "$REF_NAME"' "$FL/step.sh" || fail "the float-latest step does not run scripts/float-latest.sh with REF_NAME"
[ "$(yq -r '.jobs.promote.env.IMAGES' "$WF" | yq -r '.[] | select(.["float-latest"] == "true") | .name' | sort | tr '\n' ' ')" = "agent-aws-sso agent-base agent-codex-cli " ] || fail "the promote rows that float :latest are not agent-aws-sso agent-base agent-codex-cli"
others="$(yq -r '.jobs | to_entries | .[] | select(.key != "float-latest") | .value.steps[]? | (.run // ""), (.with.tags // "")' "$WF" | grep -v '^[[:space:]]*#' | grep ':latest' | sed 's/^[[:space:]]*//' | sort -u || true)"
[ "$others" = 'echo "ref=ghcr.io/cjohnstoniv/${BASE_NAME}:latest" >> "$GITHUB_OUTPUT"' ] || fail "a job other than float-latest writes :latest: [$others]"

echo "test-release-image-tag: ok"
