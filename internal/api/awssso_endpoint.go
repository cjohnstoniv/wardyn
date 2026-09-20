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
// every derivation naming an AWS IAM Identity Center endpoint at once:
//
//	1. the dispatch egress hosts       (ssoEgressHosts, runs_bedrock.go)
//	2. the login-run egress hosts      (harnessLogin.loginEgress, harnesscred.go)
//	   — including the device.sso.<r> entry only the interactive flow adds
//	3. the CreateToken URL             (Server.awsSSOTokenEndpoint, below)
//	4. the LOGIN sandbox env           (harnessLogin.loginEnv, awssso_pin.go)
//	5. the ssoInject sandbox env       (resolveBedrockAuth, runs_bedrock.go)
//
// ONE knob for five derivations because they were five separate hardcodings of
// `amazonaws.com` in four files: a fake reachable by the egress list was
// unreachable by the SDK, and a fake the SDK could reach was denied by the
// proxy. Anything short of moving all five leaves the walk failing somewhere
// that says nothing about the code under test.
//
// Why it exists at all. Without it, "a member signs in on Kubernetes and their
// Bedrock run gets per-user credentials" is unprovable outside a real AWS
// tenant on the owner's hardware: `aws sso login` and the SDK both dial
// oidc.<region>.amazonaws.com / portal.sso.<region>.amazonaws.com, and nothing
// could redirect them into a sandbox. The two prose claims this replaces said a
// passthrough "would be a production escape hatch" — true of an UNGATED one,
// which is why this one is refused unless WARDYN_ALLOW_TEST_ENDPOINTS=true, WARNs
// at boot naming itself a test hatch, and is documented as such in
// docs/ENV.md's test-only table and threatmodel/THREAT-MODEL.md's residual list.
//
// It is DELIBERATELY NOT WARDYN_BEDROCK_BASE_URL. That knob is the Bedrock DATA
// PLANE (a real, supported PrivateLink posture); SSO is a different service,
// and one knob meaning both would make a production PrivateLink setting also
// re-point the credential exchange. It is equally not the GLOBAL
// AWS_ENDPOINT_URL, which re-points every AWS service at once.

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
// allowTestEndpoints is WARDYN_ALLOW_TEST_ENDPOINTS. A non-empty override
// without it REFUSES BOOT — the same shape as the -local-mode-with-OIDC refusal
// in cmd/wardynd's resolveLocalMode: a posture this dangerous must take two
// deliberate acts to reach, never one stray env var in a Helm values file.
//
// The rules are looser than validateOneLLMGateway's on purpose, and only in the
// two places a TEST endpoint differs from a PRODUCTION one: plain http:// is
// accepted (the fake serves no TLS, and AWS_ENDPOINT_URL_SSO* accept http —
// proven against the real AWS CLI v2 in test/awsssofake/docker.go), and a
// loopback/private address is not refused (an in-cluster Service ClusterIP is
// private by construction). Everything a malformed value could hide — a missing
// scheme, an embedded credential, a path, a query, a fragment — is still
// refused, so a typo fails at boot rather than by silently dialing the wrong
// place.
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
