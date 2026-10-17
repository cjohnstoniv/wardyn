---
name: wardyn-k8s-runner-setup
description: Walk an operator through installing, claiming, upgrading or troubleshooting an organisation runner in a Kubernetes cluster (the wardyn-runnerd Helm chart, deploy/helm/wardyn-runnerd) — the runs namespace, sandbox.arch, apiServer.peers, the registration Secret, the org token, the claim by a second admin, the trust class, mixed-architecture clusters, and the failure modes (runner offline, revoke, relay loss, proxy self-lapse, a lost sandbox node). Use when someone wants cluster compute governed by Wardyn without the control plane dialling in, or asks about wardyn-runnerd on Kubernetes, `wardyn runner org-token mint`, `claim-org`, org_cluster, kubelet_below_floor, relay_source_rewritten, or "node powered off". NOT for deploying the Wardyn control plane to Kubernetes (k8s.enabled): that is wardyn-k8s-setup.
---

# Wardyn organisation runner on Kubernetes — install, claim, operate

Goal: a `wardyn-runnerd` pod in the cluster, registered outbound-only, claimed by a second security operator, and in an organisation pool, placing runs as pods in a dedicated runs namespace. Reuse the shipped chart; never hand-roll a manifest.

This skill is a dispatcher and a checklist. It asks the input questions, then sends every setting to the guide that owns it. Do not restate a default from memory.

## Ground truth (read these, don't restate from memory)

- The operator guide, which this walk-through follows step for step: [docs/operations/k8s-runners.md](../../../docs/operations/k8s-runners.md).
- Why it is shaped this way, with the residuals: [docs/design/0.9/k8s-runnerd.md](../../../docs/design/0.9/k8s-runnerd.md).
- Every value and its comment: [`deploy/helm/wardyn-runnerd/values.yaml`](../../../deploy/helm/wardyn-runnerd/values.yaml); the post-install notes are in the chart's `templates/NOTES.txt`.
- Every `WARDYN_*` and `WARDYN_RUNNERD_*` setting: [docs/ENV.md](../../../docs/ENV.md).
- The control plane's own Kubernetes substrate (a different thing): the `wardyn-k8s-setup` skill.

## Inputs to gather first

- The organisation's public HTTPS URL, and whether the control-plane URL's port is the chart's `relay.servicePort` (`443` by default). They must match, or every run is refused.
- The sandbox architecture (`amd64` or `arm64`). A mixed cluster needs two releases.
- A runs namespace that already exists, is dedicated to this runner and is covered by no platform default-deny policy.
- The cluster's pod and service CIDRs (for `egress.hardDenyCIDRs`) and the control plane's address range (for `apiServer.peers`).
- The runner's totals: CPUs, memory in GB, disk in GB. Register measures nothing on Kubernetes.
- Whether two security operators are available: under four-eyes the claimer must differ from the minter.

## Walk-through

Ask before each step that changes the cluster. Check the cheap prerequisites first.

1. **Prerequisites.** Confirm the kubelet is 1.31 or newer on the sandbox-arch nodes (`kubectl get nodes -o wide`), the CNI enforces NetworkPolicy, storage is ReadWriteOnce, and the cluster does not rewrite pod-to-ClusterIP source addresses. See [Requirements](../../../docs/operations/k8s-runners.md#requirements).
2. **Mint.** A security operator mints the token (Admin → Runners → Add organisation runner, or `wardyn runner org-token mint`). It is single use, lasts 72 hours and is shown once. Never paste it into chat or a file in the repo.
3. **Namespaces and Secret.** Create the runner namespace and the runs namespace; create the registration Secret with the token under the key `token`, reading it from stdin. See [Install](../../../docs/operations/k8s-runners.md#install).
4. **Values.** Author `values.yaml` from the guide's example. The chart fails the render without `org`, `runsNamespace`, `sandbox.arch` and `registration.existingSecret`. Set `apiServer.peers` and `egress.hardDenyCIDRs`.
5. **Install.** `helm install` from the chart, with `image.tag` set. With `rbac.create=false`, apply the RBAC first ([RBAC order](../../../docs/operations/k8s-runners.md#rbac-order)).
6. **Fingerprint.** Read it from `kubectl logs deploy/<release> --all-containers` and give it to the second security operator out of band.
7. **Claim.** The second operator compares the fingerprint in Admin → Runners and claims the runner. An unclaimed runner expires after 24 hours.
8. **Pool.** Add the runner to an organisation pool. Delete the registration Secret.
9. **Verify.** The card shows Online, the architecture, trust `org_cluster` and the canary verdict `enforced`. Launch a small run into the pool and check its placement on the run page.

## Troubleshooting

| Symptom | First read |
|---|---|
| Pod crash-loops with a named error | The boot error table in [Failure modes](../../../docs/operations/k8s-runners.md#failure-modes) |
| `register` refuses for the limits | `limits.totalCPU`, `totalMemory`, `totalDisk` are unset |
| Runner stays `unclaimed` or disappears | It expired after 24 hours: mint a new token and use a fresh volume |
| Every run refused for its URL port | The control-plane URL port differs from `relay.servicePort` |
| Runs cannot reach a registry or model gateway | [In-cluster services are unreachable](../../../docs/operations/k8s-runners.md#in-cluster-services-are-unreachable) |
| A run shows "containment pending" or "termination unknown" | [Sandbox node lost or partitioned](../../../docs/operations/k8s-runners.md#sandbox-node-lost-or-partitioned): decide powered off or partitioned before any step |
| A run is lost `outage` after a runnerd restart | [Self-lapse and outages](../../../docs/operations/k8s-runners.md#self-lapse-and-outages): the proxy deadline passed |
| Pods `Pending` on an arch-mixed cluster | An operator node affinity excludes `sandbox.arch`; boot does not check affinity |

## Rules

- Never taint a node, force-delete a pod or strip `wardyn.dev/stop-proof` for a run unless the guide's table says that step applies to the node's confirmed state. A partitioned node gets no destructive step.
- Never set `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL` or `WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY`: runnerd refuses to boot with either.
- Never use a production credential or the owner's token on the operator's behalf; ask them to run the mint and claim steps.
