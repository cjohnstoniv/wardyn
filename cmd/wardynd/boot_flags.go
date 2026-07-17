// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
)

// bootFlags holds every wardynd CLI flag / env pair, resolved by parseBootFlags.
// Extracted from run() so the boot sequence reads as phases instead of a single
// god-function; the flag semantics are unchanged and each usage string is still
// the single source of truth for its knob.
type bootFlags struct {
	dsn            *string
	migrateDSN     *string
	listen         *string
	tlsCert        *string
	tlsKey         *string
	tlsTerminated  *bool
	adminToken     *string
	localMode      *bool
	localOperator  *string
	localTrustFwd  *bool
	uiDir          *string
	runnerSel      *string
	identitySel    *string
	secretStoreSel *string
	recordingSel   *string
	confinementMap *string
	trustDomain    *string
	controlURL     *string
	policyPath     *string
	composerCfg    *string
	ageKey         *string
	proxyImage     *string

	recordingDir *string
	auditSinks   *string
	auditSpool   *string

	oidcIssuer       *string
	oidcInternalIss  *string
	oidcClientID     *string
	oidcClientSecret *string
	oidcRedirectURL  *string
	oidcEmailDomains *string

	autoStopInterval *time.Duration

	approvalExpiryInterval *time.Duration
	approvalExpiryAfter    *time.Duration

	envbuild     *bool
	envbuildImg  *string
	envbuildRepo *string

	agentImagesJSON *string
	agentModel      *string
	scanAIAdvisor   *bool

	bedrockRegion       *string
	bedrockModel        *string
	bedrockAWSDir       *string
	bedrockAWSProfile   *string
	bedrockAWSSSORegion *string

	proxyURL *string

	printGroundtruthToken *bool
	genAgeKey             *bool
}

