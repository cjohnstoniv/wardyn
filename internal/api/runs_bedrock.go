// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
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
// dropped here, with a reason, instead of being delivered in
// WARDYN_PROXY_CONFIG_JSON to a wardyn-proxy that then os.Exit(1)s at container
// start and takes the run's whole egress path with it. validateSiteConfig
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
// actually reads: a Bedrock Integration row's own Secrets
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
	// bearerNamespace is the namespace the bearer was READ from (set only when
	// bearer is): the operator's under shared, the run owner's own under
	// per_user. authorBedrockBearerInjection records it on the grant, and the
	// injection sink resolves the key from exactly that namespace — see
	// resolveBedrockBearerInjection.
	bearerNamespace awsSSOScope
	// runtimeHost is the EFFECTIVE Bedrock data-plane host this run resolved
	// (bedrockDataPlaneHost: the WARDYN_BEDROCK_BASE_URL override's host when
	// set, else the regional public one). Audited by applyBedrockTransport in
	// every mode; in bearer mode it is additionally the TLS-MITM and
	// Authorization-injection target.
	runtimeHost string
	// runtimePort is the port runtimeHost is reached on: the
	// WARDYN_BEDROCK_BASE_URL override's port when it names one, else 443. It
	// exists so bearer mode can author its TLS-MITM entry as "host:port"
	// (net.JoinHostPort) exactly as planArtifactRedirect does — a BARE MITM
	// entry is any-port (proxy.parseMITMHostPort), so an agent that can reach
	// the Bedrock host at all could CONNECT to it on a port nobody configured
	// and have the tunnel TLS-terminated with the Wardyn leaf and the
	// operator's Bearer injected onto whatever answered there.
	runtimePort int
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
	// ssoAccountID/ssoRoleName are the AWS account and IAM role the stored
	// session this run would carry actually names — the pair
	// awsSSOConfigFileContents bakes VERBATIM into the sandbox's ~/.aws/config
	// and botocore then asks GetRoleCredentials for. Set ONLY on the ssoInject
	// branch, and from the POST-refresh blob, so a comparison against them is a
	// comparison against what the run will really use.
	//
	// They exist because the roster PIN is checked at capture time and nowhere
	// else: a capture that predates a pin is the one identity the pin was meant
	// to govern and the one nothing compares. resolveBedrockAuth cannot make
	// that comparison itself (awsSSOScope is {perUser, owner} — it has no
	// roster), so the pair rides out to the declared-mechanism gate one frame
	// up, which already holds the site config. Empty for every other lane, and
	// for a blob captured by a binary that did not record them — see
	// bedrockBlobPinMismatch for why empty must never refuse.
	ssoAccountID, ssoRoleName string
	// ssoRegion is the captured session's OWN region (blob.Region), which may
	// differ from the Bedrock region: it is what ssoPortalHost derives the one
	// injectable host from, and what the dispatch-time scope SNAPSHOT records so
	// a resolve mid-run compares against the region this run was authored with
	// rather than whatever the roster says later.
	ssoRegion string
	// ssoProxyInject is PHASE B: the captured SSO access token is injected by the
	// proxy on portal.sso instead of being written into the sandbox
	// (WARDYN_AWS_SSO_PROXY_INJECT). Read ONCE at dispatch and carried, so a
	// running sandbox never changes lane under the operator's flip. False =
	// unchanged, byte for byte.
	ssoProxyInject bool
	// ssoRefreshFailure carries the refusal sentence when a captured AWS SSO
	// credential COULD have been renewed but the renewal did not land (the
	// refresh token is spent, or the OIDC call did not complete). It is set only
	// on a refresh=true pass (the real launch, dispatch) and is independent of ready: the SSO
	// lane simply did not fire, and within Bedrock the mount/static lanes below
	// it still may. It exists so the dispatch gate can refuse the run with a
	// reason instead of letting a run boot toward a model it cannot reach.
	ssoRefreshFailure string
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
// services. See Config.BedrockBaseURL for the ceiling this implies.
func (s *Server) bedrockDataPlaneHost(region string) string {
	return bedrockDataPlaneHostFor(region, s.cfg.BedrockBaseURL)
}

// bedrockDataPlaneHostFor is bedrockDataPlaneHost over an explicit base URL: a
// model provider's Bedrock.BaseURL rather than the boot config's.
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

