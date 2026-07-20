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
//	  GET  /api/v1/approvals?state= ; POST /api/v1/approvals/{id}/approve|deny
//	  GET  /api/v1/audit?run_id=
//	  POST /api/v1/policies ; GET /api/v1/policies ; GET /api/v1/policies/{id}
//	  PUT  /api/v1/policies/{id} ; DELETE /api/v1/policies/{id}
//	  GET  /healthz
//	Internal (run-token bearer, identity.Provider.Verify aud="wardyn-internal"):
//	  POST /api/v1/internal/decisions
//	  POST /api/v1/internal/approvals ; GET /api/v1/internal/approvals/{id}
//	  POST /api/v1/internal/credentials/mint
//	Ground-truth (host-sensor bearer, identity.Provider.Verify aud="wardyn-groundtruth"):
//	  POST /api/v1/internal/groundtruth   (eBPF/Tetragon kernel-event batch)
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
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
	Decide(ctx context.Context, id uuid.UUID, approve bool, decidedByType types.ActorType, decidedBy, reason string) (types.ApprovalRequest, error)
	Get(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error)
	List(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error)
}

// MintBroker is the credential-mint surface the API depends on (internal/broker).
type MintBroker interface {
	MintForGrant(ctx context.Context, caller *identity.Claims, grantID uuid.UUID) (broker.Minted, error)
	RevokeRun(ctx context.Context, runID uuid.UUID) error
}

// ImageBuilder builds a per-run sandbox image from a devcontainer repo. It is
// target-agnostic (the parity rule): the concrete envbuilder implementation is
// wired in wardynd behind the "docker" build tag, so the control-plane default
// build carries zero target-specific code. Nil disables devcontainer builds.
type ImageBuilder interface {
	// BuildDevcontainer builds the devcontainer for repoURL@ref and returns the
	// local image reference to run. outputTag is the deterministic per-run tag
	// the result is committed under.
	BuildDevcontainer(ctx context.Context, repoURL, ref, outputTag string) (imageRef string, err error)
	// BuildFromDevcontainerFiles builds an image from IN-MEMORY generated
	// devcontainer files (relative path -> content, e.g.
	// ".devcontainer/devcontainer.json") rather than a repo checkout, returning
	// the local image reference. It drives the SAME hardened envbuilder path as
	// BuildDevcontainer. Used for an onboarded workspace WITHOUT a wired
	// devcontainer, where core A generates a minimal one from the detected
	// profile (plan A5). outputTag is the deterministic profile-hash-keyed tag
	// the result is committed under.
	BuildFromDevcontainerFiles(ctx context.Context, files map[string]string, outputTag string) (imageRef string, err error)
	// FinalizeBase wraps an arbitrary USER-supplied base image (Bring Your Own
	// Image) with Wardyn's runner tools + a cleared ENTRYPOINT, returning the
	// runnable local image reference. No untrusted build, no registry push — just
	// the trusted FROM+COPY finalize stage; the base is pulled only if absent, so
	// a host-pre-pulled private image works. outputTag is the per-run tag.
	FinalizeBase(ctx context.Context, baseRef, outputTag string) (imageRef string, err error)
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
	// Empty defaults to BedrockRegion. Only meaningful with BedrockAWSConfigDir.
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
	// Composer, when set and Enabled(), powers the AI Run Composer endpoints
	// (POST /api/v1/runs/compose, GET /api/v1/composer/backends): a registry of
	// LLM backends turns a natural-language task description into a PROPOSED
	// {run, inline_policy} that Wardyn risk-grades deterministically and clamps to
	// DefaultPolicy before returning for human approval. Nil / not-Enabled
	// disables the endpoints (404), so the feature is strictly opt-in.
	Composer *composer.Registry
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
	// ComposerBackends is the BOOT-snapshot readiness of every configured composer
	// backend (including disabled + needs-key ones the live registry can't show).
	// Surfaced by /setup/status. Nil when the composer is unconfigured.
	ComposerBackends []ComposerBackendReadiness
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
}

// ComponentInfo describes one pluggable seam's selection for /healthz. Selected
// is ALWAYS the actual running implementation; RecommendedProduction is the
// standard Wardyn recommends converging to and may differ from Selected (and even
// from the shipped default — the honest recommended-vs-shipped split documented
// in docs/PLUGGABILITY.md). Source is "default" or "configured".
type ComponentInfo struct {
	Selected string `json:"selected"`
	// Available lists every implementation self-registered in this build's seam
	// registry (so /healthz truthfully shows what THIS binary can run — e.g. a
	// tagless build advertises sandbox.available=[]). Empty for seams without a
	// registry (policy_engine today).
	Available             []string `json:"available,omitempty"`
	RecommendedProduction string   `json:"recommended_production,omitempty"`
	Source                string   `json:"source,omitempty"`
}

