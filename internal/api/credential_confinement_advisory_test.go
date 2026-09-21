// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCredentialConfinementAdvisory is the pure function's own table (#150):
// it fires ONLY when a stored AWS SSO credential is actually delivered AND the
// enforced class is weaker than CC3 — never a refusal, just the sentence or "".
func TestCredentialConfinementAdvisory(t *testing.T) {
	cases := []struct {
		name         string
		ssoDelivered bool
		enforced     types.ConfinementClass
		wantAdvisory bool
	}{
		{"no SSO-delivered credential, CC1: silent", false, types.CC1, false},
		{"SSO-delivered credential, CC1: fires", true, types.CC1, true},
		{"SSO-delivered credential, CC2: fires", true, types.CC2, true},
		{"SSO-delivered credential, CC3: meets the floor, silent", true, types.CC3, false},
		{"no SSO-delivered credential, CC3: silent", false, types.CC3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := credentialConfinementAdvisory(types.RunPolicySpec{}, tc.enforced, tc.ssoDelivered)
			if (got != "") != tc.wantAdvisory {
				t.Fatalf("credentialConfinementAdvisory(_, %s, %v) = %q, want advisory=%v",
					tc.enforced, tc.ssoDelivered, got, tc.wantAdvisory)
			}
			if tc.wantAdvisory && !strings.Contains(got, string(tc.enforced)) {
				t.Errorf("advisory %q does not name the enforced class %s", got, tc.enforced)
			}
		})
	}
}

// ssoConfinementRosterSrv builds a pg-backed server that can only ever offer
// CC1, with a "shared" bedrock_sso roster row and a live (not expiring soon)
// captured AWS SSO credential already stored — the exact shape #150 exists
// for: a deployment that offers only the weakest confinement class still
// delivers a stored AWS identity to the sandbox at dispatch.
func ssoConfinementRosterSrv(t *testing.T) *Server {
	t.Helper()
	fr := &fakeRunner{capsClasses: []types.ConfinementClass{types.CC1}}
	srv, _ := pgHarnessWithRunner(t, fr)
	srv.cfg.DefaultPolicy.MinConfinementClass = types.CC1
	srv.cfg.BedrockRegion = "us-east-1"
	srv.cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	srv.cfg.MaskRegistry = secretmask.NewRegistry()
	// A fixed clock, one hour ahead of the stored blob's captured time and one
	// hour before it expires: renewable() is true and needsRefresh() is false,
	// so the create-time refresh pass (refresh=true) is a no-op on the wire and
	// this test needs no fake OIDC endpoint.
	srv.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	putAWSSSOBlob(t, srv, awsSSOTestFixedNow.Add(time.Hour))

	ctx := context.Background()
	if _, err := srv.cfg.Store.PutSiteConfig(ctx, types.SiteConfig{
		AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{
			{ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO},
		}},
	}); err != nil {
		t.Fatalf("seed agent roster: %v", err)
	}
	// site_config is a store-wide singleton row: restore it so a value seeded
	// here does not leak into another test sharing WARDYN_TEST_PG.
	t.Cleanup(func() {
		if _, err := srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{}); err != nil {
			t.Errorf("restore site config: %v", err)
		}
	})
	return srv
}

// TestCreateRun_CC1OnlyHostStoredSSOBlobConfinementAdvisory is #150's acceptance case: a
// deployment that can only ever offer CC1 still launches a run whose model
// credential is a stored AWS SSO session — 201, never a 422 — and the
// response carries the advisory that says so. Adding a refusal here would
// break every single-class deployment, which is the whole point of
// WARN-never-refuse.
func TestCreateRun_CC1OnlyHostStoredSSOBlobConfinementAdvisory(t *testing.T) {
	srv := ssoConfinementRosterSrv(t)
	body := `{"agent":"claude-code","repo":"acme/widgets","task":"ship it"}`

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (WARN, never refuse); body=%s", w.Code, w.Body.String())
	}
	var resp createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if resp.ConfinementClass != types.CC1 {
		t.Fatalf("confinement_class = %q, want CC1 (the only class this host offers)", resp.ConfinementClass)
	}
	if !warningsNameConfinementAdvisory(resp.Warnings) {
		t.Errorf("201 warnings = %v, want the credential-confinement advisory", resp.Warnings)
	}

	rec, ok := srv.cfg.Audit.(*recRecorder)
	if !ok {
		t.Fatal("srv.cfg.Audit is not a *recRecorder")
	}
	ev := lastAuditEvent(t, rec.events, "run.create")
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode run.create audit data: %v", err)
	}
	if data["credential_confinement"] != credentialConfinementBelowFloor {
		t.Errorf("run.create audit credential_confinement = %v, want %q", data["credential_confinement"], credentialConfinementBelowFloor)
	}
}

// TestPreflightAndCreateAgreeOnCredentialConfinementAdvisory is the
// behavioral twin of TestPreflightAndCreateAgreeOnConfinementDefault, for
// #150's own parity requirement: preflight and the real launch must carry the
// BYTE-IDENTICAL advisory (or both carry none) for the same body, because both
// call credentialConfinementAdvisory off the same resolved
// modelCredentialFacts.Mechanism.
func TestPreflightAndCreateAgreeOnCredentialConfinementAdvisory(t *testing.T) {
	srv := ssoConfinementRosterSrv(t)
	body := `{"agent":"claude-code","repo":"acme/widgets","task":"ship it"}`

	pre := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if pre.Code != http.StatusOK {
		t.Fatalf("preflight: code = %d, want 200; body=%s", pre.Code, pre.Body.String())
	}
	var pf preflightResponse
	if err := json.Unmarshal(pre.Body.Bytes(), &pf); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}

	create := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if create.Code != http.StatusCreated {
		t.Fatalf("create: code = %d, want 201; body=%s", create.Code, create.Body.String())
	}
	var resp createRunResponse
	if err := json.Unmarshal(create.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	preAdvisory := advisoryAmong(pf.Warnings)
	createAdvisory := advisoryAmong(resp.Warnings)
	if preAdvisory == "" || createAdvisory == "" {
		t.Fatalf("preflight warnings = %v, create warnings = %v — want both to carry the credential-confinement advisory",
			pf.Warnings, resp.Warnings)
	}
	if preAdvisory != createAdvisory {
		t.Errorf("preflight advisory (%q) and create advisory (%q) disagree", preAdvisory, createAdvisory)
	}
}

// warningsNameConfinementAdvisory / advisoryAmong scan a warnings slice for
// the credential-confinement sentence without hardcoding its exact frozen
// text a second time (awssso_pin.go carries the one copy) — they key off the
// substring every instance of the sentence must carry regardless of the
// enforced class it names.
func warningsNameConfinementAdvisory(warnings []string) bool {
	return advisoryAmong(warnings) != ""
}

func advisoryAmong(warnings []string) string {
	for _, w := range warnings {
		if strings.Contains(w, "stored AWS SSO session") {
			return w
		}
	}
	return ""
}