// sandboxAWSDir is where the host ~/.aws is bind-mounted read-only in the run
// (awsMount) OR materialized by agent-run from an env var (ssoInject) — same
// path either way, so the SDK env vars below don't need to branch on which.
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
// are extended. The caller stamps readiness.
func (s *Server) bedrockSSOAuth(blob awsSSOBlob, env map[string]string, hosts []string) bedrockAuth {
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
	// every run after the first. Mirrors handleHarnessCredentialPaste
	// (harnesscred.go). AddGlobal no-ops on the empty strings when a field
	// wasn't captured (Registry.MinLen).
	s.cfg.MaskRegistry.AddGlobal([]byte(blob.AccessToken))
	s.cfg.MaskRegistry.AddGlobal([]byte(blob.RefreshToken))
	s.cfg.MaskRegistry.AddGlobal([]byte(blob.ClientSecret))
	// The POST-refresh blob's own pair: a refresh=true pass is the one allowed
	// to redeem the rotating refresh token, and the identity the gate
	// compares must be the one this run will actually present.
	return bedrockAuth{env: env, egressHosts: hosts, ssoInject: true,
		ssoAccountID: blob.AccountID, ssoRoleName: blob.RoleName,
		ssoRegion: blob.Region, ssoProxyInject: proxyInjected}
}

// resolveBedrockAuth is the LEGACY lane chain, for a run that chose no model
// provider (a deployment whose provider block is nil); it retires with the
// operator-credential lanes (MP-4b). A run that chose a Bedrock provider never
// reaches it: its kind names one lane (providerBedrockTransport).
//
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
//
// Under the ZERO sso scope every secret read below is in the OPERATOR namespace
// on purpose (bare Get == For("")): Bedrock credentials are MDM/operator-set
// daemon config, never a member row (writableSecretName refuses these names for
// a non-operator).
//
// sso.perUser INVERTS that for the whole function, which is why it is a
// parameter and not a lookup: the org declared that this agent's model
// credential is one per person, so the ONLY admissible lane is the one the row
// declares, read from that principal's own namespace — their captured session
// (bedrock_sso) or their own stored bearer (bedrock_bearer), never the other
// (awsSSOScope.readsBearer / readsSSO). The ~/.aws-mount and static-key arms
// are bare operator-namespace reads, so under per_user they are SKIPPED
// ENTIRELY — a member with no credential of their own is not-configured, never
// silently served the operator's keys. That is also why the skip is a hard
// return rather than a per-arm condition: a lane added below later must not
// quietly become reachable for a member.
//
// refresh authorizes SIDE EFFECTS: only with refresh=true may the captured-SSO
// lane redeem its rotating refresh token and persist the rotated pair. The REAL
// LAUNCH (create) and DISPATCH pass true; Review's preflight and the create
// advisory pass false — a dry run must never spend a one-use token, and it does
// not need to: an expired-but-renewable credential reads READY there.
// bedrockRegionModel is the EFFECTIVE region and model for a run: the picked
// workspace/container's per-run override where it names one, else the global
// operator config.
func (s *Server) bedrockRegionModel(ws *types.WorkspaceBedrockRef) (region, model string) {
	region, model = s.cfg.BedrockRegion, s.cfg.BedrockModel
	if ws != nil {
		region = cmp.Or(strings.TrimSpace(ws.Region), region)
		model = cmp.Or(strings.TrimSpace(ws.Model), model)
	}
	return region, model
}

// bedrockLaneSelectable is resolveBedrockAuth's own "is this lane in play at
// all" predicate, lifted out so the dispatch-time roster guard
// (enforceReadableRosterForCredential) asks EXACTLY the question this function
// answers rather than a hand-copied echo of it that drifts.
func bedrockLaneSelectable(runAgent string, modelRun, subscriptionActive, haveSecrets bool, region, model string) bool {
	return modelRun && !subscriptionActive && runAgent == "claude-code" &&
		region != "" && model != "" && haveSecrets
}

// bedrockBearerFor reads the Bedrock BEARER token from the namespace scope
// names, nil when there is none to read there.
//
// The ZERO scope is the operator namespace — byte-for-byte the read this was
// before a member could hold a bearer of their own, and what a `shared` row (or
// no roster) resolves to.
//
// A PER-USER scope reads that principal's OWN row and NEVER the operator's, and
// the List-then-Get shape is the whole reason this is not a one-liner:
// Store.For(owner).Get FALLS BACK to the operator's row by contract
// (internal/secretstore/pg), so the obvious For(owner).Get would serve the
// ADMIN's bearer to a member who has stored nothing — the cross-principal
// substitution per_user exists to refuse. For("").List is never consulted, so
// the owner's own rows are all this can see (ownSecret). readAWSSSOBlob carries the
// identical dance for the identical reason; a per-user scope with no owner, or
// one read inside the no-credential member preview, is ABSENT there and here.
//
// A per-user scope whose row declares the SSO lane reads NO bearer at all
// (awsSSOScope.readsBearer): the member's own key must not win the precedence
// chain over the session the row names and have every run refused for it.
//
// An EMPTY or whitespace-only value reads as ABSENT rather than as a configured
// credential. A blank row would otherwise win the precedence chain, author a
// grant, and surface as an upstream 403 naming neither the lane it picked nor
// the empty secret it picked it on.
func (s *Server) bedrockBearerFor(ctx context.Context, scope awsSSOScope) []byte {
	if s.cfg.Secrets == nil || !scope.readsBearer() {
		return nil
	}
	var raw []byte
	var err error
	if scope.perUser {
		raw, _, err = s.ownSecret(ctx, scope.owner, bedrockAPIKeySecret)
	} else {
		raw, err = s.cfg.Secrets.Get(ctx, bedrockAPIKeySecret)
	}
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return raw
}

