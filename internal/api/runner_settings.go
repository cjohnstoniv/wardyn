// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/url"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func runnersEnabled(cfg types.SiteConfig) bool { return cfg.Runners != nil && cfg.Runners.Enabled }

// carryRunners keeps the stored runner settings on a generic site-config write. Enablement has its own
// audited, HTTPS-validated door, as does require_managed_device, so a document naming a different value
// of either is refused rather than quietly ignored; so is a non-empty MDM device CA or CRL URL that differs
// from the stored one. An echo of the stored values, or no mention at all, carries them forward. False:
// answered.
func carryRunners(w http.ResponseWriter, cfg *types.SiteConfig, existing types.SiteConfig) bool {
	if cfg.Runners != nil && cfg.Runners.Enabled != runnersEnabled(existing) {
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigRunnersViaOwnRoute,
			"runners.enabled is a super-admin switch: change it with PUT /runners/settings, not PUT /site-config")
		return false
	}
	if cfg.Runners != nil && cfg.Runners.RequireManagedDevice != requiresManagedDevice(existing) {
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigRunnersViaOwnRoute,
			"runners.require_managed_device is a super-admin switch: change it with PUT /runners/settings, not PUT /site-config")
		return false
	}
	if stored := runnerSettingsOf(existing); cfg.Runners != nil && (mdmFieldDiffers(cfg.Runners.MDMDeviceCAPEM, stored.MDMDeviceCAPEM) ||
		mdmFieldDiffers(cfg.Runners.MDMDeviceCRLURL, stored.MDMDeviceCRLURL)) {
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigRunnersViaOwnRoute,
			"runners.mdm_device_ca_pem and runners.mdm_device_crl_url are super-admin settings: change them with PUT /runners/settings, not PUT /site-config")
		return false
	}
	cfg.Runners = existing.Runners
	return true
}

// mdmFieldDiffers is whether a site-config body names an MDM field with a value other than the stored one.
// An empty field is no mention: a client built before the fields cannot name them, and clearing goes through
// PUT /runners/settings.
func mdmFieldDiffers(body, stored string) bool { return body != "" && body != stored }

// handleGetRunnerSettings is GET /api/v1/runners/settings: whether runners are on, and whether a new local
// run needs a managed device, which the runners page reads while the runner routes themselves answer
// runners_disabled.
func (s *Server) handleGetRunnerSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	writeJSON(w, http.StatusOK, runnerSettingsOf(cfg))
}

// runnersEnableRefusal is why runners cannot be turned on now, or "". The public organisation URL
// that runner identities are bound to must be HTTPS (loopback http for development). The control-plane
// URL must be https with no loopback exception: a runner's runs reach the org only through the relay,
// which serves only on the internal TLS listener (startRelayListener), so an unset or http one leaves
// enabled runners with nothing to connect to.
func (s *Server) runnersEnableRefusal() string {
	if federation.CheckOrgURL(s.cfg.RunnerOrgURL) != nil {
		return "set WARDYN_RUNNER_ORG_URL to this organisation's public HTTPS URL (http is accepted only for localhost)"
	}
	if s.cfg.ControlPlaneURL == "" {
		return "set WARDYN_CONTROL_PLANE_URL to the internal TLS listener; a runner's runs reach it through the runner's relay"
	}
	if u, err := url.Parse(s.cfg.ControlPlaneURL); err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "WARDYN_CONTROL_PLANE_URL is not https://; point it at the internal TLS listener, which a runner's relay needs"
	}
	return ""
}

// handlePutRunnerSettings is PUT /api/v1/runners/settings {enabled, require_managed_device,
// mdm_device_ca_pem, mdm_device_crl_url}: the one door that turns runners on or off, sets the managed-device
// requirement and sets the MDM device CA. SUPER admin only; PUT /site-config cannot reach any of them. A field left out is left as stored. Turning off is never refused.
func (s *Server) handlePutRunnerSettings(w http.ResponseWriter, r *http.Request) {
	var req types.RunnerSettingsRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Enabled == nil && req.RequireManagedDevice == nil && req.MDMDeviceCAPEM == nil && req.MDMDeviceCRLURL == nil {
		writeErrorReason(w, http.StatusBadRequest, reasonRunnersEnabledRequired, "enabled is required: send {\"enabled\": true} or {\"enabled\": false}, or {\"require_managed_device\": true|false}, or {\"mdm_device_ca_pem\": \"...\", \"mdm_device_crl_url\": \"...\"}")
		return
	}
	if req.Enabled != nil && *req.Enabled {
		if msg := s.runnersEnableRefusal(); msg != "" {
			writeErrorReason(w, http.StatusConflict, reasonRunnersOrgURLInvalid, "runners can't be turned on yet: "+msg)
			return
		}
	}
	before, after, ok := s.storeRunnerSettings(w, r, req)
	if !ok {
		return
	}
	if before.Enabled && !after.Enabled && s.cfg.RunnersDisabled != nil {
		// The switch is already stored and audited off: a client hanging up now must not stop the hook.
		s.cfg.RunnersDisabled(context.WithoutCancel(r.Context()))
	}
	writeJSON(w, http.StatusOK, after)
}
