// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"time"
)

// handleSetupOnboardingComplete marks THIS INSTALL as having been through the
// Getting Started funnel, by stamping SiteConfig.OnboardingCompletedAt.
//
// Why this is a server call and not a browser flag: the console kept this state
// in localStorage three different ways, and each was wrong in a way that
// shipped. A browser flag outlives the install it describes, so a wiped
// database went on skipping its own funnel; and localStorage is ORIGIN-scoped,
// so one console reached at 127.0.0.1 and at localhost disagreed about whether
// onboarding had happened at all. Onboarding is a fact about the install, so
// the install stores it.
//
// Idempotent by design: the FIRST completion is the interesting one (it is what
// the landing, the hero and the gate read), so a later call must not move the
// timestamp — an operator revisiting and re-finishing the funnel is not a new
// onboarding. Returns 200 either way; a caller that raced another and lost has
// still achieved what it asked for.
func (s *Server) handleSetupOnboardingComplete(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "no store configured")
		return
	}
	// SEAM-1: the same mutex PUT /site-config and the integration writers take,
	// for the same reason — this is a read-modify-write on the one site-config
	// document, and a concurrent integration write would otherwise clobber it.
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()

	cfg, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get site config: "+err.Error())
		return
	}
	if cfg.OnboardingCompletedAt != nil {
		writeJSON(w, http.StatusOK, map[string]any{"onboarding_complete": true})
		return
	}
	now := time.Now().UTC()
	cfg.OnboardingCompletedAt = &now
	if _, err := s.cfg.Store.PutSiteConfig(r.Context(), cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "put site config: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"setup.onboarding.completed", "site_config", "success", mustJSON(map[string]any{
			"completed_at": now,
		})))
	writeJSON(w, http.StatusOK, map[string]any{"onboarding_complete": true})
}
