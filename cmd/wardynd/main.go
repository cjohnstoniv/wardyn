// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardynd is the Wardyn control plane: REST API, embedded web UI,
// policy engine, approval FSM, token broker, and audit ingest. Postgres is the
// ONLY required dependency. It contains zero target-specific code — sandboxes
// are dispatched through the runner.Runner interface (docker driver optional).
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded" // also self-registers "embedded" via init()
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	_ "github.com/cjohnstoniv/wardyn/internal/secretstore/pg" // register "pg" secret store
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// Secret names seeded/used at boot.
const (
	secretSigningKey   = "wardyn-signing-key" // embedded identity ES256 PEM
	secretGitHubAppID  = "github-app-id"      // GitHub App numeric id
	secretGitHubAppKey = "github-app-key"     // GitHub App PEM private key
	secretSessionKey   = "wardyn-session-key" // OIDC session-cookie HMAC key (32 bytes)
)

// Host-sensor (eBPF ground-truth) token parameters. The audience MUST match the
// api package's groundtruthAudience (kept in sync as a literal because that
// const is unexported). The sentinel run id is fixed: the host sensor is
// host-scoped, not per-run, and the ground-truth auth middleware verifies only
// the audience (it ignores the run claims).
const (
	groundtruthAudience  = "wardyn-groundtruth"
	groundtruthSensorSub = "wardyn-tetragon-ingest"
)

// groundtruthSensorRunID is the fixed sentinel run id the host-sensor token is
// bound to (uuid.Nil): the sensor is host-scoped, not per-run.
var groundtruthSensorRunID = uuid.Nil

func main() {
	if err := run(); err != nil {
		log.Fatalf("wardynd: %v", err)
	}
}

