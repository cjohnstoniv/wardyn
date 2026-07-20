<!-- Copyright 2025 The Wardyn Authors -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# `make setup` requires a hand-set `WARDYN_UI_STAGE` behind a pnpm-less mirror

**Date:** 2026-07-20
**Severity:** medium. This one **hard-blocks `make setup`** (not just a cosmetic
diagnostic), and the fix that unblocks it was undocumented tribal knowledge a new
operator had no way to discover from the failure.
**Status:** fixed in 0.4.1.

## Symptom

A first-time corporate user runs the blessed front door — `make setup` — and the
build dies mid-way:

```
npm error code E404
'pnpm@… is not in this registry'      (or 403 Forbidden)
```

`Dockerfile.wardynd`'s default `ui-build` stage does `npm install -g pnpm@<pinned>`
then `pnpm install`. Behind a corporate **allowlist** mirror that hasn't onboarded
pnpm (its tarballs 404/403 even though ordinary packages resolve), that stage cannot
complete. The build fails, the stack never comes up, and the one command the docs
tell a new user to run does not work out of the box.

The escape hatch existed (`WARDYN_UI_STAGE=ui-prebuilt` + a host-built `ui/dist`), but:

- it was **not the default**, so the naive `make setup` fails;
- **nothing in the failure output mentioned it** — the operator saw a raw npm 404
  with no next step;
- it also required knowing to run `pnpm -C ui build` on the host first.

## Root cause

`scripts/up.sh` (the container path) never **chose** a UI stage: it passed
`UI_STAGE: "${WARDYN_UI_STAGE:-ui-build}"` straight through and always defaulted to
building from source. Yet **host-mode `scripts/setup.sh` already builds `ui/dist` on
the host** (`cd ui && pnpm install --frozen-lockfile && pnpm build`), where the
operator's toolchain and registry work. The pieces to "just work" already existed;
they were simply not wired together on the container path.

### A second, deterministic break found while fixing this

The `ui-build` stage also ran `update-ca-certificates` to install a staged corporate
CA — but the pinned `node:*-bookworm-slim` base `apt-get purge --auto-remove`s the
`ca-certificates` package in its own build, so that binary is **absent**. Verified by
running the pinned digest: `command -v update-ca-certificates` → missing,
`dpkg -l ca-certificates` → `un`.

This means any operator who followed the doctor's *own* advice ("copy your corp root
CA to `deploy/images/corp-ca.pem`") broke `make setup` **100% of the time**, exit 127
— arguably worse than the pnpm bug, and hit before the pnpm step ever ran. The
`deploy/images/README.md` snippet that produced it warned only about Alpine.

## What closed it

**1. Fail-then-retry, not predict.** `scripts/up.sh` now wraps the compose build: if
it fails and the operator hasn't pinned a stage, it builds the UI on the host
(reusing the existing `make ui` target) and retries with `WARDYN_UI_STAGE=ui-prebuilt`,
explaining each step. `make setup` completes with zero new knowledge, and the OSS path
is byte-identical — the branch is never entered when the build succeeds.

**Rejected: a registry probe.** The original proposal was to probe the configured
registry for `pnpm` and pre-select the stage. It would have shipped a non-fix: the
404 is on the **tarball** path (`registry/pnpm/-/pnpm-*.tgz`), so a mirror that
proxies metadata returns 200 for the probe and the build still dies. Retrying needs
no prediction and covers every `ui-build` failure mode, not just the one a probe
models.

**2. The CA block is gone.** `ui-build` now relies on `NODE_EXTRA_CA_CERTS` alone —
npm/pnpm are the only TLS clients in that stage and they read it, so a system-trust
install was never needed. Net −5 lines, and it fixes every caller of that build, not
just `make setup`. `deploy/images/README.md` was corrected so it stops reproducing
the bug.

**3. Self-documenting failure.** If the host build also fails, the error names the
exact recovery command rather than leaving a bare npm error. An explicit
`WARDYN_UI_STAGE` is always respected and disables the fallback — an operator
override is never second-guessed.

**Dropped: the interactive prompt.** With automatic retry there is nothing to ask.

## Acceptance test

On a host whose npm registry cannot serve `pnpm` (allowlist mirror), with a corporate
proxy set, a plain `make setup` (container mode) completes and brings the stack up by
auto-building the UI on the host — and in **no** case dies with an unexplained npm
404/403 and no recovery guidance. `WARDYN_UI_STAGE` remains available as an explicit
override but is **not required** for `make setup` to succeed.

Verified: `docker build --target ui` with a staged CA now succeeds (it failed exit 127
before); a full `make setup` completes and the stack reports healthy.

## Cross-reference

The underlying "pnpm not onboarded" problem was field-reported earlier and 0.4 shipped
the `ui-prebuilt` stage, closing the build-blocker itself. This report is the
**remaining ergonomic gap on top of that fix**: `make setup` still required the
operator to know and set `WARDYN_UI_STAGE` by hand. Read the two together as
"give us a prebuilt path" → "reach for it automatically."
