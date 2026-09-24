/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Site config — the operator-wide baseline every run inherits (mirrors
// internal/types.go's SiteConfig EXACTLY, incl. json tags). GET/PUT
// /api/v1/site-config (admin-gated write). Secret VALUES are never
// included on the wire — only the ref NAMES the broker/proxy resolve at
// dispatch/injection time.

/** @deprecated superseded by EgressRedirect — kept only so a PUT body saved
 *  before EgressRedirects existed keeps decoding (see SiteConfig.artifact_overrides). */
export interface ArtifactOverride {
  base_url: string;
  token_secret_ref?: string;
}

// One outbound redirect: requests to `from` are substituted to `to` (from's
// host dropped from egress, to's host allowed), with an optional token
// injected proxy-side. `ecosystem` set (one of the six package-manager keys)
// ALSO emits that ecosystem's per-tool config file, in addition to the egress
// substitution + token injection every redirect gets; empty means
// NETWORK-ONLY — egress + token only, no config file (there's no ".npmrc
// equivalent" for an arbitrary host like a container registry or a telemetry
// endpoint).
export interface EgressRedirect {
  from: string;
  to: string;
  token_secret_ref?: string;
  // The token sourced from a stored Integration's credential instead of a
  // bare secret ref — mutually exclusive with token_secret_ref (WIRE-3;
  // internal/api/site_config.go 400s a body setting both).
  token_integration_ref?: string;
  ecosystem?: string;
}

export interface SiteConfig {
  upstream_proxy_secret_ref?: string;
  // The corporate upstream proxy URL written IN THE CLEAR — topology, not a
  // credential. Must not embed a userinfo (user:pass@); the server 400s that
  // and the FE should store a credentialed URL as a secret
  // (upstream_proxy_secret_ref) instead. Mutually exclusive in practice with
  // upstream_proxy_secret_ref (either may be set; the URL wins when both are).
  upstream_proxy_url?: string;
  // The upstream proxy's BYPASS list — destinations wardyn-proxy dials
  // DIRECTLY instead of CONNECTing through the upstream (the operator-hop
  // equivalent of NO_PROXY). Exists for a private-endpoint estate where a
  // corporate forward proxy refuses to CONNECT to an internal address. Does
  // NOT lift the SSRF guard or grant policy allow on its own — see
  // internal/types/site_config.go's SiteConfig.UpstreamProxyNoProxy.
  upstream_proxy_no_proxy?: string[];
  /** @deprecated superseded by egress_redirects, which generalizes this from
   *  package registries to any outbound URL/host. */
  artifact_overrides?: Record<string, ArtifactOverride>;
  // The operator's outbound redirect list — see EgressRedirect.
  egress_redirects?: EgressRedirect[];
  scm_hosts?: string[];
  // RESPONSE-ONLY, never-PUT: the stored Integrations, echoed on every GET so
  // a caller CAN see them, but PUT /site-config hard-400s any body that
  // carries a non-empty `integrations` (internal/api/site_config.go's
  // handlePutSiteConfig) — they're written through their own /integrations
  // endpoints instead. health.putSiteConfig strips this field before every
  // write, so spreading a GET response straight into a PUT body (the natural
  // "patch one field" idiom every SiteConfig writer here uses) can't
  // resurrect the 400. Loosely typed: nothing client-side reads this array
  // today (see lib/types/setup.ts's WireIntegration for the shape a caller
  // that DOES need to read integrations should use instead).
  readonly integrations?: unknown[];
  // Operator-declared internal hostnames the proxy's unconditional private/
  // reserved-IP SSRF guard is lifted for (an in-cluster service, a corporate
  // registry) — see InternalHost. The internal model gateway needs no entry
  // here: only the proxy's own brokered LLM route resolves/dials it, with its
  // own relaxed vet (see OPERATIONS.md's Internal model gateway section). No
  // reader exists yet (the Network step's rendering ships later); the field
  // mirrors the server shape so a GET/PUT round-trip never drops it.
  internal_hosts?: InternalHost[];
  // The org's workspace-provider policy — which git hosts a run may clone from
  // and with which credential lanes, plus the storage ceilings. Absent (the
  // default) is legacy open mode. Written through its own
  // GET/PUT /workspace-providers endpoints; PUT /site-config also accepts it
  // (an absent key carries the stored value forward, `{}` clears it), but no
  // console surface writes it here — health.putSiteConfig strips it from every
  // GET-spread body via SERVER_OWNED_SITE_CONFIG_KEYS so a Network-step save
  // can never clobber the providers page.
  workspace_providers?: WorkspaceProviders;
  // The org's agent roster — which coding agents this deployment offers, the one
  // model-access lane each may use, and whose credential that is. Absent (the
  // default) is legacy open mode. Written through its own
  // GET/PUT /agent-providers endpoints on exactly the terms the sibling block
  // above is, and stripped from every GET-spread body for the same reason
  // (SERVER_OWNED_SITE_CONFIG_KEYS).
  agent_providers?: AgentProviders;
  // The org's model-provider configuration — which kinds of model credential
  // this deployment supports, where each sends requests, and which agents may
  // use it. Configuration only: every person brings their own credential.
  // Absent (the default) is today. No console surface writes it yet, and it is
  // stripped from every GET-spread body (SERVER_OWNED_SITE_CONFIG_KEYS) for the
  // sibling blocks' reason.
  model_providers?: ModelProviders;
  // #484 — the admin's own "what to do next" under the four sign-in refusals a
  // person cannot clear alone (types.SiteConfig.SignInHelpText/URL). PUBLIC:
  // the anonymous /healthz publishes both. Plain text, at most 1,000
  // characters; the URL is http(s) only. Edited on the People step.
  sign_in_help_text?: string;
  sign_in_help_url?: string;
  // RESPONSE-ONLY, never-PUT: the git hosts this deployment actually admits —
  // scm_hosts MINUS every host a provider row claims, UNION every enabled row's
  // hosts (internal/api/workspace_providers.go's effectiveScmHosts). ONE
  // spelling of the claim rule, in Go: the console reads this instead of
  // re-deriving it, because the rule is kind-wide on the two well-known hosts
  // and a client mirror of that table would drift. Older daemons omit it — read
  // it as `effective_scm_hosts ?? scm_hosts`.
  readonly effective_scm_hosts?: string[];
  // RESPONSE-ONLY, never-PUT, exactly like `integrations` above: when the
  // operator finished (or deliberately left) the Getting Started funnel on
  // THIS INSTALL. Mirrors types.SiteConfig.OnboardingCompletedAt
  // (internal/types/types.go:340, `json:"onboarding_completed_at,omitempty"`),
  // stamped by POST /setup/onboarding/complete; PUT /site-config hard-400s any
  // body that carries it ("onboarding_completed_at is managed by the setup
  // flow, not PUT /site-config", internal/api/site_config.go). It is typed here
  // precisely so health.putSiteConfig can strip it — an untyped key rides
  // invisibly through the GET-spread idiom every writer uses.
  readonly onboarding_completed_at?: string;
}

// The keys GET /site-config returns that PUT /site-config REFUSES: each one is
// server-owned, echoed on the read so a caller can see it, and a hard 400 on
// the write. Every SiteConfig writer in the console starts from a GET and
// spreads onto it (`mutate({ ...(siteConfig ?? {}), field })`), so any such key
// rides into the PUT body unless it is stripped centrally —
// health.putSiteConfig does that, ONCE, from this list. Another server-owned
// field is a line here and nothing else.
//
// workspace_providers is on this list for a different reason than the other two:
// the server does NOT refuse it on PUT /site-config (that door is how MDM
// delivers the policy to a laptop). It is stripped because no console surface
// that spreads a GET means to write the provider block — only the providers page
// does, through PUT /workspace-providers with an If-Match — and a stale spread
// would silently revert an admin's providers to whatever this tab last read.
export const SERVER_OWNED_SITE_CONFIG_KEYS = [
  "integrations",
  "onboarding_completed_at",
  "workspace_providers",
  "agent_providers",
  "model_providers",
  "effective_scm_hosts",
] as const satisfies readonly (keyof SiteConfig)[];

// The org's agent roster. Hand-maintained mirror of Go's types.AgentProviders
// (internal/types/agent_provider.go) — the json tags verbatim; a removed wire
// field is a runtime TypeError only e2e catches.
export interface AgentProviders {
  agents?: AgentProvider[];
}

// One agent row. `disabled` is negative-sense so the zero value is ENABLED, and
// a disabled row is rendered disabled with a reason, never hidden.
export interface AgentProvider {
  // A harness-catalog id ("claude-code", "codex-cli", "none") or a
  // WARDYN_AGENT_IMAGES key — the server admits both and refuses anything else.
  id: string;
  disabled?: boolean;
  // Closed set, server-validated: "anthropic_subscription" | "anthropic_api_key"
  // | "openai_api_key" | "bedrock_bearer" | "bedrock_sso" | "bedrock_env" |
  // "bedrock_aws_dir" | "none". The ONE lane this agent's runs may use; there is
  // no cross-mechanism fallback once it is declared.
  mechanism: string;
  // Closed set, server-validated: "shared" | "per_user". Absent reads as
  // "shared" — today's behaviour. "per_user" is available for "bedrock_sso"
  // only in 0.7.2.
  credential_source?: string;
  // The AWS access portal every principal signs in against — required when
  // mechanism is "bedrock_sso" and credential_source is "per_user", refused
  // otherwise. ADMIN-OWNED: a member's sign-in never chooses another.
  sso_start_url?: string;
  // Pin WHICH AWS account and role a sign-in for this row may capture.
  // Optional, set together, permitted only where sso_start_url is
  // (bedrock_sso + per_user). ADMIN-OWNED for the same reason sso_start_url is.
  sso_account_id?: string;
  sso_role_name?: string;
}

// The org's model-provider configuration. Hand-maintained mirror of Go's
// types.ModelProviders (internal/types/model_provider.go) — the json tags
// verbatim; a removed wire field is a runtime TypeError only e2e catches.
export interface ModelProviders {
  providers?: ModelProvider[];
}

// Closed set, server-validated.
export type ModelProviderKind =
  | "anthropic_subscription"
  | "bedrock_sso"
  | "anthropic_api_key"
  | "openai_api_key"
  | "bedrock_bearer"
  | "custom_endpoint";

// One provider. No credential lives here — each person supplies their own.
export interface ModelProvider {
  // The admin's slug ("corp-gateway"), what a run names.
  id: string;
  // SERVER-OWNED: minted on first write, carried by id, never reissued. A
  // submitted value is ignored.
  readonly uid?: string;
  // What people see when they choose it.
  name?: string;
  kind: ModelProviderKind;
  // Negative-sense: absent is ENABLED. A disabled provider may still be an
  // agent's default, whose runs are then refused rather than moved elsewhere.
  disabled?: boolean;
  // custom_endpoint: required. Anthropic/OpenAI kinds: an optional
  // route-through gateway. Bedrock kinds: never (see bedrock.base_url).
  base_url?: string;
  // custom_endpoint only: how each person's token is sent. The server fills
  // Authorization / "Bearer %s" when absent.
  auth?: ProviderAuth;
  // bedrock_sso / bedrock_bearer only.
  bedrock?: BedrockSettings;
  // The agents this provider may serve, with the settings for each.
  harnesses?: ProviderHarness[];
}

export interface ProviderAuth {
  header?: string;
  format?: string;
}

export interface BedrockSettings {
  region?: string;
  base_url?: string;
  // bedrock_sso only, and ADMIN-OWNED: a sign-in never chooses another.
  sso_start_url?: string;
  sso_account_id?: string;
  sso_role_name?: string;
}

export interface ProviderHarness {
  // A harness-catalog id ("claude-code", "codex-cli").
  harness: string;
  // Admin-set, no member override; required on a Bedrock kind.
  model?: string;
  // custom_endpoint only: where the endpoint serves this agent's API dialect.
  path?: string;
  auth_header?: string;
  auth_format?: string;
}

// The org's workspace-provider policy. Hand-maintained mirror of Go's
// types.WorkspaceProviders (internal/types/workspace_provider.go) — the json
// tags verbatim; a removed wire field is a runtime TypeError only e2e catches.
export interface WorkspaceProviders {
  git?: GitProvider[];
  storage?: StorageProviders;
  // git_pat_broker_enabled is READ-ONLY and SERVER-PROJECTED (#381): the
  // deployment's own WARDYN_GIT_PAT_BROKER switch, present only once at least
  // one git row is configured (absent on a never-configured install), never
  // sent on a PUT (the server clears it if one does). true/false, never a
  // bare boolean default — see scm-provider.ts's patLaneMeta.
  git_pat_broker_enabled?: boolean;
}

// One git-provider row. `disabled` is negative-sense so the zero value is
// ENABLED, and a present-but-disabled row still CLAIMS its hosts (admission
// refuses them) rather than falling back to the legacy scm_hosts list.
export interface GitProvider {
  id: string;
  // The closed kind set (internal/types/workspace_provider.go's
  // ClosedGitProviderKinds) — a write naming anything else is a 400.
  kind: GitProviderKind;
  disabled?: boolean;
  // The https addresses this row admits — no port, no credentials, no query or
  // fragment, at most two path segments. A repo is admitted when its clone URL's
  // scheme and host match one of these and its path equals that base URL's path
  // or extends it at a "/" boundary.
  base_urls: string[];
  // The closed lane set (ClosedGitLanes). EMPTY MEANS EVERY LEGACY LANE
  // (LegacyGitLane) — the field narrows, it never widens, and a lane added
  // after that list was frozen must be NAMED here to be usable.
  lanes?: GitLane[];
  // Whose credential this row's lanes use. Absent reads as "shared". The
  // "entra" lane REQUIRES "per_user" — the server refuses anything else,
  // because there is no such thing as a shared Entra sign-in.
  credential_source?: CredentialSource;
  // The Entra lane's configuration. Present only on a row whose lanes name
  // "entra", and required on one: the server refuses an orphaned block, and
  // refuses the lane without it.
  entra?: ADOEntraConfig;
}

// Whose credential a provider row's lanes use.
export type CredentialSource = "shared" | "per_user";

// How an Entra-lane run presents itself to Azure DevOps. Absent reads as
// "bearer", the only accepted mode: the server refuses "minted_pat" because
// Azure DevOps mints personal access tokens only for Microsoft's own clients.
export type ADOTokenMode = "bearer";

// The Entra lane's configuration (types.ADOEntraConfig). The capability
// strings are the classifier's vocabulary (internal/adoscope) — the console
// never invents one, and never re-words a capability's label.
export interface ADOEntraConfig {
  tenant_id: string;
  client_id: string;
  // The widest access a run on this row may ever hold. Non-empty, and it must
  // include "read".
  capability_ceiling?: string[];
  // What a run gets when it asks for nothing. Empty reads as ["read"], and it
  // must sit inside capability_ceiling.
  default_profile?: string[];
  token_mode?: ADOTokenMode;
  // Whether REST calls are brokered on this lane. ABSENT MEANS TRUE, which is
  // why it is optional rather than a plain boolean the console might write as
  // false by omission.
  rest_api?: boolean;
}

// The closed git-provider kinds. A self-hosted forge is not a third kind: it is
// a "github" (GHES) or "azure_devops" (ADO Server) row naming its own host.
export type GitProviderKind = "github" | "azure_devops";

// The lanes an EMPTY `lanes` list admits — types.LegacyGitLanes, frozen at the
// three that existed when "empty means every lane" was written down. The Git
// tab renders exactly these; a lane outside the list is configured elsewhere
// and must survive a toggle here untouched.
export type LegacyGitLane = "app" | "pat" | "ssh";

// The closed credential lanes a run may clone with (ClosedGitLanes). "app" is
// github.com only (the broker has no Azure DevOps equivalent); "ssh" reaches
// only the two hosts publishing an SSH-over-443 endpoint; "entra" is Azure
// DevOps hosted only and authorizes each person as themselves.
export type GitLane = LegacyGitLane | "entra";

// The file-system half. Each absent sub-block is legacy behaviour for that half.
export interface StorageProviders {
  ephemeral?: EphemeralProvider;
  user_drive?: UserDriveProvider;
}

// Ephemeral scratch: the size a run gets when it asks for none, and the ceiling a
// larger request is CLAMPED to (never refused — disk_mib is authored on
// policies). 0 = unset/unlimited.
export interface EphemeralProvider {
  default_disk_mib?: number;
  max_disk_mib?: number;
}

// The org-level user-drive switch and ceiling. `disabled` is the ORG switch
// ("this install offers no drives"), a different question from a governance
// profile's per-member door. 0 = no ceiling.
export interface UserDriveProvider {
  disabled?: boolean;
  max_size_mib?: number;
}

// One SiteConfig.internal_hosts entry — see that field's doc.
export interface InternalHost {
  // Matches a request host by label suffix: host_suffix itself, or any host
  // ending in "."+host_suffix (never a substring/mid-label match).
  host_suffix: string;
  // Scopes the lift to these CIDRs only (each must lie inside RFC1918,
  // fc00::/7, or 100.64.0.0/10 — validated server-side). Empty means the full
  // liftable set for a matching host.
  cidrs?: string[];
}
