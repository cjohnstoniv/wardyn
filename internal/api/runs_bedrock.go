// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
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
// proxy to a URL for ProxyConfig.UpstreamProxyURL, preferring the plain
// plainURL (types.SiteConfig.UpstreamProxyURL) when set, else falling back to
// resolving secretRef (types.SiteConfig.UpstreamProxySecretRef) exactly as
// before the plain-URL field existed. getSecret resolves a secret name to its
// plaintext value (typically s.cfg.Secrets.Get); nil means no secret store is
// configured.
//
// Fail SAFE, never errors: an empty plainURL AND empty secretRef, a reserved
// platform-internal secret name (defense-in-depth — validateSiteConfig/
// validSecretRef already reject this at PUT /api/v1/site-config write time,
// but this guards a row written before that check existed, mirroring
// handleInternalInjection's sink-side reserved-name guard), a missing secret
// store, an unresolvable secret, or a non-http URL (from EITHER source) all
// return ("", <reason>) — the caller audits the reason and dispatches with
// direct egress instead of failing the run. A resolved http URL returns
// (url, "").
//
// Scheme is restricted to http for BOTH sources because the sidecar's own
// config validation (parseUpstreamProxy, internal/egress/proxy/upstream.go)
// rejects https: the hop TO the corp proxy is a plaintext CONNECT +
// Proxy-Authorization today, and an https:// proxy URL would need a TLS wrap
// first or leak that Basic credential in cleartext — so an https value is
// skipped here rather than crashing the proxy sidecar at startup.
func resolveUpstreamProxyURL(ctx context.Context, plainURL, secretRef string, getSecret func(context.Context, string) ([]byte, error)) (proxyURL, failReason string) {
	if plainURL != "" {
		if raw, ok := normalizedHTTPProxyURL(plainURL); ok {
			return raw, ""
		}
		return "", "unsupported-scheme"
	}
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
	if raw, ok := normalizedHTTPProxyURL(string(val)); ok {
		return raw, ""
	}
	return "", "unsupported-scheme"
}

// normalizedHTTPProxyURL trims raw and reports (trimmed, true) when it parses
// as an http-scheme URL, else ("", false). Shared by both resolveUpstreamProxyURL
// sources so the scheme restriction can never drift between them.
func normalizedHTTPProxyURL(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return "", false
	}
	return trimmed, true
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
// Region/model default to OPERATOR BOOT-TIME config (BedrockRegion/BedrockModel,
// mirroring AgentAnthropicModel — no live admin write path, same as the
// WARDYN_DEFAULT_POLICY precedent) and are overridden per-run by the picked
// workspace/container's Bedrock binding (types.WorkspaceBedrockRef); the AWS
// credentials themselves stay global and are read directly
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

// bedrockGlobalSecretNames is the closed set of secret names resolveBedrockAuth
// actually reads (bug-integrations-2): a Bedrock Integration row's own Secrets
// entries are validated against this set at write time (validateIntegrationWrite)
// so a renamed/invented secret_name is rejected up front instead of silently
// orphaning the stored secret behind a field nothing reads.
var bedrockGlobalSecretNames = map[string]bool{
	bedrockAccessKeyIDSecret:     true,
	bedrockSecretAccessKeySecret: true,
	bedrockSessionTokenSecret:    true,
	bedrockAPIKeySecret:          true,
}

// bedrockGlobalSecretNamesList is bedrockGlobalSecretNames rendered for the
// validateIntegrationWrite error message — a fixed, sorted literal (not a
// map-iteration-order-dependent join) since Go map order is unspecified.
const bedrockGlobalSecretNamesList = `"` + bedrockAPIKeySecret + `", "` + bedrockAccessKeyIDSecret + `", "` + bedrockSecretAccessKeySecret + `", "` + bedrockSessionTokenSecret + `"`

