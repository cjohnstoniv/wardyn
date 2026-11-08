// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// requireManagedDevice runs a pooled local create-time judgement with the organisation's requirement on or
// off and the given classes among the pool's runners.
func requireManagedDevice(t *testing.T, require bool, trusts ...types.RunnerTrustClass) (localJudgement, *runRefusal) {
	t.Helper()
	f := newComponentFixture(t)
	f.srv.cfg.Store = localPlanTestStore{f.st}
	f.st.siteConfig = types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true, RequireManagedDevice: require}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	return f.srv.localPlacementRefusal(r, createRunRequest{Placement: placement.Local}, true, trusts, nil, types.RunPolicySpec{}, governanceCeiling{}, runComponents{}, nil, nil, composer.Baseline{})
}

// The placement door: with runners.require_managed_device on, a run whose pool holds only personal-trust
// runners is refused runner_trust_required (422); a managed_device runner among them takes it and the
// run is admitted at that class; off (the default), the personal runner takes it.
func TestRequireManagedDevice_RefusesPersonalTrustPlacement(t *testing.T) {
	_, ref := requireManagedDevice(t, true, types.RunnerTrustPersonal)
	if ref == nil || ref.body.Reason != "runner_trust_required" || ref.status != http.StatusUnprocessableEntity {
		t.Fatalf("personal only, required: %+v, want runner_trust_required 422", ref)
	}
	j, ref := requireManagedDevice(t, true, types.RunnerTrustPersonal, types.RunnerTrustManagedDevice)
	if ref != nil || j.trust != types.RunnerTrustManagedDevice {
		t.Errorf("personal and managed_device, required: %+v trust %q, want admitted at managed_device", ref, j.trust)
	}
	j, ref = requireManagedDevice(t, false, types.RunnerTrustPersonal)
	if ref != nil || j.trust != types.RunnerTrustPersonal {
		t.Errorf("personal only, not required: %+v trust %q, want admitted at personal", ref, j.trust)
	}
}

func TestPersonalRunnerRefusedWhenRequired(t *testing.T) {
	_, ref := requireManagedDevice(t, true, types.RunnerTrustPersonal)
	if ref == nil || !strings.Contains(ref.body.Error, "not marked as company-managed") {
		t.Fatalf("refusal %+v, want it to say the runner is not marked as company-managed", ref)
	}
}

// PUT /runners/settings sets the requirement without touching enabled, and enabled without touching the
// requirement; the answer and the audit row carry both; a body naming neither changes nothing.
func TestRunnerSettingsRequireManagedDeviceRoundTrip(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	f.siteCfg.Runners = &types.RunnerSettings{Enabled: true}
	srv.cfg.ControlPlaneURL = "https://wardynd.internal:8443"
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	put := func(body string) *httptest.ResponseRecorder {
		return doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, body)
	}
	if w := put(`{"require_managed_device":true}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"require_managed_device":true`) || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("turn the requirement on: %d %s", w.Code, w.Body.String())
	}
	if rs := f.siteCfg.Runners; rs == nil || !rs.Enabled || !rs.RequireManagedDevice {
		t.Fatalf("stored %+v, want enabled and required", rs)
	}
	ev := lastAuditEvent(t, h.audit.snapshot(), "runners.enabled.set")
	var data struct{ Before, After bool }
	if err := json.Unmarshal(ev.Data, &data); err != nil || data.Before || !data.After || ev.Target != "runners.require_managed_device" {
		t.Fatalf("audit %+v data %s", ev, ev.Data)
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/settings", admin, ""); !strings.Contains(w.Body.String(), `"require_managed_device":true`) {
		t.Fatalf("settings read: %s", w.Body.String())
	}
	if w := put(`{"enabled":true}`); w.Code != http.StatusOK || !f.siteCfg.Runners.RequireManagedDevice {
		t.Fatalf("an enabled-only write cleared the requirement: %d %+v", w.Code, f.siteCfg.Runners)
	}
	if w := put(`{}`); w.Code != http.StatusBadRequest || !f.siteCfg.Runners.RequireManagedDevice {
		t.Fatalf("an empty body: %d %+v", w.Code, f.siteCfg.Runners)
	}
	if w := put(`{"require_managed_device":false}`); w.Code != http.StatusOK || f.siteCfg.Runners.RequireManagedDevice || !f.siteCfg.Runners.Enabled {
		t.Fatalf("turn the requirement off: %d %+v", w.Code, f.siteCfg.Runners)
	}
}
