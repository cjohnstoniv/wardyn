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
// file. See internal/api/authz_test.go's chi.Walk-enumerated matrix for the
// authoritative, always-current classification of every route below.
func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// Security: do NOT install middleware.RealIP. It overwrites r.RemoteAddr from
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
	// Readiness: proves Postgres is reachable, not just that the process is up
	// (see handleReadyz's doc comment). Deliberately a separate endpoint from
	// /healthz, which liveness/startup keep using.
	r.Get("/readyz", s.handleReadyz)
	// Prometheus scrape surface. Admin-gated (NOT anonymous like /healthz): it
	// reports operational volumes, and the public API fails closed without a
	// credential — a scrape_config carries the admin token in an `authorization:`
	// header. Registered OUTSIDE /api/v1 with its own middleware chain, so it
	// needs the admin (requireOperator) gate explicitly — it is not swept up by
	// the operatorOnly group below. A member reading it would learn operational
	// volumes (run counts, approval decisions) about every OTHER user's runs.
	r.With(s.humanOrAdminAuth, s.requireOperator).Get("/metrics", s.handleMetrics)

	// Human SSO (OIDC): login/callback. Mounted only when configured. These are
	// unauthenticated by design (they bootstrap the session). Sign-OUT is not
	// here: it is POST /api/v1/auth/logout below, inside humanOrAdminAuth.
	if s.cfg.OIDC != nil {
		r.Get("/auth/login", s.cfg.OIDC.LoginHandler)
		r.Get("/auth/callback", s.cfg.OIDC.CallbackHandler)
	}

	r.Route("/api/v1", func(r chi.Router) {
		// Public admin-gated surface.
		r.Group(func(r chi.Router) {
			r.Use(s.humanOrAdminAuth)
			// operatorOnly and securityOps are the two gate values over this one
			// authenticated group (chi's With is what Group is built from):
			// operatorOnly is the super-admin role gate (requireOperator, which reads
			// the session's derived Role), securityOps admits admin OR
			// security_admin. A route registered on either is authenticated exactly
			// as before and then refused with 403 for a signed-in member. Every route
			// not named on one of them stays member-reachable, which is the tier
			// line: a member may USE the product and reach their OWN runs
			// (getRunAuthorized), while configuring the deployment and touching
			// credential MATERIAL are admin acts.
			//
			// Maintenance hazard: With() SNAPSHOTS the group's middleware slice —
			// these two lines must stay immediately after the group's last r.Use, or
			// a later-added Use applies to r's routes but silently NOT to them.
			//
			// Where the gated routes are registered. This is a map, not a census:
			// which addend a route lives in is the thing no test can tell you, and it
			// is stable. The counts are not, and they are deliberately absent — a
			// number restated here is a second account of something authz_test.go's
			// chi.Walk matrix already asserts, and the two drift silently. A
			// same-file `grep -c 'operatorOnly\.'` is not the count either: it misses
			// every addend registered through a differently-named chi.Router
			// parameter or living in another file. Re-derive by hand, one addend at a
			// time, and re-derive BOTH columns — a route moved between the two groups
			// leaves the total unchanged and only the split wrong, which no total can
			// catch.
			//
			//   direct registrations in this body   (both groups)
			//   mountPermissionRoutes  (this file)  securityOps
			//   mountAccountRoutes     (this file)  securityOps
			//   adminRoutes            (this file)  one per group
			//   mountLibraryRoutes     (sources.go) operatorOnly (+ member reads on r)
			//   mountSetupMutationRoutes            operatorOnly
			//   mountAccessRoutes      (access.go)  operatorOnly
			//   mountGovernanceRoutes  (governance.go) — CALLED WITH securityOps,
			//       despite naming its parameter operatorOnly; read the call site
			//   mountUserDriveFamily   (user_drives_reclaim.go) operatorOnly —
			//       wraps mountUserDriveRoutes (user_drives.go) + the destroy verb
			//   mountWorkspaceProviderRoutes        operatorOnly
			//       (workspace_providers.go)
			//   mountAgentProviderRoutes            operatorOnly
			//       (agent_providers.go)
			//   mountSiteConfigProbeRoutes          securityOps
			//       (site_config_probe.go)
			operatorOnly := r.With(s.requireOperator)
			// securityOps is the second admin tier: admin OR security_admin, via
			// requireSecurityOperator / isSecurityOperator (http.go). What the tier
			// MEANS — the line every assignment above is measured against — is
			// authority over the verdict and over the org's ceilings, and never reach
			// INTO a run, never credential material, never the host: decide/see any
			// approval, verify the audit chain, hold the org allow/denylist, revoke a
			// human's session or another human's API token, promote a workspace's
			// observed egress, author governance profiles.
			//
			// "a human's" includes a super admin's, and the revoke route's
			// {"all":true} arm is deployment-wide — see handleRevokeSessions
			// (sessions.go) for why that is the tier working rather than a hole,
			// and TestSecurityAdminRevokesSuperAdmin for the pin. Stated here
			// because "only ever SUBTRACTS reach" is the justification that put
			// the route on this group, and a reader is entitled to know it was
			// measured against a target in the tier ABOVE, not just a member.
			//
			// NOT a rung below operatorOnly on a ladder — the two tiers overlap
			// on this surface and deliberately do not nest. A security admin's
			// SSH key and attach ticket still stamp `member`, so the tier never
			// yields a shell in someone else's sandbox; that asymmetry is why
			// there are two named predicates and not one role comparison. New
			// routes keep defaulting to operatorOnly, the safe direction:
			// widening later is the one-line move this group exists for,
			// narrowing after the fact is a regression nobody notices.
			//
			// The invariant that makes /permissions delegable at all: No capability
			// kind can ever reach the admin tier. capAllowed / capGranted
			// short-circuit on isOperator ALONE (capabilities.go), so a security
			// admin is capability-bounded exactly like a member — they may self-grant
			// through the /permissions routes below, audited, and still reach nothing
			// the super-admin exemption would have given them. Without that
			// invariant, handing this tier the grant table would be a self-promotion
			// primitive; with it, it is just the org allow/denylist. Pinned by
			// TestCapabilityGrantsNeverReachTheAdminTier (security_admin_test.go) — a
			// capability kind that ever widens isOperator breaks that test, not this
			// comment.
			//
			// WHY THE OPERATOR-TOPOLOGY READS ARE NOT HERE. GET /site-config,
			// /sources, /sources/{id} and /base-images are operatorOnly, not
			// securityOps. The tier argument for widening them is real — a security
			// admin holds the org allow/denylist, so scm_hosts is arguably theirs —
			// but it does not survive what those documents actually carry: an
			// upstream-proxy PASSWORD ref, a database-password requirement key, and
			// the /srv NFS path of a local_dir source. That is credential material
			// and the host, two of the three axes this tier is DEFINED never to
			// reach. The scm_hosts case is an argument for a SCOPED endpoint
			// returning that one field, never for handing over a document that also
			// carries a proxy password — and such an endpoint has no caller today, so
			// it is not built on spec.
			//
			// One rule, and it is not "these four routes". The tier does not reach
			// the host axis — local_dir paths, the operator's registry coordinate,
			// stored secret NAMES — on ANY route, and there are two ways for a route
			// to honour that. These four stay narrowed because they serve whole
			// operator documents nobody projects. GET
			// /workspaces{,/{id},/build,/observed-egress} instead WITHHOLD that axis
			// per reader (helpers.go's workspaceReadSecurity: paths and
			// base_image.image blanked, the secret:/write: requirement keys and the
			// scanned profile's host keys dropped) while keeping the EGRESS axis,
			// which is the input to the decision this tier is widened to make.
			//
			// Said here because the asymmetry is what a reader notices first: a
			// security_admin is answered 403 by /sources and 200 by
			// /workspaces/{id} on what looks like the same datum, and the
			// tempting resolution — widen these four to match — is the wrong
			// one. The workspace read is not wider; it is projected. Pinned
			// field by field by TestSecurityAdminForeignWorkspaceFieldByField
			// (security_admin_workspace_read_test.go), and this paragraph is
			// pinned against the code by TestSecurityTierNoteStatesOneRule.
			//
			// Stored-policy WRITES (POST/PUT/DELETE /policies) stay on
			// operatorOnly for the twin reason: a stored run_policy is selectable
			// CONTENT, so a SEC write path there would let a security admin
			// author a policy pairing an operator secret with attacker egress and
			// then simply select it. Profile authoring lives at /governance below.
			securityOps := r.With(s.requireSecurityOperator)
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
			// OWNER-OR-ADMIN rather than operator-only: the ticket
			// mints a live interactive PTY inside a RUNNING sandbox — injected
			// keystrokes and whatever the agent's injected credentials left on
			// screen — which is strictly more than "launch a run", but a member
			// may still hold one for a run THEY created (handleAttachTicket's
			// getRunAuthorized gate; a foreign run 404s, no existence oracle).
			r.Post("/runs/{id}/attach-ticket", s.handleAttachTicket)

			// Approvals: reading the queue is a member act (own runs only — see
			// handleListApprovals), DECIDING is OWNER-OR-ADMIN rather than
			// operator-only: the decision IS the live authorization over an
			// egress/credential escalation, but a member may decide one raised by
			// a run THEY own — decide() enforces it (a foreign approval 404s, no
			// existence oracle). The sandbox can only ever REQUEST one (machine
			// audience, /internal/approvals below), so this cannot starve an agent
			// of anything it could previously do for itself.
			r.Get("/approvals", s.handleListApprovals)
			r.Post("/approvals/{id}/approve", s.handleApproveApproval)
			r.Post("/approvals/{id}/deny", s.handleDenyApproval)

			r.Get("/audit", s.handleQueryAudit)
			// Uncapped NDJSON bulk export beside the capped, paginated read above —
			// per-principal evidence ("everything developer X did") in one request.
			r.Get("/audit/export", s.handleExportAudit)
			// Tamper-evidence sweep over the audit hash chain (migration 0047),
			// plus the sandbox sweep — one on each tier, hence two routers.
			s.adminRoutes(operatorOnly, securityOps)
			r.Get("/me", s.handleMe)
			// Own effective capability set — member-safe (classMember): every
			// route AROUND this one on /permissions below is operator-only, but
			// a member reading only their OWN grants (ListCapabilityGrantsFor,
			// scoped to their own subjects) discloses nothing about anyone else.
			r.Get("/me/capabilities", s.handleMeCapabilities)
			// SSH gateway key registry (sshkeys.go): self-service, any authenticated
			// human — scoped to their OWN principal at the store, so this is
			// deliberately on r, not operatorOnly (see sshkeys.go's package doc).
			// Deliberately this one small, localized block rather than a mount of
			// its own.
			r.Get("/me/ssh-keys", s.handleListSSHKeys)
			r.Post("/me/ssh-keys", s.handleAddSSHKey)
			r.Delete("/me/ssh-keys/{fingerprint}", s.handleDeleteSSHKey)
			s.mountAccountRoutes(r, securityOps)
			// Sign-out. The console POSTs /api/v1/auth/logout, so the POST has to be
			// mounted HERE: a root GET /auth/logout alone leaves the POST unrouted
			// (404), the HttpOnly session cookie alive, and the next probe silently
			// re-signing the operator in. Nil-OIDC (local/token mode) is a safe
			// no-op — see handleLogout.
			r.Post("/auth/logout", s.handleLogout)
			// Revoke-a-human-now — mounted only when the store is wired
			// (same "if s.cfg.X != nil" pattern as the Secrets block below),
			// which cmd/wardynd does exactly when OIDC is configured (there is
			// nothing to revoke without an OIDC session mechanism). securityOps:
			// cutting a compromised human's live sessions is the time-critical
			// half of incident response, and it hands the actor nothing — a
			// revocation only ever SUBTRACTS reach.
			if s.cfg.SessionRevocations != nil {
				securityOps.Post("/sessions/revoke", s.handleRevokeSessions)
			}
			// First-run setup readiness. MUST stay in this humanOrAdminAuth group
			// (anonymous non-local => 401): it enumerates providers/keys/CLIs
			// (capability disclosure) and must never sit on the public /healthz.
			r.Get("/setup/status", s.handleSetupStatus)

			// Managed harness login: launch an interactive login sandbox where
			// the caller runs `claude setup-token` (or `aws sso login`), then
			// paste the resulting long-lived subscription token so Wardyn injects
			// it proxy-side into every run (compose-mode subscription without a
			// host ~/.claude). Secret store required (the token is stored
			// age-encrypted).
			//
			// RBAC, and it is SPLIT. The token PASTE and DISCONNECT are on
			// operatorOnly like policy/workspace/site-config below: humanOrAdminAuth
			// is AUTHENTICATION only, so a signed-in MEMBER gets
			// 403 there — they write the deployment's SHARED credential, which every
			// run inherits. The LOGIN LAUNCH is on this group instead, with its
			// predicate inside the handler: when the agent roster declares
			// `credential_source: per_user`, the session it captures is the
			// caller's OWN, so an admin-only door would leave a member with no route
			// to model access at all. A member with no such row still gets 403 —
			// from authorizeHarnessLogin, which also requires capAgent on the row's
			// agent. Every connect/disconnect is audited
			// (harness.credential.captured/disconnected).
			s.mountSetupMutationRoutes(r, operatorOnly)

			// Policy management (gated to authenticated humans — a valid SSO
			// session or the admin token). WRITES are additionally operator-only:
			// a signed-in MEMBER can read policies but not CRUD
			// them; an ADMIN can. Every spec is validated before it is persisted
			// (fail closed); writes are audited.
			operatorOnly.Post("/policies", s.handleCreatePolicy)
			r.Get("/policies", s.handleListPolicies)
			// Static route: chi matches this before the {id} wildcard below, so
			// "default" never reaches parseIDParam as a bogus policy UUID.
			r.Get("/policies/default", s.handleGetDefaultPolicy)
			r.Get("/policies/{id}", s.handleGetPolicy)
			operatorOnly.Put("/policies/{id}", s.handleUpdatePolicy)
			operatorOnly.Delete("/policies/{id}", s.handleDeletePolicy)
			// Policy risk grade: the deterministic composer.Grade assessment for a
			// bare spec, with no run attached and nothing persisted — the policy
			// panel's live safety meter. Member-accessible like the reads above
			// (mirrors /runs/preflight), not operator-only: a member may see the
			// risk of a spec they cannot necessarily save.
			r.Post("/policies/grade", s.handleGradePolicy)

			// Workspace management (onboarding of local dirs + repos a run may
			// attach), gated to authenticated humans (SSO session or admin token).
			//
			// Ownership tier (0048, docs/design/member-role-desktop.md §b): the
			// CRUD/scan/build routes are OWNER-OR-ADMIN rather than admin-only —
			// a member creates workspaces they own (create stamps owned_by from
			// the session) and may edit/delete/scan/build THEIR OWN. The check
			// lives INSIDE each handler as getWorkspaceAuthorized (mutations) /
			// getWorkspaceReadable (reads), exactly the way the /runs block does
			// owner-or-admin on the plain `r` group — no second middleware group,
			// no chi.Walk matrix churn beyond the reclassification. An
			// operator-owned workspace (owned_by='' — what an upgraded deployment's
			// older rows carry) still answers a member's mutation with the same 403
			// requireOperator wrote.
			//
			// A route that widens an egress ceiling, binds credential material, or
			// writes the host stays gated below; owning a workspace does not make
			// a member the operator of it. That set is split across the two admin
			// tiers: the EGRESS-DECISION lane — approved-egress,
			// denied-egress and record/{task}/promote-egress — is securityOps
			// (deciding which hosts a workspace's runs may reach is the same
			// authority as deciding an egress approval, and the promote-egress
			// writer is literally the bulk form of it), while llm-cred (binds
			// credential material), requirements, reassign (user administration),
			// env-as-code/write (writes the host) and RECORD stay operatorOnly.
			// None of the three SEC routes carries an in-handler tier check —
			// scopedWorkspaceWrite and getWorkspaceOr404 authorize nothing beyond
			// existence — so for them the router IS the whole gate, and this is
			// the only place the tier is decided.
			//
			// Record is not an egress decision, which is why it is not in that
			// lane. It was grouped there by association with promote-egress, but
			// the two do categorically different things: promote-egress writes a
			// list, while record launches an interactive sandbox — open egress by
			// default (AllowAllEgress = !confined), the workspace's local_dir
			// bind-mounted (read-WRITE when the owning member ticked Writable),
			// the repo clone credential minted, the workspace's required secret:/
			// integration: rows folded into proxy-side injections, and the
			// operator's LLM credential attached. That is all three things
			// securityOps is DEFINED never to reach — into a run, credential
			// material, the host — and the classAdmin criterion authz_test.go
			// states for llm-cred/requirements ("BIND CREDENTIAL MATERIAL or
			// WRITE THE HOST") names record too. It also stamped the run
			// CreatedBy = the CALLER, which is what defeated the guards written
			// to hold this line: handleAttachTicket's strict re-check refuses a
			// security admin a PTY in a FOREIGN sandbox, and a run they launched
			// themselves is not foreign. And the tier could not even READ the
			// workspace it was recording — getWorkspaceReadable answers a
			// security admin 404 for a member-owned row — so the surface let them
			// launch a credentialed sandbox over something they were refused a
			// GET on.
			//
			// Create/update validate the source the
			// same way policy WorkspaceMounts do (runner.ValidateMount /
			// ValidateTarget) or the way AgentRun.Repo does (repoFieldSafe +
			// repoCloneURL); writes are audited. Scan is a separate endpoint
			// (source_scan.go handleScanWorkspace, beside the fan-out it drives)
			// that runs the deterministic workspacescan and persists the
			// profile + status.
			// Tier-1 source library + tier-2 base-image catalog routes —
			// mounted from sources.go, same posture as the workspaces block.
			s.mountLibraryRoutes(r, operatorOnly)

			r.Post("/workspaces", s.handleCreateWorkspace)
			r.Get("/workspaces", s.handleListWorkspaces)
			r.Get("/workspaces/{id}", s.handleGetWorkspace)
			r.Put("/workspaces/{id}", s.handleUpdateWorkspace)
			r.Delete("/workspaces/{id}", s.handleDeleteWorkspace)
			r.Post("/workspaces/{id}/scan", s.handleScanWorkspace)
			// Workspace image build (the wizard's Build step): status is a
			// member-tier read like the other workspace GETs; kicking a build is
			// owner-or-admin — the builder runs WARDYN-GENERATED recipes from the
			// scanned profile, never member free-text (contrast devcontainer_repo,
			// which stays unconditionally operator-only).
			r.Get("/workspaces/{id}/build", s.handleGetWorkspaceBuild)
			r.Post("/workspaces/{id}/build", s.handleBuildWorkspace)
			// Operator-owned egress approvals (promotion of the scanner's
			// content-derived suggestions; see handleSetApprovedEgress).
			securityOps.Put("/workspaces/{id}/approved-egress", s.handleSetApprovedEgress)
			// Denied-egress twin (Phase 4 revocation) — the only way to undo a
			// `deny · always` decision, including curing an already-bricked
			// workspace (H5); see handleSetDeniedEgress's doc comment for why its
			// validator is deliberately narrower than the approved-egress one above.
			securityOps.Put("/workspaces/{id}/denied-egress", s.handleSetDeniedEgress)
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
			// Offboarding (decision O6): return a departed member's owned
			// workspace to the operator (owned_by=""). Admin-only HERE rather
			// than in the handler so the refusal is a constant 403 that never
			// varies with whether the id exists — owning a workspace does not
			// let a member disown it.
			operatorOnly.Post("/workspaces/{id}/reassign", s.handleReassignWorkspace)
			operatorOnly.Post("/workspaces/{id}/record", s.handleRecordWorkspace)
			securityOps.Post("/workspaces/{id}/record/{task}/promote-egress", s.handlePromoteRecordEgress)
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
			// WRITE/DELETE are self-service (migration `0050`), not
			// admin-only: any signed-in human manages their OWN row
			// (handlePutSecret/handleDeleteSecret scope by
			// secretOwnerFromRequest) — an operator's own row is the ""
			// namespace, exactly today's behavior. A member can never reach
			// another member's row (Store.For(owner) never resolves it) or
			// the four Bedrock/SigV4 names (still operator-only). Admin
			// cross-principal reads/deletes go through ?owner=. The LIST
			// stays viewer-readable — it returns names only, never values.
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
			// RBAC (same as policy/workspace above): humanOrAdminAuth is
			// AUTHENTICATION only, so the PUT is operator-only — a signed-in
			// MEMBER can read the config but not rewrite it.
			// This is where that matters most: the blast radius is corp-wide (the
			// baseline feeds every run's upstream proxy / artifact mirror / SCM
			// hosts), higher than a single policy.
			// GET is operatorOnly for the SAME reason the PUT beside it is, and
			// that symmetry is the whole point: authz_test.go justifies the PUT
			// as SUPER because it "replaces the WHOLE site-config document,
			// integration credential refs included" — and the GET returns that
			// same whole document, refs included. UpstreamProxySecretRef and
			// every integrations[].secrets[].secret_name are credential REFS;
			// UpstreamProxyURL, ScmHosts, ArtifactOverrides and EgressRedirects
			// are the operator's internal topology.
			//
			// The console degrades rather than breaks: both callers already
			// wrap this GET in .catch() and render with a null config
			// (settings-screen.tsx, setup-screen.tsx). What a member loses is
			// exactly the leak — the verbatim upstream_proxy_url line and the
			// scm_hosts-derived rows — on cards whose every control is already
			// disabled for them. Widening to securityOps later is the one-line
			// move routes.go's own tier note describes; narrowing after the
			// fact is the regression nobody notices, so this lands on the safe
			// side of that rule.
			operatorOnly.Get("/site-config", s.handleGetSiteConfig)
			operatorOnly.Put("/site-config", s.handlePutSiteConfig)
			// Live connectivity probes: launch a throwaway one-shot sandbox and
			// actually traverse the upstream proxy / egress redirect, rather than
			// a "we wrote it down" test-connection button (site_config_probe.go).
			// securityOps, NOT the PUT's tier: these are NON-MUTATING —
			// they write no config, bind no credential, and answer "does the
			// baseline this deployment already declares actually work", which is
			// the evidence half of the security admin's job. The PUT above stays
			// operatorOnly because it replaces the WHOLE document, integration
			// credential refs included; a SEC-writable per-field subset is the
			// top phase-2 item, not something to fake with a second gate here.
			// Extracted into a mount because routes() sits AT the funlen ratchet
			// (.golangci.yml, 150 non-comment lines): a new route family here has
			// to free its line by extracting an existing block first.
			s.mountSiteConfigProbeRoutes(securityOps)
			// Workspace providers — see mountWorkspaceProviderRoutes, and
			// the agent roster beside it (agent_providers.go). Both MOUNTS,
			// attached with no blank line, for the ratchet reason just above.
			s.mountWorkspaceProviderRoutes(operatorOnly)
			s.mountAgentProviderRoutes(operatorOnly)

			// Effective integration set (stored ∪ legacy-derived) with live
			// capabilities — see internal/api/integrations.go /
			// setup_integrations.go. Read-only, member-class, and deliberately
			// NOT the posture of the site-config GET above it: that sibling is
			// operatorOnly precisely BECAUSE a secret NAME is a credential REF
			// (see its own tier note), and this route serves the very rows that
			// document embeds. A payload argument for widening here would be the
			// sibling's argument for narrowing, read backwards.
			//
			// What makes the wider tier honest is the PROJECTION, not a claim
			// about the payload: a non-operator is answered
			// memberSafeIntegration's view (setup_integrations.go) — identity,
			// kind, disabled, default_for and the live capability matrix, its
			// reasons scrubbed of anything withheld — with secrets[], egress[],
			// config and docs dropped. The tier stays wide because the launch
			// card picks an integration by identity; the credential refs and
			// the internal hosts do not cross it. GET /setup/status publishes
			// the same rows through the same projection.
			r.Get("/integrations", s.handleListIntegrations)
			// Integration writes: PUT creates-or-replaces a stored row, DELETE
			// removes one. operatorOnly — same corp-wide blast radius as
			// site-config's PUT (setup_integrations.go).
			//
			// There is deliberately no test-connection route: the Settings cards
			// state what is STORED and say so plainly ("Wardyn stores this — it
			// doesn't dial the provider to check it"), which is the honest claim
			// for a credential nobody has used yet. A real run is the real test.
			operatorOnly.Put("/integrations/{id}", s.handlePutIntegration)
			operatorOnly.Delete("/integrations/{id}", s.handleDeleteIntegration)

			// Permissioning (migration 0042) — four routes, all
			// securityOps; the tier argument is on the helper below.
			s.mountPermissionRoutes(securityOps)
			// Access / role mappings (migration 0051): the
			// console's Getting Started -> People editor over the store half of
			// internal/auth/oidc's RoleMappingSource. All four routes are
			// operatorOnly (access.go's mountAccessRoutes) — this bounds who
			// derives admin at all, not a single member's capability set, so
			// unlike /permissions there is no member-safe read to carve out.
			// SUPER even for the two READS, and the contrast with
			// /permissions right above is the point: a security admin who could
			// write role mappings would simply map themselves to admin, and the
			// GET leaks the operator-email target list. This is the one admin
			// surface the tier must not be able to see or touch.
			s.mountAccessRoutes(operatorOnly)
			// Directory autocomplete: the read behind every
			// "who" picker — the governance assignment subject and the
			// People-step mapping value. securityOps, NOT the operatorOnly the
			// four /access routes right above sit on, and the split is
			// deliberate: those WRITE who derives admin, this one only READS
			// the directory, and the security admin who authors assignments is
			// exactly the person who needs to look up the group they are
			// binding. It still discloses org structure (names, emails, group
			// membership), which is why it is gated at all rather than sitting
			// on r. Registered UNCONDITIONALLY — an unset connector answers a
			// distinct 503 rather than vanishing, so the console can tell
			// "not configured" from "no such route", and TestAuthzMatrix's
			// every-conditional-route-mounted doctrine has nothing to arrange.
			securityOps.Get("/access/directory/search", s.handleDirectorySearch)

			// Governance profiles (migration 0052): named, ASSIGNABLE ceilings
			// and the rows binding them to a user/group/everyone — what lets one
			// deployment run two groups under two different policies instead of
			// the single site-wide DefaultPolicy. Seven routes, all securityOps
			// (governance.go's mountGovernanceRoutes): profile authoring IS the
			// security-admin duty. Note mountGovernanceRoutes still NAMES its
			// parameter `operatorOnly`; the group a mount function receives is
			// decided HERE, never by that parameter's name.
			//
			// Authoring a profile is not self-exemption: effectiveCeiling's
			// short-circuit keys on isOperator, so a security admin's own runs
			// stay bound by whichever profile (or DefaultPolicy) applies to
			// them. They author the ceiling; they do not stand outside it.
			// The seventh is POST /governance/preview, the dry run: it takes
			// claims and answers with the profile THE RESOLVER picks, so the
			// console never re-implements the precedence rule in order to show
			// it.
			s.mountGovernanceRoutes(securityOps)

			// User drives (migration 0054): the storage an admin registers
			// and allocates, and the per-run flag a member mounts theirs with.
			// Seven routes, all operatorOnly — SUPER, NOT the securityOps tier
			// the /governance family right above sits on, and the contrast is
			// the tier line itself. A drive names a HOST PATH (host_root) or a
			// cluster storage class, and "never the host" is exactly what
			// separates the two admin tiers; a security admin's authority over
			// drives is the DenyUserDrive door in the profile editor, which is
			// already theirs through /governance. Widening the grant + preview
			// routes to securityOps is a one-line move plus matrix rows once the
			// tier's own review settles — the safe direction, taken later.
			//
			// Registered UNCONDITIONALLY (mountUserDriveRoutes' own doc), so
			// TestAuthzMatrix's every-conditional-route-mounted doctrine has
			// nothing to arrange.
			// …and POST /drives/{id}/reclaim with them: mountUserDriveFamily
			// (user_drives_reclaim.go) joins the family's two halves.
			s.mountUserDriveFamily(operatorOnly)

			// Recording replay: GET /api/v1/runs/{id}/recording/{id}. Owner-or-admin:
			// recordingAuthorizer is the SAME ownership rule
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

			// Injection resolve: returns the formatted secret value for an
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

// mountAccountRoutes registers the caller's own account surfaces — per-user
// API tokens (self-service on r; the two admin twins on securityOps) and the
// run-detail layout — carved out of routes() purely for funlen; the routes()
// maintenance-count comment counts the two securityOps lines here.
func (s *Server) mountAccountRoutes(r chi.Router, securityOps chi.Router) {
	// Per-user API tokens (apitokens.go): the same self-service shape as
	// the ssh-keys block above — scoped to the caller's OWN principal at
	// the store, so these sit on r rather than an admin group. A MEMBER minting a
	// token grants themselves nothing new: the token carries their own
	// stamped role, so it reaches exactly the routes their session does
	// (see apiTokenAuth). The admin twins — the deployment-wide inventory
	// and revoke-ANYONE's — are the two securityOps lines below.
	//
	// securityOps rather than operatorOnly: a stale or compromised
	// credential is the incident, and inventory-then-revoke is the response.
	// Neither route hands the caller reach — the inventory returns token
	// METADATA (never a raw token, which exists only in the mint response) and
	// the DELETE only ever subtracts. Note a revoked token's owner keeps their
	// session; cutting that is POST /sessions/revoke, the same tier.
	r.Get("/me/tokens", s.handleListAPITokens)
	r.Post("/me/tokens", s.handleCreateAPIToken)
	r.Delete("/me/tokens/{id}", s.handleRevokeAPIToken)
	securityOps.Get("/tokens", s.handleListAllAPITokens)
	securityOps.Delete("/tokens/{id}", s.handleAdminRevokeAPIToken)
	// Run-detail widget layout: per-user, per-preset, server-synced so a
	// layout survives a new machine (localStorage would not). Scoped to
	// the caller's OWN principal at the store, exactly like the ssh-keys
	// block above — never a principal taken from the body.
	r.Get("/me/run-layout", s.handleGetRunLayout)
	r.Put("/me/run-layout", s.handlePutRunLayout)
	// "View as member" (membermode.go). Registered HERE rather than
	// beside the ssh-keys block in routes() only because routes() sits exactly
	// on the funlen ratchet — this is the /me self-service family either way.
	//
	// On r and never operatorOnly: the toggle acts on the CALLER's own session
	// and nobody else's, and inside the mode the caller's effective role IS
	// member, so an operator-gated exit would be a door that locks from the
	// inside. The no-per-human-role lane (admin token, local mode, no IdP) is
	// refused inside the handler — a 400, not a tier.
	r.Post("/me/member-mode", s.handleSetMemberMode)
}

// mountPermissionRoutes registers the capability-grant family: which of the
// powers a MEMBER already has may they actually use. GET returns the whole
// grant table + enforcement map in one call (the console Permissions screen's
// entire data need).
//
// All four on securityOps: this IS the org allow/denylist primitive — it
// bounds every member's blast radius rather than a single run's — and it is
// delegable ONLY because of the no-capability-reaches-admin invariant spelled
// out on securityOps in routes(). A security admin may write their own grants
// here; every write is audited, and none of them widens isOperator. The
// member-safe read of a caller's OWN effective set is GET /me/capabilities,
// which stays on the plain authenticated group in routes().
//
// Split out of routes() for the same reason adminRoutes is: routes() sits at
// golangci funlen's 150-line cap (ignore-comments, blank lines counted), so an
// added statement there has to buy its space from somewhere. A route family
// that already reads as one unit is the honest place to buy it.
func (s *Server) mountPermissionRoutes(securityOps chi.Router) {
	securityOps.Get("/permissions", s.handleGetPermissions)
	securityOps.Post("/permissions/grants", s.handleUpsertCapabilityGrant)
	securityOps.Delete("/permissions/grants/{id}", s.handleDeleteCapabilityGrant)
	securityOps.Put("/permissions/enforcement", s.handlePutCapabilityEnforcement)
}

// adminRoutes registers the two admin-gated maintenance routes — one per tier,
// which is why it takes both routers (the same two-router shape
// mountAccountRoutes and mountLibraryRoutes already use).
//
// Split out of routes() rather than inlined: routes() sits exactly at golangci
// funlen's 150-line cap, so ANY addition to it fails `make lint` — a step of
// the required `build` context. A helper is the honest answer; padding the cap
// for one route is not.
func (s *Server) adminRoutes(operatorOnly chi.Router, securityOps chi.Router) {
	// securityOps: the tamper-evidence verdict over the audit hash chain is the
	// evidence the security tier's whole job rests on, and the tier's charter
	// promises them "verify the audit chain" in words. Gated at all — unlike the two
	// paginated /audit reads in routes(), which scope themselves per-principal
	// — because the verdict counts every row in the deployment, and
	// whole-fleet audit VOLUME is the same disclosure that keeps /metrics
	// gated. Operator-INVOKED by design: wardynd never verifies at boot.
	securityOps.Get("/audit/chain/verify", s.handleVerifyAuditChain)
	// Sandbox sweep. SUPER, and the reason matters because an operator deciding
	// who to trust with RoleSecurityAdmin reads exactly these lines: the sweep
	// drives the RUNNER — Status then StopSandbox — across every run in the
	// deployment, plus the credential revoke cascade for each. That is reach at
	// the HOST over the whole fleet from one call, and the host IS one of the
	// three axes securityOps is defined never to reach. The per-run kill is
	// bounded to a run the caller names and audits per run; this is unbounded and
	// audits one row for the batch.
	//
	// Blast radius and host reach are the whole argument. Foreign-run termination
	// is NOT: the security tier has that axis deliberately (ownsRunOrAdmin is
	// isSecurityOperator, helpers.go, so POST /runs/{id}/kill admits a security
	// admin on ANY run — TestSecurityAdminCanStopAForeignRun), and the sweep
	// never touches a live run in any case. SweepTerminalSandboxes skips every
	// non-terminal row outright (`if !isTerminalRunState(run.State) ... continue`,
	// runs_lifecycle.go) and reaps only the sandbox of a run that has ALREADY
	// ended and whose container outlived it: orphan cleanup, not termination,
	// pinned by TestSweepTerminalSandboxes_TearsDownOrphanedLiveSandbox, whose
	// fixture carries a RUNNING run precisely to assert it is left alone.
	//
	// Deliberately not wired at boot (sweepOrphanedSandboxes already covers that
	// case — see reconcile.go) and deliberately not a ticker; see
	// handleSweepSandboxes for the cost argument.
	operatorOnly.Post("/admin/sandboxes/sweep", s.handleSweepSandboxes)
}
