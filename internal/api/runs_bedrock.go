// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
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
// store, an unresolvable secret, a non-http URL, or a URL the SIDECAR itself
// would refuse (from EITHER source) all return ("", <reason>) — the caller
// audits the reason and dispatches with direct egress instead of failing the
// run. A resolved, loadable http URL returns (url, "").
//
// Scheme is restricted to http for BOTH sources because the sidecar's own
// config validation (parseUpstreamProxy, internal/egress/proxy/upstream.go)
// rejects https: the hop TO the corp proxy is a plaintext CONNECT +
// Proxy-Authorization today, and an https:// proxy URL would need a TLS wrap
// first or leak that Basic credential in cleartext — so an https value is
// skipped here rather than crashing the proxy sidecar at startup.
//
// The scheme is only HALF the rule, which is why the resolved value then goes
// through loadableUpstreamProxyURL rather than being returned here: the scheme
// check alone accepts "http://proxy.corp:0", "http://proxy.corp:99999" and
// "http:///path", which the sidecar refuses with `bad port "0"` / `missing
// host` — so this function alone cannot report a URL as truly loadable.
func resolveUpstreamProxyURL(ctx context.Context, plainURL, secretRef string, getSecret func(context.Context, string) ([]byte, error)) (proxyURL, failReason string) {
	if plainURL != "" {
		if raw, ok := normalizedHTTPProxyURL(plainURL); ok {
			return loadableUpstreamProxyURL(raw)
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
		return loadableUpstreamProxyURL(raw)
	}
	return "", "unsupported-scheme"
}

// loadableUpstreamProxyURL is the last gate BOTH resolve lanes pass through: a
// URL the sidecar's own loader (proxy.ValidUpstreamProxyURL) would refuse is
// dropped here, with a reason, instead of being delivered in the proxy config
// to a wardyn-proxy that then os.Exit(1)s at container start and takes the run's whole egress path with it. validateSiteConfig
// applies the same gate at PUT /site-config, so reaching this is either a row
// written before that check existed or a URL that arrived through the SECRET
// lane, which no write-time validator can see inside — exactly the
// defense-in-depth split the reserved-secret-name guard above already makes.
// The loader's error is never returned: a secret-sourced URL may carry
// user:pass, and this function's result is audited.
//
// It DELEGATES rather than re-stating the rule: proxy.ValidUpstreamProxyURL is
// parseUpstreamProxy, the rule Config.applyDefaultsAndValidate runs and
// cmd/wardyn-proxy turns into os.Exit(1), so the whole rule travels together —
// scheme, a non-empty host, and a port in 1..65535. This is the
// ValidNoProxyEntry pattern (site_config_noproxy.go) applied to the value
// beside the list, and it is what keeps the authority half from drifting away
// from the sidecar again.
func loadableUpstreamProxyURL(raw string) (proxyURL, failReason string) {
	if err := proxy.ValidUpstreamProxyURL(raw); err != nil {
		return "", "unloadable-upstream-url"
	}
	return raw, ""
}

// normalizedHTTPProxyURL trims raw and reports (trimmed, true) when it parses
// as an http-scheme URL, else ("", false). Shared by both resolveUpstreamProxyURL
// sources AND by validateSiteConfig's write-time gate, so the scheme
// restriction can never drift between them.
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
// WARDYN_DEFAULT_POLICY precedent); the AWS credentials themselves stay global and are read directly
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
	env         map[string]string // sandbox env additions (bearer: placeholder; SSO: the synthetic ~/.aws)
	egressHosts []string          // regional data+control plane hosts to allow
	ready       bool
	// bearer selects the never-resident path: bedrock-runtime is TLS-MITM'd and the
	// Authorization: Bearer header is injected proxy-side from the run owner's own
	// key for the provider, so the sandbox holds only a placeholder.
	bearer bool
	// runtimeHost is the EFFECTIVE Bedrock data-plane host this run resolved
	// (the provider's base URL host when it names one, else the regional public
	// one). Audited by applyBedrockTransport in every mode; in bearer mode it is
	// additionally the TLS-MITM and Authorization-injection target.
	runtimeHost string
	// runtimePort is the port runtimeHost is reached on: the provider's
	// bedrock.base_url port when it names one, else 443. It
	// exists so bearer mode can author its TLS-MITM entry as "host:port"
	// (net.JoinHostPort) exactly as planArtifactRedirect does — a BARE MITM
	// entry is any-port (proxy.parseMITMHostPort), so an agent that can reach
	// the Bedrock host at all could CONNECT to it on a port nobody configured
	// and have the tunnel TLS-terminated with the Wardyn leaf and the
	// operator's Bearer injected onto whatever answered there.
	runtimePort int
	// region/model are the provider's region and this harness's model —
	// audited by applyBedrockTransport so the record names what the run used.
	region, model string
	// ssoInject selects the captured-AWS-SSO-credential delivery path (a
	// container-login `aws sso login` — see awsSSOBlob in harnesscred.go): a
	// minimal synthetic ~/.aws is materialized in the sandbox from an env var
	// (no host mount, no static keys stored).
	ssoInject bool
	// ssoAccountID/ssoRoleName are the AWS account and IAM role the stored
	// session this run would carry actually names — the pair
	// awsSSOConfigFileContents bakes VERBATIM into the sandbox's ~/.aws/config
	// and botocore then asks GetRoleCredentials for. Set ONLY on the ssoInject
	// branch, and from the POST-refresh blob, so a comparison against them is a
	// comparison against what the run will really use.
	ssoAccountID, ssoRoleName string
	// ssoRegion is the captured session's OWN region (blob.Region), which may
	// differ from the Bedrock region: it is what ssoPortalHost derives the one
	// injectable host from, and what the dispatch-time scope SNAPSHOT records so
	// a resolve mid-run compares against the region this run was authored with
	// rather than whatever the provider says later.
	ssoRegion string
	// ssoProxyInject is PHASE B: the captured SSO access token is injected by the
	// proxy on portal.sso instead of being written into the sandbox
	// (WARDYN_AWS_SSO_PROXY_INJECT). Read ONCE at dispatch and carried, so a
	// running sandbox never changes lane under the operator's flip. False =
	// unchanged, byte for byte.
	ssoProxyInject bool
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

// bedrockDataPlaneHostFor is the data-plane host a Bedrock provider's runs
// reach: its Bedrock.BaseURL override when set (a VPC/PrivateLink endpoint),
// else the regional public host. The CONTROL plane (bedrockControlHost) is
// deliberately NOT overridden: a PrivateLink endpoint is per-SERVICE, and
// bedrock-runtime and bedrock are two services.
func bedrockDataPlaneHostFor(region, baseURL string) string {
	if h := gatewayHost(baseURL); h != "" {
		return h
	}
	return bedrockRuntimeHost(region)
}

// ssoEgressHosts are the AWS IAM Identity Center (SSO) endpoints the sandbox SDK
// must reach to exchange a cached SSO token for role credentials: oidc.<r> for
// token refresh and portal.sso.<r> for GetRoleCredentials. Only needed on the
// ~/.aws-mount path; region is the SSO region (may differ from the Bedrock one).
//
// endpointOverride is the TEST hatch (awssso_endpoint.go): when set, the ONE
// fake host REPLACES both regional entries, because one server backs both
// services — their paths never collide (test/awsssofake's package doc). Empty
// (every real deployment) returns exactly what it always did.
func ssoEgressHosts(ssoRegion, endpointOverride string) []string {
	if h := gatewayHost(endpointOverride); h != "" {
		// Port-qualified beside the bare host when the override names one. The
		// bare entry is what buildInjector's AllowedExactHost check and both
		// resolve lanes ask for; the PORT is what decides whether the transport
		// may carry the credential at all. injectableTransport asks
		// Policy.AuthoredPortFor(host, port) for every non-80 port, and with one
		// bare entry that answers false -- so the cleartext fake lane on :8090
		// was allowlisted, MITM-less and silently UNCREDENTIALED: the SDK saw the
		// fake's own 401 and nothing in the proxy said why. A port-qualified
		// entry still satisfies the bare-host question (exact_host_binding.go),
		// so adding it widens nothing: the same one host, now with its transport
		// declared.
		if u, err := url.Parse(endpointOverride); err == nil && u.Port() != "" {
			return []string{h, net.JoinHostPort(h, u.Port())}
		}
		return []string{h}
	}
	return []string{
		fmt.Sprintf("oidc.%s.amazonaws.com", ssoRegion),
		fmt.Sprintf("portal.sso.%s.amazonaws.com", ssoRegion),
	}
}

// ssoPortalHost is the ONE host the captured SSO access token may be injected
// to (Phase B): the run's own regional IAM Identity Center portal, where
// the sandbox SDK exchanges the session for role credentials. It is the second
// of ssoEgressHosts' two regional entries, and the override's host when the
// test hatch moved them.
//
// Bare, never host:port -- deliberately, and for the reason
// authorBedrockBearerInjection states for its own scope: buildInjector keys
// byHost on the rule host VERBATIM (inject.go) and both resolve lanes ask with
// a bare host, so a port-qualified scope host is a rule nothing ever matches.
// The port rides two other places instead: the MITM-eligibility entry
// (net.JoinHostPort, so a bare any-port entry cannot have some other port's
// tunnel terminated with the Wardyn leaf) and the egress allowlist above.
// ssoPortalPort is the port the sandbox actually reaches the portal on: the
// override's when it names one, else the override's SCHEME default (80 for
// http://, 443 otherwise). It is the MITM-eligibility entry's port, and it must
// track ssoEgressHosts' own port entry — an entry authored at a port the run
// never dials is a tunnel nobody terminates.
func ssoPortalPort(endpointOverride string) string {
	u, err := url.Parse(endpointOverride)
	if err != nil {
		return "443"
	}
	if p := u.Port(); p != "" {
		return p
	}
	// The scheme's own default, not a flat 443. An http:// override with no port
	// means port 80, and answering 443 there authored BOTH the allowlist entry
	// and the TLS-MITM entry on a port nothing is listening on — the credential
	// withheld on the port actually dialled, for a shape that reads correct in
	// every config dump. No override at all stays 443, which is the real portal.
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return "443"
}

func ssoPortalHost(ssoRegion, endpointOverride string) string {
	if h := gatewayHost(endpointOverride); h != "" {
		return h
	}
	return fmt.Sprintf("portal.sso.%s.amazonaws.com", ssoRegion)
}

// sandboxAWSDir is where agent-run materializes a captured AWS sign-in's
// synthetic ~/.aws from an env var (ssoInject).
const sandboxAWSDir = "/home/agent/.aws"

// awsSSOProfileName is both the generated [sso-session <name>] and
// [profile <name>] name for a captured SSO credential (ssoInject). Fixed, not
// operator-configured: this profile exists only inside the ephemeral sandbox
// ~/.aws Wardyn generates, so there is no collision to name around.
const awsSSOProfileName = "wardyn"

// awsSSOPlaceholderToken is the inert value the Phase-B sandbox cache carries
// where the real SSO access token used to be. It is the SAME spelling the
// Bedrock BEARER lane already stages in AWS_BEARER_TOKEN_BEDROCK, on purpose:
// one string an operator can grep for that means "this credential is injected
// proxy-side, and what you are looking at is not a secret".
//
// It is NEVER mask-registered: registering it would redact a marker that exists
// to be visible, in every run's stream.
const awsSSOPlaceholderToken = "wardyn-proxy-injected"

// awsSSOPlaceholderCacheTTL is how far out the placeholder cache file's expiry
// is stamped: long enough that no run outlives it (the SDK refuses a cache it
// reads as expired, and would try to refresh a placeholder inside five minutes
// of it), short enough to stay a plainly synthetic value.
const awsSSOPlaceholderCacheTTL = 30 * 24 * time.Hour

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
// (RFC3339) are always present.
//
// One refresher per token. refreshToken/clientId/clientSecret are WITHHELD
// whenever the blob carries a refresh token, because the control plane redeems
// it at dispatch (awssso_refresh.go) and CreateToken ROTATES the token: with
// both parties refreshing, a long-lived run would have the sandbox rotate the
// token in-run, spend the stored one, and the next dispatch's redeem would fail
// invalid_grant — hourly manual re-auth back as the resting state. Only one
// party may hold the rotating secret, and only the control plane can persist
// what comes back.
//
// The SDK loads a cache without those three fine: it validates accessToken and
// expiresAt on load, and the registration fields matter only to its OWN refresh
// attempt. aws-sdk-js-v3 makes that attempt within 5 minutes of expiry, which
// awsSSORefreshSkew (10 min) keeps a freshly dispatched run out of; botocore
// uses a 15-minute window but does not throw on their absence — it loads such a
// cache at 50 minutes and at 3 minutes of remaining validity alike (verified
// against botocore 1.43.93) and raises only once the token has EXPIRED. A run
// that outlives its access token therefore fails visibly at its first model call
// instead of silently rotating the pair behind the control plane.
//
// A blob with NO refresh token keeps today's bytes exactly: there is nothing to
// rotate, so the registration fields are harmless where they exist.
//
// proxyInjected is PHASE B (WARDYN_AWS_SSO_PROXY_INJECT): when true the
// real access token does not reach the sandbox at all. The file carries the
// inert awsSSOPlaceholderToken and an expiry far enough out that the SDK never
// tries to refresh it -- the token itself is set on the wire by the proxy as
// x-amz-sso_bearer_token, on that one portal host, from a value the sandbox
// never holds. The rest of the file is unchanged, because the SDK still needs
// the session identity (start URL, region) to resolve the profile at all, and
// neither is a credential. With the switch off this argument is false and the
// bytes are unchanged, byte for byte.
func awsSSOCacheFileContents(b awsSSOBlob, proxyInjected bool) string {
	accessToken, expiresAt := b.AccessToken, b.ExpiresAt.UTC()
	if proxyInjected {
		// The expiry is LOCAL to the sandbox's SDK, which validates it before
		// making any call and attempts its own refresh inside five minutes of it
		// (aws-sdk-js-v3) -- against a cache with no registration fields, which
		// would fail. botocore raises only once expired. Either way the file has
		// to outlive every run, so it is stamped far out rather than mirroring
		// the real token's expiry: the real expiry is the CONTROL PLANE's to know.
		accessToken = awsSSOPlaceholderToken
		expiresAt = time.Now().UTC().Add(awsSSOPlaceholderCacheTTL)
	}
	cache := map[string]any{
		"startUrl":    b.StartURL,
		"region":      b.Region,
		"accessToken": accessToken,
		"expiresAt":   expiresAt.Format(time.RFC3339),
	}
	if proxyInjected {
		// Nothing rotatable, ever: the placeholder cannot be refreshed and the
		// registration pair is exactly what a sandbox-side refresh would need.
		raw, _ := json.Marshal(cache)
		return string(raw)
	}
	if b.RefreshToken == "" {
		if b.ClientID != "" {
			cache["clientId"] = b.ClientID
		}
		if b.ClientSecret != "" {
			cache["clientSecret"] = b.ClientSecret
		}
	}
	if !b.RegistrationExpiresAt.IsZero() {
		cache["registrationExpiresAt"] = b.RegistrationExpiresAt.UTC().Format(time.RFC3339)
	}
	raw, _ := json.Marshal(cache)
	return string(raw)
}

// bedrockBaseEnv is the sandbox env every Bedrock credential mode shares: the
// on-switch, region and model id, plus the data-plane override when baseURL
// names one (the boot config's, or a model provider's Bedrock.BaseURL).
func bedrockBaseEnv(region, model, baseURL string) map[string]string {
	env := map[string]string{
		envClaudeUseBedrock: "1",
		envAWSRegion:        region,
		envAWSDefaultRegion: region,
		envAnthropicModel:   model,
	}
	// PrivateLink data-plane override, set ONLY when the operator configured
	// one (absent = byte-identical to today). TWO variables because the four
	// credential modes below split across two clients: claude-code reads the
	// harness variable, while the three SigV4 modes route through the AWS SDK,
	// which reads its own service-specific knob.
	//
	// Never the global AWS_ENDPOINT_URL: that re-points EVERY AWS
	// service this sandbox talks to — including STS and SSO, which the
	// captured-SSO and ~/.aws-mount modes below use to exchange a token for
	// role credentials. One service's private endpoint must not silently
	// become every service's. The SSO services have their own knob for that
	// —  WARDYN_AWS_SSO_ENDPOINT_OVERRIDE (awssso_endpoint.go), which sets
	// AWS_ENDPOINT_URL_SSO/_SSO_OIDC and nothing else — and it is a TEST
	// hatch, refused unless WARDYN_ALLOW_TEST_ENDPOINTS=true. Two knobs, two
	// services, and only one of them is a supported production posture.
	if baseURL != "" {
		env[envBedrockBaseURL] = baseURL
		env[envBedrockRuntimeURL] = baseURL
	}
	return env
}

// bedrockSSOAuth is the captured-AWS-SSO credential mode over a live blob: the
// synthetic ~/.aws the sandbox SDK resolves (its token cache a placeholder under
// Phase B), the SSO egress hosts, and the blob's secrets masked. env is the
// shared Bedrock env (bedrockBaseEnv) and hosts the Bedrock egress hosts; both
// are extended. sso is the scope the blob was read in. The caller stamps readiness.
func (s *Server) bedrockSSOAuth(blob awsSSOBlob, sso awsSSOScope, env map[string]string, hosts []string) bedrockAuth {
	env[envAWSConfigFile] = sandboxAWSDir + "/config"
	// Deliberately not materialized: a missing shared-credentials file is
	// normal ("no static creds") and every AWS SDK treats it that way, which
	// is exactly right here — the only credential source is the SSO cache.
	env[envAWSSharedCredsFile] = sandboxAWSDir + "/credentials"
	env[envAWSProfile] = awsSSOProfileName
	// Phase B: with WARDYN_AWS_SSO_PROXY_INJECT on, the cache file
	// carries an inert placeholder and the real access token is injected
	// on the wire at portal.sso by the proxy. The switch is read ONCE,
	// here, at dispatch: a run already dispatched keeps the lane it was
	// authored with (its placeholder cache, its grant and its MITM entry)
	// until it ends, so flipping the switch is a change to NEW dispatches
	// and never a change under a running sandbox.
	proxyInjected := s.cfg.AWSSSOProxyInject
	env[awsSSOConfigEnvVar] = encodeArtifactConfig(map[string]string{
		".aws/config": awsSSOConfigFileContents(blob),
		".aws/sso/cache/" + awsSSOCacheFileName(awsSSOProfileName) + ".json": awsSSOCacheFileContents(blob, proxyInjected),
	})
	// The TEST endpoint hatch, if the operator set it: the SDK resolves
	// this cache by CALLING GetRoleCredentials, so pointing the egress
	// list at a fake without pointing the SDK at it too would just get the
	// call denied on the real AWS host. nil on every real deployment, so
	// this env map is byte-identical to before the knob existed.
	maps.Copy(env, ssoInjectEndpointEnv(s.cfg.AWSSSOEndpointOverride))
	hosts = append(hosts, ssoEgressHosts(blob.Region, s.cfg.AWSSSOEndpointOverride)...)
	// Mask GLOBALLY (not per-run, like the static-key branch below does via
	// the caller): this captured credential is reused across every run that
	// picks this mode, not minted fresh per run, so a per-run Add would miss
	// every run after the first. It ignores the empty strings when a field wasn't
	// captured (Registry.MinLen). Merge, not AddGlobal: this blob may predate
	// a refresh that ran concurrently outside our read, and replacing the
	// credential's set with it would retire the refresh's live tokens.
	s.cfg.MaskRegistry.MergeGlobalUntil(sso.rowOwner(), sso.ssoSecret(), blob.ExpiresAt,
		[]byte(blob.AccessToken), []byte(blob.RefreshToken), []byte(blob.ClientSecret))
	// The POST-refresh blob's own pair: a refresh=true pass is the one allowed
	// to redeem the rotating refresh token, and the identity the gate
	// compares must be the one this run will actually present.
	return bedrockAuth{env: env, egressHosts: hosts, ssoInject: true,
		ssoAccountID: blob.AccountID, ssoRoleName: blob.RoleName,
		ssoRegion: blob.Region, ssoProxyInject: proxyInjected}
}
