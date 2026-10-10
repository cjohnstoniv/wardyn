// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func runnersEnabled(cfg types.SiteConfig) bool { return cfg.Runners != nil && cfg.Runners.Enabled }

// handleGetRunnerSettings is GET /api/v1/runners/settings: whether runners are on, which the
// runners page reads while the runner routes themselves answer runners_disabled.
func (s *Server) handleGetRunnerSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	writeJSON(w, http.StatusOK, types.RunnerSettings{Enabled: runnersEnabled(cfg)})
}

// runnersEnableRefusal is why runners cannot be turned on now, or "". The public organisation URL
// that runner identities are bound to must be HTTPS (loopback http for development), and so must the
// control-plane URL the relay carries bearers over.
func (s *Server) runnersEnableRefusal() string {
	if federation.CheckOrgURL(s.cfg.RunnerOrgURL) != nil {
		return "set WARDYN_RUNNER_ORG_URL to this organisation's public HTTPS URL (http is accepted only for localhost)"
	}
	if s.cfg.ControlPlaneURL != "" && hoptls.CheckURL(s.cfg.ControlPlaneURL) != nil {
		return "WARDYN_CONTROL_PLANE_URL is plain http:// to a non-loopback host; point it at the internal TLS listener"
	}
	return ""
}

// handlePutRunnerSettings is PUT /api/v1/runners/settings {enabled}: the one door that turns runners
// on or off. SUPER admin only; PUT /site-config cannot reach it. Turning off is never refused.
func (s *Server) handlePutRunnerSettings(w http.ResponseWriter, r *http.Request) {
	var req types.RunnerSettingsRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigInvalid, "enabled is required")
		return
	}
	if *req.Enabled {
		if msg := s.runnersEnableRefusal(); msg != "" {
			writeErrorReason(w, http.StatusConflict, reasonRunnersOrgURLInvalid, "runners can't be turned on yet: "+msg)
			return
		}
	}
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
	before := runnersEnabled(cfg)
	cfg.Runners = &types.RunnerSettings{Enabled: *req.Enabled}
	if _, err := s.cfg.Store.PutSiteConfig(r.Context(), cfg); err != nil {
		writeServerError(w, r, "put site config", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		placement.ActionRunnersEnabledSet, "runners.enabled", "success", mustJSON(map[string]any{"before": before, "after": *req.Enabled})))
	writeJSON(w, http.StatusOK, *cfg.Runners)
}
