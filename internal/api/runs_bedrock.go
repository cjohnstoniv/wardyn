// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// mavenProxyOpts builds the MAVEN_OPTS JVM proxy sysprops that route Maven
// through the wardyn-proxy sidecar. Maven (unlike npm/pip/cargo/go/git) ignores
// HTTP(S)_PROXY env, so without these it resolves Maven Central directly and
// fails "Unknown host" in the gatewayless sandbox. proxyURL is "http://host:port".
// nonProxyHosts excludes loopback + the proxy itself. Returns "" if unparseable.
func mavenProxyOpts(proxyURL string) string {
	s := strings.TrimSpace(proxyURL)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimRight(s, "/")
	host, port := s, "3128"
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, port = s[:i], s[i+1:]
	}
	if host == "" {
		return ""
	}
	return fmt.Sprintf(
		"-Dhttp.proxyHost=%s -Dhttp.proxyPort=%s -Dhttps.proxyHost=%s -Dhttps.proxyPort=%s "+
			"-Dhttp.nonProxyHosts=localhost|127.0.0.1|::1|wardyn-proxy",
		host, port, host, port)
}

// resolveUpstreamProxyURL resolves the operator-wide site-config upstream/corp
// proxy secret ref (types.SiteConfig.UpstreamProxySecretRef) to a URL for
// ProxyConfig.UpstreamProxyURL. getSecret resolves a secret name to its
// plaintext value (typically s.cfg.Secrets.Get); nil means no secret store is
// configured.
//
// Fail SAFE, never errors: an empty ref, a reserved platform-internal name
// (defense-in-depth — validateSiteConfig/validSecretRef already reject this at
// PUT /api/v1/site-config write time, but this guards a row written before
// that check existed, mirroring handleInternalInjection's sink-side reserved-
// name guard), a missing secret store, an unresolvable secret, or a non-http
// URL all return ("", <reason>) — the caller audits the reason and dispatches
// with direct egress instead of failing the run. A resolved http URL returns
// (url, "").
//
// Scheme is restricted to http because the sidecar's own config validation
// (parseUpstreamProxy, internal/egress/proxy/upstream.go) rejects https: the
// hop TO the corp proxy is a plaintext CONNECT + Proxy-Authorization today, and
// an https:// proxy URL would need a TLS wrap first or leak that Basic
// credential in cleartext — so an https ref is skipped here rather than
// crashing the proxy sidecar at startup.
func resolveUpstreamProxyURL(ctx context.Context, secretRef string, getSecret func(context.Context, string) ([]byte, error)) (proxyURL, failReason string) {
	if secretRef == "" {
		return "", ""
	}
	if reservedSecret(secretRef) {
		return "", "reserved-secret-name"
	}
	if getSecret == nil {
		return "", "no-secret-store"
	}
	val, err := getSecret(ctx, secretRef)
	if err != nil {
		return "", "secret-not-found"
	}
	raw := strings.TrimSpace(string(val))
	u, perr := url.Parse(raw)
	if perr != nil || !strings.EqualFold(u.Scheme, "http") {
		return "", "unsupported-scheme"
	}
	return raw, ""
}

// Bedrock: AWS Bedrock as an Anthropic transport for claude-code runs (an
// enterprise path — no direct Anthropic egress, billed via AWS). Bedrock
// authenticates with AWS SigV4 REQUEST SIGNING, not a static bearer header, so
// unlike an api_key grant (proxy-injected, never resident) the proxy has
// nothing to strip-and-replace: the AWS credentials MUST be resident in the
// sandbox env, same tradeoff already accepted for the Claude subscription
// mount above. A ~/.aws host mount (mirroring the ~/.claude subscription
// mount) is a documented alternative for a future pass; this wires the
// secret-env lane only.
//
// Region/model are OPERATOR BOOT-TIME config (BedrockRegion/BedrockModel,
// mirroring AgentAnthropicModel — no live admin write path, same as the
// WARDYN_DEFAULT_POLICY precedent); the AWS credentials are read directly
// from the secret store at dispatch time — a new kind of secret consumption
// for this codebase (every other consumer is proxy-injection-at-mint-time),
// necessary because SigV4 can't be injected after the fact.
const (
	bedrockAccessKeyIDSecret     = "aws-access-key-id"
	bedrockSecretAccessKeySecret = "aws-secret-access-key"
	bedrockSessionTokenSecret    = "aws-session-token" // optional (STS/AssumeRole creds)
	// bedrockAPIKeySecret holds an AWS Bedrock BEARER token (AWS_BEARER_TOKEN_BEDROCK).
	// Unlike SigV4 access keys, a bearer token is a STATIC Authorization header, so
	// it can be proxy-INJECTED (never resident) exactly like an api_key — the
	// preferred, higher-trust Bedrock path when present.
	bedrockAPIKeySecret = "bedrock-api-key"
)

