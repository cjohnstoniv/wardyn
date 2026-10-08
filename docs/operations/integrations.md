> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Integrations

- An integration is a **connection** — secrets plus egress — to any named external system Wardyn talks to on a run's behalf, never an installer (what a run has installed is what its image carries).

## The shape

`types.Integration` ([`internal/types/workspace.go`](../../internal/types/workspace.go)) is ONE base shape extended by `kind`:

| Field | What it holds |
| --- | --- |
| `secrets[]` | Each a role, a store REF (a name, never a value), and its **delivery**. |
|  | `proxy_header` is the header + format the proxy presents on the wire, so the sandbox never holds the credential (the proxy injects one credential header per host). |
|  | A secret on a closed kind may declare NO delivery, meaning that kind's own hand-written transport carries it (`github_app`'s brokered halves, `git_host`'s clone credentials). |
|  | At most one `proxy_header` secret per row, and every such secret targets the row's whole egress list |
| `egress[]` | Where the system lives; why a host is reachable for a granted run instead of being hand-listed in every workspace |
| `config{}` | Non-secret knobs, key-validated per closed kind (`github_app` ⇒ `app_id`/`installation_id`/`host`); an unknown key on a closed kind 400s by name |

- The type also models `resident_file`/`resident_env` for the closed kinds whose own transport needs them.
- An operator may not DECLARE one.
- There's no generic lane that materializes a named secret into a sandbox path or env var, so a write naming one is refused.

**Model access is not an integration (0.8).** The four AI kinds — `anthropic_api_key`, `anthropic_subscription`, `bedrock`, `openai_api_key` — left the closed set.

- A write naming one is refused as an unsupported kind (`validateIntegrationWrite`, [`internal/api/integrations_write.go`](../../internal/api/integrations_write.go)) — model access is set up under Settings → Model providers — and `default_for` is no longer a field at all.
- The upgrade converted every stored AI row into a model provider and deleted it (migration `0100_model_provider_conversion`; see the [CHANGELOG](../../CHANGELOG.md)).
- No AI row is derived from the operator's own model credentials any more either.

`kind` is the ONE field that says what this connects to, and the closed set is the ONLY writable set:

| Kind | Note |
| --- | --- |
| `github_app`, `git_host` | Each has behavior in code (`capabilitiesFor`); a new one is a code change, and a write naming anything else 400s with the accepted list |
| `anthropic_api_key`, `anthropic_subscription`, `bedrock`, `openai_api_key` | Left the closed set in 0.8 — see "Model access is not an integration" above |
| Generic kinds (`"jira"`, `"artifactory"`, …) | A 0.5 carry-over — no longer writable, though a row stored under an earlier release still loads, still sits in `SiteConfig`, and is still injected by [`internal/api/integrations_run.go`](../../internal/api/integrations_run.go) |
| `azure_openai` | Gone as a kind (also a 0.5 carry-over) |
| `azure_foundry` | A model-provider kind, not an integration kind: each person's own Entra sign-in for one Azure Foundry resource. **Switched off in 0.8.6:** a provider write of this kind answers `400` "the azure_foundry kind is not available in this release". See "Azure Foundry" below |

## Where it's configured

