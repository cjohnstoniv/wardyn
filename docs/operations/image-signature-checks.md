> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Image signature checks at run create

- Wardyn can check that a member's image is signed by a signer your organisation trusts, before the run is created.
- It is site setting `images.admission`. It is off by default, so nothing changes until a security operator turns it on.
- To make the cluster itself refuse unsigned images, see [Checking image signatures in your cluster](image-admission.md).

## What it proves, and what it does not

- It proves who signed the image digest, and which statements that signer made about it.
- It does not prove the image is safe, patched or free of malware.
- It does not check images that ungoverned operators (break-glass) pick. With [`WARDYN_GOVERN_ADMIN_RUNS`](../ENV.md) on, an admin who is not exempt is governed, and the image requirements that apply to them can refuse their run. While the mode is `audit` or `enforce`, an unchecked operator run's `run.create` row carries `image_admission_exempt: true`.
- It does not check the proxy image. Runners take that from their own pin ([Verify](../VERIFY.md) covers it).
- It does not check macOS VM runs, which never run the image. Their verdict is `not_applicable`.
- With `scope: "all"` it also checks the image a run gets by convention (its agent's, its harness set's or an architecture variant of either). That check is made at launch, not at create. Under `enforce` a refusal fails the run with the reason as its hint. Under `audit` the outcome is recorded on the run only.

## Modes

| `mode` | Image check | A failing image | Registry down |
|---|---|---|---|
| `off` (default) | None. No registry is contacted and nothing is written to the run | Runs | Not read |
| `audit` | Made, never pins | Runs. For a member's image, the create response carries a warning that starts "Image check (audit only):" and the `run.create` row records `would_refuse`. For the convention image, only the run's `image_admission` records it | A warning |
| `enforce` | Made, and the run uses the checked digest | `403` with a reason below | `503` `image_admission_unavailable` |

- `scope` is `member` (default: the image a member names) or `all` (also the convention image, checked when the run launches). With `scope: "member"` and a mode of `audit` or `enforce`, Setup status shows an "Image checks" row to say the images runs get from their agent or harness set are not checked.
- Runs Wardyn launches itself are not checked, even with `scope: "all"`: workspace record sessions, source scans, harness sign-in and the site-config connectivity probes. They run the workspace's built image, or else the agent image, and revive under any mode.
- A governance profile that lists `required_image_signers` is held to those signers even while the site mode is `off` or `audit`. A profile can only tighten.
- A run that was queued is checked again when it starts. `POST /runs/preflight` makes the same check of a member's image, and any audit row it writes carries `dry_run: true`. It cannot check the convention image, which is chosen at launch, once a runner is placed. The policy preview only reports "checked at launch" and reads no registry.
- `enforce` with no signers and scope `member` is refused when you save it, because it would refuse every image. Use `audit` first.

## Turn it on: audit first