func (s *Server) resolveBedrockAuth(ctx context.Context, runAgent string, subscriptionActive, modelRun, refresh bool, ws *types.WorkspaceBedrockRef, sso awsSSOScope) bedrockAuth {
	region, model := s.bedrockRegionModel(ws)
	profile := s.cfg.BedrockAWSProfile
	if !bedrockLaneSelectable(runAgent, modelRun, subscriptionActive, s.cfg.Secrets != nil, region, model) {
		return bedrockAuth{}
	}
	runtimeHost := s.bedrockDataPlaneHost(region)
	runtimePort := redirectPort(s.cfg.BedrockBaseURL)
	hosts := []string{runtimeHost, bedrockControlHost(region)}
	// Common Bedrock env: the on-switch, region, and model id. AWS_REGION is what
	// claude-code reads; AWS_DEFAULT_REGION is the broader AWS-SDK fallback. The
	// model id is a cross-region INFERENCE-PROFILE id (e.g.
	// "us.anthropic.claude-sonnet-4-5-...") or an application-inference-profile ARN
	// — NOT a bare foundation-model id (Bedrock silently rewrites those and can 403
	// under an SCP). Operator-supplied; Wardyn does not validate the format.
	base := func() map[string]string { return bedrockBaseEnv(region, model, s.cfg.BedrockBaseURL) }
	// ssoRefreshFailure is set by the captured-SSO branch when a renewable
	// credential could not be renewed. Every return below carries it, because the
	// dispatch gate must be able to name the reason whichever lane (if any) ended
	// up winning.
	var ssoRefreshFailure string
	// ready return shared by every credential mode below: stamps the EFFECTIVE
	// region/model so the audit names what this run used, not the global config,
	// and the EFFECTIVE data-plane host so applyBedrockTransport's audit names
	// where the call actually went (bearer mode additionally uses it as the
	// TLS-MITM + Authorization-injection target).
	ready := func(b bedrockAuth) bedrockAuth {
		b.ready, b.region, b.model, b.runtimeHost = true, region, model, runtimeHost
		b.runtimePort = runtimePort
		b.ssoRefreshFailure = ssoRefreshFailure
		return b
	}

	// Preferred: bearer-token mode. A Bedrock API key is a STATIC Authorization
	// header, so the proxy TLS-MITMs bedrock-runtime and injects it — the sandbox
	// holds only a placeholder, never the real token (trust parity with api-key /
	// subscription). Selected whenever a bedrock-api-key secret exists in the
	// namespace this run's scope names — the operator's under `shared`, the
	// caller's OWN under per_user, never one standing in for the other
	// (bedrockBearerFor).
	if bearer := s.bedrockBearerFor(ctx, sso); len(bearer) > 0 {
		env := base()
		// A non-empty sentinel so claude-code uses bearer auth (not SigV4); the proxy
		// overwrites the Authorization header with the real token on the wire.
		env[envBedrockBearer] = "wardyn-proxy-injected"
		return ready(bedrockAuth{env: env, egressHosts: hosts, bearer: true, bearerNamespace: sso})
	}

	// Captured AWS SSO credential: a container-login `aws sso login` captured an
	// SSO access token (readAWSSSOBlob / awsSSOBlob, harnesscred.go). This wins
	// over the host ~/.aws mount and static keys below — it needs no host access
	// and stores no long-lived static key — but an explicit bearer token still
	// wins over it (bearer is never-resident; this mode is).
	//
	// Residency (contrast with bearer above): the captured SSO access token DOES
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
	// Phase B (not yet built): proxy-inject the token as the
	// `x-amz-sso_bearer_token` header on portal.sso.<region> instead of writing
	// it into the sandbox — that call is authtype:none (unsigned), so a MITM can
	// set the header without the sandbox ever holding the token, mirroring the
	// Bedrock bearer path above. Until Phase B ships, this is an accepted,
	// documented tradeoff (same class as the resident-SigV4 fallback), not an
	// oversight.
	//
	// Renewal (the fix for a credential the next twenty lines would have healed):
	// an EXPIRED access token is not a dead credential when the blob carries a
	// refresh token and its client registration has not lapsed — dispatch renews
	// it here and the lane fires. The fall-through below is kept for exactly the
	// two cases nothing can heal: no refresh token at all (a legacy
	// sso_start_url profile) or a lapsed registration. On a renewal FAILURE the
	// SSO lane simply is not ready and carries its reason: within Bedrock the
	// mount and static-key lanes below still run exactly as they do today (the
	// chain is one mechanism), but no reader may read that as permission to
	// substitute a DIFFERENT mechanism — a credential must never silently change
	// source, which is what ssoRefreshFailure exists to let the dispatch gate say.
	//
	// Under a per_user row that declares the bearer lane a session the member
	// also holds is never selected (awsSSOScope.readsSSO): it is not the
	// credential the row names, and renewing it would spend a refresh token for
	// a run the mechanism gate then refuses.
	if blob, found, berr := s.readAWSSSOBlob(ctx, sso); sso.readsSSO() && berr == nil && found {
		if refresh {
			blob, ssoRefreshFailure = s.refreshAWSSSOBlob(ctx, sso, blob)
		}
		switch {
		case ssoRefreshFailure != "":
			slog.ErrorContext(ctx, "wardynd: captured AWS SSO credential could not be renewed; this Bedrock lane is not ready",
				slog.Time("expired_at", blob.ExpiresAt))
		case !blob.renewable(s.cfg.Now()) && blob.expired(s.cfg.Now()):
			// Observable so the UI can tell the operator to re-login (setup status
			// reads the same readAWSSSOBlob + the same predicate this checks); fall
			// through to the next credential mode rather than handing the run a dead
			// token.
			slog.WarnContext(ctx, "wardynd: captured AWS SSO credential expired and cannot be renewed; falling back to the next Bedrock credential mode",
				slog.Time("expired_at", blob.ExpiresAt))
		default:
			return ready(s.bedrockSSOAuth(blob, base(), hosts))
		}
	}

	// Per_user stops here. The org declared one credential per person, and the
	// three arms below are all operator-namespace reads — the host ~/.aws mount
	// is the deployer's own AWS state, the static keys are the deployer's
	// secrets. Falling through would serve a member the admin's credential the
	// moment their own session lapsed, which is the substitution this whole lane
	// exists to refuse: a credential must never silently change source. The
	// caller reads a not-ready bedrockAuth (plus any renewal reason) and refuses
	// the run naming the lane, rather than dispatching on somebody else's keys.
	if sso.perUser {
		return bedrockAuth{ssoRefreshFailure: ssoRefreshFailure}
	}

	// Host-mode ~/.aws mount: bind the operator's host ~/.aws read-only into the
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
			env[envAWSConfigFile] = sandboxAWSDir + "/config"
			env["AWS_SHARED_CREDENTIALS_FILE"] = sandboxAWSDir + "/credentials"
			if profile != "" {
				env["AWS_PROFILE"] = profile
			}
			// The SSO region falls back to the EFFECTIVE Bedrock region (a workspace
			// override included) — falling back to the global one would allow the
			// wrong regional oidc/portal.sso endpoints and 403 the credential
			// exchange for a workspace that moved the run to another region.
			ssoRegion := cmp.Or(s.cfg.BedrockAWSSSORegion, region)
			hosts = append(hosts, ssoEgressHosts(ssoRegion, s.cfg.AWSSSOEndpointOverride)...)
			// The SAME pair the ssoInject branch merges above, for the same
			// reason: this lane's SDK also exchanges an SSO token for role
			// credentials, so moving its ALLOWLIST under the test hatch without
			// moving the SDK left it dialling the real AWS hosts it had just
			// stopped allowing. nil on every real deployment.
			maps.Copy(env, ssoInjectEndpointEnv(s.cfg.AWSSSOEndpointOverride))
			return ready(bedrockAuth{env: env, egressHosts: hosts,
				awsMount: true, awsMountSource: s.cfg.BedrockAWSConfigDir})
		}
	}

	// Fallback: resident SigV4 access keys. SigV4 signs each request in-process, so
	// the creds MUST be resident in the sandbox env (documented exception, masked +
	// modelRun-gated). Requires both access key + secret key.
	accessKey, aerr := s.cfg.Secrets.Get(ctx, bedrockAccessKeyIDSecret)
	secretKey, serr := s.cfg.Secrets.Get(ctx, bedrockSecretAccessKeySecret)
	if aerr != nil || serr != nil || len(accessKey) == 0 || len(secretKey) == 0 {
		return bedrockAuth{ssoRefreshFailure: ssoRefreshFailure}
	}
	env := base()
	env["AWS_ACCESS_KEY_ID"] = string(accessKey)
	env["AWS_SECRET_ACCESS_KEY"] = string(secretKey)
	if tok, terr := s.cfg.Secrets.Get(ctx, bedrockSessionTokenSecret); terr == nil && len(tok) > 0 {
		env["AWS_SESSION_TOKEN"] = string(tok)
	}
	return ready(bedrockAuth{env: env, egressHosts: hosts})
}
