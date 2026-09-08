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
// health.putSiteConfig does that, ONCE, from this list. A fourth server-owned
// field is a line here and nothing else.
export const SERVER_OWNED_SITE_CONFIG_KEYS = ["integrations", "onboarding_completed_at"] as const satisfies readonly (keyof SiteConfig)[];

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
