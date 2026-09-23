// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/ipguard"
)

// llmGatewayPublicHosts pairs each supported vendor's WARDYN_*_BASE_URL /
// _GATEWAY_HEADER / _GATEWAY_FORMAT knobs with the public host they re-point,
// in validation order.
var llmGatewayPublicHosts = []struct {
	env, envVar, headerEnvVar, formatEnvVar, publicHost string
}{
	{"anthropic", "WARDYN_ANTHROPIC_BASE_URL", "WARDYN_ANTHROPIC_GATEWAY_HEADER", "WARDYN_ANTHROPIC_GATEWAY_FORMAT", "api.anthropic.com"},
	{"openai", "WARDYN_OPENAI_BASE_URL", "WARDYN_OPENAI_GATEWAY_HEADER", "WARDYN_OPENAI_GATEWAY_FORMAT", "api.openai.com"},
}

// LLMGatewayAuth is a provider's operator-set injection header name and value
// format override for api.Config.LLMGatewayAuth (WARDYN_<VENDOR>_GATEWAY_HEADER
// / _GATEWAY_FORMAT, validated by ValidateLLMGateways). Header and Format are
// independent: either may be set alone, and an empty field means
// llmProviderFor keeps the harness catalog's compile-time vendor convention
// for that piece — byte-identical to today unless the operator explicitly set
// one.
type LLMGatewayAuth struct {
	Header string
	Format string
}

// LLMGatewayRaw is the raw operator-typed value of one provider's three
// gateway knobs, exactly as read off the boot flags, before validation.
type LLMGatewayRaw struct {
	BaseURL string
	Header  string
	Format  string
}

// ValidateLLMGateways validates the operator-set internal-model-gateway knobs
// for both providers (boot posture, control-plane-authored — the sandbox
// cannot set these) and returns api.Config.LLMGateways (public vendor host ->
// the gateway's normalized base URL) and api.Config.LLMGatewayAuth (public
// vendor host -> header/format override, present only for a provider that set
// at least one of the two). Everything unset => (nil, nil, nil),
// byte-identical to today. Fail closed on any rule violation (the
// WARDYN_SUBSCRIPTION_INJECT/agentImagesJSON precedent) — an operator-typed
// posture that doesn't parse must refuse boot, not silently fall back to the
// public host or the vendor convention: a malformed value surfacing later at
// dial time as a confusing upstream error is exactly what this guards
// against.
func ValidateLLMGateways(anthropic, openai LLMGatewayRaw) (map[string]string, map[string]LLMGatewayAuth, error) {
	raws := map[string]LLMGatewayRaw{"api.anthropic.com": anthropic, "api.openai.com": openai}
	gateways := make(map[string]string, 2)
	auth := make(map[string]LLMGatewayAuth, 2)
	for _, e := range llmGatewayPublicHosts {
		r := raws[e.publicHost]
		base := strings.TrimSpace(r.BaseURL)
		if base != "" {
			norm, err := validateOneLLMGateway(e.publicHost, base, false)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", e.envVar, err)
			}
			gateways[e.publicHost] = norm
		}
		header := strings.TrimSpace(r.Header)
		format := strings.TrimSpace(r.Format)
		if header == "" && format == "" {
			continue
		}
		if header != "" && !egress.ValidHeaderName(header) {
			return nil, nil, fmt.Errorf("%s: %q is not a valid HTTP header token", e.headerEnvVar, header)
		}
		if format != "" {
			if err := validInjectionFormat(format); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", e.formatEnvVar, err)
			}
		}
		auth[e.publicHost] = LLMGatewayAuth{Header: header, Format: format}
	}
	if len(gateways) == 0 {
		gateways = nil
	}
	if len(auth) == 0 {
		auth = nil
	}
	return gateways, auth, nil
}

