// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// Route groups routes() mounts, carved out for funlen and kept here for scripts/check-file-size.sh.
// Each group's tier comes from the router it is handed, as in routes.go.

// mountRecordingRoutes registers a run's recording replay and its audit-chain
// check on r — carved out of routes() purely for funlen.
func (s *Server) mountRecordingRoutes(r chi.Router) {
	// Recording replay: GET /api/v1/runs/{id}/recording/{id}. Owner-or-admin:
	// recordingAuthorizer is the SAME ownership rule
	// getRunAuthorized enforces, applied INSIDE the handler (the outer {id}
	// here only selects a chi sub-route, so the authorization decision
	// belongs to the mounted handler, not this registration).
	if s.cfg.RecordingStore != nil {
		r.Mount("/runs/{id}/recording", recording.Handler(s.cfg.RecordingStore, s.recordingAuthorizer))
	}
	// A desktop recording's stretch of the audit chain verifies (recording_chain.go): the recording's readers.
	r.Get("/runs/{id}/recording-chain", s.handleRecordingChain)
}

// mountRenamedRunRoutes registers the 0.8 names for three routes issue #658
// renamed for one consistent shape across the attach family and the
// synthesize-a-profile verb — carved out of routes() purely for funlen. Each
// is the SAME handler as its pre-0.8 alias (still mounted inline in routes(),
// right above the r.Group this is called from), kept one minor; see
// docs/sdk.md's "Renamed in 0.8" table.
func (s *Server) mountRenamedRunRoutes(r chi.Router) {
	r.Post("/runs/{id}/profile/synthesize", s.handleSynthesizeProfile)
	r.Get("/runs/{id}/attach/holder", s.handleAttachHolder)
	r.Post("/runs/{id}/attach/ticket", s.handleAttachTicket)
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
	securityOps.Post("/tokens/{id}/move", s.handleMoveAPIToken)
	securityOps.With(s.refuseSubjectPrincipalParam).Delete("/people/{principal}/ssh-keys", s.handleAdminDeleteSSHKeys)
	s.mountPeopleRoutes(securityOps)
	s.mountInvitationRoutes(r, securityOps)
	s.mountServiceIdentityRoutes(r, securityOps)
	s.mountSCIMStatusRoute(securityOps)
	// Run-detail widget layout: per-user, per-preset, server-synced so a layout survives a new machine (localStorage
	// would not). Scoped to the caller's OWN principal at the store, like the ssh-keys block above, never the body's.
	r.Get("/me/run-layout", s.handleGetRunLayout)
	// The caller's own kept launch setups, newest first (launch_intent.go).
	r.Get("/me/launch-intents", s.handleListLaunchIntents)
	r.Put("/me/run-layout", s.handlePutRunLayout)
	s.mountPersonalWallsRoutes(r) // the caller's own walls, keyed by their session (personal_walls.go)
	s.mountWallPresetRoutes(r)    // named walls picked at launch, with the run templates' authority (wall_presets.go)
	// Per-user Azure DevOps sign-in (ado_entra.go): two browser doors that
	// capture ONE PERSON'S Azure DevOps refresh token. It belongs in this
	// self-service block and nowhere else — both doors refuse a caller with no
	// identity provider subject, the capture is bound to that subject
	// fail-closed, and the credential is written under that principal's own
	// namespace, so neither route can reach anyone else's credential whatever
	// tier the caller holds.
	s.mountAzureDevOpsSignInRoutes(r)
	// The user view (user_view.go), renamed in 0.8 from "view as member" /
	// POST /me/member-mode (docs/OPERATIONS.md's "Renamed in 0.8" appendix; a
	// clean break — the old path is not aliased). Registered HERE rather than
	// beside the ssh-keys block in routes() only because routes() sits exactly
	// on the funlen ratchet — this is the /me self-service family either way.
	//
	// On r and never operatorOnly: the toggle acts on the CALLER's own session
	// and nobody else's, and inside the view the caller's effective role IS
	// user, so an operator-gated exit would be a door that locks from the
	// inside. The no-per-human-role lane (admin token, local mode, no IdP) is
	// refused inside the handler — a 400, not a tier.
	r.Post("/me/view", s.handleSetUserView)
}
