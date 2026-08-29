// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package api wires Wardyn's control-plane REST surface (the wardynd binary).
// It contains ZERO target-specific code (the parity rule): it talks to runners
// only through the runner.Runner interface, and to identity/secrets/broker only
// through their contract interfaces. Every security decision fails closed.
//
// Route map (see the REST contract in the architecture brief):
//
//	Public (admin bearer):
//	  POST /api/v1/runs ; GET /api/v1/runs ; GET /api/v1/runs/{id}
//	  GET  /api/v1/runs/{id}/grants
//	  POST /api/v1/runs/{id}/kill
//	  GET  /api/v1/runs/{id}/attach   (WebSocket: interactive PTY)
//	  GET  /api/v1/approvals?state=&run_id= ; POST /api/v1/approvals/{id}/approve|deny
//	  GET  /api/v1/audit?run_id=&since=&until=&action=&action_prefix=&actor_type=&outcome=
//	  POST /api/v1/policies ; GET /api/v1/policies ; GET /api/v1/policies/{id}
//	  PUT  /api/v1/policies/{id} ; DELETE /api/v1/policies/{id}
//	  GET  /metrics                   (Prometheus text exposition)
//	Anonymous:
//	  GET  /healthz
//	  GET  /readyz
//	Internal (run-token bearer, identity.Provider.Verify aud="wardyn-internal"):
//	  POST /api/v1/internal/decisions
//	  POST /api/v1/internal/approvals ; GET /api/v1/internal/approvals/{id}
//	  POST /api/v1/internal/credentials/mint
//	Ground-truth (host-sensor bearer, identity.Provider.Verify aud="wardyn-groundtruth"):
//	  POST /api/v1/internal/groundtruth   (eBPF/Tetragon kernel-event batch)
package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
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