// bedrockAuth is the resolved Bedrock authentication plan for a run.
type bedrockAuth struct {
	env         map[string]string // sandbox env additions (bearer: placeholder; resident: real creds)
	egressHosts []string          // regional data+control plane hosts to allow
	ready       bool              // false => fall back to api-key mode
	// bearer selects the never-resident path: bedrock-runtime is TLS-MITM'd and the
	// Authorization: Bearer header is injected proxy-side from bedrockAPIKeySecret,
	// so the sandbox holds only a placeholder. When false (resident path), AWS SigV4
	// creds are placed in env (SigV4 can't be proxy-injected).
	bearer      bool
	runtimeHost string // bedrock-runtime host (MITM+inject target in bearer mode)
	// awsMount selects the host-mode ~/.aws bind-mount path: the SDK resolves
	// credentials (incl. auto-refreshing AWS SSO) from the read-only mount, so no
	// static keys are stored and none are resident in env. awsMountSource is the
	// host dir to bind read-only at /home/agent/.aws. Mutually exclusive with the
	// resident-key path; bearer still wins over it.
	awsMount       bool
	awsMountSource string
}

// bedrockRuntimeHost is the regional Bedrock DATA-PLANE host claude-code's
// InvokeModel/Converse calls hit. bedrockControlHost is the companion
// CONTROL-PLANE host claude-code also calls (bedrock:ListInferenceProfiles /
// GetInferenceProfile) to resolve a cross-region inference-profile model id —
// omitting it from egress 403s a profile-id model, so both hosts are required,
// not just the data-plane one.
func bedrockRuntimeHost(region string) string {
	return fmt.Sprintf("bedrock-runtime.%s.amazonaws.com", region)
}

func bedrockControlHost(region string) string {
	return fmt.Sprintf("bedrock.%s.amazonaws.com", region)
}

// ssoEgressHosts are the AWS IAM Identity Center (SSO) endpoints the sandbox SDK
// must reach to exchange a cached SSO token for role credentials: oidc.<r> for
// token refresh and portal.sso.<r> for GetRoleCredentials. Only needed on the
// ~/.aws-mount path; region is the SSO region (may differ from the Bedrock one).
func ssoEgressHosts(ssoRegion string) []string {
	return []string{
		fmt.Sprintf("oidc.%s.amazonaws.com", ssoRegion),
		fmt.Sprintf("portal.sso.%s.amazonaws.com", ssoRegion),
	}
}

// sandboxAWSDir is where the host ~/.aws is bind-mounted read-only in the run.
const sandboxAWSDir = "/home/agent/.aws"

