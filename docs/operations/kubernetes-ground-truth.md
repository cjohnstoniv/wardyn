> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Ground truth on Kubernetes

Tie what a run did in the kernel to the run and its creating principal, using your own Tetragon. The chart can run the sensor that reads Tetragon's export; for any other sensor, use the join recipe below.

- Wardyn installs no kernel sensor and no Tetragon. The chart's sensor only reads the JSON file your Tetragon already writes.
- Corroboration means "your Tetragon reported it". Wardyn does not verify the sensor binary or its policy.
- The Compose `groundtruth` profile is a different pipeline. It does not run on Kubernetes.

## The shipped sensor

Off by default. `groundtruth.k8s.enabled=true` adds a DaemonSet that runs wardynd's own image with command `/wardyn-tetragon-ingest`. On each node it tails Tetragon's export and posts events to wardynd's internal listener.

```sh
helm upgrade wardyn deploy/helm/wardyn --reuse-values \
  --set groundtruth.k8s.enabled=true \
  --set groundtruth.k8s.namespace=<tetragon namespace> \
  --set groundtruth.k8s.exportDirectory=<tetragon exportDirectory>
```

| Value | Default | Meaning |
|---|---|---|
| `groundtruth.k8s.enabled` | `false` | Renders nothing while off. On needs `k8s.enabled` and a `k8s.runsNamespace`, or the chart refuses to render |
| `groundtruth.k8s.namespace` | the release namespace | Where the DaemonSet and its Secret live |
| `groundtruth.k8s.exportDirectory` | `/var/log/tetragon` | The node directory Tetragon exports into |
| `groundtruth.k8s.exportFilename` | `tetragon.log` | The export file inside it |
| `groundtruth.k8s.nodeSelector`, `tolerations` | empty | Placement. Cover every node that can host runs |
| `groundtruth.k8s.resources` | 50m / 64Mi requests, 256Mi limit | The sensor container |

### What your cluster needs

- Tetragon on every node that can host runs, exporting JSON to a file. Tetragon's chart writes `tetragon.log` in its `exportDirectory`, mode 600. Set `exportDirectory` to the value your release uses.
- A namespace that allows a hostPath volume. The Pod Security `baseline` level forbids it, so use Tetragon's own namespace or one labelled `privileged`.
- Exec events at least. `/healthz` reports `partial` with `missing_kinds` when other kinds never arrive. Add Tetragon policies for the kinds you want.
- If the sensor namespace has a default-deny egress policy, allow the sensor to reach wardynd's internal port. The chart opens the wardynd side only.

### How it works

| Part | Behaviour |
|---|---|
| Export mount | The chart mounts the export directory read-only, not the file. A rotation that renames the file and creates a new one is followed; a mounted file would stay on the old one |
| Credential | One Secret, `<release>-<namespace>-groundtruth` (cut and hashed past 63 characters). wardynd patches `token` and `control-plane-ca.pem` into it and renews the token inside its hour. The DaemonSet mounts it as a directory, so a new token needs no restart |
| Pod identity | No API token (`automountServiceAccountToken: false`). Runs as root because Tetragon writes its export 0600 root; all capabilities dropped, read-only root filesystem |
| wardynd's grant | A Role in the sensor namespace: `patch` on that one Secret, nothing else. No new ClusterRole. With `k8s.rbac.create=false`, grant the same Role yourself |
| NetworkPolicy | In another namespace, wardynd's internal-port rule admits that namespace's sensor pods only. Never the http, ssh or ui ports |
| Env | `WARDYN_GROUNDTRUTH_SOURCE=k8s`, `WARDYN_GROUNDTRUTH_K8S_NAMESPACES` (the runs namespace) and `WARDYN_NODE_NAME` (the node). wardynd gets `WARDYN_GROUNDTRUTH_TOKEN_SECRET` and not the file form; setting both refuses boot |

- On first install the sensor starts before the CA exists. It waits for wardynd to patch it into the Secret and for the kubelet to sync the file, about a minute, and logs the wait once. It exits after 10 minutes without one.
- The Secret is kept on uninstall. Delete it by hand if you remove the sensor for good.

### What an event becomes