// ebpfHeartbeatTTL is how recent the most recent kernel.sensor.heartbeat must
// be for /healthz to report ebpf_groundtruth=healthy. Past it, the stream is
// degraded; with no heartbeat ever, it is unavailable. This makes the overclaim
// structurally impossible: the stream is "healthy" only while events arrive.
const ebpfHeartbeatTTL = 2 * time.Minute

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
	// Store is the abstract persistence seam (run/policy/grant/approval/audit
	// CRUD + reads). The control plane talks to this instead of *pgxpool.Pool
	// directly, so a future pure-Go backend can be swapped in. Defaults to a
	// store.NewPG(Pool) adapter when wired from wardynd.
	Store store.Store
	// Identity mints/verifies/revokes per-run identities (embedded by default).
	Identity identity.Provider
	// Approvals is the approval FSM service.
	Approvals ApprovalService
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
	// sinks configured has nothing to report. See D2.
	AuditSinkDrops func() map[string]int64
	// Runner launches sandboxes. Nil => headless API-only mode.
	Runner runner.Runner
	// AdminToken gates the public API (constant-time bearer compare). Empty
	// disables the public API entirely (fail closed) except /healthz.
	AdminToken string
	// LocalMode enables LOCAL HOST MODE: the public-API auth (humanOrAdminAuth)
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
	// SubscriptionPostureOK reports whether this deployment may resolve a SHARED
	// subscription credential (one operator's live Anthropic OAuth token) into an
	// agent run. Decided once at boot by subscriptionInjectPosture (cmd/wardynd) —
	// false on the k8s runner, false when an OIDC issuer is configured, and false
	// off local mode unless WARDYN_ALLOW_SHARED_SUBSCRIPTION waives that one clause.
	//
	// This is a COMPLIANCE boundary, not only a security one: the harness vendor's
	// terms require each end user to authenticate with their own credential, so a
	// multi-user deployment sharing one subscription puts the OPERATOR in breach.
	// It is enforced at three depths — providers are not constructed at boot,
	// dispatch does not author a sentinel grant, and the injection sink refuses to
	// resolve one. The sink is the load-bearing layer: four other code paths can
	// put a sentinel grant on a run without consulting dispatch (a stored policy
	// naming it, an integration_id, a recorded profile, the managed fallback).
	SubscriptionPostureOK bool
	// SubscriptionPostureReason says WHY injection is unavailable, so the setup and
	// integrations surfaces can explain it instead of rendering identically to
	// "the operator never logged in". Empty when SubscriptionPostureOK is true.
	SubscriptionPostureReason string
	// TrustDomain is surfaced in /healthz and used for run SPIFFE ids.
	TrustDomain string
	// DefaultPolicy is applied to runs created without an explicit policy_id.
	DefaultPolicy types.RunPolicySpec
	// RunnerTarget records which target a run is dispatched to ("docker"|"k8s"),
	// or "none" for a headless control plane (-runner none: runs stay PENDING).
	// Defaults to "docker".
	RunnerTarget string
	// UIDir, when set, serves a SPA from this directory at "/".
	UIDir string
	// ControlPlaneURL is the externally-reachable base URL handed to sidecars
	// (proxy config) so they can call the internal endpoints.
	ControlPlaneURL string
	// ProxyURL, when set, overrides the WARDYN_PROXY_URL injected into sandbox
	// env. Defaults to "http://wardyn-proxy:3128" (the per-run proxy sidecar
	// hostname set by the docker driver). Non-secret: it is a network address,
	// not a credential.
	ProxyURL string
	// RecordingStore, when set, serves PTY session replays under
	// GET /api/v1/runs/{id}/recording/{id} (admin-gated) and accepts uploads
	// via PUT /api/v1/runs/{id}/recording (run-token auth).
	RecordingStore recording.Store
	// OIDC, when set, enables human SSO: it mounts /auth/login,/auth/callback,
	// /auth/logout and composes oidc.Middleware in front of the admin-gated API
	// so a valid session cookie OR the admin bearer token authenticates a caller.
	// The admin token still works for the CLI when OIDC is configured.
	OIDC *oidc.Authenticator
	// SessionRevocations (D16) is the write side of the revoke-a-human-now
	// lever: handleRevokeSessions calls RevokeSub/RevokeAll on it.
	// oidc.Middleware holds the matching READ side (Config.Revocations, wired
	// by the same cmd/wardynd adapter) — this field is nil exactly when OIDC
	// is unconfigured, and the revoke-sessions route only mounts when it is
	// set (see routes.go).
	SessionRevocations oidc.SessionRevocations
	// OperatorEmails is WARDYN_OIDC_OPERATOR_EMAILS, the legacy admin allowlist.
	// internal/api no longer reads this field directly: requireOperator/isOperator
	// (http.go) gate on the session's B1-derived Role instead. The list still
	// matters — cmd/wardynd feeds the SAME value into oidc.Config.LegacyAdminEmails,
	// so an email on it is still an additional RoleAdmin match at OIDC-login role
	// derivation time (see internal/auth/oidc's deriveRole) — it just flows through
	// Session.Role now instead of being re-checked a second time here. Kept on
	// Config (not deleted) so that wiring keeps compiling; read it here only if you
	// need the raw configured list for display (e.g. an admin-facing settings page),
	// never to gate a request.
	OperatorEmails []string
	// MemberMounts is the operator/MDM-set posture for MEMBER-authored local_dir
	// binds (WARDYN_MEMBER_WORKSPACE_ROOTS + _MAP + WARDYN_MEMBER_WRITABLE_ROOTS
	// + _DENY, parsed at boot by runner.ParseMemberMountPolicy). The ZERO VALUE
	// — the default — means a member may not onboard a host directory at all
	// (repos and operator-owned workspaces are unaffected), which is the
	// fail-closed posture the section-(c) threat model requires. It bounds ONLY
	// mounts on a member-OWNED workspace; an operator's mounts are never
	// narrowed by it. See internal/runner/member_mount.go.
	MemberMounts runner.MemberMountPolicy
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
	// AgentAnthropicModel, when set, pins the ANTHROPIC_MODEL env inside a
	// claude-code sandbox (e.g. "opus") so the agent uses a specific model rather
	// than the account/CLI default (which a promo can push to a cheaper model like
	// Fable). Empty = unset; the CLI's own default is used. Applies in both
	// subscription and api-key auth modes.
	AgentAnthropicModel string
	// BedrockRegion / BedrockModel, when BOTH set, opt a claude-code run into the
	// Amazon Bedrock Anthropic transport (CLAUDE_CODE_USE_BEDROCK) instead of the
	// default api-key/proxy-inject path — an enterprise path with no direct
	// Anthropic egress, billed via AWS. BedrockModel is a Bedrock model id (a
	// cross-region inference-profile id like "us.anthropic.claude-..." is what
	// claude-code actually expects, not a bare foundation-model id). Boot-time
	// config only (mirrors AgentAnthropicModel — no live admin write path); the
	// AWS credentials themselves come from the secret store (aws-access-key-id /
	// aws-secret-access-key / optional aws-session-token), read directly at
	// dispatch time because Bedrock's AWS SigV4 request signing can't be
	// proxy-injected the way a static x-api-key header can (see runs.go
	// resolveBedrockAuth). Empty BedrockRegion or BedrockModel disables Bedrock
	// entirely; a subscription-mode run always takes priority over Bedrock.
	BedrockRegion string
	BedrockModel  string
	// BedrockAWSConfigDir, when set, bind-mounts a host AWS config directory
	// (a `~/.aws`) READ-ONLY into the sandbox at /home/agent/.aws, so the AWS
	// SDK inside the run resolves credentials itself — including short-lived AWS
	// SSO / IAM Identity Center sessions, which it refreshes on demand. This is
	// the HOST-MODE alternative to pasting static aws-access-key-id/-secret
	// secrets (which expire under SSO and must be re-pasted): with the mount,
	// `aws sso login` on the host is enough and nothing is stored in Wardyn.
	// It is OFF by default. Host-mode setup.sh auto-detects ~/.aws; the compose
	// stack supports it too via the WARDYN_BEDROCK_AWS_DIR bind (same
	// host==container path, :ro — see deploy/compose/docker-compose.yaml), an
	// opt-in the operator sets explicitly. Because it mounts the operator's
	// ambient cloud credentials into runs, it is a single-user / self-hosted
	// choice, not for a shared multi-tenant service (invariant 1) — the deliberate
	// residency tradeoff already accepted for the ~/.claude subscription mount.
	// Empty = disabled. Takes precedence over the resident static-key path but not
	// over a bedrock-api-key bearer.
	BedrockAWSConfigDir string
	// BedrockAWSProfile, when set, is passed as AWS_PROFILE into the sandbox so
	// the SDK selects a named profile from the mounted config (common with SSO:
	// `aws sso login --profile X`). Only meaningful with BedrockAWSConfigDir.
	BedrockAWSProfile string
	// BedrockAWSSSORegion is the AWS region whose SSO endpoints (oidc.<r>,
	// portal.sso.<r>) the sandbox is allowed to reach so the SDK can exchange an
	// SSO token for role credentials. It often differs from BedrockRegion.
	// Empty defaults to BedrockRegion. Used by the BedrockAWSConfigDir mount path
	// AND by the containerized `aws sso login` (harnessLogin.loginEgress), which
	// has no other way to know which regional SSO endpoints to allow.
	BedrockAWSSSORegion string
	// Secrets is the at-rest secret store. It backs the admin secret-management
	// endpoints (PUT/DELETE/list — values are NEVER readable via the API) and
	// the internal injection-resolve endpoint the proxy calls at startup. Nil
	// disables both surfaces.
	Secrets secretstore.Store
	// MaskRegistry, when non-nil, is used to mask verbatim secret values from
	// PTY capture / asciicast uploads before they reach the RecordingStore.
	// A nil registry disables masking (existing tests stay green).
	MaskRegistry *secretmask.Registry
	// SubscriptionToken, when non-nil, yields the operator's LIVE Anthropic
	// subscription OAuth access token from the resident ~/.claude credentials.
	// The internal injection-resolve endpoint uses it to inject a fresh token
	// per request for subscription runs (secret name subscriptionOAuthSecret),
	// so the sandbox holds only an inert sentinel instead of a copy that goes
	// stale. Nil disables the subscription-injection path (falls back to the
	// resident-copy behavior).
	SubscriptionToken subscription.Provider
	// ManagedToken, when non-nil, yields the Wardyn-MANAGED Anthropic subscription
	// token — a long-lived `claude setup-token` the operator captured via the
	// container-login flow, stored age-encrypted. The injection sink resolves the
	// types.ManagedOAuthSecret sentinel through it, exactly like SubscriptionToken
	// resolves the resident-host sentinel. This is what credentials a subscription
	// run in a COMPOSE deployment whose distroless wardynd has no host ~/.claude.
	// Nil disables the managed-injection path.
	ManagedToken subscription.Provider
	// DisableSubscriptionInject is the operator ESCAPE HATCH: when true (env
	// WARDYN_SUBSCRIPTION_INJECT=off), subscription runs keep the legacy
	// resident-copy behavior (the mounted credential, which can go stale) instead
	// of auto-enabling TLS-MITM + injecting the live host token. Default false =
	// the safe proxy-side default whenever a SubscriptionToken provider is wired.
	DisableSubscriptionInject bool
	// Now is overridable in tests; defaults to time.Now.
	Now func() time.Time
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
	// AgeKeyDurable reports whether the secret store's age key was SUPPLIED
	// (WARDYN_AGE_KEY/-age-key non-empty) vs ephemerally generated at boot. When
	// false, stored secrets are unreadable after a restart — surfaced by
	// /setup/status as a durability warning. Computed at boot in cmd/wardynd.
	AgeKeyDurable bool
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
	// RequireOperatorSetEgress (WARDYN_REQUIRE_OPERATOR_SET_EGRESS, DEFAULT TRUE
	// SINCE 0.7) makes applyWorkspaceRequirements apply the same provenance gate
	// to a scan_seeded EGRESS requirement that the SECRET side has always applied
	// unconditionally (runs_create.go): only an operator_set requirement is
	// auto-added at launch, and a scan_seeded one — the workspace scanner reading
	// UNTRUSTED repo content — is skipped.
	//
	// It shipped off, because flipping it narrows egress for existing workspaces
	// on upgrade. 0.7 flips it anyway: the asymmetry was the anomaly. The secret
	// side calls this exact boundary "security-critical — do not relax", for a
	// reason that applies verbatim to egress — a hostile or simply never-reviewed
	// repo could widen a run's allowlist just by naming a host in a committed
	// file, with no operator ever acting.
	//
	// The upgrade cost is real and bounded: a workspace whose egress
	// requirements are scan_seeded stops having them auto-added, and the run's
	// warnings say which were skipped. The fix is an operator declaring the host
	// (making it operator_set), which is the action the gate exists to require.
	// Set false to restore pre-0.7 behavior.
	RequireOperatorSetEgress bool
	// DisableGitPATBroker (WARDYN_GIT_PAT_BROKER=off) is the OPERATOR ESCAPE
	// HATCH for the never-resident git_pat lane.
	//
	// With the broker on (the default), a git_pat for a non-GitHub forge is
	// minted PROXY-SIDE and injected on the outbound leg, so the PAT never enters
	// the sandbox — the same posture github_token has always had. Turning it off
	// restores the pre-0.7 behaviour, where the grant id rides the sandbox env and
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
	// UISessionKey signs the wardyn_ui_sess relay cookie (HMAC-SHA256, >= 32
	// bytes, the loadOrCreateSecret pattern). Nil/short = gateway disabled: a
	// cookie that cannot be signed must never be issued.
	UISessionKey []byte
}

