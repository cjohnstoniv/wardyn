> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Integrations

- An integration is a **connection** — secrets plus egress — to any named external system Wardyn talks to on a run's behalf, never an installer (what a run has installed is what its image carries).

## The shape

`types.Integration` ([`internal/types/workspace.go`](../../internal/types/workspace.go)) is ONE base shape extended by `kind`:

| Field | What it holds |
| --- | --- |
| `secrets[]` | Each a role, a store REF (a name, never a value), and its **delivery** (see below the table) |
| `egress[]` | Where the system lives; why a host is reachable for a granted run instead of being hand-listed in every workspace |
| `config{}` | Non-secret knobs, key-validated per closed kind (`github_app` ⇒ `app_id`/`installation_id`/`host`); an unknown key on a closed kind 400s by name |

- `proxy_header` is the header + format the proxy presents on the wire, so the sandbox never holds the credential (the proxy injects one credential header per host).
  - A secret on a closed kind may declare NO delivery, meaning that kind's own hand-written transport carries it (`github_app`'s brokered halves, `git_host`'s clone credentials).
  - At most one `proxy_header` secret per row, and every such secret targets the row's whole egress list.
- The type also models `resident_file`/`resident_env` for the closed kinds whose own transport needs them.
- An operator may not DECLARE one.
- There's no generic lane that materializes a named secret into a sandbox path or env var, so a write naming one is refused.

**Model access is not an integration.** The four AI kinds — `anthropic_api_key`, `anthropic_subscription`, `bedrock`, `openai_api_key` — left the closed set.

- A write naming one is refused as an unsupported kind (`validateIntegrationWrite`, [`internal/api/integrations_write.go`](../../internal/api/integrations_write.go)) — model access is set up under Settings → Model providers — and `default_for` is no longer a field at all.
- The upgrade converted every stored AI row into a model provider and deleted it (migration `0100_model_provider_conversion`; see the [CHANGELOG](../../CHANGELOG.md)).
- No AI row is derived from the operator's own model credentials any more either.

`kind` is the ONE field that says what this connects to, and the closed set is the ONLY writable set:

