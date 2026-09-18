# Kubernetes lifecycle acceptance — 2026-09-18

Patch: `46dbcf6dbcdc905d944a8baa7f77e7ea5bc252e2`, branch
`review/0.7x-life-terminal-pod`, base `dfa89f608fa223b469b0a2435226538f0035d533`.

## Environment and isolation

- New cluster `wardyn-review-07x-life`, created only for this review; existing clusters untouched.
- kind v0.23.0; cached `kindest/node:v1.31.4`; Calico v3.28.0 using the repository's pinned manifest.
- Namespace `wardyn-review-07x`, PSS `restricted`, enforce version v1.30.
- Dedicated kubeconfig was kept in the originating worktree at `/tmp/wardyn-review-0.7x-life-terminal-pod/local/lifecycle-live/`; no modification of the default kubeconfig. No kubeconfig, key, image, or binary is copied into this ledger evidence directory.
- After acceptance, the temporary review cluster was deleted successfully. Existing clusters `dazz-next`, `dazz-prod`, and `wardyn-entra` remained. The two uniquely tagged local test images and the local build/evidence artifacts were retained for reproducibility.
- `wardyn-review/wardyn-proxy:07x-life-46dbcf6d`: current source built with CGO disabled, copied over the cached runtime image `wardyn/wardyn-proxy:local` (base digest `sha256:aef3103e09ba3bf97b0b5b9a8b85a05a027bf2dc7a214d62b0a1193e829a84d4`). This is a test image, not production Dockerfile/build verification.
- `wardyn-review/conformance-agent:07x-life-46dbcf6d`: repository `deploy/kind/Dockerfile.conformance-agent`, current source `wardyn-rec` binary.
- Compiled conformance executable: `CGO_ENABLED=0 GOOS=linux go test -c -buildvcs=false -tags k8s -o local/lifecycle-live/conformance.test ./test/conformance`.

## Executed acceptance

The host could not reach the node's internal address `172.21.0.4:6443`; the first attempt failed at canary pod creation before any conformance case. Host access through kind's published endpoint worked. The test executable therefore ran inside the review-owned node using `/etc/kubernetes/admin.conf`, which is reachable both from the test client and from the canary pods. Calico's boot canary passed there.

```sh
docker exec \
  -e KUBECONFIG=/etc/kubernetes/admin.conf \
  -e WARDYN_TEST_K8S=1 \
  -e WARDYN_K8S_NAMESPACE=wardyn-review-07x \
  -e WARDYN_PROXY_IMAGE=wardyn-review/wardyn-proxy:07x-life-46dbcf6d \
  -e WARDYN_TEST_K8S_AGENT_IMAGE=wardyn-review/conformance-agent:07x-life-46dbcf6d \
  wardyn-review-07x-life-control-plane \
  /usr/local/bin/wardyn-review-conformance.test \
  -test.v -test.timeout=20m \
  '-test.run=^TestConformanceK8s$/(WaitExitCode|EphemeralDiskLimit)$' -test.count=1
```

Exit 0; no selected case skipped:

```text
--- PASS: TestConformanceK8s (164.39s)
    --- PASS: TestConformanceK8s/WaitExitCode (37.19s)
    --- PASS: TestConformanceK8s/EphemeralDiskLimit (114.53s)
        --- PASS: TestConformanceK8s/EphemeralDiskLimit/OverTheLimitTheRunIsEvicted (79.17s)
            --- PASS: TestConformanceK8s/EphemeralDiskLimit/OverTheLimitTheRunIsEvicted/Tmp (15.04s)
            --- PASS: TestConformanceK8s/EphemeralDiskLimit/OverTheLimitTheRunIsEvicted/Workdir (64.13s)
        --- PASS: TestConformanceK8s/EphemeralDiskLimit/AnOversizedLimitStillSchedules (35.37s)
PASS
```

Both fill cases reported actual kubelet eviction: `Evicted: Usage of EmptyDir volume "wardyn-tmp" exceeds the limit "64Mi".` and the equivalent `wardyn-work` verdict.

## Coverage boundaries and discovered harness gap

- Proves the patched driver retains real task exit codes and still provisions, observes disk eviction, and tears down actual Kubernetes pods for both metered scratch paths. The oversized-limit request-floor assertion also passed.
- Does not independently prove the missing/stale ephemeral-status fallback naturally occurred in this live cluster. Those six status combinations and the missing main-container exit are covered by deterministic regression tests that fail on baseline and pass on the patch.
- Does not exercise Wardyn API/server finalization, real SSO, CC2/gVisor, CC3/Kata, or the entire conformance suite.
- The existing conformance `minimalSpec` omits `ControlPlaneURL`. Run proxy pods exited with `config: control_plane_url is required`; the boot canary uses its own mode and passed. These selected lifecycle cases do not require an operational run proxy, so their result remains valid, but this evidence cannot support recording-upload or steady-state proxy-health claims. No unrelated harness fix was made.

## Independent security review

Reviewed `fe5c40a9ac06b2ac62fe254be443bee661e22841`: no blocking correctness or test issue. Its shared upload path reads cap+1 actual bytes, rejects oversized bodies before control-plane forwarding, audits denial, and preserves exact-cap bodies without relying on Content-Length.

`go test ./internal/egress/proxy -run 'TestBrokeredScanUploadRejectsOversizeInsteadOfTruncating|TestBrokeredUploadBodyBoundary' -count=1` passed in 0.038s with local-server permissions. The first sandboxed attempt was blocked from binding the test listener; no product failure was inferred.
