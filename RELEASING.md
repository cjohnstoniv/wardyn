# Releasing Wardyn

Wardyn is **pre-alpha** and does **not** follow semantic versioning yet — interfaces
are not stable, so a minor bump may still carry breaking changes (see the CHANGELOG
header). Releases are cut **manually** by the maintainer; there is no release workflow
or `make release` target. This document is that process, written down (U124).

## Prerequisites

- You are the maintainer (see [MAINTAINERS.md](MAINTAINERS.md)); releases push tags to
  `origin`, so only someone with push rights cuts them.
- The full CI gate is green on the commit you intend to tag: `build` (both tags),
  `vet`, `test` (incl. `test-pg` and `test-docker` where wired), `staticcheck`,
  `govulncheck` (tagless **and** `-tags docker`), `gitleaks`, `licenses`, `dco`. Run
  `make test govulncheck staticcheck` locally first.

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

## Container images

No wardynd container image is published to any registry yet (see
[docs/CI.md](docs/CI.md)). The Helm chart and compose stack build from source;
operators must build and push their own image (see `deploy/helm/wardyn/values.yaml`).
When image publishing lands, add the push + digest-pin steps here.
