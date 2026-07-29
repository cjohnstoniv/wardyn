# Releasing Wardyn

Wardyn is **pre-alpha** and does **not** follow semantic versioning yet — interfaces
are not stable, so a minor bump may still carry breaking changes (see the CHANGELOG
header). Releases are cut **manually** by the maintainer; there is no release workflow
or `make release` target — **nothing here is automated to push anything**. This
document is that process, written down.

## Prerequisites

- You are the maintainer (see [MAINTAINERS.md](MAINTAINERS.md)); releases push tags to
  `origin`, so only someone with push rights cuts them.
- The full CI gate is green on the commit you intend to tag. The gate is the
  `.github/workflows/ci.yml` job list: `build`, `diagrams`, `ui`, `ui-e2e`,
  `helm`, `compose`, `conformance`, `envbuild-integration`, `test-pg`,
  `govulncheck`, `staticcheck`, `dco`, `gitleaks`, `licenses`,
  `license-headers` (plus `sbom-stub`, which runs **only** on push to `main` —
  see "Repo settings" below).

Run the local gate first:

```bash
WARDYN_TEST_PG=postgres://... make release-check   # runs `make ci`, plus the Postgres
                                                   # lane and the `## [Unreleased]` check
```

Three CI jobs cannot run locally at all, because they need a live daemon or
service: `conformance` (`make test-conformance-docker`), `envbuild-integration`
(`make test-envbuild-integration`), and the Playwright `ui-e2e` job. Without
`WARDYN_TEST_PG` the Postgres suite prints a loud SKIPPED line.

Screenshot freshness is CI-only for a different reason: `ci.yml`'s
`screenshots-fresh` job compares the PR diff, so it can tell "you changed a
console screen without re-shooting `docs/img`" — a local commit-timestamp test
cannot, and cannot be cleared at all once `make screenshots` re-renders the PNGs
byte-identically. Re-shoot with `make screenshots` when you touch a screen.

`release-check` pushes nothing and tags nothing. A green local run means "no local
reason not to tag", not "CI is green" — check the actual CI run on the commit
before step 3.

## Steps

1. **Update the CHANGELOG.** Rename the working `## [Unreleased]` heading (or add the
   section) to `## [X.Y.Z] — YYYY-MM-DD` in [CHANGELOG.md](CHANGELOG.md), following the
   [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format already in use
   (`### Added` / `### Changed` / `### Fixed`). Keep entries user-facing and specific.

   **Then put a fresh, empty `## [Unreleased]` heading back above the new section.**
   `make release-check` hard-fails if `CHANGELOG.md` has no `## [Unreleased]`
   (`grep -q "## \[Unreleased\]" CHANGELOG.md || exit 1`), so renaming it away and
   not restoring it leaves the gate red for the *next* release — which is a
   confusing failure to debug from the tag commit backwards. Restore it in the same
   commit as the rename.
2. **Commit** the CHANGELOG bump, DCO-signed: `git commit -s -m "release: X.Y.Z"`.
3. **Tag** the release commit: `git tag vX.Y.Z` (tags are `v`-prefixed —
   `v0.1.0` … `v0.4.2`).
4. **Push** the commit and the tag: `git push origin main && git push origin vX.Y.Z`.
5. **Create the GitHub Release** for the tag, pasting that version's CHANGELOG section
   as the body. **Mark it a pre-release** (`gh release create --prerelease`) — Wardyn is
   pre-alpha.

## Repo settings (GitHub-side)

**Branch protection on `main` is enabled.** A push is gated on the CI merge-gate
status checks and normally requires a pull request. `enforce_admins` is **off**, so
the maintainer cutting a release pushes the tag commit to `main` directly (step 4)
while contributors go through PRs — this is why `CONTRIBUTING.md`'s check list is a
real server-side merge block for contributors, and the maintainer's release push
bypasses the PR requirement. Apply (or re-apply) the protection with the command
below. A required context must be **exactly** a `.github/workflows/ci.yml` job id
**that reports on a pull request** — re-check both halves whenever the merge gate
changes:

```sh
gh api -X PUT repos/cjohnstoniv/wardyn/branches/main/protection \
  --input - <<'JSON'
{
  "required_status_checks": { "strict": false, "contexts": [
    "build", "diagrams", "ui", "ui-e2e", "helm", "compose", "conformance",
    "envbuild-integration", "test-pg", "govulncheck", "staticcheck",
    "dco", "gitleaks", "licenses", "license-headers"
  ] },
  "enforce_admins": false,
  "required_pull_request_reviews": null,
  "restrictions": null
}
JSON
```

A job conditional on `push`, a schedule, or a path filter must **not** be a required
context (this is why `sbom-stub` is absent above): GitHub does not treat a
never-reported required context as passing, so the PR sits at "Expected — waiting
for status to be reported" and cannot be merged.

## Container images

No wardynd container image is published to any registry yet (see
[docs/CI.md](docs/CI.md)). The Helm chart and compose stack build from source;
operators must build and push their own image (see `deploy/helm/wardyn/values.yaml`).
When image publishing lands, add the push + digest-pin steps here.
