> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Checking image signatures in your cluster

- Wardyn's release images are signed with cosign. [Verify](../VERIFY.md) shows how to check one by hand.
- To make a cluster refuse an unsigned or foreign image on its own, run an admission controller there. Sigstore policy-controller and Kyverno both can.
- **This recipe is not run in CI.** Nothing in Wardyn's pipeline installs either controller or applies these policies. Treat it as a starting point and test it on a cluster you can break.

## What Wardyn does and does not do

- Wardyn's control plane never reads a registry. It links no registry client, and a test fails the build if one appears ([`cmd/wardynd/registry_read_guard_test.go`](../../cmd/wardynd/registry_read_guard_test.go)).
- On Kubernetes, an image is pulled by the node's kubelet. No registry request and no pull credential passes through `wardynd` ([the guard's comment](../../cmd/wardynd/registry_read_guard_test.go)).
- So `wardynd` cannot check a signature for you. The check belongs at the cluster's admission door, where the pod is created.

## The identity to trust

| Field | Value |
|---|---|
| Signing | Keyless Sigstore. There is no public key to distribute |
| Certificate identity | `https://github.com/cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/v<version>`, an exact match |
| Certificate issuer | `https://token.actions.githubusercontent.com` |
| Images | `wardynd`, `wardynd-fips`, `wardyn-proxy`, `wardyn-runnerd`, `agent-base`, `agent-codex-cli`, `agent-aws-sso`, `agent-vscode`, `agent-novnc` and `agent-novnc-signin` ([CI](../CI.md)) |

- Pin the exact tag in the identity. A pattern would also accept a signature made for another release.
- Check one image by hand first with the `cosign verify` command in [Verify](../VERIFY.md). Write the policy only once that passes.

## With Sigstore policy-controller

1. Install policy-controller from its own documentation.
2. Label the namespaces to check, for example the namespace of the control plane and the runs namespace.
3. Create a `ClusterImagePolicy` that matches your registry's Wardyn images, with a keyless authority.
4. Apply a pod for a signed image and one for an unsigned image. Only the first is admitted.

```yaml
apiVersion: policy.sigstore.dev/v1beta1
kind: ClusterImagePolicy
metadata:
  name: wardyn-release-images
spec:
  images:
    - glob: "ghcr.io/cjohnstoniv/**"
  authorities:
    - keyless:
        url: https://fulcio.sigstore.dev
        identities:
          - issuer: https://token.actions.githubusercontent.com
            subject: https://github.com/cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/v0.10.0
```

- `glob` names the registry path of the images. Change it to your mirror if you use one, and set the tag in `subject` to the release you run.
- A mirror keeps the signature only if it copies the signature along with the image. Copy it with cosign, not a plain image copy.

## With Kyverno

1. Install Kyverno from its own documentation.
2. Create a `ClusterPolicy` with a `verifyImages` rule, in `Audit` mode first.
3. Read the policy reports, then switch the rule to `Enforce`.

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: wardyn-release-images
spec:
  validationFailureAction: Audit
  webhookTimeoutSeconds: 30
  rules:
    - name: verify-wardyn-images
      match:
        any:
          - resources:
              kinds: [Pod]
      verifyImages:
        - imageReferences:
            - "ghcr.io/cjohnstoniv/*"
          attestors:
            - entries:
                - keyless:
                    issuer: https://token.actions.githubusercontent.com
                    subject: https://github.com/cjohnstoniv/wardyn/.github/workflows/release.yml@refs/tags/v0.10.0
                    rekor:
                      url: https://rekor.sigstore.dev
```

## What this does not cover

- It checks the images you name. It does not check an image a person brings with `--image` or builds from a repo.
- It does not check the run's contents or its egress. Those stay with the run's policy.
- It adds a dependency on Sigstore's public services, unless you run your own.
