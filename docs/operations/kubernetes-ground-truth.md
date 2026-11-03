> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Ground truth on Kubernetes

Use your own kernel sensor, such as Tetragon or Falco, to see what a run did, and tie each alert to a Wardyn run and a person.

- On Kubernetes, Wardyn ships no kernel sensor and no kernel-level corroboration of its own. See [known gaps](kubernetes-known-gaps.md).
- Wardyn does not read, check or store your sensor's events.
- This page is a join recipe. It makes your sensor's alerts actionable by run and person. Wardyn does not verify those events.
- The Compose `groundtruth` profile is a different pipeline. It does not run on Kubernetes.

## The join

Your sensor reports a container or a pod. Follow it to a person in four steps.

1. **Container to pod.** Most sensors already add the pod name, namespace and pod labels. If yours reports only a container ID, map it to a pod (see below).
2. **Pod to run.** Every agent and proxy pod carries three labels ([`internal/runner/k8s/naming.go`](../../internal/runner/k8s/naming.go) `wardynLabels`, constants at the top of the file):
   - `wardyn.managed=true`
   - `wardyn.run-id=<run uuid>`
   - `wardyn.component=agent` (or `proxy`)

   The agent pod is the one that runs the task. The proxy pod only forwards traffic.
3. **Run to owner.** Call `GET /api/v1/runs/{id}`. The run JSON has `created_by`, the signed-in person's principal (the token `sub`), and `sandbox_ref`, the agent pod's name ([`internal/types/types.go`](../../internal/types/types.go) `AgentRun`).
4. **Optional: the audit trail.** `GET /api/v1/audit?run_id={id}` lists the run's audit rows. Each row has `actor`, `action` and `time` ([`internal/types/types.go`](../../internal/types/types.go) `AuditEvent`).

Two checks use the same pod name. `sandbox_ref` for a Kubernetes run is the agent pod name ([`internal/runner/k8s/sandbox.go`](../../internal/runner/k8s/sandbox.go) `agentPodName`). So you can also start from the pod name: list runs and match `sandbox_ref`.

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

For Tetragon, scope a `TracingPolicy` to Wardyn pods with a pod selector:

```yaml
spec:
  podSelector:
    matchLabels:
      wardyn.managed: "true"
```

Then read the label from each event. Tetragon puts the pod name, namespace and labels on `process.pod`. Match `wardyn.run-id` there. Check your Tetragon version's output; field names can differ. For Falco, enable its Kubernetes metadata enrichment and use the `k8s.pod.label.wardyn.run-id` field in rules or output.

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
- Read access to pods (get, list) for that lookup.
- A Wardyn token for the run lookup.
- Retention for the sensor's events. Wardyn keeps none of them.

## Limits

- Pod labels disappear with the pod. Resolve the run at alert time, or copy it into the alert.
- A run's pods are removed when the run ends. The run row and audit rows stay.
- Wardyn does not ingest your sensor's events and does not raise a bypass event when the sensor is blind.
- Nothing here puts a sensor DaemonSet or an in-cluster correlator in the chart. Those are not built.
