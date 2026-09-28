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
  (`github_app`'s brokered halves, `git_host`'s clone credentials, Bedrock's AWS
  env). Those lanes are why the type also models `resident_file`/`resident_env` —
  but an operator may not DECLARE one: there is no generic lane materializing a
  named secret into a sandbox path or env var, so a write naming one is refused.
  At most one `proxy_header` secret per row (the proxy injects one credential
  header per host), and every such secret targets the row's whole egress list.
- `egress[]` — where the system lives; why a host is reachable for a granted run
  instead of being hand-listed in every workspace.
- `config{}` — non-secret knobs, key-validated per closed kind (bedrock ⇒
  `region`/`model`/`auth_lane`, `github_app` ⇒ `app_id`/`installation_id`/`host`,
  `anthropic_subscription` ⇒ `lane`); an unknown key on a closed kind 400s by
  name.

`kind` is the ONE field that says what this connects to, and the closed set is the
ONLY writable set: `anthropic_api_key`, `anthropic_subscription`, `bedrock`,
`openai_api_key`, `github_app`, `git_host`. Each has behavior in code
(`capabilitiesFor`), so a new one is a code change, and a write naming anything
else 400s with the accepted list. Two carry-overs from 0.5: generic kinds — an
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
injection path. An operator with fifty integrations configured and a workspace
that names none of them gets a run whose spec is byte-identical to having none —
true for this `integration:<id>` fold, but not for **model access**: absent a
more specific binding, an AI-provider integration marked `DefaultFor: agent_runs`
still folds into the run, even one with no workspace at all
(`resolveRunIntegration`, `internal/api/llmcred.go`; see "Model access resolves"
below).

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

### Model access resolves — it does not default to none

