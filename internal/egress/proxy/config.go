// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/ipguard"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Config is the wardyn-proxy sidecar configuration, loaded from a JSON file
// via the -config flag, or from stdin on Docker. The run token authenticates the sidecar to the
// control plane's internal endpoints (verified via identity.Provider.Verify
// with audience "wardyn-internal"); it is NOT a secret usable outside the
// platform. Injected PER-RUN credential VALUES never appear here — each is
// minted at startup from the broker and held only in proxy memory
// (InjectionConfig below carries a grant_id, never a value). One exception:
// UpstreamProxyURL below can embed the OPERATOR's own corporate-proxy
// credential, a genuine third-party secret that IS part of this struct (see
// its own doc). RunToken and MITMCAKeyPEM likewise ARE part of this struct,
// so they persist wherever the sidecar's own rendered config does: the
// proxy's memory, and the run's sealed run_proxy_configs row (#1176), which a
// revive rebuilds the proxy from and which is deleted when the run goes
// terminal. No container holds it at rest.
type Config struct {
	// RunID is the governed run this sidecar serves.
	RunID uuid.UUID `json:"run_id"`
	// ControlPlaneURL is the base URL of wardynd's internal TLS listener (e.g.
	// "https://wardynd:8443"). http:// is refused unless the host is loopback
	// (hoptls.CheckURL).
	ControlPlaneURL string `json:"control_plane_url"`
	// ControlPlaneCAPEM is wardynd's internal CA (internal/hoptls), the ONLY
	// root this sidecar trusts for control-plane calls. Required with an
	// https ControlPlaneURL.
	ControlPlaneCAPEM string `json:"control_plane_ca_pem,omitempty"`
	// RunToken authenticates internal calls (Authorization: Bearer <token>).
	RunToken string `json:"run_token"`
	// Policy is the compiled egress allowlist / method rules / first-use flag.
	Policy types.RunPolicySpec `json:"policy"`
	// Injection rules drive proxy-side credential injection (plain HTTP only).
	// Each rule carries the grant_id the secret is minted from at startup.
	Injection []InjectionConfig `json:"injection,omitempty"`
	// Listen is the proxy listen address (default ":3128").
	Listen string `json:"listen,omitempty"`
	// DecisionBufferSize caps the async decision-log buffer (default 1024).
	DecisionBufferSize int `json:"decision_buffer_size,omitempty"`
	// MITMCACertPEM / MITMCAKeyPEM are the OPTIONAL TLS-MITM certificate authority
	// (PEM). When both are set AND content inspection is enabled, the proxy
	// TLS-terminates opaque CONNECT tunnels to known LLM hosts (Anthropic/OpenAI)
	// to inspect the subscription-OAuth path. The CA PRIVATE KEY stays in proxy
	// memory only; the sandbox trusts only the CA's public cert (delivered to its
	// trust store by the driver). Empty => opaque passthrough (no MITM).
	MITMCACertPEM string `json:"mitm_ca_cert_pem,omitempty"`
	MITMCAKeyPEM  string `json:"mitm_ca_key_pem,omitempty"`
	// MITMHosts are OPERATOR-CONFIGURED corp artifact hosts (exact hostnames) this
	// proxy is permitted to TLS-MITM in addition to the built-in LLM hosts, so a
	// corporate registry token can be injected on the wire. See isMITMHost's trust
	// boundary: this is a tight per-host operator allowlist compiled at dispatch
	// from the site-config artifact overrides — NOT attacker-reachable (the sandbox
	// cannot set it), NEVER a wildcard, and only meaningful with a paired injection
	// rule that supplies the operator's OWN token. Empty => LLM hosts only.
	MITMHosts []string `json:"mitm_hosts,omitempty"`
	// GitGrants is the git-broker per-repo allowlist: canonical "<org>/<repo>" ->
	// the github_token grant id to mint from, backing the /wardyn/gh/ route so the
	// sandbox reaches only its granted repos (never all of github.com) and the token
	// stays proxy-side. Compiled at dispatch from the run's github grants. Empty =>
	// the git-broker route always 403s (no repo brokered).
	GitGrants map[string]uuid.UUID `json:"git_grants,omitempty"`
	// PATGrants is the git_pat broker's per-HOST allowlist: canonical lower-case
	// host -> the git_pat grant id to mint from, backing the /wardyn/git/ route.
	//
	// It is the non-GitHub half of the same idea as GitGrants, and it exists to
	// close an asymmetry rather than to add a feature: a github_token never
	// enters the sandbox, while a git_pat for GitLab or Azure DevOps was handed
	// to the in-sandbox credential helper and was therefore resident for the
	// life of the run. The grant kind's own doc calls that "the OPPOSITE of
	// api_key", because git-over-HTTPS is an opaque CONNECT tunnel the proxy
	// cannot inject Basic-auth into.
	//
	// This route removes the tunnel: agent-run rewrites those hosts to a
	// PLAIN-HTTP broker path, so the proxy terminates the request, mints
	// server-side and injects the credential itself. The PAT never reaches the
	// sandbox.
	//
	// Keyed per HOST, with the grant's own scope beside it: a PAT carries whatever
	// scope the operator issued it with and Wardyn cannot narrow the credential,
	// but it can narrow the RUN. A narrowed grant's repos and read-only axes are
	// enforced here, before any mint (pat_scope.go). Empty => the route always 403s.
	PATGrants map[string]PATGrant `json:"pat_grants,omitempty"`
	// BrokeredPATGrantIDs is every git_pat grant id of the run while the PAT
	// broker is on. handleBrokerMint refuses a sandbox-supplied mint naming one:
	// PATGrants is keyed per host and narrowed (a shadowed, vetoed or withheld
	// grant is absent from it), but each of those is still a stored PAT this
	// run's token could mint raw through the relay. Empty with the broker off.
	// An older proxy image refuses this key at start (strict decode), so the
	// proxy image is upgraded with wardynd.
	BrokeredPATGrantIDs []uuid.UUID `json:"brokered_pat_grant_ids,omitempty"`
	// ADOGrant is the run's per-person Azure DevOps grant, which drives the
	// REST gate (ado_gate.go, ado_grants.go). Nil == the gate is off. ONE grant
	// per sidecar: the gate is keyed by host, and every organisation shares
	// dev.azure.com, so a second grant could only overwrite the first one's
	// organisation pin. LoadConfigBytes still reads the older ado_grants list,
	// and refuses one with more than one entry.
	ADOGrant *ADOGrantConfig `json:"ado_grant,omitempty"`
	// AzureGates are the run's azure_foundry route gates, one per row (azure_gate.go). Empty == off.
	// Omitempty and set only for a run with such a row: an older proxy refuses a key it does not know.
	AzureGates []AzureGateConfig `json:"azure_gates,omitempty"`
	// MITMLLM reports whether TLS-MITM of the BUILT-IN LLM hosts (Anthropic/OpenAI)
	// is actually intended for this run — i.e. subscription credential injection OR
	// intercept_tls content inspection. Dispatch also mints the per-run CA for
	// artifact token injection (MITMHosts), so "a CA exists" no longer implies "LLM
	// MITM was wanted"; without this flag an artifact-only run would TLS-terminate a
	// direct CONNECT to Anthropic/OpenAI it was never asked to. Empty/false => the
	// LLM hosts stay opaque passthrough even when a CA is present for artifact hosts.
	MITMLLM bool `json:"mitm_llm,omitempty"`
	// UpstreamProxyURL is the OPTIONAL corporate parent/upstream proxy that this
	// sidecar chains its egress through (http[s]://[user:pass@]host[:port]). In a
	// locked-down corporate network the sandbox host has NO direct internet route
	// — the org's HTTP CONNECT proxy is the only way out (and is frequently a
	// PRIVATE address). When set, forward-egress dials are issued as
	// CONNECT <real-host> to this proxy; control-plane calls to wardynd never
	// traverse it. Any embedded credential persists wherever this struct's own
	// rendered config does — proxy memory while running, and the run's sealed
	// run_proxy_configs row until the run goes terminal (#1176; like RunToken,
	// above) — and is masked from all decision-log/stdout output. Empty =>
	// direct dial (backward-compatible).
	//
	// SOURCE vs TRANSPORT: the only source is the persisted site-config
	// (upstream_proxy_secret_ref, admin-authored via PUT /api/v1/site-config).
	// It is resolved per dispatch by resolveRunUpstreamProxy
	// (internal/api/runs_dispatch.go) through resolveUpstreamProxyURL (which
	// lives in internal/api/runs_bedrock.go), audited as
	// run.upstream_proxy.resolve on resolve success/failure, and merely
	// TRANSPORTED here by the run's ProxyConfig (on the sidecar's stdin on
	// Docker, internal/runner/docker/driver_proxy_revive.go).
	UpstreamProxyURL string `json:"upstream_proxy_url,omitempty"`
	// UpstreamProxyNoProxy is the upstream's BYPASS list
	// (SiteConfig.UpstreamProxyNoProxy, forwarded verbatim): host/domain
	// suffixes and CIDRs this sidecar dials DIRECTLY instead of CONNECTing
	// through UpstreamProxyURL. It is the operator-hop equivalent of the
	// NO_PROXY the sandbox already honours internally. This sidecar vets the
	// name on the upstream branch too (egressTarget): an endpoint that resolves
	// into blocked space is denied HERE (builtin:private-ip), and one that does
	// not is handed to the corporate forward proxy, whose own routing decides
	// whether it can be reached. An entry is for a host this sidecar can itself
	// resolve and reach; either way the bypass is what moves the dial local.
	//
	// It is a ROUTING list only: a bypassed dial falls through to the same
	// unconditional private/reserved-IP guard an unproxied dial does, so it
	// never makes a private address reachable on its own (InternalHosts above
	// is the one lift, and the two compose), and it grants no policy allow.
	// Control-plane authored, same trust boundary as TrustedCAPEM/
	// InternalHosts — the sandbox cannot set it. Empty (the default) => every
	// forward dial chains through the upstream, byte-identical to today.
	UpstreamProxyNoProxy []string `json:"upstream_proxy_no_proxy,omitempty"`
	// TrustedCAPEM is the OPERATOR's corporate CA bundle (WARDYN_TRUSTED_CA_FILE,
	// wardynd's Config.TrustedCAPEM), forwarded verbatim per run so THIS
	// sidecar's forward/egress transport additionally trusts a corporate
	// TLS-inspecting middlebox on the path to the real upstream. Never the
	// control-plane transport, which trusts ControlPlaneCAPEM alone. Control-plane
	// authored, same trust boundary as MITMCACertPEM/MITMCAKeyPEM above — the
	// sandbox cannot set it. Empty (the default) => system roots only,
	// byte-identical to today.
	TrustedCAPEM string `json:"trusted_ca_pem,omitempty"`
	// InternalHosts are the operator-declared internal hostnames
	// (SiteConfig.InternalHosts, forwarded verbatim) eligible for the
	// private-IP-guard lift in Proxy.vetHost — an admin-declared exception for a
	// specific in-cluster/CGNAT service name, never attacker-reachable (the
	// sandbox cannot set this). Empty (the default) => no lift, byte-identical
	// to today.
	InternalHosts []types.InternalHost `json:"internal_hosts,omitempty"`
	// LLMUpstreams maps a public vendor host ("api.anthropic.com" /
	// "api.openai.com") to the run's model provider's base URL (a custom
	// endpoint or route-through gateway, set at dispatch) that the
	// /wardyn/llm/* brokered routes dial instead of the vendor host. Control-plane-authored, same trust boundary
	// as TrustedCAPEM/InternalHosts above — the sandbox cannot set this. Empty
	// (the default) => every brokered LLM route dials the vendor host,
	// byte-identical to today.
	LLMUpstreams map[string]string `json:"llm_upstreams,omitempty"`
	// LLMChannelHosts maps a model host to the vendor whose request schema the
	// content scanner reads on it (api.anthropic.com or api.openai.com): an
	// azure_foundry endpoint, which speaks one of the two dialects. It feeds
	// host classification only, never LLMUpstreams' reverse lookup (llm_channel_hosts.go).
	// Control-plane-authored; omitempty because an older proxy refuses a key it does not know.
	LLMChannelHosts map[string]string `json:"llm_channel_hosts,omitempty"`
	// LLMUnavailableDetail is what the brokered-LLM 404 says about WHY no
	// credential is behind this run's LLM route, composed by the control plane at
	// dispatch (internal/api's llmUnavailableDetail) because only it knows the
	// deployment's model-provider posture — the sidecar knows only that no
	// injection rule matched. Control-plane-authored, same trust boundary as
	// LLMUpstreams above; the sandbox cannot set it. Empty (the default) => the
	// route's own generic detail, plus the below-policy clause either way
	// (proxyLLMRequest).
	LLMUnavailableDetail string `json:"llm_unavailable_detail,omitempty"`
	// Unattended marks a run nobody is driving (a non-interactive task run).
	// A push that touches a push_rules.require_review_paths entry is then
	// refused outright rather than held for a decision nobody is waiting to
	// make (push_hold.go). Control-plane-authored at dispatch; false (the
	// default) holds.
	Unattended bool `json:"unattended,omitempty"`
	// Attribution names the policy that governs this run, for the refusals the
	// policy decided (refusal_attribution.go). Control-plane-authored at dispatch
	// for a run under a governance profile, or any run when the site sets
	// policy_help; nil otherwise. LoadConfigBytes re-projects it, so what the
	// proxy writes into a header is never a value policyref.Project refused. An
	// older proxy image refuses this key at start (strict decode), so the proxy
	// image is upgraded before wardynd.
	Attribution *policyref.Ref `json:"attribution,omitempty"`
	// PushRuleSets are the push content rules per SCM entry (provider and
	// organisation): the git route applies the set of the entry that owns the
	// remote, and a remote whose entry has no set has no content rules. While a
	// config carries none, Policy.PushRules keeps applying to every remote (the
	// older-client mapping); the dispatch plan fans that block out to one set per
	// entry it brings, so a run is on one regime or the other. Control-plane
	// authored; nothing sets it until the git route reads it.
	PushRuleSets []types.PushRuleSet `json:"push_rule_sets,omitempty"`
	// HarnessToolRules are the tool rules per included harness. The approval
	// route evaluates the set of the harness that raised the call
	// (types.ToolEffectForHarness) and never another's; a harness with no entry
	// is held for a human. While a config carries none, Policy.ToolRules applies
	// to the run's one agent (the older-client mapping). Control-plane authored;
	// nothing sets it until the approval route reads it.
	HarnessToolRules []types.HarnessToolRules `json:"harness_tool_rules,omitempty"`
}

