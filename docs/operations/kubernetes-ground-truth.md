> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Ground truth on Kubernetes

Use your own kernel sensor, such as Tetragon or Falco, to see what a run did, and tie each alert to a Wardyn run and its creating principal.

- On Kubernetes, Wardyn ships no kernel sensor and no kernel-level corroboration of its own. See [known gaps](kubernetes-known-gaps.md).
- Wardyn does not read, check or store your sensor's events.
- This page is a join recipe. It makes your sensor's alerts actionable by run and creating principal. Wardyn does not verify those events.
- The Compose `groundtruth` profile is a different pipeline. It does not run on Kubernetes.

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

An empty run means the pod is not a Wardyn pod.

## What you run yourself

- The kernel sensor and its policy, on every node that can host runs.
- Pod metadata enrichment in the sensor, or the container ID lookup above.
- A security-tier token to read the service identity's sponsor, if you launch runs from services.
- Read access to pods (get, list) for that lookup.
- A Wardyn token for the run lookup.
- Retention for the sensor's events. Wardyn keeps none of them.

## Limits

- Pod labels disappear with the pod. Resolve the run at alert time, or copy it into the alert.
- Wardyn removes a run's pods during teardown. A cleanup failure can leave them until a retry removes them. The run row and audit rows stay.
- Wardyn does not ingest your sensor's events and does not raise a bypass event when the sensor is blind.
- Nothing here puts a sensor DaemonSet or an in-cluster correlator in the chart. Those are not built.
