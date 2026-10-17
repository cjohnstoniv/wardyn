# 0.9.3 Kubernetes-backed runner: the shipped design

Status: **Draft.** Trimmed from the design's rev 15 to the decisions 0.9.3 ships, with the questions the build settled. Operators read [Organisation runners in a Kubernetes cluster](../../operations/k8s-runners.md); this note is why it is shaped that way.

## Decisions

| # | Decision | Rejected alternative |
|---|---|---|
| D1 | One `wardyn-runnerd` binary and image; `serve --substrate k8s` wires `internal/runner/k8s.Driver` into the existing runner runtime | A second runner daemon: two code paths for one contract |
| D2 | Organisation ownership is a column (`runners.owner_kind = org`, empty `owner`), not a reserved principal | A sentinel owner string: one mistyped equality check hands a person an org runner |
| D3 | Trust class is a stored, admin-written column with a closed set and a hard coupling to ownership | Deriving trust from asserted posture: the runner would set its own class |
| D4 | Delivery policy gains per-trust-class overrides that fall back to the global row; `via_org` is never trust-gated | A separate trust policy document |
| D5 | Two namespaces per runner: runnerd's own, and a dedicated runs namespace bound by RoleBinding | One namespace: a sandbox pod could name runnerd's ServiceAccount |
| D6 | Proxy self-lapse is terminal; a lapsed proxy's pod is never restarted | A per-run proxy Service with replacement and auto-revive: deferred |
| D7 | A separate chart, `deploy/helm/wardyn-runnerd`, one architecture per release | A values block in the control-plane chart |
| D8 | An org-runner run keeps wire value `placement = local` | A third placement value: 34 sites plus a CHECK and TypeScript mirrors |
| D9 | Decisions stream from the proxy to a runnerd sink; runnerd is the only spool writer | A shared RWX volume: not available everywhere, and `flock` on NFS-class storage is undefined |

## Identity, enrolment and claim

| Step | Rule |
|---|---|
| Mint | `POST /api/v1/runners/org-tokens {name}` on the security-operator tier. Single-use `wdr_`, 72 hours. The operator route refuses any `owner_kind`. The `name` is audit-only |
| Four-eyes mint | A human mint is direct. An admin-token mint takes the break-glass arm: applied, a `governance.change.bypass` row, and the token stored `break_glass=true`. An admin credential is never a second human |
| Deliver | A Secret the chart references (`registration.existingSecret`), never templated |
| Register | Init container `wardyn-runnerd register --substrate k8s --if-unregistered`. The key is generated on the volume. `owner_kind` is copied from the token at redeem |
| Claim | `POST /api/v1/runners/{id}/claim-org`: security operator plus a human session. It verifies `owner_kind` and never sets it. Each claim route refuses the other kind. Under four-eyes the claimer differs from a human minter |
| Revoke | The existing route. `endQueuedRunsPinnedTo` stops filtering by owner for org rows |
| Re-key | None in 0.9.3: new token, fresh volume, revoke |

- Every `runners.owner` equality on the runner-session path accepts an empty owner only when `owner_kind = org` and the row is claimed and unrevoked.
- The registration token links to the runner (`runners.registration_token_id`, added with the organisation-runner claim) so the claim can compare claimer and minter by principal or case-folded email.
- The trust class is `org_cluster` from the insert, because the coupling CHECK forces it. The claim stamps the writer and the time.


## Trust class

- The set is `personal` < `managed_device` < `org_cluster`. `org_cluster` holds if and only if `owner_kind = org` (a database CHECK).
- Only `PUT /api/v1/runners/{id}/trust {trust_class, reason}` changes it afterwards: security-operator tier, the governance-change path, audited `runner.trust.write {before, after}`.
- No self-upgrade: the registration request is strict-decoded, and capabilities and posture never feed the column. Reflection and PG tests pin it.
- Delivery policy: `by_trust.<class>.classes.<credential class>` falls back to the global row for every class, then to `refuse`. A plan is judged per trust class among the eligible candidates, highest first. Resident delivery and revive re-judge against the bound runner's current trust and refuse `runner_trust_changed` on any difference.
- The CC3 floor and cluster-only trusted-output runs are unchanged.


## Placement in the cluster

| Piece | Shape |
|---|---|
| Runner namespace | Deployment (1 replica, `Recreate`), ServiceAccount, RWO volume, the relay Service (ports `relay` and `sink`), NetworkPolicy |
| Runs namespace | Pre-existing and dedicated. The server Role plus `pods: patch`; no `services`, no `watch` |
| Cluster scope | `runtimeclasses: get` and `nodes: list` only |
| Agent pod | Egress only to its proxy; automount and service links off |
| Proxy pod | The proxy's own policy plus the runnerd peer on the relay and sink ports; `RestartPolicy: Never` |
| Pod lifetime | `activeDeadlineSeconds = min(run cap + grace, --max-pod-lifetime)` on both pods; default `168h` |
| Architecture | `--arch` merges `kubernetes.io/arch` into the node selector and the canary's; a conflicting operator value refuses boot |

