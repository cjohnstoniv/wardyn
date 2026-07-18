# Releasing Wardyn

Wardyn is **pre-alpha** and does **not** follow semantic versioning yet — interfaces
are not stable, so a minor bump may still carry breaking changes (see the CHANGELOG
header). Releases are cut **manually** by the maintainer; there is no release workflow
or `make release` target — **nothing here is automated to push anything**. This
document is that process, written down.

## Prerequisites

- You are the maintainer (see [MAINTAINERS.md](MAINTAINERS.md)); releases push tags to
  `origin`, so only someone with push rights cuts them.
- The full CI gate is green on the commit you intend to tag: `build` (both tags),
  `vet`, `test` (incl. `test-pg` and `test-docker` where wired), `staticcheck`,
  `govulncheck` (tagless **and** `-tags docker`), `gitleaks`, `licenses`, `dco`.

Run that gate list locally first, in one command:

```bash
make release-check                              # or, to include the Postgres lane:
WARDYN_TEST_PG=postgres://... make release-check
```

`release-check` runs the same commands as the CI jobs, at the same pinned tool
versions, and pushes/tags nothing. It covers `build` (both tags), `vet` (both),
the Go suites + the union coverage floor, `-race`, `staticcheck`, `govulncheck`
(both), SPDX headers, `go-licenses` (both), and `gitleaks`; `test-pg` runs only
when `WARDYN_TEST_PG` is set and prints a loud SKIPPED line when it isn't.

**It is not a CI replica** — `test-conformance-docker` (needs a live daemon), the
UI jobs (`typecheck`/`vitest`/`build`/Playwright), and `dco` are CI-only. A green
`release-check` means "no local reason not to tag", not "CI is green". Check the
actual CI run on the commit before step 3.

## Steps

1. **Update the CHANGELOG.** Rename the working `## [Unreleased]` heading (or add the
   section) to `## [X.Y.Z] — YYYY-MM-DD` in [CHANGELOG.md](CHANGELOG.md), following the
   [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format already in use
   (`### Added` / `### Changed` / `### Fixed`). Keep entries user-facing and specific.
2. **Commit** the CHANGELOG bump, DCO-signed: `git commit -s -m "release: X.Y.Z"`.
3. **Tag** the release commit: `git tag vX.Y.Z` (tags are `v`-prefixed —
   `v0.1.0`, `v0.2.0`, `v0.3.0`).
4. **Push** the commit and the tag: `git push origin main && git push origin vX.Y.Z`.
5. **Create the GitHub Release** for the tag, pasting that version's CHANGELOG section
   as the body. **Mark it a pre-release** — Wardyn is pre-alpha, and the existing
   releases predate this policy (they should be re-flagged; see the release settings).

## One-time repo settings (operator actions, pending)

Two GitHub-side settings are documented here because automation cannot apply
them (admin-level, outward-facing); until run, `CONTRIBUTING.md`'s check list is
a review bar, not a server-side merge block:

```sh
# Re-flag the pre-alpha releases as prereleases
for t in v0.3.0 v0.2.0 v0.1.0; do gh release edit "$t" --prerelease; done

# Enable branch protection on main, requiring the CI merge-gate jobs
gh api -X PUT repos/cjohnstoniv/wardyn/branches/main/protection \
  --input - <<'JSON'
{
  "required_status_checks": { "strict": false, "contexts": [
    "build", "diagrams", "ui", "helm", "compose", "conformance",
    "envbuild-integration", "test-pg", "govulncheck", "staticcheck",
    "dco", "gitleaks", "licenses", "license-headers", "sbom-stub"
  ] },
  "enforce_admins": false,
  "required_pull_request_reviews": null,
  "restrictions": null
}
JSON
```

(The contexts list mirrors `.github/workflows/ci.yml`'s job ids at the time of
writing; re-check before running if the merge gate has changed.)

## Container images

No wardynd container image is published to any registry yet (see
[docs/CI.md](docs/CI.md)). The Helm chart and compose stack build from source;
operators must build and push their own image (see `deploy/helm/wardyn/values.yaml`).
When image publishing lands, add the push + digest-pin steps here.
