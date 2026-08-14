// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// integration_probe_test.go covers B2's three new behaviors: the base-component
// runtime fold's ADO-shaped behavior proof, the "Test" probe endpoint, and the
// explicit-adoption gate on the write path.

// adoIntegration is the ADO-shaped generic connection the contract's behavior
// proof names: ONE secret (a pat, proxy-header delivered) and ONE egress host.
// Nothing else — that is the whole point of a generic kind.
func adoIntegration() types.Integration {
	return types.Integration{
		ID: "ado-rest", Name: "Azure DevOps REST API",
		Kind:   "azure_devops",
		Egress: []string{"dev.azure.com"},
		Secrets: []types.IntegrationSecret{{Role: "pat", SecretName: "ado-pat",
			Delivery: &types.IntegrationDelivery{Mode: types.DeliveryProxyHeader,
				Header: "Authorization", Format: "Basic %s"}}},
		Probe: &types.IntegrationProbe{Method: "GET", URL: "https://dev.azure.com/_apis/projects"},
	}
}

// TestIntegrationFold_ADOGenericGetsExactlyItsPatAndHost is the contract's
// behavior proof: a run whose workspace names an ADO-style generic integration
// gets EXACTLY {a pat grant delivered by its declared proxy header, plus
// dev.azure.com egress} — no other host, no other grant, and nothing that
// depends on the ROLE being called "pat" rather than "token".
func TestIntegrationFold_ADOGenericGetsExactlyItsPatAndHost(t *testing.T) {
	srv := runIntegrationSrv(t, []types.Integration{adoIntegration()},
		map[string][]byte{"ado-pat": []byte("pat-value")})
	spec := &types.RunPolicySpec{}
	srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code",
		wsRequiring(uuid.New(), "integration:ado-rest", "required"), nil)

	if !slices.Equal(spec.AllowedDomains, []string{"dev.azure.com"}) {
		t.Errorf("AllowedDomains = %v, want exactly [dev.azure.com]", spec.AllowedDomains)
	}
	scopes := apiKeyScopes(t, spec)
	if len(scopes) != 1 {
		t.Fatalf("api_key grants = %+v, want exactly one", scopes)
	}
	want := map[string]string{
		"host": "dev.azure.com", "header": "Authorization", "format": "Basic %s", "secret_name": "ado-pat",
	}
	for k, v := range want {
		if scopes[0][k] != v {
			t.Errorf("grant scope %s = %q, want %q (full scope %+v)", k, scopes[0][k], v, scopes[0])
		}
	}
	if len(spec.WorkspaceMounts) != 0 || len(spec.EligibleGrants) != 1 {
		t.Errorf("the fold added something beyond the one grant: mounts=%+v grants=%+v", spec.WorkspaceMounts, spec.EligibleGrants)
	}
}

// TestIntegrationFold_RoleAgnosticDelivery pins the DECISION behind B1's
// widened HeaderSecret: what makes a secret injectable is its DELIVERY, never
// its role name. A row calling its secret "pat"/"api_key"/anything injects the
// same way; a row whose secret declares NO delivery (a closed kind's bespoke
// transport — the github_app halves, git_host's clone credentials) injects
// nothing, whatever it is called.
func TestIntegrationFold_RoleAgnosticDelivery(t *testing.T) {
	fold := func(t *testing.T, integ types.Integration) []map[string]string {
		t.Helper()
		srv := runIntegrationSrv(t, []types.Integration{integ}, map[string][]byte{"the-secret": []byte("v")})
		spec := &types.RunPolicySpec{}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code",
			wsRequiring(uuid.New(), "integration:"+integ.ID, "required"), nil)
		return apiKeyScopes(t, spec)
	}
	base := types.Integration{ID: "role-test", Kind: "acme", Egress: []string{"acme.corp.internal"}}
	header := &types.IntegrationDelivery{Mode: types.DeliveryProxyHeader, Header: "X-Acme"}

	for _, role := range []string{"token", "pat", "api_key", "whatever_the_operator_typed"} {
		t.Run("role "+role+" injects", func(t *testing.T) {
			integ := base
			integ.Secrets = []types.IntegrationSecret{{Role: role, SecretName: "the-secret", Delivery: header}}
			scopes := fold(t, integ)
			if len(scopes) != 1 || scopes[0]["header"] != "X-Acme" {
				t.Fatalf("scopes = %+v, want one X-Acme injection — delivery decides, not the role", scopes)
			}
		})
	}
	t.Run("no declared delivery injects nothing", func(t *testing.T) {
		integ := base
		integ.Secrets = []types.IntegrationSecret{{Role: "pat", SecretName: "the-secret"}}
		if scopes := fold(t, integ); len(scopes) != 0 {
			t.Errorf("scopes = %+v, want none — a bespoke kind transport is not this generic lane's to guess", scopes)
		}
	})
}