const (
	defaultListen     = ":3128"
	defaultBufferSize = 1024
)

// LoadConfig reads and validates a Config from path.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return LoadConfigBytes(b)
}

// configWire is the exact JSON shape LoadConfigBytes decodes: Config plus the
// legacy ado_grants list an older control plane writes in place of ado_grant
// (read here and nowhere else). Named — rather than the equivalent anonymous
// struct inline in LoadConfigBytes — so config_keys_test.go's configKeyPaths
// can walk this same type: the golden key set it pins is the sidecar's
// STRICT-decode key set (DisallowUnknownFields refuses anything outside it),
// and that set includes ado_grants, not just Config's own fields.
type configWire struct {
	Config
	LegacyADOGrants []ADOGrantConfig `json:"ado_grants"`
}

// LoadConfigBytes parses and validates a Config from raw JSON. Used by the
// sidecar's env-var config path (WARDYN_PROXY_CONFIG_JSON), which is how the
// docker driver delivers the run's policy without managing host files.
//
// The decode is STRICT (DisallowUnknownFields), matching the decodeStrict
// posture the control plane's own write paths already use.
//
// TRUST BOUNDARY (read before relaxing): the sidecar image is pinned by
// the OPERATOR, independently of wardynd (WARDYN_PROXY_IMAGE, k8s.proxyImage,
// and the shipped desktop examples pin it by DIGEST), so a config written by a
// NEWER control plane routinely meets an OLDER proxy binary. A lenient
// json.Unmarshal accepted such a config with err == nil and silently discarded
// every key the old binary did not know — and the keys this release added are
// exactly the ones a private-endpoint estate depends on
// (upstream_proxy_no_proxy, trusted_ca_pem, internal_hosts, llm_upstreams,
// pat_grants). The failure mode was therefore: the corp CA never added, the
// bypass list inert, the internal-host lift never firing — an operator's
// routing document half-honoured, with no error, no warning and no version
// handshake anywhere. A key this binary cannot honour must fail the sidecar's
// startup loudly instead of being dropped on the floor.
func LoadConfigBytes(b []byte) (*Config, error) {
	var raw configWire
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c := raw.Config
	switch {
	case len(raw.LegacyADOGrants) > 1:
		return nil, fmt.Errorf("config: ado_grants carries %d grants; a sidecar holds one Azure DevOps grant", len(raw.LegacyADOGrants))
	case len(raw.LegacyADOGrants) == 1 && c.ADOGrant != nil:
		return nil, fmt.Errorf("config: ado_grants and ado_grant are both set; a sidecar holds one Azure DevOps grant")
	case len(raw.LegacyADOGrants) == 1:
		c.ADOGrant = &raw.LegacyADOGrants[0]
	}
	if err := validPATGrants(c.PATGrants); err != nil {
		return nil, err
	}
	if err := types.ValidatePushRuleSets(c.PushRuleSets); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := types.ValidateHarnessToolRules(c.HarnessToolRules); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.applyDefaultsAndValidate(); err != nil {
		return nil, err
	}
	c.Attribution = reprojectAttribution(c.Attribution)
	return &c, nil
}

