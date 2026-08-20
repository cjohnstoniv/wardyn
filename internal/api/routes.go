// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// routes builds the chi router: every mount point, middleware group, and the
// role/ownership gate each route sits behind. Split out of server.go (which
// owns Config/Server/New/New's small helpers) so the route TABLE — the thing
// most likely to be read/audited/extended — stays a single, self-contained
// file. See server.go's package doc comment for the route map summary and
// internal/api/authz_test.go's chi.Walk-enumerated matrix for the
// authoritative, always-current classification of every route below.
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
	r.Use(securityHeaders)

	r.Get("/healthz", s.handleHealthz)
	// Prometheus scrape surface. Admin-gated (NOT anonymous like /healthz): it
	// reports operational volumes, and the public API fails closed without a
	// credential — a scrape_config carries the admin token in an `authorization:`
	// header. Registered OUTSIDE /api/v1 with its own middleware chain, so it
	// needs the admin (requireOperator) gate explicitly — it is not swept up by
	// the operatorOnly group below. A member reading it would learn operational
	// volumes (run counts, approval decisions) about every OTHER user's runs.
	r.With(s.humanOrAdminAuth, s.requireOperator).Get("/metrics", s.handleMetrics)

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
			// operatorOnly is this same group with ONE extra middleware nested in
			// front (chi's With is what Group is built from): the ADMIN role gate.
			// Routes registered on it are authenticated exactly as before and then
			// refused with 403 for a signed-in MEMBER — see requireOperator, which
			// gates on the session's B1-derived Role (admin unless
			// WARDYN_OIDC_ROLE_MAP demotes it — see requireOperator's doc). The
			// tier is "member = read + launch/own runs": creating/killing/composing
			// a run, preflight and profile stay on r deliberately (a member may USE
			// the product and reach their OWN runs — see getRunAuthorized), while
			// configuring the deployment and touching credential MATERIAL are
			// admin-only acts. DECIDING an approval and minting a PTY ticket moved
			// DOWN to owner-or-admin (item 3): a member may act on a run/approval
			// they own, gated inside the handler instead of here. Every read stays
			// on r, so the gated routes are the ones written out below and nothing
			// else silently joins them.
			// MAINTENANCE HAZARD: With() SNAPSHOTS the group's middleware slice —
			// this line must stay immediately after the group's last r.Use, or a
			// later-added Use applies to r's routes but silently NOT to these.
			// COUNT (re-verify with `grep -c 'operatorOnly\.' routes.go` plus
			// mountLibraryRoutes' own 5, rather than trusting this comment — it
			// has gone stale before, W7-S1-1): 29 direct registrations below +
			// mountLibraryRoutes' 5 (sources.go — GET /base-images/{id} is gone,
			// DEADCODE-1) = 34. NOT the whole admin
			// surface: GET /metrics (outside /api/v1, its own explicit
			// requireOperator — commit "absorb the operator tier") and the attach
			// WebSocket's ticket-LESS fallback lane (ticketOrHumanAuth's own group
			// below, admin-only; the ticket-bearing lane is owner-or-admin) are
			// both gated too, via this SAME requireOperator, just not registered
			// on this operatorOnly value.
			operatorOnly := r.With(s.requireOperator)
			r.Post("/runs", s.handleCreateRun)
			// Dry-run of the create-run resolution + gating: same resolveRunPolicy
			// chokepoint (real 4xx errors), the enforced confinement class, and the
			// deterministic setup checklist — mints/persists/dispatches nothing. The
			// manual wizard fires it on the Review step (advisory, non-gating).
			r.Post("/runs/preflight", s.handlePreflightRun)
			r.Get("/runs", s.handleListRuns)
			r.Get("/runs/{id}", s.handleGetRun)
			r.Get("/runs/{id}/grants", s.handleListGrants)
			r.Post("/runs/{id}/kill", s.handleKillRun)
			// Recording Mode: synthesize a reusable least-privilege sandbox profile
			// from what this run actually did (advisory, read-only — mints nothing).
			r.Post("/runs/{id}/profile", s.handleSynthesizeProfile)

			// Live-run evidence reads for the run-detail cockpit. All three are
			// OWNER-OR-ADMIN via getRunAuthorized (a foreign run 404s — no
			// existence oracle), same gate as GET /runs/{id} above, and all three
			// are READ-ONLY observations of a sandbox that is already running:
			// they mint nothing, open no new network path (invariant 3), and
			// change no run state.
			r.Get("/runs/{id}/files", s.handleRunFiles)
			r.Get("/runs/{id}/resources", s.handleRunResources)
			r.Get("/runs/{id}/attach-holder", s.handleAttachHolder)
			// The one WRITE in that set: displacing whoever currently holds the
			// run's tmux PTY. Audited (session.takeover, actor + previous holder)
			// because it takes a live session away from another human.
			r.Post("/runs/{id}/attach/takeover", s.handleAttachTakeover)

			// Single-use WS attach tickets: browsers cannot put the admin
			// bearer on a WebSocket handshake, so the UI first POSTs here
			// (through THIS authenticated group) and presents the returned
			// 30s ticket as ?ticket= on the attach WS below.
			//
			// OWNER-OR-ADMIN (item 3, moved down from operator-only): the ticket
			// mints a live interactive PTY inside a RUNNING sandbox — injected
			// keystrokes and whatever the agent's injected credentials left on
			// screen — which is strictly more than "launch a run", but a member
			// may still hold one for a run THEY created (handleAttachTicket's
			// getRunAuthorized gate; a foreign run 404s, no existence oracle).
			r.Post("/runs/{id}/attach-ticket", s.handleAttachTicket)

			// Approvals: reading the queue is a member act (own runs only — see
			// handleListApprovals), DECIDING is OWNER-OR-ADMIN (item 3, moved down
			// from operator-only): the decision IS the live authorization over an
			// egress/credential escalation, but a member may decide one raised by
			// a run THEY own — decide() enforces it (a foreign approval 404s, no
			// existence oracle). The sandbox can only ever REQUEST one (machine
			// audience, /internal/approvals below), so this cannot starve an agent
			// of anything it could previously do for itself.
			r.Get("/approvals", s.handleListApprovals)
			r.Post("/approvals/{id}/approve", s.handleApproveApproval)
			r.Post("/approvals/{id}/deny", s.handleDenyApproval)

			r.Get("/audit", s.handleQueryAudit)
			r.Get("/me", s.handleMe)
			// Own effective capability set — member-safe (classMember): every
			// route AROUND this one on /permissions below is operator-only, but
			// a member reading only their OWN grants (ListCapabilityGrantsFor,
			// scoped to their own subjects) discloses nothing about anyone else.
			r.Get("/me/capabilities", s.handleMeCapabilities)
			// SSH gateway key registry (sshkeys.go): self-service, any authenticated
			// human — scoped to their OWN principal at the store, so this is
			// deliberately on r, not operatorOnly (see sshkeys.go's package doc).
			// NOTE for the B2 route-group split: kept as this one small, localized
			// block on purpose.
			r.Get("/me/ssh-keys", s.handleListSSHKeys)
			r.Post("/me/ssh-keys", s.handleAddSSHKey)
			r.Delete("/me/ssh-keys/{fingerprint}", s.handleDeleteSSHKey)
			// Run-detail widget layout: per-user, per-preset, server-synced so a
			// layout survives a new machine (localStorage would not). Scoped to
			// the caller's OWN principal at the store, exactly like the ssh-keys
			// block above — never a principal taken from the body.
			r.Get("/me/run-layout", s.handleGetRunLayout)
			r.Put("/me/run-layout", s.handlePutRunLayout)
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
			// RBAC (same as policy/workspace/site-config below): humanOrAdminAuth
			// is AUTHENTICATION only, so these are ALSO on operatorOnly — a
			// signed-in MEMBER (B1's derived role) gets 403 here. An ADMIN role —
			// the default for every signed-in human when WARDYN_OIDC_ROLE_MAP is
			// unset — can connect or disconnect the shared managed subscription
			// every run inherits. Every connect/disconnect is audited
			// (harness.credential.captured/disconnected).
			if s.cfg.Secrets != nil {
				operatorOnly.Post("/setup/harness-login", s.handleHarnessLogin)
				operatorOnly.Put("/setup/harness-credential/{provider}", s.handleHarnessCredentialPaste)
				operatorOnly.Delete("/setup/harness-credential/{provider}", s.handleHarnessDisconnect)
			}

			// Policy management (gated to authenticated humans — a valid SSO
			// session or the admin token). WRITES are additionally operator-only:
			// a signed-in MEMBER (B1's derived role) can read policies but not CRUD
			// them; an ADMIN can. Every spec is validated before it is persisted
			// (fail closed); writes are audited.
			operatorOnly.Post("/policies", s.handleCreatePolicy)
			r.Get("/policies", s.handleListPolicies)
			r.Get("/policies/{id}", s.handleGetPolicy)
			operatorOnly.Put("/policies/{id}", s.handleUpdatePolicy)
			operatorOnly.Delete("/policies/{id}", s.handleDeletePolicy)

			// Workspace management (onboarding of local dirs + repos a run may
			// attach), gated to authenticated humans (SSO session or admin token);
			// every MUTATING route here is additionally operator-only, so a
			// signed-in MEMBER (B1's derived role) can list/read workspaces but
			// cannot CRUD them or widen what a run may do; an ADMIN can.
			// Create/update validate the source the
			// same way policy WorkspaceMounts do (runner.ValidateMount /
			// ValidateTarget) or the way AgentRun.Repo does (repoFieldSafe +
			// repoCloneURL); writes are audited. Scan is a separate endpoint
			// (workspaces.go handleScanWorkspace) that runs the deterministic
			// workspacescan and persists the profile + status.
			// Tier-1 source library + tier-2 base-image catalog routes —
			// mounted from sources.go, same posture as the workspaces block.
			s.mountLibraryRoutes(r, operatorOnly)

			operatorOnly.Post("/workspaces", s.handleCreateWorkspace)
			r.Get("/workspaces", s.handleListWorkspaces)
			r.Get("/workspaces/{id}", s.handleGetWorkspace)
			operatorOnly.Put("/workspaces/{id}", s.handleUpdateWorkspace)
			operatorOnly.Delete("/workspaces/{id}", s.handleDeleteWorkspace)
			operatorOnly.Post("/workspaces/{id}/scan", s.handleScanWorkspace)
			// Workspace image build (the wizard's Build step): status is a
			// member-tier read like the other workspace GETs; kicking a build
			// is an operator action (workspace_build.go).
			r.Get("/workspaces/{id}/build", s.handleGetWorkspaceBuild)
			operatorOnly.Post("/workspaces/{id}/build", s.handleBuildWorkspace)
			// Operator-owned egress approvals (promotion of the scanner's
			// content-derived suggestions; see handleSetApprovedEgress).
			operatorOnly.Put("/workspaces/{id}/approved-egress", s.handleSetApprovedEgress)
			// Denied-egress twin (Phase 4 revocation) — the only way to undo a
			// `deny · always` decision, including curing an already-bricked
			// workspace (H5); see handleSetDeniedEgress's doc comment for why its
			// validator is deliberately narrower than the approved-egress one above.
			operatorOnly.Put("/workspaces/{id}/denied-egress", s.handleSetDeniedEgress)
			// Bind (or clear) the workspace/container's model/harness creds — a run
			// that picks it inherits them (applyWorkspaceCreds). Scoped write.
			operatorOnly.Put("/workspaces/{id}/llm-cred", s.handleSetWorkspaceLLMCred)
			// Requirements contract (secrets/egress/write-access a run against
			// this workspace needs); folded into a run's resolved policy by
			// runs_create.go's applyWorkspaceRequirements. Scoped write.
			operatorOnly.Put("/workspaces/{id}/requirements", s.handleSetWorkspaceRequirements)
			// Least-privilege telemetry: egress hosts runs using this workspace
			// were denied — promotion candidates (see handleObservedEgress).
			r.Get("/workspaces/{id}/observed-egress", s.handleObservedEgress)
			// Record Mode: launch one task's OPEN recording sandbox (learn what
			// the task actually uses; see handleRecordWorkspace), then promote
			// the observed-allowed hosts into ApprovedEgress (operator one-click).
			operatorOnly.Post("/workspaces/{id}/record", s.handleRecordWorkspace)
			operatorOnly.Post("/workspaces/{id}/record/{task}/promote-egress", s.handlePromoteRecordEgress)
			// Committable env-as-code (devcontainer.json/AGENTS.md) from the
			// scanned profile. GET re-generates it any time (repo workspaces have
			// no host path to write into); the write route is local-dir-only and
			// writes it into the host source dir (see handleWriteEnvAsCode).
			r.Get("/workspaces/{id}/env-as-code", s.handleGetEnvAsCode)
			operatorOnly.Post("/workspaces/{id}/env-as-code/write", s.handleWriteEnvAsCode)

			// Secret management: write/delete/list only. Values are NEVER
			// readable through the API (read paths are the broker and the
			// internal injection-resolve endpoint, both audited).
			//
			// The writes are operator-only: this is credential MATERIAL, a
			// strictly larger blast radius than site-config (which only names a
			// secret *ref*). The LIST stays viewer-readable — it returns names
			// only, never values.
			if s.cfg.Secrets != nil {
				operatorOnly.Put("/secrets/{name}", s.handlePutSecret)
				operatorOnly.Delete("/secrets/{name}", s.handleDeleteSecret)
				r.Get("/secrets", s.handleListSecrets)
			}

			// Site config: the operator-wide, admin-authored baseline every run
			// inherits (upstream proxy secret ref, per-ecosystem artifact-registry
			// overrides, default SCM hosts). GET/PUT only — there is exactly one
			// config row; every write is validated (SSRF/injection hardening on
			// the URL/host fields) and audited (site_config.write).
			//
			// RBAC (same as policy/workspace above): humanOrAdminAuth is
			// AUTHENTICATION only, so the PUT is operator-only — a signed-in
			// MEMBER (B1's derived role) can read the config but not rewrite it.
			// This is where that matters most: the blast radius is corp-wide (the
			// baseline feeds every run's upstream proxy / artifact mirror / SCM
			// hosts), higher than a single policy.
			r.Get("/site-config", s.handleGetSiteConfig)
			operatorOnly.Put("/site-config", s.handlePutSiteConfig)
			// Live connectivity probes: launch a throwaway one-shot sandbox and
			// actually traverse the upstream proxy / egress redirect, rather than
			// a "we wrote it down" test-connection button (site_config_probe.go).
			// operatorOnly — same posture as the PUT above.
			operatorOnly.Post("/site-config/test-proxy", s.handleTestSiteConfigProxy)
			operatorOnly.Post("/site-config/test-redirect", s.handleTestSiteConfigRedirect)

			// Effective integration set (stored ∪ legacy-derived) with live
			// capabilities — see internal/api/integrations.go /
			// setup_integrations.go. Read-only, same RBAC posture as
			// site-config's GET: Credentials only ever holds secret NAMES.
			r.Get("/integrations", s.handleListIntegrations)
			// Integration writes: PUT creates-or-replaces a stored row, DELETE
			// removes one. operatorOnly — same corp-wide blast radius as
			// site-config's PUT (setup_integrations.go).
			//
			// The 0.5 console has no integration catalog: connections are the
			// four Settings cards (Host, Model provider, Git host, Your SSH
			// keys), each a radio group over concrete lanes. Two routes went
			// with the catalog they existed for. `.../adopt` promoted a DERIVED
			// legacy row into a stored one so it could be edited in the list —
			// there is no list, and nothing derived to promote. `.../test`
			// spent a throwaway confined sandbox to probe one row's egress +
			// injection; the cards state what is STORED and say so plainly
			// ("Wardyn stores this — it doesn't dial the provider to check
			// it"), which is the honest claim for a credential nobody has used
			// yet. A real run remains the real test.
			operatorOnly.Put("/integrations/{id}", s.handlePutIntegration)
			operatorOnly.Delete("/integrations/{id}", s.handleDeleteIntegration)

			// Permissioning (0.6 pillar 2, migration 0042): which of the powers a
			// MEMBER already has may they actually use. GET returns the whole
			// grant table + enforcement map in one call (the admin Permissions
			// screen's entire data need); the writes are operator-only, same
			// posture as site-config/secrets above — this bounds every member's
			// blast radius, not a single run's. The member-safe read of a
			// caller's OWN effective set is GET /me/capabilities above, not here.
			operatorOnly.Get("/permissions", s.handleGetPermissions)
			operatorOnly.Post("/permissions/grants", s.handleUpsertCapabilityGrant)
			operatorOnly.Delete("/permissions/grants/{id}", s.handleDeleteCapabilityGrant)
			operatorOnly.Put("/permissions/enforcement", s.handlePutCapabilityEnforcement)

			// Recording replay: GET /api/v1/runs/{id}/recording/{id}. Owner-or-admin
			// (item 4): recordingAuthorizer is the SAME ownership rule
			// getRunAuthorized enforces, applied INSIDE the handler (the outer {id}
			// here only selects a chi sub-route, so the authorization decision
			// belongs to the mounted handler, not this registration).
			if s.cfg.RecordingStore != nil {
				r.Mount("/runs/{id}/recording", recording.Handler(s.cfg.RecordingStore, s.recordingAuthorizer))
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

			// SSO-token upload: PUT /api/v1/internal/sso-token/{runID}
			// wardyn-aws-sso PUTs the captured AWS SSO token cache from inside the
			// aws-sso container-login run (via the proxy's brokered sso-token
			// route, which injects the run token). Same cross-run guard as scan;
			// the run-kind check is harnessLoginTask + awsSSOAgent instead of a
			// governed workspace run.
			if s.cfg.Secrets != nil {
				r.Put("/internal/sso-token/{runID}", s.handleUploadSSOToken)
			}
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
