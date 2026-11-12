> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Kubernetes: known gaps

- The `k8s` runner substrate ([`deploy/helm/wardyn`](../../deploy/helm/wardyn), `k8s.enabled=true`, [`internal/runner/k8s`](../../internal/runner/k8s)) is a separate confinement substrate from the Docker Compose path (L1/NetworkPolicy-backed vs. Compose's L0 structural one).
- Most of this document applies to both; the table below is what the Kubernetes substrate does not do.

## Gaps

- None of these are silent: the mount and BYOI gaps fail the run closed with a named error, and the resource-cap gaps log a warning naming what is unenforced.
- A sandbox is two pods, an agent pod and a proxy pod. Each run gets two NetworkPolicies: the agent pod may reach only its proxy pod, and the proxy pod accepts connections only from the agent pod.

| Gap | Status | What happens | Why, or what to do |
| --- | --- | --- | --- |
| No BYOI or devcontainer builds | Refused | A `wardyn-byoi/`-prefixed image ref is refused before any pod is created. `WARDYN_ENVBUILD` devcontainer builds are Docker-only too, whatever `k8s.enabled` says. | Ephemeral containers can't honor the selftest-then-task double-exec BYOI needs ([`internal/runner/k8s/errors.go`](../../internal/runner/k8s/errors.go)'s `errBYOIUnsupported`, [`internal/runner/k8s/exec.go`](../../internal/runner/k8s/exec.go)). |
| No `local_dir` or host-path workspace mounts | Refused | A policy with any `WorkspaceMounts` entry fails the run closed with a named error ([`internal/runner/k8s/sandbox.go`](../../internal/runner/k8s/sandbox.go)'s `errMountsUnsupported`). Git-clone workspaces (`WorkspaceRepos`) are unaffected. | A pod has no path back to a directory on wardynd's own host, so there is nothing to stage. A model provider's credential injection and the Bedrock AWS SSO exchange happen at `wardyn-proxy`, so they work unchanged on Kubernetes. |
| No in-sandbox DNS | By design | Every sandbox pod is `DNSPolicy: DNSNone` with its own proxy pod as the single nameserver. The proxy answers every query `REFUSED` but `AAAA` for its own name, `wardyn-proxy`, which gets an empty answer and no row ([`sandbox.go`](../../internal/runner/k8s/sandbox.go), [`dns_audit.go`](../../internal/egress/proxy/dns_audit.go)). | Only the `wardyn-proxy` pod resolves hostnames, as on the Compose substrate. Its `run.dns.query` rows are capped at 64 a second per proxy; excess rows, including folded repeats, are dropped and counted in the proxy's warning. |
| No per-pod PIDs limit | Not enforced | A run's `ResourceLimits.PidsLimit` is accepted but not enforced; wardynd logs a warning naming the run id each time. | Kubernetes has no per-container pids resource. Set the node-level kubelet `podPidsLimit` (or your distribution's `SystemReserved`/`KubeReserved` PID accounting) as a cluster-wide fork-bomb backstop. |
| No revive or restart with current limits | Refused | `POST /runs/{id}/revive` answers 409, and `POST /admin/runs/restart` ("Restart with current limits") answers 200 with each run `ok: false`. Both carry reason `revive_unsupported` and change nothing. | The substrate implements no `runner.ProxyReviver`: the agent pod pins the proxy pod's IP, so a new proxy pod could not be reached. |
| Live egress edit | Partial | `PATCH /runs/{id}/egress` adds the edit's hosts to the proxy's deny list. The proxy refuses them at once and closes its proxied connections, not its DNS listener, which forwards nothing. The answer waits for its policy readback. | The agent pod pins the proxy pod's IP, so the proxy is narrowed, not replaced. An edit applies only its own hosts, not governance changes made since the run started. |
| Live egress edit that needs a new proxy | Refused | `409 egress_edit_unsupported`, and nothing changes, for a run started before 0.10.0-rc.20, on an older proxy image, or on a registered runner. So is an edit that takes away a credential lane, `via_org` route, MCP server or intercepted host. | Start a new run with the narrower policy. [Live egress edit](run-lifetime.md#live-egress-edit) lists every case, and when the deny list can be lost. |
| Ground-truth sensor | Opt-in | `groundtruth.k8s.enabled` runs a DaemonSet that ties your Tetragon's events to runs ([`cmd/wardyn-tetragon-ingest`](../../cmd/wardyn-tetragon-ingest)). Off by default. | Wardyn ships no Tetragon and does not verify its events. Gaps: gVisor and Kata runs are invisible to host eBPF, and no live-Tetragon test exists yet. See [Ground truth on Kubernetes](kubernetes-ground-truth.md). |
| A default-deny NetworkPolicy already in `k8s.runsNamespace` | Refused | wardynd refuses to boot, unless the canary pod ran and could not connect. | See [the egress canary](#the-egress-canary). |
| NetworkPolicy stops being enforced after boot | Refused for new sandboxes | The canary re-runs every 10 minutes; the first check that shows egress through a deny-all policy refuses new sandboxes in that namespace. Running sandboxes keep running. | Fix the CNI; see [re-checks after boot](#re-checks-after-boot). |
| The proxy's DNS rule covers only `kube-dns` pods in `kube-system` | Partial | The per-run proxy NetworkPolicy admits DNS to its address range (cloud-metadata address excluded) and to pods labelled `k8s-app=kube-dns` in `kube-system` ([`internal/runner/k8s/sandbox.go`](../../internal/runner/k8s/sandbox.go)). | GKE Cloud DNS, NodeLocal DNSCache on the host network and DNS outside `kube-system` (OpenShift) need an operator-supplied NetworkPolicy. A No network run's proxy has no DNS rule. |
| `replicas` stays 1 unless `ha.enabled` is set | Partial | HA is supported on Kubernetes only. | See [High availability](../OPERATIONS.md#high-availability) for what the replicas share, what stays per replica (caps multiply by the replica count) and the residual risks. With registered runners keep one replica: [what a second replica still hits](high-availability.md). |

- In place of a revive, a run whose proxy is out of date is stopped and a new run started ([run lifetime](run-lifetime.md)).

## What No network guarantees on Kubernetes

- At the proxy: every request from the sandbox is refused (`policy:no-network`), and the proxy opens no connection for it.
- Proxy DNS: every query gets `REFUSED` and a `run.dns.query` row, except an `AAAA` for the proxy's own name, `wardyn-proxy`, which gets an empty answer and no row. The proxy's own calls to Wardyn go on: the request and DNS rows, approvals and the policy readback.
- At the agent pod: its NetworkPolicy lets it reach only its proxy pod, the same for every run.
- At the proxy pod, where the egress canary shows NetworkPolicy enforced and neither `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL` nor `WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY` admitted the namespace: its NetworkPolicy allows egress only to Wardyn.
- On a server, the one peer is wardynd's pods on the internal port, with Wardyn's internal address pinned in the proxy pod's hosts file and no DNS rule. On a runner, it is `wardyn-runnerd`'s relay and sink ports. The proxy pod stays: it is the run's only link to Wardyn.
- Under either override, No network is enforced by the proxy only. The run's `run.dispatch` audit row says so: `no_network_isolation` is `proxy_only`, and `network_policy` is `netpol_not_enforced` or `netpol_ambient_acknowledged`.
- `network_policy` is `netpol_not_proven` when the namespace's verdict changed during the create, and `not_proven` when a Kubernetes runner sent nothing, as one older than this release does. The row comes from a server of this release; a missing row proves nothing on an older runner.
- The proxy pod reaches Wardyn through a pod-selector peer behind a Service address, so your CNI must match NetworkPolicy peers after Service translation. On one that matches before, the run fails about a minute after it starts, if the proxy image and runner are 0.10.0-rc.18 or newer.
- That failure says the proxy did not confirm the policy it loaded and names this cause, and its `run.policy.unconfirmed` audit row has reason `deadline`. Use a CNI that matches after translation. Wardyn's Kubernetes conformance suite checks this on Calico.
- A proxy image or runner older than 0.10.0-rc.18 never confirms its policy, so that failure does not happen. The run is ended or marked lost when its run token lapses, about 65 minutes later, without that hint.
- NetworkPolicy allows add up. Another policy in the runs namespace, or a cluster-wide policy your CNI applies, that selects the proxy pod and allows more widens it. Keep such policies off pods labelled `wardyn.managed=true`.
- On a server, Wardyn's internal address must resolve to exactly one IP from wardynd's pod. A dual-stack Service with two is refused, and the run does not start.
- A No network run on a server without `WARDYN_K8S_CONTROL_PLANE_NAMESPACE` and `WARDYN_K8S_CONTROL_PLANE_POD_LABELS` is refused too. The chart sets both.
- A run on a Kubernetes runner older than this release keeps the proxy-level guarantee only.
- A live egress edit of a No network run is refused with `409 egress_edit_no_network`, on every substrate: it has nothing to narrow.

## The agent-sandbox backend

- The optional agent-sandbox backend is experimental and off by default. It has its own limits: server mode only, no warm pools, pause, `linux_vm` or revive, and a trusted controller with cluster-wide pod rights.
- Details: [Kubernetes: the agent-sandbox backend](kubernetes-agent-sandbox.md).

## The proxy's DNS peer

- The peer is fixed to `kube-system` and `k8s-app=kube-dns`; no chart value changes it.
- If your DNS pods carry other labels, it matches nothing and DNS falls back to the broad rule.
- Check with `kubectl -n kube-system get pods --show-labels`.

## Eviction, priority and run count

- **Eviction and priority.** Sandbox pods take `k8s.sandbox.priorityClassName` and `k8s.sandbox.podAnnotations` (`WARDYN_K8S_SANDBOX_PLACEMENT`, [docs/ENV.md](../ENV.md)).
- Placement metadata cannot override the reserved labels (`wardyn.managed`, `wardyn.run-id`, `wardyn.component`) the run NetworkPolicies select on: wardynd refuses to boot, naming the key.
- Wardyn sets no `cluster-autoscaler.kubernetes.io/safe-to-evict` annotation by default. `"true"` lets the cluster autoscaler evict a pod mid-run when it scales a node down, which ends a live run; `"false"` keeps the node up while a run is on it.
- **Run count.** `WARDYN_MAX_CONCURRENT_RUNS` caps how many non-terminal runs the deployment holds ([docs/ENV.md](../ENV.md)); it bounds the pods a deployment can ask for, not what a node can fit.

## `DiskMiB`: enforced by eviction, not a quota

A run's `disk_mib` is the agent container's `resources.limits[ephemeral-storage]` and the `sizeLimit` of three `emptyDir` volumes ([`internal/runner/k8s/naming.go`](../../internal/runner/k8s/naming.go)'s `ephemeralScratchVolumes`): `wardyn-tmp` at `/tmp`, `wardyn-work` at `/home/agent/work`, `wardyn-cache` at `/home/agent/.cache`.

- The ephemeral container that runs an autonomous (task-mode) agent copies the main container's mounts ([`internal/runner/k8s/exec.go`](../../internal/runner/k8s/exec.go)), so writes to those paths land in volumes the kubelet counts as the pod's local ephemeral storage.
- An interactive run's agent runs in the pod's main container, so the kubelet counts its whole writable layer, `$HOME` and toolchain caches included. Size an interactive run's budget for its caches too.
- The three `sizeLimit`s are one budget: `emptyDir` usage counts toward the pod's `ephemeral-storage` total, so filling every volume partway still evicts.

> [!WARNING]
> A `default_disk_mib` or policy `disk_mib` binds an autonomous Kubernetes run. Size it for the clone, installs and toolchain caches, or a run is evicted with its in-flight work lost.

### What is still OUTSIDE the cap

| Outside the cap | Detail |
| --- | --- |
| The rest of `$HOME` | `/home/agent/go` (GOPATH itself is unmoved, so installed tool binaries under its `bin/` stay reachable; only `GOMODCACHE` moved under the cache volume) and `~/.cache/pip` |
| Dotfiles | `~/.wardyn`, `~/.ssh`, `~/.claude` |
| Other paths | `/opt/rust`; any authored `workspace_repos` or ephemeral-source target outside `/home/agent/work` — an authored target may legally sit at `/work`, `/workspace` or elsewhere under `/home/agent` ([`internal/runner/mount.go`](../../internal/runner/mount.go)'s allowed target prefixes) |

- Nothing is mounted at `/home/agent` itself: a volume there would shadow each image's baked `.bashrc` and swallow the reserved drive target `/home/agent/drive`.
- The cache volume starts cold: an `emptyDir` at `/home/agent/.cache` shadows the full image's pre-created `/home/agent/.cache/go-build` ([`deploy/images/full/Dockerfile`](../../deploy/images/full/Dockerfile)), so the Go build cache is rebuilt from empty.
- The volumes are writable without `FSGroup`: the kubelet creates an `emptyDir` root-owned but `0777`.

> [!NOTE]
> **What the proof does not cover:** the kind conformance evidence is from the busybox conformance-agent image on runc (CC1), and `emptyDir` metering of ephemeral-container writes is unmeasured under gVisor and Kata. The live kind SSO walk exercises a real `agent-run` boot (the aws-sso sign-in sandbox and a real claude-code run) on the `emptyDir`-backed `/tmp` and `/home/agent/work`, on runc. It is manual and self-skipping, not CI (see [`docs/TEST-GAPS.md`](../TEST-GAPS.md)).

### The mechanism's remaining gap

1. **Enforcement is by eviction, not a quota.** The kubelet kills the pod once it exceeds the limit. In-flight work is lost, and the agent never sees `ENOSPC`, so it cannot flush or fail gracefully.
   - The completion watcher and restart reconciler recognize a terminal pod even if Kubernetes never publishes the agent container's exit status. An unknown agent exit is a failed run; a recorded agent exit keeps its actual result.
   - Run finalization then revokes credentials and tries to remove the run's sibling objects. These are the proxy pod, which still runs with its resolved upstream credentials, and the per-run Secret holding the run token, the MITM CA key and any injected git token.
   - A failed cleanup is retried by the control plane's orphan sweep ([`internal/api/reconcile.go`](../../internal/api/reconcile.go), implemented by [`internal/runner/k8s/lifecycle.go`](../../internal/runner/k8s/lifecycle.go)'s `SweepOrphanedSandboxes`) on the next boot and on its cadence after. Candidates must be past `undispatchedGrace`, cleanup can take longer while the Kubernetes API is unavailable, and a user drive's claim is never touched.
   - A stop or kill of a run whose agent pod is already gone removes the siblings itself, because the sandbox ref is the agent pod name. The sweep lists the per-run Secret, both NetworkPolicies and the pods, so a run whose agent and proxy pods are both gone (a deleted node takes them together) is still found.
2. **The kubelet measures periodically** (about 10 s), so a fast burst can overshoot the limit before the next tick catches it.
3. **With neither a policy-authored `disk_mib` nor a `default_disk_mib`** on the deployment's storage provider, a run's `disk_mib` is absent and both `ephemeral-storage` keys stay off the pod. Node-level eviction, a cluster-wide bound rather than a per-run one, is then the only limit, the same shape as the PIDs gap.
   - Whenever a limit is set, the agent container also carries an `ephemeral-storage` request of 256Mi, or the limit when that is smaller. Kubernetes copies an unset request from the limit, so without it the scheduler would inherit the org's whole ceiling.
   - On a node whose allocatable ephemeral storage is already short of that request, a run that sets a cap can sit `Pending` on "Insufficient ephemeral-storage".

## The `enforcement` vocabulary (shared with user drives)

User drives and `disk_mib` report the same `enforcement` values, `types.StorageEnforcement`:

| Value | Meaning |
| --- | --- |
| `filesystem` | A quota binds it |
| `eviction` | The kubelet kills the pod |
| `request` | A volume request; the storage class decides |
| `external` | Somebody else's quota binds it |
| `none` | Nothing binds it |

- A drive is `request` on a managed claim and `external` on a share.
- `disk_mib` is `filesystem` on a Docker host whose storage driver can enforce a per-container quota, `eviction` on Kubernetes, and `none` on a Docker host whose driver cannot. `none` covers two outcomes ([`internal/runner/docker/hardening.go`](../../internal/runner/docker/hardening.go)'s `applyDiskQuota`):
  - A driver that takes no size option (`vfs`, `fuse-overlayfs`) runs the run uncapped with a warning.
  - On overlay2 over a filesystem other than xfs with `pquota`, a size the run's policy wrote is refused and the run fails closed. A size that came from a request or a default runs uncapped with a warning.

> [!NOTE]
> Wardyn never enforces a drive's size itself. On Kubernetes the size is the volume request and the storage class decides whether it binds — block disks do, network-share provisioners do not. On Docker a managed drive has no byte cap, the same gap disk_mib has. A share is bounded by its own quota. The size you see is the allocation, not a guarantee.

## The egress canary

wardynd runs the canary at boot in every runs namespace, then again every 10 minutes.

A pre-existing default-deny NetworkPolicy in `k8s.runsNamespace` refuses boot, with no override, unless the canary pod ran and could not connect.

1. Phase A applies no NetworkPolicy of its own; it only proves the cluster is reachable before phase B proves Wardyn's deny-all rule takes effect.
2. If the namespace already carries a default-deny policy from something else, phase A's pod is blocked too, and wardynd refuses to boot with an indeterminate verdict, indistinguishable from a genuinely broken cluster ([`internal/runner/k8s/canary.go`](../../internal/runner/k8s/canary.go)).

**Fix:** give `k8s.runsNamespace` a namespace with no ambient default-deny, or exempt Wardyn's pods from *that policy's own* `podSelector` (a `matchExpressions` entry with `key: wardyn.managed`, `operator: NotIn`, `values: ["true"]`).

> [!CAUTION]
> **Do not instead add a separate allow policy for `wardyn.managed=true`**: NetworkPolicy allows are additive, and both the agent and proxy pods carry that label. Such a policy widens every sandbox pod's egress past Wardyn's per-run deny+proxy-only policy ([`internal/runner/k8s/sandbox.go`](../../internal/runner/k8s/sandbox.go)) and flips the canary's phase B to "CNI does not enforce" — which in turn invites `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1` and fully unconfined runs.

A managed, multi-tenant cluster may apply an ambient default-deny as its baseline.

- If the canary pod reached Running and its own connect exited with exactly 1 (not "never reached Running", not any other exit code), `WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1` acknowledges that shape and boots anyway.
- That is never proof of enforcement, because phase B is skipped. The setup checklist's `k8s_egress_containment` row grades it `warn` ("acknowledged, not proven"), never `ok`.
- Only the exempt-the-podSelector fix gives real proof.

### Re-checks after boot

- Each re-check runs the same two phases. A definite result changes the verdict at once: a check that shows egress through a deny-all policy refuses new sandboxes from that check on, and a check that proves enforcement allows them again.
- Only a check that cannot tell (an API error, a pod that never ran) waits: three in a row (about 30 minutes) make the verdict "not proven", so one flaky check never moves it.
- Under `WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1`, a phase A pod that exits 1 waits too where a definite verdict stands, since a pod-network blip looks the same. Three in a row make "enforced" read "acknowledged", and "not enforced" read "not proven". Agent-sandbox mode refuses on "acknowledged", so `/healthz` reports it as `not_proven` there.
- Once a namespace's verdict is "not enforced" or "not proven", new sandboxes there are refused. Running sandboxes keep running; stop them yourself if they must not outlive the guarantee.
- `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1` still lets plain sandboxes start where the canary proved NetworkPolicy not enforced, as at boot. It never covers "not proven", and agent-sandbox creates (`WARDYN_K8S_AGENT_SANDBOX`) need proven enforcement whatever it says.
- After a CNI change, the next check sees it within 10 minutes. Restart wardynd, or the runner, to check at once.
- `/healthz` reports `network_policy` and `network_policy_checked_at`, the time of the check that produced that verdict; checks that could not tell, fewer than three, never move it. After a failed re-check the verdict is `refused` (not enforced, new sandboxes refused) or `not_proven`. With several runs namespaces it is the worst one's, with its own time. The `k8s_egress_containment` setup row shows the same.
- A Kubernetes runner reports its verdict when its session starts, and again after each re-check that changes the verdict or its time. On a runner, the refusal at create is what stops a new sandbox.
- wardynd's `/healthz` shows the worst of its own verdict and every connected runner's, with `network_policy_source` set to `runner` when it is a runner's. The setup row shows wardynd's own.
- Health reflects connected runners only. A runner's verdict reappears when it reconnects.
- A change on a runner writes a `k8s.netpol.update` row naming the runner. Its first verdict, when not `enforced`, writes `k8s.netpol.fail`.

The canary's verdict is on the admin setup page's Environment step (`GET /setup/status`, the `k8s_egress_containment` check). The same step holds the Fence/Wall/Vault tier matrix for every driver.