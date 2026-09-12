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
// It is the ONE net-new persistence surface the enterprise Getting-Started
// enhancements introduce (the Host Proxy and Corporate Network / Egress
// Redirection steps read it; everything else rides secrets + grants). There is
// exactly one SiteConfig for the operator (a store singleton); GetSiteConfig
// returns the zero value when none has been written yet — "unconfigured" is a
// valid, common state, not an error.
//
// Secret VALUES never live here — only secret NAMES (refs) the broker/proxy
// resolve at dispatch/injection time, mirroring how RunPolicySpec's
// GrantSpec.Scope references secrets by name rather than embedding them.
type SiteConfig struct {
	// UpstreamProxySecretRef names a secret holding the corporate upstream proxy
	// URL (optionally with embedded user:pass), or "" when no upstream proxy is
	// configured. Mutually exclusive in PRACTICE with UpstreamProxyURL (either
	// may be set; UpstreamProxyURL wins when both are — see
	// resolveUpstreamProxyURL) but not rejected as a validation error, since an
	// operator migrating from one to the other may round-trip both briefly.
	UpstreamProxySecretRef string `json:"upstream_proxy_secret_ref,omitempty"`
	// UpstreamProxyURL is the corporate upstream proxy URL written IN THE CLEAR
	// (http only — see resolveUpstreamProxyURL). A proxy URL is topology, not a
	// credential, and forcing every operator through the write-only secret store
	// means a mistyped URL can never be read back to debug. It MUST NOT embed a
	// userinfo (user:pass@) — validateSiteConfig rejects that at write time with
	// a 400 telling the caller to use UpstreamProxySecretRef instead, which
	// exists precisely for a proxy that DOES need an embedded credential.
	UpstreamProxyURL string `json:"upstream_proxy_url,omitempty"`
	// UpstreamProxyNoProxy is the corporate upstream proxy's BYPASS list: the
	// destinations wardyn-proxy must dial DIRECTLY instead of CONNECTing through
	// the upstream. It is the operator-hop equivalent of the NO_PROXY every
	// sandbox already honours INSIDE itself, and it follows that convention's
	// spelling so nothing new has to be learned: each entry is either
	//
	//   - a HOST or DOMAIN SUFFIX ("vpce.amazonaws.com", ".corp.internal",
	//     "registry.corp.internal") matched by label suffix — the entry itself,
	//     or any host ending in "."+entry, never a mid-label substring; or
	//   - a CIDR ("100.64.0.0/10", "10.40.0.0/16") matched against a literal-IP
	//     destination.
	//
	// It exists because a corporate forward proxy will not CONNECT to an
	// internal address, so on a private-endpoint (PrivateLink) estate EVERY
	// private endpoint times out while the upstream takes every dial. WHAT IT
	// DOES NOT DO, and the reason it is safe: a bypassed dial falls through to
	// the proxy's own unconditional private/reserved-IP SSRF guard exactly as an
	// unproxied dial does, so bypassing a host does NOT make a private address
	// reachable — InternalHosts (above) is still the one and only lift, and the
	// two compose: the bypass routes the dial direct, InternalHosts lets that
	// address through. It also grants no policy allow; allowed_domains/
	// denied_domains decide reachability exactly as before.
	//
	// The NO_PROXY "*" wildcard is REFUSED at write time: "bypass everything" is
	// spelled by clearing upstream_proxy_url, and accepting it here would let one
	// character silently un-chain an estate's whole egress. Empty (the default)
	// => every forward dial keeps chaining through the upstream, byte-identical
	// to before this field existed.
	UpstreamProxyNoProxy []string `json:"upstream_proxy_no_proxy,omitempty"`
	// ArtifactOverrides maps an ecosystem ("npm"|"pip"|"cargo"|"maven"|"go"|
	// "nuget") to its corporate artifact-registry redirect.
	//
	// Deprecated: superseded by EgressRedirects, which generalizes this from
	// package registries to any outbound URL/host. Kept ONLY so the PUT
	// /site-config request decoder and `wardyn site-config apply` keep accepting
	// a document saved before this release — decodeStrict rejects unknown JSON
	// fields, so removing this field would turn every such legacy body into a
	// hard 400 instead of a fold. handlePutSiteConfig folds a non-empty value
	// into EgressRedirects (rejecting a body that sets both) and never persists
	// this field again; migration 0030 performs the same rewrite once, in place,
	// on the one already-stored document. Never populated by a read from
	// storage post-migration — treat a non-empty value outside the fold as
	// legacy request input only.
	ArtifactOverrides map[string]ArtifactOverride `json:"artifact_overrides,omitempty"`
	// EgressRedirects is the operator's outbound redirect list: FROM a public/
	// upstream URL or host, TO a corporate-internal replacement, generalizing
	// ArtifactOverride from package registries to any destination (a container
	// registry, a telemetry/SDK callback host, ...). Each entry is one of two
	// tiers, discriminated by Ecosystem:
	//
	//   - Ecosystem set (one of the ArtifactOverride closed set): FULL behavior,
	//     unchanged from the old ArtifactOverride — a per-tool config file
	//     (.npmrc/pip.conf/.cargo/config.toml/.m2/settings.xml/NuGet.Config/
	//     GOPROXY+GOSUMDB) via workspacescan.EmitArtifactConfig, PLUS egress
	//     substitution, PLUS token injection.
	//   - Ecosystem "" (NETWORK-ONLY): egress substitution (To's host allowed,
	//     From's host dropped) PLUS token injection for To's host, but NO config
	//     file — there is no ".npmrc equivalent" for an arbitrary host (a
	//     container registry, a telemetry endpoint, ...), and inventing one
	//     would be a lie about what Wardyn actually configures.
	EgressRedirects []EgressRedirect `json:"egress_redirects,omitempty"`
	// ScmHosts are the operator's default SCM hosts (e.g. "dev.azure.com",
	// "github.example.com") the SCM Provider step / egress bundling consult.
	ScmHosts []string `json:"scm_hosts,omitempty"`
	// Integrations are the operator-configured external connections (AI
	// providers, SCM hosts, generic connections) — the generalized replacement
	// UpstreamProxySecretRef/EgressRedirects are migrating toward. Both the
	// legacy fields and this one are read; nothing here removes the legacy
	// fields yet. IntegrationList folds pre-base-component rows forward at
	// decode and drops legacy artifact_mirror/host_proxy topology rows.
	Integrations IntegrationList `json:"integrations,omitempty"`
	// InternalHosts declares an internal hostname (an in-cluster service, a
	// corporate registry, the internal model gateway) the proxy's unconditional
	// private/reserved-IP SSRF guard would otherwise refuse regardless of
	// policy. Each entry LIFTS that guard for addresses matching its
	// host_suffix. LEAVE CIDRs EMPTY unless you know the addresses the SANDBOX
	// resolves — empty is the full RFC1918/ULA/CGNAT liftable set, still scoped
	// to the suffix, and it is the right default: a list drawn from what an
	// operator's own machine resolves for a private endpoint (a corporate
	// resolver's CGNAT answer) excludes the in-VPC address the sandbox actually
	// gets, and the denial then looks identical to having no entry at all.
	// Tighten only from sandbox-side resolution evidence. Never
	// loopback/link-local/metadata/unspecified/multicast/NAT64, which stay denied
	// unconditionally. The policy verdict
	// (allowed_domains/denied_domains) still has to allow the host separately —
	// this only lifts the SSRF builtin. Admin-only; validated at write time
	// (validateInternalHosts) so every declared CIDR lies inside
	// ipguard.Liftable. Empty (the default) => no lift, byte-identical to today.
	InternalHosts []InternalHost `json:"internal_hosts,omitempty"`
	// WorkspaceProviders is the org's workspace-provider POLICY — which git
	// hosts a run may clone from and with which credential lanes, plus the
	// ephemeral/drive storage ceilings. See WorkspaceProviders (a POINTER on
	// purpose: a value struct's omitempty is a no-op, which would add
	// "workspace_providers":{} to every 0.7.1-shaped GET /site-config and break
	// the byte-identical upgrade claim). Nil (the default) is legacy open mode.
	//
	// Written through its own GET/PUT /workspace-providers endpoints AND through
	// PUT /site-config, where an ABSENT key carries the stored value forward
	// (siteConfigFieldsAfter066) and an explicit {} clears it — the site-config
	// door is what makes providers MDM-deliverable to a laptop, whose
	// deploy/wardyn-desktop.sh re-applies /etc/wardyn/site-config.json on every
	// boot.
	WorkspaceProviders *WorkspaceProviders `json:"workspace_providers,omitempty"`
	// AgentProviders is the org's agent-enablement POLICY — which coding agents
	// this deployment offers, the ONE model-access lane each may use, and whether
	// that credential is shared or per-person. See AgentProviders (a POINTER for
	// the same byte-identical-GET reason WorkspaceProviders is one). Nil (the
	// default) is legacy open mode: the image map decides availability and the
	// existing credential precedence chain decides auth, exactly as in 0.7.1.
	//
	// Written through its own GET/PUT /agent-providers endpoints AND through
	// PUT /site-config on the same terms as the sibling block above — an absent
	// key carries the stored value forward, an explicit {} clears it — which is
	// what makes the agent roster MDM-deliverable to a laptop.
	AgentProviders *AgentProviders `json:"agent_providers,omitempty"`
	// EffectiveScmHosts is READ-ONLY and SERVER-OWNED: the one spelling of
	// "which git hosts does this deployment actually admit" — ScmHosts MINUS
	// every host a present provider row claims, UNION the hosts of every ENABLED
	// provider row's base URLs (internal/api's effectiveScmHosts).
	//
	// It is a real field of this struct rather than a response-wrapper key
	// because `wardyn site-config get > f && wardyn site-config apply f` decodes
	// with DisallowUnknownFields — a wrapper-only key would 400 that documented
	// round trip. PUT /site-config therefore IGNORES a submitted value (cleared
	// before the write, the way Integrations is carried forward) and the console
	// strips it from every GET-spread write body
	// (SERVER_OWNED_SITE_CONFIG_KEYS). Projected on read; never stored.
	EffectiveScmHosts []string `json:"effective_scm_hosts,omitempty"`
	// OnboardingCompletedAt records when an operator finished (or deliberately
	// left) the Getting Started funnel on THIS INSTALL. Nil until then.
	//
	// It lives here, server-side, because it is a fact about the install and
	// every previous attempt to keep it in the browser was wrong in a way that
	// shipped: a localStorage flag outlives the install it describes (a wiped
	// database kept skipping its own funnel) and is scoped to an ORIGIN, so the
	// same console reached at 127.0.0.1 and at localhost disagreed about
	// whether onboarding had happened.
	//
	// NOT settable through PUT /site-config: the handler IGNORES a
	// client-supplied value and carries the stored one forward, exactly as it
	// does for Integrations — otherwise a naive GET-then-PUT round-trip by an
	// older client would silently erase it. Ignored, never REFUSED: GET emits
	// this key, so the documented capture/apply round-trip and the
	// MDM-delivered /etc/wardyn/site-config.json echo it back on every write,
	// and the carry-forward already makes a submitted value inert. A dropped
	// value is reported instead — onboarding_completed_at_ignored in PUT's
	// response body, which `wardyn site-config apply` prints as a warning.
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
// the two-tier Ecosystem behavior. From/To are validated (validateSiteConfig)
// with the same control-char/shell-metacharacter/real-host discipline as the
// legacy ArtifactOverride.BaseURL — either a full http(s) URL (validSiteURL) or
// a bare host (validSiteHost); workspacescan.EmitArtifactConfig relies on that
// safety for its raw string interpolation into .npmrc/settings.xml/etc, so
// keep any future validation change there in sync.
type EgressRedirect struct {
	// From is the public/upstream URL or host being redirected away from.
	From string `json:"from"`
	// To is the corporate-internal URL or host every matching run's egress is
	// substituted to. For an Ecosystem row this is also the value emitted into
	// that ecosystem's config file (the old ArtifactOverride.BaseURL).
	To string `json:"to"`
	// TokenSecretRef optionally names a secret whose value is injected
	// proxy-side as a Bearer token for To's host (the sandbox never holds it).
	// Mutually exclusive with TokenIntegrationRef — validateSiteConfig rejects a
	// row that sets both, since they answer the same question two ways.
	TokenSecretRef string `json:"token_secret_ref,omitempty"`
	// TokenIntegrationRef optionally names an Integration (SiteConfig.
	// Integrations[i].ID) to take this redirect's token FROM, instead of naming
	// a bare secret in TokenSecretRef.
	//
	// This is the seam between the two surfaces, and it exists because a private
	// registry is genuinely both things: a SYSTEM you authenticate to, and
	// sometimes the DESTINATION a public endpoint is rerouted to. The rule that
	// keeps them from duplicating each other — the integration owns the system
	// and its credential; the redirect owns rerouting a public endpoint to it.
	// So a redirect points AT the integration rather than restating its secret.
	//
	// It carries more than the secret name: the integration's Header and Format
	// come with it, so a feed that authenticates with something other than
	// "Authorization: Bearer" finally can (the bare-secret path below is
	// hardcoded to that shape). A ref naming nothing, a disabled row, or one with
	// no header credential degrades to redirect-WITHOUT-token, exactly as a
	// dangling TokenSecretRef already does — a redirect that still reroutes is
	// more useful than a run that fails.
	TokenIntegrationRef string `json:"token_integration_ref,omitempty"`
	// Ecosystem, when set, is one of the six package-manager ecosystems
	// ("npm"|"pip"|"cargo"|"maven"|"go"|"nuget") this redirect ALSO emits a
	// per-tool config file for, in addition to the egress substitution and
	// token injection every redirect gets. Empty means NETWORK-ONLY: no config
	// file is emitted (see SiteConfig.EgressRedirects).
	Ecosystem string `json:"ecosystem,omitempty"`
}