1. Add a signer that covers one image prefix you use. See [Signers](#signers).
2. Set `mode` to `audit` with `PUT /api/v1/site-config`. A body that omits `images` keeps the stored block.
3. Create runs as members and read the warnings and the `image_admission` key on their `run.create` rows. With `scope: "all"`, also read `image_admission` on each run (`GET /api/v1/runs/{id}`): the launch's check of the convention image is recorded only there, with `ref` naming the image, and `verdict` `would_refuse` with a `reason` when it would be refused.
4. Fix the images and signers that would be refused.
5. Set `mode` to `enforce`.

- While `enforce` is on, a revive re-checks the digest the run's create pinned, never its tag. A run with no pinned digest is refused with `revive_image_not_admitted`: one created before `enforce` was on, or under `audit`, which never pins. Start a new run to use an allowed image.
- A kept sandbox on an image Wardyn built for the run (a devcontainer or a generated workspace image), or from a run Wardyn launched itself (above), is not checked and revives under any mode.
- Under `enforce` with scope all, a run that named no image and recorded no image origin is refused like one on the convention image. That is a run from an earlier release, or one whose origin record failed to write under `off` or `audit`.
- The same holds for a member's image while the mode is `off`, when the owner's profile names `required_image_signers`.
- With `scope: "all"`, a kept run on the convention image is re-checked at the digest its launch recorded. One whose launch recorded no admitted digest is refused the same way.
- Under `enforce`, or for a profile's required signers, removing a signer binds kept sandboxes too: a revive is checked under the signers as they are now.
- A bad `images.admission` block is refused with `400` `site_config_invalid` and a message that starts "images.admission:".
- The write compiles the block, so the API and the checker agree on an empty image list, the reserved names and absolute predicate URIs.

```json
{
  "images": {
    "admission": {
      "mode": "audit",
      "scope": "member",
      "signers": [
        {
          "name": "corp-ci",
          "images": ["registry.corp.example/team/*"],
          "keyless": {
            "issuer": "https://token.actions.githubusercontent.com",
            "identity": "https://github.com/corp/images/.github/workflows/build.yml@refs/heads/main"
          },
          "require_attestations": ["https://slsa.dev/provenance/v1"]
        }
      ]
    }
  }
}
```

## Signers

| Field | Meaning |
|---|---|
| `name` | Referenced by profiles. Unique. `wardyn-release` and `public-good` are reserved |
| `images` | Prefixes the signer covers, written like [image grants](capabilities.md). Entries are compared as whole strings, tag or digest included, so `ghcr.io/x/y` does not cover `ghcr.io/x/y:1.2` |
| `keyless` | A Sigstore identity: `issuer` (https), then `identity` or `identity_regexp`, optional `source_repository`, and `trusted_root` (default `public-good`) |
| `public_key` | A PEM public key instead: ECDSA P-256 or P-384, Ed25519, or RSA of 2048 bits or more |
| `transparency_log` | Key signers only. Default `true`: the signature must be in the public Rekor log. `false` checks offline, for air-gapped signing |
| `require_attestations` | Predicate types the same signer must also have attested to the digest |
| `signature_predicates` | Extra statement types that count as this signer's signature. Absolute URIs |

- A signer has `keyless` or `public_key`, never both.
- Prefer an exact `identity`. An `identity_regexp` must start with `^` and end with `$`, and is matched against the whole identity, so `^https://github.com/corp/.*$` admits every workflow in that organisation.
- An image passes when one signer that covers it passes. A signer that covers the image and fails does not block another that passes.
- An image no signer covers is refused `image_admission_no_policy` without any registry request. Wardyn contacts only registries your signers name.

### What counts as a signature

- A cosign signature, or a signed statement whose predicate type is `https://sigstore.dev/cosign/sign/v1`, SLSA provenance v0.2 (`https://slsa.dev/provenance/v0.2`) or v1 (`https://slsa.dev/provenance/v1`).
- Any other statement is an attestation. It never counts as the signature, so a scan or SBOM attestation by the same identity does not admit an image. Add its type to `signature_predicates` only if you mean it.
- The statement's subject must be the image digest. A statement about another digest is ignored.
- Legacy cosign signatures (`sha256-<hex>.sig`) and attestations (`.att`) are read as well as the referrers API.
- At most 16 signatures are tried per signer, bundles first. Once bundles fill those 16, legacy signatures are not tried, though they are still fetched, and a failed fetch still makes the check unavailable.
- An attestation or signature over 1 MiB is not read. Wardyn's own CycloneDX SBOM is 2.3 to 3.2 MB, so `require_attestations: ["https://cyclonedx.org/bom"]` cannot be met from it.
- A legacy signature's certificate chain annotation is never trusted. Its signed timestamp counts only when the trust root lists a timestamp authority; a root with a Rekor log needs the signature's Rekor entry.

### The built-in signer `wardyn-release`

- It is always defined and any profile or site entry may name it.
- It covers `ghcr.io/cjohnstoniv/*` on release builds, for the exact release identity of this Wardyn version. Another release's signature is `image_signature_invalid`.
- It covers nothing on a development build (a version with `+commit`), so images there need a signer you add.
- It passes on the release's build provenance (the referrer) or on the legacy cosign signature.
- Operator-overridden agent images (`WARDYN_AGENT_IMAGES`) need a signer you add.

## Profiles

- `required_image_signers` on a governance profile or restriction lists signer names. Each must also pass, for every member image.
- Each name must be a site signer or `wardyn-release`, or the write is refused `400`.
- A name whose signer is later removed makes its members fail closed with `image_admission_no_policy`.
- A profile cannot name roots, keys or identities, so it cannot add trust. Removing a required name is a loosening, so it needs the second approver where that is on.

## Trust roots

| Root | Use |
|---|---|
| `public-good` (default) | The Sigstore public-good root embedded in this build. Keyless checks need a signed certificate timestamp and a Rekor entry |
| A file you supply | `trusted_roots: [{"name": "github-private", "path": "/etc/wardyn/sigstore/trusted-root.json"}]`, an absolute path read and parsed when you save |

- For a GitHub private instance, write the file with `gh attestation trusted-root > trusted-root.json`. That root has no Rekor log; a timestamp authority serves instead.
- An older Wardyn keeps verifying older signatures. After a Sigstore key rotation, run a newer Wardyn or supply a root file.
- Wardyn fetches no trust material at run time.

## Registries

- Registries are https-only. A plain-http registry, realm or redirect is refused.
- Wardyn reads the registries your signers name, with the transport that honours `WARDYN_TRUSTED_CA_FILE` and the daemon proxy.
- Credentials come from `WARDYN_REGISTRY_AUTH_FILE`, a Docker `config.json`-shaped file. Unset means anonymous. See [ENV](../ENV.md).
- On Kubernetes, `imageAdmission.registryAuthSecret` mounts a `dockerconfigjson` Secret for it.
- A credential is sent only to its own registry's token realm, over https. A redirect to another host carries none.
- A registry, realm or redirect whose literal address, or local resolution, is loopback, link-local or the metadata address is refused.
- Behind the operator's forward proxy the proxy resolves names it is handed. What they resolve to is the operator's boundary.
- A TLS-inspecting proxy whose CA is in `WARDYN_TRUSTED_CA_FILE` can read and deny, but cannot forge an admission: trust is in the signature, not the channel.

## Air-gapped estates

1. Copy the image and its signatures into your registry with `cosign copy` or `oras cp -r`. A plain image copy drops them.
2. Add a signer that covers your registry's names, with the original signer's identity.
3. For a key signer, set `transparency_log` to `false` if the signature was never logged.
4. Supply the private Sigstore's root by path if you run your own.

## One image, many architectures

- A tag that resolves to an index is checked at the index digest. Wardyn signs index digests.
- An image pinned to one platform's digest needs a signature on that digest. Wardyn does not walk up to the index.

## Caching and limits

| Result | Kept | Where |
|---|---|---|
| Admitted | 1 hour | Per wardynd replica |
| Refused: no signature, wrong signer | 60 seconds | Per replica |
| `image_admission_unavailable` | Never | Retried on the next create |

- The key covers the digest, the matched signers and every trust root's contents, so any policy or root change checks again.
- The tag is resolved on every create, never cached. The verdict is on the run row.
- At most 64 referrers are listed and 16 signature bundles verified per signer. A manifest over 4 MiB or a blob over 1 MiB is refused unread.
- A whole check takes at most 20 seconds, with at most 3 redirects.

## Reasons

The full table is in the [SDK reference](../sdk.md).

- A refusal at create writes a `run.create` `denied` row with the reason and the `image_admission` facts.
- A refusal at revive writes a `run.revive` `denied` row with the same facts.
- A refusal of the convention image at launch fails the run and writes a `run.dispatch` `failure` row.
- Under `audit`, the convention image's outcome is on the run (`image_admission`), not in an audit row.

| Reason | Status | Meaning |
|---|---|---|
| `image_admission_no_policy` | `403` | No signer covers the image, or a signer the profile requires does not or is gone |
| `image_signature_missing` | `403` | No signature from a covering signer |
| `image_signature_invalid` | `403` | Signed, but by another identity or key |
| `image_attestation_missing` | `403` | A required attestation is absent |
| `image_admission_unavailable` | `503` | The registry or a trust root could not be read; the sentence names the cause |
| `revive_image_not_admitted` | `403` | A revive while the image is held, of a run with no checked digest pinned. A run recorded as on an image Wardyn built for it is not refused; under scope all, one with no recorded image origin is |

## Audit

- `run.create` carries `image_admission` (mode, verdict, digest, signer, identity or key id, attestations). The key is absent while the mode is `off` and no profile names `required_image_signers`.
- `site_config.write` records the mode, scope, signer names and their before and after, as in [Audit actions](../AUDIT-ACTIONS.md).