// ─── POST /integrations/{id}/test ────────────────────────────────────────────

// integrationProbeHarness is newProbeHarness with `integs` stored and their
// secrets available, so the probe folds a real credential.
func integrationProbeHarness(t *testing.T, integs []types.Integration, secrets map[string][]byte, fr *probeFakeRunner) (*Server, *probeStore) {
	t.Helper()
	srv, ps := newProbeHarness(t, types.SiteConfig{Integrations: integs}, fr)
	srv.cfg.Secrets = &memSecrets{m: secrets}
	return srv, ps
}

// TestHandleTestIntegration_TraversesTheRunsOwnEgressAndInjection is the whole
// claim of the button: the probe run carries the integration's OWN egress and
// its OWN proxy-side credential grant — the same fold a granted run gets — so a
// green chip means the path was really walked, credential and all.
func TestHandleTestIntegration_TraversesTheRunsOwnEgressAndInjection(t *testing.T) {
	srv, ps := integrationProbeHarness(t, []types.Integration{adoIntegration()},
		map[string][]byte{"ado-pat": []byte("pat-value")}, &probeFakeRunner{exitCode: 0})

	w := do(t, srv, http.MethodPost, "/api/v1/integrations/ado-rest/test", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.IntegrationProbeStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (body %q)", err, w.Body.String())
	}
	if got.State != "passed" || got.CheckedAt.IsZero() {
		t.Errorf("status = %+v, want passed with a checked_at", got)
	}
	if !strings.Contains(got.Detail, "dev.azure.com/_apis/projects") {
		t.Errorf("detail = %q, want it to name what was actually probed", got.Detail)
	}
	grants := ps.grantSpecs()
	if len(grants) != 1 {
		t.Fatalf("persisted grants = %d, want exactly the integration's own injection grant", len(grants))
	}
	if h := apiKeyGrantScopeHost(grants[0].Spec.Scope); h != "dev.azure.com" {
		t.Errorf("grant host = %q, want dev.azure.com", h)
	}
	if n := len(ps.actionEvents("integration.test")); n != 1 {
		t.Errorf("integration.test audit events = %d, want 1", n)
	}
}