// Server is the control-plane HTTP server. It is safe for concurrent use.
type Server struct {
	cfg    Config
	router chi.Router
	// attachTix holds outstanding single-use WS attach tickets (see
	// attach_ticket.go). Zero value is ready to use.
	attachTix attachTickets
	// composeResults holds in-flight compose-run proposal uploads keyed by run id
	// (see composeresult.go). Zero value is ready to use.
	composeResults composeResultStore
}

// New constructs a Server and builds its router. It does not start listening.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RunnerTarget == "" {
		cfg.RunnerTarget = "docker"
	}
	if cfg.BaseCtx == nil {
		cfg.BaseCtx = context.Background()
	}
	s := &Server{cfg: cfg}
	s.router = s.routes()
	// Late-bind the sandbox composer backend's run launcher. The composer registry
	// (and its backends) are built at boot BEFORE the Server exists, but the
	// sandbox backend runs its claude wire INSIDE a governed run this Server
	// launches — so it needs the Server's RunClaudeCompose. Wire it now that s
	// exists (mirrors how a func on cfg is set at construction). No-op unless a
	// sandbox backend is configured.
	if cfg.Composer != nil {
		for _, c := range cfg.Composer.Composers() {
			if sink, ok := c.(composeRunnerSink); ok {
				sink.SetRunClaude(s.RunClaudeCompose)
			}
		}
	}
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

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// SECURITY: do NOT install middleware.RealIP. It overwrites r.RemoteAddr from
	// the client-supplied X-Forwarded-For / X-Real-IP headers with no
	// trusted-proxy allowlist, and r.RemoteAddr is persisted as the append-only
	// audit source_ip (handlePostDecision / handleGroundtruthEvents). Trusting
	// those headers would let any caller reaching the internal/groundtruth
	// endpoints FORGE the source_ip in the audit log. We keep r.RemoteAddr as the
	// real TCP peer instead. If Wardyn is ever fronted by a trusted reverse proxy,
	// reintroduce X-Forwarded-For parsing ONLY behind an explicit allowlist of
	// trusted proxy addresses.
	r.Use(middleware.Recoverer)

	r.Get("/healthz", s.handleHealthz)

	// Human SSO (OIDC): login/callback/logout. Mounted only when configured.
	// These are unauthenticated by design (they bootstrap the session).
	if s.cfg.OIDC != nil {
		r.Get("/auth/login", s.cfg.OIDC.LoginHandler)
		r.Get("/auth/callback", s.cfg.OIDC.CallbackHandler)
		r.Get("/auth/logout", s.cfg.OIDC.LogoutHandler)
	}

	r.Route("/api/v1", func(r chi.Router) {
		// Public admin-gated surface.
		r.Group(func(r chi.Router) {
			r.Use(s.humanOrAdminAuth)
			r.Post("/runs", s.handleCreateRun)
			// Dry-run of the create-run resolution + gating: same resolveRunPolicy
			// chokepoint (real 4xx errors), the enforced confinement class, and the
			// deterministic setup checklist — mints/persists/dispatches nothing. The
			// manual wizard fires it on the Review step (advisory, non-gating).
			r.Post("/runs/preflight", s.handlePreflightRun)
			r.Post("/runs/compose", s.handleComposeRun)
			// Escalation-only Ask help agent + client funnel beacon (advisory only;
			// same composer-enabled gate + hardened backend transport as compose).
			r.Post("/runs/compose/assist", s.handleComposeAssist)
			r.Post("/runs/compose/telemetry", s.handleComposeTelemetry)
			r.Get("/composer/backends", s.handleListComposerBackends)
			r.Get("/runs", s.handleListRuns)
			r.Get("/runs/{id}", s.handleGetRun)
			r.Get("/runs/{id}/grants", s.handleListGrants)
			r.Post("/runs/{id}/kill", s.handleKillRun)
			// Recording Mode: synthesize a reusable least-privilege sandbox profile
			// from what this run actually did (advisory, read-only — mints nothing).
			r.Post("/runs/{id}/profile", s.handleSynthesizeProfile)

			// Single-use WS attach tickets: browsers cannot put the admin
			// bearer on a WebSocket handshake, so the UI first POSTs here
			// (through THIS authenticated group) and presents the returned
			// 30s ticket as ?ticket= on the attach WS below.
			r.Post("/runs/{id}/attach-ticket", s.handleAttachTicket)

			r.Get("/approvals", s.handleListApprovals)
			r.Post("/approvals/{id}/approve", s.handleApproveApproval)
			r.Post("/approvals/{id}/deny", s.handleDenyApproval)

			r.Get("/audit", s.handleQueryAudit)
			r.Get("/me", s.handleMe)
			// FIX #6: sign-out. The UI POSTs /api/v1/auth/logout, but the OIDC
			// logout was mounted ONLY as a root GET /auth/logout, so the POST hit
			// no route (404), the HttpOnly session cookie survived, and the next
			// probe silently re-signed the operator in. Mount the POST here so the
			// client's existing call actually terminates the session. Nil-OIDC
			// (local/token mode) is a safe no-op — see handleLogout.
			r.Post("/auth/logout", s.handleLogout)
			// First-run setup readiness. MUST stay in this humanOrAdminAuth group
			// (anonymous non-local => 401): it enumerates providers/keys/CLIs
			// (capability disclosure) and must never sit on the public /healthz.
			r.Get("/setup/status", s.handleSetupStatus)

			// Managed harness login: launch an interactive login sandbox where
			// the operator runs `claude setup-token`, then paste the resulting
			// long-lived subscription token so Wardyn injects it proxy-side into
			// every run (compose-mode subscription without a host ~/.claude).
			// Secret store required (the token is stored age-encrypted).
			//
			// RBAC caveat (same as policy/workspace below): these sit in the
			// humanOrAdminAuth group, which is AUTHENTICATION only — dedicated
			// admin-role gating is planned, so today ANY authenticated human in
			// OIDC mode (not just an admin) can connect/disconnect the shared
			// managed subscription every run inherits. Every connect/disconnect is
			// audited (harness.credential.captured/disconnected).
			if s.cfg.Secrets != nil {
				r.Post("/setup/harness-login", s.handleHarnessLogin)
				r.Put("/setup/harness-credential/{provider}", s.handleHarnessCredentialPaste)
				r.Delete("/setup/harness-credential/{provider}", s.handleHarnessDisconnect)
			}

			// Policy management (gated to authenticated humans — a valid SSO
			// session or the admin token; dedicated admin-role gating is
			// planned, so today ANY authenticated human in OIDC mode can CRUD
			// policies, not just admins). Every spec is validated before it is
			// persisted (fail closed); writes are audited.
			r.Post("/policies", s.handleCreatePolicy)
			r.Get("/policies", s.handleListPolicies)
			r.Get("/policies/{id}", s.handleGetPolicy)
			r.Put("/policies/{id}", s.handleUpdatePolicy)
			r.Delete("/policies/{id}", s.handleDeletePolicy)

			// Workspace management (onboarding of local dirs + repos a run may
			// attach — plan core B1), gated to authenticated humans (SSO session
			// or admin token); dedicated admin-role gating is planned, so today
			// ANY authenticated human in OIDC mode can CRUD workspaces, not just
			// admins. Create/update validate the source the
			// same way policy WorkspaceMounts do (runner.ValidateMount /
			// ValidateTarget) or the way AgentRun.Repo does (repoFieldSafe +
			// repoCloneURL); writes are audited. Scan is a separate, currently-stub
			// endpoint (see workspaces.go handleScanWorkspace).
			r.Post("/workspaces", s.handleCreateWorkspace)
			r.Get("/workspaces", s.handleListWorkspaces)
			r.Get("/workspaces/{id}", s.handleGetWorkspace)
			r.Put("/workspaces/{id}", s.handleUpdateWorkspace)
			r.Delete("/workspaces/{id}", s.handleDeleteWorkspace)
			r.Post("/workspaces/{id}/scan", s.handleScanWorkspace)
			// Operator-owned egress approvals (promotion of the scanner's
			// content-derived suggestions; see handleSetApprovedEgress).
			r.Put("/workspaces/{id}/approved-egress", s.handleSetApprovedEgress)
			// Bind (or clear) the workspace/container's model/harness creds — a run
			// that picks it inherits them (applyWorkspaceCreds). Scoped write.
			r.Put("/workspaces/{id}/llm-cred", s.handleSetWorkspaceLLMCred)
			// Least-privilege telemetry: egress hosts runs using this workspace
			// were denied — promotion candidates (see handleObservedEgress).
			r.Get("/workspaces/{id}/observed-egress", s.handleObservedEgress)
			// Operator-approved setup commands the verify run executes
			// (promoted from the scanner's advisory profile.setup_commands).
			r.Put("/workspaces/{id}/setup-commands", s.handleSetSetupCommands)
			// Launch a governed verify run: execute the approved setup commands
			// in the built image under confinement (see handleVerifyWorkspace).
			r.Post("/workspaces/{id}/verify", s.handleVerifyWorkspace)
			// Record Mode: launch one task's OPEN recording sandbox (learn what
			// the task actually uses; see handleRecordWorkspace), then promote
			// the observed-allowed hosts into ApprovedEgress (operator one-click).
			r.Post("/workspaces/{id}/record", s.handleRecordWorkspace)
			r.Post("/workspaces/{id}/record/{task}/promote-egress", s.handlePromoteRecordEgress)
			// Finalize the import: mark ready + optionally emit committable
			// env-as-code (devcontainer.json/AGENTS.md).
			r.Post("/workspaces/{id}/finalize", s.handleFinalizeWorkspace)
			// Agentic verify-fix: ask a compose backend to diagnose a failed
			// verify and suggest a concrete fix (advisory; see handleSuggestVerifyFix).
			r.Post("/workspaces/{id}/verify/suggest-fix", s.handleSuggestVerifyFix)

			// Secret management: write/delete/list only. Values are NEVER
			// readable through the API (read paths are the broker and the
			// internal injection-resolve endpoint, both audited).
			if s.cfg.Secrets != nil {
				r.Put("/secrets/{name}", s.handlePutSecret)
				r.Delete("/secrets/{name}", s.handleDeleteSecret)
				r.Get("/secrets", s.handleListSecrets)
			}

			// Site config: the operator-wide, admin-authored baseline every run
			// inherits (upstream proxy secret ref, per-ecosystem artifact-registry
			// overrides, default SCM hosts). GET/PUT only — there is exactly one
			// config row; every write is validated (SSRF/injection hardening on
			// the URL/host fields) and audited (site_config.write).
			//
			// RBAC caveat (same as policy/workspace above): this is in the
			// humanOrAdminAuth group — AUTHENTICATION only, no admin-role gate yet
			// (planned), so today ANY authenticated human in OIDC mode can CRUD it,
			// not just admins. Blast radius is corp-wide (this baseline feeds every
			// run's upstream proxy / artifact mirror / SCM hosts) — arguably higher
			// than a single policy — so the missing role gate matters most here.
			r.Get("/site-config", s.handleGetSiteConfig)
			r.Put("/site-config", s.handlePutSiteConfig)

			// Recording replay: GET /api/v1/runs/{id}/recording/{id}
			if s.cfg.RecordingStore != nil {
				r.Mount("/runs/{id}/recording", recording.Handler(s.cfg.RecordingStore))
			}
		})

		// Interactive attach (WebSocket). Its own group: browsers cannot put
		// the admin bearer on a WS handshake, so this route ALSO accepts a
		// single-use ?ticket= minted via POST /runs/{id}/attach-ticket above
		// (ticketOrHumanAuth falls through to humanOrAdminAuth when no ticket
		// is presented — OIDC-cookie and CLI bearer attach are unchanged). The
		// handler upgrades to a WebSocket and relays a live PTY from a RUNNING
		// sandbox. The interactive shell is bounded by the same L0 egress +
		// confinement envelope as the agent (invariant 3) and the principal —
		// the ticket's MINTER for ticket auth — is recorded for attribution
		// (invariant 4).
		r.Group(func(r chi.Router) {
			r.Use(s.ticketOrHumanAuth)
			r.Get("/runs/{id}/attach", s.handleAttachWS)
		})

		// Internal sidecar surface (run-token bearer).
		r.Group(func(r chi.Router) {
			r.Use(s.internalAuth)
			r.Post("/internal/decisions", s.handlePostDecision)
			r.Post("/internal/approvals", s.handleInternalRequestApproval)
			r.Get("/internal/approvals/{id}", s.handleInternalGetApproval)
			r.Post("/internal/credentials/mint", s.handleInternalMint)

			// Token renew: POST /api/v1/internal/token/renew
			// The per-run proxy re-issues its own (short-TTL) run token before it
			// lapses, authenticated by the CURRENT token. Without this producer a
			// run outliving the 1h TTL loses every /internal/* call. NOT forwarded
			// by any brokered local route — the sandbox cannot reach it.
			r.Post("/internal/token/renew", s.handleInternalTokenRenew)

			// Injection resolve: returns the FORMATTED SECRET VALUE for an
			// api_key grant. SECURITY: this path must NEVER be forwarded by a
			// wardyn-proxy brokered local route — the proxy calls it directly
			// at startup; the sandbox has no network path to it (the brokered
			// routes forward only mint/approvals/recordings, by construction).
			if s.cfg.Secrets != nil {
				r.Get("/internal/injection/{grantID}", s.handleInternalInjection)
			}

			// Recording upload: PUT /api/v1/internal/recordings/{runID}
			// wardyn-rec POSTs the finished cast from inside the agent container.
			if s.cfg.RecordingStore != nil {
				r.Put("/internal/recordings/{runID}", s.handleUploadRecording)
			}

			// Scan-result upload: PUT /api/v1/internal/scan-results/{runID}
			// wardyn-scan PUTs the workspace ScanFacts from inside a governed scan
			// run (via the proxy's brokered scan-result route, which injects the
			// run token). Cross-run uploads are rejected (token run id must match
			// the path run id).
			r.Put("/internal/scan-results/{runID}", s.handleUploadScanResult)

			// Compose-result upload: PUT /api/v1/internal/compose-results/{runID}
			// The in-sandbox claude compose wire PUTs its raw proposal JSON from a
			// governed compose run (via the proxy's brokered compose-result route,
			// which injects the run token) for the waiting RunClaudeCompose to read.
			// Same cross-run guard as scan (token run id must match the path run id).
			r.Put("/internal/compose-results/{runID}", s.handleUploadComposeResult)

			// Verify-result upload: PUT /api/v1/internal/verify-results/{runID}
			// wardyn-verify PUTs the VerifyResult (per-step exit codes + bounded
			// logs) from inside a governed verify run via the proxy's brokered
			// verify-result route. Same cross-run guard + trusted linkage as scan.
			r.Put("/internal/verify-results/{runID}", s.handleUploadVerifyResult)
		})

		// Ground-truth ingest surface (host-sensor bearer, aud=wardyn-groundtruth).
		// SEPARATE auth group from the run-token internal surface above: the
		// host eBPF sensor's token is audit-write-only and is rejected by the
		// mint/approval endpoints. This is the SECOND of the three audit streams
		// (Postgres self-report + PTY replay are the others).
		r.Group(func(r chi.Router) {
			r.Use(s.internalAuthGroundtruth)
			r.Post("/internal/groundtruth", s.handleGroundtruthEvents)
		})
	})

	s.mountUI(r)
	return r
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
		"status":            "ok",
		"identity_provider": idp,
		"trust_domain":      s.cfg.TrustDomain,
		"runner":            runnerName,
		// confinement_classes are the enforceable isolation LEVELS; the
		// confinement_substrates map names WHICH runtime backs each (e.g.
		// "CC3":"oci/kata-qemu") — honest visibility into the pluggable substrate.
		"confinement_classes":    caps,
		"confinement_substrates": substrates,
		// components reports the SELECTED pluggable-component impl per seam plus
		// the recommended production default. selected is always the actual
		// running impl; recommended_production may differ (honest advertisement).
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
// time of the most recent beat (omitted if none). dropped_total/observed_total
// are the sensor-reported counts carried on the heartbeat's data (0 when
// absent). When no Store is wired (tests), reports unavailable.
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
	out["last_heartbeat"] = ev.Time.UTC().Format(rfc3339)
	// dropped_total and observed_total are published by the sensor on the
	// heartbeat data when available; tolerate their absence.
	var hb struct {
		DroppedTotal  uint64 `json:"dropped_total"`
		ObservedTotal uint64 `json:"observed_total"`
	}
	if len(ev.Data) > 0 {
		_ = json.Unmarshal(ev.Data, &hb)
	}
	out["dropped_total"] = hb.DroppedTotal
	out["observed_total"] = hb.ObservedTotal
	switch {
	case s.cfg.Now().Sub(ev.Time) > ebpfHeartbeatTTL:
		// Heartbeat stale: the sensor process itself has stalled/died.
		out["state"] = "degraded"
	case hb.ObservedTotal == 0:
		// Process alive and beating, but it has mapped ZERO kernel events: the
		// sensor is blind (Tetragon dead / wrong export path / no TracingPolicy)
		// or the run is genuinely idle. Either way there is no ground truth yet,
		// so report "idle" with a reason rather than the "healthy" overclaim.
		out["state"] = "idle"
		out["reason"] = "no kernel events observed"
	default:
		// Beating within the TTL AND real kernel events have been observed.
		out["state"] = "healthy"
	}
	return out
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
