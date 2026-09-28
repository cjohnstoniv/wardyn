// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package types defines Wardyn's core domain vocabulary: the four nouns
// (AgentRun, RunPolicy, CredentialGrant, ApprovalRequest) plus the audit
// event shape. These types are the single source of truth shared by the
// control plane, runners, sidecars, and (on Kubernetes) the CRD layer.

package types

import "time"

// The site-config family: SiteConfig and the value types its fields carry.
// Split out of types.go by seam in 0.7.2 when the struct gained the provider
// blocks (workspace_providers, agent_providers) and the file crossed the
// 1000-line gate — nothing here changed meaning in the move.

// SiteConfig is the operator-wide, admin-authored baseline every run inherits:
// a corporate upstream proxy, egress redirects (package-registry mirrors and,
// more generally, any outbound URL/host/IP redirect), and default SCM hosts.
// There is exactly one SiteConfig for the operator (a store singleton);
// GetSiteConfig returns the zero value when none has been written yet —
// "unconfigured" is a valid, common state, not an error.
//
// Secret VALUES never live here — only secret NAMES (refs) the broker/proxy
// resolve at dispatch/injection time, mirroring GrantSpec.Scope.
type SiteConfig struct {
	// UpstreamProxySecretRef names a secret holding the corporate upstream proxy
	// URL (optionally with embedded user:pass), or "" when none is configured.
	// Mutually exclusive IN PRACTICE with UpstreamProxyURL (UpstreamProxyURL
	// wins when both are set — resolveUpstreamProxyURL) but not rejected as a
	// validation error, since an operator migrating between them may round-trip
	// both briefly.
	UpstreamProxySecretRef string `json:"upstream_proxy_secret_ref,omitempty"`
	// UpstreamProxyURL is the corporate upstream proxy URL written IN THE CLEAR
	// (http only). A proxy URL is topology, not a credential, and forcing every
	// operator through the write-only secret store means a mistyped URL can
	// never be read back to debug. MUST NOT embed userinfo — validateSiteConfig
	// rejects that with a 400 pointing at UpstreamProxySecretRef instead.
	UpstreamProxyURL string `json:"upstream_proxy_url,omitempty"`
	// UpstreamProxyNoProxy is the corporate upstream proxy's BYPASS list: the
	// destinations wardyn-proxy dials DIRECTLY instead of CONNECTing through the
	// upstream — the operator-hop equivalent of NO_PROXY, same spelling:
	//
	//   - a HOST or DOMAIN SUFFIX matched by label suffix (the entry itself, or
	//     any host ending in "."+entry, never a mid-label substring); or
	//   - a CIDR matched against a literal-IP destination.
	//
	// Exists because a corporate forward proxy will not CONNECT to an internal
	// address, so on a private-endpoint (PrivateLink) estate EVERY private
	// endpoint times out while the upstream takes every dial. SAFE because a
	// bypassed dial still falls through to the unconditional private/reserved-IP
	// SSRF guard exactly as an unproxied dial does — InternalHosts is still the
	// one and only lift, and the two compose. Grants no policy allow either;
	// allowed_domains/denied_domains decide reachability as before.
	//
	// The NO_PROXY "*" wildcard is REFUSED at write time: "bypass everything" is
	// spelled by clearing upstream_proxy_url, not by one silently un-chaining
	// character here. Empty (the default) => every dial keeps chaining through
	// the upstream, byte-identical to before this field existed.
	UpstreamProxyNoProxy []string `json:"upstream_proxy_no_proxy,omitempty"`
	// ArtifactOverrides maps an ecosystem ("npm"|"pip"|"cargo"|"maven"|"go"|
	// "nuget") to its corporate artifact-registry redirect.
	//
	// Deprecated: superseded by EgressRedirects, which generalizes this from
	// package registries to any outbound URL/host. Kept ONLY so PUT
	// /site-config and `wardyn site-config set` keep accepting a document
	// saved before this release (decodeStrict rejects unknown fields, so
	// removing this would 400 every legacy body instead of folding it).
	// handlePutSiteConfig folds a non-empty value into EgressRedirects
	// (rejecting a body that sets both); migration 0030 performs the same
	// rewrite once on the stored document. Never populated by a post-migration
	// read — treat a non-empty value outside the fold as legacy request input.
	ArtifactOverrides map[string]ArtifactOverride `json:"artifact_overrides,omitempty"`
	// EgressRedirects is the operator's outbound redirect list: FROM a public/
	// upstream URL or host, TO a corporate-internal replacement, generalizing
	// ArtifactOverride to any destination. Each entry is one of two tiers,
	// discriminated by Ecosystem:
	//
	//   - Ecosystem set: FULL behavior, unchanged from ArtifactOverride — a
	//     per-tool config file via workspacescan.EmitArtifactConfig, PLUS egress
	//     substitution, PLUS token injection.
	//   - Ecosystem "" (NETWORK-ONLY): egress substitution + token injection but
	//     NO config file — there is no ".npmrc equivalent" for an arbitrary
	//     host, and inventing one would misstate what Wardyn configures.
	EgressRedirects []EgressRedirect `json:"egress_redirects,omitempty"`
	// ScmHosts are the operator's default SCM hosts (e.g. "dev.azure.com",
	// "github.example.com") the SCM Provider step / egress bundling consult.
	ScmHosts []string `json:"scm_hosts,omitempty"`
	// Integrations are the operator-configured external connections (AI
	// providers, SCM hosts, generic connections) — the generalized replacement
	// UpstreamProxySecretRef/EgressRedirects are migrating toward. Both the
	// legacy fields and this one are read; nothing removes the legacy fields
	// yet. IntegrationList folds pre-base-component rows forward at decode and
	// drops legacy artifact_mirror/host_proxy topology rows.
	Integrations IntegrationList `json:"integrations,omitempty"`
	// InternalHosts declares an internal hostname the proxy's unconditional
	// private/reserved-IP SSRF guard would otherwise refuse regardless of
	// policy; each entry LIFTS the guard for addresses matching its
	// host_suffix. LEAVE CIDRs EMPTY unless you know the addresses the SANDBOX
	// resolves — empty is the full RFC1918/ULA/CGNAT liftable set, still
	// suffix-scoped, and the right default: a list drawn from an operator's own
	// machine (a corporate resolver's CGNAT answer) excludes the in-VPC address
	// the sandbox actually gets, and the denial then looks like no entry at
	// all. Never loopback/link-local/metadata/unspecified/multicast/NAT64,
	// which stay denied unconditionally. The policy verdict
	// (allowed_domains/denied_domains) still has to allow the host separately
	// — this only lifts the SSRF builtin. Admin-only; validated at write time
	// so every declared CIDR lies inside ipguard.Liftable. Empty (the default)
	// => no lift, byte-identical to today.
	InternalHosts []InternalHost `json:"internal_hosts,omitempty"`
	// WorkspaceProviders is the org's workspace-provider POLICY — which git
	// hosts a run may clone from and with which credential lanes, plus
	// ephemeral/drive storage ceilings. A POINTER on purpose: a value struct's
	// omitempty is a no-op, which would add "workspace_providers":{} to every
	// 0.7.1-shaped GET and break the byte-identical upgrade claim. Nil (the
	// default) is legacy open mode.
	//
	// Written through its own GET/PUT /workspace-providers endpoints AND
	// through PUT /site-config, where an ABSENT key carries the stored value
	// forward and an explicit {} clears it — the site-config door is what
	// makes providers MDM-deliverable to a laptop.
	WorkspaceProviders *WorkspaceProviders `json:"workspace_providers,omitempty"`
	// AgentProviders is the org's agent-enablement POLICY — which coding agents
	// this deployment offers, the ONE model-access lane each may use, and
	// whether that credential is shared or per-person. A POINTER for the same
	// byte-identical-GET reason as WorkspaceProviders. Nil (the default) is
	// legacy open mode: the image map decides availability and the existing
	// credential precedence chain decides auth, as in 0.7.1.
	//
	// Written through its own GET/PUT /agent-providers endpoints AND through
	// PUT /site-config on the same absent-key/explicit-{} terms as the sibling
	// block above — what makes the agent roster MDM-deliverable.
	AgentProviders *AgentProviders `json:"agent_providers,omitempty"`
	// ModelProviders is the org's model-provider configuration — a POINTER for
	// the byte-identical-GET reason the two blocks above are. Nil (the
	// default) is today: create and dispatch keep the existing lane-resolution
	// path. Written through PUT /site-config on the sibling blocks' terms.
	ModelProviders *ModelProviders `json:"model_providers,omitempty"`
	// SignInHelpText and SignInHelpURL are the admin's own "what to do next",
	// shown on the sign-in page under the four refusals a person cannot clear
	// alone. PUBLIC by design: the anonymous /healthz publishes both, so they
	// must name a request process, never internal systems. Text is plain (at
	// most 1,000 characters, no control characters); the URL is http(s) only.
	// An absent key carries the stored value forward on PUT /site-config.
	SignInHelpText string `json:"sign_in_help_text,omitempty"`
	SignInHelpURL  string `json:"sign_in_help_url,omitempty"`
	// EffectiveScmHosts is READ-ONLY and SERVER-OWNED: which git hosts this
	// deployment actually admits — ScmHosts MINUS every host a present
	// provider row claims, UNION the hosts of every ENABLED provider row's
	// base URLs.
	//
	// A real field rather than a response-wrapper key because `wardyn
	// site-config get > f && wardyn site-config set f` decodes with
	// DisallowUnknownFields — a wrapper-only key would 400 that round trip.
	// PUT /site-config IGNORES a submitted value (cleared before the write,
	// like Integrations) and the console strips it from every GET-spread
	// write body. Projected on read; never stored.
	EffectiveScmHosts []string `json:"effective_scm_hosts,omitempty"`
	// OnboardingCompletedAt records when an operator finished (or deliberately
	// left) the Getting Started funnel on THIS INSTALL. Nil until then.
	//
	// Lives here, server-side, because every previous attempt to keep it in
	// the browser shipped a bug: a localStorage flag outlives the install it
	// describes (a wiped database kept skipping its own funnel) and is scoped
	// to an ORIGIN, so the same console reached at 127.0.0.1 and at localhost
	// disagreed about whether onboarding had happened.
	//
	// NOT settable through PUT /site-config: the handler IGNORES a
	// client-supplied value and carries the stored one forward, like
	// Integrations — otherwise a naive GET-then-PUT round-trip by an older
	// client would silently erase it. Ignored, never REFUSED: GET emits this
	// key, so the documented capture/apply round-trip echoes it back, and the
	// carry-forward already makes a submitted value inert. A dropped value is
	// reported instead (onboarding_completed_at_ignored in PUT's response,
	// which `wardyn site-config set` prints as a warning).
	OnboardingCompletedAt *time.Time `json:"onboarding_completed_at,omitempty"`
}

