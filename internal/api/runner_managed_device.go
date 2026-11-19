// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// requiresManagedDevice is runners.require_managed_device: a new local run is refused on a personal-trust
// runner. Off by default (nil settings and false are the same), so an existing install is unchanged until
// a super admin turns it on.
func requiresManagedDevice(cfg types.SiteConfig) bool {
	return cfg.Runners != nil && cfg.Runners.RequireManagedDevice
}

// runnerSettingsOf is the stored runner settings as the settings routes answer them.
func runnerSettingsOf(cfg types.SiteConfig) types.RunnerSettings {
	return types.RunnerSettings{Enabled: runnersEnabled(cfg), RequireManagedDevice: requiresManagedDevice(cfg)}
}

// storeRunnerSettings writes the fields req names under the site-config lock, leaves the others as
// stored, and audits each field it names, returning the settings before and after. The lock is released
// before it returns, so nothing the caller does next holds it.
func (s *Server) storeRunnerSettings(w http.ResponseWriter, r *http.Request, req types.RunnerSettingsRequest) (before, after types.RunnerSettings, ok bool) {
	r, unlock, ok := s.lockDoor(w, r, db.SiteConfigLockClass)
	if !ok {
		return before, after, false
	}
	defer unlock()
	cfg, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return before, after, false
	}
	before = runnerSettingsOf(cfg)
	after = before
	if req.Enabled != nil {
		after.Enabled = *req.Enabled
	}
	if req.RequireManagedDevice != nil {
		after.RequireManagedDevice = *req.RequireManagedDevice
	}
	cfg.Runners = &after
	if _, err := s.cfg.Store.PutSiteConfig(r.Context(), cfg); err != nil {
		writeServerError(w, r, "put site config", err)
		return before, after, false
	}
	auditSetting := func(target string, was, now bool) {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			placement.ActionRunnersEnabledSet, target, "success", mustJSON(map[string]any{"before": was, "after": now})))
	}
	if req.Enabled != nil {
		auditSetting("runners.enabled", before.Enabled, after.Enabled)
	}
	if req.RequireManagedDevice != nil {
		auditSetting("runners.require_managed_device", before.RequireManagedDevice, after.RequireManagedDevice)
	}
	return before, after, true
}
