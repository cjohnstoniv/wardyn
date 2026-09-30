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

// validateOneLLMGateway enforces the gateway URL's seven rules and returns its
// normalized form (one trailing "/" trimmed; path prefix and port preserved
// otherwise). allowPlainHTTP relaxes rule 1 alone, and only a model provider's
// Bedrock base URL (validateProviderBedrock) ever passes it true (gated on
// WARDYN_ALLOW_TEST_ENDPOINTS). Every other rule below applies identically in
// both postures.
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
// already-validated base URL, for use anywhere the api-key convention's host
// matters as a policy/injector key (Policy.AllowedExactHost takes no port).
func gatewayHost(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// gatewayHostPort is gatewayHost with its port attached (default 443), or ""
// when base has no host.
func gatewayHostPort(base string) string {
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
