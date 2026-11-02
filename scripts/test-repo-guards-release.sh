#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Guards 26-29 and 31 of scripts/test-repo-guards.sh: the release.yml and nightly.yml checks (CLI targets,
# Go module seeding, watched kind walks, write steps tied to a release tag ref, the guest agent's stamp). They sit in their own
# file to keep test-repo-guards.sh under the file-size limit. Not run on its own: test-repo-guards.sh
# sources it after guard 25, so these guards share its fail, bad, ok, REL, NIGHTLY and NOTIFY_EXEMPT.

# ── 26. release.yml's CLI targets, upload steps and asserted assets agree ────
# The binaries job builds every TARGETS entry in one step, but each target has its
# own fixed upload step and its own line in the release-asset assert. A target in
# one list and not the others builds a CLI that never ships, or fails only on the
# tag. Windows ships wardyn-windows-<arch>.exe.
cli_targets="$(sed -n 's/^ *TARGETS: "\(.*\)"$/\1/p' "$REL" | tr ' ' '\n' | sed '/^$/d; s#/#-#' | sort)"
cli_uploads="$(sed -n 's/^ *name: cli-\([a-z0-9]*-[a-z0-9]*\)$/\1/p' "$REL" | sort)"
cli_asserted="$(sed -n '/Assert every required asset actually landed/,/done$/p' "$REL" \
    | grep -oE 'wardyn-(linux|darwin|windows)-[a-z0-9]+(\.exe)?' | sed 's/^wardyn-//' | sort -u)"
cli_want_assert="$(printf '%s\n' "$cli_targets" | sed '/^windows-/s/$/.exe/' | sort)"
if [ -z "$cli_targets" ]; then
    bad "release.yml: no binaries TARGETS line found — guard 26 checks nothing"
elif [ "$cli_uploads" != "$cli_targets" ] || [ "$cli_asserted" != "$cli_want_assert" ]; then
    bad "release.yml: CLI TARGETS ($(echo $cli_targets)) != upload steps cli-* ($(echo $cli_uploads)) or asserted assets ($(echo $cli_asserted)); a target needs its cli-<os>-<arch> upload step and its wardyn-<os>-<arch> (.exe on windows) assert line"
else
    ok "release.yml's $(echo "$cli_targets" | wc -l) CLI targets each have an upload step and an asserted release asset"
fi

# ── 27. nightly image builds take their Go modules from the job, not the proxy ─
# #3469: every buildx build re-downloaded all Go modules, so one module-proxy stream
# error failed a whole multi-arch build. The nightly job fills the module cache once and
# passes it as the `gomodcache` build context, and each Go Dockerfile it builds declares
# that stage (empty unless passed). release.yml keeps the cold fetch (release
# reproducibility, DECISIONS-010.md:1137), so it must pass no build-contexts at all.
gm_fail=0
gm_bps='.steps[] | select((.uses // "") | test("^docker/build-push-action@"))'
gm_ctx="$(yq -r ".jobs[\"buildx-smoke\"] | $gm_bps | .with[\"build-contexts\"] // \"\"" "$NIGHTLY")"
case "$gm_ctx" in
    "$WANT_CTX") ;;
    *) bad "$NIGHTLY: buildx-smoke's build step does not pass the filled Go module cache as 'gomodcache=' build-contexts (got '$gm_ctx') — each image build would fetch every module from the proxy (#3469)"; gm_fail=1 ;;
esac
gm_fill="$(yq -r '.jobs["buildx-smoke"].steps[] | select(.id == "gomod") | .run' "$NIGHTLY")"
case "$gm_fill" in
    *"go mod download"*"cache/download"*) ;;
    *) bad "$NIGHTLY: buildx-smoke has no step id 'gomod' that runs go mod download and outputs the cache/download dir (#3469)"; gm_fail=1 ;;
esac
gm_fill_at="$(yq -r '.jobs["buildx-smoke"].steps | to_entries | .[] | select(.value.id == "gomod") | .key' "$NIGHTLY")"
gm_build_at="$(yq -r ".jobs[\"buildx-smoke\"].steps | to_entries | .[] | select((.value.uses // \"\") | test(\"^docker/build-push-action@\")) | .key" "$NIGHTLY")"
if [ -z "$gm_fill_at" ] || [ -z "$gm_build_at" ] || [ "$gm_fill_at" -ge "$gm_build_at" ]; then
    bad "$NIGHTLY: buildx-smoke's gomod fill step must come before its build step (fill at '${gm_fill_at}', build at '${gm_build_at}')"; gm_fail=1