// ValidateBedrockBaseURL validates WARDYN_BEDROCK_BASE_URL — the Bedrock
// DATA-PLANE (bedrock-runtime) override that points a regulated deployment at
// its VPC/PrivateLink endpoint — and returns the normalized base URL for
// api.Config.BedrockBaseURL. Empty => ("", nil), byte-identical to today.
//
// It delegates to validateOneLLMGateway rather than growing a second rule set:
// the seven rules are already exactly right here, including the floor that
// refuses loopback/link-local/metadata/NAT64 while ALLOWING an RFC1918/CGNAT
// literal — which is precisely what a PrivateLink endpoint resolves into.
// Sharing them is also what keeps the two knobs from drifting.
//
// region supplies rule 5's "must not equal the public host" comparison
// (bedrock-runtime.<region>.amazonaws.com). It is the deployment's GLOBAL
// region: a workspace's per-run region override cannot be known at boot, and
// pointing this at the public host of some other region is not an escape —
// the value is the operator's own either way, and the ceiling is already one
// data-plane host per deployment.
//
// allowTestEndpoints (WARDYN_ALLOW_TEST_ENDPOINTS) relaxes RULE 1 and nothing
// else: a plain http:// data-plane host becomes acceptable. It exists for one
// caller — the kind SSO walk, which points this at test/awsssofake's
// bedrock-runtime stub so a member's minted role credential is actually SPENT
// by something. That stub serves no TLS, and this is the SigV4 lane, so no
// per-run TLS-MITM terminates for it; https:// was not a usable answer.
//
// Refused by default, and the refusal names BOTH variables, for exactly the
// reason ValidateAWSSSOEndpointOverride does (awssso_endpoint.go): plain HTTP to
// the model data plane is a test posture, and a posture that dangerous takes two
// deliberate acts, never one env var. With the acknowledgement unset — every
// real deployment — this function is byte-identical to before the parameter
// existed.
// BedrockPlainHTTPWarn is the BOOT WARN every boot that actually TAKES the
// plain-http relaxation logs. WARDYN_ALLOW_TEST_ENDPOINTS unlocks two
// relaxations and only the AWS SSO one was audible; this one re-points the
// BEARER-mode injection target, so the Bedrock API key rides
// `Authorization: Bearer` in cleartext on every model call — and it said
// nothing. It opens with the same greppable literal AWSSSOEndpointOverrideWarn
// does, and it names the plaintext target, because the thing that must never
// happen is this posture going unnoticed in an inherited values file.
//
// DRAFT (M2 canon pending)
const BedrockPlainHTTPWarn = "wardynd: TEST HATCH ACTIVE — WARDYN_BEDROCK_BASE_URL is plain http://, " +
	"so Bedrock inference traffic (and, in bearer mode, the API key riding it as Authorization: Bearer) " +
	"crosses the network UNENCRYPTED to this target on every model call; it is accepted only because " +
	"WARDYN_ALLOW_TEST_ENDPOINTS=true acknowledges this as a test deployment"

func ValidateBedrockBaseURL(raw, region string, allowTestEndpoints bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	// The scheme check happens HERE rather than inside validateOneLLMGateway's
	// rule 1 so the refusal can name both knobs; rule 1's own message is shared
	// with the vendor gateways, where WARDYN_ALLOW_TEST_ENDPOINTS means nothing.
	if !allowTestEndpoints && strings.HasPrefix(strings.ToLower(raw), "http://") {
		return "", fmt.Errorf("WARDYN_BEDROCK_BASE_URL: %q is plain http:// — inference traffic "+
			"(and, in bearer mode, the credential riding it) would cross the network unencrypted, so it is "+
			"refused as a production posture; use https://, or explicitly set WARDYN_ALLOW_TEST_ENDPOINTS=true "+
			"to acknowledge that this deployment is a test deployment pointed at a local stub", raw)
	}
	norm, err := validateOneLLMGateway(bedrockRuntimeHost(region), raw, allowTestEndpoints)
	if err != nil {
		return "", fmt.Errorf("WARDYN_BEDROCK_BASE_URL: %w", err)
	}
	return norm, nil
}

