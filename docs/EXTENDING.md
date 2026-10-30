# Extending Wardyn: layers and extension points

- This page lists the layers of the code, the interface each extension implements, and where a long-running workload plugs in.
- [Pluggable components](PLUGGABILITY.md) is the standard for a swappable seam, and [ARCHITECTURE.md](../ARCHITECTURE.md) is the design. This page is the map between them.
- A new model provider or credential class is not a seam. Each is a closed set, so adding one is a change in several packages, not a registration.

## Layers

| Layer | Code | Owns | Never |
|---|---|---|---|
| Control plane | `cmd/wardynd`, `internal/api`, `internal/store` | The REST API, the policy engine, the approval state machine, the token broker, audit ingest and the sweeps | Carries a sandbox's outbound traffic: the proxy does |
| Governance | `internal/authz`, [`internal/api/governance.go#governanceCeiling`](../internal/api/governance.go), [`internal/api/run_fold.go#Server.foldRunRequest`](../internal/api/run_fold.go) | Who may do what, and the ceiling every run is folded under | Lets a request widen its ceiling: each fold only narrows |
| Runners | `internal/runner/orchestrator`, `internal/runner/remote`, `internal/runnerwire`, `cmd/wardyn-runnerd` | Placing a run on a substrate, its lifecycle, and the session to an organisation or laptop runner | Schedules, resolves a ceiling, brokers a credential or writes audit: the organisation does |
| Substrates | `internal/runner/docker`, `internal/runner/k8s`, `internal/runner/localtmux`, `internal/vm`, `internal/macvm` | Isolating one run: create, exec, attach, stop | Decides policy |
| Proxy | `cmd/wardyn-proxy`, `internal/egress/proxy` | Default-deny egress, first-use approval, decision logs and proxy-side credential injection | Is shared between runs: one proxy sits beside each run |
| Broker and identity | `internal/broker`, `internal/identity`, `internal/secretstore` | Minting a credential for a run, run identity and the secret store | Hands the sandbox an `api_key` value: it stays at the proxy |
| Audit and recording | `internal/audit`, `internal/recording` | The append-only audit chain and session recordings | Erases a row: only a partition drop removes one |

## Extension points

| Extension | You implement | Register or wire | Held to | Kind |
|---|---|---|---|---|
| Confinement substrate | [`internal/runner/substrate/substrate.go#Substrate`](../internal/runner/substrate/substrate.go) | [`internal/runner/substrate/registry.go#Register`](../internal/runner/substrate/registry.go), from `init()` | [`test/conformance/conformance.go#Run`](../test/conformance/conformance.go) | Seam |
| Lifecycle over substrates | [`internal/runner/runner.go#Runner`](../internal/runner/runner.go), plus optional capabilities such as `SandboxEnder`, `ProxyReviver`, `Freezer` and `ImageChecker` | The orchestrator multiplexes substrates by confinement class | The same conformance suite | Seam |
| Registered runner | `wardyn-runnerd` speaking [`internal/runnerwire/payload.go#Caps`](../internal/runnerwire/payload.go) | The organisation side is [`internal/runner/remote/substrate.go#Substrate`](../internal/runner/remote/substrate.go) | The wire tests in `internal/runnerwire` | Seam |
| Identity provider | [`internal/identity/identity.go#Provider`](../internal/identity/identity.go) | [`internal/identity/registry.go`](../internal/identity/registry.go) | `internal/identity/identitytest` | Seam |
| Secret store and key | [`internal/secretstore/secretstore.go#Store`](../internal/secretstore/secretstore.go), `External`, and [`internal/secretstore/kek/kek.go#KEK`](../internal/secretstore/kek/kek.go) | [`internal/secretstore/registry.go`](../internal/secretstore/registry.go) | `internal/secretstore/secretstoretest` | Seam |
| Recording store | [`internal/recording/store.go#Store`](../internal/recording/store.go) | [`internal/recording/registry.go`](../internal/recording/registry.go) | `internal/recording/recordingtest` | Seam |
| Egress decision | [`internal/egress/evaluator.go#Evaluator`](../internal/egress/evaluator.go) | `Options.Evaluator` in [`internal/egress/proxy/proxy.go`](../internal/egress/proxy/proxy.go) | Package tests | Seam |
| Content scan and audit sink | [`internal/contentscan/contentscan.go#Detector`](../internal/contentscan/contentscan.go), [`internal/audit/sink.go#Sink`](../internal/audit/sink.go) | The scan engine in `internal/contentscan`; [`internal/audit/sinks/config.go#ParseSinks`](../internal/audit/sinks/config.go) | Package tests | Seam |
| Model provider | A new `ModelProviderKind` in [`internal/types/model_provider.go#ModelProviderKind`](../internal/types/model_provider.go) | A dispatch lane: [`internal/api/runs_dispatch_provider.go#providerKeyLaneFor`](../internal/api/runs_dispatch_provider.go) | Package tests | Closed set |
| Credential class | A new `GrantKind` in [`internal/types/types.go#GrantKind`](../internal/types/types.go) | A mint in [`internal/broker/broker.go#Broker.mintKind`](../internal/broker/broker.go) | Package tests | Closed set |