// resolveBedrockAuth decides whether this run should authenticate to Claude via
// Amazon Bedrock and, if so, returns the sandbox env additions (the
// CLAUDE_CODE_USE_BEDROCK on-switch, region, model id, and resident AWS creds)
// plus the regional egress hosts to allow. ready is false whenever Bedrock isn't
// configured (region/model unset — the common case for non-Bedrock operators)
// OR is misconfigured (region/model set but the AWS credential secrets aren't
// both present) — either way the caller falls back to the existing api-key
// path, so a partial Bedrock config never breaks a run, it just doesn't get
// Bedrock. subscriptionActive pre-empts Bedrock: the resident Claude OAuth
// mount and Bedrock are mutually exclusive Anthropic transports. modelRun=false
// (a verify or scan run that makes no model call) also returns ready=false, so
// the resident AWS creds never land in a sandbox that won't sign a Bedrock request.
func (s *Server) resolveBedrockAuth(ctx context.Context, runAgent string, subscriptionActive, modelRun bool) bedrockAuth {
	if !modelRun || subscriptionActive || runAgent != "claude-code" ||
		s.cfg.BedrockRegion == "" || s.cfg.BedrockModel == "" || s.cfg.Secrets == nil {
		return bedrockAuth{}
	}
	runtimeHost := bedrockRuntimeHost(s.cfg.BedrockRegion)
	hosts := []string{runtimeHost, bedrockControlHost(s.cfg.BedrockRegion)}
	// Common Bedrock env: the on-switch, region, and model id. AWS_REGION is what
	// claude-code reads; AWS_DEFAULT_REGION is the broader AWS-SDK fallback. The
	// model id is a cross-region INFERENCE-PROFILE id (e.g.
	// "us.anthropic.claude-sonnet-4-5-...") or an application-inference-profile ARN
	// — NOT a bare foundation-model id (Bedrock silently rewrites those and can 403
	// under an SCP). Operator-supplied; Wardyn does not validate the format.
	base := func() map[string]string {
		return map[string]string{
			"CLAUDE_CODE_USE_BEDROCK": "1",
			"AWS_REGION":              s.cfg.BedrockRegion,
			"AWS_DEFAULT_REGION":      s.cfg.BedrockRegion,
			"ANTHROPIC_MODEL":         s.cfg.BedrockModel,
		}
	}

	// PREFERRED: bearer-token mode. A Bedrock API key is a STATIC Authorization
	// header, so the proxy TLS-MITMs bedrock-runtime and injects it — the sandbox
	// holds only a placeholder, never the real token (trust parity with api-key /
	// subscription). Selected whenever a bedrock-api-key secret exists.
	if bearer, berr := s.cfg.Secrets.Get(ctx, bedrockAPIKeySecret); berr == nil && len(bearer) > 0 {
		env := base()
		// A non-empty sentinel so claude-code uses bearer auth (not SigV4); the proxy
		// overwrites the Authorization header with the real token on the wire.
		env["AWS_BEARER_TOKEN_BEDROCK"] = "wardyn-proxy-injected"
		return bedrockAuth{env: env, egressHosts: hosts, ready: true, bearer: true, runtimeHost: runtimeHost}
	}

	// HOST-MODE ~/.aws MOUNT: bind the operator's host ~/.aws read-only into the
	// sandbox and let the AWS SDK resolve credentials itself — including AWS SSO /
	// IAM Identity Center sessions it refreshes on demand, so a short-lived login
	// never goes stale and nothing is stored in Wardyn. No resident static keys.
	// Opt-in + host-mode-only (BedrockAWSConfigDir is set only by run-host.sh /
	// setup.sh); fail SAFE to the next path if the dir doesn't exist on this host.
	if s.cfg.BedrockAWSConfigDir != "" {
		if st, err := os.Stat(s.cfg.BedrockAWSConfigDir); err == nil && st.IsDir() {
			env := base()
			// Point the SDK at the mount explicitly (robust even if HOME isn't
			// /home/agent for some exec path); no AWS_ACCESS_KEY_ID — the SDK
			// resolves from the mounted config + SSO cache.
			env["AWS_CONFIG_FILE"] = sandboxAWSDir + "/config"
			env["AWS_SHARED_CREDENTIALS_FILE"] = sandboxAWSDir + "/credentials"
			if s.cfg.BedrockAWSProfile != "" {
				env["AWS_PROFILE"] = s.cfg.BedrockAWSProfile
			}
			ssoRegion := cmp.Or(s.cfg.BedrockAWSSSORegion, s.cfg.BedrockRegion)
			hosts = append(hosts, ssoEgressHosts(ssoRegion)...)
			return bedrockAuth{env: env, egressHosts: hosts, ready: true,
				awsMount: true, awsMountSource: s.cfg.BedrockAWSConfigDir}
		}
	}

	// FALLBACK: resident SigV4 access keys. SigV4 signs each request in-process, so
	// the creds MUST be resident in the sandbox env (documented exception, masked +
	// modelRun-gated). Requires both access key + secret key.
	accessKey, aerr := s.cfg.Secrets.Get(ctx, bedrockAccessKeyIDSecret)
	secretKey, serr := s.cfg.Secrets.Get(ctx, bedrockSecretAccessKeySecret)
	if aerr != nil || serr != nil || len(accessKey) == 0 || len(secretKey) == 0 {
		return bedrockAuth{}
	}
	env := base()
	env["AWS_ACCESS_KEY_ID"] = string(accessKey)
	env["AWS_SECRET_ACCESS_KEY"] = string(secretKey)
	if tok, terr := s.cfg.Secrets.Get(ctx, bedrockSessionTokenSecret); terr == nil && len(tok) > 0 {
		env["AWS_SESSION_TOKEN"] = string(tok)
	}
	return bedrockAuth{env: env, egressHosts: hosts, ready: true}
}