- **Settings** (the Admin view's sidebar) is the one surface for these — a Model provider card, a radio group over concrete lanes; the standalone `/integrations` page is deleted.
- **The Git host card retired in 0.7.2.**
  - Its three git credential lanes (GitHub App, PAT, SSH key) now render INSIDE the provider row they apply to, on the Workspace Providers screen (`/admin/providers`).
  - Settings keeps a card in its place that summarizes the provider policy and links there.
- The lanes are the same radio group over the same concrete lanes. What changed: "which git hosts this org clones from" and "how a run authenticates to them" stopped being two surfaces that could disagree.
- A row's `lanes` list now says which of the three it permits at all (a lane a row does not permit drops its wiring, with a warning on the 201).
- Rows are also DERIVED from what already exists (stored secret names, site config, setup status), so an operator who never opens Settings keeps identical run behavior.
- Host proxy and Egress redirection are deliberately NOT here — that's network topology, configured under **Network** on the same `SiteConfig` document.

### Wardyn does not dial the provider

- There is no "Test" action; `POST /api/v1/integrations/{id}/test` was removed in 0.5.
- Settings states what is STORED and says so plainly: "Wardyn stores this, it doesn't dial the provider to check it."
- A real run is the real test, and it fails loudly with an audit trail if the credential is wrong.

### Nothing is ambient

Configuring an integration grants nothing by itself.

- A run gets one only when a workspace's requirements name it by key — `integration:<id>`, alongside `secret:`/`egress:`/`write:` — and that workspace is what the run attaches (`applyIntegrationRequirement`, [`internal/api/integrations_run.go`](../../internal/api/integrations_run.go)).
- Once granted, its hosts join the run's egress allowlist unconditionally, even under `allow_all_egress` (the proxy's credential injector doesn't honor allow-all, so the exact-host entry has to be there regardless).
- A header-delivering integration authors one `api_key` grant per host through the ordinary proxy-side injection path.
  - **Except** on a host that serves a model (`modelServingHosts`, [`internal/api/llmcred.go`](../../internal/api/llmcred.go)): a model vendor's API, or any model provider row's own host.
  - That last case covers a custom endpoint, a route-through gateway, or a Bedrock row's regional hosts.
- On a model-serving host the credential is skipped and audited (`run.requirement.skip`, reason `model_host`) instead: a model credential comes only from the run's model provider (0.8, #547).
  - An integration write that would present its credential on such a host is refused.
  - A `secret:<name>` requirement is skipped the same way, always — its grant was the agent's model host — so it grants nothing (`skipRequiredSecret`, [`internal/api/runs_create_requirements.go`](../../internal/api/runs_create_requirements.go)).
- An operator with fifty integrations configured and a workspace that names none of them gets a run whose spec is byte-identical to having none.

That fold degrades silently by design.

- A workspace may state an `integration:<id>` requirement before the integration exists, and a missing one must never brick a run.
- So the create-run preflight checklist carries an explicit row instead (`setupWorkspaceIntegrationItems`, [`internal/api/compose_setup.go`](../../internal/api/compose_setup.go)): "no integration named `<id>` is configured, add it under Integrations", or "turned off", or "names no hosts".
- That's stated as config state (amber, not the destructive red reserved for a missing credential) with the requiring workspace named.
- Optional requirements are never rowed there.

### A header credential needs a bare exact host

- An integration's `egress` entries may carry a leading `*.` wildcard or a `:port` qualifier UNLESS one of its secrets delivers `proxy_header`.
- Write-time validation (`validateIntegrationHosts`, [`internal/api/integrations_write.go`](../../internal/api/integrations_write.go)) then requires every host to be a bare exact hostname, because proxy-side injection resolves through `Policy.AllowedExactHost`, which consults the exact-host set only.

| Shape | What happens |
| --- | --- |
| A wildcard host with `proxy_header` | Opens the path and silently never presents the credential — rejected at write time |
| A port-qualified host with `proxy_header` | Makes the injector refuse to build a rule, a hard proxy startup failure — rejected at write time |
| Either shape on an integration with no credential header | Allowed — a data store on `db.corp.internal:5432`, egress only, is exactly the shape this is for |

## Model access comes from a model provider

Since 0.8.2 a model run is credentialed by the model provider it chose (`enforceRunModelProvider`, [`internal/api/run_model_provider.go`](../../internal/api/run_model_provider.go)) from its owner's own key, token or sign-in, or by nothing.

- No integration folds, no managed or host-mounted subscription, no operator key and no boot-time Bedrock configuration serves it.
- Dispatch drops every model credential its policy carries that the provider did not author (`dropLegacyModelInjections`, `resolveProviderLane`, [`internal/api/runs_dispatch_provider.go`](../../internal/api/runs_dispatch_provider.go)).
- Nor may an `env_secret` grant set a variable a model credential rides in, or a provider arm sets (`modelEnvNames`, [`internal/api/provider_env.go`](../../internal/api/provider_env.go), table below). Such a grant is refused with a 422 at create and Review, naming the grant and the variable, and dispatch refuses the run again if one arrives another way.
- Every other `env_secret` grant is placed as before.

| Group | Refused variables |
| --- | --- |
| Core | `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `OPENAI_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, `AWS_BEARER_TOKEN_BEDROCK`, `AWS_ACCESS_KEY_ID`, `ANTHROPIC_CUSTOM_HEADERS` |
| Foundry | `ANTHROPIC_FOUNDRY_API_KEY`, `ANTHROPIC_FOUNDRY_AUTH_TOKEN`, `ANTHROPIC_FOUNDRY_BASE_URL`, `ANTHROPIC_FOUNDRY_RESOURCE`, `CLAUDE_CODE_USE_FOUNDRY` |
| Vertex | `CLAUDE_CODE_USE_VERTEX`, `ANTHROPIC_VERTEX_BASE_URL`, `ANTHROPIC_VERTEX_PROJECT_ID` |
| Anthropic-on-AWS | `ANTHROPIC_AWS_API_KEY`, `ANTHROPIC_AWS_BASE_URL`, `ANTHROPIC_PROFILE` |
| Every arm | Its own base-URL, model, region and config variables |

**No integration chooses a run's model credential.** A run naming one (`integration_id`) is refused with a `422` at create and Review (`decodeAndValidateCreateRun`, [`internal/api/runs_create_validate.go`](../../internal/api/runs_create_validate.go)).

- A workspace pins a model provider (`LLMCred.ProviderRef`), never an integration.
- The 0.8.2 upgrade converts a 0.7.x or 0.8.0 integration pin, an integration's `DefaultFor: agent_runs` mark and the agent roster's model credential fields into model providers and drops the AI integration rows (`0100_model_provider_conversion`, see [CHANGELOG](../../CHANGELOG.md)).
- Record, verify and build sessions no longer mint an `api_key` grant from an operator secret either (`recordSessionModelAccess`, [`internal/api/record_model_provider.go`](../../internal/api/record_model_provider.go)).

## Azure Foundry (`azure_foundry`)

> [!WARNING]
> **Switched off in 0.8.6:** a provider write of this kind answers `400` "the azure_foundry kind is not available in this release". This section describes the design a later release turns on. Do not set up Entra consent for it yet.

- An `azure_foundry` model provider sends a run's model calls to one Azure Foundry endpoint.
- It uses the launching person's own Entra sign-in.
- No key or token is ever in the sandbox.
- The harness holds the inert `wardyn-proxy-injected` sentinel.
- The proxy terminates TLS for the endpoint with the run's own certificate authority and attaches the person's token.
- The control plane redeems that token for the provider's one audience (`resolveAzureFoundryInjection`, [`internal/api/injection_azure.go`](../../internal/api/injection_azure.go)).

- **One row, one route, one audience.**
  - Route `anthropic` serves the Messages harness (`claude-code`) and signs in for the Foundry audience.
  - Route `openai_v1` serves `codex-cli` and signs in for the Cognitive Services audience.
  - Each harness entry names its deployment (`model`).
  - The Messages harness may name a second deployment for its small-model alias (`fast_model`).
- **Admin prerequisite.** The sign-in is the console's own Entra login application, so the install needs Entra console login. Grant that application the delegated permission for the row's audience, with admin consent. Without it every sign-in fails with `consent_required`.
- **What dispatch writes into the sandbox.** The env names the endpoint and the deployments. The Messages harness gets `CLAUDE_CODE_USE_FOUNDRY`, the `ANTHROPIC_FOUNDRY_*` variables and the `ANTHROPIC_DEFAULT_*_MODEL` variables, and never `ANTHROPIC_API_KEY`. Codex gets `WARDYN_CODEX_BASE_URL`, `WARDYN_CODEX_MODEL` and `CODEX_API_KEY`.
- **What dispatch writes into the proxy.**
  - The endpoint goes on the exact egress allowlist and is TLS-terminated.
  - A route gate holds the token to that route's inference calls on the pinned deployments.
  - The host is also marked as a model host, so the content scanner reads its dialect.
  - `/anthropic/v1/messages` is scanned.
  - `/openai/v1/responses` is reported as uninspected, which `require_inspectable_llm` refuses.
  - The endpoint is never a gateway upstream, so the brokered `/wardyn/llm/*` routes never reach it.
- **Two private-endpoint knobs.**
  - The proxy's private-address guard refuses an endpoint that resolves to a private address.
  - `internal_hosts` lifts the guard for that host.
  - `upstream_proxy_no_proxy` is a separate setting.
  - It matters only when a corporate proxy is configured, and it does not lift the guard.
  - A provider write warns when the endpoint resolves privately with no `internal_hosts` entry.
- **The requested model can still change.** A repo config or the interactive model picker can ask for a model other than the pinned deployment. The route gate refuses that request with `azure_route_refused`. The run's own `model` setting is only a default.
- **When the sign-in ends mid-run.**
  - A revoked refresh token holds the run's model request on a sign-in request (HTTP 423, `credential_reauth`).
  - So does a Conditional Access policy that asks for the person.
  - The hold ends when the person signs in to that provider again.
  - A failure at boot fails the run with the remedy as its hint.
- **Conditional Access.**
  - Wardyn renews the sign-in from the control plane's address.
  - A policy that needs the person's device or a fresh challenge cannot be satisfied there.
  - The refusal names the class Entra reported: multi-factor authentication, device compliance, an approved client, or a blocked sign-in.
  - No policy class is promised to work, and continuous access evaluation is not supported.

## What the New Run rail states

- The right-hand "What this run can do" panel is read, not asserted.
- Two of its rows consult the server, and both are silent rather than wrong when the server hasn't answered.

**Credentials** names the model provider THIS run would use and where its credential lands, with no click. The picker itself is on the Run panel, beside the agent; the rail only states the choice:

| Answer | Meaning |
| --- | --- |
| `proxy` | A key or token, injected by the proxy at launch and never written into the sandbox |
| `sandbox` | A Bedrock AWS sign-in, which signs inside the run for its lifetime |

- Where several providers serve the agent and none is the default, the Run panel asks which, and Launch waits for the answer.
- Where no provider serves the agent it reads "Resolved at launch." and an invitation to press **Check again**. Preflight runs on its own as the run is edited; the button dry-runs the exact body Launch would send, and Launch is blocked by a refusal for the current body graded under 60s ago.
- With no model provider connected at all the rail shows the no-provider warning instead.
- A run that makes no model call (a shell command) shows no Credentials row at all.

**Recording** reads `/healthz`.

- A stock Helm install leaves `persistence.enabled=false`, which renders `WARDYN_RECORDING_STORE=off` — the actual switch.
- So no run on that server ever produces a cast; the rail then states that instead of promising "every keystroke and every outbound connection."
- While that read is still in flight, or if it failed, the rail states neither.
- Recording is off only where `WARDYN_RECORDING_STORE=off`.
- Turn it on with `persistence.enabled=true` (the `fs` store on the PVC) or `env.WARDYN_RECORDING_STORE=pg` (no PVC needed).
- `WARDYN_RECORDING_DIR` only moves the `fs` store's path — it doesn't turn recording on or off.

## What an admin can put a fence around

| Kind | Narrows or widens |
| --- | --- |
| Hosts a member may add or approve | Narrows |
| Secrets a member may reference | Narrows |
| Workspaces a member may launch against | Narrows |
| Base images a member may name | **Widens** — a grant is what makes an image nameable at all |
| Agents a member may run | Narrows |
| Model providers a member may name | Narrows |
| Git providers their work may come from | Narrows |
| Stored policies a member may select | Narrows |
| Whether they may add an **SSH key** or mint an **API token** at all (`feature`) | About the member rather than a run |

- ([Capabilities: what one member, or one group, may do](../OPERATIONS.md#capabilities-what-one-member-or-one-group-may-do) has the kind table.)
- Eight of the nine *narrow* — until you enforce one, members keep exactly the powers they had, and a deny bites even before you do.
- Base images are the one that *widens*.

A permission always bounds what the **member** chose and never what you pre-authorized.

- Model providers are the one exception.
- The answer to "can I fence a model provider" is the `model_provider` kind.
- It bounds the provider a run names, the one its workspace pins, and the agent's default alike — because every model credential is the person's own.

The assigned governance profile's egress still applies on top: its denied hosts are re-asserted at dispatch and withhold every credential lane that would reach one.

- Governance profiles also carry the limits that are not choices at all: `max_concurrent_runs`, and the two launch modes a profile can refuse outright.
- Like every profile field, they bind only the people a profile is assigned to.

## Round-trip

Profiles and their assignments live in Postgres, so `make reset` / `make reset-all` take them with the volume.

1. Export: `wardyn governance get > governance.json` (`GET /api/v1/governance`) before a reset.
2. Restore: `wardyn governance set governance.json` after — the same get-then-set shape `wardyn site-config get|set` and `wardyn drive get|set` already take (0.8, #1108).

- `set` upserts every named profile **by name** (its unique handle) and every assignment by its own natural key (subject_type, subject).
- A profile the file does not mention is left alone.
- One present server-side but absent from the file is only removed with `--prune`.
- `--prune` also removes an omitted assignment.
- It removes assignments before profiles.
- So a still-referenced profile never trips the delete-while-assigned refusal above.
- `wardyn governance get > f && wardyn governance set f` is a no-op: unchanged rows are skipped rather than re-written, so a repeat `set` issues no writes and adds no audit rows.