// ValidUpstreamProxyURL reports whether raw is an upstream/corp proxy URL this
// sidecar's own loader will ACCEPT — the write-time delegate for
// UpstreamProxyURL that ValidNoProxyEntry already is for UpstreamProxyNoProxy,
// and for the same reason: a second copy of the rule in the API package is a
// dual matcher over one operator-authored field, and this one drifted. The
// control plane checked only url.Parse + the http scheme, so a port of `0` or
// `99999` was persisted with 200 OK and then failed applyDefaultsAndValidate
// below at container start, which cmd/wardyn-proxy/main.go turns into
// os.Exit(1) — the egress sidecar of EVERY dispatched run, killed by a value
// the write path said was fine.
//
// It IS parseUpstreamProxy — the very rule applyDefaultsAndValidate runs below —
// exported so the control plane can apply THE SAME rule at write time
// (validateSiteConfig) and at dispatch (loadableUpstreamProxyURL,
// internal/api/runs_bedrock.go) instead of keeping a second, narrower copy.
//
// The error is returned rather than a bool so the caller can name the real
// cause; parseUpstreamProxy never echoes the raw URL (it may carry
// user:pass credentials), so the message is always safe to surface. The empty
// string is valid (upstream disabled), matching parseUpstreamProxy.
func ValidUpstreamProxyURL(raw string) error {
	_, err := parseUpstreamProxy(raw)
	return err
}

