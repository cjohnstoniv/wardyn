// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/url"
	"strings"
)

// The AWS SSO endpoint override — one test-only knob
// (WARDYN_AWS_SSO_ENDPOINT_OVERRIDE, Config.AWSSSOEndpointOverride) that moves
// every derivation naming an AWS IAM Identity Center endpoint at once: the
// dispatch egress hosts (ssoEgressHosts), the login-run egress hosts
// (harnessLogin.loginEgress, including device.sso.<r>), the CreateToken URL
// (Server.awsSSOTokenEndpoint), the LOGIN sandbox env (harnessLogin.loginEnv)
// and the ssoInject sandbox env (resolveBedrockAuth). Moving fewer than all five
// leaves a fake reachable by one side and denied by the other. Gated: refused
// unless WARDYN_ALLOW_TEST_ENDPOINTS=true, WARNs at boot, and documented in
// docs/ENV.md and threatmodel/THREAT-MODEL.md. DELIBERATELY NOT
// WARDYN_BEDROCK_BASE_URL (the Bedrock DATA PLANE, a real PrivateLink posture)
// nor the global AWS_ENDPOINT_URL, which re-points every AWS service.

// DRAFT (M2 canon pending)

const (
	// AWSSSOEndpointOverrideRefusal is the BOOT REFUSAL when the override is set
	// without the acknowledgement. It names both vars because the operator
	// reading it on a crash-looping pod has to decide which one they meant. %q is
	// the offending value.
	AWSSSOEndpointOverrideRefusal = "refusing to start: WARDYN_AWS_SSO_ENDPOINT_OVERRIDE is set to %q — " +
		"it re-points AWS IAM Identity Center at a server of your choosing for the containerized login " +
		"AND for every Bedrock run's credential exchange, which is a TEST hatch and never a production posture; " +
		"unset it, or explicitly set WARDYN_ALLOW_TEST_ENDPOINTS=true to acknowledge that this deployment is a test deployment"
	// AWSSSOEndpointOverrideWarn is the BOOT WARN every boot carrying the hatch
	// logs. It opens with a literal an operator (and scripts/kind-sso-walk.sh)
	// can grep for, because the thing that must never happen is this posture
	// going unnoticed in an inherited values file.
	AWSSSOEndpointOverrideWarn = "wardynd: TEST HATCH ACTIVE — WARDYN_AWS_SSO_ENDPOINT_OVERRIDE re-points " +
		"AWS IAM Identity Center (sso-oidc AND the sso portal) at this URL for the containerized login, " +
		"for every Bedrock run's credential exchange and for dispatch-time token renewal. " +
		"No AWS SSO endpoint is contacted. This is never a production posture; unset it and " +
		"WARDYN_ALLOW_TEST_ENDPOINTS on any deployment holding a real credential."
)

// The two AWS SDK / CLI variables that re-point the SSO services — and ONLY
// those two. Named constants because five call sites and three tests spell them.
const (
	awsEndpointURLSSOEnv     = "AWS_ENDPOINT_URL_SSO"
	awsEndpointURLSSOOIDCEnv = "AWS_ENDPOINT_URL_SSO_OIDC"
)

// ValidateAWSSSOEndpointOverride validates WARDYN_AWS_SSO_ENDPOINT_OVERRIDE and
// returns the normalized base URL for Config.AWSSSOEndpointOverride. Empty =>
// ("", nil), byte-identical to a deployment that never heard of this knob.
//
// allowTestEndpoints is WARDYN_ALLOW_TEST_ENDPOINTS; a non-empty override
// without it REFUSES BOOT, so this posture takes two deliberate acts. Looser than
// validateOneLLMGateway only where a TEST endpoint differs: plain http:// is
// accepted (the fake serves no TLS; proven against the real AWS CLI v2 in
// test/awsssofake/docker.go) and a loopback/private address is not refused (a
// ClusterIP is private). A missing scheme, embedded credential, path, query or
// fragment is still refused, so a typo fails at boot.
func ValidateAWSSSOEndpointOverride(raw string, allowTestEndpoints bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !allowTestEndpoints {
		return "", fmt.Errorf(AWSSSOEndpointOverrideRefusal, raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("WARDYN_AWS_SSO_ENDPOINT_OVERRIDE: invalid URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("WARDYN_AWS_SSO_ENDPOINT_OVERRIDE: must be http:// or https:// (got %q)", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("WARDYN_AWS_SSO_ENDPOINT_OVERRIDE: must not embed a credential (user:pass@)")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("WARDYN_AWS_SSO_ENDPOINT_OVERRIDE: host is empty")
	}
	if u.RawQuery != "" {
		return "", fmt.Errorf("WARDYN_AWS_SSO_ENDPOINT_OVERRIDE: must not carry a query string")
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("WARDYN_AWS_SSO_ENDPOINT_OVERRIDE: must not carry a fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	// A PATH is refused rather than quietly carried (R-07). Both consumers treat
	// this value as a base: awsSSOTokenEndpoint appends "/token", and
	// AWS_ENDPOINT_URL_SSO* treat it as a prefix — plausibly compatible, but
	// nothing exercises it, and a silently-carried "/sso" is exactly the typo
	// this function's every other rule exists to fail closed on. The trailing
	// slash is trimmed FIRST, so "http://host:8090/" is still just a base URL.
	if u.Path != "" {
		return "", fmt.Errorf("WARDYN_AWS_SSO_ENDPOINT_OVERRIDE: must not carry a path (got %q) — it is a base URL, and the two AWS endpoint variables plus the CreateToken URL are derived from it", raw)
	}
	return u.String(), nil
}

// ssoInjectEndpointEnv is derivation 5 (and, via loginEnv, the shape of 4): the
// two SDK variables, or NOTHING when the override is unset. nil rather than an
// empty map so a caller merging it adds no key at all on a real deployment.
//
// Never AWS_ENDPOINT_URL: that is the global knob resolveBedrockAuth already
// refuses to set, because it would also re-point STS, S3 and everything
// else the sandbox can reach.
func ssoInjectEndpointEnv(override string) map[string]string {
	if override == "" {
		return nil
	}
	return map[string]string{
		awsEndpointURLSSOEnv:     override,
		awsEndpointURLSSOOIDCEnv: override,
	}
}

// awsSSOTokenEndpoint is derivation 3: the SSO-OIDC CreateToken URL a
// dispatch-time renewal POSTs to. It is the region-derived public host
// (awsSSOTokenURL, awssso_refresh.go — a package var only because tests point it
// at an httptest server) unless the override moves it.
//
// A method, not a second package var: the override is deployment configuration
// and belongs on Config, where a boot log and the ENV.md registry can name it.
func (s *Server) awsSSOTokenEndpoint(ssoRegion string) string {
	if s.cfg.AWSSSOEndpointOverride != "" {
		return s.cfg.AWSSSOEndpointOverride + "/token"
	}
	return awsSSOTokenURL(ssoRegion)
}
