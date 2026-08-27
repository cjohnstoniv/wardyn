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
	dsn                  *string
	migrateDSN           *string
	migrateTimeout       *time.Duration
	listen               *string
	tlsCert              *string
	tlsKey               *string
	tlsTerminated        *bool
	allowPlaintextListen *bool
	adminToken           *string
	localMode            *bool
	localOperator        *string
	localTrustFwd        *bool
	// allowLocalModeWithOIDC is the escape hatch (see validateOperatorPosture's
	// allowOIDCNoOperatorList for the sibling pattern) for the refusal in
	// resolveLocalMode: an explicit -local-mode alongside a configured
	// -oidc-issuer is refused by default, because humanOrAdminAuth branches on
	// LocalMode FIRST and bypasses OIDC entirely without ever consulting it
	// (bug-rbac-1) — a configured SSO deployment must not silently lose its
	// RBAC to one stray env var.
	allowLocalModeWithOIDC *bool
	// allowSharedSubscription waives ONLY the local-mode clause of
	// subscriptionInjectPosture (boot_posture.go) — never the k8s or OIDC clauses.
	// It exists so the compose demo stack, which runs a shared admin token rather
	// than local mode, keeps working without anyone being tempted to "fix" a
	// refusal by setting WARDYN_LOCAL_MODE=true and disabling auth outright.
	allowSharedSubscription *bool
	// MEMBER-MODE DESKTOP (W-MEMB, docs/design/member-role-desktop.md). memberMode
	// asserts the topology in which the human at the keyboard is a MEMBER and the
	// operator authority lives elsewhere (an org IdP / MDM): it refuses to start
	// unless that is actually true. The four member*Roots knobs bound what a
	// member may bind into a sandbox from their own machine — parsed by
	// runner.ParseMemberMountPolicy, which fails boot closed on a malformed value
	// and returns the O4 posture warnings.
	memberMode          *bool
	memberRoots         *string
	memberRootsMap      *string
	memberWritableRoots *string
	memberWritableDeny  *string
	uiDir               *string
	runnerSel           *string
	identitySel         *string
	secretStoreSel      *string
	recordingSel        *string
	confinementMap      *string
	trustDomain         *string
	controlURL          *string
	policyPath          *string
	ageKey              *string
	proxyImage          *string

	recordingDir       *string
	recordingRetention *int
	auditSinks         *string
	auditSpool         *string
	auditSource        *string

	oidcIssuer       *string
	oidcInternalIss  *string
	oidcClientID     *string
	oidcClientSecret *string
	oidcRedirectURL  *string
	oidcEmailDomains *string
	// oidcOperatorEmails is the minimal member/admin role gate's allowlist
	// (api.Config.OperatorEmails). Empty = every authenticated human is an
	// operator, i.e. exactly the pre-existing behavior — which is REFUSED at boot
	// when OIDC is configured unless allowOIDCNoOperatorList overrides it (see
	// validateOperatorPosture).
	oidcOperatorEmails      *string
	allowOIDCNoOperatorList *bool
	// oidcRoleMap / oidcDefaultRole feed oidc.Config.RoleMap / DefaultRole (the
	// admin/member role gate the derivation layer computes — see
	// internal/auth/oidc's deriveRole). Both empty means role derivation is
	// off: every signed-in human keeps role "admin", exactly today's
	// behavior. Parsed and validated in buildOptionalFeatures (bad role value
	// in either fails boot closed).
	oidcRoleMap     *string
	oidcDefaultRole *string

	autoStopInterval *time.Duration

	approvalExpiryInterval *time.Duration
	approvalExpiryAfter    *time.Duration

	envbuild     *bool
	envbuildImg  *string
	envbuildRepo *string

	agentImagesJSON    *string
	agentModel         *string
	scanAIAdvisor      *bool
	requireOpSetEgress *bool
	gitPATBroker       *string

	bedrockRegion       *string
	bedrockModel        *string
	bedrockAWSDir       *string
	bedrockAWSProfile   *string
	bedrockAWSSSORegion *string

	proxyURL *string

	printGroundtruthToken *bool
	genAgeKey             *bool
	// rotateAgeKey is the one knob in this struct with NO WARDYN_* env pair, on
	// purpose: it is a destructive maintenance mode that re-encrypts every
	// stored secret, so it must be an explicit act on a command line. Its
	// early-exit siblings above are print-and-quit and harmless if an env var
	// turns them on; a stray WARDYN_ROTATE_AGE_KEY left in a compose .env would
	// rotate the store on EVERY boot. See rotateAgeKeyMode (rekey.go).
	rotateAgeKey *string

	// SSH gateway (C2/C3): sshListen empty = off = no listener, no new surface
	// (see resolveSSHGateway). sshAdvertise is purely advisory copy for the
	// run-detail pane's `ssh` command — never read by the gateway itself.
	// sshRoleTTL (migration 0046) bounds how stale a key's admin-override
	// stamp may be — see api.Config.SSHRoleTTL.
	sshListen    *string
	sshAdvertise *string
	sshRoleTTL   *time.Duration

	// UI-sandbox gateway (pillar 4): uiListen empty = off = no listener, no new
	// surface, exactly like sshListen. uiAdvertise/uiOriginTemplate are the
	// externally-reachable form of that listener — the console reads whichever
	// one /healthz publishes and never composes the origin itself.
	uiListen         *string
	uiAdvertise      *string
	uiOriginTemplate *string
}

