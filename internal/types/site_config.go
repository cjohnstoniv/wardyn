// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package types defines Wardyn's core domain vocabulary: the four nouns
// (AgentRun, RunPolicy, CredentialGrant, ApprovalRequest) plus the audit
// event shape. These types are the single source of truth shared by the
// control plane, runners, sidecars, and (on Kubernetes) the CRD layer.

package types

import (
	"time"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
)

// The site-config family: SiteConfig and the value types its fields carry.
// Split out of types.go by seam in 0.7.2; nothing here changed meaning.

// SiteConfig is the operator-wide, admin-authored baseline every run
// inherits: a corporate upstream proxy, egress redirects (package-registry
// mirrors and any outbound URL/host/IP redirect), and default SCM hosts. One
// singleton per operator; GetSiteConfig returns the zero value
// ("unconfigured") when none has been written yet.
//
// SECURITY: secret VALUES never live here — only secret NAMES (refs) the
// broker/proxy resolve at dispatch/injection time, mirroring GrantSpec.Scope.
type SiteConfig struct {
	// Runners defaults off. PUT /site-config retains the stored value; the
	// dedicated audited runner enablement operation owns changes.
	Runners *RunnerSettings `json:"runners,omitempty"`
	// UpstreamProxySecretRef names a secret holding the corporate upstream
	// proxy URL, or "" for none. UpstreamProxyURL wins if both are set (not
	// rejected, so an operator can migrate between the two).
	UpstreamProxySecretRef string `json:"upstream_proxy_secret_ref,omitempty"`
	// UpstreamProxyURL is the corporate upstream proxy URL written IN THE
	// CLEAR (http only) — topology, not a credential. MUST NOT embed
	// userinfo (validateSiteConfig rejects that).
	UpstreamProxyURL string `json:"upstream_proxy_url,omitempty"`
	// UpstreamProxyNoProxy is the upstream proxy's BYPASS list (like
	// NO_PROXY): destinations wardyn-proxy dials DIRECTLY, matched by
	// host/domain-suffix or CIDR. Exists for a private-endpoint estate whose
	// corporate forward proxy has no route to the endpoint but this sidecar
	// does: an entry is for a host the sidecar can itself resolve and reach.
	// Where the corporate proxy is the only route, leave the host off this
	// list and declare it in InternalHosts alone.
	//
	// SECURITY: a bypassed dial still hits the unconditional private/
	// reserved-IP SSRF guard (InternalHosts is the only lift) and grants no
	// policy allow. "*" is REFUSED at write time; empty (default) is
	// unchanged behavior.
	UpstreamProxyNoProxy []string `json:"upstream_proxy_no_proxy,omitempty"`
	// ArtifactOverrides maps an ecosystem ("npm"|"pip"|"cargo"|"maven"|"go"|
	// "nuget") to its corporate artifact-registry redirect.
	//
	// Deprecated: superseded by EgressRedirects; kept only so a
	// pre-EgressRedirects document still decodes. handlePutSiteConfig and
	// migration 0030 each fold a set value into EgressRedirects once.
	ArtifactOverrides map[string]ArtifactOverride `json:"artifact_overrides,omitempty"`
	// EgressRedirects is the operator's outbound redirect list: FROM a
	// public/upstream URL or host TO a corporate-internal replacement.
	// Ecosystem set gets the full behavior (config file + substitution +
	// token injection); Ecosystem "" is network-only (no config file).
	EgressRedirects []EgressRedirect `json:"egress_redirects,omitempty"`
	// ScmHosts are the operator's default SCM hosts (e.g. "dev.azure.com",
	// "github.example.com") the SCM Provider step / egress bundling consult.
	ScmHosts []string `json:"scm_hosts,omitempty"`
	// Integrations are the operator-configured external connections (AI
	// providers, SCM hosts, generic connections), the generalized
	// replacement the legacy fields above are migrating toward. Both are
	// read; nothing removes the legacy fields yet.
	Integrations IntegrationList `json:"integrations,omitempty"`
	// InternalHosts declares an internal hostname the proxy's unconditional
	// private/reserved-IP SSRF guard would otherwise refuse; each entry
	// LIFTS the guard for matching addresses.
	//
	// SECURITY: leave CIDRs empty unless you know the sandbox's actual
	// addresses — empty is the full liftable set, the safe default.
	// Loopback/link-local/metadata/multicast/NAT64 stay denied regardless.
	// This only lifts the SSRF builtin; policy still decides reachability.
	// Admin-only, validated against ipguard.Liftable. Empty => no lift.
	InternalHosts []InternalHost `json:"internal_hosts,omitempty"`
	// WorkspaceProviders is the org's workspace-provider POLICY: which git
	// hosts a run may clone from, with which credential lanes, plus storage
	// ceilings. A POINTER since a value struct's omitempty is a no-op
	// (would break the byte-identical-GET claim). Nil is legacy open mode;
	// PUT /site-config's absent-key/explicit-{} makes it MDM-deliverable.
	WorkspaceProviders *WorkspaceProviders `json:"workspace_providers,omitempty"`
	// AgentProviders is the org's agent-enablement POLICY: which coding
	// agents this deployment offers, each one's model-access lane, and
	// whether that credential is shared or per-person. A POINTER for the
	// same byte-identical-GET reason as WorkspaceProviders, with the same
	// absent-key/explicit-{} write terms. Nil is legacy open mode.
	AgentProviders *AgentProviders `json:"agent_providers,omitempty"`
	// ModelProviders is the org's model-provider configuration — a POINTER
	// for the same byte-identical-GET reason. Nil keeps today's
	// lane-resolution path.
	ModelProviders *ModelProviders `json:"model_providers,omitempty"`
	// SignInHelpText and SignInHelpURL are the admin's "what to do next",
	// shown on the sign-in page for refusals a person can't clear alone.
	//
	// SECURITY: PUBLIC — the anonymous /healthz publishes both, so they must
	// never name internal systems. Text <=1,000 chars, no control chars;
	// URL is http(s) only.
	SignInHelpText string `json:"sign_in_help_text,omitempty"`
	SignInHelpURL  string `json:"sign_in_help_url,omitempty"`
	// PolicyHelp is the contact a signed-in person is shown when a deployment-wide
	// rule (no profile binds them) refuses them. A POINTER for the same
	// byte-identical-GET reason as the provider blocks.
	//
	// NOT public: unlike the sign-in help pair, /healthz never publishes it.
	// Validated by policyref.Validate at PUT; a stored value is re-validated by
	// policyref.Project on every read.
	PolicyHelp *policyref.Contact `json:"policy_help,omitempty"`
	// Branding is the part of the console branding (#1215) a site config can
	// carry: today only the logo, as a file wardynd reads when this document is
	// applied. A POINTER for the same byte-identical-GET reason as the provider
	// blocks; nil is "the site config says nothing about branding". Name and
	// colours stay with the Branding card (PUT /branding/settings).
	Branding *SiteBranding `json:"branding,omitempty"`
	// Components is the org's custom-component POLICY. A POINTER for the same
	// byte-identical-GET reason as the provider blocks. Nil — never set, or a
	// document stored before the block existed — is the owner-decided default:
	// no autonomy cap, env-var and file delivery allowed, no CC3 floor for
	// component credentials.
	Components *ComponentSettings `json:"components,omitempty"`
	// EffectiveScmHosts is READ-ONLY, SERVER-OWNED: ScmHosts minus hosts a
	// provider row claims, union enabled providers' hosts. A real field
	// (not a wrapper key) so a get|set round trip still decodes under
	// DisallowUnknownFields. PUT ignores a submitted value; never stored.
	EffectiveScmHosts []string `json:"effective_scm_hosts,omitempty"`
	// WithheldScmHosts is READ-ONLY, SERVER-OWNED, on the same terms as
	// EffectiveScmHosts: each host a DISABLED provider row claims that
	// EffectiveScmHosts therefore leaves out, with the row that withholds it, so
	// an admin is told why a host is missing rather than left to look for it.
	// Projected on read, ignored on PUT, never stored.
	WithheldScmHosts []WithheldScmHost `json:"withheld_scm_hosts,omitempty"`
	// OnboardingCompletedAt records when an operator finished (or left) the
	// Getting Started funnel on THIS INSTALL. Nil until then.
	//
	// Server-side because a localStorage flag outlived a wiped database and
	// was scoped per-origin (127.0.0.1 vs localhost disagreed).
	//
	// NOT settable via PUT /site-config — the handler ignores a submitted
	// value and carries the stored one forward; a dropped value is reported
	// as onboarding_completed_at_ignored.
	OnboardingCompletedAt *time.Time `json:"onboarding_completed_at,omitempty"`
}