// validateOneLLMGateway enforces the gateway URL's seven rules and returns its
// normalized form (one trailing "/" trimmed; path prefix and port preserved
// otherwise) for storage in api.Config.LLMGateways.
// allowPlainHTTP relaxes rule 1 alone, and ONLY ValidateBedrockBaseURL ever
// passes it true (gated on WARDYN_ALLOW_TEST_ENDPOINTS — see there). Every other
// rule below applies identically in both postures.
func validateOneLLMGateway(publicHost, raw string, allowPlainHTTP bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	// Rule 1: https:// only — no loopback exception. forwardInspectedLLM dials
	// "https://"+host unconditionally, and a host-loopback gateway is
	// unreachable from the sandbox netns anyway.
	if u.Scheme != "https" && !(allowPlainHTTP && u.Scheme == "http") {
		return "", fmt.Errorf("must be https:// (got %q)", raw)
	}
	// Rule 2: no userinfo (the site_config.go upstream-proxy-URL rule).
	if u.User != nil {
		return "", fmt.Errorf("must not embed a credential (user:pass@)")
	}
	host := u.Hostname()
	// Rule 3: host non-empty.
	if host == "" {
		return "", fmt.Errorf("host is empty")
	}
	// Rule 4: an IP literal is allowed only OUTSIDE loopback/link-local/
	// metadata/unspecified/multicast/NAT64 — RFC1918/CGNAT literals ARE
	// allowed (that is the whole point of an internal gateway).
	if ip := net.ParseIP(host); ip != nil && ipguard.GatewayIPRefused(ip) {
		return "", fmt.Errorf("IP literal %q is loopback/link-local/metadata/unspecified/multicast/NAT64 — unreachable from the sandbox netns", host)
	}
	// Rule 5: must not equal the public provider host — a gateway "pointing at
	// itself" is either a no-op or a way to defeat rule 1/4 via DNS. Trim a
	// trailing "." first: "api.anthropic.com." is DNS-identical to
	// "api.anthropic.com" but EqualFold alone treats them as different hosts,
	// so this rule would pass a request that then never matches gatewayVendor
	// (whose keys, and every per-request host this proxy vets, are already
	// dot-trimmed).
	if strings.EqualFold(strings.TrimSuffix(host, "."), publicHost) {
		return "", fmt.Errorf("host must not equal the public provider host %q", publicHost)
	}
	// Rule 6: query refused.
	if u.RawQuery != "" {
		return "", fmt.Errorf("must not carry a query string")
	}
	// Rule 7: fragment refused.
	if u.Fragment != "" {
		return "", fmt.Errorf("must not carry a fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String(), nil
}

// ValidateDemoVideoBaseURL validates WARDYN_DEMO_VIDEO_BASE_URL — the base URL
// an air-gapped deployment re-points the Getting Started demo episodes at,
// since github.com is unreachable there — and returns its normalized form for
// api.Config.DemoVideoBaseURL. Empty => ("", nil), byte-identical to today
// (episodeUrl's hardcoded github.com download URL, and the CSP's two
// hardcoded GitHub hosts).
//
// It delegates to validateOneLLMGateway rather than growing a second rule
// set, with publicHost "" (rule 5, "must not equal the public provider host",
// has nothing to compare against for this knob — an empty publicHost can
// never equal a host rule 3 has already required to be non-empty, so the rule
// is a harmless no-op here) and allowPlainHTTP false (no test hatch for this
// knob): https:// only, no userinfo, non-empty host, no query, no fragment —
// the same fail-closed-at-boot posture as the model gateways.
func ValidateDemoVideoBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	norm, err := validateOneLLMGateway("", raw, false)
	if err != nil {
		return "", fmt.Errorf("WARDYN_DEMO_VIDEO_BASE_URL: %w", err)
	}
	return norm, nil
}

// gatewayHost extracts the bare host (no scheme, port, or path) from an
// already-validated api.Config.LLMGateways entry, for use anywhere the
// api-key convention's host matters as a policy/injector key
// (Policy.AllowedExactHost takes no port).
func gatewayHost(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// anthropicGatewayBase returns the operator-configured Anthropic gateway's
// validated base URL (s.cfg.LLMGateways["api.anthropic.com"]) and ok=true, or
// ("", false) when none is configured. The single place every subscription/
// managed-lane gateway consumer — dispatch's ANTHROPIC_BASE_URL, the
// injection-host allowlist, the egress precondition, and the per-run MITM host
// — resolves the gateway from, so they can never drift on which config key or
// normalization they read.
func (s *Server) anthropicGatewayBase() (string, bool) {
	base, ok := s.cfg.LLMGateways[subscriptionInjectionHost]
	return base, ok
}

// anthropicGatewayHost is anthropicGatewayBase's bare host (no scheme, port or
// path), or "" when no gateway is configured.
func (s *Server) anthropicGatewayHost() string {
	base, ok := s.anthropicGatewayBase()
	if !ok {
		return ""
	}
	return gatewayHost(base)
}

// anthropicGatewayHostPort is anthropicGatewayHost with its port attached
// (default 443, matching the proxy's own LLMUpstreams parsing) — the
// "host:port" form a per-run MITM host entry needs, mirroring how
// authorBedrockBearerInjection joins its own runtime host and port. "" when no
// gateway is configured.
func (s *Server) anthropicGatewayHostPort() string {
	base, ok := s.anthropicGatewayBase()
	if !ok {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := "443"
	if p := u.Port(); p != "" {
		port = p
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// anthropicBaseURL is the base URL subscription and Wardyn-managed runs dial:
// the operator-configured gateway when one is set, else the vendor default —
// unset is byte-identical to today ("https://" + subscriptionInjectionHost).
// The harness-login (`claude setup-token`) lane never calls this: that flow
// mints the OAuth token itself and must stay on the public host.
func (s *Server) anthropicBaseURL() string {
	if base, ok := s.anthropicGatewayBase(); ok {
		return base
	}
	return "https://" + subscriptionInjectionHost
}