// parseBootFlags declares every wardynd flag (with its WARDYN_* env fallback)
// and parses the command line. Moved verbatim out of run(); the usage strings
// carry the operator-facing documentation for each knob.
func parseBootFlags() *bootFlags {
	f := &bootFlags{
		dsn:            flagEnv("dsn", "WARDYN_PG_DSN", "", "Postgres DSN (required)"),
		migrateDSN:     flagEnv("migrate-dsn", "WARDYN_PG_MIGRATE_DSN", "", "OPTIONAL Postgres DSN for an owner/migrator role that runs migrations; when set, WARDYN_PG_DSN is used ONLY for the least-privilege runtime app pool (enables audit_events DDL protection). Empty = single-DSN mode (no DDL protection, unchanged behavior)."),
		migrateTimeout: flagDuration("migrate-timeout", "WARDYN_MIGRATE_TIMEOUT", 5*time.Minute, "how long db.Migrate may run before boot fails closed — separate from the fixed 30s connect budget so a slow migration on a large table (e.g. a new index) doesn't crash-loop the upgrade"),
		listen:         flagEnv("listen", "WARDYN_LISTEN", ":8080", "HTTP listen address"),
		tlsCert:        flagEnv("tls-cert", "WARDYN_TLS_CERT", "", "path to the TLS certificate (PEM); enables built-in TLS when set together with -tls-key"),
		tlsKey:         flagEnv("tls-key", "WARDYN_TLS_KEY", "", "path to the TLS private key (PEM); enables built-in TLS when set together with -tls-cert"),
		tlsTerminated:  flagBool("tls-terminated", "WARDYN_TLS_TERMINATED", false, "set when TLS terminates at an upstream reverse proxy; marks session cookies Secure even though wardynd itself serves plain HTTP"),
		// Refused by default (validateConfig) when NO TLS posture is configured and
		// the bind is a specific non-loopback interface — see listenBindsSpecificRoutable.
		// Loopback and the unspecified bind (":8080", the compose topology) are
		// already warn-only, unaffected by this flag.
		allowPlaintextListen:    flagBool("allow-plaintext-listen", "WARDYN_ALLOW_PLAINTEXT_LISTEN", false, "override: allow boot on a specific non-loopback bind serving plain HTTP with no TLS (normally refused — prefer -tls-cert/-tls-key or -tls-terminated)"),
		adminToken:              flagEnv("admin-token", "WARDYN_ADMIN_TOKEN", "", "admin bearer token gating the public API"),
		localMode:               flagBool("local-mode", "WARDYN_LOCAL_MODE", false, "LOCAL HOST MODE: bypass public-API auth (no SSO/token) and attribute actions to the local operator. Single-developer localhost use only — refused on a publicly-routable bind. Sidecar/run-token auth is unaffected. Auto-enabled when no auth is configured AND the bind is loopback."),
		localOperator:           flagEnv("local-operator", "WARDYN_LOCAL_OPERATOR", "", "operator principal stamped on runs/approvals/audit in -local-mode (default: local:<os-user>)"),
		localTrustFwd:           flagBool("local-trust-forwarder", "WARDYN_LOCAL_TRUST_FORWARDER", false, "in -local-mode, accept a non-loopback request peer (the no-auth bypass otherwise requires a loopback TCP peer). COMPOSE/TEAM ONLY: safe solely when the port is published loopback-only (127.0.0.1:PORT) so the peer is always the docker gateway. NEVER set on a directly-bound host-mode wardynd — it re-opens LAN no-auth access."),
		allowLocalModeWithOIDC:  flagBool("allow-local-mode-with-oidc", "WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC", false, "override: allow boot with -local-mode explicitly set alongside a configured -oidc-issuer, i.e. — silently disable the configured SSO/RBAC deployment and attribute every request to the fixed local operator (normally refused — unset -local-mode or -oidc-issuer instead)"),
		allowSharedSubscription: flagBool("allow-shared-subscription", "WARDYN_ALLOW_SHARED_SUBSCRIPTION", false, "override: allow ONE operator's Anthropic subscription credential to be injected into runs on a deployment that is not -local-mode (e.g. the compose demo stack, which uses a shared admin token). Does NOT waive the refusals on the k8s runner or a configured OIDC issuer — those are multi-user by definition, and sharing a subscription there breaches the harness vendor's per-user authentication terms. DEMO/SINGLE-USER BOXES ONLY."),
		memberMode:              flagBool("member-mode", "WARDYN_MEMBER_MODE", false, "MEMBER-MODE DESKTOP: assert that the human using this daemon is a MEMBER and the operator authority is elsewhere (an org IdP / MDM). Refuses to start unless -local-mode is off AND OIDC is configured — the two preconditions under which isOperator is false for the developer's every request. Adds no middleware; it makes the assumption checkable instead of assumed."),
		memberRoots:             flagEnv("member-workspace-roots", "WARDYN_MEMBER_WORKSPACE_ROOTS", "", "comma-separated absolute host directories a MEMBER's own local_dir workspace source may live under. A member source is allowed only if its CANONICALIZED real path is inside one of these (symlink-resolved, credential dotfiles denied regardless). Empty (the default) = members may not mount host directories at all; repos and operator-owned workspaces are unaffected. Point it at a dedicated projects dir, NEVER $HOME."),
		memberRootsMap:          flagEnv("member-workspace-roots-map", "WARDYN_MEMBER_WORKSPACE_ROOTS_MAP", "", `optional per-member override of -member-workspace-roots, as JSON {"<principal>": ["/abs/root", ...]} keyed by OIDC sub or email. A principal with an entry uses ONLY that entry — per-member REPLACES the shared list (it exists to narrow, so a union would make adding a row widen). An empty list for a principal means that member mounts nothing.`),
		memberWritableRoots:     flagEnv("member-writable-roots", "WARDYN_MEMBER_WRITABLE_ROOTS", "", "comma-separated absolute host directories where a MEMBER may mark their own mount WRITABLE. Empty (the default) = no writable member mounts at all; a member's mounts are read-only. Operators keep their unrestricted per-source writable opt-in."),
		memberWritableDeny:      flagEnv("member-writable-deny", "WARDYN_MEMBER_WRITABLE_DENY", "", "comma-separated absolute host directories carved OUT of -member-writable-roots. Deny WINS over allow, so a subtree inside a writable root can be pinned read-only for members."),
		uiDir:                   flagEnv("ui-dir", "WARDYN_UI_DIR", "", "directory holding the built web UI (optional)"),
		runnerSel:               flagEnv("runner", "WARDYN_RUNNER", "none", `runner substrate: "none" or a registered confinement substrate ("docker" in -tags docker builds)`),
		identitySel:             flagEnv("identity", "WARDYN_IDENTITY", "embedded", `identity provider (pluggable seam): "embedded" (default)`),
		secretStoreSel:          flagEnv("secret-store", "WARDYN_SECRET_STORE", "pg", `secret store (pluggable seam): "pg" (default)`),
		recordingSel:            flagEnv("recording-store", "WARDYN_RECORDING_STORE", "pg", `recording store (pluggable seam): "pg" (default; Postgres-backed, visible to every replica) or "fs" (legacy per-pod on-disk store)`),
		confinementMap:          flagEnv("confinement-map", "WARDYN_CONFINEMENT_MAP", "", `optional per-class substrate/runtime pins making CC3 runtime-pluggable, e.g. "CC2=runsc;CC3=kata-qemu" (or "CC3=oci:kata-qemu"); empty = built-in defaults`),
		trustDomain:             flagEnv("trust-domain", "WARDYN_TRUST_DOMAIN", embedded.DefaultTrustDomain, "SPIFFE trust domain"),
		controlURL:              flagEnv("control-plane-url", "WARDYN_CONTROL_PLANE_URL", "http://wardynd:8080", "externally-reachable control plane URL for sidecars"),
		policyPath:              flagEnv("default-policy", "WARDYN_DEFAULT_POLICY", "examples/policies/default.json", "path to the default RunPolicy spec JSON"),
		ageKey:                  flagEnv("age-key", "WARDYN_AGE_KEY", "", "age X25519 identity (AGE-SECRET-KEY-...) for the secret store; generated+logged if empty"),
		proxyImage:              flagEnv("proxy-image", "WARDYN_PROXY_IMAGE", "", "OCI image for the wardyn-proxy sidecar (docker runner)"),

		recordingDir: flagEnv("recording-dir", "WARDYN_RECORDING_DIR", "./data/recordings", `directory for stored PTY session recordings (asciicast); "fs" store only — empty disables replay there, but is ignored by the default "pg" store (set -recording-store=fs too)`),
		// OFF by default (0 = keep forever): a session recording is the governance
		// evidence this product exists to produce, so nothing deletes one unless
		// the operator asks for a retention window.
		recordingRetention: flagIntEnv("recording-retention-days", "WARDYN_RECORDING_RETENTION_DAYS", 0, "delete stored session recordings older than N days (0 = keep forever, the default)"),
		auditSinks:         flagEnv("audit-sinks", "WARDYN_AUDIT_SINKS", "", "audit sink config JSON (file/webhook/syslog); empty disables fanout"),
		auditSource:        flagEnv("audit-source", "WARDYN_AUDIT_SOURCE", "", "#10: optional static string stamped as an extra \"source\" field on every event a sink (file/webhook/syslog) serializes — lets one SIEM index tell multiple wardynd instances/environments apart. Empty = no stamp (byte-identical to today). Never written to Postgres, sink payloads only."),
		auditSpool:         flagEnv("audit-spool", "WARDYN_AUDIT_SPOOL", "./data/audit-spool.jsonl", "local append-only JSONL fallback for audit events whose Postgres write fails (durability so a security event is never lost); empty disables"),

		oidcIssuer:         flagEnv("oidc-issuer", "WARDYN_OIDC_ISSUER", "", "OIDC public issuer URL — browser-facing, matches the id_token iss (enables human SSO when set)"),
		oidcInternalIss:    flagEnv("oidc-internal-issuer", "WARDYN_OIDC_INTERNAL_ISSUER", "", "OIDC issuer URL reachable from wardynd for server-side calls (e.g. http://dex:5556); defaults to the public issuer"),
		oidcClientID:       flagEnv("oidc-client-id", "WARDYN_OIDC_CLIENT_ID", "", "OIDC client id"),
		oidcClientSecret:   flagEnv("oidc-client-secret", "WARDYN_OIDC_CLIENT_SECRET", "", "OIDC client secret"),
		oidcRedirectURL:    flagEnv("oidc-redirect-url", "WARDYN_OIDC_REDIRECT_URL", "", "OIDC redirect URL (<base>/auth/callback)"),
		oidcEmailDomains:   flagEnv("oidc-email-domains", "WARDYN_OIDC_EMAIL_DOMAINS", "", "comma-separated allowed email domains (empty = any verified email)"),
		oidcOperatorEmails: flagEnv("oidc-operator-emails", "WARDYN_OIDC_OPERATOR_EMAILS", "", "comma-separated operator (admin) emails; a signed-in human NOT listed is a member (owner-scoped: reads + launches/kills their OWN runs, 403 on configuring the deployment, secret writes, and admin-only credential/tool_call approvals — still decides egress_domain approvals on and attaches to their own runs). Empty with OIDC configured is REFUSED at boot — see -allow-oidc-no-operator-list"),
		// Refused by default (validateOperatorPosture) when OIDC SSO is configured
		// and the operator allowlist is empty — the same refuse-with-an-escape-hatch
		// shape as -allow-plaintext-listen above.
		allowOIDCNoOperatorList: flagBool("allow-oidc-no-operator-list", "WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST", false, "override: allow boot with OIDC SSO configured but WARDYN_OIDC_OPERATOR_EMAILS empty, i.e. — absent a WARDYN_OIDC_ROLE_MAP — every signed-in human admin-equivalent (normally refused — prefer setting the operator allowlist)"),
		oidcRoleMap:             flagEnv("oidc-role-map", "WARDYN_OIDC_ROLE_MAP", "", `comma-separated "value=role" pairs mapping an Entra App Role ("roles" claim), a "groups" claim entry, or an email to "admin" or "member" (e.g. "Wardyn.Admin=admin,eng-team=member,alice@corp.com=admin"); any admin match wins when more than one matches. Empty (the default) disables role derivation: every signed-in human is "admin", exactly today's behavior`),
		oidcDefaultRole:         flagEnv("oidc-default-role", "WARDYN_OIDC_DEFAULT_ROLE", "", `role ("admin" or "member") assigned when -oidc-role-map is set but nothing in a signed-in human's roles/groups/email matched an entry. Empty (the default) DENIES that login instead, naming WARDYN_OIDC_ROLE_MAP in the error page. Ignored when -oidc-role-map is empty`),

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
		// #12: the SAME provenance gate applyWorkspaceRequirements already
		// applies to a scan_seeded SECRET requirement (never auto-grant from
		// untrusted repo content), now optionally applied to a scan_seeded
		// EGRESS requirement too. Default OFF: today every scan-seeded egress
		// host a workspace scan finds is auto-added at launch regardless of
		// provenance, and flipping that off by default would silently narrow
		// egress for every existing workspace on upgrade. An operator in a
		// higher-trust posture (repo content is reviewed, or the exfil risk
		// inline_policy.go's filterMemberGrants comment names matters more than
		// the convenience) opts in here.
		requireOpSetEgress: flagBool("require-operator-set-egress", "WARDYN_REQUIRE_OPERATOR_SET_EGRESS", true, "require a workspace egress requirement's provenance to be operator_set before applyWorkspaceRequirements auto-adds it at launch — a scan_seeded egress host (the workspace scanner reading untrusted repo content) is skipped instead. Mirrors the operator_set-only gate the SECRET side has always applied unconditionally. ON by default since 0.7; set false to restore pre-0.7 behavior, where any enabled egress requirement was auto-added regardless of provenance."),
		gitPATBroker: flagEnv("git-pat-broker", "WARDYN_GIT_PAT_BROKER", "on", "never-resident git_pat lane: `on` (default) mints a non-GitHub forge's PAT PROXY-SIDE and injects it on the outbound leg, so the credential never enters the sandbox — the posture github_token has always had. `off` restores pre-0.7 behavior, where the in-sandbox credential helper mints the PAT into the agent's process. Off is an escape hatch for a forge that misbehaves under the broker's insteadOf rewrite, not a supported posture."),

		// Bedrock: an enterprise Anthropic transport (no direct Anthropic egress,
		// billed via AWS). Both must be set to enable it; the AWS credentials
		// themselves are NOT flags — they come from the secret store
		// (aws-access-key-id/aws-secret-access-key/aws-session-token), read at
		// dispatch time since Bedrock's SigV4 request signing can't be
		// proxy-injected. See internal/api.Config.BedrockRegion/BedrockModel.
		bedrockRegion:       flagEnv("bedrock-region", "WARDYN_BEDROCK_REGION", "", `optional: AWS region for the Amazon Bedrock Anthropic transport (e.g. "us-east-1"). Falls back to the standard AWS_REGION / AWS_DEFAULT_REGION when left empty. Requires -bedrock-model too, plus aws-access-key-id/aws-secret-access-key secrets. Empty (and no AWS_REGION) = Bedrock disabled.`),
		bedrockModel:        flagEnv("bedrock-model", "WARDYN_BEDROCK_MODEL", "", `optional: Bedrock model id for claude-code (a cross-region inference-profile id, e.g. "us.anthropic.claude-sonnet-4-5-...", not a bare foundation-model id). Requires -bedrock-region too.`),
		bedrockAWSDir:       flagEnv("bedrock-aws-dir", "WARDYN_BEDROCK_AWS_DIR", "", `bind a host ~/.aws directory READ-ONLY into each Bedrock run so the AWS SDK resolves credentials itself. SSO/IAM-Identity-Center auto-refresh works only for sso-session profiles whose CACHED token is still valid (the read-only mount cannot write back a rotated token; legacy sso_start_url profiles need a periodic host 'aws sso login'). Works in compose too (mount it via the WARDYN_BEDROCK_AWS_DIR bind, same path host==container). Exposes the WHOLE ~/.aws to the untrusted sandbox — point it at ~/.aws only. Leave empty to use static aws-* secrets or a bedrock-api-key instead.`),
		bedrockAWSProfile:   flagEnv("bedrock-aws-profile", "WARDYN_BEDROCK_AWS_PROFILE", "", `optional: AWS_PROFILE to select from the mounted ~/.aws (common with SSO). Falls back to the standard AWS_PROFILE when left empty. Only used with -bedrock-aws-dir.`),
		bedrockAWSSSORegion: flagEnv("bedrock-aws-sso-region", "WARDYN_BEDROCK_AWS_SSO_REGION", "", `optional: AWS SSO region whose oidc.<r>/portal.sso.<r> endpoints the sandbox may reach to exchange an SSO token for role creds, and which endpoints the containerized 'aws sso login' may reach. Defaults to -bedrock-region.`),

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

		// flag.String, NOT flagEnv: no env pair by design — see the struct field.
		// The backquoted word is deliberate: flag.PrintDefaults renders the first
		// one in a usage string as the argument placeholder ("-rotate-age-key path").
		rotateAgeKey: flag.String("rotate-age-key", "", "MAINTENANCE MODE, daemon must be STOPPED: mint a new age identity, re-encrypt every stored secret from WARDYN_AGE_KEY to it in ONE transaction, "+
			"replace the key file at `path` (previous kept as <path>.bak), then exit. Serves nothing. "+
			"That file must already hold the CURRENT identity as a bare AGE-SECRET-KEY-... line (# comments allowed) — it is NOT an env file. See docs/OPERATIONS.md"),

		sshListen:        flagEnv("ssh-listen", "WARDYN_SSH_LISTEN", "", `SSH gateway listen address (e.g. ":2222"); empty (the default) disables the gateway entirely — no listener, no new surface`),
		uiListen:         flagEnv("ui-sandbox-listen", "WARDYN_UI_SANDBOX_LISTEN", "", `UI-sandbox gateway listen address (e.g. ":8081"); empty (the default) disables the gateway entirely — no listener, no new surface. MUST differ from -listen: relayed pages are the sandbox's own code, and the separate origin is what keeps them away from the console's session`),
		uiAdvertise:      flagEnv("ui-sandbox-advertise", "WARDYN_UI_SANDBOX_ADVERTISE", "", `externally-reachable base URL of the UI-sandbox gateway (e.g. "https://wardyn-ui.example.com"), published on /healthz for the console's Open button; purely advisory copy (the gateway binds -ui-sandbox-listen, not this)`),
		uiOriginTemplate: flagEnv("ui-sandbox-origin-template", "WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE", "", `optional PER-RUN origin for the UI-sandbox gateway, e.g. "https://run-{run}.ui.example.com" (needs wildcard DNS + a wildcard certificate). Set, every run gets its own browser origin and an enter on any other host is refused; empty, all runs share one origin separated only by a path-scoped cookie`),

		sshAdvertise: flagEnv("ssh-advertise", "WARDYN_SSH_ADVERTISE", "", `externally-reachable host[:port] for the SSH gateway, shown in the run-detail "Connect via SSH" pane's ssh command; purely advisory copy (the gateway itself binds -ssh-listen, not this). Empty publishes NO address at all: /healthz reports an empty advertise_addr, the console pane has no host to show and "wardyn ssh" refuses with that message — so set this whenever the gateway is enabled`),
		sshRoleTTL:   flagDuration("ssh-role-ttl", "WARDYN_SSH_ROLE_TTL", 24*time.Hour, `how stale a registered SSH key's admin-override stamp (role_checked_at, migration 0046) may be before the gateway refuses the override; refreshed on every OIDC login for that key's owning principal (bounded-stale, never live — see docs/SSH.md Bounds)`),
	}
	flag.Parse()

	// Standard-AWS fallback. An operator whose environment is already configured
	// for AWS shouldn't have to restate the same values under a Wardyn-specific
	// name. WARDYN_BEDROCK_* (and its flag) stay authoritative — these apply only
	// where it resolved EMPTY.
	//
	// Post-parse, NOT as the flagEnv default argument: compose passes
	// WARDYN_BEDROCK_REGION="" unconditionally (docker-compose.yaml) and flagEnv
	// honours an explicitly-empty env as an intentional blank, so a default-arg
	// fallback would be dead in the deployment mode most people run. Here in
	// parseBootFlags rather than resolveLocalMode (where the sibling Bedrock
	// auto-detect lives) because that function returns early when local mode is
	// off — which is every auth-configured deployment, i.e. exactly the
	// enterprise Bedrock audience.
	//
	// Cannot silently enable Bedrock: that needs region AND model, and there is
	// no standard env for the model.
	if *f.bedrockRegion == "" {
		*f.bedrockRegion = envOr("AWS_REGION", envOr("AWS_DEFAULT_REGION", ""))
	}
	if *f.bedrockAWSProfile == "" {
		*f.bedrockAWSProfile = envOr("AWS_PROFILE", "")
	}
	return f
}