// A failed probe is a real, cached answer — not an error, and never a silent
// "passed" because the transport worked.
func TestHandleTestIntegration_FailedIsCachedAndServedOnTheRow(t *testing.T) {
	srv, _ := integrationProbeHarness(t, []types.Integration{adoIntegration()},
		map[string][]byte{"ado-pat": []byte("pat-value")}, &probeFakeRunner{exitCode: 7})

	w := do(t, srv, http.MethodPost, "/api/v1/integrations/ado-rest/test", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (a failed probe is still a definite answer); body=%s", w.Code, w.Body.String())
	}
	var got types.IntegrationProbeStatus
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.State != "failed" || !strings.Contains(got.Detail, "connection refused") {
		t.Errorf("status = %+v, want failed naming the real curl cause", got)
	}

	// And the cached verdict rides the row every reader already fetches.
	list := do(t, srv, http.MethodGet, "/api/v1/integrations", adminToken, "")
	var body struct {
		Integrations []SetupIntegration `json:"integrations"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, row := range body.Integrations {
		if row.ID != "ado-rest" {
			continue
		}
		if row.ProbeStatus == nil || row.ProbeStatus.State != "failed" {
			t.Fatalf("listed row probe_status = %+v, want the cached failed verdict", row.ProbeStatus)
		}
		return
	}
	t.Fatal("ado-rest missing from GET /integrations")
}

// An untested row reports NOTHING rather than a green default: not_tested is
// the absence of a result, and after a restart that is the honest state.
func TestHandleTestIntegration_UntestedRowCarriesNoStatus(t *testing.T) {
	srv, _ := integrationProbeHarness(t, []types.Integration{adoIntegration()}, nil, &probeFakeRunner{})
	if st := srv.probeStatus("ado-rest"); st != nil {
		t.Errorf("probeStatus = %+v, want nil — nothing has been tested yet", st)
	}
}

func TestHandleTestIntegration_Refusals(t *testing.T) {
	noProbe := adoIntegration()
	noProbe.ID, noProbe.Probe = "no-probe", nil
	offEgress := adoIntegration()
	offEgress.ID = "off-egress"
	offEgress.Probe = &types.IntegrationProbe{Method: "GET", URL: "https://status.example.com/health"}
	off := adoIntegration()
	off.ID, off.Disabled = "switched-off", true

	srv, ps := integrationProbeHarness(t, []types.Integration{noProbe, offEgress, off},
		map[string][]byte{"ado-pat": []byte("p")}, &probeFakeRunner{exitCode: 0})

	cases := []struct {
		name, id string
		want     int
	}{
		{"unknown id", "nope", http.StatusNotFound},
		{"row configures no probe", "no-probe", http.StatusBadRequest},
		// The probe must traverse what a RUN traverses: a target outside the
		// row's own egress is unreachable for a granted run too, so widening the
		// allowlist just for the probe would make the chip lie.
		{"probe url outside the row's own egress", "off-egress", http.StatusBadRequest},
		// A disabled row grants a run nothing, so there is no path to probe —
		// say that instead of spending a sandbox to discover it.
		{"disabled row", "switched-off", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, srv, http.MethodPost, "/api/v1/integrations/"+c.id+"/test", adminToken, "")
			if w.Code != c.want {
				t.Errorf("code = %d, want %d; body=%s", w.Code, c.want, w.Body.String())
			}
		})
	}
	if ps.runCount() != 0 {
		t.Errorf("runCount = %d, want 0 — every refusal is decided before a sandbox is spent", ps.runCount())
	}
}

// A headless control plane has nothing to launch a probe with: an honest state,
// not a 5xx (same rule as the two site-config probes).
func TestHandleTestIntegration_NoRunner(t *testing.T) {
	srv, _ := integrationProbeHarness(t, []types.Integration{adoIntegration()},
		map[string][]byte{"ado-pat": []byte("p")}, nil)
	srv.cfg.Runner = nil
	w := do(t, srv, http.MethodPost, "/api/v1/integrations/ado-rest/test", adminToken, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "no_runner") {
		t.Errorf("code = %d body = %s, want 200 no_runner", w.Code, w.Body.String())
	}
}

// ─── adoption is explicit ────────────────────────────────────────────────────

// TestHandlePutIntegration_RefusesToSilentlyAdoptADerivedRow pins the approved
// mock's rule: a PUT to an id that exists only as a DERIVATION 409s and names
// the adopt endpoint. The default-for checkbox was the write that hit this
// every time, quietly freezing a snapshot of live config as a stored row.
func TestHandlePutIntegration_RefusesToSilentlyAdoptADerivedRow(t *testing.T) {
	srv, fake, _ := integrationWriteHarness(t, nil)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-live")}}
	body := `{"name":"Anthropic API key","kind":"anthropic_api_key",` +
		`"secrets":[{"role":"api_key","secret_name":"anthropic-api-key"}],"default_for":["agent_runs"]}`

	w := do(t, srv, http.MethodPut, "/api/v1/integrations/anthropic_api_key", adminToken, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/adopt") {
		t.Errorf("body = %s, want it to name the one promotion path", w.Body.String())
	}
	if len(fake.cfg.Integrations) != 0 {
		t.Fatalf("stored = %+v, want nothing persisted by a refused write", fake.cfg.Integrations)
	}

	// Adopt explicitly, and the SAME PUT now lands.
	if a := do(t, srv, http.MethodPost, "/api/v1/integrations/anthropic_api_key/adopt", adminToken, ""); a.Code != http.StatusOK {
		t.Fatalf("adopt: code = %d, want 200; body=%s", a.Code, a.Body.String())
	}
	if w2 := do(t, srv, http.MethodPut, "/api/v1/integrations/anthropic_api_key", adminToken, body); w2.Code != http.StatusOK {
		t.Fatalf("PUT after adopt: code = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	if len(fake.cfg.Integrations) != 1 || !slices.Contains(fake.cfg.Integrations[0].DefaultFor, "agent_runs") {
		t.Errorf("stored = %+v, want the adopted row now carrying the mark", fake.cfg.Integrations)
	}
}

// A brand-new id is NOT a derived row, so creating one is untouched by the gate.
func TestHandlePutIntegration_NewIDStillCreates(t *testing.T) {
	srv, fake, _ := integrationWriteHarness(t, nil)
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-feed", adminToken,
		`{"name":"Acme","kind":"artifactory","egress":["feed.corp.example"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 1 {
		t.Errorf("stored = %+v, want the new row", fake.cfg.Integrations)
	}
}