// InternalHost is one SiteConfig.InternalHosts entry — see that field's doc.
type InternalHost struct {
	// HostSuffix matches a request host by label suffix: HostSuffix itself, or
	// any host ending in "."+HostSuffix (never a substring/mid-label match).
	HostSuffix string `json:"host_suffix"`
	// CIDRs scopes the lift to these ranges only. Each must lie entirely inside
	// RFC1918, fc00::/7, or 100.64.0.0/10 (ipguard.Liftable) — never loopback/
	// link-local/metadata/multicast/NAT64. Empty means the lift applies to the
	// full Liftable set for a matching host.
	CIDRs []string `json:"cidrs,omitempty"`
}

// ArtifactOverride is one ecosystem's corporate artifact-registry redirect: the
// base URL to emit into that ecosystem's config (.npmrc/pip.conf/cargo config/
// settings.xml/GOPROXY/nuget.config) plus an optional secret ref for a token
// injected proxy-side (the sandbox never holds the value).
//
// Deprecated: superseded by EgressRedirect (BaseURL -> To, unchanged
// semantics). See SiteConfig.ArtifactOverrides for why the type is kept.
type ArtifactOverride struct {
	BaseURL        string `json:"base_url"`
	TokenSecretRef string `json:"token_secret_ref,omitempty"`
}

