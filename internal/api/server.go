// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package api wires Wardyn's control-plane REST surface (the wardynd binary).
// It contains ZERO target-specific code (the parity rule): it talks to runners
// only through the runner.Runner interface, and to identity/secrets/broker only
// through their contract interfaces. Every security decision fails closed.
//
// Routes: see routes.go for the table. The authoritative, always-current
// classification of every one of them — which role gate each sits behind — is
// authz_test.go's chi.Walk-enumerated matrix, which cannot go stale because it
// walks the router itself.
package api

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/directory"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// internalAudience is the audience run tokens are verified against on the
// internal (sidecar) endpoints (RFC 8707 discipline).
const internalAudience = "wardyn-internal"

// groundtruthAudience is the SEPARATE audience the host-scoped eBPF
// ground-truth sensor's token is verified against (RFC 8707 discipline). It is
// distinct from internalAudience on purpose: a token minted for this audience
// is accepted ONLY on POST /api/v1/internal/groundtruth (audit-write), and is
// rejected by every mint/approval endpoint (which verify internalAudience).
// This bounds the host sensor's authority to exactly "write ground-truth audit
// events" and nothing else.
const groundtruthAudience = "wardyn-groundtruth"

// ApprovalService is the narrow approval FSM surface the API depends on. It is
// satisfied by package-level wrappers over internal/approval (see wardynd wiring),
// keeping the API decoupled from concrete storage.
type ApprovalService interface {
	Request(ctx context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error)
	// Decide transitions an approval to decision.State (APPROVED or DENIED —
	// the caller picks; there is no separate approve bool, since
	// types.ApprovalDecision.State already says which). decidedByType is kept
	// as its own parameter rather than folded into ApprovalDecision: it is
	// audit attribution (who/what decided), not a property of the decision
	// itself.
	Decide(ctx context.Context, id uuid.UUID, decidedByType types.ActorType, decision types.ApprovalDecision) (types.ApprovalRequest, error)
	Get(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error)
	List(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error)
	// CancelForRun moves every still-PENDING approval of a run that has just
	// reached a terminal state to CANCELLED, returning how many moved per kind. It is
	// part of the terminal cascade, beside identity/broker revocation: an
	// approval whose run has ended is a control that cannot function, and a row
	// left PENDING renders live Approve/Deny buttons in the console. reason names
	// the transition ("run_killed", "run_completed", ...). Idempotent by
	// construction — a second call finds nothing PENDING and emits nothing.
	CancelForRun(ctx context.Context, runID uuid.UUID, reason string) (map[string]int, error)
	// ExpireOne moves one still-PENDING approval to EXPIRED (a no-op once decided):
	// wardyn-toolgate's give-up signal (#811). actor is the withdrawing agent.
	ExpireOne(ctx context.Context, id uuid.UUID, actor, reason string) error
	// CountForRun returns how many approvals a run has raised, in ANY state —
	// the per-run cap handleInternalRequestApproval enforces. A sandbox chooses
	// the hosts it asks about, so without that cap the number of rows one run can
	// create is bounded by nothing.
	CountForRun(ctx context.Context, runID uuid.UUID) (int, error)
}

// MintBroker is the credential-mint surface the API depends on (internal/broker).
type MintBroker interface {
	MintForGrant(ctx context.Context, caller *identity.Claims, grantID uuid.UUID) (broker.Minted, error)
	RevokeRun(ctx context.Context, runID uuid.UUID) error
}

// RefRulesetVerifier answers whether GitHub itself confines the App's writes on
// a repo to the run branch namespace. Satisfied by broker.GitHubMinter. It is a
// SEPARATE, optional Config field rather than a method on MintBroker because it
// is the only thing in the API layer that calls an external network service, and
// the /setup/status checklist must degrade to "unknown" — never "fail" — when it
// is absent or errors.
type RefRulesetVerifier interface {
	VerifyRefRuleset(ctx context.Context, repo string) (confined bool, detail string, err error)
}

// ImageBuilder builds a per-run sandbox image from a devcontainer repo. It is
// target-agnostic (the parity rule): the concrete envbuilder implementation is
// wired in wardynd behind the "docker" build tag, so the control-plane default
// build carries zero target-specific code. Nil disables devcontainer builds.
type ImageBuilder interface {
	// BuildDevcontainer builds the devcontainer for repoURL@ref and returns the
	// local image reference to run. outputTag is the deterministic per-run tag
	// the result is committed under. logSink, when non-nil, receives build
	// output lines as they happen (e.g. the wizard Build step's in-memory log);
	// nil preserves the implementation's own default (wardynd's slog).
	BuildDevcontainer(ctx context.Context, repoURL, ref, outputTag string, logSink io.Writer) (imageRef string, err error)
	// BuildFromDevcontainerFiles builds an image from IN-MEMORY generated
	// devcontainer files (relative path -> content, e.g.
	// ".devcontainer/devcontainer.json") rather than a repo checkout, returning
	// the local image reference. It drives the SAME hardened envbuilder path as
	// BuildDevcontainer. Used for an onboarded workspace WITHOUT a wired
	// devcontainer, where internal/workspacescan generates a minimal one from the
	// detected profile. outputTag is the deterministic profile-hash-keyed tag
	// the result is committed under. logSink: see BuildDevcontainer.
	BuildFromDevcontainerFiles(ctx context.Context, files map[string]string, outputTag string, logSink io.Writer) (imageRef string, err error)
	// FinalizeBase wraps an arbitrary USER-supplied base image (Bring Your Own
	// Image) with Wardyn's runner tools + a cleared ENTRYPOINT, returning the
	// runnable local image reference. No untrusted build, no registry push — just
	// the trusted FROM+COPY finalize stage; the base is pulled only if absent, so
	// a host-pre-pulled private image works. outputTag is the per-run tag.
	// logSink: see BuildDevcontainer.
	FinalizeBase(ctx context.Context, baseRef, outputTag string, logSink io.Writer) (imageRef string, err error)
}

