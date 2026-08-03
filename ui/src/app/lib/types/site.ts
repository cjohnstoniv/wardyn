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
}