// SiteBranding is SiteConfig.Branding. LogoPath is an absolute path on the
// wardynd host to an SVG or PNG file; it is read, checked as a console upload
// is, and stored as the branding logo at every apply. Naming it makes the file
// the logo's owner: the console offers no Remove logo while it is set. Taking
// it out of the document (and applying) removes the logo it delivered.
type SiteBranding struct {
	LogoPath string `json:"logo_path,omitempty"`
}

// ComponentSettings is SiteConfig.Components. Every field's zero value is the
// owner-decided default, so an absent block and an all-zero one mean the same
// thing (PUT /site-config stores the latter as the former).
type ComponentSettings struct {
	// RequireVaultForCredentials restores the CC3 confinement floor for a
	// component's header credential to a non-baseline host, org and person rows
	// alike. False (default) lifts the floor for component-authored grants only.
	RequireVaultForCredentials bool `json:"require_vault_for_credentials,omitempty"`
	// DenyResidentDelivery refuses env-var and file delivery DEPLOYMENT-WIDE,
	// for org and person rows alike; header delivery keeps working.
	DenyResidentDelivery bool `json:"deny_resident_delivery,omitempty"`
	// AutonomyCap caps a run carrying a SELF-DEFINED component, with or without
	// a governance profile: "" no cap (default), "L1" holds tool calls, "L0"
	// refuses unattended runs. No other value is accepted: the cap only tightens.
	AutonomyCap AutonomyLevel `json:"autonomy_cap,omitempty"`
}