// Config holds the API server's non-secret configuration and injected
// collaborators. All interface fields except Runner are required; Runner may be
// nil for headless API-only operation (runs stay PENDING with a clear message).
type Config struct {
	// SweeperLease, when set, gates the sweeps that must run on one replica (the
	// run pause) to the elected leader and fences them by lease epoch. Nil means
	// this process is the only one sweeping: every pass runs and no fence applies.
	SweeperLease SweeperLease
	// SweepHealth records each background sweep's ticks in the shared sweep_ticks
	// record and reads them back for the sweep gauges and the substrate_health
	// setup row. Nil records nothing and reports no sweep.
	SweepHealth *sweephealth.Tracker
	// Store is the abstract persistence seam (run/policy/grant/approval/audit
	// CRUD + reads). The control plane talks to this instead of *pgxpool.Pool
	// directly, so a future pure-Go backend can be swapped in. Defaults to a
	// store.NewPG(Pool) adapter when wired from wardynd.
	Store store.Store
	// Identity mints/verifies/revokes per-run identities (embedded by default).
	Identity identity.Provider
	// Approvals is the approval FSM service.
	Approvals ApprovalService
	// ApprovalExpiryAfter mirrors WARDYN_APPROVAL_EXPIRY_AFTER, the deployment's
	// ceiling on how long any request waits for a decision. A run's captured
	// wait (captureRunLimits) never exceeds it. 0 means unknown here. Dispatch
	// also mirrors it onto a hold-mode run's sandbox (approval_expiry.go, RL-1).
	ApprovalExpiryAfter time.Duration
	RunLeaseConfig      // the run lease's settings (run_lease_server.go)
	// Broker mints credentials inside the approval-gated transaction.
	Broker MintBroker
	// GitHubRulesets, when set, lets the setup checklist ask GitHub whether the
	// App is ref-confined on a granted repo. Nil omits that row entirely — which
	// is also what happens when no GitHub App is configured, so the vast majority
	// of deployments never make the call.
	GitHubRulesets RefRulesetVerifier
	// Audit records control-plane-originated audit events. The recorder handed in
	// is the shared masking → spooling → store/fanout chain (see cmd/wardynd), so a
	// failed durable write is masked, logged loudly, and spooled to the local
	// append-only fallback for EVERY writer — the API layer no longer spools itself.
	Audit audit.Recorder
	// AuditSpool is the SAME durable fallback spool the recorder chain appends to on
	// a failed store write (see cmd/wardynd buildAuditChain). When set together with
	// AuditDrainRecorder, New starts a background loop that replays spooled events
	// back into the durable store once it recovers and empties the file, so the
	// queryable audit trail heals automatically. Nil disables the drain.
	AuditSpool *AuditSpool
	// AuditDrainRecorder is the RAW durable recorder (store.Recorder — NOT the
	// spooling chain) the spool drain replays into. It must bypass the spool to
	// avoid a re-spool loop / lock re-entry; a nil recorder disables the drain.
	AuditDrainRecorder audit.Recorder
	// AuditSinkDrops, when set, reports per-sink audit-delivery drop counts for
	// the wardyn_audit_sink_drops_total metric (cmd/wardynd wires it to the audit
	// Fanout's DropsByName). Nil omits the metric — a deployment with no SIEM
	// sinks configured has nothing to report.
	AuditSinkDrops func() map[string]int64
	// Runner launches sandboxes. Nil => headless API-only mode.
	Runner runner.Runner
	// AdminToken gates the public API (constant-time bearer compare). Empty
	// disables the public API entirely (fail closed) except /healthz.
	AdminToken string
	// LocalMode enables local host mode: the public-API auth (humanOrAdminAuth)
	// is bypassed entirely and every admin-gated action is attributed to
	// LocalOperator. This is the single-developer localhost path — no SSO, no
	// token, no Dex. It NEVER affects internalAuth (sidecar/run-token
	// verification), so the sidecar callback path is unchanged. The daemon
	// (cmd/wardynd) refuses LocalMode when bound to an EXPLICIT public IP, but
	// only WARNS (does not refuse) on an unspecified bind (0.0.0.0, the
	// WARDYN_LISTEN default) — operators must bind/publish loopback-only for a
	// real guarantee (the Compose default already publishes 127.0.0.1).
	LocalMode bool
	// LocalOperator is the principal stamped on runs/approvals/audit in
	// LocalMode (e.g. "local:<os-user>"). Ignored unless LocalMode is true.
	LocalOperator string
	// MemberMode mirrors WARDYN_USER_DESKTOP (cmd/wardynd's validateMemberModePosture
	// already enforces its precondition at boot). internal/api did not carry this
	// bit before #378/#379: it exists here so handleHealthz can compute
	// token_login — a member-mode desktop's admin token is a PROCESS credential
	// (deploy/desktop/wardyn.env.m-prime.example), never a human sign-in path, so
	// the console must not offer it as one.
	MemberMode bool
	// SSOOnly mirrors WARDYN_SSO_ONLY: the operator's declaration that SSO is the
	// ONLY way into this console. cmd/wardynd's validateSSOOnlyPosture refuses
	// boot unless that is actually true (OIDC configured, admin token/local
	// mode/member mode/no-operator-list override all absent) before this field
	// is ever set, so handleHealthz's sso_only bit — which the sign-in screen
	// reads to drop the admin-token form and the role-derivation caveat — can
	// never overclaim.
	SSOOnly bool
	// GovernAdminRuns mirrors WARDYN_GOVERN_ADMIN_RUNS: an SSO admin's or an
	// admin-role personal token's runs are governed like a member's (see
	// runUngoverned in govern_admin.go). The admin token and local mode stay
	// ungoverned either way.
	GovernAdminRuns bool
	// TrustDomain is surfaced in /healthz and used for run SPIFFE ids.
	TrustDomain string
	// DefaultPolicy is applied to runs created without an explicit policy_id.
	DefaultPolicy types.RunPolicySpec
	// TrustedCAPEM is WARDYN_TRUSTED_CA_FILE's content (cmd/wardynd's
	// loadTrustedCA), read once at boot: a PEM bundle of additional roots a
	// corporate TLS-inspecting middlebox signs with. "" (the default) means the
	// knob is unset. Forwarded verbatim to two other trust boundaries dispatch
	// composes per run — the proxy sidecar (runner.ProxyConfig.TrustedCAPEM,
	// which rides WARDYN_PROXY_CONFIG_JSON) and the sandbox's own CA trust
	// (installSandboxTrustedCA, appended to WARDYN_MITM_CA_PEM) — wardynd's OWN
	// outbound TLS trusts it separately, via installTrustedCA mutating
	// http.DefaultTransport at boot. Control-plane-authored only: never a
	// SiteConfig field, never agent-reachable.
	TrustedCAPEM string
	// RunnerTarget records which target a run is dispatched to ("docker"|"k8s"),
	// or "none" for a headless control plane (-runner none: runs stay PENDING).
	// Defaults to "docker".
	RunnerTarget string
	// UIDir, when set, serves a SPA from this directory at "/".
	UIDir string
	// ControlPlaneURL is the externally-reachable base URL handed to sidecars
	// (proxy config) so they can call the internal endpoints.
	ControlPlaneURL string
	// ControlPlaneCAPEM is wardynd's internal CA certificate (internal/hoptls),
	// handed to every proxy as the only root it trusts for ControlPlaneURL.
	// Empty only when ControlPlaneURL is loopback http (a local install).
	ControlPlaneCAPEM string
	// ProxyURL, when set, overrides the WARDYN_PROXY_URL injected into sandbox
	// env. Defaults to "http://wardyn-proxy:3128" (the per-run proxy sidecar
	// hostname set by the docker driver). Non-secret: it is a network address,
	// not a credential.
	ProxyURL string
	// RecordingStore, when set, serves PTY session replays under
	// GET /api/v1/runs/{id}/recording/{id} (admin-gated) and accepts uploads
	// via PUT /api/v1/runs/{id}/recording (run-token auth).
	RecordingStore recording.Store
	// OIDC, when set, enables human SSO: it mounts /auth/login,/auth/callback
	// and composes oidc.Middleware in front of the admin-gated API
	// so a valid session cookie OR the admin bearer token authenticates a caller.
	// The admin token still works for the CLI when OIDC is configured.
	OIDC *oidc.Authenticator
	// Directory, when set, backs GET /access/directory/search — the
	// console's autocomplete over the identity provider for every "who" field (a
	// governance assignment's subject, the People-step mapping value), so an
	// admin picks a DisplayName and Wardyn stores the ClaimValue instead of
	// hand-typing a group's object GUID.
	//
	// nil is the ABSENT MODE, not a broken one, and it is the default: the
	// handler answers a distinct 503 (directoryUnconfiguredCode) the console
	// reads as "degrade the combobox to a plain text input". Wired only when
	// WARDYN_DIRECTORY_PROVIDER is set (cmd/wardynd's resolveDirectoryConfig,
	// which REFUSES boot rather than handing over a connector that cannot
	// authenticate). Set, it means this daemon can read the WHOLE directory —
	// stated plainly in docs/OPERATIONS.md, because it is a real expansion of
	// the minimal-reach posture rather than a convenience toggle.
	Directory directory.Directory
	// SessionRevocations is the write side of the revoke-a-human-now
	// lever: handleRevokeSessions calls RevokeSub/RevokeAll on it.
	// oidc.Middleware holds the matching READ side (Config.Revocations, wired
	// by the same cmd/wardynd adapter) — this field is nil exactly when OIDC
	// is unconfigured, and the revoke-sessions route only mounts when it is
	// set (see routes.go).
	SessionRevocations oidc.SessionRevocations
	// OperatorEmails is WARDYN_OIDC_OPERATOR_EMAILS, the legacy admin allowlist.
	// internal/api no longer reads this field directly: requireOperator/isOperator
	// (http.go) gate on the session's derived Role instead. The list still
	// matters — cmd/wardynd feeds the SAME value into oidc.Config.LegacyAdminEmails,
	// so an email on it is still an additional RoleAdmin match at OIDC-login role
	// derivation time (see internal/auth/oidc's deriveRole) — it just flows through
	// Session.Role now instead of being re-checked a second time here. Kept on
	// Config (not deleted) so that wiring keeps compiling; read it here only if you
	// need the raw configured list for display (e.g. an admin-facing settings page),
	// never to gate a request.
	OperatorEmails []string
	// AllowEmailMappings is WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS (Q7 adjudication,
	// docs/design/people-access-prompt.md): the console People-step opt-in
	// gate for an email-shaped (contains "@") role-mapping VALUE on
	// POST /access/mappings. Default false refuses one with EMAIL_KEY_REFUSED
	// (access.go) — an SSO/Entra deployment's default posture steers an admin
	// to an App Role or group key instead. Belongs on api.Config, not
	// oidc.Config: it gates a CONSOLE WRITE, not a boot-time role-derivation
	// input, and env WARDYN_OIDC_ROLE_MAP's own email-keyed entries are
	// UNAFFECTED either way (legacy, still boot-warned separately — see
	// buildOptionalFeatures).
	AllowEmailMappings bool
	// UserMounts is the operator/MDM-set posture for MEMBER-authored local_dir
	// binds (WARDYN_USER_WORKSPACE_ROOTS + _MAP + WARDYN_USER_WRITABLE_ROOTS
	// + _DENY, parsed at boot by runner.ParseUserMountPolicy). The ZERO VALUE
	// — the default — means a member may not onboard a host directory at all
	// (repos and operator-owned workspaces are unaffected), which is the
	// fail-closed posture the section-(c) threat model requires. It bounds ONLY
	// mounts on a member-OWNED workspace; an operator's mounts are never
	// narrowed by it. See internal/runner/member_mount.go.
	UserMounts runner.UserMountPolicy
	// UserDriveHostRoots is the operator/MDM-set ceiling over admin-authored
	// host_path USER DRIVES (WARDYN_USER_DRIVE_HOST_ROOTS, parsed at boot by
	// runner.ParseUserDriveHostRoots). The ZERO VALUE — the default — means NO
	// host_path drive may be authored at all, the same fail-closed posture
	// UserMounts takes above and for the same reason: a drive's host root is
	// authored in the database and bound into OTHER PEOPLE's sandboxes, so its
	// ceiling has to live where a console compromise cannot reach it.
	//
	// A []string rather than the types.UserDriveHostRootCheck closure because
	// GET /drives has to report whether a ceiling is CONFIGURED (so the console
	// can disable the host_path option with the reason rather than offering a
	// save that 422s), which a func value cannot answer. The closure is built
	// from it at the write boundary — see Server.userDriveHostRootCheck.
	UserDriveHostRoots []string
	// ImageBuilder, when set, builds a per-run sandbox image from the
	// devcontainer_repo in a create-run request. Nil disables devcontainer
	// builds (the request degrades to the convention image).
	ImageBuilder ImageBuilder
	// AgentImages, when set, is an agent-name -> OCI image-ref map that
	// overrides the ghcr convention image for named agents. Agents not present
	// in the map fall back to the convention. Validated at server construction
	// (must parse if set); nil disables the override and the convention is used
	// for every agent.
	AgentImages map[string]string
	// BedrockRegion is read by nothing: the boot Bedrock lane (WARDYN_BEDROCK_*)
	// is retired and a run's region is its provider's. ponytail: kept only so
	// the ssotoken tests that still set it compile; delete it with those lines.
	BedrockRegion string
	// AWSSSOEndpointOverride re-points the two AWS IAM Identity Center services
	// (sso-oidc and the sso portal) at ONE server of the operator's choosing —
	// a TEST hatch, and nothing else. It exists so "a member signs in on
	// Kubernetes and their Bedrock run gets per-user credentials" is provable
	// without a real AWS tenant, by pointing the login sandbox, the dispatch
	// egress list and the dispatch-time token renewal at test/awsssofake running
	// on the cluster. See awssso_endpoint.go for the five derivations it moves
	// and why it is not the global AWS_ENDPOINT_URL.
	//
	// Normalized by ValidateAWSSSOEndpointOverride at boot, which REFUSES a
	// non-empty value unless WARDYN_ALLOW_TEST_ENDPOINTS=true; wardynd also
	// WARNs on every boot that carries it. Empty (the default, and every real
	// deployment) => every SSO derivation is byte-identical to a build that
	// never had this field. A BOOT flag, never a SiteConfig field: a
	// runtime-writable spelling would let an admin re-point a credential
	// exchange with no restart and no boot log.
	AWSSSOEndpointOverride string
	// AllowTestEndpoints is WARDYN_ALLOW_TEST_ENDPOINTS: the only thing that lets
	// a model provider's bedrock.base_url be plain http:// (validateProviderBedrock).
	AllowTestEndpoints bool

	HarnessLoginCPUMillis int // WARDYN_HARNESS_LOGIN_CPU_MILLIS; see harnessLoginResources (harnesscred.go)
	HarnessLoginMemoryMiB int // WARDYN_HARNESS_LOGIN_MEMORY_MIB; ditto — still governance-ceiling capped

	// AWSSSOProxyInject is the kill switch for proxy-side SSO token injection
	// (WARDYN_AWS_SSO_PROXY_INJECT,
	// resolved by ResolveAWSSSOProxyInject at boot): when true a captured-AWS-SSO
	// Bedrock dispatch stages an inert placeholder in the sandbox's token cache
	// and authors a proxy-side injection of the real token onto that run's own
	// portal.sso host, so the session is never resident; when false NEW
	// dispatches stage the real token in the sandbox's own cache instead (a run
	// already dispatched keeps its authored lane until it ends).
	//
	// It is the rollback for an SDK or corporate-MITM surprise without a
	// downgrade. See runs_dispatch_sso_inject.go for the default and docs/ENV.md
	// for the operator-facing contract.
	AWSSSOProxyInject bool
	// Secrets is the at-rest secret store. It backs the admin secret-management
	// endpoints (PUT/DELETE/list — values are NEVER readable via the API) and
	// the internal injection-resolve endpoint the proxy calls at startup. Nil
	// disables both surfaces.
	Secrets secretstore.Store
	// MaskRegistry, when non-nil, is used to mask verbatim secret values from
	// PTY capture / asciicast uploads before they reach the RecordingStore.
	// A nil registry disables masking (existing tests stay green).
	MaskRegistry *secretmask.Registry
	// MaskManifests, when non-nil, keeps each dispatched run's masking
	// manifest in Postgres and gates the five doors that relay or persist a
	// run's output on it (mask_manifest.go): a run whose corpus it cannot prove
	// complete is refused instead of passed through. Nil keeps no manifests
	// and gates nothing, as a nil MaskRegistry masks nothing.
	MaskManifests *maskmanifest.Manifests
	// ExecOutputTailOff is WARDYN_EXEC_OUTPUT_TAIL=off: no non-interactive run
	// keeps an output tail for GET /runs/{id}/output (run_output.go).
	ExecOutputTailOff bool
	// RunOutputTailBytes is WARDYN_RUN_OUTPUT_TAIL_BYTES: each run's tail size
	// and the cap on ?tail=. Zero defaults to defaultRunOutputTailBytes in New.
	RunOutputTailBytes int
	// ExecOutputTailTTL is WARDYN_EXEC_OUTPUT_TAIL_TTL: how long a run's output
	// tail is kept after its last output. Zero defaults to
	// defaultExecOutputTailTTL in New.
	ExecOutputTailTTL time.Duration
	// RunOutputPersistOff is WARDYN_RUN_OUTPUT_PERSIST=off: the final tail stays
	// in memory only and nothing reaches run_outputs (run_output_final.go). A
	// store that keeps no run outputs behaves the same.
	RunOutputPersistOff bool
	// RunOutputRetention is WARDYN_RUN_OUTPUT_RETENTION_DAYS as a duration: final
	// rows older than it are deleted by the retention sweeper. Zero keeps them
	// forever.
	RunOutputRetention time.Duration
	// ADOEntra resolves the Azure DevOps Entra app registration the per-user
	// sign-in runs against (see ado_entra.go). Nil — the default — means this
	// deployment offers no Azure DevOps sign-in and both of its routes refuse.
	// A function rather than a value so the provider row stays the single
	// source of truth and the sign-in never acts on a cached copy of it.
	ADOEntra ADOEntraSource
	// ADOEntraByRow resolves the row with this id whether or not it is enabled or
	// first, so a token created through a row an admin has since disabled can
	// still be revoked (ado_run_pat_sweep.go). Nil: the revoke uses ADOEntra.
	ADOEntraByRow func(ctx context.Context, rowID string) (ADOEntraConfig, bool, error)
	// AzureFoundryEntra resolves the Entra application an azure_foundry provider
	// row signs people in against, by the row's uid (azure_foundry_entra.go). It
	// answers found=false for a uid that is not an azure_foundry row. The
	// application is always the console's own sign-in application, so a
	// deployment without Entra console login answers an unusable configuration
	// and both legs refuse. Nil: no Azure sign-in is offered.
	AzureFoundryEntra func(ctx context.Context, rowUID string) (ADOEntraConfig, bool, error)
	// ADOLoginFacts is the console's own OIDC client, tenant and whether it holds a secret (S1; nil: none).
	ADOLoginFacts func() (clientID, tenantID string, hasSecret bool)
	// AuditCoalesceWindow folds IDENTICAL consecutive auth.fail audit rows —
	// same boundary, reason, path and peer — into the first row plus one summary
	// row carrying count/first_seen/last_seen (env WARDYN_AUDIT_COALESCE_WINDOW,
	// default 5m at the boot flag; 0 or unset = off, one row per refusal exactly
	// as before). It is a maximum GAP between two consecutive identical rows, not
	// a cap on a streak's duration: the flood this exists for was one row a
	// minute forever from a single retrying sidecar, which the 1/sec rate limiter
	// never trips and which still evicted every real security event out of the
	// console's 1000-row window in minutes. See coalesceAuthFailed (http.go) for
	// the bounds that keep a burst from collapsing into one row. The same
	// window folds the device routes' failure rows (device_audit_bounds.go).
	AuditCoalesceWindow time.Duration
	HostCapacityConfig
	// MaxConcurrentRuns caps non-terminal runs across the whole deployment, every
	// replica and every creation door (env WARDYN_MAX_CONCURRENT_RUNS); 0 or less
	// is unlimited. Past it every door answers 422 run_quota (createRun); POST /runs
	// refuses before minting and audits nothing for it, bar a create that loses
	// the race at the cap, which keeps its identity.mint row.
	MaxConcurrentRuns int
	// PreflightRatePerMin is WARDYN_PREFLIGHT_RATE_PER_MIN: the per-person
	// POST /runs/preflight rate (burst 5). 0 turns the limit off.
	PreflightRatePerMin int
	// Now is overridable in tests; defaults to time.Now.
	Now func() time.Time
	// OrgFederation is the hybrid audit forwarder's status (cmd/wardynd's
	// bootHybrid), nil when WARDYN_ORG_URL is unset. It feeds /healthz's
	// org_federation block, the wardyn_org_federation_lag gauge and the
	// create-run refusal once the organisation has revoked this device.
	OrgFederation func() federation.Status
	// BaseCtx is the process-lifetime base context used for detached background
	// work that MUST outlive the request that started it — specifically the
	// completion watcher dispatch starts after Exec. The request/dispatch ctx is
	// cancelled when the HTTP handler returns, which would kill a watcher
	// immediately; BaseCtx (threaded from main.go's rootCtx) keeps it alive for
	// the lifetime of the daemon and is cancelled on shutdown. Defaults to
	// context.Background() when unset (the watcher then only stops on process
	// exit).
	BaseCtx context.Context
	// Components advertises, per pluggable seam (identity, secret_store,
	// recording, policy_engine, sandbox, ...), the SELECTED running implementation
	// and the recommended production default, for honest /healthz visibility. Nil
	// => the components object is omitted.
	Components map[string]ComponentInfo
	// AgeKeyDurable: the age key was SUPPLIED (WARDYN_AGE_KEY), not generated at boot, or no local key holds anything (store
	// mode, a key service). False, stored secrets are unreadable after a restart: /setup/status warns. Computed in cmd/wardynd.
	AgeKeyDurable bool
	// SecretStoreExternal names the store every credential is written to in store mode ("Vault at
	// vault.example:8200"), "" in local mode; set, /setup/status shows store_external, not the age-key row.
	SecretStoreExternal string
	// SecretKeyService: the key service wrapping every stored data key ("Vault Transit at host"), or "" for the local key; set, /setup/status shows kek_service.
	SecretKeyService string
	// KEKRequired: WARDYN_KEK_REQUIRED; /setup/status shows kek_required_unmet while neither a key service nor an external store holds the credentials.
	KEKRequired bool
	// PlatformKeySeparate: WARDYN_PLATFORM_KEY_FILE gives the boot keys their own local key; false in local mode, /setup/status shows platform_shared (§2.13 c).
	PlatformKeySeparate bool
	// LocalLoopback reports whether the HTTP listen address binds only loopback.
	// It feeds SetupAuth.LocalLoopback so the wizard can explain the local-mode
	// posture. Computed at boot in cmd/wardynd (listenIsLoopback).
	LocalLoopback bool
	// LocalTrustForwarder, when true, tells the LocalMode no-auth bypass to accept a
	// NON-loopback request peer (r.RemoteAddr). It exists for the compose/team
	// deployment ONLY: there wardynd binds 0.0.0.0 inside a container but the host
	// publishes the port loopback-only (127.0.0.1:PORT), so a host UI/CLI request
	// arrives at wardynd from the docker bridge gateway, not loopback. The LAN
	// protection in that topology is the loopback PUBLISH (a LAN peer cannot reach a
	// 127.0.0.1-bound host port at all), not the peer check — so the peer gate is a
	// false positive there. The DNS-rebinding Host gate still applies. NEVER set this
	// for a directly-bound host-mode wardynd on 0.0.0.0: that would re-open the LAN
	// no-auth exposure the peer gate closes. Default false; set by compose only.
	LocalTrustForwarder bool
	// RequireOperatorSetEgress (WARDYN_REQUIRE_OPERATOR_SET_EGRESS, DEFAULT
	// TRUE) makes applyWorkspaceRequirements apply the same provenance gate
	// to a scan_seeded EGRESS requirement that the SECRET side has always applied
	// unconditionally (runs_create.go): only an operator_set requirement is
	// auto-added at launch, and a scan_seeded one — the workspace scanner reading
	// UNTRUSTED repo content — is skipped.
	//
	// Defaulting it TRUE narrows egress for existing workspaces on upgrade, and
	// that cost is deliberate: the secret side calls this exact boundary
	// "security-critical — do not relax", for a reason that applies verbatim to
	// egress — a hostile or simply never-reviewed repo could widen a run's
	// allowlist just by naming a host in a committed file, with no operator ever
	// acting.
	//
	// The upgrade cost is real and bounded: a workspace whose egress
	// requirements are scan_seeded stops having them auto-added, and the run's
	// warnings say which were skipped. The fix is an operator declaring the host
	// (making it operator_set), which is the action the gate exists to require.
	// Set false to auto-add scan_seeded egress requirements again.
	RequireOperatorSetEgress bool
	// DisableGitPATBroker (WARDYN_GIT_PAT_BROKER=off) is the operator escape
	// hatch for the never-resident git_pat lane.
	//
	// With the broker on (the default), a git_pat for a non-GitHub forge is
	// minted PROXY-SIDE and injected on the outbound leg, so the PAT never enters
	// the sandbox — the same posture github_token has always had. Turning it off
	// restores the in-sandbox lane, where the grant id rides the sandbox env and
	// the in-sandbox credential helper mints the PAT into the agent's process.
	//
	// It exists because the broker changes the git TRANSPORT for those hosts (an
	// insteadOf rewrite to a plain-HTTP broker path), and a forge that behaves
	// unexpectedly under that rewrite must not leave a fleet unable to clone.
	// Reverting is a flag flip and a restart, not a redeploy.
	DisableGitPATBroker bool
	// OIDCRoleMapConfigured reports whether WARDYN_OIDC_ROLE_MAP is non-empty —
	// the sso_rbac /setup/status check's gate. Only the presence, never the
	// mapping itself: the API layer has no use for individual entries, only
	// whether the operator has opted into role derivation at all (see
	// internal/auth/oidc's deriveRole). Computed at boot in cmd/wardynd.
	OIDCRoleMapConfigured bool
	// OIDCRedirectURL echoes WARDYN_OIDC_REDIRECT_URL (a URL, not a credential)
	// for the tls_cookie_posture /setup/status check, which flags an https
	// redirect issued while OIDCSecureCookies is still false — the classic
	// behind-an-ingress misconfiguration where WARDYN_TLS_TERMINATED was never
	// set. Computed at boot in cmd/wardynd.
	OIDCRedirectURL string
	// OIDCSecureCookies is the boot-computed secureCookies posture
	// (validateConfig, cmd/wardynd/main.go): true iff wardynd knows the
	// connection is TLS-protected end to end (built-in TLS or
	// WARDYN_TLS_TERMINATED) — the same value threaded into
	// oidc.Config.SecureCookies. Feeds tls_cookie_posture alongside
	// OIDCRedirectURL. Computed at boot in cmd/wardynd.
	OIDCSecureCookies bool
	// BasePath is WARDYN_BASE_PATH ("" = the host root): the prefix Handler
	// mounts every console route under (base_path.go).
	BasePath string
	// ScanAIAdvisor, when non-nil, enables the ADVISORY AI workspace-scan fallback
	// (internal/workspacescan/ai.go): after the deterministic DeriveProfile, when
	// the profile is low-confidence or left unrecognized samples (ShouldAdvise),
	// this gap-fills EMPTY fields only and can only RAISE NeedsReview — it never
	// overrides a deterministic fact and FAILS OPEN (any error keeps the
	// deterministic profile unchanged and the upload still succeeds). Nil (default)
	// = feature OFF, byte-identical to the deterministic-only behavior. Production
	// wires it (from WARDYN_SCAN_AI_ADVISOR) to a workspacescan.AdviseProfile
	// closure; it doubles as the test seam so tests inject a fake instead of
	// shelling out to a real coding-agent CLI.
	ScanAIAdvisor func(context.Context, workspacescan.ScanFacts, workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile
	// SSHListenAddr is WARDYN_SSH_LISTEN: the address the SSH gateway binds
	// (e.g. ":2222"). Empty = off = no listener, no new surface — ServeSSHGateway
	// no-ops when this is empty, so a caller may invoke it unconditionally.
	SSHListenAddr string
	// SSHAdvertiseAddr is WARDYN_SSH_ADVERTISE: the externally-reachable
	// host[:port] surfaced on /healthz and shown in the run-detail "Connect via
	// SSH" pane's `ssh` command — NOT what wardynd binds to (that's
	// SSHListenAddr), since a container/NAT deployment's bind and reachable
	// address routinely differ. Purely advisory copy; the gateway itself never
	// reads it.
	SSHAdvertiseAddr string
	// SSHHostKey is the gateway's persisted ed25519 host key (loadOrCreateSecret
	// pattern, cmd/wardynd), used to derive the ssh.Signer AddHostKey wants and
	// the fingerprint /healthz discloses ("verify on first connect" — public by
	// design, it identifies the server, it authenticates no one). Empty when the
	// gateway is disabled (cmd/wardynd only loads/generates it when
	// SSHListenAddr is set, so a deployment with SSH off never even mints this
	// secret).
	SSHHostKey ed25519.PrivateKey
	// SSHRoleTTL is WARDYN_SSH_ROLE_TTL: how stale a key's role_checked_at
	// (migration 0046) may be before sshAuth's admin-override path refuses it
	// — the bound on the OIDC-login re-check, since SSH itself has no live
	// session to read a current role from. Zero defaults to a sensible 24h in
	// New (matching the flag's own default), so a Config built without going
	// through cmd/wardynd's flags — every test harness, notably — gets the
	// same posture production does rather than an accidental zero-tolerance
	// TTL that fails every override.
	SSHRoleTTL time.Duration
	// APITokenMaxTTL is WARDYN_API_TOKEN_MAX_TTL: the longest lifetime a newly
	// minted API token may have. Zero (the default) means no cap. A mint that
	// asks for no TTL gets this one; a mint that asks for more is clamped to it.
	// It never touches a token already minted.
	APITokenMaxTTL time.Duration
	// RoleStampTTL is WARDYN_ROLE_STAMP_TTL: the oldest an API token's role and
	// group stamp (api_tokens.identity_stamped_at) may be before apiTokenAuth
	// refuses it until its owner signs in again. Zero, the default, is off: no
	// token is refused for the age of its stamp.
	RoleStampTTL time.Duration
	// UIListenAddr is WARDYN_UI_SANDBOX_LISTEN: the address the UI-sandbox
	// gateway binds (e.g. ":8081"). Empty = off = no listener, no new surface,
	// mirroring SSHListenAddr. It MUST NOT equal the console's -listen: relayed
	// content is sandbox-authored, and the second listener IS the origin
	// separation that keeps it away from the console's storage (boot refuses —
	// cmd/wardynd's validateUISandboxConfig).
	UIListenAddr string
	// UIAdvertiseURL is WARDYN_UI_SANDBOX_ADVERTISE: the externally-reachable
	// base URL of that listener (e.g. "https://wardyn-ui.example.com"), used to
	// build the enter-URL template /healthz publishes. Advisory copy, like
	// SSHAdvertiseAddr — the gateway binds UIListenAddr, never this.
	UIAdvertiseURL string
	// UIOriginTemplate is WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE: an optional PER-RUN
	// origin (e.g. "https://run-{run}.ui.example.com") for deployments with
	// wildcard DNS. Set, it gives every run its own browser origin — closing the
	// shared-origin residual path mode leaves — and the gateway then REFUSES an
	// enter served on any other host. Empty = shared path-mode origin, where one
	// run's page is separated from another's only by the path-scoped cookie.
	UIOriginTemplate string
	// UISessionTTL is WARDYN_UI_SANDBOX_SESSION_TTL: how long a minted relay
	// session stays usable, and — because the cookie carries its own issued-at
	// — how stale an ALREADY-minted one may be. The UI relay's sibling of
	// SSHRoleTTL: the SSH gateway bounds a stale admin-override stamp, this
	// bounds a stale relay session, and both exist because neither lane has a
	// live console session to re-read a current role from. Zero defaults to
	// defaultUISessionTTL in New (matching the flag's default), so a Config
	// built without going through cmd/wardynd's flags gets the shipped posture
	// rather than a zero TTL that would refuse every session.
	UISessionTTL time.Duration
	// UICookiePolicy is WARDYN_UI_SANDBOX_STRIP_COOKIES: which inbound cookies,
	// beyond the always-stripped wardyn_* namespace, the relay forwards to a
	// sandbox app. The zero value forwards every other cookie.
	UICookiePolicy UICookiePolicy
	// UISessionKey signs the wardyn_ui_sess relay cookie (HMAC-SHA256, >= 32
	// bytes, the loadOrCreateSecret pattern). Nil/short = gateway disabled: a
	// cookie that cannot be signed must never be issued.
	UISessionKey []byte
	// RunConfigKey seals each run's stored proxy config (32 bytes, the
	// wardyn-run-config-key boot key; run_proxy_config.go). Nil: none is kept.
	RunConfigKey []byte
	// DemoVideoBaseURL is WARDYN_DEMO_VIDEO_BASE_URL, validated at boot by
	// ValidateDemoVideoBaseURL (same seven-rule shape as an internal model
	// gateway: https://, no userinfo, no query/fragment). It re-points the
	// Getting Started demo episodes at an operator-run mirror for an
	// air-gapped deployment, where github.com is unreachable — both the
	// download URL episodeUrl's /healthz-reading caller builds and the CSP's
	// media-src this base's host is echoed into (cspMediaSrc,
	// security_headers.go). Empty (the default) = the two hardcoded GitHub
	// hosts, byte-identical to today. Control-plane-authored only, same trust
	// boundary as TrustedCAPEM above — never a SiteConfig field,
	// never agent-reachable.
	DemoVideoBaseURL string
}

// Server is the control-plane HTTP server. It is safe for concurrent use.
type Server struct {
	cfg    Config
	router chi.Router
	// metrics holds the /metrics scrape counters (see metrics.go). Zero value is
	// ready to use.
	metrics metrics
	// capRowsScanned counts capability-grant rows compared inside capBatch (see
	// capabilities.go). It is INSTRUMENTATION, read by nothing on any request
	// path: the per-request cost of the capability seam is chosen partly by the
	// member's own request body (spec.AllowedDomains), so "how much work did one
	// request buy" is a claim that has to be assertable rather than asserted —
	// capability_batch_test.go's growth law reads it. One atomic add per row
	// already being compared. Zero value is ready to use.
	capRowsScanned atomic.Int64
	// locks is the in-process fallback for the cross-replica locks (locks.go).
	// The audit chain verify sweep, the site-config and capability-enforcement
	// writers, the per-run operation lock and the two refresh single-flights
	// all take theirs there. Zero value is ready to use.
	locks lockState
	// lastTouch debounces the decision-ingest TouchRun UPDATEs per run (see
	// shouldTouch in internal.go). Zero value is ready to use.
	lastTouchMu sync.Mutex
	lastTouch   map[uuid.UUID]time.Time
	// keepaliveEvery overrides attachKeepaliveInterval for THIS server only. It
	// exists so a test can watch the keepalive tick without waiting 30 real
	// seconds; nothing sets it in production and the zero value means "use the
	// constant" (attachKeepaliveEvery). Per-server rather than a package var so
	// two tests running side by side cannot race on it.
	keepaliveEvery time.Duration
	// pingEvery overrides attachPingInterval for THIS server only (the liveness
	// probe on an otherwise-idle attach socket) — same reason and same
	// per-server shape as keepaliveEvery above: a test drives a dead-peer holder
	// on a millisecond clock instead of the real 30s budget.
	pingEvery time.Duration
	// maskBeat overrides maskCheckEvery for THIS server only (tests): how often
	// an in-flight consumer re-reads its run's fence.
	maskBeat time.Duration
	// runOutputDrainWaitOverride and runOutputRetryBaseOverride shrink the
	// output finaliser's drain barrier and retry backoff for a test; zero uses
	// runOutputDrainWait and runOutputRetryBase (run_output_final.go).
	runOutputDrainWaitOverride, runOutputRetryBaseOverride time.Duration
	// runOutputRecoverWaitOverride shrinks runOutputRecoverWait for a test.
	runOutputRecoverWaitOverride time.Duration
	// refRuleset caches the ONE outbound GitHub call the setup checklist makes,
	// so polling /setup/status (which the wizard does) cannot turn into a
	// per-poll API call or a rate-limit. Zero value is ready to use.
	// substrateProbe is this replica's cached probe of the runner's substrate,
	// read by /metrics and /setup/status (substrate_health.go).
	substrateProbe substrateProbeCache

	refRulesetMu   sync.Mutex
	refRulesetAt   time.Time
	refRulesetRow  SetupCheck
	refRulesetShow bool
	// sshSessions counts concurrent SSH "session" channels (shell/exec/
	// subsystem) per run, enforcing maxSSHSessionsPerRun (sshgateway.go). Zero
	// value is ready to use. Process-local like lastTouch above — the same
	// single-replica topology every in-memory bound in this codebase already
	// assumes (see secretmask.Registry's residual in THREAT-MODEL.md).
	sshSessionsMu sync.Mutex
	sshSessions   map[uuid.UUID]int
	// builds tracks per-workspace image builds (the wizard's Build step).
	// Zero value is ready to use.
	builds buildTracker
	// attachHolders tracks who currently holds each run's SHARED tmux PTY, so a
	// second client can be admitted read-only instead of silently competing for
	// the same terminal (see attach_holder.go). Process-local like sshSessions
	// and lastTouch above, and correct for the same reason: replicas>1 is
	// refused by construction (deployment.yaml). Zero value is ready to use.
	attachHolders attachHolderRegistry
	// creates lets a kill cancel a STARTING run's CreateSandbox (runs_create_cancel.go).
	creates   inflightCreates
	runEvents runEventHub // each run's lifecycle event ring (run_events.go)
	// execOutputs holds each non-interactive run's output tail (run_output.go).
	execOutputs execOutputTails
	// uiConns counts concurrent UI-gateway relay connections per run, enforcing
	// maxUIConnsPerRun (uigateway.go) — each one is a live socat exec in the
	// sandbox. uiReady caches the per-(run,app) launcher probe, and uiProxy is
	// the single shared reverse proxy + exec-lane transport built on first use.
	// All process-local, like sshSessions and lastTouch above and correct for
	// the same reason (replicas>1 is refused by construction). Zero values are
	// ready to use.
	uiConnsMu sync.Mutex
	uiConns   map[uuid.UUID]int
	uiReadyMu sync.Mutex
	uiReady   map[string]time.Time
	// uiReassert debounces the relay's REQUEST-path authorization re-check, one
	// entry per minted session (uiReassertKey) — see uiReassertRelay.
	uiReassertMu sync.Mutex
	uiReassert   map[string]time.Time
	uiProxyOnce  sync.Once
	uiProxy      *httputil.ReverseProxy
	// authFailedLimiter rate-bounds the auth.fail audit emit (see
	// adminAuth/auditAuthFailed in http.go) so a scanner cannot flood the
	// append-only log. Zero value is ready to use.
	authFailedLimiter authFailedLimiter
	// authFailedStreak is the open run of identical consecutive auth.fail rows
	// the coalescer is folding (see coalesceAuthFailed, http.go). Process-local
	// like the bounds above. Zero value is ready to use.
	authFailedStreakMu sync.Mutex
	authFailedStreak   *authFailedStreak
	// identityExpiredSeen is the per-run once-guard behind run.identity.expire
	// (claimIdentityExpired, http.go): a run whose identity has expired keeps
	// calling /internal/* and 401ing, so the row has to be emitted once per run
	// rather than once per request, or the audit log floods.
	// It is CLAIMED BEFORE the run read, so a repeat refusal costs no store read
	// either, and bounded at 4096 entries exactly like lastTouch. In memory and
	// process-local, like lastTouch/sshSessions above: a restart may re-emit once
	// per run, which is the acceptable end of the trade.
	identityExpiredMu   sync.Mutex
	identityExpiredSeen map[uuid.UUID]bool
	// dirLimiter rate-bounds GET /access/directory/search PER PRINCIPAL — it is
	// hit once per keystroke, and each miss is an upstream Graph call
	// (directory_search.go). Zero value is ready to use.
	dirLimiter principalLimiter
	// preflightLimiter rate-bounds POST /runs/preflight per person
	// (preflight.go); nil when Config.PreflightRatePerMin is 0 (off).
	preflightLimiter *principalLimiter
	deviceRouteState // the device routes' process state (server_devices.go)
	runLeaseState    // the run lease sweep's process state (run_lease_server.go)
	// pause is the pause sweep's process-local state (run_pause.go).
	pause pauseClocks
	// ssoRefreshMu guards ssoRefreshSpent, which the control-plane AWS SSO
	// refresher owns (awssso_refresh.go). The refresh is single-flight per owner
	// through a cross-replica lock (locks.go) that encloses re-read -> expiry
	// check -> CreateToken -> Put, so two dispatches of the same principal, on
	// this or another replica, cannot both redeem one rotating refresh token
	// (the second re-reads inside the lock and finds it already renewed).
	// ssoRefreshSpent records the fingerprints of refresh tokens the OIDC
	// endpoint has already told us are gone, keyed to the TOKEN rather than the
	// credential, so a later capture is never pre-marked dead. Process-local
	// like sshSessions and lastTouch above — but, unlike them, a CACHE rather
	// than the source of truth: it is write-through and read-once-memoized
	// against store.AWSSSOSpentTokenStore (Postgres), so a restart no longer
	// re-grades an already-spent credential `live` (#149) — the first read of a
	// given fingerprint after a restart costs one best-effort row read, and
	// every read after that (in THIS process) costs nothing.
	// ponytail: ssoRefreshSpent grows one small entry per spent token per daemon
	// lifetime — bound it only if that ever stops being negligible.
	ssoRefreshMu    sync.Mutex
	ssoRefreshSpent map[string]bool
	// adoEntraTokens reuses a minted Azure DevOps access token across the
	// per-host grants of one run (injection_ado.go), so a sidecar's boot does
	// not rotate one person's refresh token once per host.
	adoEntraTokens adoEntraAccessCache
	// adoSignInEnds counts each person's disconnects and erases, so a run
	// token created while one ran revokes itself (ado_pat_console.go).
	adoSignInEnds adoSignInEnds
	// adoPATs replaces the sign-in configuration's own vssps client
	// (ADOEntraConfig.patClient) for a `minted_pat` run's creates and revokes;
	// set by tests only. adoRunPATs holds each run's current token in memory
	// (ado_run_pat_cache.go).
	adoPATs    adoPATClient
	adoRunPATs adoRunPATCache
	// bg tracks every goroutine spawned through goBackground — work detached
	// from a request so the answering call does not wait for it
	// (finishHarnessLoginLaunch, killTeardownTail's supersede caller).
	// http.Server (cmd/wardynd/boot_serve.go) only waits for in-flight
	// HANDLERS to return; these goroutines outlive their handler by design, so
	// Shutdown alone would let a SIGTERM cut one off mid-teardown — a run left
	// KILLED with its sandbox still up, its credentials unrevoked and no
	// run.kill row. See WaitBackground, which cmd/wardynd calls after
	// httpSrv.Shutdown so an orderly stop gives this work its own bounded
	// window to finish. Zero value is ready to use.
	bg sync.WaitGroup
	// bgWaitBudget overrides backgroundShutdownBudget for THIS server only,
	// same shape as keepaliveEvery/pingEvery above: a test proving WaitBackground
	// actually gives up at its bound needs to do so in milliseconds, not the
	// real ~35s budget. Zero (the default) means "use backgroundShutdownBudget".
	// See background.go; signInCaptureKillGrace is ssotoken.go's (tests set 0).
	bgWaitBudget, signInCaptureKillGrace time.Duration
}

// New constructs a Server and builds its router. It does not start listening.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RunnerTarget == "" {
		cfg.RunnerTarget = "docker"
	}
	if cfg.SSHRoleTTL <= 0 {
		cfg.SSHRoleTTL = defaultSSHRoleTTL
	}
	if cfg.UISessionTTL <= 0 {
		cfg.UISessionTTL = defaultUISessionTTL
	}
	if cfg.RunOutputTailBytes <= 0 {
		cfg.RunOutputTailBytes = defaultRunOutputTailBytes
	}
	if cfg.ExecOutputTailTTL <= 0 {
		cfg.ExecOutputTailTTL = defaultExecOutputTailTTL
	}
	if cfg.BaseCtx == nil {
		cfg.BaseCtx = context.Background()
	}
	s := &Server{cfg: cfg, signInCaptureKillGrace: signInCaptureKillGrace,
		deviceRouteState: deviceRouteState{
			enrolLimiter:         principalLimiter{rate: enrolRatePerSec, burst: enrolBurst, max: enrolLimiterMaxPeers},
			ingestFailureLimiter: principalLimiter{rate: ingestFailureRatePerSec, burst: ingestFailureBurst, max: ingestFailureMaxDevices},
		},
	}
	if cfg.PreflightRatePerMin > 0 {
		s.preflightLimiter = &principalLimiter{rate: float64(cfg.PreflightRatePerMin) / 60, burst: preflightBurst, max: preflightLimiterMaxPeople}
	}
	s.router = s.routes()
	// drain the durable audit-fallback spool back into the store once it
	// recovers, so a PG outage no longer leaves spooled events permanently invisible
	// to /audit and `wardyn audit`. Uses BaseCtx (daemon lifetime) so it survives
	// individual requests and stops on shutdown. No-op unless both are wired.
	if cfg.AuditSpool != nil && cfg.AuditDrainRecorder != nil {
		go cfg.AuditSpool.StartDrain(cfg.BaseCtx, cfg.AuditDrainRecorder, auditSpoolDrainInterval, auditSpoolDrainBatch)
	}
	return s
}

