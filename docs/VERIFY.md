# Verifying what you pulled

Every Wardyn image is signed, carries an SBOM you can read, and records how it was
built. This page is how you check that yourself, without trusting this page.

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

## 1. Verify the signature

Signing is keyless (Sigstore): there is no public key to distribute, and no
private key for anyone to steal. What you verify instead is *which workflow, in
which repository, at which tag* produced the image.

```sh
cosign verify \
  --certificate-identity-regexp '^https://github\.com/cjohnstoniv/wardyn/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/cjohnstoniv/wardynd:0.6.2
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
  ghcr.io/cjohnstoniv/wardynd:0.6.2 \
  | jq -r '.payload' | base64 -d | jq '.predicate.components[] | {name, version, licenses}'
```

A signature proves *who built* an image. It says nothing about what is inside
one. The attestation is what makes it say something — verify both, or the second
question is still open.

## 3. Verify the build provenance

How it was built, in the format GitHub's own tooling reads:

```sh
gh attestation verify oci://ghcr.io/cjohnstoniv/wardynd:0.6.2 --repo cjohnstoniv/wardyn
```

## 4. Verify the Helm chart

The chart is an OCI artifact signed by the same workflow:

```sh
cosign verify \
  --certificate-identity-regexp '^https://github\.com/cjohnstoniv/wardyn/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/cjohnstoniv/charts/wardyn:0.6.2
```

Then install it directly — `oci://` is native Helm, no `helm repo add`:

```sh
helm install wardyn oci://ghcr.io/cjohnstoniv/charts/wardyn --version 0.6.2 \
  --namespace wardyn --create-namespace \
  --set auth.adminToken.secretRef.name=wardyn-auth
```

## 5. Verify the release assets

Each release carries the per-image SBOMs, `THIRD-PARTY-NOTICES.md`, `LICENSE`,
`NOTICE`, and a signed `SHA256SUMS`:

```sh
gh release download v0.6.2 --repo cjohnstoniv/wardyn
sha256sum -c SHA256SUMS
cosign verify-blob \
  --certificate SHA256SUMS.pem --signature SHA256SUMS.sig \
  --certificate-identity-regexp '^https://github\.com/cjohnstoniv/wardyn/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
```

## 6. If your scanner flags GO-2026-5932

It will, and it is a false positive that we have written down rather than
suppressed. `golang.org/x/crypto/openpgp` is unmaintained with no fix available,
and the `golang.org/x/crypto` *module* is in our build — but the `openpgp`
*subpackage* is not imported by Wardyn or by anything Wardyn calls.
`govulncheck`'s call-graph analysis reports zero affected symbols on every push.

Rather than ask you to take that on trust, it ships as a machine-readable
[OpenVEX statement](../security/vex/wardyn.openvex.json) (`not_affected` /
`vulnerable_code_not_present`) that most scanners can consume directly. The full
reasoning is in [`threatmodel/THREAT-MODEL.md`](../threatmodel/THREAT-MODEL.md).

## If verification fails

Do not run the image. Open a security advisory —
[`SECURITY.md`](../SECURITY.md) has the process. A verification failure is either
a real supply-chain problem or a bug in what this page tells you to run, and both
are worth hearing about.
