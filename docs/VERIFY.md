# Verifying what you pulled

Every Wardyn **release** image is signed, carries an SBOM you can read, and
records how it was built. This page is how you check that yourself, without
trusting this page. Wardyn also publishes a *continuous* lane, on different
terms — see ["The continuous lane"](#the-continuous-lane) before you verify a
`:latest` or `:sha-…` tag with anything on this page.

Nothing here needs an account, a token, or a GitHub login.

## What is published

| image | contains |
|---|---|
| `ghcr.io/cjohnstoniv/wardynd` | the control plane (distroless) |
| `ghcr.io/cjohnstoniv/wardyn-proxy` | the egress proxy (distroless) |
| `ghcr.io/cjohnstoniv/agent-base` | the agent runner contract, **no coding agent** |
| `ghcr.io/cjohnstoniv/agent-codex-cli` | agent-base + OpenAI Codex CLI (Apache-2.0) |
| `ghcr.io/cjohnstoniv/agent-aws-sso` | agent-base + AWS CLI v2, for the SSO login flow |
| `ghcr.io/cjohnstoniv/charts/wardyn` | the Helm chart, as an OCI artifact |

There is deliberately **no published image containing Anthropic's Claude Code
CLI** — it is not open source, and its terms are not even readable from inside an
image that bundles it. Build that one locally with `make agent-images`; you then
install the vendor CLI under your own agreement with Anthropic. See
[`deploy/images/THIRD-PARTY-TERMS.md`](../deploy/images/THIRD-PARTY-TERMS.md).

## 0. Pick the version you are verifying

Every command below is parameterised on `$WARDYN_VERSION`. Set it once, in the
shell you are about to paste into. A stale literal in a doc is how `cosign
verify` ends up answering `MANIFEST_UNKNOWN` for a tag that was never published
— which reads like a verification failure and is not one:

```sh
# The newest release. NOT releases/latest — that endpoint excludes pre-releases,
# and every Wardyn release is one, so it 404s and leaves this empty.
WARDYN_VERSION=$(curl -fsSL "https://api.github.com/repos/cjohnstoniv/wardyn/releases?per_page=1" \
                 | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -1)
: "${WARDYN_VERSION:?could not resolve a version — set it by hand, e.g. WARDYN_VERSION=0.6.6}"
```

Or set it by hand to the release you actually pulled — `wardyn --version`
prints it, and it is the `version` field of `GET /healthz`. The image tag and
the release tag are the same number; only the git tag carries the `v`. What is
published is listed at
<https://github.com/cjohnstoniv/wardyn/pkgs/container/wardynd>, and a tag that
is not there has no signature to verify.

## 1. Verify the signature

Signing is keyless (Sigstore): there is no public key to distribute, and no
private key for anyone to steal. What you verify instead is *which workflow, in
which repository, at which tag* produced the image.

```sh
cosign verify \
  --certificate-identity-regexp '^https://github\.com/cjohnstoniv/wardyn/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "ghcr.io/cjohnstoniv/wardynd:${WARDYN_VERSION}"
```

Read the identity regexp before you copy it. It is the whole check: it says the
image was built by *this repo's release workflow, from a tag*. A signature that
verifies against some other identity is not the same claim.

## 2. Read the SBOM

The image's own component inventory, attested to its digest:

```sh
cosign verify-attestation --type cyclonedx \
  --certificate-identity-regexp '^https://github\.com/cjohnstoniv/wardyn/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "ghcr.io/cjohnstoniv/wardynd:${WARDYN_VERSION}" \
  | jq -r '.payload' | base64 -d | jq '.predicate.components[] | {name, version, licenses}'
```

A signature proves *who built* an image. It says nothing about what is inside
one. The attestation is what makes it say something — verify both, or the second
question is still open.

## 3. Verify the build provenance

How it was built, in the format GitHub's own tooling reads:

```sh
gh attestation verify "oci://ghcr.io/cjohnstoniv/wardynd:${WARDYN_VERSION}" --repo cjohnstoniv/wardyn
```

## 4. Verify the Helm chart

The chart is an OCI artifact signed by the same workflow:

```sh
cosign verify \
  --certificate-identity-regexp '^https://github\.com/cjohnstoniv/wardyn/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "ghcr.io/cjohnstoniv/charts/wardyn:${WARDYN_VERSION}"
```

Then install it directly — `oci://` is native Helm, no `helm repo add`:

```sh
helm install wardyn oci://ghcr.io/cjohnstoniv/charts/wardyn --version "${WARDYN_VERSION}" \
  --namespace wardyn --create-namespace \
  --set auth.adminToken.secretRef.name=wardyn-auth
```

## 5. Verify the release assets

Each release carries the per-image SBOMs, `THIRD-PARTY-NOTICES.md`, `LICENSE`,
`NOTICE`, `install.sh`, the four `wardyn-<os>-<arch>` CLI binaries, and a signed
`SHA256SUMS` — `release.yml`'s `release-assets` job fails the release unless
every one of them landed:

```sh
gh release download "v${WARDYN_VERSION}" --repo cjohnstoniv/wardyn
sha256sum -c SHA256SUMS
cosign verify-blob \
  --certificate SHA256SUMS.pem --signature SHA256SUMS.sig \
  --certificate-identity-regexp '^https://github\.com/cjohnstoniv/wardyn/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
```

## 6. What the one-line installer checks — and what it leaves to you

`install.sh` is not a shorter way to run the steps above. It makes exactly one
integrity check — the first bullet — and even that one is **same-origin**:

- **The CLI binary** it puts on your PATH is hashed (`sha256_hex`) against a
  `SHA256SUMS` fetched from the *same* `releases/download/<tag>/` base as the
  binary itself, and `install_cli` dies on a mismatch rather than installing it
  unverified. That catches a truncated download or the wrong asset. It cannot
  catch a tampered release, which would serve a matching `SHA256SUMS` — and the
  installer never fetches `SHA256SUMS.sig` or `SHA256SUMS.pem`. The `cosign
  verify-blob` in step 5 is **yours** to run; nothing in the installer runs
  cosign.
- **The images it does pull** — `wardynd`, and the third-party `postgres` and
  `registry`, which are the only three services in the compose file's default
  profile — arrive by *tag* (`docker compose pull`); see the next bullet for the
  four that do not arrive at all. They are cosign-verifi**able** — that is step
  1 — and the installer verifies none of them. On a fresh install the first
  foreign code to execute on your machine is in fact the `wardynd` image's
  `-gen-age-key` entrypoint, which the installer runs to mint your secret-store
  key *before* `docker compose up`.
- **Four of the images do not arrive at install time at all.** `docker compose
  pull` resolves only the three default-profile services (`wardynd`,
  `postgres`, `registry`); the proxy sidecar (`WARDYN_PROXY_IMAGE`) and the
  three agent images the installer registers in `WARDYN_AGENT_IMAGES`
  (`agent-base`, `agent-codex-cli`, `agent-aws-sso`) are pulled by **wardynd
  itself, at your first run**, long after the install transcript scrolled past.
  They carry the same release tag and verify exactly the same way, so run step 1
  against each of them too — the installer's "Pulling signed images" line covers
  neither the pull nor the verification of these four.
- **From a clone, `make setup` also runs published code on the host itself.**
  Its pull-first path fetches the same five release images and then copies the
  host-native `wardyn` CLI out of the `wardynd` image to `bin/wardyn` and
  **executes it outside any container** to detect your host's proxy settings
  (`seed_host_proxy`, `scripts/up.sh`) — no confinement applies to that process.
  Since 0.7 that path runs `cosign verify` + `cosign verify-attestation` itself
  when `cosign` is on your PATH, refuses an image that fails, and says plainly
  that nothing was checked when it is not; `WARDYN_BUILD_LOCAL=1` skips the pull
  entirely and builds from your own tree.
- **The compose file** — `deploy/compose/docker-compose.yaml`, which decides
  which images run, which ports publish on which interface, whether
  `WARDYN_LOCAL_MODE` is on, and what is bind-mounted — is fetched from the
  release tag over TLS with **no digest check at all**, because it is not among
  the signed release assets. It is short by design and stays at
  `~/.wardyn/docker-compose.yaml` for you to read.

None of that is hidden: it is published as an accepted risk
(`threatmodel/THREAT-MODEL.md` §5, residual 32) rather than quietly verified.
`curl … | sh` is a decision to trust this project's release origin for one
command. Steps 1-5 are how you check afterwards that it deserved it, and every
artifact the installer fetched is still on disk to check *against*.

## 7. If your scanner flags GO-2026-5932

It will, and it is a false positive that we have written down rather than
suppressed. `golang.org/x/crypto/openpgp` is unmaintained with no fix available,
and the `golang.org/x/crypto` *module* is in our build — but the `openpgp`
*subpackage* is not imported by Wardyn or by anything Wardyn calls.
`govulncheck`'s call-graph analysis reports zero affected symbols on every push.

Rather than ask you to take that on trust, it ships as a machine-readable
[OpenVEX statement](../security/vex/wardyn.openvex.json) (`not_affected` /
`vulnerable_code_not_present`) that most scanners can consume directly. The full
reasoning is in [`threatmodel/THREAT-MODEL.md`](../threatmodel/THREAT-MODEL.md).

## The continuous lane

Everything above is about **release** images — the five `vX.Y.Z`-tagged images
`release.yml` publishes. A second lane publishes on every push to `main`:
`.github/workflows/publish-image.yml` builds `wardynd` alone and pushes
`ghcr.io/cjohnstoniv/wardynd:latest` and `:sha-<short-sha>`.

Those tags are **signed but not attested**, and their signature is under a
different identity:

- **Signed.** The workflow runs `cosign sign --yes` on the pushed digest, keyless.
- **No SBOM, no provenance.** It runs no `cosign attest` and no
  `attest-build-provenance` step, so §2 and §3 above have nothing to fetch for
  these tags — `cosign verify-attestation` finds no attestation, which is the
  expected answer, not a tampering signal.
- **A different certificate identity.** The regexp every command on this page
  uses is pinned to `release.yml@refs/tags/v.*` and structurally cannot match a
  `main`-push signature. Verify a continuous tag with its own:

  ```sh
  cosign verify ghcr.io/cjohnstoniv/wardynd:latest \
    --certificate-identity-regexp '^https://github\.com/cjohnstoniv/wardyn/\.github/workflows/publish-image\.yml@refs/heads/main$' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```

`deploy/desktop/install.sh` defaults to `:latest`, so a desktop install runs
this lane unless `WARDYN_INSTALL_IMAGE` pins the release digest (its own error
path prints that command). If you need an SBOM and provenance for what you run,
run a release tag.

## If verification fails

Do not run the image. Open a security advisory —
[`SECURITY.md`](../SECURITY.md) has the process. A verification failure is either
a real supply-chain problem or a bug in what this page tells you to run, and both
are worth hearing about.