fi
gm_rows="$(yq -r '.jobs["buildx-smoke"].strategy.matrix.include[] | .name + " " + .dockerfile + " " + (.gomod // "")' "$NIGHTLY")"
while read -r gm_name gm_df gm_flag; do
    [ -n "$gm_name" ] || continue
    if grep -qE '^[^#]*go mod download' "$gm_df"; then
        [ "$gm_flag" = true ] || { bad "$NIGHTLY: buildx-smoke row '$gm_name' builds $gm_df, which downloads Go modules, but its gomod flag is not \"true\" — it would fetch cold"; gm_fail=1; }
        grep -q '^FROM scratch AS gomodcache$' "$gm_df" && grep -q 'from=gomodcache' "$gm_df" \
            || { bad "$gm_df: downloads Go modules but lacks the 'gomodcache' stage and its bind mount — the nightly seed would be ignored (#3469)"; gm_fail=1; }
    fi
done <<<"$gm_rows"
if [ "$(yq -r '[.jobs[].steps[]? | select((.uses // "") | test("^docker/build-push-action@")) | select(.with["build-contexts"])] | length' "$REL")" != 0 ]; then
    bad "$REL: a docker/build-push-action step passes build-contexts — release keeps the cold Go module fetch (DECISIONS-010.md:1137)"; gm_fail=1
fi
# Every Dockerfile that downloads Go modules retries the fetch (#3469); vm-linux, added after it, did not.
while read -r gm_df; do
    if grep -E '^[^#]*go mod download' "$gm_df" | grep -qv 'for i in 1 2 3; do go mod download && break'; then
        bad "$gm_df: 'go mod download' outside the three-try retry loop, so one module-proxy stream error fails the build (#3469)"; gm_fail=1
    fi
done < <(git ls-files '*Dockerfile*')
if [ "$gm_fail" = 0 ]; then ok "image builds retry the Go module fetch, nightly ones seed it from the job; release.yml builds cold"; fi

# ── 28. the kind walks stay watched, matrix legs included ────────────────────
# #3439: green-by-tree.sh reads a watched matrix job through its "name (leg)" rows, so
# k6-kind-conformance and kind-acceptance-walks (matrix jobs) and the three single walks that
# were exempt only until they had gone green are on release.yml's watched=, and none is back in
# NOTIFY_EXEMPT. A job with a matrix and no `name:` still reports "<id> (<leg>)", so the id prefixes the leg.
walk_fail=0
walk_watched="$(yq -r '.jobs["preflight-green"].steps[].run // ""' "$REL" | sed -n 's/^ *watched="\(.*\)"$/\1/p' | head -1)"
for job in kind-survival-walk k6-kind-conformance kind-acceptance-walks hybrid-walk kind-upgrade-walk; do
    case " $walk_watched " in *" $job "*) ;; *) bad "$REL: preflight-green's watched= lacks '$job' (#3439)"; walk_fail=1 ;; esac
    case " $NOTIFY_EXEMPT " in *" $job "*) bad "scripts/test-repo-guards.sh: NOTIFY_EXEMPT names '$job', which green-by-tree.sh can watch since #3439"; walk_fail=1 ;; esac
    case "$(yq -r ".jobs[\"$job\"].name // \"$job\"" "$NIGHTLY")" in
        "$job"|"$job ("*) ;;
        *) bad "$NIGHTLY: job '$job' has a name that is neither '$job' nor '$job (<leg>)' — green-by-tree.sh cannot match its checks"; walk_fail=1 ;;
    esac
done
if [ "$walk_fail" = 0 ]; then ok "the five kind walks are watched by release.yml and none is exempt"; fi