// ComponentInfo describes one pluggable seam's selection for /healthz. Runtime
// facts only: Selected is ALWAYS the actual running implementation. The
// recommended-vs-shipped split is prose and lives in docs/PLUGGABILITY.md +
// ROADMAP.md. Source is "default" or "configured".
type ComponentInfo struct {
	Selected string `json:"selected"`
	// Available lists every implementation self-registered in this build's seam
	// registry (so /healthz truthfully shows what THIS binary can run — e.g. a
	// tagless build advertises sandbox.available=[]). Empty for seams without a
	// registry (policy_engine today).
	Available []string `json:"available,omitempty"`
	Source    string   `json:"source,omitempty"`
}

// Server is the control-plane HTTP server. It is safe for concurrent use.
type Server struct {
	cfg    Config
	router chi.Router
	// metrics holds the /metrics scrape counters (see metrics.go). Zero value is
	// ready to use.
	metrics metrics
	// lastTouch debounces the decision-ingest TouchRun UPDATEs per run (see
	// shouldTouch in internal.go). Zero value is ready to use.
	lastTouchMu sync.Mutex
	lastTouch   map[uuid.UUID]time.Time
	// refRuleset caches the ONE outbound GitHub call the setup checklist makes,
	// so polling /setup/status (which the wizard does) cannot turn into a
	// per-poll API call or a rate-limit. Zero value is ready to use.
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
	// siteConfigMu serializes the single site-config document's four
	// read-modify-write writers (PUT /site-config, PUT/DELETE/POST-adopt
	// /integrations/{id}) — an unconditional Postgres upsert (store.go's
	// PutSiteConfig) with no CAS, so two overlapping RMWs on one process can
	// otherwise silently erase each other's write (SEAM-1: a hand-authored
	// integration row, a default_for:[agent_runs] mark, or the just-saved
	// corp proxy/redirect config). Correct because replicas>1 is refused by
	// construction (deployment.yaml) — a single in-process mutex covers every
	// writer that can ever exist. Zero value is ready to use. ponytail:
	// promote to a PG advisory lock (gt_rotator.go's pattern) if
	// allowMultiReplica ever becomes real.
	siteConfigMu sync.Mutex
	// capEnforcementMu is siteConfigMu's sibling for the OTHER whole-document
	// replace this package added If-Match/ETag optimistic concurrency to
	// (etag.go): PUT /permissions/enforcement reads the current enforcement
	// map to check If-Match against, then writes the new one, and this mutex
	// is what keeps that check-then-write atomic against a second overlapping
	// PUT on the same process — same reasoning as siteConfigMu above (single
	// replica by construction), just a second lock because the two documents
	// live in different tables and a writer on one must never block a writer
	// on the other. Zero value is ready to use.
	capEnforcementMu sync.Mutex
	// attachHolders tracks who currently holds each run's SHARED tmux PTY, so a
	// second client can be admitted read-only instead of silently competing for
	// the same terminal (see attach_holder.go). Process-local like sshSessions
	// and lastTouch above, and correct for the same reason: replicas>1 is
	// refused by construction (deployment.yaml). Zero value is ready to use.
	attachHolders attachHolderRegistry
	// uiConns counts concurrent UI-gateway relay connections per run, enforcing
	// maxUIConnsPerRun (uigateway.go) — each one is a live socat exec in the
	// sandbox. uiReady caches the per-(run,app) launcher probe, and uiProxy is
	// the single shared reverse proxy + exec-lane transport built on first use.
	// All process-local, like sshSessions and lastTouch above and correct for
	// the same reason (replicas>1 is refused by construction). Zero values are
	// ready to use.
	uiConnsMu   sync.Mutex
	uiConns     map[uuid.UUID]int
	uiReadyMu   sync.Mutex
	uiReady     map[string]time.Time
	uiProxyOnce sync.Once
	uiProxy     *httputil.ReverseProxy
	// authFailedLimiter rate-bounds the auth.failed audit emit (see
	// adminAuth/auditAuthFailed in http.go) so a scanner cannot flood the
	// append-only log. Zero value is ready to use.
	authFailedLimiter authFailedLimiter
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
	if cfg.BaseCtx == nil {
		cfg.BaseCtx = context.Background()
	}
	s := &Server{cfg: cfg}
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

// Handler returns the configured http.Handler (the chi router).
func (s *Server) Handler() http.Handler { return s.router }

// securityHeaders sets the console's defense-in-depth response headers on EVERY
// response — /healthz, /auth/*, /api/v1/* and the SPA alike. The console is a
// full-admin surface (it holds the admin bearer in web storage and drives
// approve/deny/kill), so frame-ancestors 'none' is load-bearing: a framed click
// originates INSIDE the wardyn origin, which means the local-mode CSRF Origin
// guard (http.go) would pass it.
//
// Each CSP directive is deliberate — do not "tidy" them:
//   - style-src allows 'unsafe-inline': xterm injects a theme <style> at
//     runtime, Radix sets style attributes, and the no-UI-bundle fallback page
//     (ui.go fallbackStatusPage) carries its own inline <style>.
//   - font-src allows data:: the built console CSS embeds its woff2 inline, and
//     data: is not covered by 'self'.
//   - connect-src names ws:/wss: explicitly. 'self' matching a same-origin
//     WebSocket is CSP3 behavior WebKit has historically not implemented, and
//     the PTY attach must not silently die there.
//   - media-src names github.com and release-assets.githubusercontent.com: the
//     Getting Started demo episodes (ui/src/app/lib/demo-videos.ts) are GitHub
//     release assets, loaded only on explicit click (no autoplay, no
//     prefetch). The download link 302s from the first host to the second —
//     CSP checks the redirect target, not just the link — and GitHub has moved
//     that host before, so RELEASING.md's re-shoot step re-verifies it live.
//   - script-src is 'self' plus 'wasm-unsafe-eval' — the RECORDING replay player
//     (asciinema-player, a WASM VT core) calls WebAssembly.instantiate(), which
//     browsers refuse under a bare default-src 'self'. 'wasm-unsafe-eval' permits
//     WASM compilation ONLY; it is not 'unsafe-eval' (no JS eval/Function), so
//     scripts stay locked to same-origin. Without it the player renders its chrome
//     but never plays (duration stuck at --:--). The live attach terminal (xterm,
//     no WASM) is unaffected either way. run-ui-e2e asserts on console errors so a
//     future CSP tightening against a WASM component can't silently regress this.
//
// No HSTS: the default posture is plain http on loopback, where an HSTS header
// would poison every other localhost port.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; " +
		"object-src 'none'; connect-src 'self' ws: wss:; " +
		"media-src 'self' https://github.com https://release-assets.githubusercontent.com; " +
		"script-src 'self' 'wasm-unsafe-eval'; " +
		"style-src 'self' 'unsafe-inline'; font-src 'self' data:"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// handleReadyz is the READINESS probe: unlike /healthz (liveness — "is the
// process up"), it proves the store is actually reachable. /healthz alone
// reported "ok" unconditionally, so a dead/unreachable Postgres still read
// healthy — a dead DB never took the pod out of the Service's endpoint list.
// Deliberately anonymous like /healthz (discloses nothing beyond up/down) and
// deliberately a SEPARATE endpoint from /healthz rather than teaching it to
// fail: liveness/startup also point at /healthz in the chart, and a DB blip
// must not restart-loop a pod that is otherwise fine.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), storePingTimeout)
	defer cancel()
	if s.cfg.Store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	if err := s.cfg.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error", "postgres": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleHealthz reports liveness plus the identity provider name so the trust
// boundary (embedded vs spire) is always visible to operators and the UI.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	idp := ""
	if s.cfg.Identity != nil {
		idp = s.cfg.Identity.Name()
	}
	runnerName := ""
	caps := []types.ConfinementClass(nil)
	var substrates map[types.ConfinementClass]string
	if s.cfg.Runner != nil {
		runnerName = s.cfg.Runner.Name()
		if c, err := s.cfg.Runner.Capabilities(r.Context()); err == nil {
			caps = c.ConfinementClasses
			substrates = c.Resolved
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		// version is the daemon's own build. It is a DELIBERATE disclosure on the
		// anonymous /healthz (unlike the capability enumeration below, which is
		// admin-gated on /setup/status): a support issue or a CLI/server skew after
		// a rolling upgrade has to be answerable without a credential, and the
		// sign-in screen reads /healthz before anyone is authenticated.
		"version": version.Version,
		// sso reports whether the OIDC login flow is mounted (/auth/login). The
		// sign-in screen reads it BEFORE anyone is authenticated to decide whether to
		// offer the SSO link — without it the console has no usable sign-in at all in
		// the OIDC-configured, admin-token-empty deployment wardynd itself suggests
		// (cmd/wardynd: "Set WARDYN_ADMIN_TOKEN, enable OIDC, or use -local-mode").
		// One bit, no configuration detail: it discloses nothing /auth/login's own
		// presence does not.
		"sso":               s.cfg.OIDC != nil,
		"identity_provider": idp,
		"trust_domain":      s.cfg.TrustDomain,
		"runner":            runnerName,
		// confinement_classes are the enforceable isolation LEVELS; the
		// confinement_substrates map names WHICH runtime backs each (e.g.
		// "CC3":"oci/kata-qemu") — honest visibility into the pluggable substrate.
		// confinement_names is the static CC-code -> friendly-tier-name dictionary
		// (types.ConfinementClassNames, mirroring the UI's cc-meta.ts) so a
		// scriptable consumer can learn "CC1" means "Fence" without hardcoding it.
		"confinement_classes":    caps,
		"confinement_substrates": substrates,
		"confinement_names":      types.ConfinementClassNames,
		// components reports the SELECTED pluggable-component impl per seam, plus
		// what this build's registries actually hold. Runtime facts only.
		"components": s.cfg.Components,
		// ebpf_groundtruth is the honest health of the SECOND audit stream. It
		// is driven by the most recent kernel.sensor.heartbeat: healthy only when
		// beats are fresh AND real kernel events have been observed, idle when the
		// sidecar is alive but blind (no events), degraded if the beat is stale,
		// unavailable if no sensor has ever beaten. The overclaim ("we have eBPF
		// ground truth") is structurally impossible — healthy reflects real events.
		"ebpf_groundtruth": s.ebpfGroundtruthStatus(r.Context()),
		// llm_egress_inspection advertises that the OPTIONAL outbound content-
		// inspection capability is built in. Whether a given run actually scans
		// (and in which mode) is per-run policy (RunPolicySpec.LLMInspection),
		// and per-decision coverage is reported on the egress decision/audit
		// stream (scanned / tunneled-opaque / llm.scan.blind), not here.
		"llm_egress_inspection": "available",
		// ssh discloses the gateway's presence + the two facts the run-detail
		// "Connect via SSH" pane needs to render its command/config block before
		// a human is authenticated (anonymous, like every other /healthz field):
		// advertise_addr (WARDYN_SSH_ADVERTISE, purely advisory copy) and the
		// host key's SHA256 fingerprint ("verify on first connect" — PUBLIC BY
		// DESIGN, it identifies the server, it authenticates no one; see
		// docs/SSH.md). nil (renders as JSON null) when the gateway is
		// disabled — the smallest honest wire change: no new endpoint, one
		// field a deployment without SSH simply omits populating.
		"ssh": s.sshGatewayHealthz(),
		// ui_sandbox discloses the UI-sandbox gateway's presence and the ONE
		// field the console needs to open a declared app: the enter-URL
		// template on the gateway's own origin (the console must never build
		// that origin itself — a different origin is the whole point). nil
		// (JSON null) when the gateway is off, the same "a deployment without
		// it simply omits the block" shape as ssh above.
		"ui_sandbox": s.uiSandboxHealthz(),
	})
}