### Add a model provider kind

1. Add the kind to the closed set in [`internal/types/model_provider.go#ModelProviderKind`](../internal/types/model_provider.go).
2. Add its dispatch lane beside [`internal/api/runs_dispatch_provider.go#providerKeyLaneFor`](../internal/api/runs_dispatch_provider.go) and, for a cloud sign-in, [`internal/api/provider_azure.go#providerAzureLaneFor`](../internal/api/provider_azure.go).
3. Find every other switch on a kind with `grep -rl ModelProviderAzureFoundry --include='*.go' .`. It lists placement, sizing, harness choice, the credential inventory, provider access and the Azure Entra paths.
4. Give each switch a case and a test. A kind with no case is a silent fall-through, not a refusal.

### Add a credential class

1. Add the kind to [`internal/types/types.go#GrantKind`](../internal/types/types.go).
2. Mint it in [`internal/broker/broker.go#Broker.mintKind`](../internal/broker/broker.go), and say in [`internal/broker/revoke.go#revokeNote`](../internal/broker/revoke.go) what a run's kill does to it.
3. Name the hosts it reaches in [`internal/api/runs_dispatch_ceiling.go#grantHosts`](../internal/api/runs_dispatch_ceiling.go), and, for a run on a local runner, prove where its value comes from in [`internal/api/run_placement_eligibility.go#Server.localGrantOrigin`](../internal/api/run_placement_eligibility.go).
4. Grade its risk in [`internal/composer/risk.go#gradeGrant`](../internal/composer/risk.go), and deliver it in the proxy, `internal/egress/proxy`.
5. State where the credential is while the run works in [CREDENTIALS.md](CREDENTIALS.md). Do not claim a credential never enters the sandbox unless the mint is brokered.

## Where a long-running workload plugs in

None of the three adds a method to `Substrate`. A service and governed inbound access ride `Exec` and `ExecStream`, which every substrate already has. A workload resource rides the runner session.

| Need | What shipped | Code | Page |
|---|---|---|---|
| A command that stays running | `service` on a background run: a supervisor in the sandbox restarts it and writes a health state file | [`deploy/images/common/agent-run-lib.sh`](../deploy/images/common/agent-run-lib.sh), [`internal/api/run_service.go#serviceEnv`](../internal/api/run_service.go) | [Services](SERVICES.md) |
| Health | The server reads the state file over `ExecStream` and records `run.service.health` | [`internal/api/run_service_health.go#Server.sweepServiceHealth`](../internal/api/run_service_health.go) | [Services](SERVICES.md#health-is-the-sandboxs-claim) |
| Lifetime as policy | The lease sweep ends a run at its lease. A healthy service under a lifetime cap has its lease renewed within the cap | [`internal/api/run_lease.go#lifetimeEndDue`](../internal/api/run_lease.go), [`internal/api/run_service_lease.go#Server.renewServiceLease`](../internal/api/run_service_lease.go) | [Services](SERVICES.md#renewal-while-healthy) |
| Governed inbound | A `ui_apps` entry marked `inbound` is called through the app gateway by named callers, with a browser ticket or an inbound token | [`internal/api/uigateway_inbound.go#Server.admitInbound`](../internal/api/uigateway_inbound.go), [`internal/api/inbound_ticket.go#Server.handleAppTicket`](../internal/api/inbound_ticket.go) | [UI sandboxes](UI-SANDBOXES.md#inbound-let-named-callers-reach-a-port) |
| A declared run | `wardyn apply` stores a workload and starts its run. On a Kubernetes organisation runner a `WardynWorkload` resource does the same | [`internal/api/workloads.go#Server.handlePutWorkload`](../internal/api/workloads.go), [`internal/api/runner_workloads.go#Server.WorkloadFromRunner`](../internal/api/runner_workloads.go) | [Workloads](operations/workloads.md) |
| A runner watching a cluster | A `workload` event up and a `workload_status` call down, over the runner session | [`internal/runnerwire/rpc.go#WorkloadReport`](../internal/runnerwire/rpc.go), [`cmd/wardyn-runnerd/workloads_k8s.go#newWorkloadWatcher`](../cmd/wardyn-runnerd/workloads_k8s.go) | [Workloads](operations/workloads.md#kubernetes-the-wardynworkload-resource) |

- The supervisor restarts the command inside the sandbox, so a restart is not a second `Exec`. A Kubernetes sandbox allows one `Exec` per reference.
- A workload launches as a service identity under that identity's ceiling. A binding names an organisation pool only.

## Invariants an extension keeps

- The [security invariants](../ARCHITECTURE.md#security-invariants-every-contributor-and-subagent-must-preserve-these) hold for every layer above, and the [standard's invariants](PLUGGABILITY.md#1-the-standard) hold for every seam.
- Fail closed: `CreateSandbox` rolls back in full and returns an error on any failure.
- Add the audit row, its [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md) entry and the test in the same change as the behaviour.