func (c *Config) applyDefaultsAndValidate() error {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.DecisionBufferSize <= 0 {
		c.DecisionBufferSize = defaultBufferSize
	}
	if c.RunID == uuid.Nil {
		return fmt.Errorf("config: run_id is required")
	}
	if c.ControlPlaneURL == "" {
		return fmt.Errorf("config: control_plane_url is required")
	}
	if c.RunToken == "" {
		return fmt.Errorf("config: run_token is required")
	}
	if err := hoptls.CheckURL(c.ControlPlaneURL); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.ControlPlaneURL)), "https://") && c.ControlPlaneCAPEM == "" {
		return fmt.Errorf("config: an https control_plane_url needs control_plane_ca_pem — control-plane calls trust wardynd's internal CA only, never the system roots")
	}
	if _, err := hoptls.ClientConfig(c.ControlPlaneCAPEM); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// Validate (but do not retain) the upstream proxy URL: fail fast on a bad
	// scheme/host/port. The live proxy re-parses it in NewServer.
	if _, err := parseUpstreamProxy(c.UpstreamProxyURL); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// Parse-check each bypass entry, same "validate at load, build for real in
	// NewServer" split as the upstream proxy URL above. compileNoProxy DROPS an
	// unparseable entry rather than widening the list, so without this a typo'd
	// suffix would silently mean "still proxied" — the failure mode this whole
	// field exists to end. The write-time validator (internal/api) rejects the
	// same shapes, so a failure here means a config authored outside that path.
	for i, e := range c.UpstreamProxyNoProxy {
		if !ValidNoProxyEntry(e) {
			return fmt.Errorf("config: upstream_proxy_no_proxy[%d]: %q is neither a CIDR nor a host/domain suffix", i, e)
		}
	}
	// Warn (never fail boot — the operator may genuinely want the portal
	// proxied) when a corp upstream is configured and this run carries an AWS
	// SSO injection host (isAWSSSOPortalHost) with no bypass entry covering
	// it: every SSO call on that host then CONNECTs through the corporate
	// upstream, which a corp forward proxy frequently cannot reach (it is a
	// PrivateLink-style host resolving into CGNAT/private space — see
	// mitm_upstream_test.go) and which times out looking exactly like a
	// network fault instead of a routing gap the operator can name. Shares
	// noProxyRulesCoverHost with the live routing decision (bypassUpstream)
	// so the two can never disagree about what counts as covered.
	if c.UpstreamProxyURL != "" {
		rules := compileNoProxy(c.UpstreamProxyNoProxy)
		for _, inj := range c.Injection {
			h := strings.ToLower(strings.TrimSpace(inj.Host))
			if h == "" || !isAWSSSOPortalHost(h) {
				continue
			}
			if !noProxyRulesCoverHost(rules, h) {
				slog.Warn("wardyn-proxy: upstream proxy is configured with an AWS SSO injection host not covered by any upstream_proxy_no_proxy entry — every SSO call on this host will CONNECT through the corporate upstream; "+
					"a bypass entry helps only if wardyn-proxy itself (not the sandbox) can resolve and reach the host; if neither hop can, the estate needs a route, not a configuration change",
					slog.String("host", inj.Host))
			}
		}
	}
	// Validate (but do not retain) the trusted CA PEM, same shape as the
	// upstream proxy URL above: fail fast on garbage. NewServer builds and
	// RETAINS the real pool (system roots + this bundle) for the live proxy.
	if c.TrustedCAPEM != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(c.TrustedCAPEM)) {
		return fmt.Errorf("config: trusted_ca_pem does not contain a valid PEM certificate")
	}
	// Parse-check (but do not retain a compiled form) each declared internal
	// host's CIDRs: the persisted site-config already enforced "inside
	// ipguard.Liftable" at write time (validateInternalHosts, internal/api), so
	// a failure here means a config file authored outside that path. Fail
	// closed rather than silently drop a malformed entry.
	for i, h := range c.InternalHosts {
		for j, cidr := range h.CIDRs {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				return fmt.Errorf("config: internal_hosts[%d].cidrs[%d]: %q: %w", i, j, cidr, err)
			}
			if !slices.ContainsFunc(ipguard.Liftable, func(l netip.Prefix) bool {
				return l.Bits() <= prefix.Bits() && l.Contains(prefix.Addr())
			}) {
				return fmt.Errorf("config: internal_hosts[%d].cidrs[%d]: %q must lie inside RFC1918, fc00::/7 or 100.64.0.0/10", i, j, cidr)
			}
		}
	}
	if err := c.validateTerminatedGates(); err != nil {
		return err
	}
	// Parse-check (but do not retain a compiled form) each configured LLM
	// gateway base URL: api.ValidateLLMGateways already fail-fast-checked these
	// at boot without retaining a parsed form (same "validate at load, build
	// for real here" split as TrustedCAPEM/UpstreamProxyURL above); a failure
	// here means a config authored outside that path.
	for vendor, raw := range c.LLMUpstreams {
		if _, err := url.Parse(raw); err != nil {
			return fmt.Errorf("config: llm_upstreams[%q]: %w", vendor, err)
		}
	}
	return c.validateChannelHosts()
}

