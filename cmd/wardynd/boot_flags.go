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
	// HYBRID BOOT (issue #100, docs/design/0.8/PLAN.md). orgURL is the org
	// control plane a managed laptop belongs to — the missing half of
	// member-mode desktop, which asserts the human is a member but never said
	// WHICH org. Unset (the default) means no hybrid posture at all, and
	// validateHybridPosture (boot_posture.go) is a no-op. orgEnrolToken and
	// orgDeviceName are meaningless without it.
	orgURL *string
	// orgEnrolToken is WARDYN_ORG_ENROLMENT_TOKEN — a secret, so never logged
	// and never echoed in a boot refusal.
	orgEnrolToken *string
	orgDeviceName *string
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
}

// parseBootFlags declares every wardynd flag (with its WARDYN_* env fallback)
// and parses the command line. Moved verbatim out of run(); the usage strings
// carry the operator-facing documentation for each knob.
func parseBootFlags() *bootFlags {
	f := &bootFlags{
		dsn:            flagEnv("dsn", "WARDYN_PG_DSN", "", "Postgres DSN (required)"),
		migrateDSN:     flagEnv("migrate-dsn", "WARDYN_PG_MIGRATE_DSN", "", "OPTIONAL Postgres DSN for an owner/migrator role that runs migrations; when set, WARDYN_PG_DSN is used ONLY for the least-privilege runtime app pool (enables audit_events DDL protection). Empty = single-DSN mode (no DDL protection, unchanged behavior)."),
		migrateTimeout: flagDuration("migrate-timeout", "WARDYN_MIGRATE_TIMEOUT", 5*time.Minute, "how long db.Migrate may run before boot fails closed — separate from the fixed 30s connect budget so a slow migration on a large table (e.g. a new index) doesn't crash-loop the upgrade"),
		listen:         flagEnv("listen", "WARDYN_LISTEN", defaultListenAddr, "HTTP listen address"),
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
		orgURL:                  flagEnv("org-url", "WARDYN_ORG_URL", "", "the org control plane this managed laptop belongs to (https://, or a plain http:// loopback URL for local testing). Unset (the default) = no hybrid posture at all. Set, it is REFUSED at boot unless -member-mode is also on (see validateHybridPosture, boot_posture.go)."),
		orgEnrolToken:           flagEnv("org-enrolment-token", "WARDYN_ORG_ENROLMENT_TOKEN", "", "secret enrolment token this device presents to -org-url. Setting it with no -org-url is REFUSED at boot — a token with nowhere to send it is a misconfiguration, not a no-op."),
		orgDeviceName:           flagEnv("org-device-name", "WARDYN_ORG_DEVICE_NAME", "", "human-readable name this device registers under at -org-url (e.g. a hostname or asset tag). Empty is fine while -org-url is unset; it carries no posture of its own."),
		userDriveHostRoots:      flagEnv("user-drive-host-roots", "WARDYN_USER_DRIVE_HOST_ROOTS", "", "comma-separated absolute host directories a USER DRIVE of backend host_path may be registered inside — typically the mount point of an NFS/SMB share the operator mounted host-side. A drive's host_root is allowed only if its CANONICALIZED real path is inside one of these (symlink-resolved, the bind-mount deny-list applied, must exist on this host), and only that person's SUBDIRECTORY is ever bound into a run. Empty (the default) = no host_path drive may be registered at all; Wardyn-managed volume drives are unaffected. Point it at the share's mount point, NEVER $HOME or /."),
		ssoOnly:                 flagBool("sso-only", "WARDYN_SSO_ONLY", false, "declare SSO the ONLY way into the console: refuses to start unless OIDC is configured and WARDYN_ADMIN_TOKEN, WARDYN_LOCAL_MODE, WARDYN_MEMBER_MODE and WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST are all unset (validateSSOOnlyPosture). Publishes sso_only on /healthz so the sign-in screen drops the admin-token form and the role-derivation caveat."),
		uiDir:                   flagEnv("ui-dir", "WARDYN_UI_DIR", "", "directory holding the built web UI (optional)"),
		runnerSel:               flagEnv("runner", "WARDYN_RUNNER", "none", `runner substrate: "none" or a registered confinement substrate ("docker" in -tags docker builds)`),
		runnerTargetOverride:    flagEnv("runner-target", "WARDYN_RUNNER_TARGET", "", `substrate name STORED objects validate against when -runner is "none" ("docker" or "k8s"); TEST HARNESSES ONLY — it changes what may be REGISTERED (a user drive names the backend one target can mount), never what is dispatched, and is IGNORED whenever a runner is configured. Empty (the default) resolves the target "none", which refuses every drive backend`),
		identitySel:             flagEnv("identity", "WARDYN_IDENTITY", "embedded", `identity provider (pluggable seam): "embedded" (default)`),
		secretStoreSel:          flagEnv("secret-store", "WARDYN_SECRET_STORE", "pg", `secret store (pluggable seam): "pg" (default)`),
		recordingSel:            flagEnv("recording-store", "WARDYN_RECORDING_STORE", "pg", `recording store (pluggable seam): "pg" (default; Postgres-backed, visible to every replica), "fs" (legacy per-pod on-disk store) or "off" (no recording, no replay)`),
		confinementMap:          flagEnv("confinement-map", "WARDYN_CONFINEMENT_MAP", "", `optional per-class substrate/runtime pins making CC3 runtime-pluggable, e.g. "CC2=runsc;CC3=kata-qemu" (or "CC3=oci:kata-qemu"); empty = built-in defaults`),
		trustDomain:             flagEnv("trust-domain", "WARDYN_TRUST_DOMAIN", embedded.DefaultTrustDomain, "SPIFFE trust domain"),
		controlURL:              flagEnv("control-plane-url", "WARDYN_CONTROL_PLANE_URL", "https://wardynd:8443", "the URL every run's proxy dials to reach this daemon's internal TLS listener (-internal-listen); its host is the name wardynd's internal CA certifies. http:// is refused at boot unless the host is loopback (localhost, 127.0.0.0/8, ::1)"),
		internalListen:          flagEnv("internal-listen", "WARDYN_INTERNAL_LISTEN", ":8443", "listen address of the proxy-facing TLS listener (the /api/v1/internal/ routes and /healthz only), served with a certificate from wardynd's own internal CA. Runs whenever -control-plane-url is https"),
		policyPath:              flagEnv("default-policy", "WARDYN_DEFAULT_POLICY", "examples/policies/default.json", "path to the default RunPolicy spec JSON"),
		trustedCAFile:           flagEnv("trusted-ca-file", "WARDYN_TRUSTED_CA_FILE", "", "path to a PEM bundle of additional trusted roots (e.g. a corporate TLS-inspecting middlebox's CA), added to the system roots for wardynd's own outbound TLS, the proxy sidecar's forwarding transport, and every sandbox's CA trust. Empty (default) = system roots only, byte-identical to today"),
		daemonProxyURL:          flagEnv("daemon-proxy-url", "WARDYN_DAEMON_PROXY_URL", "", "forward proxy (http:// or https://, no user:pass@) wardynd's OWN outbound HTTP calls traverse: OIDC discovery/JWKS, audit webhooks, GitHub App token minting, AWS SSO CreateToken renewal, and Entra directory sync. Empty (default) = http.DefaultTransport is left untouched (today's ProxyFromEnvironment behavior). Malformed ⇒ boot refused. See docs/ENV.md"),
		daemonNoProxy:           flagEnv("daemon-no-proxy", "WARDYN_DAEMON_NO_PROXY", "", "NO_PROXY-spelled bypass list for WARDYN_DAEMON_PROXY_URL (host, .suffix, CIDR, *). wardynd auto-appends three hosts: KUBERNETES_SERVICE_HOST, the WARDYN_AWS_SSO_ENDPOINT_OVERRIDE host, and the WARDYN_OIDC_INTERNAL_ISSUER host. Ignored when the proxy URL is unset"),
		daemonProxySecretFile:   flagEnv("daemon-proxy-secret-file", "WARDYN_DAEMON_PROXY_SECRET", "", "path to a file holding ONE forward-proxy URL that MAY embed user:pass@ — the credentialed form of WARDYN_DAEMON_PROXY_URL, for an egress proxy that requires a credential. File mode must be 0600 or tighter. Refused at boot if WARDYN_DAEMON_PROXY_URL is ALSO set. Empty (default) = unused. See docs/ENV.md"),
		anthropicBaseURL:        flagEnv("anthropic-base-url", "WARDYN_ANTHROPIC_BASE_URL", "", "operator-set internal model gateway base URL (https://, RFC1918/CGNAT literal allowed) re-pointing Anthropic's brokered upstream instead of api.anthropic.com. Empty (default) = the public host, byte-identical to today. Covers the api-key lane AND subscription/Wardyn-managed runs: setting this sends the operator's live OAuth token to the configured gateway instead of only ever api.anthropic.com. The harness-login (claude setup-token) lane is exempt and always stays on the public host"),
		openaiBaseURL:           flagEnv("openai-base-url", "WARDYN_OPENAI_BASE_URL", "", "same as -anthropic-base-url, for OpenAI's api-key lane (api.openai.com)"),
		demoVideoBaseURL:        flagEnv("demo-video-base-url", "WARDYN_DEMO_VIDEO_BASE_URL", "", "operator-run mirror base URL (https://, no userinfo, no query/fragment) re-pointing the Getting Started demo episodes for an air-gapped deployment where github.com is unreachable. Empty (default) = the two hardcoded GitHub hosts, byte-identical to today. Published on /healthz; the console reads it to build each episode's download URL and the CSP's media-src names its origin"),
		anthropicGatewayHeader:  flagEnv("anthropic-gateway-header", "WARDYN_ANTHROPIC_GATEWAY_HEADER", "", "injection header name -anthropic-base-url's gateway wants instead of x-api-key. Empty (default) = x-api-key, byte-identical to today. Must be a valid HTTP header token; a malformed value refuses boot"),
		anthropicGatewayFormat:  flagEnv("anthropic-gateway-format", "WARDYN_ANTHROPIC_GATEWAY_FORMAT", "", `value format -anthropic-base-url's gateway wants instead of the bare key ("%s"), e.g. "Bearer %s". Empty (default) = the bare key, byte-identical to today. Must contain exactly one %s and no other verb; a malformed value refuses boot`),
		openaiGatewayHeader:     flagEnv("openai-gateway-header", "WARDYN_OPENAI_GATEWAY_HEADER", "", "same as -anthropic-gateway-header, for OpenAI's gateway (default Authorization)"),
		openaiGatewayFormat:     flagEnv("openai-gateway-format", "WARDYN_OPENAI_GATEWAY_FORMAT", "", `same as -anthropic-gateway-format, for OpenAI's gateway (default "Bearer %s")`),
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
		oidcEmailDomains:   flagEnv("oidc-email-domains", "WARDYN_OIDC_EMAIL_DOMAINS", "", "comma-separated allowed email domains; requires email_verified=true when set (empty = no domain or email_verified checks)"),
		oidcOperatorEmails: flagEnv("oidc-operator-emails", "WARDYN_OIDC_OPERATOR_EMAILS", "", "comma-separated operator (admin) emails; a signed-in human NOT listed is a member (owner-scoped: reads + launches/kills their OWN runs, 403 on configuring the deployment, secret writes, and admin-only credential/tool_call approvals — still decides egress_domain approvals on and attaches to their own runs). Empty with OIDC configured is REFUSED at boot — see -allow-oidc-no-operator-list"),
		// Refused by default (validateOperatorPosture) when OIDC SSO is configured
		// and the operator allowlist is empty — the same refuse-with-an-escape-hatch
		// shape as -allow-plaintext-listen above.
		allowOIDCNoOperatorList: flagBool("allow-oidc-no-operator-list", "WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST", false, "override: allow boot with OIDC SSO configured but WARDYN_OIDC_OPERATOR_EMAILS empty, i.e. — absent a WARDYN_OIDC_ROLE_MAP — every signed-in human admin-equivalent (normally refused — prefer setting the operator allowlist)"),
		oidcRoleMap:             flagEnv("oidc-role-map", "WARDYN_OIDC_ROLE_MAP", "", `comma-separated "value=role" pairs mapping an Entra App Role ("roles" claim), a "groups" claim entry, or an email to "admin", "security_admin" or "member" (e.g. "Wardyn.Admin=admin,Wardyn.Security=security_admin,eng-team=member"); when more than one matches, the HIGHEST tier wins (member < security_admin < admin). "security_admin" governs approvals, audit, permissions and governance profiles but never reaches another human's run, credentials or host config — and this map is the ONLY way to grant it. Empty (the default) disables role derivation: every signed-in human is "admin", exactly today's behavior`),
		oidcDefaultRole:         flagEnv("oidc-default-role", "WARDYN_OIDC_DEFAULT_ROLE", "", `role ("admin" or "member") assigned when -oidc-role-map is set but nothing in a signed-in human's roles/groups/email matched an entry. "security_admin" is REFUSED here (boot fails closed): it is a mapped tier only, never the tier every unnamed human falls through to — name its App Role/group in -oidc-role-map instead. Empty (the default) DENIES that login instead, naming WARDYN_OIDC_ROLE_MAP in the error page. Ignored when -oidc-role-map is empty`),
		oidcAllowEmailMappings:  flagBool("oidc-allow-email-mappings", "WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS", false, "override: allow an email-shaped value (contains \"@\") on a console People-step role mapping (POST /access/mappings). Refused by default — an SSO/Entra deployment's default posture steers to an App Role or group key instead; env WARDYN_OIDC_ROLE_MAP email keys are unaffected either way (legacy, still boot-warned separately)"),

		dirProvider: flagEnv("directory-provider", "WARDYN_DIRECTORY_PROVIDER", "", `identity-directory connector for the console's "who" autocomplete (governance assignment subject, People-step mapping value): "entra" (Microsoft Graph) or empty. Empty (the default) is the whole feature OFF — no directory read, every field stays free text. Enabling it grants wardynd READ OF THE WHOLE DIRECTORY (users + groups, App Roles when consented) and adds daemon-side outbound HTTPS to graph.microsoft.com:443 — see docs/OPERATIONS.md`),
		dirTenant:   flagEnv("directory-tenant", "WARDYN_DIRECTORY_TENANT", "", "Entra tenant id/domain for the directory connector. Empty derives it from WARDYN_OIDC_ISSUER; set it with -directory-client-id/-directory-client-secret to use a DEDICATED least-privilege app registration instead of the OIDC one"),
		dirClientID: flagEnv("directory-client-id", "WARDYN_DIRECTORY_CLIENT_ID", "", "client id of the dedicated directory app registration. Empty reuses WARDYN_OIDC_CLIENT_ID; all three of -directory-tenant/-client-id/-secret are set together or not at all"),
		dirSecret:   flagEnv("directory-client-secret", "WARDYN_DIRECTORY_CLIENT_SECRET", "", "client secret of the dedicated directory app registration. Empty reuses WARDYN_OIDC_CLIENT_SECRET — which is REFUSED AT BOOT when that is itself empty (a PUBLIC OIDC client cannot do the client-credentials flow Graph needs)"),

		autoStopInterval: flagDuration("autostop-interval", "WARDYN_AUTOSTOP_INTERVAL", time.Minute, "how often the lifecycle reaper scans for idle runs (0 disables)"),

		approvalExpiryInterval: flagDuration("approval-expiry-interval", "WARDYN_APPROVAL_EXPIRY_INTERVAL", 10*time.Minute, "how often to sweep stale PENDING approvals (0 disables)"),
		approvalExpiryAfter:    flagDuration("approval-expiry-after", "WARDYN_APPROVAL_EXPIRY_AFTER", 24*time.Hour, "PENDING approvals older than this are transitioned to EXPIRED"),
		endedRunGrace:          flagDuration("ended-run-grace", "WARDYN_ENDED_RUN_GRACE", 7*24*time.Hour, "how long a run whose end has passed keeps its files — stopped, with no network and no broker credentials — before it is torn down (0 tears it down at its end)"),
		// A maximum GAP between two IDENTICAL consecutive auth.failed rows, not a
		// cap on how long a streak may run: the flood this bounds was one row a
		// minute forever from one retrying sidecar, which the auth.failed rate
		// limiter (1/sec) never trips. 0 disables it — every refusal is its own row.
		auditCoalesceWindow: flagDuration("audit-coalesce-window", "WARDYN_AUDIT_COALESCE_WINDOW", 5*time.Minute, "fold IDENTICAL consecutive auth.failed audit rows (same boundary, reason, path and peer) into the first row plus one summary row carrying count/first_seen/last_seen; this is the maximum GAP between two identical rows, and 0 disables the folding"),

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
		gitPATBroker:       flagEnv("git-pat-broker", "WARDYN_GIT_PAT_BROKER", "on", "never-resident git_pat lane: `on` (default) mints a non-GitHub forge's PAT PROXY-SIDE and injects it on the outbound leg, so the credential never enters the sandbox — the posture github_token has always had. `off` restores pre-0.7 behavior, where the in-sandbox credential helper mints the PAT into the agent's process. Off is an escape hatch for a forge that misbehaves under the broker's insteadOf rewrite, not a supported posture."),

		// Bedrock: an enterprise Anthropic transport (no direct Anthropic egress,
		// billed via AWS). Both must be set to enable it; the AWS credentials
		// themselves are NOT flags — they come from the secret store
		// (aws-access-key-id/aws-secret-access-key/aws-session-token), read at
		// dispatch time since Bedrock's SigV4 request signing can't be
		// proxy-injected. See internal/api.Config.BedrockRegion/BedrockModel.
		bedrockRegion:          flagEnv("bedrock-region", "WARDYN_BEDROCK_REGION", "", `optional: AWS region for the Amazon Bedrock Anthropic transport (e.g. "us-east-1"). Falls back to the standard AWS_REGION / AWS_DEFAULT_REGION when left empty. Requires -bedrock-model too, plus aws-access-key-id/aws-secret-access-key secrets. Empty (and no AWS_REGION) = Bedrock disabled.`),
		bedrockModel:           flagEnv("bedrock-model", "WARDYN_BEDROCK_MODEL", "", `optional: Bedrock model id for claude-code — a cross-region inference-profile id (e.g. "us.anthropic.claude-sonnet-4-5-..."), or the profile's FULL ARN ("arn:aws:bedrock:<region>:<acct>:inference-profile/<id>" or ".../application-inference-profile/<id>", which is how quota, logging and guardrails attach to the profile rather than the bare model). Not a bare foundation-model id. Passed to the agent verbatim — Wardyn does not parse or validate the shape. Requires -bedrock-region too.`),
		bedrockBaseURL:         flagEnv("bedrock-base-url", "WARDYN_BEDROCK_BASE_URL", "", `optional: Bedrock DATA-PLANE base URL (https://, an RFC1918/CGNAT literal allowed) re-pointing bedrock-runtime at a VPC/PrivateLink endpoint, so inference traffic never traverses the public internet. Empty (default) = the regional public host, byte-identical to today. It moves the egress allow-list entry, the bearer mode's TLS-MITM + Authorization-injection target, and the sandbox's ANTHROPIC_BEDROCK_BASE_URL / AWS_ENDPOINT_URL_BEDROCK_RUNTIME together. CEILING: ONE data-plane host per deployment — it wins for EVERY region, so a multi-region estate must not set it. The CONTROL plane (bedrock.<region>.amazonaws.com) is NOT overridden; pin an inference-profile ARN with -bedrock-model instead. A malformed value refuses boot.`),
		awsSSOEndpointOverride: flagEnv("aws-sso-endpoint-override", "WARDYN_AWS_SSO_ENDPOINT_OVERRIDE", "", `TEST ONLY: re-point BOTH AWS IAM Identity Center services (sso-oidc and the sso portal) at this base URL — http:// or https://. It moves the containerized login sandbox's AWS_ENDPOINT_URL_SSO/_SSO_OIDC, the ssoInject sandbox's, the SSO egress allow-list entries (including the login flow's device.sso.<r>) and the dispatch-time CreateToken URL together. It exists so an AWS SSO walk can run against test/awsssofake on a throwaway cluster with no AWS tenant. REFUSED unless -allow-test-endpoints (WARDYN_ALLOW_TEST_ENDPOINTS=true) is also set, and every boot carrying it WARNs. Never a production posture, and never a substitute for -bedrock-base-url (a different service, and a supported one). Empty (the default) = the real regional AWS endpoints.`),
		awsSSOProxyInject:      flagEnv("aws-sso-proxy-inject", "WARDYN_AWS_SSO_PROXY_INJECT", api.AWSSSOProxyInjectFlagDefault(), `on|off: whether a captured AWS SSO session is injected PROXY-SIDE on the run's own portal.sso host (the sandbox's token cache holds only a placeholder, and a session that lapses mid-run HOLDS the model call while its owner signs in again) or written into the sandbox as it was before 0.7.6. "off" restores the older behaviour for NEW dispatches only — a run already dispatched keeps the lane it was authored with until it ends. It is the rollback for an SDK or corporate-MITM surprise; an unrecognised value takes the default rather than refusing boot.`),
		allowTestEndpoints:     flagBool("allow-test-endpoints", "WARDYN_ALLOW_TEST_ENDPOINTS", false, "override: acknowledge that this deployment is a TEST deployment. It unlocks exactly two relaxations: -aws-sso-endpoint-override may re-point AWS IAM Identity Center at a server of your choosing, and -bedrock-base-url may be plain http:// (which sends inference traffic, and in bearer mode the API key riding it, unencrypted). Without it either one REFUSES boot; with it, each logs a TEST HATCH ACTIVE warning at every boot. Never set on a deployment holding a real credential."),
		bedrockAWSDir:          flagEnv("bedrock-aws-dir", "WARDYN_BEDROCK_AWS_DIR", "", `bind a host ~/.aws directory READ-ONLY into each Bedrock run so the AWS SDK resolves credentials itself. SSO/IAM-Identity-Center auto-refresh works only for sso-session profiles whose CACHED token is still valid (the read-only mount cannot write back a rotated token; legacy sso_start_url profiles need a periodic host 'aws sso login'). Works in compose too (mount it via the WARDYN_BEDROCK_AWS_DIR bind, same path host==container). Exposes the WHOLE ~/.aws to the untrusted sandbox — point it at ~/.aws only. Leave empty to use static aws-* secrets or a bedrock-api-key instead.`),
		bedrockAWSProfile:      flagEnv("bedrock-aws-profile", "WARDYN_BEDROCK_AWS_PROFILE", "", `optional: AWS_PROFILE to select from the mounted ~/.aws (common with SSO). Falls back to the standard AWS_PROFILE when left empty. Only used with -bedrock-aws-dir.`),
		bedrockAWSSSORegion:    flagEnv("bedrock-aws-sso-region", "WARDYN_BEDROCK_AWS_SSO_REGION", "", `optional: AWS SSO region whose oidc.<r>/portal.sso.<r> endpoints the sandbox may reach to exchange an SSO token for role creds, and which endpoints the containerized 'aws sso login' may reach. Defaults to -bedrock-region.`),

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

		// flag.Bool, NOT flagBool: no env pair by design — see the struct field.
		allowMultiInstance: flag.Bool("allow-multi-instance", false,
			"override: start even though another wardynd already holds this database's single-instance lock. "+
				"wardynd's secret-masking registry is process-local and FAILS OPEN, so a recording uploaded to the instance "+
				"that did not serve the run's proxy injection is persisted verbatim, live credentials in cleartext. "+
				"The runtime twin of the chart's allowMultiReplica; normally refused."),

		// flag.String, NOT flagEnv: no env pair by design — see the struct field.
		// The backquoted word is deliberate: flag.PrintDefaults renders the first
		// one in a usage string as the argument placeholder ("-rotate-age-key path").
		rotateAgeKey: flag.String("rotate-age-key", "", "MAINTENANCE MODE, daemon must be STOPPED: mint a new age identity, rewrap every stored secret's data key from WARDYN_AGE_KEY's key to it in ONE transaction, "+
			"replace the key file at `path` (previous kept as <path>.bak), then exit. Serves nothing. "+
			"That file must already hold the CURRENT identity as a bare AGE-SECRET-KEY-... line (# comments allowed) — it is NOT an env file. See docs/OPERATIONS.md"),

		sshListen:        flagEnv("ssh-listen", "WARDYN_SSH_LISTEN", "", `SSH gateway listen address (e.g. ":2222"); empty (the default) disables the gateway entirely — no listener, no new surface`),
		uiListen:         flagEnv("ui-sandbox-listen", "WARDYN_UI_SANDBOX_LISTEN", "", `UI-sandbox gateway listen address (e.g. ":8081"); empty (the default) disables the gateway entirely — no listener, no new surface. MUST differ from -listen: relayed pages are the sandbox's own code, and the separate origin is what keeps them away from the console's session`),
		uiAdvertise:      flagEnv("ui-sandbox-advertise", "WARDYN_UI_SANDBOX_ADVERTISE", "", `externally-reachable base URL of the UI-sandbox gateway (e.g. "https://wardyn-ui.example.com"), published on /healthz for the console's Open button; purely advisory copy (the gateway binds -ui-sandbox-listen, not this)`),
		uiSessionTTL:     flagDuration("ui-sandbox-session-ttl", "WARDYN_UI_SANDBOX_SESSION_TTL", 8*time.Hour, `how long a UI-sandbox relay session (the wardyn_ui_sess cookie minted at the ticket handoff) stays usable; the relay's sibling of -ssh-role-ttl. Applied to cookies ALREADY in browsers, since the cookie carries its own issued-at — so shortening it takes effect at once. Role, run ownership and the revoke cutoff are re-checked on every new connection regardless; this bounds how long a session can outlive its enter at all`),
		uiOriginTemplate: flagEnv("ui-sandbox-origin-template", "WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE", "", `optional PER-RUN origin for the UI-sandbox gateway, e.g. "https://run-{run}.ui.example.com" (needs wildcard DNS + a wildcard certificate). Set, every run gets its own browser origin and an enter on any other host is refused; empty, all runs share one origin separated only by a path-scoped cookie`),

		sshAdvertise: flagEnv("ssh-advertise", "WARDYN_SSH_ADVERTISE", "", `externally-reachable host[:port] for the SSH gateway, shown in the run-detail "Connect via SSH" pane's ssh command; purely advisory copy (the gateway itself binds -ssh-listen, not this). Empty publishes NO address at all: /healthz reports an empty advertise_addr, the console pane has no host to show and "wardyn ssh" refuses with that message — so set this whenever the gateway is enabled`),
		sshRoleTTL:   flagDuration("ssh-role-ttl", "WARDYN_SSH_ROLE_TTL", 24*time.Hour, `how stale a registered SSH key's admin-override stamp (role_checked_at, migration 0046) may be before the gateway refuses the override; refreshed on every OIDC login for that key's owning principal (bounded-stale, never live — see docs/SSH.md Bounds)`),
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
