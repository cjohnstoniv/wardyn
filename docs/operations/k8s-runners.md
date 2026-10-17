> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Organisation runners in a Kubernetes cluster

- An organisation runner is a `wardyn-runnerd` pod that places each run as an agent pod and a proxy pod in a dedicated runs namespace of the cluster it runs in.
- It dials the organisation outbound only: the control plane never reaches into the cluster.
- This page is for the platform engineer who installs it and the security operators who claim and run it. Design and residuals: [the design note](../design/0.9/k8s-runnerd.md).

## When to use it

| Situation | Use |
|---|---|
| Compute lives in a cluster the control plane should not dial into (another cloud, another account, on premises) | An organisation runner: this page |
| The control plane runs in the cluster that also runs the sandboxes | The control-plane chart's `k8s.enabled` substrate: [Kubernetes runner substrate](../../deploy/helm/wardyn/README.md#kubernetes-runner-substrate-k8senabled) |
| A person's own machine | A client-mode runner: [Managed laptops](hybrid-laptops.md) |
| A run must reach a service inside the cluster (an artifact registry, a model gateway) | Not an organisation runner: see [In-cluster services are unreachable](#in-cluster-services-are-unreachable) |

- An organisation runner belongs to the organisation, not to a person. Its owner column is empty and its [trust class](#trust-class) is `org_cluster`.
- It takes runs only through an organisation [runner pool](runner-pools.md); no user pool can hold it.

## Requirements

| Requirement | Detail |
|---|---|
| Kubelet version | 1.31 or newer on every schedulable node of the sandbox architecture. Boot refuses with `kubelet_below_floor` and names the nodes. |
| NetworkPolicy enforcement | The CNI must enforce policy. Boot runs an egress canary and starts only on an `enforced` verdict. |
| Runs namespace | Exists before the install, is dedicated to this runner, and is covered by no platform default-deny policy. The chart never creates it. |
| Storage | One ReadWriteOnce volume. NFS-class and ReadWriteMany storage are unsupported because the state relies on `flock`. |
| Node list | The ServiceAccount lists every Node in the cluster (cluster-wide read of names, addresses, labels, taints and images). A cluster that refuses this cannot host organisation runners. |
| Pod-to-ClusterIP traffic | The cluster must not rewrite its source address (for example kube-proxy `masqueradeAll`). Boot refuses with `relay_source_rewritten`. |
| Organisation URL | `WARDYN_RUNNER_ORG_URL` is set on the control plane. The runner's `org` value must match it. |
| Control-plane URL port | The port of the organisation's control-plane URL equals `relay.servicePort` (`443` by default). A run whose URL port differs is refused. |
| Two people | Under four-eyes, the security operator who claims is a different human from the one who minted. |

- Runnerd records the Node each run's pods bound to. Do not reuse a Node name for another machine while any run's pods remain: see [Sandbox node lost](#sandbox-node-lost-or-partitioned).
- Replacement nodes take fresh names. That includes names derived from a reusable address and hostnames kept across an on-premises rebuild.

## Install

1. A security operator mints a single-use `wdr_` token with `wardyn runner org-token mint --name <name>` (`POST /api/v1/runners/org-tokens`). It lasts 72 hours and prints once. The console has no form for this.
2. Create the two namespaces. The runner's own namespace holds only runnerd; the runs namespace holds every sandbox.

   ```sh
   kubectl create namespace wardyn-runner
   kubectl create namespace wardyn-runs
   ```

3. Store the token in a Secret under the key `token`. The chart never templates it.

   ```sh
   kubectl -n wardyn-runner create secret generic wardyn-runner-registration \
     --from-file=token=/dev/stdin
   ```

4. Write `values.yaml`. The chart refuses to render without `org`, `runsNamespace`, `sandbox.arch` and `registration.existingSecret`.

   ```yaml
   fullnameOverride: wardyn-runner
   org: https://wardyn.example.com
   runsNamespace: wardyn-runs
   sandbox:
     arch: amd64
   registration:
     existingSecret: wardyn-runner-registration
   limits:
     totalCPU: 16
     totalMemory: 64
     totalDisk: 400
   apiServer:
     peers:
       - ipBlock: {cidr: 10.0.0.2/32}
   egress:
     hardDenyCIDRs: ["10.244.0.0/16", "10.96.0.0/12"]
   ```

5. Install the chart from a checkout of the release you run.

   ```sh
   helm install wardyn-runner deploy/helm/wardyn-runnerd \
     --namespace wardyn-runner -f values.yaml
   ```

6. Read the fingerprint the init container printed.

   ```sh
   kubectl -n wardyn-runner logs deploy/wardyn-runner --all-containers
   ```

7. A second security operator, signed in as themselves (an API token cannot claim), finds the runner with `wardyn runner list --all` and claims it with `wardyn runner claim-org <runner-id> --fingerprint <fingerprint>` (`POST /api/v1/runners/{id}/claim-org`). An unclaimed runner expires after 24 hours.
8. An admin adds the runner to an organisation pool with `wardyn runner pool add <pool> <runner>` (`PUT /api/v1/runner-pools/{pool}/runners/{runner}`). `wardyn runner pool remove`, or `DELETE` on the same path, takes it out. An organisation runner goes only into an organisation pool, and a runner serves one pool only.
9. Confirm in *Admin → Runners* that the runner is Online, reports its architecture and shows the trust class "Organization cluster". Runnerd boots only on an `enforced` canary verdict, so a running pod has passed it.
10. Delete the registration Secret. The chart mounts it optional, so restarts keep working.

- The chart is not published to a registry in 0.9.3. Install it from a checkout of the release tag, where the default `image.tag` is that release.
- The registration token already fixes `owner_kind` as organisation, so the runner is `org_cluster` from registration. The claim records who bound it and when.
- A break-glass mint (an admin API token under four-eyes) is audited `governance.change.bypass` and never counts as the second human. Claiming it is audited `runner.claim` with `break_glass` true.
- The init container runs `wardyn-runnerd register --substrate k8s --if-unregistered`. The Ed25519 key is generated on the volume and never leaves it.
- Register measures nothing on Kubernetes. The three totals are required in practice: `limits.totalCPU` in CPUs, `limits.totalMemory` and `limits.totalDisk` in GB. Register refuses until they are set.

### Values

| Value | Meaning |
|---|---|
| `org` | The organisation's public HTTPS URL (required) |
| `runsNamespace` | The pre-existing runs namespace. Empty, or equal to the release namespace, fails the render (required) |
| `sandbox.arch` | `amd64` or `arm64` (required): pinned as every run pod's `kubernetes.io/arch` selector |
| `sandbox.maxPodLifetime` | Upper bound on both pods' `activeDeadlineSeconds`; default `168h` |
| `registration.existingSecret` | The Secret holding the token under `token` (required) |
| `limits.*` | `maxRuns`, `totalCPU`, `totalMemory`, `totalDisk`, `perRunCPU`, `perRunMemory`, `perRunDisk`; zero leaves a flag out |
| `recording` | `pg`, `fs` or `off`: the runner's ceiling for session recording |
| `trustedCA.configMap` | A ConfigMap holding a PEM bundle under `ca.pem`, trusted by the org session, the proxy and the sandbox |
| `relay.servicePort`, `relay.targetPort` | The Service port (`443`) and the pod port (`8443`) |
| `sink.port` | The decision sink port on the same Service (`8444`) |
| `clusterDomain` | The cluster DNS suffix (`cluster.local`) |
| `apiServer.ports`, `apiServer.peers` | The apiserver egress rule: ports `443` and `6443`, and raw NetworkPolicy peers |
| `egress.orgCIDRs`, `egress.extra`, `egress.hardDenyCIDRs` | The organisation egress rule, extra raw egress rules, and extra CIDRs every proxy denies |
| `k8s.*` | Sandbox placement: `sandbox.nodeSelector`, `tolerations`, `affinity`, `priorityClassName`, labels, `runtimeClasses.CC2` and `CC3`, `imagePullSecret` |
| `rbac.create` | `false` when a platform team applies the RBAC itself: see [RBAC order](#rbac-order) |

- `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL` and `WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY` have no values. The chart refuses `k8s.allowUnenforcedNetPol` and `k8s.ackAmbientDefaultDeny`, and runnerd refuses to boot with either variable set.
- The chart passes flags and `WARDYN_RUNNERD_*` settings to runnerd; every one is listed in [ENV.md](../ENV.md).

## Trust class

| Class | Meaning |
|---|---|
| `personal` | A person's own runner (the default for every existing runner) |
| `managed_device` | An administrator's assertion that the device is managed |
| `org_cluster` | An organisation runner. This class holds if and only if the runner is organisation-owned |

- Only a security operator changes a class: `wardyn runner trust <runner-id> <class> --reason <text>` (`PUT /api/v1/runners/{id}/trust`), or the trust section of *Admin → Runners*. It goes through the governance-change path and is audited `runner.trust.write`.
- An organisation runner is always `org_cluster` and cannot be changed. The write moves a person's runner between `personal` and `managed_device` only.
- A runner cannot raise its own class: the registration payload, capabilities and posture never feed the column.
- Trusting `org_cluster` means trusting that cluster's administrators with the per-run and keystore Secrets, etcd backups and the per-run MITM key.
- The delivery policy can differ per class (`by_trust`, falling back to the global row). A run is judged at the highest class among its eligible runners and bound to a runner of that class.
- A downgrade does not pull values already delivered. A resident delivery or revive that finds the class changed is refused `runner_trust_changed`.
- The CC3 floor is unchanged: no runner takes a resident credential for a CC3 run.

## What the chart applies

| Object | Rule |
|---|---|
| Deployment | One replica, `Recreate`. Never raise the replicas: `serve.lock` keeps one writer |
| RBAC, runs namespace | The control-plane chart's sandbox Role plus `pods: patch` (for the stop-proof finalizer). No `services`, no `watch` |
| RBAC, runner namespace | `secrets: create, get, delete, deletecollection` |
| RBAC, cluster | `runtimeclasses: get` and `nodes: list` only |
| Agent pod policy | Egress only to its proxy |
| Proxy pod policy | Egress as before, plus runnerd on the relay and sink ports |
| runnerd ingress | Only from proxy pods of the runs namespace, on the relay and sink ports |
| runnerd egress | DNS, the apiserver, and the organisation: port `relay.servicePort` to `egress.orgCIDRs` |
| Runs namespace | No default-deny policy: it would trip the canary |

- Sandbox pods have no ServiceAccount token mounted, and a pod can name only a ServiceAccount of its own namespace.
- With `apiServer.peers` empty, runnerd may reach TCP 443 and 6443 at any address, including the pod network. Set `ipBlock` peers for the control plane.
- With `egress.orgCIDRs` empty, port `relay.servicePort` is open to `0.0.0.0/0` except RFC 1918, `100.64.0.0/10` and `169.254.0.0/16`.
- `pods: patch` can also change a running pod's image. It adds no authority beyond the pod create, exec and NetworkPolicy create the runner already holds in that namespace.

### RBAC order

1. Helm applies RBAC kinds before the Deployment on install and upgrade, so a default install needs nothing.
2. With `rbac.create=false`, apply the ClusterRole, ClusterRoleBinding, both Roles and both RoleBindings before the image starts, and again before any upgrade that adds a verb.
3. Render what the chart would apply: `helm template wardyn-runner deploy/helm/wardyn-runnerd -n wardyn-runner -f values.yaml --set rbac.create=true --show-only templates/rbac.yaml`.
4. Runnerd's boot access review refuses to start while any verb is missing, and names the missing verbs.

## Mixed architectures

- One release serves one architecture: `sandbox.arch` is `amd64` or `arm64`.
- A mixed cluster installs two releases. Each takes its own release name, registration Secret (a fresh token) and runs namespace.
- A shared runs namespace cannot work: the foreign-object check refuses a namespace holding another runner's objects.
- A `kubernetes.io/arch` in `k8s.sandbox.nodeSelector` of another architecture refuses boot. A node affinity that excludes the architecture is not checked: the pods stay `Pending` instead.

## Upgrade

1. Read the release notes for a verb added to the Roles. Apply that RBAC first when `rbac.create` is `false`.
2. Run `helm upgrade` with the new `image.tag`. The chart pins runnerd and the proxy to one tag.
3. `Recreate` stops the old pod before the new one starts. The organisation supports runnerd N and N-1.
4. Runs survive the restart only if runnerd is READY again and each proxy renews before its deadline: see [Self-lapse](#self-lapse-and-outages).

- The state volume holds the runner's identity. Losing it is identity loss: mint a new token, use a fresh volume and revoke the old runner.
- There is no re-key in 0.9.3.
- Upgrade the control plane first. Runnerd N-1 keeps working against it.

## Self-lapse and outages

| Fact | Value |
|---|---|
| Proxy deadline | The send time of its last successful renew plus 60 minutes |
| Renew cadence | Every 10 minutes (plus or minus 1) after a success; every 30 seconds (plus or minus 5) after a failure |
| At the deadline | The proxy closes tunnels, drains its sink, exits non-zero and is never restarted |
| Decisions during an outage | Buffered up to the grant: 5,000 records or the byte credit, whichever comes first |
| Pod lifetime | Both pods end at `sandbox.maxPodLifetime` (default `168h`) at the latest |

- A run survives a runnerd outage if and only if runnerd is READY again, so that the relay forwards, and the proxy's next renew succeeds before its deadline. Boot counts against the budget.
- With healthy renewals the last success is at most about 11 minutes before the crash, so a runner READY again within about 48 minutes keeps its runs. That is an example, not a guarantee.
- Renewals that were already failing shorten the budget: last success at minute 0, runnerd down from minute 40 to 61, and the run lapses at minute 60.
- After the deadline the agent stays up without network until runnerd returns, then is contained and the run is lost with reason `outage`. There is no revive on Kubernetes in 0.9.3.
- During an outage new requests are refused once the grant is used. Approvals and brokered internal routes fail while the relay is down; the run itself is not ended.
- The deadline runs on the boot clock, which counts suspended time, so a paused node does not extend it.
- Identity renewals are audited as `identity.renew`: on a Kubernetes runner a run writes 6 rows an hour, not 2.

## Failure modes

| Event | What happens | What you do |
|---|---|---|
| Runner offline, inside the budget | Pods keep running; decisions buffer; the run continues on reconnect | Restore runnerd |
| Runner offline past a proxy's deadline | That proxy exits; the run is lost `outage` | Restore runnerd; the run does not revive |
| Proxy crashes, runs out of memory or lapses | Terminal: the pod stays `Failed`, runnerd contains the run and settles its decisions | Start a new run |
| Relay lost | First-use approvals are denied; decisions buffer | Restore the relay Service |
| New proxy pod's IP still fenced for an earlier run | The runner stops the new run before it opens; the run fails with reason `relay_ip_fenced` | Start the run again. The address frees when the earlier run's stop is confirmed |
| Revoked while online | The org ends the runner's runs `runner_revoked`; runnerd stops every proxy, tears down every sandbox and deletes the per-run and keystore Secrets | Reinstall with a fresh token if wanted |
| Revoked while offline and never returning | Nothing releases the stop-proof finalizer or the policies | Follow [Revoked and gone](#revoked-and-gone) |
| runnerd's node lost | The old pod stays Terminating and the volume stays attached | Force-delete the pod, or taint the node `node.kubernetes.io/out-of-service`; proxy deadlines bound egress meanwhile |
| Boot refusal | runnerd exits with a named error | See the table below |

| Boot error | Cause | Remedy |
|---|---|---|
| Canary not `enforced` | The CNI does not enforce policy, or a platform default-deny covers the runs namespace | Use a CNI that enforces; use a runs namespace the deny does not cover. No override exists |
| Access review names verbs | A Role or ClusterRole verb is missing | Apply the RBAC ([order](#rbac-order)) |
| Foreign objects | The runs namespace holds `wardyn.managed` objects with another `runner-id` or none | Clean the namespace, or use another |
| Relay Service not one IP | The Service does not resolve to exactly one ClusterIP | Fix the Service; do not use a headless one |
| `relay_source_rewritten` | The cluster rewrites pod-to-ClusterIP source addresses | Unsupported: use a cluster that keeps pod source addresses |
| `kubelet_below_floor` | A schedulable node of the sandbox architecture runs a kubelet older than 1.31 | Upgrade the named nodes |
| No decision sink | The decision sink did not start, so the runner refuses to create a sandbox | Read the sink error in the log |
| Arch conflict | `k8s.sandbox.nodeSelector` names another `kubernetes.io/arch` | Remove the key or match `sandbox.arch` |

- The kubelet floor is conservative. Since [Kubernetes 1.27](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/) the kubelet moves a deleted pod to a terminal phase before it is deleted, except static pods and force-deleted pods without a finalizer. [Pod disruption conditions](https://kubernetes.io/docs/concepts/workloads/pods/disruptions/#pod-disruption-conditions), including `DeletionByPodGC`, are stable since 1.31.
- Below the floor a stop has no terminal status, so the run waits in termination unknown instead of ending.
- A node that joins below the floor after boot is shown on the runner card.

## Sandbox node lost or partitioned

A run on a lost node is fenced at the relay before its first delete. Both pods stay Terminating; runnerd keeps the NetworkPolicies, the per-run Secrets and the liability, and reports the run to the organisation as `ContainmentPending`. Other runs on the runner stay READY.

| Node state | Do |
|---|---|
| Powered off, confirmed out of band (provider console or management controller) | Taint it `node.kubernetes.io/out-of-service`. This releases a run whose Node object still shows the machine recorded when the run bound. If the Node object is already gone, record "Node powered off" (last bullets) |
| Partitioned, or state unknown | Nothing destructive: no taint, no force delete. The run waits until the node returns or is confirmed off |
| Healthy node, no causal association, no status bound | Two security operators record "No prior holder", while the holder still reports the pod's terminal status. If another machine held the name, the record is "Node powered off" |
| Node name reused | Only the two-person "Node powered off" record, covering every machine that used the name since the run bound |

- The taint is read only for a run whose Node has a causal association, still shows the machine recorded at binding, and has no recorded name reuse. It is never a release for a reused name.
- A deleted Node object is not confirmation. The record binds to the run, its pods, the node name and the machine.
- A force delete is never evidence. Runnerd's finalizer keeps the object, and runnerd keeps the fence. Never strip `wardyn.dev/stop-proof` by hand.
- Where another machine held the name, drain and power off the replacement, and delete its Node object, before recording the original machine's release.
- An anchored container whose closing list differs only in InternalIP is refused tier 1 and takes the record path. That is the safe side of a rebooted node that returns on a new address.
- The pending run keeps its concurrency slot, and a reinstall refuses until the namespace is clean.
- Make the record on the run's detail page (*Record the node's state*, shown to security operators while the run is held), or with `wardyn runner node-record <run-id> --attestation node_powered_off|no_prior_holder --source provider_console|management_controller|other --source-id <id>` (`POST /api/v1/runs/{id}/node-record`). Both show the machines the runner recorded and send them with the record. The admin token cannot make one.
- The run detail's label decides the path. "Node name reused" and "No causal association" allow only the record. A run held with a causal node can also be released by the taint.
- A record needs two security operators for either of those labels, and whenever the organisation's four-eyes rule is on; the second approves it in *Governance → Changes*. Local mode cannot show who the people are, so it refuses a two-person record. The audit row is `run.node.record`.
- "No prior holder" is accepted only for a run with no causal association, and never after a recorded name reuse.

### Revoked and gone

A revoked runner whose runnerd never returns leaves objects behind. Select them in the runs namespace by the labels runnerd stamps, `wardyn.managed=true,wardyn.runner-id=<runner-id>`. In this order:

1. For each run whose node is confirmed healthy or powered off, remove only `wardyn.dev/stop-proof` from its pods, with a JSON patch that carries a `test` op.
2. Confirm those pods are gone (NotFound, not merely Terminating).
3. Delete the per-run and keystore Secrets.
4. Delete the NetworkPolicies, last.

- Never clear the finalizer for a run on a partitioned node or one whose state is unknown. Its pods and policies stay until the node is confirmed powered off or returns.
- A reinstall refuses until the namespace is clean. Pods end at their deadline anyway.

## In-cluster services are unreachable

- Runnerd writes a hard deny into every proxy: the `svc` suffix and the cluster domain, the relay ClusterIP, runnerd's pod IP, `apiServer.peers` and `egress.hardDenyCIDRs`.
- The chart adds the apiserver's own addresses, read from the `kubernetes` Service's EndpointSlice at `helm install` and `helm upgrade`.
  - The read is Helm's `lookup` of the `kubernetes` EndpointSlice in `default`, so the installing identity needs `get` on `endpointslices` in `default`.
  - A render without cluster access adds none: `helm template`, and a GitOps tool such as Argo CD that renders with it, give an empty entry. Set `egress.hardDenyCIDRs` and `apiServer.peers` there.
- The deny is checked on the name and every resolved address before the `internal_hosts` lift, so declaring a host does not help.
- A run therefore cannot reach any in-cluster service, such as an artifact registry or a model gateway. Reach such a service by an external name or address outside the deny set, or host it outside the cluster.
- Set `egress.hardDenyCIDRs` to the cluster's pod and service CIDRs.

| What you see or need | Fact |
|---|---|
| A denied request in the audit | The row's rule source is `builtin:private-ip`, with the class "an in-cluster name or address this runner denies to every run". There is no `hard_deny` rule source |
| An operator-authored gateway or trusted URL | Dialled by the runner path, not by agent egress, so the deny does not apply to it. An in-cluster model gateway you configure still works |
| An upstream proxy inside the cluster | It voids this protection. A name the proxy cannot resolve locally gets only the name and suffix check, and the upstream proxy can resolve it to an in-cluster address |

- Do not point the runner's upstream proxy (`--upstream-proxy` or `WARDYN_UPSTREAM_PROXY`) at an address inside the cluster. Runnerd refuses it at boot when the address is a literal IP inside the hard deny, and names the address. A host name is not checked at boot.

## What the kind conformance run proves

`scripts/k6-kind-conformance.sh` (`WARDYN_TEST_K8S=1`) is the proof.

- It builds a kind cluster of one control plane and two workers on Calico, and installs this chart with its rendered Role against a fake organisation.
- It runs [`test/k6`](../../test/k6/k6_test.go) under runnerd's own ServiceAccount token, never cluster-admin.
- It runs on the 1.31 floor (`kindest/node:v1.31.4`) and on `kindest/node:v1.35.0`. Both passed all 19 cases on the 0.9.3 tree; the table groups those with an operator-visible result.

| Case | What it shows |
|---|---|
| In-cluster runnerd boot | The chart Role, access review, foreign-object scan, kubelet floor, relay listen and relay source probe pass on Calico |
| Rendered Role | A pod naming runnerd's ServiceAccount is refused; seven denials hold under runnerd's token |
| Keystore | Set (create, then replace), Get and Delete work under the own-namespace Role |
| Finalizer semantics | A force delete leaves a finalized object held; a patch with a wrong UID or stale version is refused |
| Normal stop | `StopProxy` ends in the `terminal` arm with a causal association |
| Pod IP reuse after stop | A stopped proxy's IP is released and a later proxy that receives it starts; an IP still fenced for another run is refused |
| Bind and delete, serial and overlapping | Both winning orders were observed on each kubelet: delete-first gives arm `unbound_deleted`, bind-first gives `never_initialized` or `terminal` with a causal association |
| Never initialised | An image pull that fails, and a cancel during the pull, both complete |
| Restart while unbound | A restart between the unbound read and the bind keeps the run causal |
| Crash gaps | A crash between steps of `StopProxy`, label teardown and the orphan sweep finishes after restart |
| Hard deny in proxy config | `svc`, the cluster domain, the relay ClusterIP and the apiserver are in the proxy's `hard_deny` |
| Retained-policy probe | A pod held for deletion, and one whose finalizer was stripped, make 0 connections through the retained policy on kind's Calico |
| Node name reuse | A pod GC mark and a reused name are recorded on the tick, and `StopProxy` is refused |

The bind and delete cases are the release gate for the unbound-pod arm, and they passed on both versions.

The run did not cover:

- Cases that need an organisation hub session, which the fake organisation does not provide. They are a renew through the relay, the proxy's self-lapse end to end, and a hard-denied agent request.
- The same missing hub hides restart fence replay, sink re-registration, the organisation-triggered sweep and keystore deletion on revoke. They also hide an in-flight request across the 3-second drain. These have unit tests only; none ran in a cluster.
- Placement by trust class for an organisation runner and the refusal for a personal runner.
- Enforcement on a CNI other than Calico on kind, or a managed cloud's control plane.
- Two kubelets under one Node name, or a holder acting on a stale snapshot. Both are unsupported; the operator requirement above stands in for them.
- Whether a provider names replacement nodes freshly. Check your provider's documentation; Wardyn makes no claim about any provider's default naming.

## Residuals

| # | Residual |
|---|---|
| R1 | Organisation-runner evidence stays `runner_asserted` |
| R3 | With runnerd down, agents run without network until runnerd returns or the pod deadline arrives |
| R4 | If the runner namespace's CNI does not enforce policy, the relay and sink ports are reachable in-cluster. The relay carries ciphertext and the sink needs a per-run key |
| R9 | `ipBlock` versus pod-IP semantics, and policy enforcement for a pod marked for deletion, depend on the CNI |
| R10 | A proxy that lapses or exits ends its run, with no revive |
| R11 | A pod that cannot be confirmed stopped may serve cached-policy egress until its proxy deadline |
| R12 | Clusters that rewrite pod-to-ClusterIP source addresses are unsupported |
| R14 | The ServiceAccount lists every Node and patches pods in its runs namespace |
| R16 | Reusing a Node name for another machine while a run's pods remain is unsupported |

- All residuals, with the rejected alternatives: [the design note](../design/0.9/k8s-runnerd.md#residuals).
