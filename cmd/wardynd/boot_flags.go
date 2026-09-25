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

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/cliutil"
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
	// runner.ParseUserMountPolicy, which fails boot closed on a malformed value
	// and returns the O4 posture warnings.
	memberMode          *bool
	memberRoots         *string
	memberRootsMap      *string
	memberWritableRoots *string
	memberWritableDeny  *string
	// HYBRID BOOT (issue #100, docs/design/0.8/PLAN.md). orgURL is the org
	// control plane a managed laptop belongs to — the missing half of
	// member-mode desktop, which asserts the human is a member but never said
	// WHICH org. Unset (the default) means no hybrid posture at all, and
	// validateHybridPosture (boot_posture.go) is a no-op. orgEnrolToken is
	// meaningless without it.
	orgURL *string
	// orgEnrolToken is WARDYN_ORG_ENROLMENT_TOKEN — a secret, so never logged
	// and never echoed in a boot refusal.
	orgEnrolToken *string
	// userDriveHostRoots is the SAME class of knob one level up: where an ADMIN
	// may point a host_path user drive, whose per-person subdirectories Wardyn
	// then binds into OTHER PEOPLE's sandboxes. Parsed by
	// runner.ParseUserDriveHostRoots — same CSV shape, same refuse-boot on a
	// malformed entry, same WARN on a root so wide it bounds nothing — and
	// unset means no host_path drive may be authored at all.
	userDriveHostRoots *string
	// ssoOnly is WARDYN_SSO_ONLY (flag -sso-only): the operator's declaration
	// that SSO is the ONLY way into this console. validateSSOOnlyPosture
	// (boot_posture.go) enforces the precondition — OIDC configured, and the
	// admin token / local mode / member mode / no-operator-list override all
	// absent — before this ever reaches api.Config.SSOOnly, which /healthz
	// publishes as sso_only so the sign-in screen stops offering a form that
	// cannot work.
	ssoOnly   *bool
	uiDir     *string
	runnerSel *string
	// runnerTargetOverride is WARDYN_RUNNER_TARGET, and it is a TEST-HARNESS
	// knob: the substrate name STORED objects validate against while -runner is
	// "none". A runner-less daemon resolves the target "none", which no drive
	// backend can name (types.DriveBackend.RunnerTarget answers "docker" or
	// "k8s"), so types.ValidateUserDrive refuses EVERY backend and the
	// Playwright backend (scripts/e2e-backend.sh) cannot register a drive by any
	// route. It moves the REGISTRATION boundary only: the runner is still nil,
	// so nothing is dispatched and every run still stays PENDING. Ignored
	// whenever a runner IS configured — there the resolved substrate's own name
	// is the only truthful target. Unknown value = boot refusal, not a guess.
	runnerTargetOverride *string
	identitySel          *string
	secretStoreSel       *string
	recordingSel         *string
	confinementMap       *string
	trustDomain          *string
	controlURL           *string
	// internalListen is the proxy-facing TLS listener (internal_tls.go); it
	// runs whenever controlURL is https.
	internalListen *string
	policyPath     *string
	// trustedCAFile is WARDYN_TRUSTED_CA_FILE (see trusted_ca.go): a PATH to a
	// PEM bundle of additional roots a corporate TLS-inspecting middlebox signs
	// with. Same shape as policyPath above (a path read once at boot, not a
	// value) — precedent WARDYN_DEFAULT_POLICY. Empty = unset = every outbound
	// TLS client in this process trusts exactly the system roots, as today.
	trustedCAFile *string
	// daemonProxyURL / daemonNoProxy are WARDYN_DAEMON_PROXY_URL /
	// WARDYN_DAEMON_NO_PROXY (see daemon_proxy.go): a forward proxy for
	// wardynd's OWN outbound HTTP calls (OIDC discovery/JWKS, audit webhooks,
	// GitHub App minting, AWS SSO CreateToken renewal, Entra sync) — the
	// supported replacement for setting HTTPS_PROXY on wardynd, which stays
	// unsupported (Go's net/http would also re-point the Kubernetes client and
	// every http.ProxyFromEnvironment reader process-wide). Same posture class
	// as trustedCAFile above: control-plane-authored only, never a SiteConfig
	// field. Empty daemonProxyURL = the shared http.DefaultTransport is left
	// untouched, byte-identical to today.
	daemonProxyURL *string
	daemonNoProxy  *string
	// daemonProxySecretFile is WARDYN_DAEMON_PROXY_SECRET (see
	// installDaemonProxySecret, daemon_proxy.go): a PATH to a file holding one
	// proxy URL that MAY embed user:pass@ — the credentialed form
	// daemonProxyURL above refuses. Same shape as trustedCAFile: a path read
	// once at boot, control-plane-authored only. Mutually exclusive with
	// daemonProxyURL (bootDaemonProxy refuses boot if both are set).
	daemonProxySecretFile *string
	// anthropicBaseURL / openaiBaseURL are WARDYN_ANTHROPIC_BASE_URL /
	// WARDYN_OPENAI_BASE_URL (see internal/api/llm_gateway.go's
	// ValidateLLMGateways): an operator-set internal model gateway base URL
	// re-pointing the api-key lane's brokered upstream. Empty (default) =
	// every api-key lane dials the public provider host, byte-identical to
	// today. Control-plane-authored, same posture class as trustedCAFile
	// above — never a SiteConfig field, never agent-reachable.
	anthropicBaseURL *string
	openaiBaseURL    *string
	// demoVideoBaseURL is WARDYN_DEMO_VIDEO_BASE_URL (see
	// internal/api/llm_gateway.go's ValidateDemoVideoBaseURL): an
	// operator-run mirror re-pointing the Getting Started demo episodes for
	// an air-gapped deployment, where github.com is unreachable. Empty
	// (default) = the two hardcoded GitHub hosts, byte-identical to today.
	// Same posture class as anthropicBaseURL/openaiBaseURL above —
	// control-plane-authored, never a SiteConfig field, never
	// agent-reachable.
	demoVideoBaseURL *string
	// anthropicGatewayHeader / anthropicGatewayFormat (WARDYN_ANTHROPIC_GATEWAY_HEADER
	// / _FORMAT) and their OpenAI pair below are the injection header name and
	// value format a gateway configured via anthropicBaseURL/openaiBaseURL wants
	// instead of the harness catalog's compile-time vendor convention
	// (harness.go's Gateway field: x-api-key bare / Authorization: Bearer %s).
	// Each is independent and optional — set the header alone, the format
	// alone, or neither — validated at boot by api.ValidateLLMGateways
	// (exactly one %s in the format, a valid HTTP header token) and applied in
	// (*Server).llmProviderFor. Both empty (default) = the vendor convention,
	// byte-identical to today.
	anthropicGatewayHeader *string
	anthropicGatewayFormat *string
	openaiGatewayHeader    *string
	openaiGatewayFormat    *string
	ageKey                 *string
	platformKeyFile        *string
	proxyImage             *string

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
	// oidcAllowEmailMappings feeds api.Config.AllowEmailMappings — it gates a
	// CONSOLE write (POST /access/mappings refusing an email-shaped value),
	// not a boot-time derivation input, so unlike oidcRoleMap/oidcDefaultRole
	// above it belongs on api.Config rather than oidc.Config. Default false:
	// an SSO/Entra deployment's default posture steers admins to App
	// Role/group keys, which env WARDYN_OIDC_ROLE_MAP email keys were never
	// gated on (legacy, still boot-warned separately).
	oidcAllowEmailMappings *bool

	// Directory autocomplete (§I / PF-29), strictly OPT-IN. dirProvider empty is
	// the whole feature off: no connector, no Graph reach, every "who" field
	// stays free text. The other three are the DEDICATED app-registration
	// override; unset, the credentials are derived from the OIDC ones above
	// (tenant from the issuer), which is why the common case is one variable.
	// resolveDirectoryConfig (boot_posture.go) resolves the pair and REFUSES
	// boot when neither source can do client credentials — the connector itself
	// can only 503 lazily at first search, which is not a refusal.
	dirProvider *string
	dirTenant   *string
	dirClientID *string
	dirSecret   *string

	autoStopInterval *time.Duration

	approvalExpiryInterval *time.Duration
	approvalExpiryAfter    *time.Duration
	endedRunGrace          *time.Duration
	auditCoalesceWindow    *time.Duration

	envbuild     *bool
	envbuildImg  *string
	envbuildRepo *string

	agentImagesJSON    *string
	agentModel         *string
	scanAIAdvisor      *bool
	requireOpSetEgress *bool
	gitPATBroker       *string

	bedrockRegion *string
	bedrockModel  *string
	// bedrockBaseURL is WARDYN_BEDROCK_BASE_URL (see api.ValidateBedrockBaseURL):
	// the Bedrock DATA-PLANE endpoint override that points a regulated
	// deployment at its VPC/PrivateLink endpoint. Same posture class as
	// anthropicBaseURL above — boot-time only, never a SiteConfig field, because
	// in bearer mode it IS the TLS-MITM and Authorization-injection target.
	bedrockBaseURL *string
	// awsSSOEndpointOverride / allowTestEndpoints are the TEST hatch that makes
	// "a member signs in on Kubernetes and their Bedrock run gets per-user
	// credentials" provable without a real AWS tenant: one URL re-points BOTH
	// AWS IAM Identity Center services (see api.Config.AWSSSOEndpointOverride
	// for the five derivations it moves). Refused unless allowTestEndpoints is
	// explicitly true — the same two-deliberate-acts shape as
	// allowLocalModeWithOIDC, and for the same reason.
	awsSSOEndpointOverride *string
	// awsSSOProxyInject is the PHASE B kill switch: whether a captured-AWS-SSO
	// Bedrock dispatch injects the session proxy-side (the token never resident)
	// or writes it into the sandbox. A STRING, not a bool flag,
	// because its two documented spellings are `on` and `off` and an
	// unrecognised value takes the DEFAULT rather than refusing boot — see
	// api.ResolveAWSSSOProxyInject for why a switch meant to be reached in a
	// hurry must not be able to crash-loop a daemon.
	awsSSOProxyInject   *string
	allowTestEndpoints  *bool
	bedrockAWSDir       *string
	bedrockAWSProfile   *string
	bedrockAWSSSORegion *string

	proxyURL *string

	printGroundtruthToken *bool
	genAgeKey             *bool
	// rotateAgeKey is the one knob in this struct with NO WARDYN_* env pair, on
	// purpose: it is a destructive maintenance mode that rewraps every
	// stored secret, so it must be an explicit act on a command line. Its
	// early-exit siblings above are print-and-quit and harmless if an env var
	// turns them on; a stray WARDYN_ROTATE_AGE_KEY left in a compose .env would
	// rotate the store on EVERY boot. See rotateAgeKeyMode (rekey.go).
	rotateAgeKey *string
	// migrateSecrets, migrateTo and reconcile are the store-mode maintenance
	// modes (migrate_secrets.go); like rotateAgeKey they have NO env pair.
	migrateSecrets *bool
	migrateTo      *string
	reconcile      *bool
	// rewrap is `wardynd -rewrap` (rewrap.go): no env pair, like the above.
	rewrap *bool
	// vault configures the Vault KV v2 external store, azure the Azure Key
	// Vault one (secret_store.go).
	vault vaultFlags
	azure azureFlags

	// allowMultiInstance is the runtime twin of the Helm chart's
	// allowMultiReplica: it waives the single-instance boot lock
	// (claimSingleInstance). Like rotateAgeKey it has NO WARDYN_* env pair — a
	// stray variable in a compose .env must not silently disable a safety
	// control, and the chart passes it as an arg where it is set deliberately.
	allowMultiInstance *bool

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
	// uiSessionTTL bounds the relay session cookie — see api.Config.UISessionTTL.
	uiSessionTTL *time.Duration

	// allowUnknownMigrations is the break-glass past db.Migrate's downgrade
	// refusal (a database a newer wardynd migrated) — see connectAndMigrate.
	allowUnknownMigrations *bool
}

