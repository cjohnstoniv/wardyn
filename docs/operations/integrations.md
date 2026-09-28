> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Integrations

An integration is a **connection** — secrets plus egress — to any named external
system Wardyn talks to on a run's behalf, never an installer (what a run has
installed is what its image carries). `types.Integration`
(`internal/types/workspace.go`) is ONE base shape extended by `kind`:

- `secrets[]` — each a role, a store REF (a name, never a value), and its
  **delivery**: `proxy_header` (the header + format the proxy presents on the
  wire, so the sandbox never holds the credential). A secret on a closed kind may
  declare NO delivery, meaning that kind's own hand-written transport carries it
  (`github_app`'s brokered halves, `git_host`'s clone credentials). Those lanes are why the type also models `resident_file`/`resident_env` —
  but an operator may not DECLARE one: there is no generic lane materializing a
  named secret into a sandbox path or env var, so a write naming one is refused.
  At most one `proxy_header` secret per row (the proxy injects one credential
  header per host), and every such secret targets the row's whole egress list.
- `egress[]` — where the system lives; why a host is reachable for a granted run
  instead of being hand-listed in every workspace.
- `config{}` — non-secret knobs, key-validated per closed kind (`github_app` ⇒
  `app_id`/`installation_id`/`host`); an unknown key on a closed kind 400s by
  name.

`kind` is the ONE field that says what this connects to, and the closed set is the
ONLY writable set: `github_app`, `git_host`. Each has behavior in code
(`capabilitiesFor`), so a new one is a code change, and a write naming anything
else 400s with the accepted list.

**Model access is not an integration (0.8).** The four AI kinds —
`anthropic_api_key`, `anthropic_subscription`, `bedrock`, `openai_api_key` — left
the closed set. A write naming one is refused as an unsupported kind
(`validateIntegrationWrite`, `internal/api/integrations_write.go`) — model access
is set up under Settings → Model providers — and
`default_for` is no longer a field at all. The upgrade converted every stored AI
row into a model provider and deleted it (migration
`0099_model_provider_conversion`; see the CHANGELOG), and no AI row is derived from
the operator's own model credentials any more either. Two carry-overs from 0.5: generic kinds — an
open slug (`"jira"`, `"artifactory"`, …) validated for shape only — are no longer
writable, though a row stored under an earlier release still loads, still sits in
`SiteConfig`, and is still injected by `internal/api/integrations_run.go`; and
`azure_openai` is gone as a kind.

**Settings** (the Admin view's sidebar) is the one surface for these — a Model provider card,
a radio group over concrete lanes; the standalone `/integrations` page is deleted.
**The Git host card retired in 0.7.2.** Its three git
credential lanes (GitHub App, PAT, SSH key) now render INSIDE the provider row
they apply to, on the Workspace Providers screen (`/admin/providers`); Settings keeps a
card in its place that summarizes the provider policy and links there. The lanes
are the same radio group over the same concrete lanes — what changed is that
"which git hosts this org clones from" and "how a run authenticates to them"
stopped being two surfaces that could disagree, and a row's `lanes` list now says
which of the three it permits at all (a lane a row does not permit drops its
wiring, with a warning on the 201). Rows are also DERIVED from
what already exists (stored secret names, site config, setup status), so an
operator who never opens Settings keeps identical run behavior. Host proxy and
Egress redirection are deliberately NOT here: that is network topology, configured
under **Network** (below) on the same `SiteConfig` document.

### Wardyn does not dial the provider

There is no "Test" action; `POST /api/v1/integrations/{id}/test` was removed in
0.5. Settings states what is STORED and says so plainly — "Wardyn stores this, it
doesn't dial the provider to check it". A real run is the real test, and it fails
loudly with an audit trail if the credential is wrong.

### Nothing is ambient

Configuring an integration grants nothing by itself. A run gets one only when a
workspace's requirements name it by key — `integration:<id>`, alongside
`secret:`/`egress:`/`write:` — and that workspace is what the run attaches
(`applyIntegrationRequirement`, `internal/api/integrations_run.go`). Once granted,
its hosts join the run's egress allowlist unconditionally, even under
`allow_all_egress` (the proxy's credential injector does not honor allow-all, so
the exact-host entry has to be there regardless), and a header-delivering
integration authors one `api_key` grant per host through the ordinary proxy-side
injection path — except on a host that serves a model (a model vendor's API, a
configured gateway, the boot Bedrock hosts, or any model provider row's own
host: a custom endpoint, a route-through gateway, a Bedrock row's regional
hosts — `modelServingHosts`, `internal/api/llmcred.go`), where the credential is
skipped and audited (`run.requirement.skip`, reason `model_host`): a model
credential comes only from the run's model provider (0.8, #547). An integration
write that would present its credential on such a host is refused. A `secret:<name>` requirement is
skipped the same way, always — its grant was the agent's model host — so it
grants nothing (`skipRequiredSecret`, `internal/api/runs_create_requirements.go`).
An operator with fifty integrations configured and a workspace that names none of
them gets a run whose spec is byte-identical to having none.

That fold degrades silently by design — a workspace may state an
`integration:<id>` requirement before the integration exists, and a missing one
must never brick a run — so the create-run preflight checklist carries an explicit
row instead (`setupWorkspaceIntegrationItems`, `internal/api/compose_setup.go`):
"no integration named `<id>` is configured, add it under Integrations", or "turned
off", or "names no hosts", stated as config state (amber, not the destructive red
reserved for a missing credential) with the requiring workspace named. Optional
requirements are never rowed there.