// bedrockAuth is the resolved Bedrock authentication plan for a run.
type bedrockAuth struct {
	env         map[string]string // sandbox env additions (bearer: placeholder; resident: real creds)
	egressHosts []string          // regional data+control plane hosts to allow
	ready       bool              // false => fall back to api-key mode
	// bearer selects the never-resident path: bedrock-runtime is TLS-MITM'd and the
	// Authorization: Bearer header is injected proxy-side from bedrockAPIKeySecret,
	// so the sandbox holds only a placeholder. When false (resident path), AWS SigV4
	// creds are placed in env (SigV4 can't be proxy-injected).
	bearer bool
	// runtimeHost is the EFFECTIVE Bedrock data-plane host this run resolved
	// (bedrockDataPlaneHost: the WARDYN_BEDROCK_BASE_URL override's host when
	// set, else the regional public one). Audited by applyBedrockTransport in
	// every mode; in bearer mode it is additionally the TLS-MITM and
	// Authorization-injection target.
	runtimeHost string
	// awsMount selects the host-mode ~/.aws bind-mount path: the SDK resolves
	// credentials (incl. auto-refreshing AWS SSO) from the read-only mount, so no
	// static keys are stored and none are resident in env. awsMountSource is the
	// host dir to bind read-only at /home/agent/.aws. Mutually exclusive with the
	// resident-key path; bearer still wins over it.
	awsMount       bool
	awsMountSource string
	// region/model are the EFFECTIVE selection this run resolved (a workspace
	// binding's override, else the global operator config) — audited by
	// applyBedrockTransport so the record names what the run actually used.
	region, model string
	// ssoInject selects the captured-AWS-SSO-credential delivery path (a
	// container-login `aws sso login` — see awsSSOBlob in harnesscred.go): a
	// minimal synthetic ~/.aws is materialized in the sandbox from an env var
	// (no host mount, no static keys stored). See the resolveBedrockAuth
	// residency note for why this differs from bearer. Mutually exclusive with
	// awsMount and the resident-key path; bearer still wins over it.
	ssoInject bool
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

// bedrockDataPlaneHost is THE data-plane host every Bedrock consumer derives:
// the operator's WARDYN_BEDROCK_BASE_URL override when set (a VPC/PrivateLink
// endpoint), else the regional public host. It exists because four call sites
// derive that host independently (resolveBedrockAuth's egress + MITM host, the
// workspace-integration egress union in llmcred.go, the record-mode skip list
// in record.go) — deriving it in one place is what makes the egress allowlist,
// the MITM host, the bearer-injection scope and the record-mode skip list all
// follow the override for free instead of four times.
//
// The CONTROL plane (bedrockControlHost) is deliberately NOT overridden: a
// PrivateLink endpoint is per-SERVICE, and bedrock-runtime and bedrock are two
// services. See Config.BedrockBaseURL for the PF-44 ceiling this implies.
func (s *Server) bedrockDataPlaneHost(region string) string {
	if h := gatewayHost(s.cfg.BedrockBaseURL); h != "" {
		return h
	}
	return bedrockRuntimeHost(region)
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

// sandboxAWSDir is where the host ~/.aws is bind-mounted read-only in the run
// (awsMount) OR materialized by agent-run from an env var (ssoInject) — same
// path either way, so the SDK env vars below don't need to branch on which.
const sandboxAWSDir = "/home/agent/.aws"

// awsSSOProfileName is both the generated [sso-session <name>] and
// [profile <name>] name for a captured SSO credential (ssoInject). Fixed, not
// operator-configured: this profile exists only inside the ephemeral sandbox
// ~/.aws Wardyn generates, so there is no collision to name around.
const awsSSOProfileName = "wardyn"

// awsSSOConfigEnvVar carries the generated ~/.aws files (config + SSO token
// cache) into the sandbox: same shape as WARDYN_ARTIFACT_CONFIG_B64 —
// newline-delimited "<home-relative-path>\t<base64(content)>" records (see
// encodeArtifactConfig, reused here as-is). agent-run-lib.sh materializes it
// before claude/aws-sdk runs (materialize_aws_sso_config, in
// deploy/images/common/agent-run-lib.sh).
const awsSSOConfigEnvVar = "WARDYN_AWS_SSO_CONFIG_B64"

// awsSSOCacheFileName is the AWS CLI/SDK's cache-filename convention for an
// sso-session-based profile: the SHA1 hex digest of the session name (a
// LEGACY non-session profile instead hashes the start URL — irrelevant here
// since the generated config always uses an sso-session block, chosen so one
// name derives both the config block and the cache file unambiguously).
func awsSSOCacheFileName(sessionName string) string {
	sum := sha1.Sum([]byte(sessionName))
	return hex.EncodeToString(sum[:])
}

// awsSSOConfigFileContents generates a minimal ~/.aws/config binding the
// captured SSO session to a single profile the sandbox SDK resolves against.
func awsSSOConfigFileContents(b awsSSOBlob) string {
	return fmt.Sprintf(
		"[sso-session %[1]s]\nsso_start_url = %[2]s\nsso_region = %[3]s\nsso_registration_scopes = sso:account:access\n\n"+
			"[profile %[1]s]\nsso_session = %[1]s\nsso_account_id = %[4]s\nsso_role_name = %[5]s\noutput = json\n",
		awsSSOProfileName, b.StartURL, b.Region, b.AccountID, b.RoleName,
	)
}

// awsSSOLoginConfigFileContents generates the PRE-login half of the same
// ~/.aws/config: only the [sso-session] block, no [profile] and no token cache.
// It is what the containerized `aws sso login --sso-session wardyn` needs to
// exist BEFORE it runs (the CLI reads sso_start_url/sso_region from it); the
// account/role and the token are outputs of that login, not inputs.
//
// Non-secret by construction — the start URL and region are operator
// configuration, which is why the login sandbox may hold them (it must hold no
// credential; it exists to obtain one).
func awsSSOLoginConfigFileContents(startURL, region string) string {
	return fmt.Sprintf(
		"[sso-session %s]\nsso_start_url = %s\nsso_region = %s\nsso_registration_scopes = sso:account:access\n",
		awsSSOProfileName, startURL, region,
	)
}

// awsSSOCacheFileContents generates the SSO token-cache JSON the AWS SDK reads
// from ~/.aws/sso/cache/<awsSSOCacheFileName>.json: accessToken/expiresAt
// (RFC3339) are always present; the refresh/registration fields ride along
// when the login also registered a public client, so the SDK can silently
// refresh instead of forcing a re-login once only the access token (not the
// client registration) has lapsed.
func awsSSOCacheFileContents(b awsSSOBlob) string {
	cache := map[string]any{
		"startUrl":    b.StartURL,
		"region":      b.Region,
		"accessToken": b.AccessToken,
		"expiresAt":   b.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if b.RefreshToken != "" {
		cache["refreshToken"] = b.RefreshToken
	}
	if b.ClientID != "" {
		cache["clientId"] = b.ClientID
	}
	if b.ClientSecret != "" {
		cache["clientSecret"] = b.ClientSecret
	}
	if !b.RegistrationExpiresAt.IsZero() {
		cache["registrationExpiresAt"] = b.RegistrationExpiresAt.UTC().Format(time.RFC3339)
	}
	raw, _ := json.Marshal(cache)
	return string(raw)
}

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
//
// ws is the picked workspace/container's Bedrock selection (nil for a run that
// binds none): its region/model WIN over the global config, which remains the
// fallback for whatever the workspace leaves unset. The workspace never carries
// credentials — those stay operator-global (secrets / ~/.aws / captured SSO).
func (s *Server) resolveBedrockAuth(ctx context.Context, runAgent string, subscriptionActive, modelRun bool, ws *types.WorkspaceBedrockRef) bedrockAuth {
	region, model, profile := s.cfg.BedrockRegion, s.cfg.BedrockModel, s.cfg.BedrockAWSProfile
	if ws != nil {
		region = cmp.Or(strings.TrimSpace(ws.Region), region)
		model = cmp.Or(strings.TrimSpace(ws.Model), model)
	}
	if !modelRun || subscriptionActive || runAgent != "claude-code" ||
		region == "" || model == "" || s.cfg.Secrets == nil {
		return bedrockAuth{}
	}
	runtimeHost := s.bedrockDataPlaneHost(region)
	hosts := []string{runtimeHost, bedrockControlHost(region)}
	// Common Bedrock env: the on-switch, region, and model id. AWS_REGION is what
	// claude-code reads; AWS_DEFAULT_REGION is the broader AWS-SDK fallback. The
	// model id is a cross-region INFERENCE-PROFILE id (e.g.
	// "us.anthropic.claude-sonnet-4-5-...") or an application-inference-profile ARN
	// — NOT a bare foundation-model id (Bedrock silently rewrites those and can 403
	// under an SCP). Operator-supplied; Wardyn does not validate the format.
	base := func() map[string]string {
		env := map[string]string{
			"CLAUDE_CODE_USE_BEDROCK": "1",
			"AWS_REGION":              region,
			"AWS_DEFAULT_REGION":      region,
			"ANTHROPIC_MODEL":         model,
		}
		// PrivateLink data-plane override, set ONLY when the operator configured
		// one (absent = byte-identical to today). TWO variables because the four
		// credential modes below split across two clients: claude-code reads the
		// harness variable, while the three SigV4 modes route through the AWS SDK,
		// which reads its own service-specific knob.
		//
		// NEVER the global AWS_ENDPOINT_URL (PF-45): that re-points EVERY AWS
		// service this sandbox talks to — including STS and SSO, which the
		// captured-SSO and ~/.aws-mount modes below use to exchange a token for
		// role credentials. One service's private endpoint must not silently
		// become every service's.
		if s.cfg.BedrockBaseURL != "" {
			env["ANTHROPIC_BEDROCK_BASE_URL"] = s.cfg.BedrockBaseURL
			env["AWS_ENDPOINT_URL_BEDROCK_RUNTIME"] = s.cfg.BedrockBaseURL
		}
		return env
	}
	// ready return shared by every credential mode below: stamps the EFFECTIVE
	// region/model so the audit names what this run used, not the global config,
	// and the EFFECTIVE data-plane host so applyBedrockTransport's audit names
	// where the call actually went (bearer mode additionally uses it as the
	// TLS-MITM + Authorization-injection target).
	ready := func(b bedrockAuth) bedrockAuth {
		b.ready, b.region, b.model, b.runtimeHost = true, region, model, runtimeHost
		return b
	}

	// PREFERRED: bearer-token mode. A Bedrock API key is a STATIC Authorization
	// header, so the proxy TLS-MITMs bedrock-runtime and injects it — the sandbox
	// holds only a placeholder, never the real token (trust parity with api-key /
	// subscription). Selected whenever a bedrock-api-key secret exists.
	// Operator namespace on purpose (bare Get == For("")): Bedrock credentials are
	// MDM/operator-set daemon config, never a member row (writableSecretName
	// refuses these names for a non-operator).
	if bearer, berr := s.cfg.Secrets.Get(ctx, bedrockAPIKeySecret); berr == nil && len(bearer) > 0 {
		env := base()
		// A non-empty sentinel so claude-code uses bearer auth (not SigV4); the proxy
		// overwrites the Authorization header with the real token on the wire.
		env["AWS_BEARER_TOKEN_BEDROCK"] = "wardyn-proxy-injected"
		return ready(bedrockAuth{env: env, egressHosts: hosts, bearer: true})
	}

	// CAPTURED AWS SSO CREDENTIAL: a container-login `aws sso login` captured an
	// SSO access token (readAWSSSOBlob / awsSSOBlob, harnesscred.go). This wins
	// over the host ~/.aws mount and static keys below — it needs no host access
	// and stores no long-lived static key — but an explicit bearer token still
	// wins over it (bearer is never-resident; this mode is).
	//
	// RESIDENCY (contrast with bearer above): the captured SSO access token DOES
	// land resident in the sandbox — a minimal synthetic ~/.aws, delivered the
	// same way the managed-subscription sentinel is (base64 in a sandbox env
	// var, materialized by agent-run; see WARDYN_CLAUDE_MANAGED_B64 /
	// materialize_managed_claude_config in deploy/images/common/agent-run-lib.sh
	// for the precedent this mirrors). The sandbox SDK then exchanges that token
	// for SHORT-LIVED role credentials itself (portal.sso.<region>
	// GetRoleCredentials) — those role credentials were always going to be
	// resident (SigV4 can't be proxy-injected, see the resident-key fallback
	// below); what's new here is the longer-lived SSO access token also being
	// resident, not just the ephemeral role creds it mints.
	//
	// PHASE B (not yet built): proxy-inject the token as the
	// `x-amz-sso_bearer_token` header on portal.sso.<region> instead of writing
	// it into the sandbox — that call is authtype:none (unsigned), so a MITM can
	// set the header without the sandbox ever holding the token, mirroring the
	// Bedrock bearer path above. Until Phase B ships, this is an accepted,
	// documented tradeoff (same class as the resident-SigV4 fallback), not an
	// oversight.
	if blob, found, berr := s.readAWSSSOBlob(ctx); berr == nil && found {
		if blob.expired(s.cfg.Now()) {
			// Observable so the UI can tell the operator to re-login (setup status
			// reads the same readAWSSSOBlob + expired() this checks); fall through to
			// the next credential mode rather than handing the run a dead token.
			slog.WarnContext(ctx, "wardynd: captured AWS SSO credential expired; falling back to the next Bedrock credential mode",
				slog.Time("expired_at", blob.ExpiresAt))
		} else {
			env := base()
			env["AWS_CONFIG_FILE"] = sandboxAWSDir + "/config"
			// Deliberately not materialized: a missing shared-credentials file is
			// normal ("no static creds") and every AWS SDK treats it that way, which
			// is exactly right here — the only credential source is the SSO cache.
			env["AWS_SHARED_CREDENTIALS_FILE"] = sandboxAWSDir + "/credentials"
			env["AWS_PROFILE"] = awsSSOProfileName
			env[awsSSOConfigEnvVar] = encodeArtifactConfig(map[string]string{
				".aws/config": awsSSOConfigFileContents(blob),
				".aws/sso/cache/" + awsSSOCacheFileName(awsSSOProfileName) + ".json": awsSSOCacheFileContents(blob),
			})
			hosts = append(hosts, ssoEgressHosts(blob.Region)...)
			// Mask GLOBALLY (not per-run, like the static-key branch below does via
			// the caller): this captured credential is reused across every run that
			// picks this mode, not minted fresh per run, so a per-run Add would miss
			// every run after the first. Mirrors handleHarnessCredentialPaste
			// (harnesscred.go). AddGlobal no-ops on the empty strings when a field
			// wasn't captured (Registry.MinLen).
			s.cfg.MaskRegistry.AddGlobal([]byte(blob.AccessToken))
			s.cfg.MaskRegistry.AddGlobal([]byte(blob.RefreshToken))
			s.cfg.MaskRegistry.AddGlobal([]byte(blob.ClientSecret))
			return ready(bedrockAuth{env: env, egressHosts: hosts, ssoInject: true})
		}
	}

	// HOST-MODE ~/.aws MOUNT: bind the operator's host ~/.aws read-only into the
	// sandbox and let the AWS SDK resolve credentials itself — including AWS SSO /
	// IAM Identity Center sessions it refreshes on demand, so a short-lived login
	// never goes stale and nothing is stored in Wardyn. No resident static keys.
	// Opt-in via WARDYN_BEDROCK_AWS_DIR (host mode OR compose — in compose the same
	// path is bind-mounted host==container so the daemon-side sandbox mount resolves;
	// see deploy/compose/docker-compose.yaml). Fail SAFE to the next path if the dir
	// doesn't exist on this host.
	if s.cfg.BedrockAWSConfigDir != "" {
		if st, err := os.Stat(s.cfg.BedrockAWSConfigDir); err == nil && st.IsDir() {
			env := base()
			// Point the SDK at the mount explicitly (robust even if HOME isn't
			// /home/agent for some exec path); no AWS_ACCESS_KEY_ID — the SDK
			// resolves from the mounted config + SSO cache.
			env["AWS_CONFIG_FILE"] = sandboxAWSDir + "/config"
			env["AWS_SHARED_CREDENTIALS_FILE"] = sandboxAWSDir + "/credentials"
			if profile != "" {
				env["AWS_PROFILE"] = profile
			}
			// The SSO region falls back to the EFFECTIVE Bedrock region (a workspace
			// override included) — falling back to the global one would allow the
			// wrong regional oidc/portal.sso endpoints and 403 the credential
			// exchange for a workspace that moved the run to another region.
			ssoRegion := cmp.Or(s.cfg.BedrockAWSSSORegion, region)
			hosts = append(hosts, ssoEgressHosts(ssoRegion)...)
			return ready(bedrockAuth{env: env, egressHosts: hosts,
				awsMount: true, awsMountSource: s.cfg.BedrockAWSConfigDir})
		}
	}

	// FALLBACK: resident SigV4 access keys. SigV4 signs each request in-process, so
	// the creds MUST be resident in the sandbox env (documented exception, masked +
	// modelRun-gated). Requires both access key + secret key.
	// Operator namespace on purpose (bare Get == For("")): Bedrock credentials are
	// MDM/operator-set daemon config, never a member row (writableSecretName
	// refuses these names for a non-operator).
	accessKey, aerr := s.cfg.Secrets.Get(ctx, bedrockAccessKeyIDSecret)
	secretKey, serr := s.cfg.Secrets.Get(ctx, bedrockSecretAccessKeySecret)
	if aerr != nil || serr != nil || len(accessKey) == 0 || len(secretKey) == 0 {
		return bedrockAuth{}
	}
	env := base()
	env["AWS_ACCESS_KEY_ID"] = string(accessKey)
	env["AWS_SECRET_ACCESS_KEY"] = string(secretKey)
	// Operator namespace on purpose (bare Get == For("")): Bedrock credentials are
	// MDM/operator-set daemon config, never a member row (writableSecretName
	// refuses these names for a non-operator).
	if tok, terr := s.cfg.Secrets.Get(ctx, bedrockSessionTokenSecret); terr == nil && len(tok) > 0 {
		env["AWS_SESSION_TOKEN"] = string(tok)
	}
	return ready(bedrockAuth{env: env, egressHosts: hosts})
}

// SetupBedrock is the Amazon Bedrock Anthropic-transport readiness snapshot the
// wizard renders. It lives HERE, immediately below resolveBedrockAuth, because
// ready() must accept exactly the credential set that function accepts: when the
// two drifted (readiness omitted the captured-SSO lane above) the wizard told an
// operator whose runs authenticate fine that no integration could drive Claude
// Code. Keep the two lists edited together.
type SetupBedrock struct {
	Region string `json:"region,omitempty"`
	Model  string `json:"model,omitempty"`
	// The four credential SOURCES resolveBedrockAuth accepts, in its precedence
	// order (bearer > captured AWS SSO session > ~/.aws mount > resident SigV4).
	// ANY one is sufficient — a mount-, bearer- or SSO-credentialed host has NO
	// aws-access-key-id/-secret secrets yet is fully ready, so gating readiness on
	// CredsPresent alone wrongly reads "needs setup".
	CredsPresent  bool `json:"creds_present"`  // resident aws-access-key-id + aws-secret-access-key secrets
	AWSMount      bool `json:"aws_mount"`      // host-mode read-only ~/.aws bind-mount (SSO auto-refreshes)
	BearerPresent bool `json:"bearer_present"` // bedrock-api-key bearer token secret (never resident)
	SSOPresent    bool `json:"sso_present"`    // captured, NON-EXPIRED container-login AWS SSO session
	// Ready is the server-computed readiness (region+model+any credential source),
	// echoed so the UI doesn't re-derive — and drift from — this gate.
	Ready bool `json:"ready"`
}

// ready reports whether a claude-code run would actually get the Bedrock
// transport right now — mirrors resolveBedrockAuth's gate: region + model AND at
// least one credential source (a bearer token, a captured AWS SSO session, a
// ~/.aws mount, or resident keys). Presence, not value, is enough here (no live
// secret-store read) — except for the SSO session, whose expiry IS honoured
// because resolveBedrockAuth falls through an expired blob to the next mode.
func (b SetupBedrock) ready() bool {
	return b.Region != "" && b.Model != "" &&
		(b.CredsPresent || b.AWSMount || b.BearerPresent || b.SSOPresent)
}

// configured reports whether the operator has touched ANY Bedrock knob (region,
// model, or a credential source that is Bedrock's alone) — used to decide
// whether the bedrock_provider check is worth showing at all vs. staying silent
// for the overwhelming majority of operators who never use Bedrock. SSOPresent
// is deliberately NOT a term: a container AWS SSO login on its own says nothing
// about wanting Bedrock, and ready() already implies configured() through Region.
func (b SetupBedrock) configured() bool {
	return b.Region != "" || b.Model != "" || b.CredsPresent || b.AWSMount || b.BearerPresent
}

// credSourceDesc names the winning credential source (resolveBedrockAuth's
// precedence) for honest UI copy — "resident keys" is wrong for a mount/bearer host.
func (b SetupBedrock) credSourceDesc() string {
	switch {
	case b.BearerPresent:
		return "a proxy-injected Bedrock API key (never resident in the sandbox)"
	case b.SSOPresent:
		return "your captured AWS SSO session (container login; re-login when it expires)"
	case b.AWSMount:
		return "your host AWS credentials via a read-only ~/.aws mount (SSO auto-refreshes)"
	default:
		return "resident AWS SigV4 credentials"
	}
}

// setupBedrock reports Bedrock readiness: region/model are boot-time config
// (non-secret, safe to echo to the UI) and each credential flag mirrors the
// matching resolveBedrockAuth branch — presence, never the value. AWSMount
// mirrors its opt-in host-mode path: BedrockAWSConfigDir set AND the dir still
// exists (stat it, so a since-deleted ~/.aws doesn't read ready). SSOPresent
// mirrors its captured-SSO branch, which is why this needs a ctx: the blob is a
// secret-store read, and an expired one is not a credential there either.
func (s *Server) setupBedrock(ctx context.Context, present map[string]bool) SetupBedrock {
	awsMount := false
	if s.cfg.BedrockAWSConfigDir != "" {
		st, err := os.Stat(s.cfg.BedrockAWSConfigDir)
		awsMount = err == nil && st.IsDir()
	}
	sso := false
	if blob, found, err := s.readAWSSSOBlob(ctx); err == nil && found {
		sso = !blob.expired(s.cfg.Now())
	}
	b := SetupBedrock{
		Region:        s.cfg.BedrockRegion,
		Model:         s.cfg.BedrockModel,
		CredsPresent:  present[bedrockAccessKeyIDSecret] && present[bedrockSecretAccessKeySecret],
		AWSMount:      awsMount,
		BearerPresent: present[bedrockAPIKeySecret],
		SSOPresent:    sso,
	}
	b.Ready = b.ready()
	return b
}