// ebpfGroundtruthStatus reports the eBPF/Tetragon ground-truth stream's health
// from the latest kernel.sensor.heartbeat:
//
//	unavailable — no heartbeat ever (no sensor configured on this host)
//	degraded    — last heartbeat older than ebpfHeartbeatTTL (sensor stalled/dead)
//	idle        — heartbeat fresh but observed_total==0: the sidecar process is
//	              alive and reachable, yet has mapped ZERO kernel events (sensor
//	              blind, or the run is genuinely quiet) — NOT proof of ground truth
//	healthy     — heartbeat fresh AND real kernel events observed (ground truth flowing)
//
// A live heartbeat alone only proves the sidecar PROCESS is alive; "healthy"
// additionally requires observed kernel events, so the "we have eBPF ground
// truth" overclaim is structurally impossible. last_heartbeat is the RFC3339
// time of the most recent beat (omitted if none). dropped_total/observed_total/
// dropped_unmapped are the sensor-reported counts carried on the heartbeat's
// data (0 when absent); dropped_unmapped separates a blind sensor from a broken
// correlation — see the idle branch. When no Store is wired (tests), reports
// unavailable.
func (s *Server) ebpfGroundtruthStatus(ctx context.Context) map[string]any {
	out := map[string]any{"state": "unavailable", "dropped_total": uint64(0)}
	if s.cfg.Store == nil {
		return out
	}
	ev, err := s.cfg.Store.LatestAuditEventByAction(ctx, groundtruth.ActionSensorHeartbeat)
	if err != nil {
		// ErrNotFound (no sensor ever) or any query error: report unavailable.
		return out
	}
	out["last_heartbeat"] = ev.Time.UTC().Format(time.RFC3339)
	// dropped_total and observed_total are published by the sensor on the
	// heartbeat data when available; tolerate their absence.
	var hb struct {
		DroppedTotal    uint64            `json:"dropped_total"`
		ObservedTotal   uint64            `json:"observed_total"`
		DroppedUnmapped uint64            `json:"dropped_unmapped"`
		ObservedByKind  map[string]uint64 `json:"observed_by_kind"`
	}
	if len(ev.Data) > 0 {
		_ = json.Unmarshal(ev.Data, &hb)
	}
	out["dropped_total"] = hb.DroppedTotal
	out["observed_total"] = hb.ObservedTotal
	out["dropped_unmapped"] = hb.DroppedUnmapped
	if len(hb.ObservedByKind) > 0 {
		out["observed_by_kind"] = hb.ObservedByKind
	}
	switch {
	case s.cfg.Now().Sub(ev.Time) > ebpfHeartbeatTTL:
		// Heartbeat stale: the sensor process itself has stalled/died.
		out["state"] = "degraded"
	case hb.ObservedTotal == 0:
		// Process alive and beating, but it has mapped ZERO kernel events: the
		// sensor is blind (Tetragon dead / wrong export path / no TracingPolicy)
		// or the run is genuinely idle. Either way there is no ground truth yet,
		// so report "idle" with a reason rather than the "healthy" overclaim.
		//
		// The two idle causes are NOT the same failure and must not read the
		// same: dropped_unmapped>0 means the sensor saw kernel events and could
		// not bind ANY of them to a run (correlation broken — the 0.6 frozen-
		// counter defect), which no amount of waiting fixes.
		out["state"] = "idle"
		out["reason"] = "no kernel events observed"
		if hb.DroppedUnmapped > 0 {
			out["reason"] = fmt.Sprintf("kernel events observed but none correlated to a run (%d dropped as unmapped)", hb.DroppedUnmapped)
		}
	default:
		// W20-W20-groundtruth-mapper-4: "healthy" used to be one aggregate over
		// every kernel event kind — a sensor seeing only process.exec (a
		// mis-scoped TracingPolicy that never fires for network.connect or
		// file.write, say) reported healthy identically to one seeing all
		// three. When the sensor publishes the per-kind breakdown, require
		// EVERY known kind to have arrived at least once; report "partial"
		// (not the "healthy" overclaim) and name what's missing otherwise. An
		// older sensor build that has not upgraded to publish
		// observed_by_kind at all (empty map) cannot be assessed this way —
		// fall back to the aggregate-only "healthy" rather than downgrading
		// on missing DATA rather than a missing EVENT KIND.
		if missing := missingGroundtruthKinds(hb.ObservedByKind); len(hb.ObservedByKind) > 0 && len(missing) > 0 {
			out["state"] = "partial"
			out["missing_kinds"] = missing
		} else {
			out["state"] = "healthy"
		}
	}
	return out
}

