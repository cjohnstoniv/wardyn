// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
)

// llmGatewayPublicHosts pairs each supported vendor's WARDYN_*_BASE_URL knob
// with the public host it re-points, in validation order.
var llmGatewayPublicHosts = []struct {
	env, envVar, publicHost string
}{
	{"anthropic", "WARDYN_ANTHROPIC_BASE_URL", "api.anthropic.com"},
	{"openai", "WARDYN_OPENAI_BASE_URL", "api.openai.com"},
}

// ValidateLLMGateways validates the two operator-set internal-model-gateway
// knobs (boot posture, control-plane-authored — the sandbox cannot set these)
// and returns api.Config.LLMGateways: public vendor host -> the gateway's
// normalized base URL. Both empty => nil map, byte-identical to today. Fail
// closed on any rule violation (the WARDYN_SUBSCRIPTION_INJECT/agentImagesJSON
// precedent) — an operator-typed posture that doesn't parse must refuse boot,
// not silently fall back to the public host.
func ValidateLLMGateways(anthropicRaw, openaiRaw string) (map[string]string, error) {
	raws := map[string]string{"api.anthropic.com": anthropicRaw, "api.openai.com": openaiRaw}
	out := make(map[string]string, 2)
	for _, e := range llmGatewayPublicHosts {
		raw := strings.TrimSpace(raws[e.publicHost])
		if raw == "" {
			continue
		}
		norm, err := validateOneLLMGateway(e.publicHost, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.envVar, err)
		}
		out[e.publicHost] = norm
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
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
// the value is the operator's own either way, and PF-44's ceiling already says
// one data-plane host per deployment.
func ValidateBedrockBaseURL(raw, region string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	norm, err := validateOneLLMGateway(bedrockRuntimeHost(region), raw)
	if err != nil {
		return "", fmt.Errorf("WARDYN_BEDROCK_BASE_URL: %w", err)
	}
	return norm, nil
}

// validateOneLLMGateway enforces the gateway URL's seven rules and returns its
// normalized form (one trailing "/" trimmed; path prefix and port preserved
// otherwise) for storage in api.Config.LLMGateways.
func validateOneLLMGateway(publicHost, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	// Rule 1: https:// only — no loopback exception. forwardInspectedLLM dials
	// "https://"+host unconditionally, and a host-loopback gateway is
	// unreachable from the sandbox netns anyway.
	if u.Scheme != "https" {
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
	if ip := net.ParseIP(host); ip != nil && llmGatewayIPRefused(ip) {
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

// llmGatewayIPRefused reports whether ip is a kind an internal model gateway
// can never legitimately be: loopback, link-local (the metadata address
// included), unspecified, multicast, or NAT64-embedded. RFC1918/ULA/CGNAT
// literals are NOT refused here — those are exactly the addresses an internal
// gateway is expected to live on.
func llmGatewayIPRefused(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	_, isNAT64 := ipguard.NAT64EmbeddedV4(ip)
	return isNAT64
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