// parseBootFlags declares every wardynd flag (with its WARDYN_* env fallback)
// and parses the command line. Moved verbatim out of run(); the usage strings
// carry the operator-facing documentation for each knob.
func parseBootFlags() *bootFlags {
	f := &bootFlags{
		dsn:            flagEnv("dsn", "WARDYN_PG_DSN", "", "Postgres DSN (required)"),
		migrateDSN:     flagEnv("migrate-dsn", "WARDYN_PG_MIGRATE_DSN", "", "OPTIONAL Postgres DSN for an owner/migrator role that runs migrations; when set, WARDYN_PG_DSN is used ONLY for the least-privilege runtime app pool (enables audit_events DDL protection). Empty = single-DSN mode (no DDL protection, unchanged behavior)."),
		listen:         flagEnv("listen", "WARDYN_LISTEN", ":8080", "HTTP listen address"),
		tlsCert:        flagEnv("tls-cert", "WARDYN_TLS_CERT", "", "path to the TLS certificate (PEM); enables built-in TLS when set together with -tls-key"),
		tlsKey:         flagEnv("tls-key", "WARDYN_TLS_KEY", "", "path to the TLS private key (PEM); enables built-in TLS when set together with -tls-cert"),
		tlsTerminated:  flagBool("tls-terminated", "WARDYN_TLS_TERMINATED", false, "set when TLS terminates at an upstream reverse proxy; marks session cookies Secure even though wardynd itself serves plain HTTP"),
		adminToken:     flagEnv("admin-token", "WARDYN_ADMIN_TOKEN", "", "admin bearer token gating the public API"),
		localMode:      flagBool("local-mode", "WARDYN_LOCAL_MODE", false, "LOCAL HOST MODE: bypass public-API auth (no SSO/token) and attribute actions to the local operator. Single-developer localhost use only — refused on a publicly-routable bind. Sidecar/run-token auth is unaffected. Auto-enabled when no auth is configured AND the bind is loopback."),
		localOperator:  flagEnv("local-operator", "WARDYN_LOCAL_OPERATOR", "", "operator principal stamped on runs/approvals/audit in -local-mode (default: local:<os-user>)"),
		localTrustFwd:  flagBool("local-trust-forwarder", "WARDYN_LOCAL_TRUST_FORWARDER", false, "in -local-mode, accept a non-loopback request peer (the no-auth bypass otherwise requires a loopback TCP peer). COMPOSE/TEAM ONLY: safe solely when the port is published loopback-only (127.0.0.1:PORT) so the peer is always the docker gateway. NEVER set on a directly-bound host-mode wardynd — it re-opens LAN no-auth access."),
		uiDir:          flagEnv("ui-dir", "WARDYN_UI_DIR", "", "directory holding the built web UI (optional)"),
		runnerSel:      flagEnv("runner", "WARDYN_RUNNER", "none", `runner substrate: "none" or a registered confinement substrate ("docker" in -tags docker builds)`),
		identitySel:    flagEnv("identity", "WARDYN_IDENTITY", "embedded", `identity provider (pluggable seam): "embedded" (default)`),
		secretStoreSel: flagEnv("secret-store", "WARDYN_SECRET_STORE", "pg", `secret store (pluggable seam): "pg" (default)`),
		recordingSel:   flagEnv("recording-store", "WARDYN_RECORDING_STORE", "fs", `recording store (pluggable seam): "fs" (default)`),
		confinementMap: flagEnv("confinement-map", "WARDYN_CONFINEMENT_MAP", "", `optional per-class substrate/runtime pins making CC3 runtime-pluggable, e.g. "CC2=runsc;CC3=kata-qemu" (or "CC3=oci:kata-qemu"); empty = built-in defaults`),
		trustDomain:    flagEnv("trust-domain", "WARDYN_TRUST_DOMAIN", embedded.DefaultTrustDomain, "SPIFFE trust domain"),
		controlURL:     flagEnv("control-plane-url", "WARDYN_CONTROL_PLANE_URL", "http://wardynd:8080", "externally-reachable control plane URL for sidecars"),
		policyPath:     flagEnv("default-policy", "WARDYN_DEFAULT_POLICY", "examples/policies/default.json", "path to the default RunPolicy spec JSON"),
		composerCfg:    flagEnv("composer-config", "WARDYN_COMPOSER_CONFIG", "", "AI Run Composer registry config: a JSON file path or inline JSON ({default,backends}); empty disables the composer"),
		ageKey:         flagEnv("age-key", "WARDYN_AGE_KEY", "", "age X25519 identity (AGE-SECRET-KEY-...) for the secret store; generated+logged if empty"),
		proxyImage:     flagEnv("proxy-image", "WARDYN_PROXY_IMAGE", "", "OCI image for the wardyn-proxy sidecar (docker runner)"),

		recordingDir: flagEnv("recording-dir", "WARDYN_RECORDING_DIR", "./data/recordings", "directory for stored PTY session recordings (asciicast); empty disables replay"),
		auditSinks:   flagEnv("audit-sinks", "WARDYN_AUDIT_SINKS", "", "audit sink config JSON (file/webhook/syslog); empty disables fanout"),
		auditSpool:   flagEnv("audit-spool", "WARDYN_AUDIT_SPOOL", "./data/audit-spool.jsonl", "local append-only JSONL fallback for audit events whose Postgres write fails (durability so a security event is never lost); empty disables"),

		oidcIssuer:       flagEnv("oidc-issuer", "WARDYN_OIDC_ISSUER", "", "OIDC public issuer URL — browser-facing, matches the id_token iss (enables human SSO when set)"),
		oidcInternalIss:  flagEnv("oidc-internal-issuer", "WARDYN_OIDC_INTERNAL_ISSUER", "", "OIDC issuer URL reachable from wardynd for server-side calls (e.g. http://dex:5556); defaults to the public issuer"),
		oidcClientID:     flagEnv("oidc-client-id", "WARDYN_OIDC_CLIENT_ID", "", "OIDC client id"),
		oidcClientSecret: flagEnv("oidc-client-secret", "WARDYN_OIDC_CLIENT_SECRET", "", "OIDC client secret"),
		oidcRedirectURL:  flagEnv("oidc-redirect-url", "WARDYN_OIDC_REDIRECT_URL", "", "OIDC redirect URL (<base>/auth/callback)"),
		oidcEmailDomains: flagEnv("oidc-email-domains", "WARDYN_OIDC_EMAIL_DOMAINS", "", "comma-separated allowed email domains (empty = any verified email)"),

		autoStopInterval: flagDuration("autostop-interval", "WARDYN_AUTOSTOP_INTERVAL", time.Minute, "how often the lifecycle reaper scans for idle runs (0 disables)"),

		approvalExpiryInterval: flagDuration("approval-expiry-interval", "WARDYN_APPROVAL_EXPIRY_INTERVAL", 10*time.Minute, "how often to sweep stale PENDING approvals (0 disables)"),
		approvalExpiryAfter:    flagDuration("approval-expiry-after", "WARDYN_APPROVAL_EXPIRY_AFTER", 24*time.Hour, "PENDING approvals older than this are transitioned to EXPIRED"),

		envbuild:     flagBool("envbuild", "WARDYN_ENVBUILD", false, "enable devcontainer image builds for create-run (requires -tags docker)"),
		envbuildImg:  flagEnv("envbuild-image", "WARDYN_ENVBUILD_IMAGE", "", "envbuilder OCI image override (empty = upstream default)"),
		envbuildRepo: flagEnv("envbuild-cache-repo", "WARDYN_ENVBUILD_CACHE_REPO", "", "optional OCI registry ref for envbuilder layer cache (enables safe daemonless push mode)"),

		// agentImagesJSON is a JSON object mapping agent names to OCI image refs
		// (e.g. '{"claude-code":"wardyn/agent-claude-code:local"}'). When set,
		// named agents use the specified image instead of the ghcr convention.
		// Must be valid JSON when non-empty; validated at boot (fail closed).
		agentImagesJSON: flagEnv("agent-images", "WARDYN_AGENT_IMAGES", "", `JSON map of agent-name -> OCI image ref; overrides ghcr convention for named agents (env WARDYN_AGENT_IMAGES)`),
		agentModel:      flagEnv("agent-anthropic-model", "WARDYN_AGENT_ANTHROPIC_MODEL", "", `optional: pin ANTHROPIC_MODEL inside claude-code sandboxes (e.g. "opus") so the agent doesn't use the account/CLI default (which a promo can push to Fable). Empty = CLI default.`),
		scanAIAdvisor:   flagBool("scan-ai-advisor", "WARDYN_SCAN_AI_ADVISOR", false, "enable the ADVISORY AI workspace-scan fallback: when the deterministic scanner is unsure (low confidence / unrecognized build system), a resident read-only coding-agent CLI gap-fills EMPTY profile fields and forces needs_review. Advisory-only + fail-open (never overrides a deterministic fact, never fails the scan upload). Requires a resident claude CLI on the host PATH. Off = deterministic-only (default)."),

		// Bedrock: an enterprise Anthropic transport (no direct Anthropic egress,
		// billed via AWS). Both must be set to enable it; the AWS credentials
		// themselves are NOT flags — they come from the secret store
		// (aws-access-key-id/aws-secret-access-key/aws-session-token), read at
		// dispatch time since Bedrock's SigV4 request signing can't be
		// proxy-injected. See internal/api.Config.BedrockRegion/BedrockModel.
		bedrockRegion:       flagEnv("bedrock-region", "WARDYN_BEDROCK_REGION", "", `optional: AWS region for the Amazon Bedrock Anthropic transport (e.g. "us-east-1"). Requires -bedrock-model too, plus aws-access-key-id/aws-secret-access-key secrets. Empty = Bedrock disabled.`),
		bedrockModel:        flagEnv("bedrock-model", "WARDYN_BEDROCK_MODEL", "", `optional: Bedrock model id for claude-code (a cross-region inference-profile id, e.g. "us.anthropic.claude-sonnet-4-5-...", not a bare foundation-model id). Requires -bedrock-region too.`),
		bedrockAWSDir:       flagEnv("bedrock-aws-dir", "WARDYN_BEDROCK_AWS_DIR", "", `HOST MODE ONLY: bind a host ~/.aws directory READ-ONLY into each Bedrock run so the AWS SDK resolves credentials itself, including auto-refreshing AWS SSO. Avoids pasting static aws-access-key-id/-secret secrets (which expire under SSO). Leave empty for team/compose deployments.`),
		bedrockAWSProfile:   flagEnv("bedrock-aws-profile", "WARDYN_BEDROCK_AWS_PROFILE", "", `optional: AWS_PROFILE to select from the mounted ~/.aws (common with SSO). Only used with -bedrock-aws-dir.`),
		bedrockAWSSSORegion: flagEnv("bedrock-aws-sso-region", "WARDYN_BEDROCK_AWS_SSO_REGION", "", `optional: AWS SSO region whose oidc.<r>/portal.sso.<r> endpoints the sandbox may reach to exchange an SSO token for role creds. Defaults to -bedrock-region. Only used with -bedrock-aws-dir.`),

		// proxyURL overrides the WARDYN_PROXY_URL injected into sandbox env.
		// Defaults to "http://wardyn-proxy:3128" (per-run sidecar docker alias).
		proxyURL: flagEnv("proxy-url", "WARDYN_PROXY_URL_OVERRIDE", "", `sandbox WARDYN_PROXY_URL override (default http://wardyn-proxy:3128)`),

		// printGroundtruthToken, when set, mints a host-sensor token
		// (aud="wardyn-groundtruth") for the eBPF/Tetragon ground-truth ingest
		// sidecar, prints it to stdout, and exits. This is how compose seeds
		// WARDYN_GROUNDTRUTH_TOKEN. The token grants ONLY audit-write on
		// POST /api/v1/internal/groundtruth — it can never mint or approve
		// (those endpoints verify aud="wardyn-internal"). Fail-closed: minting
		// requires the identity provider; the token has the provider's standard
		// 1h TTL (operators re-mint on rotation).
		printGroundtruthToken: flagBool("print-groundtruth-token", "WARDYN_PRINT_GROUNDTRUTH_TOKEN", false, "mint and print a host-sensor token (aud=wardyn-groundtruth) for wardyn-tetragon-ingest, then exit"),

		// genAgeKey, when set, prints a freshly-generated age X25519 identity
		// (AGE-SECRET-KEY-...) to stdout and exits — BEFORE any DSN/DB work — so
		// `docker run --rm wardyn/wardynd:local -gen-age-key` can mint a durable
		// WARDYN_AGE_KEY with no Postgres.
		genAgeKey: flagBool("gen-age-key", "WARDYN_GEN_AGE_KEY", false, "generate a fresh age X25519 identity (AGE-SECRET-KEY-...) to stdout for WARDYN_AGE_KEY, then exit (no DSN required)"),
	}
	flag.Parse()
	return f
}