# ── 29. release.yml writes only on a release tag ref ─────────────────────────
# A step that leaves the runner (registry push or copy, signature, attestation, Release upload,
# notarization) must run only when the ref is a vX.Y.Z tag, so a dispatch off a branch, or a fork's
# branch, can publish nothing even with dry_run=false. A step counts as guarded only when
#   (i)   its own `if`, or its job's `if`, holds startsWith(github.ref, 'refs/tags/v') un-negated,
#         with no `||` anywhere in that expression; or
#   (ii)  its `with.push` holds the same, again with no `||`; or
#   (iii) its OWN env sets DRY_RUN to `inputs.dry_run == true || !startsWith(...)` or PUBLISH to
#         `inputs.dry_run != true && startsWith(...)`, exactly, and its run text reads that variable.
# A job-level env never stands in for a step: a write step added to a job must carry its own guard.
# The yq pipeline prints one row per write step with "guarded" true/false and awk filters, because
# yq 4.44's select() on a value that does not depend on `.` keeps every row.
release_write_steps() { # <workflow> -> "<guarded>\t<job>\t<step>" for each step that runs a write command
    WRITE_RE='docker( buildx)?( [a-z-]+)* push|--push|imagetools +create|(^|[^A-Za-z0-9_-])cosign +(sign|sign-blob|attest|attach|copy)([^A-Za-z0-9_-]|$)|(^|[^A-Za-z0-9_-])crane +(copy|cp|tag|push|mutate)([^A-Za-z0-9_-]|$)|skopeo +copy|(^|[^A-Za-z0-9_-])regctl([^A-Za-z0-9_-]|$)|gh +release +(create|edit|upload|delete)|gh +api .*(-X|--method)[ =]*(POST|PUT|PATCH|DELETE)|notarytool|git +push|oras +push|helm +push|build-runner-pkg\.sh' \
    TAG_POS="(^|[^!])startsWith\\(github\\.ref, 'refs/tags/v'\\)" \
    ENV_DRY="\${{ inputs.dry_run == true || !startsWith(github.ref, 'refs/tags/v') }}" \
    ENV_PUBLISH="\${{ inputs.dry_run != true && startsWith(github.ref, 'refs/tags/v') }}" \
    yq -r '
      .jobs | to_entries[] | .key as $j
      | (.value.if // "" | tostring) as $jif
      | .value.steps[]?
      | ((.run // "x") | split("\n") | map(select(test("^\\s*#") | not)) | join("\n")) as $code
      | ($code | test(strenv(WRITE_RE))) as $w
      | ((.uses // "") | test("^(docker/build-push-action|actions/attest-build-provenance)@")) as $u
      | (.if // "" | tostring) as $sif
      | (.with.push // "" | tostring) as $push
      | ((($sif | test(strenv(TAG_POS))) and (($sif | test("[|][|]")) | not))
         or (($jif | test(strenv(TAG_POS))) and (($jif | test("[|][|]")) | not))
         or (($push | test(strenv(TAG_POS))) and (($push | test("[|][|]")) | not))
         or ((.env.DRY_RUN // "" | tostring) == strenv(ENV_DRY) and ($code | test("[$][{]?DRY_RUN([^A-Za-z0-9_]|$)")))
         or ((.env.PUBLISH // "" | tostring) == strenv(ENV_PUBLISH) and ($code | test("[$][{]?PUBLISH([^A-Za-z0-9_]|$)")))) as $g
      | [($w or $u), $g, $j, (.name // .uses // "unnamed step")] | @tsv' "$1" \
    | awk -F'\t' '$1 == "true" { print $2 "\t" $3 "\t" $4 }'
}
unguarded_release_writes() { release_write_steps "$1" | awk -F'\t' '$1 == "false" { print $2 " / " $3 }'; }
tag_fail=0
planted="$(mktemp -d)"
cat >"$planted/w.yml" <<'PLANT'
jobs:
  steps-ok:
    steps:
      - name: sign on a tag
        if: inputs.dry_run != true && startsWith(github.ref, 'refs/tags/v')
        run: cosign sign --yes "$REF"
      - name: build and push on a tag
        uses: docker/build-push-action@v7
        with:
          push: ${{ inputs.dry_run != true && startsWith(github.ref, 'refs/tags/v') }}
      - name: guarded through its own env
        env:
          DRY_RUN: ${{ inputs.dry_run == true || !startsWith(github.ref, 'refs/tags/v') }}
        run: DRY_RUN="$DRY_RUN" scripts/build-runner-pkg.sh --out x.pkg
      - name: publish flag
        env:
          PUBLISH: ${{ inputs.dry_run != true && startsWith(github.ref, 'refs/tags/v') }}
        run: |
          if [ "$PUBLISH" != "true" ]; then exit 0; fi
          helm push x.tgz oci://ghcr.io/x
      - name: reads and comments are not writes
        run: |
          # cosign sign would go here
          gh release view "$TAG"
          gh api repos/x/y --jq .id
          cosign verify "$REF"
  job-ok:
    if: ${{ !cancelled() && startsWith(github.ref, 'refs/tags/v') }}
    steps:
      - name: upload
        run: gh release upload "$TAG" assets/*
  bad-sign:
    steps:
      - name: sign anywhere
        if: inputs.dry_run != true
        run: cosign sign --yes "$REF"
  bad-or:
    steps:
      - name: sign on a tag or a real run
        if: inputs.dry_run != true || startsWith(github.ref, 'refs/tags/v')
        run: cosign sign --yes "$REF"
  bad-negated:
    steps:
      - name: sign off a tag
        if: '!startsWith(github.ref, ''refs/tags/v'')'
        run: cosign sign --yes "$REF"
  bad-push:
    steps:
      - uses: docker/build-push-action@v7
        with:
          push: ${{ inputs.dry_run != true }}
  bad-script:
    steps:
      - name: notarize
        run: |
          xcrun notarytool submit x.pkg --wait
  bad-job-env:
    env:
      DRY_RUN: ${{ inputs.dry_run == true || !startsWith(github.ref, 'refs/tags/v') }}
    steps:
      - name: sign with only a job-level env
        run: cosign sign --yes "$REF"
  bad-env-unread:
    steps:
      - name: env set but never read
        env:
          DRY_RUN: ${{ inputs.dry_run == true || !startsWith(github.ref, 'refs/tags/v') }}
        run: cosign sign --yes "$REF"
  bad-other-writers:
    steps:
      - name: imagetools
        run: docker buildx imagetools create -t ghcr.io/x/y:1 ghcr.io/x/y@sha256:0
      - name: crane
        run: crane copy a b
      - name: skopeo
        run: skopeo copy docker://a docker://b
      - name: regctl
        run: regctl image copy a b
      - name: gh api post
        run: gh api -X POST repos/x/y/releases
      - name: attach
        run: cosign attach sbom --sbom s.json "$REF"
PLANT
planted_hits="$(unguarded_release_writes "$planted/w.yml" | sort | tr '\n' '|')"
planted_want="bad-env-unread / env set but never read|bad-job-env / sign with only a job-level env|bad-negated / sign off a tag|bad-or / sign on a tag or a real run|bad-other-writers / attach|bad-other-writers / crane|bad-other-writers / gh api post|bad-other-writers / imagetools|bad-other-writers / regctl|bad-other-writers / skopeo|bad-push / docker/build-push-action@v7|bad-script / notarize|bad-sign / sign anywhere|"
if [ "$planted_hits" != "$planted_want" ]; then
    bad "guard 29 self-test: the detector found '$planted_hits', wanted '$planted_want' (guarded steps, reads and comments must pass; every planted write must be named)"; tag_fail=1
fi
rm -rf "$planted"
rel_writes="$(release_write_steps "$REL" | wc -l)"
rel_loose="$(unguarded_release_writes "$REL" | tr '\n' ';')"
if [ "$rel_writes" -lt 20 ]; then
    bad "$REL: guard 29 found only $rel_writes write steps (expected 20 or more) — the detector no longer reads the workflow"; tag_fail=1
fi
if [ -n "$rel_loose" ]; then
    bad "$REL: write steps with no un-negated startsWith(github.ref, 'refs/tags/v') on the step's or job's if (no ||), its with.push, or its own DRY_RUN/PUBLISH env that its script reads: $rel_loose — add it, ANDed with the dry_run condition"; tag_fail=1
fi
if [ "$tag_fail" = 0 ]; then ok "all $rel_writes release.yml write steps are tied to a release tag ref (self-test catches $(printf %s "$planted_want" | tr -cd "|" | wc -c) planted violations)"; fi

# ── 31. release.yml reads the release stamp back from the packaged guest agent ─
# #3636: the agent has no version command and -trimpath hides -ldflags from the build info, so the
# package step could not tell a stamped agent from an unstamped one. runner-pkg's last step before the
# upload runs scripts/check-release-stamp.sh on the agent inside the expanded package; the script's own
# test (stub `go`) proves it refuses an unstamped binary.
stamp_run="$(yq -r '.jobs["runner-pkg"].steps[] | select(.name == "The packaged guest agent carries the release stamp") | .run' "$REL")"
stamp_at="$(yq -r '.jobs["runner-pkg"].steps | to_entries | .[] | select(.value.name == "The packaged guest agent carries the release stamp") | .key' "$REL")"
pkg_at="$(yq -r '.jobs["runner-pkg"].steps | to_entries | .[] | select((.value.name // "") | test("^Build, sign and notarize")) | .key' "$REL")"
case "$stamp_run" in
    *'*/Payload/usr/local/wardyn/bin/wardyn-guest-agent'*'scripts/check-release-stamp.sh "$agent"'*) stamp_ok=1 ;;
    *) stamp_ok=0 ;;
esac
if [ "$stamp_ok" != 1 ] || [ -z "$stamp_at" ] || [ -z "$pkg_at" ] || [ "$stamp_at" -le "$pkg_at" ]; then
    bad "$REL: runner-pkg has no step after the package build that runs scripts/check-release-stamp.sh on the packaged wardyn-guest-agent (#3636) — a release could ship an agent without the stamp"
elif ! bash "$ROOT/scripts/test-check-release-stamp.sh" >/dev/null 2>&1; then
    bad "scripts/test-check-release-stamp.sh failed — check-release-stamp.sh no longer tells a stamped binary from an unstamped one (#3636)"
else
    ok "release.yml checks the packaged guest agent's release stamp, and the check refuses an unstamped binary"
fi
