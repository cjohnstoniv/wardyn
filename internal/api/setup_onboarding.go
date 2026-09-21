// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// handleSetupOnboardingComplete marks THIS INSTALL as having been through the
// Getting Started funnel, by stamping SiteConfig.OnboardingCompletedAt.
//
// Why this is a server call and not a browser flag: a browser flag outlives
// the install it describes, so a wiped database went on skipping its own
// funnel; and localStorage is ORIGIN-scoped,
// so one console reached at 127.0.0.1 and at localhost disagreed about whether
// onboarding had happened at all. Onboarding is a fact about the install, so
// the install stores it.
//
// Idempotent by design: the FIRST completion is the interesting one (it is what
// the landing, the hero and the gate read), so a later call must not move the
// timestamp — an operator revisiting and re-finishing the funnel is not a new
// onboarding. Returns 200 either way; a caller that raced another and lost has
// still achieved what it asked for.
// mountSetupMutationRoutes hangs the setup flow's mutating endpoints off the
// operator-only group — split from routes() at the funlen gate, and a real
// seam: these are the calls the Getting Started funnel makes on the
// operator's behalf, as opposed to the read-only status the whole console
// polls. The harness-credential trio needs a secret store to write into;
// the onboarding mark writes site config and mounts unconditionally.
//
// ONE of them is not operator-only any more. The container LOGIN launch sits on
// the signed-in-human group with its predicate inside the handler
// (authorizeHarnessLogin, harnesscred.go), because under a per_user roster row
// the credential it captures is the CALLER'S OWN and a member with no route to
// this endpoint has no route to model access at all. The token PASTE and the
// DISCONNECT stay operator-only: both write the deployment's shared credential.
func (s *Server) mountSetupMutationRoutes(humanOrAdmin, operatorOnly chi.Router) {
	if s.cfg.Secrets != nil {
		humanOrAdmin.Post("/setup/harness-login", s.handleHarnessLogin)
		operatorOnly.Put("/setup/harness-credential/{provider}", s.handleHarnessCredentialPaste)
		operatorOnly.Delete("/setup/harness-credential/{provider}", s.handleHarnessDisconnect)
	}
	// Install-side onboarding mark — why it exists is on the handler below.
	operatorOnly.Post("/setup/onboarding-complete", s.handleSetupOnboardingComplete)
}

func (s *Server) handleSetupOnboardingComplete(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "no store configured")
		return
	}
	// The same mutex PUT /site-config and the integration writers take,
	// for the same reason — this is a read-modify-write on the one site-config
	// document, and a concurrent integration write would otherwise clobber it.
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()

	cfg, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	if cfg.OnboardingCompletedAt != nil {
		writeJSON(w, http.StatusOK, map[string]any{"onboarding_complete": true})
		return
	}
	now := time.Now().UTC()
	cfg.OnboardingCompletedAt = &now
	if _, err := s.cfg.Store.PutSiteConfig(r.Context(), cfg); err != nil {
		writeServerError(w, r, "put site config", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"setup.onboarding.completed", "site_config", "success", mustJSON(map[string]any{
			"completed_at": now,
		})))
	writeJSON(w, http.StatusOK, map[string]any{"onboarding_complete": true})
}
