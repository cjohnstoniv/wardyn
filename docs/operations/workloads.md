> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Workloads: a run Wardyn keeps declared

- A workload is a named, declared run that Wardyn keeps running for its owner, a person or a [service identity](api-tokens.md#service-identities).
- You write it as a file and apply it with `wardyn apply`, or as a `WardynWorkload` resource that a Kubernetes organisation runner reports.
- A workload grants nothing. Every launch is the ordinary run-create path under the owner, so your walls, your organisation's ceiling and the pool's limits apply unchanged.

## What a workload is

| Part | What it holds |
|---|---|
| Name | Unique for its owner: a lowercase DNS label that starts with a letter, at most 63 characters |
| Owner | The caller of an apply, or `service:<name>` for a resource a bound namespace reports |
| Source | `api` for the CLI and API, `k8s` for a custom resource. One name has one source, or the apply is `409` `workload_source_conflict` |
| Intent | What to run: exactly the `intent` of a `kind: Run` document ([Run documents](../sdk.md#run-documents)) |
| Generation | Counts spec changes. `current_generation` is the one the current run was launched from |
| Digest | The hex SHA-256 over the intent, `run_in` and `restart_key`. An apply with the same digest changes nothing. The security tier reading another owner's workload does not get it |
| Run | The current run. `state` is its state, with its service health when it has a [service](../SERVICES.md) block |

- An owner holds at most 100 workloads (`422` `workload_cap_reached`).
- The store must be Postgres. Any other backend answers `501` `workloads_unavailable`.
- A workload is not a schedule or a queue. It has one current run, and it starts a run only when its spec is ahead of its run.

## The workload file

A file holds one or more YAML or JSON documents, separated by `---`. A field Wardyn does not know is refused, and so is a name used twice in one file.

| Field | Meaning |
|---|---|
| `api_version` | `wardyn/v1` |
| `kind` | `Workload` |
| `name` | The workload's name |
| `description` | At most 200 characters. It is not part of the spec: changing it starts no run |
| `run_in` | The unit, by id or name, the run is launched in |
| `restart_key` | At most 64 characters, opaque. Changing it starts a fresh run of the same intent |
| `intent` | The run to keep, required. The whole spec is at most 256 KiB |

```yaml
api_version: wardyn/v1
kind: Workload
name: web
description: the web tier
intent:
  experience: background
  workload: {kind: command, command: make serve}
  no_repositories_or_drives: true
```

- The server normalises the intent through the same door as `POST /templates/import`, so a YAML and a JSON copy of one spec store the same bytes.
- The run is titled with the workload's name and carries its description.
- `wardyn apply` also reads a `wardyn.dev/v1alpha1` `WardynWorkload`: the fields above sit under `spec`, and the name is `metadata.name`. The namespace is ignored, and the workload is yours with source `api`.

## The CLI

| Command | What it does |
|---|---|
| `wardyn apply -f FILE` | Stores each workload the file names, and starts a run when it is new or changed |
| `wardyn diff -f FILE` | Prints a unified diff of the stored workload against the file. Writes nothing |
| `wardyn delete -f FILE` | Ends each workload's run and deletes the workload |
| `wardyn workload list` | Lists `NAME`, `OWNER`, `SOURCE`, `STATE`, `GENERATION` (current/latest), `RUN` and `REFUSED` (the last refusal's reason) |
| `wardyn workload get NAME` | Prints the workload, its document and status, as JSON |
| `wardyn workload restart NAME` | Starts a fresh run of the stored spec, then ends the old one |
| `wardyn workload delete NAME` | The same as `wardyn delete -f`, by name |
| `wardyn workload binding create\|list\|delete` | Binds a namespace to a service identity. [Security tier](#bind-a-namespace) |

- `-f -` reads standard input. `FILE` is required.
- `workload list|get|restart|delete` take `--owner` for an administrator, or a service identity's sponsor, who acts on another owner's workloads. `list` takes `--limit`, `--offset` and `--json`.
- Authentication is unchanged: `WARDYN_URL`, `WARDYN_TOKEN` or your stored sign-in. A pipeline uses a service identity's token, which reaches only that identity's own workloads.

### apply

| Flag | Meaning |
|---|---|
| `--dry-run` | Writes nothing. Prints the diff and the setup checklist the run would get. Excludes `--wait` |
| `--wait` | Follows each launched run: until it is running for a service, until it ends for any other workload |
| `--timeout` | Gives up waiting after this long, `30m` by default. The run keeps going |
| `--json` | Prints the results as a JSON array, with progress on stderr |

- Each workload prints one line: `workload/web created run <id>`, `workload/web updated run <id>`, `workload/web unchanged`, or `workload/web refused <reason>: <sentence>`.
- The server's warnings for a launched run print on stderr as `  warning: …`.

| Exit | When |
|---|---|
| `0` | Every workload applied, with or without a new run |
| `1` | A workload was refused (its spec is stored), or the file, a flag or the spec could not be used |
| `2` to `5` | The request failed as for any `wardyn` command: auth, any other `4xx`, `5xx`, network. [Exit codes](../CI.md#exit-codes) |
| `124` | `--wait` timed out |
| `--wait` outcome | The run's own exit, as `wardyn run --wait`. A service that ends before it is running exits `1` if FAILED, else `2` |

`wardyn diff` exits `0` when every workload equals the stored one, `1` when any differs or is not stored yet, and `2` or more on an error. A file that does not parse and a spec the server refuses exit `2`.

## What an apply does

| You apply | Outcome | Run |
|---|---|---|
| A new name | `created` (`201`) | A run starts |
| The same intent, `run_in` and `restart_key` | `unchanged` | Nothing starts. A changed `description` is stored; no `workload.apply` audit row is written |
| A changed intent, `run_in` or `restart_key` | `updated` | The new run starts, then the old run is killed with `run.kill` reason `workload_replaced`. The audit row is `workload.replace` |
| An unchanged spec whose last launch was refused | `unchanged` | The launch is tried again, because the spec is still ahead of its run |
| An unchanged spec whose run has ended | `unchanged` | Nothing. An ended run is restarted with `wardyn workload restart`, never relaunched by an apply |

- The new run is created before the old one is ended. A workload is never without a run because of a change.
- A refused launch is a `200`, not an error. The spec is stored at its new generation, and the old run keeps running.
- The answer carries `launch.state` `refused`, with the create path's own `reason` and sentence. The ceiling, a pool's limits and the unit walls all refuse here as they refuse `POST /runs`.
- The workload records `last_refusal` (`generation`, `reason`, `message`, `at`) until a launch succeeds. The audit row is `workload.launch_refused`.
- Two applies that race leave one run: the launch that loses is ended with reason `workload_replaced`.
- `workload restart` is recorded as `workload.restart`. When it is not the owner's own request it launches only for a service identity whose sponsor's session is live; a person's workload then starts at their next apply.
- Deleting a workload removes the row, then ends its run with reason `workload_deleted`.
- `GET /workloads/{name}/history[?owner=]` returns the workload's `workload.*` audit rows, newest first: `time`, `action` (`apply`, `launch`, `replace`, `launch_refused`, `restart` or `delete`), `generation`, `run_id`, `ended_run_id` and `reason`. The same callers read it as `GET /workloads/{name}`; a service identity's token does not. A name applied again after a delete shares the old history. The console's drawer lists it.

## Kubernetes: the WardynWorkload resource

A Kubernetes organisation runner can watch namespaces for `WardynWorkload` resources, so a GitOps tool keeps the workloads. The runner reports what it sees. Wardyn decides what it means and writes the answer to the resource's status.

- It needs an [organisation runner on Kubernetes](k8s-runners.md). A Docker or remote runner has no resource path; `wardyn apply` works on every substrate.
- The resource is only an authorisation key and a status carrier. Its run pods go to the pool's runs namespace on that runner, not to the resource's namespace.
- A resource's workload is owned by the bound service identity, as `service:<name>`, with source `k8s`.

### Bind a namespace

A binding says: the resources of namespace N, on the runners of pool P, launch as service identity S. It is standing authority, so it is made by a signed-in person with the security role, never with an API token or the admin token.

1. Make the service identity, with a sponsor ([Service identities](api-tokens.md#service-identities)).
2. Set `workloads.namespaces` in the runner chart to the namespaces to watch. The chart adds a Role and RoleBinding per namespace: `get`, `list` and `watch` on `wardynworkloads`, and `update` and `patch` on `wardynworkloads/status`. No Secret, nothing cluster-wide.
3. Apply the CRD, [`wardyn.dev_wardynworkloads.yaml`](../../deploy/helm/wardyn-runnerd/crds/wardyn.dev_wardynworkloads.yaml). Helm installs it once from `crds/` and never upgrades it, so apply the file again by hand on a chart upgrade.
4. Bind the namespace:

```sh
wardyn workload binding create --pool <pool> --namespace team-a --service web-bot
```

5. Commit the resource to `team-a`.

- With `rbac.create=false`, apply the per-namespace Roles before the runner image starts. The runner refuses to boot without them.
- `--pool` takes a pool's id, or an organisation pool's name. A namespace is bound once per pool (`409` `workload_binding_conflict`).
- Bind only pools whose runners are all in the one cluster the namespace belongs to.
- A runner serves a pool in one runs namespace, `runs_namespace`, fixed when the runner is added to the pool ([Runner pools](runner-pools.md)). That is where the pods go; the binding's namespace is only where the resource lives.
- The organisation finds a binding through the reporting runner's own pools. If a runner is in two pools that bind one namespace, the older binding wins.
- A resource may name a `runner_pool_id` in its intent, but only the bound pool's. The binding chooses the pool.

The binding stops being authority when any of these happens. The spec is still recorded, nothing launches, and a running run keeps running.

| Cause | Result |
|---|---|
| The service identity is suspended | Phase `BindingInactive`, reason `workload_binding_inactive` |
| The identity's sponsor changed since the binding was made | The same |
| The sponsor's sessions were cut | The same |
| The identity is retired | Its bindings are deleted |

To bind the namespace again, delete the binding and create it again with the identity's current sponsor. `wardyn workload binding delete ID` ends the namespace's launching; the runs it started continue.

### What the runner reports

- The runner reports every resource when a namespace's informer has synced, when its session to the organisation comes up, and every 10 minutes.
- Between those it reports a resource that is added, or whose `metadata.generation` changed, and one that is deleted. A status write changes no generation, so it reports nothing.
- Each full report ends with the names the namespace holds. The organisation deletes the workloads of that namespace the list omits, with audit `via` `k8s_sync`. This catches a delete missed while the runner was down.
- Deleting a resource deletes its workload and ends its run. There is no finalizer on the resource.
- An admin or security admin may delete a `k8s` workload through the API. The resource writes it back at the next full report unless it is gone too. Anyone else gets `409` `workload_managed_by_k8s`.

### The status Wardyn writes

The organisation sends a status for each resource whose status changed, in a sweep every 30 seconds, and again for all of a runner's resources when it becomes READY. It also sends one at once after a launch and for a resource it cannot record. Wardyn never reads a resource's own `status`.

The runner writes `status` and nothing else, only to the resource named by the answer: its `metadata.uid` and its `metadata.generation` must equal the answer's. An answer for a resource deleted and recreated under the name, or for a spec since changed, is dropped. The write carries the resource version it checked, and a conflict is read again up to three times.

| `status` field | Value |
|---|---|
| `observedGeneration` | The `metadata.generation` the answer is for |
| `phase` | One of the phases below |
| `runID` | The current run, when there is one |
| `reason`, `message` | A Wardyn reason token and its sentence, on a refusal. Up to 512 bytes of message |
| `conditions` | One `Ready` condition: `True` only in phase `Running`, `reason` the phase, and `lastTransitionTime` moves only when `Ready` flips |

`kubectl get wardynworkloads` shows `Phase`, `Run` and `Age`.

| Phase | Means |
|---|---|
| `Pending` | The current spec has no run yet, or its run is starting |
| `Queued` | The run is waiting in the queue |
| `Running` | The run is live and its service, if any, is not unhealthy |
| `Unhealthy` | The run's service health is unhealthy |
| `Completed` | The run completed |
| `Ended` | The run ended another way: failed, killed or stopped |
| `Refused` | The launch of this spec was refused. The previous run keeps running |
| `Invalid` | The spec is not a workload Wardyn accepts |
| `NotBound` | No binding names this namespace on this runner's pools. `reason` is empty |
| `BindingInactive` | The namespace's binding no longer carries authority |

| `reason` | Phase | Cause |
|---|---|---|
| `workload_invalid` | `Invalid` | The spec has an unknown field, or the intent fails the run-document door. The message names the first problem |
| `workload_pool_mismatch` | `Refused` | The intent names a `runner_pool_id` other than the pool the namespace is bound on. Nothing is recorded |
| `workload_binding_inactive` | `BindingInactive` | See [Bind a namespace](#bind-a-namespace) |
| `workload_source_conflict` | `Refused` | The service identity already holds a workload of that name from the API or another namespace or pool |
| `workload_cap_reached` | `Refused` | The identity holds 100 workloads |
| The create path's reason | `Refused` | The launch was refused, for example by the ceiling. [sdk.md](../sdk.md#reason-branch-on-this-never-on-errors-prose) lists the reasons |

### Argo CD health

Argo CD reads a resource's health from Lua in its `argocd-cm` ConfigMap. This check maps the phase to Argo's health and treats a status for an older spec as still progressing.

```yaml
data:
  resource.customizations.health.wardyn.dev_WardynWorkload: |
    local hs = {}
    local status = obj.status
    if status == nil or status.phase == nil then
      hs.status = "Progressing"
      hs.message = "Waiting for Wardyn to report this workload"
      return hs
    end
    if status.observedGeneration ~= obj.metadata.generation then
      hs.status = "Progressing"
      hs.message = "Waiting for Wardyn to report this spec"
      return hs
    end
    local phase = status.phase
    if phase == "Running" or phase == "Completed" then
      hs.status = "Healthy"
    elseif phase == "Pending" or phase == "Queued" then
      hs.status = "Progressing"
    else
      hs.status = "Degraded"
    end
    local message = phase
    if status.reason ~= nil and status.reason ~= "" then
      message = message .. ": " .. status.reason
    end
    if status.message ~= nil and status.message ~= "" then
      message = message .. ": " .. status.message
    end
    hs.message = message
    return hs
```

| Phase | Argo health |
|---|---|
| `Running`, `Completed` | Healthy |
| `Pending`, `Queued` | Progressing |
| `Unhealthy`, `Ended`, `Refused`, `Invalid`, `NotBound`, `BindingInactive` | Degraded |
| No status yet, or a status for an older generation | Progressing |

Do not use the `Ready` condition for health: a completed workload is `Ready=False`.

## Known limits

> [!WARNING]
> A resource the runner cannot send gets no status. The runner logs `workload not reported` at warn level, with the namespace, name, operation and error, and sends nothing to the organisation. The resource shows no error.

- That covers a spec over 256 KiB and a name that does not start with a letter. Read the runner's log, not the resource.
- A resource that grows past the cap after it was reported keeps its last recorded spec running. Its status stays as it was, so the Argo check above reads it as Progressing.
- A namespace holding more than 1000 resources sends no full-list sync. The runner logs `workload sync not sent`, and a resource deleted while the runner was down is not pruned.
- The schema leaves `intent` open on purpose, so `kubectl apply` accepts a spec that Wardyn then marks `Invalid`. Read the status after an apply.
- A status can lag a change by up to 30 seconds. A report made while the runner's session is down is dropped, and the next full report, at most 10 minutes later, carries it.
- A change to a binding takes effect at the next report of each resource, not at once.

## Audit

[AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md) has the rows: `workload.apply`, `workload.launch`, `workload.launch_refused`, `workload.replace`, `workload.restart`, `workload.delete`, `workload_binding.create` and `workload_binding.delete`. A row from the resource path carries `via` `k8s` or `k8s_sync`, and the runner, pool, namespace and resource uid.

## Where it lives

| Part | Code |
|---|---|
| The file and its limits | [`pkg/client/workloads.go`](../../pkg/client/workloads.go) |
| Apply, restart, delete | [`internal/api/workloads.go`](../../internal/api/workloads.go) |
| Bindings | [`internal/api/workload_bindings.go`](../../internal/api/workload_bindings.go) |
| Resources, status and the sweep | [`internal/api/runner_workloads.go`](../../internal/api/runner_workloads.go) |
| The runner's watch and status write | [`cmd/wardyn-runnerd/workloads_k8s.go`](../../cmd/wardyn-runnerd/workloads_k8s.go) |
| The wire, phases and bounds | [`internal/runnerwire/rpc.go`](../../internal/runnerwire/rpc.go) |
| The CLI | [`cmd/wardyn/apply.go`](../../cmd/wardyn/apply.go), [`cmd/wardyn/workload.go`](../../cmd/wardyn/workload.go) |