// EgressRedirect is one outbound redirect: requests to From are substituted to
// To (From's host dropped from egress, To's host allowed), with an optional
// token injected proxy-side for To's host. See SiteConfig.EgressRedirects for
// the two-tier Ecosystem behavior. From/To are validated with the same
// control-char/shell-metacharacter/real-host discipline as the legacy
// ArtifactOverride.BaseURL — either a full http(s) URL or a bare host;
// workspacescan.EmitArtifactConfig relies on that safety for its raw string
// interpolation into .npmrc/settings.xml/etc, so keep any future validation
// change there in sync.
type EgressRedirect struct {
	// From is the public/upstream URL or host being redirected away from.
	From string `json:"from"`
	// To is the corporate-internal URL or host every matching run's egress is
	// substituted to. For an Ecosystem row this is also the value emitted into
	// that ecosystem's config file (the old ArtifactOverride.BaseURL).
	To string `json:"to"`
	// TokenSecretRef optionally names a secret whose value is injected
	// proxy-side as a Bearer token for To's host (the sandbox never holds it).
	// Mutually exclusive with TokenIntegrationRef — a row setting both answers
	// the same question two ways and is rejected.
	TokenSecretRef string `json:"token_secret_ref,omitempty"`
	// TokenIntegrationRef optionally names an Integration
	// (SiteConfig.Integrations[i].ID) to take this redirect's token FROM,
	// instead of a bare secret in TokenSecretRef.
	//
	// The seam between the two surfaces: a private registry is genuinely both
	// a SYSTEM you authenticate to and sometimes the DESTINATION a public
	// endpoint is rerouted to. The integration owns the system and its
	// credential; the redirect owns rerouting a public endpoint to it, so a
	// redirect points AT the integration rather than restating its secret —
	// and inherits its Header/Format, so a feed authenticating with something
	// other than "Authorization: Bearer" finally can (the bare-secret path is
	// hardcoded to that shape). A ref naming nothing, a disabled row, or one
	// with no header credential degrades to redirect-WITHOUT-token, exactly as
	// a dangling TokenSecretRef already does.
	TokenIntegrationRef string `json:"token_integration_ref,omitempty"`
	// Ecosystem, when set, is one of the six package-manager ecosystems
	// ("npm"|"pip"|"cargo"|"maven"|"go"|"nuget") this redirect ALSO emits a
	// per-tool config file for, in addition to egress substitution and token
	// injection. Empty means NETWORK-ONLY: no config file is emitted.
	Ecosystem string `json:"ecosystem,omitempty"`
}