// localModeState is the resolved LOCAL HOST MODE posture: whether the no-auth
// bypass is in effect, the operator principal to stamp, and whether the bind is
// loopback (also feeds /setup/status readiness).
type localModeState struct {
	enabled  bool
	operator string
	loopback bool
}

// resolveLocalMode decides the LOCAL HOST MODE posture (single-developer
// localhost path): bypass public-API auth so the browser UI works with no
// SSO/Dex and no token. Auto-enable when no auth is configured AND the bind is
// loopback; otherwise honor the explicit flag. FAIL CLOSED: never serve a
// no-auth public API on a publicly-routable IP. The sidecar/run-token path
// (internalAuth) is unaffected either way.
//
// Side effect (host-mode Bedrock auto-detect): when Bedrock is configured with
// NO credential source, *f.bedrockAWSDir is defaulted to the host ~/.aws so the
// AWS SDK resolves the operator's creds (SSO auto-refreshes) — see the inline
// comment. Extracted verbatim from run().
func resolveLocalMode(f *bootFlags) (localModeState, error) {
	lm := localModeState{loopback: listenIsLoopback(*f.listen)}
	lm.enabled = *f.localMode || (*f.adminToken == "" && *f.oidcIssuer == "" && lm.loopback)
	lm.operator = strings.TrimSpace(*f.localOperator)
	if !lm.enabled {
		return lm, nil
	}
	if listenIsRoutablePublic(*f.listen) {
		return lm, fmt.Errorf("refusing to start: -local-mode bypasses authentication but the listen address %q is a publicly-routable IP; bind to loopback (127.0.0.1) or a private address, or configure auth (WARDYN_ADMIN_TOKEN / OIDC)", *f.listen)
	}
	// -local-trust-forwarder DISABLES the unspoofable loopback-PEER gate (it
	// trusts a loopback-only host publish so the peer is always the docker
	// gateway). That holds ONLY on a loopback bind or the compose
	// 0.0.0.0-in-container topology whose host publishes 127.0.0.1:PORT. On a
	// SPECIFIC non-loopback interface (private/RFC1918, link-local, or public)
	// it re-opens UNAUTHENTICATED LAN admin access, so refuse to start. The
	// unspecified all-interfaces bind is indistinguishable from the safe
	// compose case from inside the container (wardynd cannot see the host's
	// docker publish), so it earns a DISTINCT error-level log naming the exact
	// requirement rather than a refusal.
	if *f.localTrustFwd {
		if listenBindsSpecificRoutable(*f.listen) {
			return lm, fmt.Errorf("refusing to start: -local-trust-forwarder disables the loopback-peer gate but the listen address %q binds a specific non-loopback interface — "+
				"this re-opens UNAUTHENTICATED LAN admin access; use it ONLY when the port is published loopback-only (127.0.0.1:PORT), i.e. bind loopback here or an unspecified address inside a compose container", *f.listen)
		}
		if !lm.loopback {
			slog.Error("wardynd: -local-trust-forwarder on an unspecified bind DISABLES the loopback-peer gate — the UNAUTHENTICATED public API is exposed to the LAN unless the host publishes 127.0.0.1:PORT ONLY (the Compose default). Verify your docker publish / host firewall.",
				slog.String("listen", *f.listen),
			)
		}
	}
	if lm.operator == "" {
		lm.operator = defaultLocalOperator()
	}
	if lm.loopback {
		slog.Info("wardynd: LOCAL HOST MODE — public-API auth disabled; loopback bind. No SSO/token required.",
			slog.String("operator", lm.operator),
			slog.String("listen", *f.listen),
		)
	} else {
		slog.Warn("wardynd: LOCAL HOST MODE on a non-loopback bind — the UNAUTHENTICATED public API is reachable beyond localhost; ensure a host firewall or configure auth.",
			slog.String("listen", *f.listen),
			slog.String("operator", lm.operator),
		)
	}

	// Host-mode Bedrock auto-detect: region+model configured but NO credential
	// source given (no -bedrock-aws-dir, no aws-*/bedrock-api-key secrets) — the
	// exact "Needs setup" state where the operator already runs Claude on Bedrock
	// via their host ~/.aws. Default the read-only mount to ~/.aws so the AWS SDK
	// resolves their creds (SSO auto-refreshes) with nothing to paste. Host-mode
	// only + fail-safe: only when ~/.aws actually exists, so resolveBedrockAuth
	// still falls through cleanly otherwise.
	if *f.bedrockRegion != "" && *f.bedrockModel != "" && *f.bedrockAWSDir == "" {
		if home, herr := os.UserHomeDir(); herr == nil {
			awsDir := filepath.Join(home, ".aws")
			if st, serr := os.Stat(awsDir); serr == nil && st.IsDir() {
				*f.bedrockAWSDir = awsDir
				slog.Info("wardynd: host-mode Bedrock — no credential configured; auto-mounting the host AWS dir read-only (the AWS SDK resolves your host creds, SSO auto-refreshes)",
					slog.String("aws_dir", awsDir),
				)
			}
		}
	}
	return lm, nil
}