func run() error {
	var (
		dsn            = flagEnv("dsn", "WARDYN_PG_DSN", "", "Postgres DSN (required)")
		migrateDSN     = flagEnv("migrate-dsn", "WARDYN_PG_MIGRATE_DSN", "", "OPTIONAL Postgres DSN for an owner/migrator role that runs migrations; when set, WARDYN_PG_DSN is used ONLY for the least-privilege runtime app pool (enables audit_events DDL protection). Empty = single-DSN mode (no DDL protection, unchanged behavior).")
		listen         = flagEnv("listen", "WARDYN_LISTEN", ":8080", "HTTP listen address")
		tlsCert        = flagEnv("tls-cert", "WARDYN_TLS_CERT", "", "path to the TLS certificate (PEM); enables built-in TLS when set together with -tls-key")
		tlsKey         = flagEnv("tls-key", "WARDYN_TLS_KEY", "", "path to the TLS private key (PEM); enables built-in TLS when set together with -tls-cert")
		tlsTerminated  = flagBool("tls-terminated", "WARDYN_TLS_TERMINATED", false, "set when TLS terminates at an upstream reverse proxy; marks session cookies Secure even though wardynd itself serves plain HTTP")
		adminToken     = flagEnv("admin-token", "WARDYN_ADMIN_TOKEN", "", "admin bearer token gating the public API")
		localMode      = flagBool("local-mode", "WARDYN_LOCAL_MODE", false, "LOCAL HOST MODE: bypass public-API auth (no SSO/token) and attribute actions to the local operator. Single-developer localhost use only — refused on a publicly-routable bind. Sidecar/run-token auth is unaffected. Auto-enabled when no auth is configured AND the bind is loopback.")
		localOperator  = flagEnv("local-operator", "WARDYN_LOCAL_OPERATOR", "", "operator principal stamped on runs/approvals/audit in -local-mode (default: local:<os-user>)")
		localTrustFwd  = flagBool("local-trust-forwarder", "WARDYN_LOCAL_TRUST_FORWARDER", false, "in -local-mode, accept a non-loopback request peer (the no-auth bypass otherwise requires a loopback TCP peer). COMPOSE/TEAM ONLY: safe solely when the port is published loopback-only (127.0.0.1:PORT) so the peer is always the docker gateway. NEVER set on a directly-bound host-mode wardynd — it re-opens LAN no-auth access.")
		uiDir          = flagEnv("ui-dir", "WARDYN_UI_DIR", "", "directory holding the built web UI (optional)")
		runnerSel      = flagEnv("runner", "WARDYN_RUNNER", "none", `runner driver: "docker" | "none"`)
		identitySel    = flagEnv("identity", "WARDYN_IDENTITY", "embedded", `identity provider (pluggable seam): "embedded" (default)`)
		secretStoreSel = flagEnv("secret-store", "WARDYN_SECRET_STORE", "pg", `secret store (pluggable seam): "pg" (default)`)
		recordingSel   = flagEnv("recording-store", "WARDYN_RECORDING_STORE", "fs", `recording store (pluggable seam): "fs" (default)`)
		confinementMap = flagEnv("confinement-map", "WARDYN_CONFINEMENT_MAP", "", `optional per-class substrate/runtime pins making CC3 runtime-pluggable, e.g. "CC2=runsc;CC3=kata-qemu" (or "CC3=oci:kata-qemu"); empty = built-in defaults`)
		trustDomain    = flagEnv("trust-domain", "WARDYN_TRUST_DOMAIN", embedded.DefaultTrustDomain, "SPIFFE trust domain")
		controlURL     = flagEnv("control-plane-url", "WARDYN_CONTROL_PLANE_URL", "http://wardynd:8080", "externally-reachable control plane URL for sidecars")
		policyPath     = flagEnv("default-policy", "WARDYN_DEFAULT_POLICY", "examples/policies/default.json", "path to the default RunPolicy spec JSON")
		composerCfg    = flagEnv("composer-config", "WARDYN_COMPOSER_CONFIG", "", "AI Run Composer registry config: a JSON file path or inline JSON ({default,backends}); empty disables the composer")
		ageKey         = flagEnv("age-key", "WARDYN_AGE_KEY", "", "age X25519 identity (AGE-SECRET-KEY-...) for the secret store; generated+logged if empty")
		proxyImage     = flagEnv("proxy-image", "WARDYN_PROXY_IMAGE", "", "OCI image for the wardyn-proxy sidecar (docker runner)")

		recordingDir = flagEnv("recording-dir", "WARDYN_RECORDING_DIR", "./data/recordings", "directory for stored PTY session recordings (asciicast); empty disables replay")
		auditSinks   = flagEnv("audit-sinks", "WARDYN_AUDIT_SINKS", "", "audit sink config JSON (file/webhook/syslog); empty disables fanout")
		auditSpool   = flagEnv("audit-spool", "WARDYN_AUDIT_SPOOL", "./data/audit-spool.jsonl", "local append-only JSONL fallback for audit events whose Postgres write fails (durability so a security event is never lost); empty disables")

		oidcIssuer       = flagEnv("oidc-issuer", "WARDYN_OIDC_ISSUER", "", "OIDC public issuer URL — browser-facing, matches the id_token iss (enables human SSO when set)")
		oidcInternalIss  = flagEnv("oidc-internal-issuer", "WARDYN_OIDC_INTERNAL_ISSUER", "", "OIDC issuer URL reachable from wardynd for server-side calls (e.g. http://dex:5556); defaults to the public issuer")
		oidcClientID     = flagEnv("oidc-client-id", "WARDYN_OIDC_CLIENT_ID", "", "OIDC client id")
		oidcClientSecret = flagEnv("oidc-client-secret", "WARDYN_OIDC_CLIENT_SECRET", "", "OIDC client secret")
		oidcRedirectURL  = flagEnv("oidc-redirect-url", "WARDYN_OIDC_REDIRECT_URL", "", "OIDC redirect URL (<base>/auth/callback)")
		oidcEmailDomains = flagEnv("oidc-email-domains", "WARDYN_OIDC_EMAIL_DOMAINS", "", "comma-separated allowed email domains (empty = any verified email)")

		autoStopInterval = flagDuration("autostop-interval", "WARDYN_AUTOSTOP_INTERVAL", time.Minute, "how often the lifecycle reaper scans for idle runs (0 disables)")

		approvalExpiryInterval = flagDuration("approval-expiry-interval", "WARDYN_APPROVAL_EXPIRY_INTERVAL", 10*time.Minute, "how often to sweep stale PENDING approvals (0 disables)")
		approvalExpiryAfter    = flagDuration("approval-expiry-after", "WARDYN_APPROVAL_EXPIRY_AFTER", 24*time.Hour, "PENDING approvals older than this are transitioned to EXPIRED")

		envbuild     = flagBool("envbuild", "WARDYN_ENVBUILD", false, "enable devcontainer image builds for create-run (requires -tags docker)")
		envbuildImg  = flagEnv("envbuild-image", "WARDYN_ENVBUILD_IMAGE", "", "envbuilder OCI image override (empty = upstream default)")
		envbuildRepo = flagEnv("envbuild-cache-repo", "WARDYN_ENVBUILD_CACHE_REPO", "", "optional OCI registry ref for envbuilder layer cache (enables safe daemonless push mode)")

		// agentImages is a JSON object mapping agent names to OCI image refs
		// (e.g. '{"claude-code":"wardyn/agent-claude-code:local"}'). When set,
		// named agents use the specified image instead of the ghcr convention.
		// Must be valid JSON when non-empty; validated at boot (fail closed).
		agentImagesJSON = flagEnv("agent-images", "WARDYN_AGENT_IMAGES", "", `JSON map of agent-name -> OCI image ref; overrides ghcr convention for named agents (env WARDYN_AGENT_IMAGES)`)
		agentModel      = flagEnv("agent-anthropic-model", "WARDYN_AGENT_ANTHROPIC_MODEL", "", `optional: pin ANTHROPIC_MODEL inside claude-code sandboxes (e.g. "opus") so the agent doesn't use the account/CLI default (which a promo can push to Fable). Empty = CLI default.`)
		scanAIAdvisor   = flagBool("scan-ai-advisor", "WARDYN_SCAN_AI_ADVISOR", false, "enable the ADVISORY AI workspace-scan fallback: when the deterministic scanner is unsure (low confidence / unrecognized build system), a resident read-only coding-agent CLI gap-fills EMPTY profile fields and forces needs_review. Advisory-only + fail-open (never overrides a deterministic fact, never fails the scan upload). Requires a resident claude CLI on the host PATH. Off = deterministic-only (default).")

		// Bedrock: an enterprise Anthropic transport (no direct Anthropic egress,
		// billed via AWS). Both must be set to enable it; the AWS credentials
		// themselves are NOT flags — they come from the secret store
		// (aws-access-key-id/aws-secret-access-key/aws-session-token), read at
		// dispatch time since Bedrock's SigV4 request signing can't be
		// proxy-injected. See internal/api.Config.BedrockRegion/BedrockModel.
		bedrockRegion       = flagEnv("bedrock-region", "WARDYN_BEDROCK_REGION", "", `optional: AWS region for the Amazon Bedrock Anthropic transport (e.g. "us-east-1"). Requires -bedrock-model too, plus aws-access-key-id/aws-secret-access-key secrets. Empty = Bedrock disabled.`)
		bedrockModel        = flagEnv("bedrock-model", "WARDYN_BEDROCK_MODEL", "", `optional: Bedrock model id for claude-code (a cross-region inference-profile id, e.g. "us.anthropic.claude-sonnet-4-5-...", not a bare foundation-model id). Requires -bedrock-region too.`)
		bedrockAWSDir       = flagEnv("bedrock-aws-dir", "WARDYN_BEDROCK_AWS_DIR", "", `HOST MODE ONLY: bind a host ~/.aws directory READ-ONLY into each Bedrock run so the AWS SDK resolves credentials itself, including auto-refreshing AWS SSO. Avoids pasting static aws-access-key-id/-secret secrets (which expire under SSO). Leave empty for team/compose deployments.`)
		bedrockAWSProfile   = flagEnv("bedrock-aws-profile", "WARDYN_BEDROCK_AWS_PROFILE", "", `optional: AWS_PROFILE to select from the mounted ~/.aws (common with SSO). Only used with -bedrock-aws-dir.`)
		bedrockAWSSSORegion = flagEnv("bedrock-aws-sso-region", "WARDYN_BEDROCK_AWS_SSO_REGION", "", `optional: AWS SSO region whose oidc.<r>/portal.sso.<r> endpoints the sandbox may reach to exchange an SSO token for role creds. Defaults to -bedrock-region. Only used with -bedrock-aws-dir.`)

		// proxyURL overrides the WARDYN_PROXY_URL injected into sandbox env.
		// Defaults to "http://wardyn-proxy:3128" (per-run sidecar docker alias).
		proxyURL = flagEnv("proxy-url", "WARDYN_PROXY_URL_OVERRIDE", "", `sandbox WARDYN_PROXY_URL override (default http://wardyn-proxy:3128)`)

		// printGroundtruthToken, when set, mints a host-sensor token
		// (aud="wardyn-groundtruth") for the eBPF/Tetragon ground-truth ingest
		// sidecar, prints it to stdout, and exits. This is how compose seeds
		// WARDYN_GROUNDTRUTH_TOKEN. The token grants ONLY audit-write on
		// POST /api/v1/internal/groundtruth — it can never mint or approve
		// (those endpoints verify aud="wardyn-internal"). Fail-closed: minting
		// requires the identity provider; the token has the provider's standard
		// 1h TTL (operators re-mint on rotation).
		printGroundtruthToken = flagBool("print-groundtruth-token", "WARDYN_PRINT_GROUNDTRUTH_TOKEN", false, "mint and print a host-sensor token (aud=wardyn-groundtruth) for wardyn-tetragon-ingest, then exit")

		// genAgeKey, when set, prints a freshly-generated age X25519 identity
		// (AGE-SECRET-KEY-...) to stdout and exits — BEFORE any DSN/DB work — so
		// `docker run --rm wardyn/wardynd:local -gen-age-key` can mint a durable
		// WARDYN_AGE_KEY with no Postgres.
		genAgeKey = flagBool("gen-age-key", "WARDYN_GEN_AGE_KEY", false, "generate a fresh age X25519 identity (AGE-SECRET-KEY-...) to stdout for WARDYN_AGE_KEY, then exit (no DSN required)")
	)
	flag.Parse()

	// -gen-age-key: EARLY EXIT before validateConfig / any DB or pool work, so it
	// needs no DSN. Mirrors the -print-groundtruth-token early-exit pattern.
	if *genAgeKey {
		return genAndPrintAgeKey(os.Stdout)
	}

	// Validate + derive the TLS/DSN posture from the resolved flag/env values.
	// Extracted into a pure helper (validateConfig) so the fail-closed rules —
	// DSN required, TLS cert+key both-or-neither, Secure-cookie derivation — are
	// unit-testable without standing up the whole daemon.
	posture, err := validateConfig(*dsn, *tlsCert, *tlsKey, *tlsTerminated)
	if err != nil {
		return err
	}
	tlsEnabled := posture.tlsEnabled
	secureCookies := posture.secureCookies

	// Parse the agent images map at boot so a malformed value fails closed
	// immediately rather than silently using the convention for all agents.
	var agentImages map[string]string
	if *agentImagesJSON != "" {
		if err := json.Unmarshal([]byte(*agentImagesJSON), &agentImages); err != nil {
			return fmt.Errorf("parse WARDYN_AGENT_IMAGES: %w", err)
		}
		log.Printf("wardynd: agent image overrides: %v", agentImages)
	}

	// LOCAL HOST MODE (single-developer localhost path): bypass public-API auth so
	// the browser UI works with no SSO/Dex and no token. Auto-enable when no auth
	// is configured AND the bind is loopback; otherwise honor the explicit flag.
	// FAIL CLOSED: never serve a no-auth public API on a publicly-routable IP. The
	// sidecar/run-token path (internalAuth) is unaffected either way.
	loopbackBind := listenIsLoopback(*listen)
	effLocalMode := *localMode || (*adminToken == "" && *oidcIssuer == "" && loopbackBind)
	localOp := strings.TrimSpace(*localOperator)
	if effLocalMode {
		if listenIsRoutablePublic(*listen) {
			return fmt.Errorf("refusing to start: -local-mode bypasses authentication but the listen address %q is a publicly-routable IP; bind to loopback (127.0.0.1) or a private address, or configure auth (WARDYN_ADMIN_TOKEN / OIDC)", *listen)
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
		if *localTrustFwd {
			if listenBindsSpecificRoutable(*listen) {
				return fmt.Errorf("refusing to start: -local-trust-forwarder disables the loopback-peer gate but the listen address %q binds a specific non-loopback interface — this re-opens UNAUTHENTICATED LAN admin access; use it ONLY when the port is published loopback-only (127.0.0.1:PORT), i.e. bind loopback here or an unspecified address inside a compose container", *listen)
			}
			if !loopbackBind {
				log.Printf("wardynd: ERROR -local-trust-forwarder on unspecified bind %s DISABLES the loopback-peer gate — the UNAUTHENTICATED public API is exposed to the LAN unless the host publishes 127.0.0.1:PORT ONLY (the Compose default). Verify your docker publish / host firewall.", *listen)
			}
		}
		if localOp == "" {
			localOp = defaultLocalOperator()
		}
		if loopbackBind {
			log.Printf("wardynd: LOCAL HOST MODE — public-API auth disabled; operator=%q (loopback bind %s). No SSO/token required.", localOp, *listen)
		} else {
			log.Printf("wardynd: WARNING LOCAL HOST MODE on a non-loopback bind %s — the UNAUTHENTICATED public API is reachable beyond localhost; ensure a host firewall or configure auth. operator=%q", *listen, localOp)
		}

		// Host-mode Bedrock auto-detect: region+model configured but NO credential
		// source given (no -bedrock-aws-dir, no aws-*/bedrock-api-key secrets) — the
		// exact "Needs setup" state where the operator already runs Claude on Bedrock
		// via their host ~/.aws. Default the read-only mount to ~/.aws so the AWS SDK
		// resolves their creds (SSO auto-refreshes) with nothing to paste. Host-mode
		// only + fail-safe: only when ~/.aws actually exists, so resolveBedrockAuth
		// still falls through cleanly otherwise.
		if *bedrockRegion != "" && *bedrockModel != "" && *bedrockAWSDir == "" {
			if home, herr := os.UserHomeDir(); herr == nil {
				awsDir := filepath.Join(home, ".aws")
				if st, serr := os.Stat(awsDir); serr == nil && st.IsDir() {
					*bedrockAWSDir = awsDir
					log.Printf("wardynd: host-mode Bedrock — no credential configured; auto-mounting %s read-only (the AWS SDK resolves your host creds, SSO auto-refreshes)", awsDir)
				}
			}
		}
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Connect + migrate (Postgres is the only required dependency).
	bootCtx, cancel := context.WithTimeout(rootCtx, 30*time.Second)
	defer cancel()
	pool, err := db.Connect(bootCtx, *dsn)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	// N4 (DDL protection via role separation): when WARDYN_PG_MIGRATE_DSN is set,
	// run migrations through a SEPARATE owner/migrator pool and keep the main app
	// pool on the least-privilege WARDYN_PG_DSN role — so a compromised app role
	// cannot DROP/DISABLE the audit_events append-only triggers. When unset,
	// behavior is EXACTLY as before (single DSN), but we log an honest notice that
	// audit_events is not DDL-protected without the role split.
	if mDSN := strings.TrimSpace(*migrateDSN); mDSN != "" {
		mpool, merr := db.Connect(bootCtx, mDSN)
		if merr != nil {
			return fmt.Errorf("connect migrate db: %w", merr)
		}
		merr = db.Migrate(bootCtx, mpool)
		mpool.Close()
		if merr != nil {
			return fmt.Errorf("migrate: %w", merr)
		}
		// Do NOT assume the split delivered protection: VERIFY the app role
		// (WARDYN_PG_DSN) is actually a non-owner, non-superuser of audit_events.
		// An operator who pointed WARDYN_PG_MIGRATE_DSN at the same (or another
		// owner/superuser) role gets no protection — logging "protected"
		// unconditionally would be an overclaim (invariant 5).
		protected, perr := db.AuditDDLProtected(bootCtx, pool)
		if perr != nil {
			return fmt.Errorf("verify audit ddl protection: %w", perr)
		}
		if protected {
			log.Printf("wardynd: migrations applied via WARDYN_PG_MIGRATE_DSN (owner/migrator role); app role is a verified non-owner of audit_events — the append-only guard is DDL-protected")
		} else {
			log.Printf("wardynd: WARNING WARDYN_PG_MIGRATE_DSN is set but the app role (WARDYN_PG_DSN) still owns audit_events or is a superuser — DDL protection is NOT in effect; connect wardynd as a distinct non-owner role that has only INSERT/SELECT on audit_events")
		}
	} else {
		if err := db.Migrate(bootCtx, pool); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		log.Printf("wardynd: NOTICE single-DSN mode — wardynd's DB role owns audit_events, so DROP TRIGGER / ALTER TABLE ... DISABLE TRIGGER / DROP TABLE bypass the append-only guard. Set WARDYN_PG_MIGRATE_DSN to a separate owner/migrator role (wardynd then connects as a non-owner app role) for DDL protection.")
	}

	// Audit recorder: the Postgres store is the source of truth. When audit
	// sinks are configured, wrap it in a fanoutRecorder so every persisted event
	// ALSO fans out to file/webhook/syslog. The store write is authoritative;
	// fanout failures are logged and never fail the primary record (invariant 6:
	// audit is append-only and free — and must never gate the write).
	var auditRec audit.Recorder = store.Recorder{Pool: pool}
	fan, ferr := buildAuditFanout(rootCtx, *auditSinks)
	if ferr != nil {
		return ferr
	}
	if fan != nil {
		auditRec = fanoutRecorder{primary: store.Recorder{Pool: pool}, fanout: fan}
		log.Printf("wardynd: audit fanout enabled")
	}
	// SecretRegistry: process-wide secret masking registry. Minted github_token
	// values and resolved api_key injection values are registered here so they
	// can be masked ("<secret-hidden>") from PTY/asciicast captures and audit
	// event fields before they leave the control-plane process. Constructed early
	// so the identity provider and broker mask their audit events too.
	maskReg := secretmask.NewRegistry()
	// maskedRec is the masked + fanned-out recorder. It wraps auditRec (the
	// fanoutRecorder when sinks are configured, else the plain store recorder)
	// so EVERY component that records audit events — the identity provider and
	// the token broker included — has ev.Data/ev.Target scrubbed of registered
	// secrets AND fans out to the SIEM sinks.
	//
	// SECURITY (audit fanout gap): previously the idp and broker were handed a
	// masked recorder that wrapped the plain store recorder (no fanout), so
	// broker credential.* events and identity events were persisted to Postgres
	// but NEVER reached file/webhook/syslog sinks. Wrapping auditRec here closes
	// that gap while keeping masking applied. (The masking is byte-verbatim
	// defense-in-depth — these events carry no raw secret today.)
	// Durable audit fallback (C1/H9): when the primary Postgres write fails, the
	// event is spooled to a local append-only JSONL file so it is never silently
	// lost. Constructed HERE — before the recorder chain — so it can sit BELOW
	// masking and be shared by EVERY audit writer (API, broker, identity, approvals,
	// sweeper), not just the API server. Best-effort: a spool that cannot be opened
	// degrades to log-only, never blocking startup.
	var auditFallback *api.AuditSpool
	if strings.TrimSpace(*auditSpool) != "" {
		af, aerr := api.NewAuditSpool(*auditSpool)
		if aerr != nil {
			log.Printf("wardynd: WARNING audit spool unavailable at %s: %v (failed audit writes will be logged only)", *auditSpool, aerr)
		} else {
			auditFallback = af
			log.Printf("wardynd: audit fallback spool at %s", *auditSpool)
		}
	}
	// Recorder chain: maskingRecorder → spoolingRecorder → auditRec (fanout → store).
	// Masking is outermost so the spool (and the store, and the SIEM sinks) all
	// receive the already-masked event — the H9 fix for the spool that previously
	// wrote the PRE-masking event.
	maskedRec := maskingRecorder{inner: spoolingRecorder{inner: auditRec, spool: auditFallback}, reg: maskReg}

	// Secret store (pluggable seam; default "pg" = age-encrypted Postgres column).
	secrets, err := buildSecretStore(pool, *ageKey, *secretStoreSel)
	if err != nil {
		return err
	}

	// Embedded identity provider: signing key persisted in the secret store,
	// generated on first boot. The pg-backed revocation store is the kill-switch
	// denylist (identity_revocations).
	signKey, err := loadOrCreateSigningKey(bootCtx, secrets)
	if err != nil {
		return err
	}
	// Identity provider (pluggable seam; default "embedded"). pgRevocations is the
	// pg-backed kill-switch denylist, supplied to whichever provider is selected.
	idp, err := identity.New(*identitySel, identity.Deps{
		SigningKey:  signKey,
		TrustDomain: *trustDomain,
		Revocations: &pgRevocations{pool: pool},
		Audit:       maskedRec,
	})
	if err != nil {
		return fmt.Errorf("identity provider: %w", err)
	}

	// Host-sensor token minting (-print-groundtruth-token). Mint a token bound
	// to the SEPARATE aud="wardyn-groundtruth" so the eBPF/Tetragon ingest
	// sidecar can authenticate to POST /api/v1/internal/groundtruth. The token
	// is audit-write-only by construction: the mint/approval endpoints verify
	// aud="wardyn-internal" and reject this audience. We bind it to a fixed
	// sentinel run id (it is host-scoped, not per-run); the ground-truth auth
	// middleware ignores the run claims and checks only the audience. Print and
	// exit so this slots cleanly into a compose token-seeding step.
	if *printGroundtruthToken {
		mintCtx, mintCancel := context.WithTimeout(rootCtx, 10*time.Second)
		defer mintCancel()
		ri, merr := idp.MintRunIdentity(mintCtx, groundtruthSensorRunID, groundtruthSensorSub, groundtruthSensorSub, groundtruthAudience)
		if merr != nil {
			return fmt.Errorf("mint groundtruth token: %w", merr)
		}
		fmt.Println(ri.Token)
		return nil
	}

	// auditRec becomes the masked + fanned-out recorder handed to the API server
	// and the lifecycle reaper — the SAME recorder the idp and broker already
	// hold (maskedRec). This guarantees a single audit path: every event is
	// masked, persisted, and fanned out to the SIEM sinks. The MaskRegistry
	// field in api.Config additionally gates the recording-upload and
	// injection-resolve paths.
	auditRec = maskedRec

	// Token broker: GitHub minter only when the App credentials are present;
	// otherwise github_token grants fail closed at mint with a clear error.
	// The broker shares maskedRec so its credential.* events fan out to SIEM.
	gh := buildGitHubMinter(secrets)
	brk := broker.New(broker.NewPgxStore(pool), secrets, maskedRec, idp, gh).WithMaskRegistry(maskReg)

	// Approval FSM service (adapter over internal/approval + internal/store).
	// FIX #5: wired with maskedRec (masked + SIEM fanout), matching idp/broker —
	// approval.decide events now reach file/webhook/syslog sinks, not just Postgres.
	approvals := &approvalService{pool: pool, rec: maskedRec}

	// Runner (optional). docker | none. The docker driver is compiled in only
	// under the "docker" build tag (parity rule: the control plane carries zero
	// target-specific code by default; the driver is a pluggable add-on). Build
	// wardynd with `-tags docker` to enable it.
	// Confinement substrate/runtime pins (pluggable CC3): parsed fail-closed so a
	// typo never silently downgrades isolation. Empty => built-in defaults.
	confRuntimes, err := parseConfinementMap(*confinementMap)
	if err != nil {
		return err
	}

	var run runner.Runner
	// M31: reflect the ACTUAL resolved runner. Previously hardcoded to "docker"
	// before the switch, so /healthz reported the sandbox component Selected=docker
	// even under -runner none (runs actually stay PENDING) — a truthfulness gap.
	runnerTarget := "none"
	switch *runnerSel {
	case "docker":
		d, derr := newDockerRunner(*proxyImage, confRuntimes)
		if derr != nil {
			return fmt.Errorf("docker runner: %w", derr)
		}
		run = d
		runnerTarget = "docker"
		log.Printf("wardynd: docker runner enabled (proxy image %q)", *proxyImage)
	case "none", "":
		log.Printf("wardynd: no runner selected; runs stay PENDING (headless API-only)")
	default:
		return fmt.Errorf("unknown -runner %q (want docker|none)", *runnerSel)
	}

	defaultPolicy, err := api.LoadPolicySpec(*policyPath)
	if err != nil {
		return err
	}

	if *adminToken == "" && !effLocalMode {
		log.Printf("wardynd: WARNING admin token unset; the public API is DISABLED (only /healthz responds). Set WARDYN_ADMIN_TOKEN, enable OIDC, or use -local-mode for single-developer localhost use.")
	}

	// Recording store (pluggable seam; default "fs"). The fs store serves replays
	// and accepts wardyn-rec uploads. Empty -recording-dir => a nil store (replay
	// disabled), the same as before.
	recStore, rerr := recording.New(*recordingSel, recording.Deps{Dir: *recordingDir})
	if rerr != nil {
		return fmt.Errorf("recording store: %w", rerr)
	}
	if recStore != nil {
		log.Printf("wardynd: recording store (%s) at %s", *recordingSel, *recordingDir)
	}

	// Human SSO (OIDC), optional. The session-cookie HMAC key is loaded from the
	// secret store ("wardyn-session-key"), generated and persisted on first boot.
	var authn *oidc.Authenticator
	if *oidcIssuer != "" {
		sessKey, kerr := loadOrCreateSessionKey(bootCtx, secrets)
		if kerr != nil {
			return kerr
		}
		authn, err = oidc.New(rootCtx, oidc.Config{
			IssuerURL:           *oidcIssuer,
			InternalIssuerURL:   *oidcInternalIss,
			ClientID:            *oidcClientID,
			ClientSecret:        *oidcClientSecret,
			RedirectURL:         *oidcRedirectURL,
			AllowedEmailDomains: splitCSV(*oidcEmailDomains),
			SecureCookies:       secureCookies,
		}, sessKey)
		if err != nil {
			return fmt.Errorf("oidc: %w", err)
		}
		log.Printf("wardynd: OIDC SSO enabled (issuer=%s)", *oidcIssuer)
		log.Printf("wardynd: NOTE human SSO / team mode is EXPERIMENTAL — a first-class team deployment is coming soon; the UI's 'Sign in with SSO' button is disabled, so use the admin token or the CLI for now")
	}

	// Devcontainer image builder (optional; docker build tag only). When -envbuild
	// is set but wardynd was not built with -tags docker, newEnvBuilder returns an
	// error so the misconfiguration fails closed at boot rather than silently.
	var imgBuilder api.ImageBuilder
	if *envbuild {
		b, berr := newEnvBuilder(*envbuildImg, *envbuildRepo)
		if berr != nil {
			return fmt.Errorf("envbuild: %w", berr)
		}
		imgBuilder = b
		log.Printf("wardynd: devcontainer builds enabled")
	}

	// AI Run Composer (optional): build the backend registry from -composer-config.
	// Nil when unconfigured, which disables the compose endpoints (fail closed).
	composerReg, composerReadiness, err := buildComposerRegistry(*composerCfg, secrets)
	if err != nil {
		return fmt.Errorf("composer: %w", err)
	}
	// Map the boot-snapshot readiness onto the api wire type for /setup/status.
	// backends.BackendReadiness and api.ComposerBackendReadiness have identical
	// fields/types/order, so this is a plain Go struct conversion (tags ignored).
	var composerBackends []api.ComposerBackendReadiness
	for _, b := range composerReadiness {
		composerBackends = append(composerBackends, api.ComposerBackendReadiness(b))
	}
	if composerReg != nil && composerReg.Enabled() {
		names := make([]string, 0)
		for _, b := range composerReg.List() {
			names = append(names, b.Name)
		}
		log.Printf("wardynd: AI Run Composer enabled (backends=%v default=%q)", names, composerReg.Default())
	}

	// Pluggable-component selection advertised on /healthz. "selected" is the
	// ACTUAL running impl; "recommended_production" is the standard Wardyn
	// recommends converging to (may differ from the shipped default — the honest
	// recommended-vs-shipped split, see docs/PLUGGABILITY.md).
	sourceOf := func(selected, def string) string {
		if selected == def {
			return "default"
		}
		return "configured"
	}
	components := map[string]api.ComponentInfo{
		"identity":      {Selected: *identitySel, RecommendedProduction: "spire", Source: sourceOf(*identitySel, "embedded")},
		"secret_store":  {Selected: *secretStoreSel, RecommendedProduction: "openbao", Source: sourceOf(*secretStoreSel, "pg")},
		"recording":     {Selected: *recordingSel, RecommendedProduction: "fs", Source: sourceOf(*recordingSel, "fs")},
		"policy_engine": {Selected: "builtin", RecommendedProduction: "opa"},
		"sandbox":       {Selected: runnerTarget, RecommendedProduction: "kata-cc3", Source: sourceOf(*runnerSel, "none")},
	}

	// Subscription OAuth token provider: yields the operator's LIVE Anthropic
	// access token from the resident ~/.claude so subscription runs are
	// credentialed PROXY-SIDE (the sandbox holds an inert sentinel that never
	// goes stale) instead of a copy whose refresh token rotates out from under it.
	// Constructed unconditionally; it only reads/refreshes when a subscription run
	// resolves its injection. Escape hatch: WARDYN_SUBSCRIPTION_INJECT=off keeps
	// the legacy resident-copy behavior.
	subToken, subErr := subscription.New(subscription.Config{})
	if subErr != nil {
		log.Printf("wardynd: subscription token provider unavailable (%v); subscription runs fall back to the resident-copy behavior", subErr)
		subToken = nil
	}
	disableSubInject := strings.EqualFold(strings.TrimSpace(os.Getenv("WARDYN_SUBSCRIPTION_INJECT")), "off")

	// Managed subscription token: a long-lived `claude setup-token` captured via
	// the container-login flow and stored age-encrypted. Serves subscription runs
	// PROXY-SIDE in deployments (compose) whose distroless wardynd has no host
	// ~/.claude for subToken above. Store-only (no Server dependency, no cycle);
	// nil when there is no secret store.
	managedToken := api.NewManagedCredProvider(secrets, "anthropic")

	// Advisory AI scan fallback (opt-in): wired to the fail-open
	// workspacescan.AdviseProfile with a bounded timeout so a slow/hung CLI can
	// never stall — let alone fail — the sidecar's scan upload. nil = OFF.
	var scanAdvisor func(context.Context, workspacescan.ScanFacts, workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile
	if *scanAIAdvisor {
		scanAdvisor = func(ctx context.Context, facts workspacescan.ScanFacts, base workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile {
			return workspacescan.AdviseProfile(ctx, facts, base, workspacescan.AIOptions{Timeout: 60 * time.Second})
		}
		log.Printf("wardynd: advisory AI workspace-scan fallback ENABLED (WARDYN_SCAN_AI_ADVISOR); advisory-only + fail-open, needs a resident read-only claude CLI on PATH")
	}

	srv := api.New(api.Config{
		Store:                     store.NewPG(pool),
		Identity:                  idp,
		Approvals:                 approvals,
		Broker:                    brk,
		Audit:                     auditRec,
		Runner:                    run,
		AdminToken:                *adminToken,
		LocalMode:                 effLocalMode,
		LocalOperator:             localOp,
		TrustDomain:               *trustDomain,
		DefaultPolicy:             defaultPolicy,
		RunnerTarget:              runnerTarget,
		UIDir:                     *uiDir,
		ControlPlaneURL:           *controlURL,
		RecordingStore:            recStore,
		OIDC:                      authn,
		ImageBuilder:              imgBuilder,
		AgentImages:               agentImages,
		AgentAnthropicModel:       *agentModel,
		BedrockRegion:             *bedrockRegion,
		BedrockModel:              *bedrockModel,
		BedrockAWSConfigDir:       *bedrockAWSDir,
		BedrockAWSProfile:         *bedrockAWSProfile,
		BedrockAWSSSORegion:       *bedrockAWSSSORegion,
		ProxyURL:                  *proxyURL,
		Secrets:                   secrets,
		MaskRegistry:              maskReg,
		SubscriptionToken:         subToken,
		ManagedToken:              managedToken,
		DisableSubscriptionInject: disableSubInject,
		Composer:                  composerReg,
		Components:                components,
		ScanAIAdvisor:             scanAdvisor,
		// First-run setup readiness inputs (GET /api/v1/setup/status).
		AgeKeyDurable:       strings.TrimSpace(*ageKey) != "",
		LocalLoopback:       loopbackBind,
		LocalTrustForwarder: *localTrustFwd,
		ComposerBackends:    composerBackends,
		// rootCtx is the daemon-lifetime base context for detached background
		// work (the run completion watcher) that must outlive the create-run
		// request. It is cancelled on SIGINT/SIGTERM at shutdown.
		BaseCtx: rootCtx,
	})

	// Lifecycle reaper: stop idle RUNNING sandboxes past their policy threshold.
	// Disabled when no runner is wired (nothing to stop) or interval <= 0.
	if run != nil && *autoStopInterval > 0 {
		reaper := lifecycle.New(
			lifecycleStore{pool: pool},
			lifecycleStopper{pool: pool, runner: run, identity: idp, broker: brk},
			auditRec,
			lifecycle.Config{Interval: *autoStopInterval},
		)
		go goSafe("lifecycle.reaper", func() { reaper.Run(rootCtx) })
		log.Printf("wardynd: lifecycle reaper started (interval=%s)", *autoStopInterval)
	}

	// Groundtruth token rotator: keep a shared token file fresh so the eBPF/Tetragon
	// ingest sidecar — which re-reads the file on a 401 — recovers when its ~1h token
	// expires instead of going permanently blind (U009). Off unless a file path is
	// configured; the shipped compose stack points wardynd + the ingest at the same
	// shared-volume file. The static WARDYN_GROUNDTRUTH_TOKEN env path cannot refresh.
	if gtFile := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_FILE")); gtFile != "" {
		go goSafe("groundtruth.rotator", func() { runGroundtruthTokenRotator(rootCtx, idp, gtFile) })
		log.Printf("wardynd: groundtruth token rotator started (file=%s)", gtFile)
	}

	// Approval expiry sweeper: transition PENDING approvals older than the
	// cutoff to EXPIRED so the queue does not grow unbounded. approval.ExpireStale
	// was implemented but never scheduled; this wires it on the same goroutine
	// pattern as the reaper.
	if *approvalExpiryInterval > 0 {
		go goSafe("approval.sweeper", func() {
			// FIX #5: sweeper shares maskedRec so approval.expire events fan out to SIEM.
			runApprovalSweeper(rootCtx, approvalStore{pool: pool, rec: maskedRec}, *approvalExpiryInterval, *approvalExpiryAfter)
		})
		log.Printf("wardynd: approval expiry sweeper started (interval=%s, after=%s)", *approvalExpiryInterval, *approvalExpiryAfter)
	}

	// Boot-time reconciliation (C3): re-derive the state of any run left
	// non-terminal by a previous process (crash/restart) so it is not stranded
	// RUNNING forever with a live sandbox and un-revoked credentials. Best-effort;
	// a reconciliation error never blocks startup.
	if run != nil {
		if rerr := srv.ReconcileOnBoot(rootCtx); rerr != nil {
			log.Printf("wardynd: boot reconciliation: %v", rerr)
		}
	}

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: long-lived streaming endpoints (the attach
		// WebSocket and the fleet SSE stream) must not be killed by a whole-request
		// deadline. IdleTimeout bounds idle keep-alive connections and MaxHeaderBytes
		// caps header size (slowloris/abuse) without affecting request bodies.
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		switch {
		case tlsEnabled:
			log.Printf("wardynd: listening on %s with built-in TLS (identity=%s trust-domain=%s)", *listen, idp.Name(), *trustDomain)
			if err := httpSrv.ListenAndServeTLS(*tlsCert, *tlsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		default:
			log.Printf("wardynd: listening on %s (identity=%s trust-domain=%s)", *listen, idp.Name(), *trustDomain)
			if *tlsTerminated {
				log.Printf("wardynd: serving plain HTTP behind a TLS-terminating reverse proxy (WARDYN_TLS_TERMINATED=true); cookies marked Secure")
			} else {
				log.Printf("wardynd: WARNING serving PLAIN HTTP with no TLS — the control plane MUST be fronted by TLS for any non-localhost deployment (set WARDYN_TLS_CERT/WARDYN_TLS_KEY for built-in TLS, or WARDYN_TLS_TERMINATED=true behind a TLS-terminating reverse proxy)")
			}
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}
	}()

	select {
	case <-rootCtx.Done():
		log.Printf("wardynd: shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// Drain audit sinks last, after the HTTP server has stopped accepting new
	// requests (so no further audit events are produced). Close blocks until the
	// webhook flusher has drained and delivered its final batch and the file/
	// syslog sinks have been closed; previously sinks were never Closed on
	// shutdown, so the last batch and the drain goroutine were abandoned.
	if fan != nil {
		if cerr := fan.Close(); cerr != nil {
			log.Printf("wardynd: audit sink shutdown: %v", cerr)
		}
	}
	return nil
}

// tlsPosture is the validated TLS/cookie posture derived from the resolved
// config. tlsEnabled is true only when wardynd serves built-in TLS (cert+key both
// set); secureCookies is true when the connection is TLS-protected end to end
// (built-in TLS OR an upstream TLS-terminating proxy via WARDYN_TLS_TERMINATED).
type tlsPosture struct {
	tlsEnabled    bool
	secureCookies bool
}

// validateConfig applies the boot-time fail-closed configuration rules and
// derives the TLS/cookie posture. It is a pure function of the already-resolved
// (flag-or-env) values so it can be unit-tested in isolation:
//
//   - dsn is REQUIRED (Postgres is the only mandatory dependency).
//   - TLS cert and key are both-or-neither: setting exactly one is a
//     misconfiguration that fails closed (a half-configured TLS posture would
//     silently fall back to plain HTTP, which is worse than a loud error).
//   - secureCookies is true when TLS protects the connection end to end —
//     either wardynd serves built-in TLS, or TLS terminates at an upstream proxy
//     (tlsTerminated). When neither holds it MUST stay false: Secure cookies are
//     never sent over plain HTTP and would break login.
func validateConfig(dsn, tlsCert, tlsKey string, tlsTerminated bool) (tlsPosture, error) {
	if dsn == "" {
		return tlsPosture{}, errors.New("missing -dsn / WARDYN_PG_DSN")
	}
	if (tlsCert != "") != (tlsKey != "") {
		return tlsPosture{}, errors.New("TLS misconfigured: set BOTH -tls-cert/WARDYN_TLS_CERT and -tls-key/WARDYN_TLS_KEY, or neither")
	}
	tlsEnabled := tlsCert != "" && tlsKey != ""
	return tlsPosture{
		tlsEnabled:    tlsEnabled,
		secureCookies: tlsEnabled || tlsTerminated,
	}, nil
}

// knownPublicAgeKeys are age identities this repository has published — each was
// once a committed default, so it lives in git history forever and any secret
// encrypted under one is effectively public. wardynd refuses to start with ANY of
// them (invariant 5, fail closed): unset WARDYN_AGE_KEY to generate an ephemeral
// key, or mint your own with `wardynd -gen-age-key`.
//
// Add an entry here whenever a key is published, never remove one: a key cannot
// be un-published, and the denylist is what keeps a stale copy-pasted .env from
// silently encrypting a real secret store under a key anyone can read.
var knownPublicAgeKeys = []string{
	// Baked-in default of earlier Compose files (deploy/compose).
	"AGE-SECRET-KEY-1YGHJK4A24GHQGAL2U2ZU7M05080VNWSZ0EU9KRM3DVYKDN0XYSTS3TK3YR",
	// Committed default of scripts/run-local.sh + scripts/e2e-backend.sh. Those
	// scripts now mint an ephemeral per-boot key via `wardynd -gen-age-key`, so
	// nothing legitimate uses this one.
	"AGE-SECRET-KEY-1CMRQ5GEN2G4NKWXQQ4DKK7GSMJDZXXW69W9QN3ALX8Y49CF6RLYS7Y6KHF",
}

// isKnownPublicAgeKey reports whether ageKey is one of the published identities.
func isKnownPublicAgeKey(ageKey string) bool {
	return slices.Contains(knownPublicAgeKeys, strings.TrimSpace(ageKey))
}

// parseConfinementMap parses WARDYN_CONFINEMENT_MAP — a ";"-separated list of
// CLASS=runtime (or CLASS=substrate:runtime) pins selecting which substrate
// runtime backs each Confinement Class. It is the operator knob that makes CC3
// runtime-pluggable (e.g. "CC3=kata-qemu" to pin QEMU Kata, "CC2=runsc"). Empty
// => nil (the driver's built-in default mapping). FAIL CLOSED: an unknown class,
// malformed entry, empty runtime, or a non-"oci" substrate is a startup error,
// so a typo can never silently downgrade isolation.
func parseConfinementMap(s string) (map[types.ConfinementClass]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[types.ConfinementClass]string{}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: malformed entry %q (want CLASS=runtime)", part)
		}
		class := types.ConfinementClass(strings.TrimSpace(kv[0]))
		val := strings.TrimSpace(kv[1])
		// Optional "substrate:runtime"; only the OCI substrate exists today.
		if i := strings.Index(val, ":"); i >= 0 {
			if sub := strings.TrimSpace(val[:i]); sub != "" && sub != "oci" {
				return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: substrate %q for %s is not supported (only %q today; non-OCI VMM substrates are a future runner driver)", sub, class, "oci")
			}
			val = strings.TrimSpace(val[i+1:])
		}
		switch class {
		case types.CC1, types.CC2, types.CC3:
		default:
			return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: unknown confinement class %q (want CC1|CC2|CC3)", class)
		}
		if val == "" {
			return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: empty runtime for %s", class)
		}
		out[class] = val
	}
	return out, nil
}

// genAndPrintAgeKey writes a freshly-generated age X25519 identity
// (AGE-SECRET-KEY-...) to w. It is the body of the -gen-age-key early-exit flag,
// extracted so it is unit-testable without standing up the daemon. The printed
// key is what an operator sets as WARDYN_AGE_KEY to make the secret store durable
// (buildSecretStore parses it via age.ParseX25519Identity).
func genAndPrintAgeKey(w io.Writer) error {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return fmt.Errorf("generate age identity: %w", err)
	}
	_, err = fmt.Fprintln(w, id.String())
	return err
}

// buildSecretStore constructs the age-encrypted Postgres secret store. The age
// identity comes from -age-key; if empty one is generated and logged (operators
// MUST persist it across restarts to keep prior ciphertext readable).
func buildSecretStore(pool *pgxpool.Pool, ageKey, storeName string) (secretstore.Store, error) {
	var id *age.X25519Identity
	var err error
	if ageKey == "" {
		id, err = age.GenerateX25519Identity()
		if err != nil {
			return nil, fmt.Errorf("generate age identity: %w", err)
		}
		// F10: log the PUBLIC recipient as a fingerprint, never the secret identity.
		// The old message printed the full AGE-SECRET-KEY- to a log file created at
		// the default umask (~/.wardyn/host-wardynd.log), leaking the secret-store
		// master key. To persist, mint one with `wardynd -gen-age-key` (prints to
		// stdout by design) and set WARDYN_AGE_KEY — do not copy it out of this log.
		log.Printf("wardynd: WARNING generated ephemeral age identity (public %s); secrets are LOST on restart. Persist one with `wardynd -gen-age-key` + set WARDYN_AGE_KEY", id.Recipient().String())
	} else {
		if isKnownPublicAgeKey(ageKey) {
			return nil, fmt.Errorf("refusing to start: WARDYN_AGE_KEY is a publicly-known key (published in this repo's git history) — secrets encrypted under it are not protected; unset WARDYN_AGE_KEY to generate an ephemeral key, or mint your own with `wardynd -gen-age-key`")
		}
		id, err = age.ParseX25519Identity(ageKey)
		if err != nil {
			return nil, fmt.Errorf("parse age identity: %w", err)
		}
	}
	s, err := secretstore.New(storeName, secretstore.Deps{Pool: pool, AgeIdentity: id})
	if err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}
	return s, nil
}

// secretKeyStore is the minimal secret-store surface loadOrCreateSecret needs.
// Narrowing the dependency to Get/Put makes the load-or-create control flow
// unit-testable with a hand-rolled fake (cmd/wardynd/main_test.go) and documents
// that key bootstrap touches nothing else. secretstore.Store satisfies it.
type secretKeyStore interface {
	Get(ctx context.Context, name string) ([]byte, error)
	Put(ctx context.Context, name string, value []byte) error
}

// loadOrCreateSecret is the shared, fail-closed bootstrap for the two boot keys
// (the embedded-identity signing key and the OIDC session key).
//
// SECURITY (boot-key destruction): the previous per-key logic treated ANY
// Get error as "key not present" and then generated + Put a fresh key,
// OVERWRITING whatever ciphertext was already there. The pg secret store
// distinguishes a TRUE not-found (it wraps pgx.ErrNoRows) from an age-decrypt
// failure (a generic error). Conflating the two meant a single transient/
// permanent decrypt error silently rotated the key, invalidating every issued
// SVID and every active session cookie. We now regenerate ONLY when the key is
// genuinely absent or present-but-invalid; on any other error we FAIL CLOSED —
// return the error and never Put, so the existing ciphertext is preserved.
//
//   - valid reports whether an existing raw value is usable as-is.
//   - generate produces fresh key material to persist (called only when the key
//     is absent or invalid).
func loadOrCreateSecret(
	ctx context.Context,
	secrets secretKeyStore,
	name string,
	valid func(raw []byte) bool,
	generate func() ([]byte, error),
) ([]byte, error) {
	raw, err := secrets.Get(ctx, name)
	switch {
	case err == nil:
		if valid(raw) {
			return raw, nil
		}
		// Present but unusable (e.g. a legacy too-short session key): fall
		// through to regenerate. This is safe — the stored value cannot serve
		// its purpose anyway.
	case errors.Is(err, pgx.ErrNoRows):
		// TRUE not-found (first boot): generate + persist below.
	default:
		// Decrypt failure or any other Get error: FAIL CLOSED. Do NOT generate
		// or Put — overwriting here would destroy the existing key.
		return nil, fmt.Errorf("load secret %q: %w", name, err)
	}

	val, gerr := generate()
	if gerr != nil {
		return nil, fmt.Errorf("generate secret %q: %w", name, gerr)
	}
	if perr := secrets.Put(ctx, name, val); perr != nil {
		return nil, fmt.Errorf("persist secret %q: %w", name, perr)
	}
	return val, nil
}

// loadOrCreateSigningKey returns the embedded identity ES256 key, persisting a
// freshly-generated one into the secret store on first boot. The key never
// enters a sandbox; it lives only in the broker/control-plane process memory
// and the encrypted secret column. A decrypt error fails closed (see
// loadOrCreateSecret) rather than minting a fresh key over the old one.
func loadOrCreateSigningKey(ctx context.Context, secrets secretKeyStore) (*ecdsa.PrivateKey, error) {
	raw, err := loadOrCreateSecret(ctx, secrets, secretSigningKey,
		func(b []byte) bool { return len(b) > 0 },
		func() ([]byte, error) {
			key, gerr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if gerr != nil {
				return nil, fmt.Errorf("generate signing key: %w", gerr)
			}
			pemBytes, merr := marshalECPrivateKeyPEM(key)
			if merr != nil {
				return nil, merr
			}
			log.Printf("wardynd: generated and persisted embedded identity signing key")
			return pemBytes, nil
		},
	)
	if err != nil {
		return nil, err
	}
	key, perr := parseECPrivateKeyPEM(raw)
	if perr != nil {
		return nil, fmt.Errorf("parse stored signing key: %w", perr)
	}
	return key, nil
}

// loadOrCreateSessionKey returns the 32-byte OIDC session-cookie HMAC key,
// persisting a freshly-generated one into the secret store on first boot. Like
// the signing key it never enters a sandbox; it lives only in process memory
// and the encrypted secret column. Returning the key is safe — the caller is
// the OIDC authenticator, which never logs it. A decrypt error fails closed
// (see loadOrCreateSecret) rather than rotating every session out from under
// logged-in users.
func loadOrCreateSessionKey(ctx context.Context, secrets secretKeyStore) ([]byte, error) {
	return loadOrCreateSecret(ctx, secrets, secretSessionKey,
		func(b []byte) bool { return len(b) >= 32 },
		func() ([]byte, error) {
			key := make([]byte, 32)
			if _, gerr := rand.Read(key); gerr != nil {
				return nil, fmt.Errorf("generate session key: %w", gerr)
			}
			log.Printf("wardynd: generated and persisted OIDC session key")
			return key, nil
		},
	)
}

// goSafe runs fn with panic recovery so a panic in a DETACHED background
// goroutine (reaper, approval sweeper, completion watcher) logs and is contained
// instead of crashing the whole control plane — which would take every governed
// run and the kill-switch down with it. Use as `go goSafe("name", func(){ ... })`.
func goSafe(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("wardynd: PANIC in background goroutine %q (contained): %v", name, r)
		}
	}()
	fn()
}