### A header credential needs a bare exact host

An integration's `egress` entries may carry a leading `*.` wildcard or a `:port`
qualifier UNLESS one of its secrets delivers `proxy_header`. Write-time validation
(`validateIntegrationHosts`, `internal/api/integrations_write.go`) then requires
every host to be a bare exact hostname, because proxy-side injection resolves
through `Policy.AllowedExactHost`, which consults the exact-host set only. A
wildcard would open the path and silently never present the credential; a
port-qualified host makes the injector refuse to build a rule, a hard proxy
startup failure. Both are rejected at write time, by name. Neither restriction
applies to an integration that delivers no credential header (a data store on
`db.corp.internal:5432`, egress only, is exactly the shape this is for).

### Model access comes from a model provider

Since 0.8 a model run is credentialed by the model provider it chose
(`enforceRunModelProvider`, `internal/api/run_model_provider.go`) from its
owner's own key, token or sign-in, or by nothing: no integration folds, no
managed or host-mounted subscription, no operator key and no boot-time Bedrock
configuration serves it, and dispatch drops every model credential its policy
carries that the provider did not author (`dropLegacyModelInjections`,
`resolveProviderLane`, `internal/api/runs_dispatch_provider.go`). Nor may an
`env_secret` grant: one that would set a variable a model credential rides in
or a provider arm sets (`modelEnvNames`, `internal/api/provider_env.go`:
`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `OPENAI_API_KEY`,
`CLAUDE_CODE_OAUTH_TOKEN`, `AWS_BEARER_TOKEN_BEDROCK`, `AWS_ACCESS_KEY_ID`,
`ANTHROPIC_CUSTOM_HEADERS`, the Foundry, Vertex and Anthropic-on-AWS variables
(`ANTHROPIC_FOUNDRY_API_KEY`, `ANTHROPIC_FOUNDRY_AUTH_TOKEN`,
`ANTHROPIC_FOUNDRY_BASE_URL`, `ANTHROPIC_FOUNDRY_RESOURCE`,
`CLAUDE_CODE_USE_FOUNDRY`, `CLAUDE_CODE_USE_VERTEX`, `ANTHROPIC_VERTEX_BASE_URL`,
`ANTHROPIC_VERTEX_PROJECT_ID`, `ANTHROPIC_AWS_API_KEY`, `ANTHROPIC_AWS_BASE_URL`,
`ANTHROPIC_PROFILE`), and each arm's base-URL, model, region and config
variables) is refused with a 422 at create and Review, naming the grant and the
variable, and dispatch refuses the run again if one arrives another way. Every
other `env_secret` grant is placed as before.

**No integration chooses a run's model credential.** A run naming one
(`integration_id`) is refused with a `422` at create and Review
(`decodeAndValidateCreateRun`, `internal/api/runs_create_validate.go`). A
workspace pins a model provider (`LLMCred.ProviderRef`), never an integration;
the upgrade to 0.8 converts a 0.7 integration pin, an integration's
`DefaultFor: agent_runs` mark and the agent roster's model credential fields
into model providers and drops the AI integration rows
(`0099_model_provider_conversion`, see [CHANGELOG](../../CHANGELOG.md)). Record,
verify and build sessions no longer mint an `api_key` grant from an operator
secret either (`recordSessionModelAccess`,
`internal/api/record_model_provider.go`).

### What the New Run rail states

The right-hand "What this run can do" panel is read, not asserted. Two of its
rows consult the server, and both are silent rather than wrong when the server
has not answered.

- **Credentials** names the model provider THIS run would use and where its
  credential lands, with no click: `proxy` (a key or token, injected by the
  proxy at launch and never written into the sandbox) or `sandbox` (a Bedrock
  AWS sign-in, which signs inside the run for its lifetime, chipped
  **Per-person AWS sign-in** — ownership, not sign-in status). Where several
  providers serve the agent and none is the default, the rail asks which. Where
  no provider serves the agent it reads "Resolved at launch." and an invitation
  to press **Preflight**, which dry-runs the exact body Launch would send. With
  no model provider connected at all the rail shows the no-provider warning
  instead. A run that makes no model call (a shell command) shows no
  Credentials row at all.
- **Recording** reads `/healthz`. A stock Helm install leaves
  `persistence.enabled=false`, which renders `WARDYN_RECORDING_STORE=off` — the
  actual switch — so no run on that server ever produces a cast; the rail then
  states that instead of promising "every keystroke and every outbound
  connection". While that read is still in flight — or if it failed — the rail
  states neither. Recording is off only where `WARDYN_RECORDING_STORE=off`.
  Turn it on with `persistence.enabled=true` (the `fs` store on the PVC) or
  `env.WARDYN_RECORDING_STORE=pg` (no PVC needed). `WARDYN_RECORDING_DIR` only
  moves the `fs` store's path — it does not turn recording on or off.

### What an admin can put a fence around

Eight things a member chooses on their own run each carry a permission on the
Permissions page: the **hosts** they may add or approve, the **secrets** they may
reference, the **workspaces** they may launch against, the **base images** they
may name, the **agents** they may run, the **model providers** they may name, and
the **git providers** their work may come from, and the **stored policies** they
may select ("Capabilities: what one member, or one group, may do" above has the
kind table). A ninth is about the member rather than a run: whether they may add
an **SSH key** or mint an **API token** at all (`feature`).
Eight of the nine *narrow* — until you enforce one, members keep exactly the powers
they had, and a deny bites even before you do; base images are the one that
*widens*, so a grant is what makes an image nameable at all.

A permission always bounds what the **member** chose and never what you
pre-authorized. Model providers are the one exception, and the answer to "can I
fence a model provider": the `model_provider` kind bounds the provider a run
names, the one its workspace pins and the agent's default alike, because every
model credential is the person's own. The assigned governance profile's egress
still applies on top: its denied hosts are re-asserted at dispatch and withhold
every credential lane that would reach one.

Governance profiles also carry the limits that are not choices at all —
`max_concurrent_runs`, and the two launch modes a profile can refuse outright —
and, like every profile field, they bind only the people a profile is assigned to.

Profiles and their assignments live in Postgres, so `make reset` / `make
reset-all` take them with the volume; `wardyn governance get > governance.json`
(`GET /api/v1/governance`) before a reset and `wardyn governance apply
governance.json` after is the round-trip (0.8, #1108) — the same get-then-apply
shape `wardyn site-config get|set` and `wardyn drive get|apply` already take.
`apply` upserts every named profile **by name** (its unique handle) and every
assignment by its own natural key (subject_type, subject); a profile the file
does not mention is left alone, and one present server-side but absent from the
file is only removed with `--prune` (which also removes an omitted assignment,
assignments before profiles so a still-referenced profile never trips the
delete-while-assigned refusal above). `wardyn governance get > f && wardyn
governance apply f` is a no-op: unchanged rows are skipped rather than
re-written, so a repeat apply issues no writes and adds no audit rows.

