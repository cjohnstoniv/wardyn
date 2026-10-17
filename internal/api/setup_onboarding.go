// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
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
// polls. Every person signs in to a model provider through that provider's own
// door (POST /model-providers/{id}/sign-in, provider_signin.go).
func (s *Server) mountSetupMutationRoutes(operatorOnly chi.Router) {
	// Install-side onboarding mark — why it exists is on the handler below.
	operatorOnly.Post("/setup/onboarding-complete", s.handleSetupOnboardingComplete)
}

func (s *Server) handleSetupOnboardingComplete(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store == nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonSetupOnboardingStoreUnavailable, "no store configured")
		return
	}
	// The same mutex PUT /site-config and the integration writers take,
	// for the same reason — this is a read-modify-write on the one site-config
	// document, and a concurrent integration write would otherwise clobber it.
	r, unlock, ok := s.lockDoor(w, r, db.SiteConfigLockClass)
	if !ok {
		return
	}
	defer unlock()

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
		"setup.onboarding.complete", "site_config", "success", mustJSON(map[string]any{
			"completed_at": now,
		})))
	writeJSON(w, http.StatusOK, map[string]any{"onboarding_complete": true})
}
