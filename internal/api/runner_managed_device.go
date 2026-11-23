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
	st := types.RunnerSettings{Enabled: runnersEnabled(cfg), RequireManagedDevice: requiresManagedDevice(cfg)}
	if cfg.Runners != nil {
		st.MDMDeviceCAPEM, st.MDMDeviceCRLURL = cfg.Runners.MDMDeviceCAPEM, cfg.Runners.MDMDeviceCRLURL
	}
	return st
}

// applyRunnerSettingsRequest sets the fields req names on st and leaves the others as stored.
func applyRunnerSettingsRequest(st *types.RunnerSettings, req types.RunnerSettingsRequest) {
	if req.Enabled != nil {
		st.Enabled = *req.Enabled
	}
	if req.RequireManagedDevice != nil {
		st.RequireManagedDevice = *req.RequireManagedDevice
	}
	if req.MDMDeviceCAPEM != nil {
		st.MDMDeviceCAPEM = *req.MDMDeviceCAPEM
	}
	if req.MDMDeviceCRLURL != nil {
		st.MDMDeviceCRLURL = *req.MDMDeviceCRLURL
	}
}

// auditMDMDeviceSettings records each MDM field req names. The CA is audited as the sorted SHA-256
// fingerprints of its certificates, never the PEM; the URL as itself.
func (s *Server) auditMDMDeviceSettings(r *http.Request, req types.RunnerSettingsRequest, before, after types.RunnerSettings) {
	audit := func(target string, was, now any) {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			placement.ActionRunnersMDMDeviceSet, target, "success", mustJSON(map[string]any{"before": was, "after": now})))
	}
	if req.MDMDeviceCAPEM != nil {
		audit("runners.mdm_device_ca_pem", mdmBundleFingerprints(before.MDMDeviceCAPEM), mdmBundleFingerprints(after.MDMDeviceCAPEM))
	}
	if req.MDMDeviceCRLURL != nil {
		audit("runners.mdm_device_crl_url", before.MDMDeviceCRLURL, after.MDMDeviceCRLURL)
	}
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
	applyRunnerSettingsRequest(&after, req)
	if msg := validateMDMDeviceSettings(after); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerMDMDeviceInvalid, msg)
		return before, after, false
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
	s.auditMDMDeviceSettings(r, req, before, after)
	return before, after, true
}