// defaultLocalOperator is the local-host-mode operator principal: "local:<os-user>",
// falling back to "local:operator" when the OS user is unavailable.
func defaultLocalOperator() string {
	if u, err := user.Current(); err == nil {
		if name := strings.TrimSpace(u.Username); name != "" {
			return "local:" + name
		}
	}
	return "local:operator"
}

// listenHost extracts the host portion of a listen address, tolerating a bare
// host, a bare ":port", or "host:port".
func listenHost(listen string) string {
	if host, _, err := net.SplitHostPort(listen); err == nil {
		return strings.TrimSpace(host)
	}
	return strings.TrimSpace(listen)
}

// listenIsLoopback reports whether the listen address binds ONLY the loopback
// interface (127.0.0.0/8, ::1, or host "localhost"). An empty host (":8080") or
// 0.0.0.0/[::] binds all interfaces and is NOT loopback.
func listenIsLoopback(listen string) bool {
	host := listenHost(listen)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// listenIsRoutablePublic reports whether the listen address binds a SPECIFIC,
// globally-routable public IP (not loopback, not private/RFC1918, not link-local,
// and not the unspecified all-interfaces bind). It is the fail-closed gate for
// LocalMode: a no-auth public API must never be served on a public IP. The
// unspecified bind (":8080"/0.0.0.0) is treated as non-public here — it MIGHT
// include a public IP, so it earns a loud warning rather than a refusal (refusing
// it would block the common docker-bridge/compose single-host case).
func listenIsRoutablePublic(listen string) bool {
	host := listenHost(listen)
	if host == "" || strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // a hostname we can't classify — don't refuse
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	return ip.IsGlobalUnicast()
}

// listenBindsSpecificRoutable reports whether the listen address binds a
// SPECIFIC non-loopback interface — a private/RFC1918, link-local, or public IP
// a LAN peer can reach directly. It EXCLUDES loopback (peers are already local)
// and the unspecified all-interfaces bind (0.0.0.0/[::]), which from inside a
// container is indistinguishable from the safe compose 127.0.0.1-publish
// topology. It is the fail-closed gate for -local-trust-forwarder, which
// disables the loopback-PEER check and is therefore safe ONLY on a loopback or
// unspecified/compose bind. Unlike listenIsRoutablePublic this DELIBERATELY
// catches private and link-local too: with the peer gate disabled, those are
// LAN-reachable no-auth surfaces as well.
func listenBindsSpecificRoutable(listen string) bool {
	host := listenHost(listen)
	if host == "" || strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // a hostname we can't classify — don't refuse
	}
	return !ip.IsLoopback() && !ip.IsUnspecified()
}

func marshalECPrivateKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal ec key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func parseECPrivateKeyPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in signing key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// buildGitHubMinter arms the LAZY GitHub minter: it reads the App credentials
// (github-app-id / github-app-key) on the FIRST mint, not here, so adding those
// secrets after boot no longer needs a wardynd restart before github_token
// grants can mint. A github_token grant that reaches mint with the secrets still
// absent fails closed with a clear error. Construction only validates the secret
// NAMES; an error there is logged, not fatal.
func buildGitHubMinter(secrets secretstore.Store) broker.GitHubMinter {
	gh, err := broker.NewGitHubMinter(secrets, broker.GitHubMinterConfig{
		AppIDSecret:      secretGitHubAppID,
		PrivateKeySecret: secretGitHubAppKey,
	})
	if err != nil {
		log.Printf("wardynd: github minter unavailable: %v (github_token grants will fail closed)", err)
		return nil
	}
	log.Printf("wardynd: lazy github minter armed; App credentials (%s/%s) are read on first mint (no restart needed after adding them)", secretGitHubAppID, secretGitHubAppKey)
	return gh
}

// flagEnv/flagBool/flagDuration/splitCSV are shared with cmd/wardyn-tetragon-ingest
// via internal/cliutil (mirrored duplicates there previously).
var (
	flagEnv      = cliutil.FlagEnv
	flagBool     = cliutil.FlagBool
	flagDuration = cliutil.FlagDuration
	splitCSV     = cliutil.SplitCSV
)
