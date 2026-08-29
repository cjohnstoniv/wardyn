// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Config is the wardyn-proxy sidecar configuration, loaded from a JSON file
// via the -config flag. The run token authenticates the sidecar to the
// control plane's internal endpoints (verified via identity.Provider.Verify
// with audience "wardyn-internal"); it is NOT a secret usable outside the
// platform. No third-party secrets ever appear here — injected credentials
// are minted at startup from the broker and held only in proxy memory.
type Config struct {
	// RunID is the governed run this sidecar serves.
	RunID uuid.UUID `json:"run_id"`
	// ControlPlaneURL is the base URL of wardynd (e.g. "http://wardynd:8080").
	ControlPlaneURL string `json:"control_plane_url"`
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
	// Per-HOST rather than per-repo, deliberately: unlike a GitHub App
	// installation token, a PAT carries whatever scope the operator issued it
	// with and Wardyn cannot narrow it — so a per-repo key here would imply a
	// confinement the credential does not have. Empty => the route always 403s.
	PATGrants map[string]PATGrant `json:"pat_grants,omitempty"`
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
	// traverse it. Any embedded credential is held proxy-memory-only (like
	// RunToken) and masked from all decision-log/stdout output. Empty => direct
	// dial (backward-compatible).
	//
	// SOURCE vs TRANSPORT: the only source is the persisted site-config
	// (upstream_proxy_secret_ref, admin-authored via PUT /api/v1/site-config).
	// It is resolved per dispatch by resolveRunUpstreamProxy
	// (internal/api/runs_dispatch.go) through resolveUpstreamProxyURL (which
	// lives in internal/api/runs_bedrock.go), audited as
	// run.upstream_proxy.resolve on resolve success/failure, and merely
	// TRANSPORTED here by the run's ProxyConfig (WARDYN_PROXY_CONFIG_JSON,
	// internal/runner/docker/driver.go).
	UpstreamProxyURL string `json:"upstream_proxy_url,omitempty"`
	// TrustedCAPEM is the OPERATOR's corporate CA bundle (WARDYN_TRUSTED_CA_FILE,
	// wardynd's Config.TrustedCAPEM), forwarded verbatim per run so THIS
	// sidecar's own outbound TLS (the forward/egress transport AND the
	// control-plane transport, see NewServer) additionally trusts a corporate
	// TLS-inspecting middlebox on the path to the real upstream. Control-plane
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

// LoadConfigBytes parses and validates a Config from raw JSON. Used by the
// sidecar's env-var config path (WARDYN_PROXY_CONFIG_JSON), which is how the
// docker driver delivers the run's policy without managing host files.
func LoadConfigBytes(b []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.applyDefaultsAndValidate(); err != nil {
		return nil, err
	}
	return &c, nil
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
	// Validate (but do not retain) the upstream proxy URL: fail fast on a bad
	// scheme/host/port. The live proxy re-parses it in NewServer.
	if _, err := parseUpstreamProxy(c.UpstreamProxyURL); err != nil {
		return fmt.Errorf("config: %w", err)
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
	return nil
}

// PATGrant is one host's git_pat brokering: which grant to mint from, and the
// git username that host expects alongside the PAT (Azure DevOps wants "pat",
// GitLab wants "oauth2", and an operator may override either).
type PATGrant struct {
	GrantID  uuid.UUID `json:"grant_id"`
	Username string    `json:"username,omitempty"`
}
