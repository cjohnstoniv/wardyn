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

// The org's workspace-provider policy. Hand-maintained mirror of Go's
// types.WorkspaceProviders (internal/types/workspace_provider.go) — the json
// tags verbatim; a removed wire field is a runtime TypeError only e2e catches.
export interface WorkspaceProviders {
  git?: GitProvider[];
  storage?: StorageProviders;
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
  // The closed lane set (ClosedGitLanes). EMPTY MEANS EVERY LANE the kind
  // supports — the field narrows, it never widens.
  lanes?: GitLane[];
}

// The closed git-provider kinds. A self-hosted forge is not a third kind: it is
// a "github" (GHES) or "azure_devops" (ADO Server) row naming its own host.
export type GitProviderKind = "github" | "azure_devops";

// The closed credential lanes a run may clone with. "app" is github.com only
// (the broker has no Azure DevOps equivalent); "ssh" reaches only the two hosts
// publishing an SSH-over-443 endpoint.
export type GitLane = "app" | "pat" | "ssh";

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