// InternalHost is one SiteConfig.InternalHosts entry — see that field's doc.
type InternalHost struct {
	// HostSuffix matches a request host by label suffix: itself, or any host
	// ending in "."+HostSuffix (never a mid-label substring match).
	HostSuffix string `json:"host_suffix"`
	// CIDRs scopes the lift to these ranges only, each inside RFC1918,
	// fc00::/7, or 100.64.0.0/10 (ipguard.Liftable) — never loopback/
	// link-local/metadata/multicast/NAT64. Empty lifts the full Liftable set.
	CIDRs []string `json:"cidrs,omitempty"`
}

// ArtifactOverride is one ecosystem's corporate artifact-registry redirect:
// the base URL to emit into that ecosystem's config (.npmrc/pip.conf/cargo
// config/settings.xml/GOPROXY/nuget.config) plus an optional secret ref for
// a token injected proxy-side (the sandbox never holds the value).
//
// Deprecated: superseded by EgressRedirect. See SiteConfig.ArtifactOverrides
// for why the type is kept.
type ArtifactOverride struct {
	BaseURL        string `json:"base_url"`
	TokenSecretRef string `json:"token_secret_ref,omitempty"`
}

// EgressRedirect is one outbound redirect: requests to From are substituted
// to To (From's host dropped from egress, To's host allowed), with an
// optional token injected proxy-side for To's host. From/To share
// ArtifactOverride.BaseURL's validation discipline — keep any future change
// there in sync, since workspacescan.EmitArtifactConfig relies on it for raw
// string interpolation into .npmrc/settings.xml/etc.
type EgressRedirect struct {
	// From is the public/upstream URL or host being redirected away from.
	From string `json:"from"`
	// To is the corporate-internal URL or host every matching run's egress
	// substitutes to; for an Ecosystem row it's also emitted into that
	// ecosystem's config file (the old ArtifactOverride.BaseURL).
	To string `json:"to"`
	// TokenSecretRef optionally names a secret injected proxy-side as a
	// Bearer token for To's host (the sandbox never holds it). Mutually
	// exclusive with TokenIntegrationRef.
	TokenSecretRef string `json:"token_secret_ref,omitempty"`
	// TokenIntegrationRef optionally names an Integration
	// (SiteConfig.Integrations[i].ID) to take this redirect's token FROM
	// instead of a bare secret. The redirect points AT the integration
	// rather than restating its secret, inheriting its Header/Format so a
	// feed authenticating with something other than "Authorization: Bearer"
	// finally can. Naming nothing, a disabled row, or one with no header
	// credential degrades to redirect-without-token.
	TokenIntegrationRef string `json:"token_integration_ref,omitempty"`
	// Ecosystem, when set, is one of six package-manager ecosystems
	// ("npm"|"pip"|"cargo"|"maven"|"go"|"nuget") this redirect ALSO emits a
	// config file for. Empty means NETWORK-ONLY: no config file is emitted.
	Ecosystem string `json:"ecosystem,omitempty"`
}

// WithheldScmHost is one entry of SiteConfig.WithheldScmHosts: a host kept out
// of the effective set because the named provider row is turned off. The row is
// named the way a refused clone names it, by kind and id.
type WithheldScmHost struct {
	Host         string          `json:"host"`
	ProviderID   string          `json:"provider_id"`
	ProviderKind GitProviderKind `json:"provider_kind"`
}

// RunnerSettings controls admission to runner routes. Nil and false both deny.
type RunnerSettings struct {
	Enabled bool `json:"enabled"`
}
