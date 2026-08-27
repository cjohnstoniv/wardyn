# Releasing Wardyn

Wardyn is **pre-alpha** and does **not** follow semantic versioning yet — interfaces
are not stable, so a minor bump may still carry breaking changes (see the CHANGELOG
header). Releases are cut **manually** by the maintainer; there is no `make release`
target and no workflow that cuts a tag or a Release for you (`release.yml` only
reacts to a tag you push) — **nothing here is automated to push anything**. This
document is that process, written down.

## Prerequisites

- You are the maintainer (see [MAINTAINERS.md](MAINTAINERS.md)); releases push tags to
  `origin`, so only someone with push rights cuts them.
- The full CI gate is green on the commit you intend to tag. The gate is the
  `.github/workflows/ci.yml` job list: `build`, `diagrams`, `ui`, `ui-e2e`,
  `helm`, `helm-install-test`, `compose`, `conformance`, `conformance-k8s`,
  `envbuild-integration`, `test-pg`, `screenshots-fresh` (PR-only), `gates`
  (a matrix job: `govulncheck`, `staticcheck`, `licenses`,
  `license-headers`, `gitleaks`), `dco`, `desktop-envelope`, `buildx-smoke`,
  `trivy`, and **`notices`** — the copyleft / unreviewed-dependency gate, which
  was missing from this list entirely. `sbom-stub` used to be named here and is
  **gone**: it was deleted along with `make sbom` (CHANGELOG, *Removed*), so a
  maintainer following this list literally was waiting on a phantom job while
  skipping the one that catches a GPL regression. Two more publish
  workflows are not part of this job list at all (see "Container images"
  below): `publish-image` (`.github/workflows/publish-image.yml`, push to
  `main` only) and `release` (`.github/workflows/release.yml`, triggered by
  step 5's tag push itself, so it cannot be a prerequisite of tagging).

Run the local gate first:

```bash
WARDYN_TEST_PG=postgres://... make release-check   # runs `make ci`, plus the Postgres
                                                   # lane and the `## [Unreleased]` check
```

Eight CI jobs cannot run locally at all, because they need a live daemon or
service: `conformance` (`make test-conformance-docker`), `conformance-k8s`
(`make test-conformance-k8s`, needs a local `kind` cluster + a registered
`k8s`-tagged build), `envbuild-integration` (`make test-envbuild-integration`),
`helm-install-test` (`make helm-install-test`, also needs a local `kind`
cluster), the Playwright `ui-e2e` job, `desktop-envelope` (compose build +
up), `buildx-smoke`, and `trivy` (both docker builds). Without
`WARDYN_TEST_PG` the Postgres suite prints a loud SKIPPED line.

Screenshot freshness is CI-only for a different reason: `ci.yml`'s
`screenshots-fresh` job compares the PR diff, so it can tell "you changed the
console (anything under `ui/src/app` or `ui/src/styles`) without re-shooting
`docs/img`" — a local commit-timestamp test cannot, and cannot be cleared at all
once `make screenshots` re-renders the PNGs byte-identically. Re-shoot with
`make screenshots` when you touch the console, or apply the `no-screenshots`
label (and push again) when the change is provably invisible in the two shots.

`release-check` pushes nothing and tags nothing. A green local run means "no local
reason not to tag", not "CI is green" — check the actual CI run on the commit
before step 3.

## Steps

1. **Update the CHANGELOG.** Rename the working `## [Unreleased]` heading (or add the
   section) to `## [X.Y.Z] — YYYY-MM-DD` in [CHANGELOG.md](CHANGELOG.md), following the
   [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format already in use
   (`### Added` / `### Changed` / `### Fixed`). Keep entries user-facing and specific.
   **Also bump `threatmodel/THREAT-MODEL.md`'s currency line** (`**Version:** v2
   (tracks the shipped codebase; last reviewed at vX.Y.Z)`) to the version you're
   cutting — this has drifted from the shipped version before, twice.

   **Then put a fresh, empty `## [Unreleased]` heading back above the new section.**
   `make release-check` hard-fails if `CHANGELOG.md` has no `## [Unreleased]`
   (`grep -q "## \[Unreleased\]" CHANGELOG.md || exit 1`), so renaming it away and
   not restoring it leaves the gate red for the *next* release — which is a
   confusing failure to debug from the tag commit backwards. Restore it in the same
   commit as the rename.
1b. **Bump the shipped version strings** to `X.Y.Z`, in the same commit as the
   CHANGELOG rename: `internal/version/version.go` (`const Version`),
   `deploy/helm/wardyn/Chart.yaml` (both `version:` AND `appVersion:` — the
   chart-publish job now REFUSES to push if `version:` does not equal the tag,
   since `helm install --version` would otherwise resolve to a different chart
   than the release being cut), and
   `ui/package.json` (`"version"`), and **the pinned `install.sh` release-asset
   URL in `README.md` and in `install.sh`'s own header comment** — those two
   point at the cosign-signed copy rather than tip-of-`main`, so a missed bump
   hands new users the previous release's installer.
   `scripts/test-install-sh.sh` asserts the two agree with each other, but it
   cannot know the tag you are cutting. `cmd/wardyn/version_test.go`'s
   `TestVersionMatchesChangelog`/`TestShippedVersionStringsAgree` enforce that
   all four agree with the CHANGELOG's newest section — but only catch a
   missed bump if `make release-check` runs AFTER this commit; the
   Prerequisites run above only sees the previous release's already-consistent
   versions and passes either way. **Re-run `make release-check` after this
   commit** before tagging.
2. **Commit** the CHANGELOG and version-string bumps together, DCO-signed:
   `git commit -s -m "release: X.Y.Z"`.
3. **Cut (or reuse) the release branch.** Starting with 0.5, every minor
   release lives on a `release/X.Y` branch cut from the release commit:
   `git checkout -b release/X.Y`. The branch is where that minor's patch
   releases come from — fixes land on `main` (or the feature branch) first and
   are cherry-picked onto `release/X.Y`; the branch never takes new features.
4. **Tag** on the release branch: `git tag vX.Y.Z` (tags are `v`-prefixed —
   `v0.1.0` … `v0.4.3`). For a patch release, compute the next patch number
   from the branch's own tags rather than by hand — auto-increment, so two
   people cutting patches never collide:

   ```sh
   git checkout release/X.Y
   LAST=$(git tag -l "vX.Y.*" --sort=-v:refname | head -1)   # e.g. vX.Y.3
   NEXT="vX.Y.$(( ${LAST##*.} + 1 ))"                        # -> vX.Y.4
   git tag "$NEXT"
   ```

   (Bump `internal/version/version.go` + the CHANGELOG section on the release
   branch in the same stroke — `make release-check` holds there too.)
5. **Push** the branch and the tag:
   `git push origin release/X.Y && git push origin vX.Y.Z`
   (and `git push origin main` if step 2's commit landed there).
   Pushing the tag is what triggers `release.yml`, which now does more than build
   and sign: per published digest it attests a CycloneDX SBOM scanned from the
   **pushed image** (not the source tree, which sees no OS packages) and a build
   provenance statement, then a `release-assets` job uploads those SBOMs,
   `THIRD-PARTY-NOTICES.md`, `LICENSE`, `NOTICE` and a cosign-signed `SHA256SUMS`
   to the Release — creating a draft Release first if you have not cut one yet —
   and **fails if any of them did not land**. So the supply-chain assets are no
   longer yours to remember; the demo videos in step 7 still are.

   If that job is red, the Release is missing assets `docs/VERIFY.md` tells
   consumers to check. Treat it as a failed release, not a cosmetic warning.

6. **Create the GitHub Release** for the tag, pasting that version's CHANGELOG section
   as the body. **Mark it a pre-release** (`gh release create --prerelease`) — Wardyn is
   pre-alpha.
7. **Publish the demo videos as release assets** (after step 6 — upload needs the
   Release to exist). Release assets live outside git history, so clones stay small.
   Stage the shipping take per episode — the newest `PASS` row per id in
   `local/TAKES-LEDGER.md`'s attempt log, never `ls -t` (failed takes share the
   folder) — under stable, timestamp-free names, then upload:

   ```sh
   TAG=vX.Y.Z; SRC=/mnt/c/Users/Chaz/Videos; STAGE=$(mktemp -d)
   awk -F'|' '$6 ~ /PASS/ && $7 ~ /mp4/ {gsub(/ /,"",$3); gsub(/ /,"",$7); a[$3]=$7}
              END {for (id in a) print a[id]}' local/TAKES-LEDGER.md |
   while read -r f; do
     cp "$SRC/$f" "$STAGE/$(printf '%s' "$f" | sed -E 's/-[0-9]{8}T[0-9]{6}Z(-ffwd)?-narrated//')"
   done
   ls -l "$STAGE"          # eyeball: one file per episode, stable names, sane sizes
   gh release upload "$TAG" "$STAGE"/*.mp4 --clobber
   ```

   Docs link the **pinned tag** —
   `https://github.com/cjohnstoniv/wardyn/releases/download/vX.Y.Z/<name>.mp4`.
   (`releases/latest/download/` resolves only to non-prerelease releases; every
   Wardyn release is a pre-release, so `latest` 404s.) A later release that
   re-records an episode re-uploads under the same stable name and bumps the tag
   in the linking docs (one `sed`).

## Repo settings (GitHub-side)

**Branch protection on `main` is enabled.** A push is gated on the CI merge-gate
status checks and normally requires a pull request. `enforce_admins` is **off**, so
the maintainer cutting a release pushes the tag commit to `main` directly (step 5)
while contributors go through PRs — this is why `CONTRIBUTING.md`'s check list is a
real server-side merge block for contributors, and the maintainer's release push
bypasses the PR requirement. Apply (or re-apply) the protection with the command
below. A required context must be **exactly** a `.github/workflows/ci.yml` job id
**that reports on a pull request** — re-check both halves whenever the merge gate
changes:

Change the **contexts only** — `PATCH .../protection/required_status_checks` leaves
the review, admin and force-push settings alone. A full `PUT .../protection` rewrites
every field, so any key you omit is silently reset (the earlier version of this
document shipped a `PUT` body that would have flipped `strict` to false and dropped
`require_code_owner_reviews`):

```sh
gh api -X PATCH repos/cjohnstoniv/wardyn/branches/main/protection/required_status_checks \
  --input - <<'JSON'
{
  "strict": true,
  "contexts": [
    "build", "ui", "dco",
    "gates (govulncheck)", "gates (staticcheck)", "gates (gitleaks)",
    "gates (licenses)", "gates (license-headers)"
  ]
}
JSON
```

Read it back with
`gh api repos/cjohnstoniv/wardyn/branches/main/protection --jq .required_status_checks.contexts`.
A matrix job reports one context per cell as `<job-id> (<matrix-value>)`, which is why
the five supply-chain gates are `gates (...)` rather than bare names.

A job conditional on `push`, a schedule, or a path filter must **not** be a required
context: GitHub does not treat a never-reported required context as passing, so
the PR sits at "Expected — waiting for status to be reported" and cannot be
merged. `screenshots-fresh` (PR-only) is the live example. This paragraph used
to cite `sbom-stub`, which no longer exists.

## Container images

Two workflows publish images, on two different triggers — neither overlaps
the other:

- **Continuous (every push to `main`).**
  `.github/workflows/publish-image.yml` builds and pushes `wardynd` only, to
  `ghcr.io/cjohnstoniv/wardynd` (`:latest`, `:sha-<commit>`). Unsigned. The
  compose stack still always builds from source (see [docs/CI.md](docs/CI.md)).
- **Release (every `vX.Y.Z` tag).** `.github/workflows/release.yml` builds and
  pushes all FIVE images a release ships —
  `ghcr.io/cjohnstoniv/wardynd` (built with both runner substrates,
  `GO_BUILD_TAGS=docker,k8s`), `ghcr.io/cjohnstoniv/wardyn-proxy`,
  `ghcr.io/cjohnstoniv/agent-claude-code`, `ghcr.io/cjohnstoniv/agent-codex-cli`,
  `ghcr.io/cjohnstoniv/agent-aws-sso`
  — each tagged with the bare semver (e.g. `0.6.0`, matching `Chart.yaml`'s
  `appVersion`) and **cosign-signed (keyless)** by digest. Step 5's tag push
  is what triggers it. It also attests a per-digest CycloneDX SBOM and build
  provenance, publishes the Helm chart to `oci://ghcr.io/cjohnstoniv/charts`,
  and uploads the SBOMs, notices and a cosign-signed `SHA256SUMS` to the Release
  — failing if any asset did not land. The supply-chain artifacts are no longer
  yours to remember; the demo videos in step 7 still are.
  Each is a **multi-arch index** covering `linux/amd64` and `linux/arm64`
  (arm64 laptops are the desktop tier's ordinary hardware — see
  [docs/DESKTOP.md](docs/DESKTOP.md)). Images are **not** digest-pinned
  anywhere they're *consumed* (the chart's `image.tag` still floats on the
  mutable semver tag) — only the cosign signature is by digest; pinning every
  consumer to a digest is separate, unstarted work.

  **On the first multi-arch tag, verify the INDEX digest by hand.** cosign
  signs whatever digest `docker/build-push-action` reports, and with a
  two-platform `platforms:` list that is the *index* digest, not a per-arch
  manifest — so there is exactly ONE signature per image and the per-arch
  children are **not** individually signed. That is the normal multi-arch
  posture, but it is a change from the single-arch shape earlier releases had,
  so prove it once rather than assuming it:

  ```sh
  TAG=0.6.0   # the bare semver just pushed, no leading v
  for img in wardynd wardyn-proxy agent-claude-code agent-codex-cli agent-aws-sso; do
    ref="ghcr.io/cjohnstoniv/$img:$TAG"
    # 1. the tag resolves to an index listing BOTH platforms
    docker buildx imagetools inspect "$ref"
    # 2. that index digest — not a child manifest — is what carries the signature
    digest=$(docker buildx imagetools inspect "$ref" --format '{{json .Manifest.Digest}}' | tr -d '"')
    cosign verify "ghcr.io/cjohnstoniv/$img@$digest" \
      --certificate-identity-regexp '^https://github\.com/cjohnstoniv/wardyn/\.github/workflows/release\.yml@refs/tags/v' \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com
  done
  ```

  A `cosign verify` against a per-arch *child* digest is **expected to fail** —
  that is the index-signing model, not a broken release. Consumers verify the
  index ref.