- The relay listens on the pod IP at `relay.targetPort`; the Service exposes `relay.servicePort`. A run whose control-plane URL port differs from `relay.servicePort` is refused by name.
- Corporate CA: the bundle reaches the proxy configuration, the sandbox and the org session's HTTP client (`SystemCertPool` plus the bundle), on both substrates.
- Recording: the per-run `record` choice is stamped on the agent pod as `wardyn.record`; `--recording-store` is the runner's ceiling. Uploads ride the relay and are stored `runner_asserted`.
- `hard_deny` goes into every proxy configuration: the `svc` suffix and the cluster domain, the relay ClusterIP, runnerd's pod IP, the apiserver's addresses (the chart reads them from the `kubernetes` Service's EndpointSlice at `helm install` and `helm upgrade`; a render without cluster access, such as `helm template`, adds none) and `apiServer.peers`, and `egress.hardDenyCIDRs`. It is checked before the `internal_hosts` lift, for agent egress only, and a refusal is audited under rule source `builtin:private-ip` (there is no `hard_deny` source). Operator-authored dial paths (the brokered model gateway, trusted URLs) do not pass through it, and an upstream proxy inside the cluster would void the protection for names it resolves itself, so runnerd refuses one at boot when its address is a literal IP inside the hard deny (a host name is not checked).

## Startup checks

runnerd refuses to boot, each time with a named error, when:

1. `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL` or `WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY` is set, or the canary verdict is not `enforced`.
2. A SelfSubjectAccessReview for any needed verb fails; the error names the verbs.
3. The runs namespace holds `wardyn.managed` objects with another `runner-id`, or with none.
4. The relay Service does not resolve to exactly one IP.
5. The relay probe pod's connection arrives from another address: `relay_source_rewritten`.
6. A schedulable node of the sandbox architecture runs a kubelet older than 1.31: `kubelet_below_floor`.
7. `apiServer.peers` holds an entry without an `ipBlock`, or `WARDYN_K8S_NAMESPACE` is set.

The 1.31 floor is conservative: deleted pods reach a terminal phase before the kubelet deletes them since 1.27, and pod disruption conditions are GA in 1.31. A run on a node below the floor stays pending instead of ending on a guess.

## Decision sink and self-lapse

| Piece | Rule |
|---|---|
| Handshake | The proxy opens TLS to runnerd's sink port, pinned by the SPKI SHA-256 in its configuration, and sends `hello {run_id, instance, key}`. runnerd keeps only `sha256(key)` |
| Grants | Write-ahead: at most 5,000 records and a byte credit (16 reservations at the largest record) beyond `producer.seq`, never into the last slot or the last 4 KiB (the loss reserve) |
| Commit, then ack | One manifest write commits the record and the producer sequence; only then is it acked. Every sequence ends in exactly one durable disposition: `appended`, `refused` or `lost` |
| Drain | Before a proxy pod is deleted, runnerd sends `drain` and waits at most 5 seconds. Containment proceeds regardless |
| Loss | Settled only once the producer is proven gone. Exact after a drain; an upper bound of at most 5,000 records after an abnormal end. Always a durable `builtin:spool-lost` row, acked by the org before seal |
| Self-lapse | `closed` from start, `open` after the first renew, `lapsed` terminal. Deadline: the last successful renew's send time plus `RunTokenTTL` (60 minutes) on `CLOCK_BOOTTIME` |
| Cadence | Every 10 minutes (plus or minus 1) after a success; every 30 seconds (plus or minus 5) after a failure; nothing gives up before the deadline |
| At the lapse | `lapsed {reason}`, drain at most 5 seconds, exit non-zero. The pod stays `Failed`, which survives a runnerd restart |

- The proxy's deadline is the containment bound. The org's and runnerd's deadlines are cleanup bounds; an org clock change can move them, never the proxy's.
- A run survives a runnerd outage if and only if runnerd is READY again and the proxy's next renew succeeds before its deadline. The budget includes runnerd's own boot.
- runnerd records a durable `ProxyExited` obligation only on a validated `lapsed` frame or an observed `Failed`, `Succeeded` or NotFound proxy pod. A sink session ending is not evidence.
- On k8s runners `identity.renew` rows rise from 2 to 6 per run per hour.
- Without a decision sink, runner mode refuses every sandbox create, and a sink that fails to start refuses boot.

## Containment and completion evidence

A deleted API object does not prove a process stopped. NotFound is never evidence, on any path.

| Piece | Rule |
|---|---|
| Relay fence | Before the first delete, the run's relay connections close and new ones are refused; the fence is durable and replayed before the runner is Online. An IP still fenced for another run refuses with `relay_ip_fenced` |
| Stop-proof finalizer | Both runner-mode pods carry `wardyn.dev/stop-proof`, set in the create request. runnerd removes it only on completion evidence, after persisting the witness |
| Proxy delete | `Orphan` propagation and a 1 second grace, never 0 |
| Terminal status | The recorded pod UID has a terminal phase, every container is `terminated`, no `DeletionByPodGC` condition, and a tier binds it to the execution |
| Tier 1 | A terminated container ID equals its anchored baseline, with a closing anchor and a post-status list. Refused on runs with unknown association |
| Tier 2 | A causal node UID whose Node still exists with matching `machineID`, `systemUUID` and host IP |
| Taint | `node.kubernetes.io/out-of-service`, honoured on the causal UID when the Node still shows the recorded machine and no name reuse is recorded |
| Record | A "node powered off" or "no prior holder" record, bound to the run, its pods, the node name and the machine. It needs two people after a name reuse, without a causal association, and when four-eyes is on |
| Pending states | `contained_agent_only` and `contained_pending_delete`; reported as `ContainmentPending`, never plain containment; NetworkPolicies held until evidence |

- Name reuse is detected from the before, after, anchor and closing lists. A reuse latches a generation; only the two-person record releases such a run.
- The unbound-pod arm (a pod deleted before it was bound) ships enabled. The kind conformance run's bind and delete race, serial and overlapping with both winning orders, passed on the 1.31 floor and on 1.35.
- Operational threat model: this guards against honest failure, not against a holder of a node's kubelet credentials. Two kubelets under one Node name are unsupported.

## Failure behaviour

| Event | Behaviour |
|---|---|
| runnerd down, inside the budget | Pods keep running; decisions buffer up to the grant; the run continues on reconnect |
| runnerd down past a proxy's deadline | That proxy exits; the agent has no network until runnerd returns, then the run is lost `outage` |
| Proxy exits | Terminal; runnerd contains and settles the loss |
| Sandbox node lost or partitioned | Fence first; pods stay Terminating; the run waits for the node, the taint or the record |
| Revoked, online | `runner_revoked`; durable latch; every proxy stopped, every sandbox torn down, Secrets deleted |
| Revoked, offline, never returns | A label-scoped cleanup runbook in a fixed order, with the finalizer in the loop |
| Lost relay | First-use approvals denied; decisions buffer |
| New proxy pod's IP still fenced for an earlier run | The runner refuses the create; the run fails at start with `relay_ip_fenced` |
| Cluster API errors | Create rolls back; `Status` errors are transient; a failed caps replay blocks READY |

## Settled during the build

| Question | Resolution |
|---|---|
| No decision sink at create | The create is refused; a failing sink refuses boot |
| Unbound arm switch | Enabled in code with no switch; the kind conformance run's race gate decides whether it stays |
| Tier 2 offered | Only for a container with no anchored baseline or no terminated ID |
| Node record transport | `PendingAction.NodeRecord` on a `stop_proxy` action |
| Node list fails before a create | The create is refused |
| `apiServer.peers` without `ipBlock` | Refuses startup; the hard deny cannot hold it |
| Register on k8s | Measures nothing; the totals come from flags |
| `WARDYN_K8S_NAMESPACE` | Refused; `--runs-namespace` only |
| Org session trust | The CA bundle applies to both substrates |
| RBAC order | With `rbac.create=false`, the RBAC is applied before the image and before any upgrade that adds a verb |

## Residuals

| # | Residual |
|---|---|
| R1 | Organisation-runner evidence stays `runner_asserted`; the CC3 floor refuses resident values on any runner |
| R2 | `org_cluster` means trusting that cluster's administrators with per-run and keystore Secrets, etcd backups and the per-run MITM key |
| R3 | With runnerd down, agents run without network until runnerd returns or the pod deadline arrives |
| R4 | If the runner namespace's CNI does not enforce policy, the relay and sink ports are reachable in-cluster |
| R5 | Volume loss is identity loss |
| R6 | Decision loss is exact after a drain and at most 5,000 records after an abnormal end |
| R7 | A trust downgrade does not pull values already delivered |
| R8 | One architecture per runner |
| R9 | `ipBlock` semantics and enforcement for a pod marked for deletion depend on the CNI; the kind probe records kind's behaviour |
| R10 | A proxy that lapses or exits ends its run, with no revive |
| R11 | A pod that cannot be confirmed stopped may serve cached-policy egress until its proxy deadline; the run waits in termination unknown |
| R12 | Clusters that rewrite pod-to-ClusterIP source addresses are unsupported |
| R13 | In-cluster services are unreachable from org-runner runs |
| R14 | The ServiceAccount lists every Node and patches pods in its runs namespace |
| R15 | Two kubelets under one Node name are unsupported |
| R16 | Reusing a Node name for another machine while a run's pods remain is unsupported; runnerd records a reuse it observes and releases only by the two-person record |

- R14: besides removing its finalizer, `patch` can change a running pod's image, which is the power the Role already holds through pod create, exec and ephemeral containers. A cluster that will not grant the cluster-scoped read cannot host organisation runners.
- R16: runnerd cannot see a reuse that none of its lists samples before release. Such a holder can start the pod after release; the exposure is that namespace's ambient egress, with no renewable identity.

## Deferred

- A per-run Service, `ReplaceProxy` and auto-revive. Reopening it needs a split proxy-config Secret, the Service verb in the server chart Role in the same release (chart before image), a `Config` flag, a `CheckFit` count and a renewable identity after a proxy restart.
- Persisting deadlines on apiserver time, and a pod-lifetime state file for the renewed token: both rejected.