// validateTerminatedGates checks the gates that run only on a connection the proxy terminates. An
// Azure DevOps grant is enforced by the REST gate; without the MITM CA nothing terminates, and the
// covered hosts would degrade to a credential-less tunnel no gate sees — so a config carrying ado_grant
// without the CA is refused at boot. The Azure route gate has the same rule and its own checks.
func (c *Config) validateTerminatedGates() error {
	if c.ADOGrant != nil && (c.MITMCACertPEM == "" || c.MITMCAKeyPEM == "") {
		return fmt.Errorf("config: ado_grant requires mitm_ca_cert_pem and mitm_ca_key_pem — the Azure DevOps gate runs only on a terminated connection")
	}
	for host, g := range c.PATGrants {
		if g.API && (c.MITMCACertPEM == "" || c.MITMCAKeyPEM == "") {
			return fmt.Errorf("config: pat_grants[%q] sets api and requires mitm_ca_cert_pem and mitm_ca_key_pem — the forge API door runs only on a terminated connection", host)
		}
	}
	return c.validateAzureGates()
}

// PATGrant is one host's git_pat brokering: which grant to mint from, and the
// git username that host expects alongside the PAT (Azure DevOps wants "pat",
// GitLab wants "oauth2", and an operator may override either).
//
// The narrowing fields carry the grant's scope for the run, not for the PAT:
// the credential keeps whatever reach its issuer gave it, and the broker refuses
// a request outside this scope before it mints. Dispatch sets them only for a
// narrowed grant, so an unnarrowed grant renders exactly as it did before and
// meets an older proxy image unchanged.
//
//	Repos   nil = every repository the PAT reaches; an empty list = none
//	Access  "read" refuses a push; "" is write
//	Forge   the path table Repos is read with; "" is generic
//	API     the forge API door (read by the API gate, not by git)
type PATGrant struct {
	GrantID  uuid.UUID `json:"grant_id"`
	Username string    `json:"username,omitempty"`
	Repos    *[]string `json:"repos,omitempty"`
	Access   string    `json:"access,omitempty"`
	Forge    string    `json:"forge,omitempty"`
	API      bool      `json:"api,omitempty"`
}
