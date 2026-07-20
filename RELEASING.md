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

Run both local gates first. Neither one contains the other:

```bash
make ci                                          # the WIDER gate — run this
WARDYN_TEST_PG=postgres://... make release-check  # + the Postgres lane and the CHANGELOG check
```

- **Both** run: `build`, `build-docker`, `lint` (vet both tag sets + the
  size/complexity gates), `cover-check`, `test-race`, `staticcheck`,
  `govulncheck` (both), `license-headers`, `licenses`, `gitleaks`.
- **Only `make ci`** (Makefile `ci:`): `diagrams`, `helm-lint`,
  `compose-config`, `dco`, `npm-license`, `ui-typecheck`, `ui-test`, `ui`,
  `test-conformance-stub` — i.e. the CI jobs `diagrams`, `helm`, `compose`,
  `dco` and the non-Playwright half of `ui` are covered **only** by `make ci`,
  not by `release-check`.
- **Only `make release-check`**: the `## [Unreleased]` CHANGELOG check, and the
  Postgres suite (`test-report-pg`) when `WARDYN_TEST_PG` is set — it prints a
  loud SKIPPED line when it isn't.
- **Neither**, because they need a live daemon or service: `conformance`
  (`make test-conformance-docker`), `envbuild-integration`
  (`make test-envbuild-integration`), and the Playwright `ui-e2e` job.

Both push nothing and tag nothing. A green local run means "no local reason not
to tag", not "CI is green" — check the actual CI run on the commit before step 3.

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
   `v0.1.0` … `v0.3.1`).
4. **Push** the commit and the tag: `git push origin main && git push origin vX.Y.Z`.
5. **Create the GitHub Release** for the tag, pasting that version's CHANGELOG section
   as the body. **Mark it a pre-release** — Wardyn is pre-alpha, and the existing
   releases predate this policy (they should be re-flagged; see the release settings).

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

Two corrections this list carries, both worth understanding before editing it:

- **`ui-e2e` was missing and is now required.** The Playwright lane (`ui-e2e`)
  runs on every PR, but was absent from the applied contexts — so a red
  Playwright run did not block a merge server-side; only a human noticing the
  red check did.
- **`sbom-stub` was required and is now removed — do not add it back.** That job
  is gated `if: github.event_name == 'push' && github.ref == 'refs/heads/main'`,
  so it never reports a status on a pull request. GitHub does not treat a
  never-reported required context as passing: the PR sits at "Expected — waiting
  for status to be reported" forever and cannot be merged. **Operator action:**
  re-run the `gh api` command above (it replaces the whole contexts list) on any
  repo still carrying the old list. Only make `sbom-stub` required again if the
  job loses its push-only `if:` and starts running on PRs.

The same rule applies to any future job: if it is conditional on `push`, a
schedule, or a path filter, it must **not** be a required context.

**Still pending (operator action):** the pre-alpha releases *before* v0.3.1 are not
yet flagged as prereleases (v0.3.1 was created with `--prerelease`). Re-flag the rest:

```sh
for t in v0.3.0 v0.2.0 v0.1.0; do gh release edit "$t" --prerelease; done
```

## Container images

No wardynd container image is published to any registry yet (see
[docs/CI.md](docs/CI.md)). The Helm chart and compose stack build from source;
operators must build and push their own image (see `deploy/helm/wardyn/values.yaml`).
When image publishing lands, add the push + digest-pin steps here.