// ebpfGroundtruthCaveat is the one-line, human-readable note a Record Mode
// capture (RecordTaskResult.Caveats, reconcileRecordRun) and a synthesized
// profile (profileResponse.Warnings, handleSynthesizeProfile) each stamp for
// every non-healthy sensor state — the SAME state ebpfGroundtruthStatus
// reports on /healthz, read through the one function so the two surfaces can
// never disagree about what "healthy" means (W20-W20-groundtruth-mapper-4:
// before this, the per-kind coverage state existed ONLY on the admin-only
// /healthz endpoint — nowhere an operator reviewing a capture or a
// synthesized profile would ever see it). "" (no caveat) when the sensor is
// fully healthy — there is nothing to warn about.
func (s *Server) ebpfGroundtruthCaveat(ctx context.Context) string {
	status := s.ebpfGroundtruthStatus(ctx)
	switch status["state"] {
	case "unavailable":
		return "kernel ground truth: unavailable — no eBPF sensor heartbeat was ever observed for this run; " +
			"proxy egress decisions are the only signal behind this capture"
	case "degraded":
		return "kernel ground truth: degraded — the eBPF sensor's heartbeat is stale; kernel-level coverage " +
			"for this capture may be incomplete"
	case "idle":
		return "kernel ground truth: idle — the eBPF sensor is alive but mapped zero kernel events; " +
			"this capture has no kernel-level corroboration"
	case "partial":
		missing, _ := status["missing_kinds"].([]string)
		return "kernel ground truth: partial — the eBPF sensor never observed " + strings.Join(missing, ", ") +
			"; this capture's kernel-level corroboration is incomplete"
	default: // "healthy", or absent (tests with no Store — same as unavailable, but there's no run to caveat)
		return ""
	}
}