- An event maps to a run when its pod is `wardyn-agent-<run UUID>`, in the runs namespace, in the agent container. Proxy pods, other namespaces and look-alike names stay unmapped.
- A mapped event is a `kernel.*` audit row on the run. It shows in the run's Audit tab, and the run gives the creating principal as in [the join](#the-join).
- An unmapped event is dropped and counted in `wardyn_groundtruth_dropped_unmapped_total`. It is not stored.
- wardynd re-checks a mapped event. One from a pod outside the runs namespace (`namespace_not_runs`), or for a run that ended over 15 minutes ago (`run_ended`), loses its run and is stored as unmapped with that reason.
- Each sensor sends a `kernel.sensor.ping` heartbeat that names its node. `/healthz` lists every node once two are live: see [Monitoring](monitoring.md#per-node-sensor-health).

### Runs the sensor cannot see

- Host eBPF sees the host kernel only. A gVisor (CC2) or Kata (CC3) guest has its own kernel, so its processes never reach the export.
- With the Kubernetes sensor on, each CC2 and CC3 run gets one `kernel.sensor.bypass` row at dispatch (`cc2-gvisor-host-ebpf-blind` or `cc3-kata-host-ebpf-blind`). Actor `wardynd`.
- Compose differs: a CC3 run gets that row on every install, and a CC2 run gets none. A Compose CC2 run is blind to the sensor with no row saying so.

### Limits of the shipped sensor

- The kind job feeds the sensor a made-up export line shaped from Tetragon's published reference. It has not run against a live Tetragon. "Joined" is tested; "corroborated" is your Tetragon's word.
- A token in a Secret is readable by anyone who can `get secrets` in the sensor namespace. It authorises `kernel.*` audit writes only and lasts about an hour.
- The join keys on a pod's name. Only a principal who can create pods in the runs namespace can make a `wardyn-agent-` pod, which is why the chart refuses an empty `k8s.runsNamespace`.
- A different sensor still needs the recipe below.

## Bring your own sensor

The sensors below do not use the chart's DaemonSet. Their alerts join to a run by hand, and Wardyn does not read, check or store their events.

## The join

Your sensor reports a container or a pod. Follow it to a run and its creating principal in four steps.

1. **Container to pod.** Most sensors already add the pod name, namespace and pod labels. If yours reports only a container ID, map it to a pod (see below).
2. **Pod to run.** Every agent and proxy pod carries three labels ([`internal/runner/k8s/naming.go`](../../internal/runner/k8s/naming.go) `wardynLabels`, constants at the top of the file):
   - `wardyn.managed=true`
   - `wardyn.run-id=<run uuid>`
   - `wardyn.component=agent` (or `proxy`)

   The agent pod is the one that runs the task. The proxy pod only forwards traffic.
3. **Run to creating principal.** Call `GET /api/v1/runs/{id}`. The run JSON has `created_by`, the principal that created the run (the token `sub`), and `sandbox_ref`, the agent pod's name ([`internal/types/types.go`](../../internal/types/types.go) `AgentRun`).
4. **Optional: the audit trail.** `GET /api/v1/audit?run_id={id}` lists the run's audit rows. Each row has `actor`, `action` and `time` ([`internal/types/types.go`](../../internal/types/types.go) `AuditEvent`).

Keep `wardyn.run-id` with the event when your sensor captures it, before the pod is deleted. That is the reliable join.

A late alert may have only a pod name:

- The agent pod name is `wardyn-agent-<run UUID>` ([`internal/runner/k8s/naming.go`](../../internal/runner/k8s/naming.go) `agentPodName`, `runIDFromAgentPodName`).
- Check the name matches that exact format. Take the UUID, then `GET /api/v1/runs/{id}`.
- Do not search runs by `sandbox_ref`. Wardyn clears it when a run ends ([`internal/api/run_terminal_effects.go`](../../internal/api/run_terminal_effects.go)), so the match works only for a run that is still live.

## Person or service

`created_by` is a person only when a person created the run. A run launched by a service identity has `created_by` of the form `service:<name>` ([`internal/types/service_identity.go`](../../internal/types/service_identity.go) `ServicePrincipalPrefix`).

- For such a run the join gives you the service, not a human.
- The human is the service's sponsor. Read it separately: the service identity record has a `sponsor` field (`ServiceIdentity`, same file). Read it with `GET /api/v1/service-identities/{name}`, a security-tier route ([`internal/api/service_identities.go`](../../internal/api/service_identities.go)); a run reader token is not enough.
- The sponsor can change. The record holds the current sponsor, not necessarily the one at the time of the run ([`internal/api/helpers.go`](../../internal/api/helpers.go) `sponsorsRun`, `runSponsor`). Check the audit trail for service changes if the date matters.
- The label join alone establishes neither a person nor a sponsor.

## Who may read the run

The read needs a Wardyn bearer token ([API tokens](api-tokens.md)): `Authorization: Bearer wdn_…`.

- `GET /runs/{id}` answers for the run's owner, a sponsor of the run, or an admin ([`internal/api/helpers.go`](../../internal/api/helpers.go) `ownsRunOrSponsorOrAdmin`).
- Anyone else gets the same 404 a missing run gets.
- `GET /audit` for a member returns rows only for a run they own. Use an admin or security admin token for a cross-team lookup ([ownership and offboarding](ownership-and-offboarding.md)).
- For automation, give the lookup its own admin-issued token. Do not reuse a person's token.

```sh
RUN=$(kubectl get pod -n <runs-namespace> <pod> -o jsonpath='{.metadata.labels.wardyn\.run-id}')
curl -s -H "Authorization: Bearer $TOKEN" "$WARDYN/api/v1/runs/$RUN" \
  | jq '{id, created_by, sandbox_ref, agent, repo}'
```

## Example selectors

Find the pods of one run:

```sh
kubectl get pods -n <runs-namespace> -l wardyn.run-id=<run uuid>
```

Find every Wardyn agent pod:

```sh
kubectl get pods -A -l wardyn.managed=true,wardyn.component=agent
```

Use your sensor's own pod-label field to read `wardyn.run-id`. Two points from the sensors' own docs:

- Tetragon events carry the pod under `process.pod`, with `name`, `namespace` and `pod_labels` ([Tetragon gRPC API reference](https://tetragon.io/docs/reference/grpc-api/)).
- Falco reads one label with `k8s.pod.label[wardyn.run-id]` or `k8s.pod.label.wardyn.run-id` ([Falco supported fields](https://falco.org/docs/reference/rules/supported-fields/)). Its `container.id` is only the first 12 characters of the ID.

Scoping a sensor policy to Wardyn pods by label depends on your sensor and version. Follow its own docs.

## Container ID to pod

When an event has only a container ID, ask the API server. On containerd the ID is prefixed `containerd://` in pod status:

```sh
CID=<64 hex chars from the event>
kubectl get pods -A -o json | jq -r --arg cid "containerd://$CID" '
  .items[]
  | select([.status.containerStatuses[]?, .status.initContainerStatuses[]?, .status.ephemeralContainerStatuses[]?]
           | any(.containerID == $cid))
  | "\(.metadata.namespace)/\(.metadata.name) run=\(.metadata.labels["wardyn.run-id"])"'
```

An empty `run` means the label lookup could not establish a run. It does not prove the pod is not a Wardyn pod: an outside edit can remove the label while the agent pod keeps its fixed name. With the agent-sandbox backend on, Wardyn detects that state and ends the run ([`internal/runner/k8s/lifecycle.go`](../../internal/runner/k8s/lifecycle.go) `agentPodReplaced`).

- If the pod name has the exact `wardyn-agent-<run UUID>` form, use the pod name fallback in [The join](#the-join): take the UUID, then `GET /api/v1/runs/{id}` with a Wardyn token.
- Labels are lookup hints, not proof of origin. Anyone who can patch a pod can change them.

## What you run yourself

- The kernel sensor and its policy, on every node that can host runs.
- Pod metadata enrichment in the sensor, or the container ID lookup above.
- A security-tier token to read the service identity's sponsor, if you launch runs from services.
- Read access to pods (get, list) for that lookup.
- A Wardyn token for the run lookup.
- Retention for the sensor's events. Wardyn keeps none of them.

## DNS queries

The sensor sees where a packet went, not the name it asked for. Wardyn records the DNS queries a run asks as `run.dns.query` audit rows with the name, the type and what the sandbox was told, bounded as the last bullet says. On Kubernetes, Docker and VM runs, no query is looked up outside the run.

- **Kubernetes:** the agent pod's one nameserver is its proxy pod. The proxy answers every query `REFUSED` and records it as a row, within the bound in the last bullet, with `via` `proxy`. The agent's NetworkPolicy allows port 53, TCP and UDP, to its proxy pod only. The proxy binds 53 through the pod sysctl `net.ipv4.ip_unprivileged_port_start`, with no added capability.
- **VM runs:** the relay keeps answering from its own name table, because that answer is how a guest reaches an allowed host by name. Each row says `answered` (with the address the guest got), `refused` (with `cause` `name` or `full`), and `via` `vm_relay`. A base-image preparation has no run proxy, so its `prep_refused` queries are only in the relay's log.
- **Docker:** the agent's one nameserver is its run proxy, through a read-only `/etc/resolv.conf` the driver mounts from a per-run Docker volume. The proxy answers every query `REFUSED` and records it as a row, within the bound in the last bullet, with `via` `proxy`. Covered by CI tests on runc (every pull request) and gVisor (nightly); Kata and krun use the same mount but are untested. It needs Docker Engine 26 or later and Docker API 1.45 or later. On an older engine run with `WARDYN_ALLOW_HOST_REACHABLE_RUN_NETWORK`, or a client pinned to an older API with `DOCKER_API_VERSION`, lookups fail at Docker's resolver as before and write no row.
- Rows are bounded: the first of each name, type and outcome in a second is written, and the rest are counted on the next one (`repeat`). Rows are capped at 64 a second per proxy; excess rows, including folded repeats, are dropped and counted in the proxy's warning.

## Limits

- Pod labels disappear with the pod. Resolve the run at alert time, or copy it into the alert.
- Wardyn removes a run's pods during teardown. A cleanup failure can leave them until a retry removes them. The run row and audit rows stay.
- With your own sensor, Wardyn does not ingest its events and does not raise a bypass event when it is blind.