| Kind | Note |
| --- | --- |
| `github_app`, `git_host` | Each has behavior in code (`capabilitiesFor`); a new one is a code change, and a write naming anything else 400s with the accepted list |
| `anthropic_api_key`, `anthropic_subscription`, `bedrock`, `openai_api_key` | Not integrations: see "Model access is not an integration" above |
| Generic kinds (`"jira"`, `"artifactory"`, …) | No longer writable, though a row stored under an earlier release still loads, still sits in `SiteConfig`, and is still injected by [`internal/api/integrations_run.go`](../../internal/api/integrations_run.go) |
| `azure_openai` | Gone as a kind |
| `azure_foundry` | A model-provider kind, not an integration kind. **Switched off in 0.8.6:** a provider write of this kind answers `400` "the azure_foundry kind is not available in this release". See [Azure Foundry](#azure-foundry-azure_foundry) |

## Where it's configured

- **Settings** (the Admin view's sidebar) is the one surface for these — a Model provider card, a radio group over concrete lanes; the standalone `/integrations` page is deleted.
- The three git credential lanes (GitHub App, PAT, SSH key) render INSIDE the provider row they apply to, on the Workspace Providers screen (`/admin/providers`). Settings keeps a card that summarizes the provider policy and links there.
- A row's `lanes` list says which of the three it permits at all (a path a row does not permit drops its wiring, with a warning on the 201).
- Rows are also DERIVED from what already exists (stored secret names, site config, setup status), so an operator who never opens Settings keeps identical run behavior.
- Host proxy and Egress redirection are deliberately NOT here — that's network topology, configured under **Network** on the same `SiteConfig` document.

### Wardyn does not dial the provider

- There is no "Test" action.
- Settings states what is STORED and says so plainly: "Wardyn stores this, it doesn't dial the provider to check it."
- A real run is the real test, and it fails loudly with an audit trail if the credential is wrong.

### Nothing is ambient

Configuring an integration grants nothing by itself.

- A run gets one only when a workspace's requirements name it by key — `integration:<id>`, alongside `secret:`/`egress:`/`write:` — and that workspace is what the run attaches (`applyIntegrationRequirement`, [`internal/api/integrations_run.go`](../../internal/api/integrations_run.go)).
- Once granted, its hosts join the run's egress allowlist unconditionally, even under `allow_all_egress` (the proxy's credential injector doesn't honor allow-all, so the exact-host entry has to be there regardless).
- A header-delivering integration authors one `api_key` grant per host through the ordinary proxy-side injection path.
  - **Except** on a host that serves a model (`modelServingHosts`, [`internal/api/llmcred.go`](../../internal/api/llmcred.go)): a model vendor's API, or any model provider row's own host.
  - That last case covers a custom endpoint, a route-through gateway, or a Bedrock row's regional hosts.
- On a model-serving host the credential is skipped and audited (`run.requirement.skip`, reason `model_host`) instead: a model credential comes only from the run's model provider.
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

A model run is credentialed by the model provider it chose (`enforceRunModelProvider`, [`internal/api/run_model_provider.go`](../../internal/api/run_model_provider.go)) from its owner's own key, token or sign-in, or by nothing.

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

### The provider kinds

- **API key** (`anthropic_api_key`, `openai_api_key`): each person stores their own key for the provider. The proxy adds it to the outgoing request, as `x-api-key` for `anthropic_api_key` and as `Authorization: Bearer` for `openai_api_key`; **never resident**.
- **Claude subscription** (`anthropic_subscription`): each person signs in to the provider from Getting started.
  - A short-lived sign-in sandbox runs `claude setup-token`. The captured token is stored age-encrypted in that person's own namespace and injected proxy-side as `Authorization: Bearer`.
  - The sandbox holds only an inert sentinel (`docker exec … env | grep -i key` is empty).
  - It needs a Claude Code image pinned in `WARDYN_AGENT_IMAGES` (from a clone, `make agent-images-core` builds one). The one-line install's `agent-base` is not one.

> [!WARNING]
> - **Security note:** a `claude setup-token` is **long-lived (~1 year)** and does **not** auto-rotate — it sits age-encrypted at rest in the secret store, masked from all streams, host-pinned to `api.anthropic.com`, and never enters a run's sandbox. It exists only in the short-lived sign-in sandbox that captures it, which stops when idle.
> - Protect `deploy/compose/.env` and the postgres volume, and revoke the token in the Anthropic console if a host is compromised.

- **AWS Bedrock** is a provider of kind `bedrock_sso` (each person signs in with AWS; the region, model and base URL live on the provider record) or `bedrock_bearer`.
  - With `bedrock_bearer`, each person stores a Bedrock bearer token, proxy-injected as `Authorization: Bearer` into `bedrock-runtime.*`, **never resident**.
  - The model is a cross-region *inference-profile* id, or the profile's full ARN — `arn:aws:bedrock:<region>:<acct>:inference-profile/<id>` or `…:application-inference-profile/<id>` — not a bare model id.
  - By default a `bedrock_sso` run holds that person's short-lived role credentials in its own sandbox (see [threat model §5.1a](../../threatmodel/THREAT-MODEL.md#51a-llm-egress-content-inspection--the-honest-claims-contract)).
  - With `WARDYN_AWS_SIGV4_AT_PROXY` on and every AWS host the run needs able to be re-signed, the proxy signs instead: the sandbox gets a placeholder `credential_process` profile and neither session nor role credentials are resident. Turning the switch on alone does not make every host eligible; a run with an ineligible host keeps the default ([Credentials](../CREDENTIALS.md#a-cloud-credential-made-inside)).

## Azure Foundry (`azure_foundry`)

> [!WARNING]
> **Switched off in 0.8.6:** a provider write of this kind answers `400` "the azure_foundry kind is not available in this release". This section describes the design a later release turns on. Do not set up Entra consent for it yet.

- An `azure_foundry` model provider sends a run's model calls to one Azure Foundry endpoint.
- It uses the launching person's own Entra sign-in.
- No key or token is ever in the sandbox.
- The harness holds the inert `wardyn-proxy-injected` sentinel.
- The proxy terminates TLS for the endpoint with the run's own certificate authority and attaches the person's token.
- The control plane redeems that token for the provider's one audience (`resolveAzureFoundryInjection`, [`internal/api/injection_azure.go`](../../internal/api/injection_azure.go)).
- Route `anthropic` serves the Messages harness (`claude-code`); route `openai_v1` serves `codex-cli`.
- Until the kind ships, nothing else about it is documented.

## Who may use what

Which hosts, secrets, workspaces, images, agents, model providers, git providers, policies and components a member may name is a capability, not an integration setting: [Capabilities](capabilities.md#capabilities-what-one-member-or-one-group-may-do) has the kind table.