// groundtruthKinds is the set of kernel event kinds required at least once
// before ebpfGroundtruthStatus reports "healthy" when the sensor publishes a
// per-kind breakdown at all — exec and connect only. kernel.file.write (the
// sensor's third mapped kind, cmd/wardyn-tetragon-ingest/main.go's
// processLine) is DELIBERATELY excluded: sensitive.go's own allowlist filter
// means it fires only on a write to a narrow set of credential-shaped paths
// (~/.ssh, ~/.aws, ...), so a normal capture that never happens to touch one
// is not a coverage gap — requiring it made "healthy" chronically unreachable
// and stamped a spurious "partial coverage" caveat on nearly every Record
// Mode capture (bug-audit-1). observed_by_kind still reports its count when
// the sensor does see one; it just never gates the health verdict.
var groundtruthKinds = []string{
	groundtruth.ActionProcessExec,
	groundtruth.ActionNetworkConnect,
}

// missingGroundtruthKinds returns the subset of groundtruthKinds absent or
// zero in observedByKind, in a stable order.
func missingGroundtruthKinds(observedByKind map[string]uint64) []string {
	var missing []string
	for _, k := range groundtruthKinds {
		if observedByKind[k] == 0 {
			missing = append(missing, k)
		}
	}
	return missing
}

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