// deprecatedEnvAliases is UT-5's six WARDYN_MEMBER_* → WARDYN_USER_* renames
// (user-types-design.md rev 4 §6, §4's D5 ruling): {new, old} pairs, resolved
// before any flag is parsed so every FlagBool/FlagEnv/os.Getenv(new) read below
// sees the operator's value whichever name they used. No M-surface-2 generic
// alias mechanism exists yet (issue UT-5 allows "or self-contained"), so this
// is wardynd's own list rather than a shared registry; a later M-surface-2 PR
// can fold it into a bigger one using the same cliutil.EnvAlias primitive.
// Accepted through 0.8.x, removed in 0.9 — same shape as the boot WARN for a
// chart WARDYN_OIDC_ROLE_MAP entry still saying `=member` (UT-2a).
var deprecatedEnvAliases = [][2]string{
	{"WARDYN_USER_DESKTOP", "WARDYN_MEMBER_MODE"},
	{"WARDYN_USER_WORKSPACE_ROOTS", "WARDYN_MEMBER_WORKSPACE_ROOTS"},
	{"WARDYN_USER_WORKSPACE_ROOTS_MAP", "WARDYN_MEMBER_WORKSPACE_ROOTS_MAP"},
	{"WARDYN_USER_WRITABLE_ROOTS", "WARDYN_MEMBER_WRITABLE_ROOTS"},
	{"WARDYN_USER_WRITABLE_DENY", "WARDYN_MEMBER_WRITABLE_DENY"},
	{"WARDYN_ALLOW_USER_ENV_SECRET", "WARDYN_ALLOW_MEMBER_ENV_SECRET"},
}

