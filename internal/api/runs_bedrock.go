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
// encodeArtifactConfig, reused here as-is). agent-run-lib.sh must materialize
// it before claude/aws-sdk runs; that function does not exist yet (owned by
// another agent) — see the snippet in resolveBedrockAuth's doc comment above
// the ssoInject branch.
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
	//
	// agent-run-lib.sh needs a materialization function for awsSSOConfigEnvVar
	// (does not exist yet — owned by another agent), mirroring
	// install_artifact_config:
	//
	//   materialize_aws_sso_config() {
	//       [[ -n "${WARDYN_AWS_SSO_CONFIG_B64:-}" ]] || return 0
	//       local relpath b64 dst
	//       while IFS=$'\t' read -r relpath b64; do
	//           [[ -n "$relpath" ]] || continue
	//           case "$relpath" in
	//               /*|*..*) echo "agent-run: skipping unsafe aws-sso-config path: $relpath" >&2; continue ;;
	//           esac
	//           dst="${HOME}/${relpath}"
	//           mkdir -p "$(dirname "$dst")"
	//           if printf '%s' "$b64" | base64 -d > "$dst" 2>/dev/null; then
	//               chmod 0600 "$dst"
	//           else
	//               echo "agent-run: WARNING failed to write aws-sso-config ${relpath}" >&2
	//           fi
	//       done <<< "$WARDYN_AWS_SSO_CONFIG_B64"
	//   }
	//
	// (install_artifact_config's own `-n "$b64"` guard would silently DROP an
	// empty-content record; this variant drops that guard since it never emits
	// one — AWS_SHARED_CREDENTIALS_FILE deliberately points at a path nothing
	// creates, see below.) Call it alongside prepare_claude_config_dir /
	// materialize_managed_claude_config / install_artifact_config, before any
	// aws/claude invocation.
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
			return bedrockAuth{env: env, egressHosts: hosts, ready: true, ssoInject: true}
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