// Audit-spool drain cadence: a modest ticker with a bounded per-tick batch so a
// large post-outage backlog replays over several ticks without holding the spool
// lock (blocking Append) for long or hammering the store in one burst.
const (
	auditSpoolDrainInterval = 30 * time.Second
	auditSpoolDrainBatch    = 200
)

// handleLogout terminates the human session. FIX #6: it is mounted as
// POST /api/v1/auth/logout inside the humanOrAdminAuth group so the UI's existing
// POST actually kills the session (the old code only had a root GET /auth/logout,
// which the POST never reached — 404 — leaving the HttpOnly OIDC cookie valid).
//
//   - OIDC session mode: delegate to the OIDC LogoutHandler, which clears the
//     HttpOnly wardyn_session cookie (and redirects to "/").
//   - Admin-token / local mode (OIDC not configured): there is NO server-side
//     session to kill — the client just drops its local bearer token. Return 204
//     (no content). The nil-OIDC guard is REQUIRED so this never panics in
//     local/token mode.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OIDC != nil {
		s.cfg.OIDC.LogoutHandler(w, r) // clears the HttpOnly session cookie
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// recordAudit is a best-effort audit emit for control-plane-originated events.
// Audit failures must never block the primary operation (the Recorder owns
// durability/retry); we swallow the error after the event is constructed.
func (s *Server) recordAudit(ctx context.Context, ev types.AuditEvent) {
	if s.cfg.Audit == nil {
		return
	}
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	if ev.Time.IsZero() {
		ev.Time = s.cfg.Now().UTC()
	}
	ev.Data = audit.StampDelegation(ctx, ev.Data) // delegation is recorded as delegation (#1142)
	// Invariant 6, C1: the audit log is the system of record. A failed durable
	// write is handled by the shared recorder chain (spoolingRecorder below
	// maskingRecorder in cmd/wardynd), which masks, logs loudly, and spools the
	// event to the durable local fallback so it is never silently lost — for every
	// audit writer, not just this one. So there is no API-layer-only spool here.
	_ = s.cfg.Audit.Record(ctx, ev)
}

// auditEvent is a small constructor used across handlers.
func (s *Server) auditEvent(runID *uuid.UUID, actorType types.ActorType, actor, action, target, outcome string, data []byte) types.AuditEvent {
	return types.AuditEvent{
		ID:        uuid.New(),
		Time:      s.cfg.Now().UTC(),
		RunID:     runID,
		ActorType: actorType,
		Actor:     actor,
		Action:    action,
		Target:    target,
		Outcome:   outcome,
		Data:      data,
	}
}