// demoAdminToken is the admin bearer the compose stack and the docs ship
// (deploy/compose/docker-compose.yaml, docs/TRY-IT.md, docs/sdk.md, docs/ENV.md).
// It is published, therefore not a secret — the analogue of knownPublicAgeKeys.
const demoAdminToken = "demo-admin-token"

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
	// bug-rbac-1: an EXPLICIT -local-mode alongside a configured -oidc-issuer
	// is refused up front, before it can ever reach lm.enabled below. The
	// auto-enable heuristic already excludes a configured issuer
	// (*f.oidcIssuer == "" in the OR's right-hand side), so this refusal only
	// ever fires for the explicit flag — humanOrAdminAuth branches on
	// LocalMode FIRST and bypasses OIDC entirely without ever consulting it,
	// so a boot that got this far would silently drop the whole SSO/RBAC
	// deployment to a fixed local:operator with no warning anywhere.
	if *f.localMode && strings.TrimSpace(*f.oidcIssuer) != "" && !*f.allowLocalModeWithOIDC {
		return lm, fmt.Errorf("refusing to start: -local-mode is explicitly set alongside a configured -oidc-issuer %q — "+
			"local mode bypasses ALL public-API auth and would silently disable the configured OIDC/SSO admin-member RBAC deployment; "+
			"unset -local-mode (WARDYN_LOCAL_MODE) or -oidc-issuer, or explicitly set WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC=true to override", *f.oidcIssuer)
	}
	lm.enabled = *f.localMode || (*f.adminToken == "" && *f.oidcIssuer == "" && lm.loopback)
	lm.operator = strings.TrimSpace(*f.localOperator)
	// The demo admin token is published in this repo (compose + docs) and nothing
	// mints a random one, so "auth configured" with THAT value is a full-admin API
	// anyone who read the README can drive. This check sits ABOVE the !lm.enabled
	// return on purpose: setting an admin token is exactly what turns local mode
	// off. Same refuse-vs-warn split as -local-trust-forwarder below — refuse on a
	// specific routable bind, warn on the unspecified one, which from inside a
	// container is indistinguishable from the safe compose 127.0.0.1-publish. Honest
	// ceiling: an operator who republishes compose's port on 0.0.0.0 still has
	// WARDYN_LISTEN=":8080" in the container, so the refusal cannot fire for that
	// (most realistic) exposure path — this is a backstop, not full coverage.
	if strings.TrimSpace(*f.adminToken) == demoAdminToken {
		if listenBindsSpecificRoutable(*f.listen) {
			return lm, fmt.Errorf("refusing to start: WARDYN_ADMIN_TOKEN is the demo token published in this repo (deploy/compose + docs) but the listen address %q binds a specific non-loopback interface — that is an effectively-unauthenticated admin API; set a secret token (e.g. `openssl rand -hex 32`) or bind loopback", *f.listen)
		}
		slog.Warn("wardynd: WARDYN_ADMIN_TOKEN is the demo token published in this repo — anyone who read the docs has full admin. Fine for the loopback-published demo stack; set a secret token before exposing this port.",
			slog.String("listen", *f.listen),
		)
	}
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