// resolveDeprecatedEnvAliases applies deprecatedEnvAliases. It WARNs once per
// deprecated name actually carrying a value, naming 0.9 as the removal release,
// and WARNs when both spellings are set to different values, naming the one it
// ignored — for WARDYN_USER_WRITABLE_DENY a silently dropped old list would
// widen the writable set.
func resolveDeprecatedEnvAliases() {
	for _, pair := range deprecatedEnvAliases {
		newEnv, oldEnv := pair[0], pair[1]
		aliased, ignored := cliutil.EnvAlias(newEnv, oldEnv)
		attrs := []any{slog.String("old_env", oldEnv), slog.String("new_env", newEnv)}
		switch {
		case aliased:
			slog.Warn(fmt.Sprintf("wardynd: %s is no longer a variable name; use %s instead. Accepted through 0.8.x, removed in 0.9.", oldEnv, newEnv), attrs...)
		case ignored:
			slog.Warn(fmt.Sprintf("wardynd: %s and %s are both set, to different values; using %s and ignoring %s. Unset %s, which is removed in 0.9.", newEnv, oldEnv, newEnv, oldEnv, oldEnv), attrs...)
		}
	}
}

// parseBootFlags declares every wardynd flag (with its WARDYN_* env fallback)
// and parses the command line. Moved verbatim out of run(); the usage strings
// carry the operator-facing documentation for each knob.
func parseBootFlags() *bootFlags {
	resolveDeprecatedEnvAliases()
	f := &bootFlags{
		dsn:            flagEnv("dsn", "WARDYN_PG_DSN", "", "Postgres connection string (required)"),
		migrateDSN:     flagEnv("migrate-dsn", "WARDYN_PG_MIGRATE_DSN", "", "Postgres DSN for a migrator role, used only to run migrations; when set, the main DSN is used only for the least-privilege runtime pool. Empty (default) runs migrations on the main DSN directly"),
		migrateTimeout: flagDuration("migrate-timeout", "WARDYN_MIGRATE_TIMEOUT", 5*time.Minute, "how long db.Migrate may run before boot fails closed (duration)"),
		listen:         flagEnv("listen", "WARDYN_LISTEN", defaultListenAddr, "HTTP listen address"),
		tlsCert:        flagEnv("tls-cert", "WARDYN_TLS_CERT", "", "path to the TLS certificate PEM file; enables built-in TLS together with -tls-key"),
		tlsKey:         flagEnv("tls-key", "WARDYN_TLS_KEY", "", "path to the TLS private key PEM file; enables built-in TLS together with -tls-cert"),
		tlsTerminated:  flagBool("tls-terminated", "WARDYN_TLS_TERMINATED", false, "set when TLS terminates at an upstream reverse proxy; marks session cookies Secure even though wardynd itself serves plain HTTP (default false)"),
		// Refused by default (validateConfig) when NO TLS posture is configured and
		// the bind is a specific non-loopback interface — see listenBindsSpecificRoutable.
		// Loopback and the unspecified bind (":8080", the compose topology) are
		// already warn-only, unaffected by this flag.
		allowPlaintextListen:    flagBool("allow-plaintext-listen", "WARDYN_ALLOW_PLAINTEXT_LISTEN", false, "allow boot on a specific non-loopback bind serving plain HTTP with no TLS configured, normally refused (default false)"),
		adminToken:              flagEnv("admin-token", "WARDYN_ADMIN_TOKEN", "", "admin bearer token gating the public API"),
		localMode:               flagBool("local-mode", "WARDYN_LOCAL_MODE", false, "bypass public-API auth (no SSO/token) and attribute actions to the local operator; single-developer localhost use only, refused on a publicly-routable bind. Auto-enabled when no auth is configured and the bind is loopback (default false)"),
		localOperator:           flagEnv("local-operator", "WARDYN_LOCAL_OPERATOR", "", "operator principal stamped on runs/approvals/audit in -local-mode (default local:<os-user>)"),
		localTrustFwd:           flagBool("local-trust-forwarder", "WARDYN_LOCAL_TRUST_FORWARDER", false, "in -local-mode, accept a non-loopback request peer instead of requiring a loopback TCP peer; safe only when the port is published loopback-only, e.g. 127.0.0.1:PORT; never set on a directly-bound host-mode wardynd, which re-opens no-auth LAN access (default false)"),
		allowLocalModeWithOIDC:  flagBool("allow-local-mode-with-oidc", "WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC", false, "allow boot with -local-mode explicitly set alongside a configured -oidc-issuer, which disables the configured SSO/RBAC deployment; normally refused (default false)"),
		allowSharedSubscription: flagBool("allow-shared-subscription", "WARDYN_ALLOW_SHARED_SUBSCRIPTION", false, "allow one operator's Anthropic subscription credential to be injected into runs on a deployment that is not -local-mode, e.g. the compose demo stack. Does not waive the refusals for the k8s runner or a configured OIDC issuer; demo/single-user boxes only (default false)"),
		memberMode:              flagBool("member-mode", "WARDYN_USER_DESKTOP", false, "assert that the human using this daemon is a MEMBER and operator authority lives elsewhere, e.g. an org IdP/MDM; refuses to start unless -local-mode is off and OIDC is configured (default false)"),
		memberRoots:             flagEnv("member-workspace-roots", "WARDYN_USER_WORKSPACE_ROOTS", "", "comma-separated absolute host directories a member's own local_dir workspace source may live under. Empty (default) means members may not mount host directories at all; point it at a dedicated projects directory, never $HOME"),
		memberRootsMap:          flagEnv("member-workspace-roots-map", "WARDYN_USER_WORKSPACE_ROOTS_MAP", "", `optional per-member override of -member-workspace-roots, as JSON {"<principal>": ["/abs/root", ...]} keyed by OIDC sub or email; a listed principal's entry replaces the shared list rather than adding to it`),
		memberWritableRoots:     flagEnv("member-writable-roots", "WARDYN_USER_WRITABLE_ROOTS", "", "comma-separated absolute host directories where a member may mark their own mount writable. Empty (default) means member mounts are read-only"),
		memberWritableDeny:      flagEnv("member-writable-deny", "WARDYN_USER_WRITABLE_DENY", "", "comma-separated absolute host directories carved out of -member-writable-roots; deny wins over allow"),
		orgURL:                  flagEnv("org-url", "WARDYN_ORG_URL", "", "org control plane this managed laptop belongs to (https://, or a plain http:// loopback URL for local testing). Empty (default) means no hybrid posture; requires -member-mode when set"),
		orgEnrolToken:           flagEnv("org-enrolment-token", "WARDYN_ORG_ENROLMENT_TOKEN", "", "secret enrolment token this device presents to -org-url; requires -org-url to also be set"),
		userDriveHostRoots:      flagEnv("user-drive-host-roots", "WARDYN_USER_DRIVE_HOST_ROOTS", "", "comma-separated absolute host directories a host_path user drive may be registered inside, typically the mount point of a share the operator mounted host-side. Empty (default) means no host_path drive may be registered; never $HOME or /"),
		ssoOnly:                 flagBool("sso-only", "WARDYN_SSO_ONLY", false, "declare SSO the only way into the console; refuses to start unless OIDC is configured and the admin token, local mode, member mode and no-operator-list override are all unset (default false)"),
		uiDir:                   flagEnv("ui-dir", "WARDYN_UI_DIR", "", "directory holding the built web UI (optional)"),
		runnerSel:               flagEnv("runner", "WARDYN_RUNNER", "none", `runner substrate: "none" or a registered confinement substrate, e.g. "docker" in -tags docker builds`),
		runnerTargetOverride:    flagEnv("runner-target", "WARDYN_RUNNER_TARGET", "", `substrate name stored objects validate against when -runner is "none" ("docker" or "k8s"); test harnesses only, ignored whenever a runner is configured. Empty (default) refuses every drive backend`),
		identitySel:             flagEnv("identity", "WARDYN_IDENTITY", "embedded", "identity provider"),
		secretStoreSel:          flagEnv("secret-store", "WARDYN_SECRET_STORE", "pg", "secret store"),
		recordingSel:            flagEnv("recording-store", "WARDYN_RECORDING_STORE", "pg", `session recording store: "pg" (Postgres-backed, visible to every replica), "fs" (per-pod on-disk store) or "off" (no recording, no replay)`),
		confinementMap:          flagEnv("confinement-map", "WARDYN_CONFINEMENT_MAP", "", `optional per-class substrate/runtime pins, e.g. "CC2=runsc;CC3=kata-qemu". Empty (default) uses the built-in defaults`),
		trustDomain:             flagEnv("trust-domain", "WARDYN_TRUST_DOMAIN", embedded.DefaultTrustDomain, "SPIFFE trust domain"),
		controlURL:              flagEnv("control-plane-url", "WARDYN_CONTROL_PLANE_URL", "https://wardynd:8443", "the URL every run's proxy dials to reach this daemon's internal TLS listener (-internal-listen); its host is the name wardynd's internal CA certifies. http:// is refused at boot unless the host is loopback (localhost, 127.0.0.0/8, ::1)"),
		internalListen:          flagEnv("internal-listen", "WARDYN_INTERNAL_LISTEN", ":8443", "listen address of the proxy-facing TLS listener (the /api/v1/internal/ routes and /healthz only), served with a certificate from wardynd's own internal CA. Runs whenever -control-plane-url is https"),
		policyPath:              flagEnv("default-policy", "WARDYN_DEFAULT_POLICY", "examples/policies/default.json", "path to the default RunPolicy spec JSON"),
		trustedCAFile:           flagEnv("trusted-ca-file", "WARDYN_TRUSTED_CA_FILE", "", "path to a PEM bundle of additional trusted roots, e.g. a corporate TLS-inspecting proxy's CA; added to the system roots for wardynd's own outbound TLS, the proxy sidecar and every sandbox. Empty (default) trusts only the system roots"),
		daemonProxyURL:          flagEnv("daemon-proxy-url", "WARDYN_DAEMON_PROXY_URL", "", "forward proxy (http:// or https://, no user:pass@) for wardynd's own outbound HTTP calls: OIDC discovery/JWKS, audit webhooks, GitHub App token minting, AWS SSO token renewal and Entra directory sync. Empty (default) leaves the default transport untouched"),
		daemonNoProxy:           flagEnv("daemon-no-proxy", "WARDYN_DAEMON_NO_PROXY", "", "NO_PROXY-style bypass list for -daemon-proxy-url (host, .suffix, CIDR or *); ignored when the proxy URL is unset"),
		daemonProxySecretFile:   flagEnv("daemon-proxy-secret-file", "WARDYN_DAEMON_PROXY_SECRET", "", "path to a file holding one forward-proxy URL that may embed user:pass@, the credentialed form of -daemon-proxy-url; file mode must be 0600 or tighter. Mutually exclusive with -daemon-proxy-url"),
		anthropicBaseURL:        flagEnv("anthropic-base-url", "WARDYN_ANTHROPIC_BASE_URL", "", "internal model gateway base URL (https://) re-pointing Anthropic's brokered upstream instead of api.anthropic.com, for both the api-key lane and subscription runs (the operator's OAuth token then goes to that gateway); the harness-login lane is exempt. Empty (default) uses the public host"),
		openaiBaseURL:           flagEnv("openai-base-url", "WARDYN_OPENAI_BASE_URL", "", "same as -anthropic-base-url, for OpenAI's api-key lane (api.openai.com)"),
		demoVideoBaseURL:        flagEnv("demo-video-base-url", "WARDYN_DEMO_VIDEO_BASE_URL", "", "mirror base URL (https://) re-pointing the Getting Started demo episodes for an air-gapped deployment where github.com is unreachable. Empty (default) uses the two GitHub hosts"),
		anthropicGatewayHeader:  flagEnv("anthropic-gateway-header", "WARDYN_ANTHROPIC_GATEWAY_HEADER", "", "injection header name -anthropic-base-url's gateway wants instead of x-api-key (default x-api-key)"),
		anthropicGatewayFormat:  flagEnv("anthropic-gateway-format", "WARDYN_ANTHROPIC_GATEWAY_FORMAT", "", `value format -anthropic-base-url's gateway wants instead of the bare key, e.g. "Bearer %s"; must contain exactly one %s (default: bare key)`),
		openaiGatewayHeader:     flagEnv("openai-gateway-header", "WARDYN_OPENAI_GATEWAY_HEADER", "", "same as -anthropic-gateway-header, for OpenAI's gateway (default Authorization)"),
		openaiGatewayFormat:     flagEnv("openai-gateway-format", "WARDYN_OPENAI_GATEWAY_FORMAT", "", `same as -anthropic-gateway-format, for OpenAI's gateway (default "Bearer %s")`),
		ageKey:                  flagEnv("age-key", "WARDYN_AGE_KEY", "", "age X25519 identity (AGE-SECRET-KEY-...) for the secret store; generated and logged if empty"),
		platformKeyFile:         flagEnv("platform-key-file", "WARDYN_PLATFORM_KEY_FILE", "", "path to a second age identity that alone protects wardynd's signing, session and SSH host keys when secrets are sealed locally. Empty (default): WARDYN_AGE_KEY protects both. Set on an existing install, run wardynd -rewrap once; see docs/OPERATIONS.md"),
		proxyImage:              flagEnv("proxy-image", "WARDYN_PROXY_IMAGE", "", "OCI image for the wardyn-proxy sidecar (docker runner)"),

		recordingDir: flagEnv("recording-dir", "WARDYN_RECORDING_DIR", "./data/recordings", `directory for stored PTY session recordings (asciicast); used only by the "fs" recording store`),
		// OFF by default (0 = keep forever): a session recording is the governance
		// evidence this product exists to produce, so nothing deletes one unless
		// the operator asks for a retention window.
		recordingRetention: flagIntEnv("recording-retention-days", "WARDYN_RECORDING_RETENTION_DAYS", 0, "delete stored session recordings older than N days (default 0, keep forever)"),
		auditSinks:         flagEnv("audit-sinks", "WARDYN_AUDIT_SINKS", "", "audit sink config JSON (file/webhook/syslog); empty disables fanout"),
		auditSource:        flagEnv("audit-source", "WARDYN_AUDIT_SOURCE", "", `optional static string stamped as an extra "source" field on every audit event a sink serializes, so one SIEM index can tell multiple wardynd instances apart. Empty (default) adds no stamp`),
		auditSpool:         flagEnv("audit-spool", "WARDYN_AUDIT_SPOOL", "./data/audit-spool.jsonl", "local append-only JSONL fallback for audit events whose Postgres write fails; empty disables"),

		oidcIssuer:         flagEnv("oidc-issuer", "WARDYN_OIDC_ISSUER", "", "OIDC public issuer URL, browser-facing, matches the id_token iss; enables human SSO when set"),
		oidcInternalIss:    flagEnv("oidc-internal-issuer", "WARDYN_OIDC_INTERNAL_ISSUER", "", "OIDC issuer URL reachable from wardynd for server-side calls, e.g. http://dex:5556; defaults to the public issuer"),
		oidcClientID:       flagEnv("oidc-client-id", "WARDYN_OIDC_CLIENT_ID", "", "OIDC client id"),
		oidcClientSecret:   flagEnv("oidc-client-secret", "WARDYN_OIDC_CLIENT_SECRET", "", "OIDC client secret"),
		oidcRedirectURL:    flagEnv("oidc-redirect-url", "WARDYN_OIDC_REDIRECT_URL", "", "OIDC redirect URL (<base>/auth/callback)"),
		oidcEmailDomains:   flagEnv("oidc-email-domains", "WARDYN_OIDC_EMAIL_DOMAINS", "", "comma-separated allowed email domains; requires email_verified=true when set. Empty (default) applies no domain or email_verified check"),
		oidcOperatorEmails: flagEnv("oidc-operator-emails", "WARDYN_OIDC_OPERATOR_EMAILS", "", "comma-separated operator (admin) emails; a signed-in human not listed is a standard user. Empty with OIDC configured is refused at boot unless -allow-oidc-no-operator-list is set"),
		// Refused by default (validateOperatorPosture) when OIDC SSO is configured
		// and the operator allowlist is empty — the same refuse-with-an-escape-hatch
		// shape as -allow-plaintext-listen above.
		allowOIDCNoOperatorList: flagBool("allow-oidc-no-operator-list", "WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST", false, "allow boot with OIDC SSO configured but -oidc-operator-emails empty, making every signed-in human admin-equivalent absent a role map; normally refused (default false)"),
		oidcRoleMap:             flagEnv("oidc-role-map", "WARDYN_OIDC_ROLE_MAP", "", `comma-separated "value=role" pairs mapping an App Role, group or email to "admin", "security_admin" or "user"; highest tier wins (user < security_admin < admin); this map is the only way to grant "security_admin". "member" means "user" until 0.9, with a boot warning. Empty (default) disables role derivation; every signed-in human is "admin"`),
		oidcDefaultRole:         flagEnv("oidc-default-role", "WARDYN_OIDC_DEFAULT_ROLE", "", `role ("admin" or "user"; "member" means "user" until 0.9, with a boot warning) assigned when -oidc-role-map is set but nothing matched; "security_admin" is refused here. Empty (default) denies that login instead. Ignored when -oidc-role-map is empty`),
		oidcAllowEmailMappings:  flagBool("oidc-allow-email-mappings", "WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS", false, "allow an email-shaped value on a console People-step role mapping (POST /access/mappings); refused by default in favor of an App Role or group key (default false)"),

		dirProvider: flagEnv("directory-provider", "WARDYN_DIRECTORY_PROVIDER", "", `identity-directory connector for the console's "who" autocomplete: "entra" (Microsoft Graph) or empty. Empty (default) is the feature off; enabling it grants wardynd read of the whole directory`),
		dirTenant:   flagEnv("directory-tenant", "WARDYN_DIRECTORY_TENANT", "", "Entra tenant id/domain for the directory connector. Empty (default) derives it from -oidc-issuer; set together with -directory-client-id/-secret to use a dedicated app registration"),
		dirClientID: flagEnv("directory-client-id", "WARDYN_DIRECTORY_CLIENT_ID", "", "client id of the dedicated directory app registration. Empty (default) reuses -oidc-client-id; all three of tenant/client-id/secret are set together or not at all"),
		dirSecret:   flagEnv("directory-client-secret", "WARDYN_DIRECTORY_CLIENT_SECRET", "", "client secret of the dedicated directory app registration. Empty (default) reuses -oidc-client-secret"),

		autoStopInterval: flagDuration("autostop-interval", "WARDYN_AUTOSTOP_INTERVAL", time.Minute, "how often the lifecycle reaper scans for idle runs (duration; 0 disables)"),

		approvalExpiryInterval: flagDuration("approval-expiry-interval", "WARDYN_APPROVAL_EXPIRY_INTERVAL", 10*time.Minute, "how often to sweep stale PENDING approvals (duration; 0 disables)"),
		approvalExpiryAfter:    flagDuration("approval-expiry-after", "WARDYN_APPROVAL_EXPIRY_AFTER", 24*time.Hour, "PENDING approvals older than this transition to EXPIRED (duration)"),
		endedRunGrace:          flagDuration("ended-run-grace", "WARDYN_ENDED_RUN_GRACE", 7*24*time.Hour, "how long a run past its end keeps its files, stopped with no network and no broker credentials, before it is torn down (duration; 0 tears it down at its end)"),
		// A maximum GAP between two IDENTICAL consecutive auth.failed rows, not a
		// cap on how long a streak may run: the flood this bounds was one row a
		// minute forever from one retrying sidecar, which the auth.failed rate
		// limiter (1/sec) never trips. 0 disables it — every refusal is its own row.
		auditCoalesceWindow: flagDuration("audit-coalesce-window", "WARDYN_AUDIT_COALESCE_WINDOW", 5*time.Minute, "fold identical consecutive auth.failed audit rows into one summary row when the gap between them is under this window (duration; 0 disables folding)"),

		envbuild:     flagBool("envbuild", "WARDYN_ENVBUILD", false, "enable devcontainer image builds for create-run; requires -tags docker (default false)"),
		envbuildImg:  flagEnv("envbuild-image", "WARDYN_ENVBUILD_IMAGE", "", "envbuilder OCI image override. Empty (default) uses the upstream default"),
		envbuildRepo: flagEnv("envbuild-cache-repo", "WARDYN_ENVBUILD_CACHE_REPO", "", "optional OCI registry ref for the envbuilder layer cache; enables daemonless push mode"),

		// agentImagesJSON is a JSON object mapping agent names to OCI image refs
		// (e.g. '{"claude-code":"wardyn/agent-claude-code:local"}'). When set,
		// named agents use the specified image instead of the ghcr convention.
		// Must be valid JSON when non-empty; validated at boot (fail closed).
		agentImagesJSON: flagEnv("agent-images", "WARDYN_AGENT_IMAGES", "", "JSON map of agent-name -> OCI image ref, overriding the ghcr convention for named agents"),
		agentModel:      flagEnv("agent-anthropic-model", "WARDYN_AGENT_ANTHROPIC_MODEL", "", `optional model to pin ANTHROPIC_MODEL to inside claude-code sandboxes, e.g. "opus". Empty (default) uses the CLI default`),
		scanAIAdvisor:   flagBool("scan-ai-advisor", "WARDYN_SCAN_AI_ADVISOR", false, "enable the advisory AI workspace-scan fallback that gap-fills empty profile fields when the deterministic scanner is unsure; advisory-only and fail-open. Requires a resident claude CLI on the host PATH (default false, deterministic-only)"),
		// #12: the SAME provenance gate applyWorkspaceRequirements already
		// applies to a scan_seeded SECRET requirement (never auto-grant from
		// untrusted repo content), now optionally applied to a scan_seeded
		// EGRESS requirement too. Default OFF: today every scan-seeded egress
		// host a workspace scan finds is auto-added at launch regardless of
		// provenance, and flipping that off by default would silently narrow
		// egress for every existing workspace on upgrade. An operator in a
		// higher-trust posture (repo content is reviewed, or the exfil risk
		// inline_policy.go's filterUserGrants comment names matters more than
		// the convenience) opts in here.
		requireOpSetEgress: flagBool("require-operator-set-egress", "WARDYN_REQUIRE_OPERATOR_SET_EGRESS", true, "require a workspace egress requirement's provenance to be operator_set before it is auto-added at launch; a scan_seeded egress host is skipped instead"),
		gitPATBroker:       flagEnv("git-pat-broker", "WARDYN_GIT_PAT_BROKER", "on", `never-resident git_pat lane: "on" mints a non-GitHub forge's PAT proxy-side so it never enters the sandbox; "off" mints it into the sandbox process instead, for a forge that misbehaves under the broker's rewrite`),

		// Bedrock: an enterprise Anthropic transport (no direct Anthropic egress,
		// billed via AWS). Both must be set to enable it; the AWS credentials
		// themselves are NOT flags — they come from the secret store
		// (aws-access-key-id/aws-secret-access-key/aws-session-token), read at
		// dispatch time since Bedrock's SigV4 request signing can't be
		// proxy-injected. See internal/api.Config.BedrockRegion/BedrockModel.
		bedrockRegion:          flagEnv("bedrock-region", "WARDYN_BEDROCK_REGION", "", `AWS region for the Amazon Bedrock Anthropic transport, e.g. "us-east-1"; falls back to AWS_REGION / AWS_DEFAULT_REGION. Requires -bedrock-model and the aws-access-key-id/aws-secret-access-key secrets. Empty (default) disables Bedrock`),
		bedrockModel:           flagEnv("bedrock-model", "WARDYN_BEDROCK_MODEL", "", "Bedrock model id for claude-code: a cross-region inference-profile id or its full ARN, passed to the agent verbatim. Requires -bedrock-region"),
		bedrockBaseURL:         flagEnv("bedrock-base-url", "WARDYN_BEDROCK_BASE_URL", "", "Bedrock data-plane base URL (https://) re-pointing bedrock-runtime at a VPC/PrivateLink endpoint; one per deployment, does not override the control-plane host. Empty (default) uses the regional public host"),
		awsSSOEndpointOverride: flagEnv("aws-sso-endpoint-override", "WARDYN_AWS_SSO_ENDPOINT_OVERRIDE", "", "TEST ONLY: re-point both AWS IAM Identity Center services (sso-oidc and the sso portal) at this base URL, so an AWS SSO walk can run with no real AWS tenant. Requires -allow-test-endpoints; warns at every boot. Empty (default) uses the real AWS endpoints"),
		awsSSOProxyInject:      flagEnv("aws-sso-proxy-inject", "WARDYN_AWS_SSO_PROXY_INJECT", api.AWSSSOProxyInjectFlagDefault(), `"on" or "off": inject a captured AWS SSO session proxy-side, leaving only a placeholder in the sandbox, or write it into the sandbox directly. Applies to new dispatches only; an unrecognised value takes the default`),
		allowTestEndpoints:     flagBool("allow-test-endpoints", "WARDYN_ALLOW_TEST_ENDPOINTS", false, "acknowledge this is a TEST deployment; unlocks -aws-sso-endpoint-override and an unencrypted http:// -bedrock-base-url, both refused otherwise. Never set on a deployment holding a real credential (default false)"),
		bedrockAWSDir:          flagEnv("bedrock-aws-dir", "WARDYN_BEDROCK_AWS_DIR", "", "bind a host ~/.aws directory read-only into each Bedrock run so the AWS SDK resolves credentials itself; exposes the whole directory to the sandbox, so point it at ~/.aws only. Empty (default) uses static aws-* secrets or a bedrock-api-key instead"),
		bedrockAWSProfile:      flagEnv("bedrock-aws-profile", "WARDYN_BEDROCK_AWS_PROFILE", "", "AWS_PROFILE to select from the mounted ~/.aws; falls back to the standard AWS_PROFILE. Only used with -bedrock-aws-dir"),
		bedrockAWSSSORegion:    flagEnv("bedrock-aws-sso-region", "WARDYN_BEDROCK_AWS_SSO_REGION", "", "AWS SSO region for exchanging an SSO token for role credentials. Defaults to -bedrock-region"),

		// proxyURL overrides the WARDYN_PROXY_URL injected into sandbox env.
		// Defaults to "http://wardyn-proxy:3128" (per-run sidecar docker alias).
		proxyURL: flagEnv("proxy-url", "WARDYN_PROXY_URL_OVERRIDE", "", "sandbox WARDYN_PROXY_URL override (default http://wardyn-proxy:3128)"),

		// printGroundtruthToken, when set, mints a host-sensor token
		// (aud="wardyn-groundtruth") for the eBPF/Tetragon ground-truth ingest
		// sidecar, prints it to stdout, and exits. This is how compose seeds
		// WARDYN_GROUNDTRUTH_TOKEN. The token grants ONLY audit-write on
		// POST /api/v1/internal/groundtruth — it can never mint or approve
		// (those endpoints verify aud="wardyn-internal"). Fail-closed: minting
		// requires the identity provider; the token has the provider's standard
		// 1h TTL (operators re-mint on rotation).
		printGroundtruthToken: flagBool("print-groundtruth-token", "WARDYN_PRINT_GROUNDTRUTH_TOKEN", false, "mint and print a host-sensor token for wardyn-tetragon-ingest, then exit (default false)"),

		// genAgeKey, when set, prints a freshly-generated age X25519 identity
		// (AGE-SECRET-KEY-...) to stdout and exits — BEFORE any DSN/DB work — so
		// `docker run --rm wardyn/wardynd:local -gen-age-key` can mint a durable
		// WARDYN_AGE_KEY with no Postgres.
		genAgeKey: flagBool("gen-age-key", "WARDYN_GEN_AGE_KEY", false, "generate a fresh age X25519 identity (AGE-SECRET-KEY-...) to stdout for WARDYN_AGE_KEY, then exit; no Postgres required (default false)"),

		// flag.Bool, NOT flagBool: no env pair by design — see the struct field.
		allowMultiInstance: flag.Bool("allow-multi-instance", false,
			"start even though another wardynd already holds this database's single-instance lock; "+
				"a recording served by a different instance than the one that did the proxy injection is persisted with "+
				"live credentials in cleartext, since the secret-masking registry is process-local (default false)"),

		// flag.String, NOT flagEnv: no env pair by design — see the struct field.
		// The backquoted word is deliberate: flag.PrintDefaults renders the first
		// one in a usage string as the argument placeholder ("-rotate-age-key path").
		rotateAgeKey: flag.String("rotate-age-key", "", "maintenance mode, daemon must be stopped: mint a new age identity, rewrap every stored secret's data key from "+
			"WARDYN_AGE_KEY's key to it in one transaction, replace the key file at `path` (previous kept as <path>.bak), then exit. "+
			"That file must already hold the current identity as a bare AGE-SECRET-KEY-... line; see docs/OPERATIONS.md"),

		// flag.Bool/flag.String, NOT the env helpers: no env pair by design.
		migrateSecrets: flag.Bool("migrate-secrets", false, "maintenance mode, safe while a daemon serves: move every stored secret to the store -to names, one row at a time, then exit; idempotent and resumable. See docs/OPERATIONS.md (default false)"),
		migrateTo:      flag.String("to", "", `target of -migrate-secrets: "vaultkv", "azurekv" or "local"`),
		reconcile:      flag.Bool("reconcile", false, "maintenance mode: list the pointer rows and the external store side by side, report pointers without values and values without pointers, then exit, non-zero on any; deletes nothing (default false)"),
		rewrap:         flag.Bool("rewrap", false, "maintenance mode: in one transaction, rewrap every stored secret's data key onto the key a write uses today (its purpose's local key, or the WARDYN_KEK=transit key at its latest version), then exit; values are never decrypted. See docs/OPERATIONS.md (default false)"),
		vault:          registerVaultFlags(),
		azure:          registerAzureFlags(),

		sshListen:        flagEnv("ssh-listen", "WARDYN_SSH_LISTEN", "", `SSH gateway listen address, e.g. ":2222". Empty (default) disables the gateway entirely`),
		uiListen:         flagEnv("ui-sandbox-listen", "WARDYN_UI_SANDBOX_LISTEN", "", `UI-sandbox gateway listen address, e.g. ":8081". Empty (default) disables the gateway entirely; must differ from -listen`),
		uiAdvertise:      flagEnv("ui-sandbox-advertise", "WARDYN_UI_SANDBOX_ADVERTISE", "", "externally-reachable base URL of the UI-sandbox gateway, published on /healthz for the console's Open button; advisory only"),
		uiSessionTTL:     flagDuration("ui-sandbox-session-ttl", "WARDYN_UI_SANDBOX_SESSION_TTL", 8*time.Hour, "how long a UI-sandbox relay session cookie stays usable (duration)"),
		uiOriginTemplate: flagEnv("ui-sandbox-origin-template", "WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE", "", `optional per-run origin for the UI-sandbox gateway, e.g. "https://run-{run}.ui.example.com" (needs wildcard DNS and certificate); must contain {run}. Empty (default) shares one origin across every run`),

		sshAdvertise:           flagEnv("ssh-advertise", "WARDYN_SSH_ADVERTISE", "", `externally-reachable host[:port] for the SSH gateway, shown in the run-detail Connect pane; advisory only. Empty (default) publishes no address, so "wardyn ssh" refuses`),
		sshRoleTTL:             flagDuration("ssh-role-ttl", "WARDYN_SSH_ROLE_TTL", 24*time.Hour, "how stale a registered SSH key's admin-override stamp may be before the gateway refuses it (duration)"),
		allowUnknownMigrations: flagBool("allow-unknown-migrations", "WARDYN_ALLOW_UNKNOWN_MIGRATIONS", false, "BREAK-GLASS: boot even though the database records migrations this wardynd does not ship (a newer wardynd migrated it). Normally refused — a downgrade is unsupported; restore the pre-upgrade dump instead"),
	}
	flag.Parse()

	// An empty -listen/WARDYN_LISTEN is not a bind — see normalizeListenAddr.
	// Done HERE, once, so every listen classifier and the http.Server itself
	// read the same real address instead of net/http's implicit 0.0.0.0:80.
	*f.listen = normalizeListenAddr(*f.listen)

	// Standard-AWS fallback. An operator whose environment is already configured
	// for AWS shouldn't have to restate the same values under a Wardyn-specific
	// name. WARDYN_BEDROCK_* (and its flag) stay authoritative — these apply only
	// where it resolved EMPTY.
	//
	// Post-parse, NOT as the flagEnv default argument: compose passes
	// WARDYN_BEDROCK_REGION="" unconditionally (docker-compose.yaml), and the
	// fallback has to key off what the flag ACTUALLY resolved to — including an
	// explicit `-bedrock-region=` — not off what the compiled-in default was.
	// (flagEnv now reads an empty env as "unset, keep the default" like every
	// other helper in cliutil, so the env half alone would work as a default
	// argument; the flag half still would not.) Here in
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

	// <VAR>_FILE twins resolve here, with the rest of the flag/env reading,
	// so every caller — -rotate-age-key included — sees one resolved value. A
	// bad file is a malformed setting like a bad flag, so it exits here the way
	// flag.Parse does, with main's own fatal line (run() has no cyclomatic
	// budget left for another early return).
	if err := resolveSecretFiles(secretFileSettings(f)); err != nil {
		slog.Error("wardynd: fatal", slog.Any("err", err))
		os.Exit(1)
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

// resolveAWSSSOEndpointOverride resolves the gated AWS SSO endpoint hatch:
// refuse when it is set without the acknowledgement, otherwise normalize it and
// WARN — loudly, every boot, naming it a test hatch. The warning is the point:
// this is the one knob that makes wardynd treat an arbitrary HTTP server as AWS
// IAM Identity Center, and an operator who inherits a values file carrying it
// must see that in the log rather than discover it from an audit row.
//
// The refusal shape is this file's own -local-mode-with-OIDC refusal below,
// deliberately: a posture this dangerous takes two explicit acts, never one
// stray env var.
func resolveAWSSSOEndpointOverride(f *bootFlags) (string, error) {
	override, err := api.ValidateAWSSSOEndpointOverride(*f.awsSSOEndpointOverride, *f.allowTestEndpoints)
	if err != nil {
		return "", err
	}
	if override != "" {
		slog.Warn(api.AWSSSOEndpointOverrideWarn,
			slog.String("aws_sso_endpoint_override", override),
		)
	}
	return override, nil
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
	// S-06: "admin-token" is the reserved MECHANISM principal a per_user row's
	// harness-login refuses (internal/api's adminTokenPrincipal) — naming the
	// local operator seat that would collide it with a non-person, refusing
	// the SAME seat boot just accepted.
	if lm.operator == api.AdminTokenPrincipal() {
		return lm, fmt.Errorf("refusing to start: -local-operator (WARDYN_LOCAL_OPERATOR) is %q, the reserved admin-token mechanism principal — local mode's operator seat must be a real, distinguishable person; pick a different name", lm.operator)
	}
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
	// The four-eyes egress switch cannot be enforced in local mode, and an
	// operator who set both learns that when their FIRST approval hangs — a 503
	// per decision, forever. Say it at boot instead.
	//
	// Scoped to the combination that is actually broken, and only that one: local
	// mode authenticates nobody, so both the decider and the run's created_by
	// come from the same client-supplied source and no request in that mode can
	// prove a second human decided (requireSecondHuman refuses outright). With
	// either half alone there is nothing to say — the switch works normally off
	// local mode, and local mode is unaffected with the switch unset — so this
	// stays silent for both. A boot warning that fires on a merely unusual
	// configuration gets filtered out of the logs within a week, and is then
	// missing for the deployment that needed it.
	//
	// WARN, not a refusal: everything else in the deployment works, so refusing
	// to start would cost an operator their whole daemon over one disabled
	// control. The message names the consequence and the remedy.
	if api.EgressSecondHumanEnabled() {
		slog.Warn("wardynd: WARDYN_EGRESS_SECOND_HUMAN is set but LOCAL MODE authenticates nobody — the four-eyes gate cannot be enforced here, so EVERY egress_domain approval decision will be refused with 503. Configure SSO to use this switch, or unset it.",
			slog.String("listen", *f.listen),
		)
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