**With a model-provider block set (0.8), none of this section applies.** A model
run is credentialed by the provider it chose (`enforceRunModelProvider`,
`internal/api/run_model_provider.go`) from its owner's own key, token or sign-in, or by
nothing: no integration folds, no managed or host-mounted subscription and no
operator key serves it, and dispatch drops every other model credential its
policy carries (`resolveProviderLane`, `internal/api/runs_dispatch_provider.go`).
Nor may an `env_secret` grant: one that would set a variable a model credential
rides in or a provider arm sets (`modelEnvNames`, `internal/api/provider_env.go`:
`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `OPENAI_API_KEY`,
`CLAUDE_CODE_OAUTH_TOKEN`, `AWS_BEARER_TOKEN_BEDROCK`, `AWS_ACCESS_KEY_ID`,
`ANTHROPIC_CUSTOM_HEADERS`, the Foundry, Vertex and Anthropic-on-AWS variables
(`ANTHROPIC_FOUNDRY_API_KEY`, `ANTHROPIC_FOUNDRY_AUTH_TOKEN`,
`ANTHROPIC_FOUNDRY_BASE_URL`, `ANTHROPIC_FOUNDRY_RESOURCE`,
`CLAUDE_CODE_USE_FOUNDRY`, `CLAUDE_CODE_USE_VERTEX`, `ANTHROPIC_VERTEX_BASE_URL`,
`ANTHROPIC_VERTEX_PROJECT_ID`, `ANTHROPIC_AWS_API_KEY`, `ANTHROPIC_AWS_BASE_URL`,
`ANTHROPIC_PROFILE`), and each arm's base-URL, model, region and config variables) is
refused with a 422 at create and Review, naming the grant and the variable, and
dispatch refuses the run again if one arrives another way. Every other
`env_secret` grant is placed as before, and with no block nothing changes.
The tiers below are the path of a deployment with no block.

A Claude run's model access is not configured per run. It resolves, in order
(`resolveRunIntegration`, `internal/api/llmcred.go`):

1. an explicit integration named on the run (`integration_id`);
2. else the primary workspace's `LLMCred.IntegrationRef` binding;
3. else the operator's `DefaultFor: agent_runs` integration — the one
   stored integration marked as the site-wide default for agent runs, of
   any AI-provider kind.

A workspace binding that names something — even something stale or
miscategorized — is the operator's SPECIFIC choice and does not cascade to the
site-wide default; that would be a credential surprise, not a convenience. Launch
and preflight resolve this identically (`foldRunIntegration`), so Review cannot
preview access the run won't get.

**When none of the three tiers resolves, that is not the same as no access.**
Below the Integration system, dispatch's own transport resolution
(`resolveLLMTransport`, `internal/api/runs_dispatch_llm.go`) still credentials
the run from whatever GLOBAL provider config exists, independent of any
integration or workspace binding: a Wardyn-managed subscription connected via
`wardyn subscription connect` (`managedInjectReady`,
`internal/api/harnesscred.go` — checks that the run's agent is `claude-code` and
a captured token exists, never that any integration names it) injects
proxy-side, and a global Bedrock config
(`WARDYN_BEDROCK_REGION`+`WARDYN_BEDROCK_MODEL`, [ENV.md](../ENV.md)) still
credentials Bedrock calls when no workspace/integration selection overrides it
(`resolveBedrockAuth`, `internal/api/runs_bedrock.go` — a selection wins only the
fields it sets). The `agent == "claude-code"` gate means the
managed-subscription fallback is not universal: a `codex-cli` run with a
connected managed subscription and no integration gets no model access via this
lane. Full transport precedence (subscription → Bedrock → api-key) once a run
reaches dispatch: [TRY-IT.md](../TRY-IT.md) → "Model auth: three ways".

### What the New Run rail states

The right-hand "What this run can do" panel is read, not asserted. Two of its
rows consult the server, and both are silent rather than wrong when the server
has not answered.

- **Credentials** names where THIS run's model credential will land. Pressing
  **Preflight** is what produces that answer for real: it dry-runs the exact body
  Launch would send and returns `proxy` (injected by the proxy at launch, never
  written into the sandbox — a static API key, a stored Bedrock bearer or the
  subscription token, injected as it stands; nothing is minted), `sandbox` (a
  live credential inside the run for its lifetime — a Bedrock SSO exchange
  mints role credentials there), or
  `image` (a `none` roster row — Wardyn wires nothing and cannot say where the
  image's own credential lives), or `unknown` (nothing resolved — reads the
  same "Resolved at launch." as no verdict at all). The `sandbox` arm chips
  whose credential it is ONLY for a Bedrock lane — **Per-person AWS sign-in**
  under a `per_user` roster row, **Admin's credential** under `shared` —
  ownership, not sign-in status: the rail is painted from the roster row alone
  and never reads whether that person has actually signed in. The Claude
  subscription's own `sandbox` case (the `~/.claude` mount with proxy-side
  injection off) carries no such chip — nothing AWS is involved.
  **With no click the rail states a residency only under a per-person Bedrock
  SSO roster row** (`credential_source: per_user`, `mechanism: bedrock_sso`),
  which is resident whether or not that person has signed in — the one shape the
  roster settles on its own, and the one whose Preflight answer is a `422` for
  exactly the member who needs it. Every other deployment WITH A PROVIDER
  CONNECTED reads "Resolved at launch." and an invitation to press Preflight
  (until Preflight has run). That is deliberate: a roster
  cannot tell which lane a run resolves — the run's policy, its workspace
  binding, and the folded run/workspace/default integration all move it — so an
  answer given before the run is described could be confidently wrong in either
  direction, which is the defect this replaced. With no model provider
  connected the rail shows the no-provider warning instead, and the Preflight
  hint does not render. A run that makes no model call
  (a shell command) shows no Credentials row at all.
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
pre-authorized, which is the whole answer to "can I fence a model provider": the
`integration` kind gates the integration a member names on the run, and nothing
else. The provider a workspace is pinned to and your site-wide `DefaultFor:
agent_runs` default are yours, so they still reach every run, granted or not —
gating them would let one `all` deny row strip the deployment's model access. What
bounds those is the assigned governance profile's egress: its denied hosts are
re-asserted at dispatch and withhold every credential lane that would reach one.

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

