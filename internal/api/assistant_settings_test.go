// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const assistantSettingsPath = "/api/v1/assistant/settings"

func assistantSiteConfig(a *types.AssistantSettings, providers ...types.ModelProvider) types.SiteConfig {
	return types.SiteConfig{Assistant: a, ModelProviders: &types.ModelProviders{Providers: providers}}
}

func assistantETag(t *testing.T, srv *Server, c *http.Cookie) string {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, assistantSettingsPath, c, "")
	if w.Code != http.StatusOK || w.Header().Get("ETag") == "" {
		t.Fatalf("GET = %d %s, ETag %q, want 200 and an ETag", w.Code, w.Body, w.Header().Get("ETag"))
	}
	return w.Header().Get("ETag")
}

func TestAssistantSettingsAreASuperAdminDoor(t *testing.T) {
	srv, fake, h := egressBaselineServer(t, types.SiteConfig{})
	member := ssoSession(t, "sub-m", "m@corp.example", oidc.RoleUser)
	sec := ssoSession(t, "sub-s", "s@corp.example", oidc.RoleSecurityAdmin)
	for _, c := range []*http.Cookie{member, sec} {
		if w := doSSO(t, srv, http.MethodGet, assistantSettingsPath, c, ""); w.Code != http.StatusForbidden {
			t.Errorf("GET = %d, want 403", w.Code)
		}
		if w := doSSOIfMatch(t, srv, http.MethodPut, assistantSettingsPath, c, "*", `{}`); w.Code != http.StatusForbidden {
			t.Errorf("PUT = %d, want 403", w.Code)
		}
	}
	if fake.putSeen != nil || len(govCovAudits(h, assistantSettingsWriteAction)) != 0 {
		t.Fatal("a refused write reached the store or the audit log")
	}

	// With nothing saved every purpose is off, and the lists answer as [] rather than null.
	admin := ssoSession(t, "sub-a", "a@corp.example", oidc.RoleAdmin)
	w := doSSO(t, srv, http.MethodGet, assistantSettingsPath, admin, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"never_sent_kinds":[]`) || !strings.Contains(w.Body.String(), `"providers":[]`) {
		t.Fatalf("GET with nothing saved = %d %s, want the zero block", w.Code, w.Body)
	}
}

// If-Match is required: no header is a 428 and a mismatch a 412, and neither writes anything.
func TestAssistantSettingsPutNeedsIfMatch(t *testing.T) {
	srv, fake, _ := egressBaselineServer(t, types.SiteConfig{})
	admin := ssoSession(t, "sub-a", "a@corp.example", oidc.RoleAdmin)
	wantReason(t, doSSO(t, srv, http.MethodPut, assistantSettingsPath, admin, `{}`), http.StatusPreconditionRequired, reasonAssistantSettingsInvalid)
	wantReason(t, doSSOIfMatch(t, srv, http.MethodPut, assistantSettingsPath, admin, `"stale"`, `{}`), http.StatusPreconditionFailed, reasonAssistantSettingsInvalid)
	if fake.putSeen != nil {
		t.Fatal("a refused write reached the store")
	}
}

// An applied write records one row with the block before and after, and stores the block in the site config.
func TestAssistantSettingsNarrowingWriteIsAuditedOnce(t *testing.T) {
	direct := assistantTestProvider("direct", "uid-direct", types.ModelProviderAnthropicAPIKey, "claude-sonnet-x")
	stored := &types.AssistantSettings{Compose: true, Review: true, NeverSentKinds: []string{"role_mapping"},
		Providers: []types.AssistantProviderMark{assistantTestMark("direct", true, true, "m1", "m2")}}
	srv, fake, h := egressBaselineServer(t, assistantSiteConfig(stored, direct))
	admin := ssoSession(t, "sub-a", "a@corp.example", oidc.RoleAdmin)

	body := `{"compose":true,"review":false,"run_history_counts":false,"never_sent_kinds":["role_mapping","governance_unit"],` +
		`"providers":[{"provider_id":"direct","compose":true,"review":false,"models":["m1"]}]}`
	w := doSSOIfMatch(t, srv, http.MethodPut, assistantSettingsPath, admin, assistantETag(t, srv, admin), body)
	if w.Code != http.StatusOK {
		t.Fatalf("narrowing PUT = %d %s, want 200", w.Code, w.Body)
	}
	got := fake.cfg.Assistant
	if got == nil || got.Review || !got.Compose || !slices.Equal(got.NeverSentKinds, []string{"governance_unit", "role_mapping"}) ||
		len(got.Providers) != 1 || got.Providers[0].Review || !slices.Equal(got.Providers[0].Models, []string{"m1"}) {
		t.Errorf("stored block = %+v, want the narrowed block", got)
	}
	if fake.cfg.ModelProviders == nil || len(fake.cfg.ModelProviders.Providers) != 1 {
		t.Errorf("the write changed the model providers: %+v", fake.cfg.ModelProviders)
	}
	rows := govCovAudits(h, assistantSettingsWriteAction)
	if len(rows) != 1 || rows[0].Target != assistantTargetKey || rows[0].Actor != "sub-a" {
		t.Fatalf("audit rows = %+v, want one by the writer", rows)
	}
	d := govCovAuditData(t, rows[0])
	before, _ := d["before"].(map[string]any)
	after, _ := d["after"].(map[string]any)
	if before["review"] != true || after["review"] != false || before["compose"] != true {
		t.Errorf("audit data = %v, want the block before and after", d)
	}
	if marks, _ := after["providers"].([]any); len(marks) != 1 || !strings.Contains(string(rows[0].Data), `"purposes":["compose"]`) {
		t.Errorf("audit marks = %v in %s, want provider_id, purposes and models", marks, rows[0].Data)
	}
	if len(govCovAudits(h, "site_config.write")) != 0 {
		t.Error("the assistant write is audited on its own action, not site_config.write")
	}

	// The same block again is a 200 and writes no second row.
	fake.putSeen = nil
	again := doSSOIfMatch(t, srv, http.MethodPut, assistantSettingsPath, admin, assistantETag(t, srv, admin), body)
	if again.Code != http.StatusOK || fake.putSeen != nil || len(govCovAudits(h, assistantSettingsWriteAction)) != 1 {
		t.Errorf("identical PUT = %d, wrote %v, %d rows; want 200, no write, still one row", again.Code, fake.putSeen, len(govCovAudits(h, assistantSettingsWriteAction)))
	}
	if again.Header().Get("ETag") != assistantETag(t, srv, admin) {
		t.Error("the answer's ETag is not the one GET gives")
	}
}

// A refused block is a 400 under its own reason and writes nothing.
func TestAssistantSettingsRefusesABlockThatCannotServe(t *testing.T) {
	direct := assistantTestProvider("direct", "uid-direct", types.ModelProviderAnthropicAPIKey, "m")
	gateway := assistantTestProvider("gw", "uid-gw", types.ModelProviderCustomEndpoint, "m")
	gateway.GatewayIdentity = &types.ModelProviderGatewayIdentity{Inspection: types.GatewayInspectionGateway}
	bedrock := assistantTestProvider("bedrock", "uid-b", types.ModelProviderBedrockBearer, "m")
	srv, fake, h := egressBaselineServer(t, assistantSiteConfig(nil, direct, gateway, bedrock))
	admin := ssoSession(t, "sub-a", "a@corp.example", oidc.RoleAdmin)
	etag := assistantETag(t, srv, admin)
	mark := func(id string) string {
		return `{"providers":[{"provider_id":"` + id + `","compose":true,"review":false,"models":[]}]}`
	}
	for _, tc := range []struct{ name, body, reason string }{
		{"defers inspection", mark("gw"), reasonAssistantDefersInspection},
		{"bedrock", mark("bedrock"), reasonAssistantCannotServe},
		{"unknown provider", mark("nope"), reasonAssistantSettingsInvalid},
		{"unknown kind", `{"never_sent_kinds":["no_such_kind"]}`, reasonAssistantSettingsInvalid},
	} {
		wantReason(t, doSSOIfMatch(t, srv, http.MethodPut, assistantSettingsPath, admin, etag, tc.body), http.StatusBadRequest, tc.reason)
		if fake.putSeen != nil || len(govCovAudits(h, assistantSettingsWriteAction)) != 0 {
			t.Fatalf("%s: a refused block reached the store or the audit log", tc.name)
		}
	}
}

// GET leaves out a mark whose provider was deleted, is Bedrock or now defers inspection, and its ETag is the
// stored block's, so a PUT of the answer is judged against what is stored.
func TestAssistantSettingsGetLeavesOutDeadMarks(t *testing.T) {
	direct := assistantTestProvider("direct", "uid-direct", types.ModelProviderAnthropicAPIKey, "m")
	gateway := assistantTestProvider("gw", "uid-gw", types.ModelProviderCustomEndpoint, "m")
	gateway.GatewayIdentity = &types.ModelProviderGatewayIdentity{Inspection: types.GatewayInspectionGateway}
	bedrock := assistantTestProvider("bedrock", "uid-b", types.ModelProviderBedrockBearer, "m")
	stored := &types.AssistantSettings{Compose: true, Providers: []types.AssistantProviderMark{
		assistantTestMark("bedrock", true, false), assistantTestMark("direct", true, false), assistantTestMark("gone", true, false), assistantTestMark("gw", true, false),
	}}
	srv, fake, _ := egressBaselineServer(t, assistantSiteConfig(stored, direct, gateway, bedrock))
	admin := ssoSession(t, "sub-a", "a@corp.example", oidc.RoleAdmin)
	w := doSSO(t, srv, http.MethodGet, assistantSettingsPath, admin, "")
	var got types.AssistantSettings
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Providers) != 1 || got.Providers[0].ProviderID != "direct" {
		t.Fatalf("GET = %d %s (%v), want only the live mark", w.Code, w.Body, err)
	}
	if want := computeETag(assistantState(fake.cfg)); w.Header().Get("ETag") != want {
		t.Errorf("ETag = %s, want the stored block's %s", w.Header().Get("ETag"), want)
	}
	// Sending the answer back drops the dead marks: a removal, which applies at once.
	if w2 := doSSOIfMatch(t, srv, http.MethodPut, assistantSettingsPath, admin, w.Header().Get("ETag"), w.Body.String()); w2.Code != http.StatusOK {
		t.Fatalf("PUT of the GET answer = %d %s, want 200", w2.Code, w2.Body)
	}
	if fake.cfg.Assistant == nil || len(fake.cfg.Assistant.Providers) != 1 {
		t.Errorf("stored marks = %+v, want only the live one", fake.cfg.Assistant)
	}
}

// PUT /site-config never changes the assistant block: a changed one is refused, the stored one, or none, passes.
func TestPutSiteConfigNeverChangesTheAssistantBlock(t *testing.T) {
	stored := &types.AssistantSettings{Compose: true, NeverSentKinds: []string{"role_mapping"}}
	srv, fake, _ := egressBaselineServer(t, types.SiteConfig{Assistant: stored})
	for _, body := range []string{`{}`, `{"assistant":{"compose":true,"review":false,"run_history_counts":false,"never_sent_kinds":["role_mapping"],"providers":[]}}`} {
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body); w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s, want 200", body, w.Code, w.Body)
		}
		if fake.cfg.Assistant == nil || !fake.cfg.Assistant.Compose || !slices.Equal(fake.cfg.Assistant.NeverSentKinds, []string{"role_mapping"}) {
			t.Fatalf("after PUT %s the stored block = %+v, want it as it was", body, fake.cfg.Assistant)
		}
	}
	for _, body := range []string{`{"assistant":{"compose":false}}`, `{"assistant":{"compose":true,"review":true}}`, `{"assistant":{}}`} {
		fake.putSeen = nil
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		if w.Code != http.StatusBadRequest || errorReason(w) != reasonAssistantSettingsViaOwnRoute || !strings.Contains(w.Body.String(), "PUT /assistant/settings") {
			t.Errorf("PUT %s = %d %s, want 400 %s naming PUT /assistant/settings", body, w.Code, w.Body, reasonAssistantSettingsViaOwnRoute)
		}
		if fake.putSeen != nil {
			t.Errorf("refused PUT %s wrote %+v", body, fake.putSeen)
		}
	}
}

// inspectionDeferredFor is the expression gatewayRouteOf holds, for a gateway, a direct and a Bedrock provider.
func TestInspectionDeferredForAgreesWithTheGatewayRoute(t *testing.T) {
	withIdentity := func(p types.ModelProvider, inspection string) types.ModelProvider {
		p.GatewayIdentity = &types.ModelProviderGatewayIdentity{Audience: "wardyn", Inspection: inspection}
		return p
	}
	gateway := withIdentity(types.ModelProvider{ID: "gw", Kind: types.ModelProviderCustomEndpoint, BaseURL: "https://gw.corp.example/v1"}, types.GatewayInspectionGateway)
	direct := withIdentity(types.ModelProvider{ID: "direct", Kind: types.ModelProviderCustomEndpoint, BaseURL: "https://gw.corp.example/v1"}, "")
	bedrock := withIdentity(types.ModelProvider{ID: "bedrock", Kind: types.ModelProviderBedrockBearer, Bedrock: &types.BedrockSettings{BaseURL: "https://bedrock.corp.example"}}, types.GatewayInspectionGateway)
	for name, tc := range map[string]struct {
		p    types.ModelProvider
		want bool
	}{"gateway": {gateway, true}, "direct": {direct, false}, "bedrock": {bedrock, false}} {
		route, ok := gatewayRouteOf(tc.p)
		if !ok {
			t.Fatalf("%s: no gateway route", name)
		}
		if route.InspectionDeferred != tc.want || inspectionDeferredFor(tc.p) != route.InspectionDeferred {
			t.Errorf("%s: route defers %v, inspectionDeferredFor %v, want both %v", name, route.InspectionDeferred, inspectionDeferredFor(tc.p), tc.want)
		}
	}
	if inspectionDeferredFor(types.ModelProvider{Kind: types.ModelProviderCustomEndpoint}) {
		t.Error("a provider with no gateway identity defers inspection")
	}
	// The reasons Validate answers with are the ones the API documents.
	if types.AssistantReasonCannotServe != reasonAssistantCannotServe || types.AssistantReasonDefersInspection != reasonAssistantDefersInspection ||
		types.AssistantReasonSettingsInvalid != reasonAssistantSettingsInvalid {
		t.Error("types and api name an assistant reason differently")
	}
}

// TestMeAssistantIsOpenToAMemberAndKeepsAnOffAssistantQuiet: any signed-in person may ask, an unknown purpose
// is a 400, and with nothing saved the answer is off and names no provider.
func TestMeAssistantIsOpenToAMemberAndKeepsAnOffAssistantQuiet(t *testing.T) {
	srv, _, _ := egressBaselineServer(t, types.SiteConfig{})
	member := ssoSession(t, "sub-m", "m@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/me/assistant?purpose=compose", member, "")
	if w.Code != http.StatusOK {
		t.Fatalf("member GET = %d %s, want 200", w.Code, w.Body)
	}
	var got availabilityAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.State != types.AssistantOff || got.Reason != reasonAssistantOff {
		t.Fatalf("answer = %s (%v), want off", w.Body, err)
	}
	for _, q := range []string{"", "?purpose=", "?purpose=other"} {
		if w := doSSO(t, srv, http.MethodGet, "/api/v1/me/assistant"+q, member, ""); w.Code != http.StatusBadRequest {
			t.Errorf("GET %q = %d, want 400", q, w.Code)
		}
	}
}

// GET /me/assistant asks the same "Available to" grant start does: of two marked providers a person granted
// one is ready on that one, a person granted neither is denied by their unit, and a super admin is exempt.
func TestMeAssistantAppliesTheProviderGrant(t *testing.T) {
	// A local model server with no token needs no credential, so only the grant decides.
	first := assistantTestProvider("first", "uid-first", types.ModelProviderLocalModel, "m1")
	second := assistantTestProvider("second", "uid-second", types.ModelProviderLocalModel, "m2")
	site := assistantSiteConfig(&types.AssistantSettings{Compose: true,
		Providers: []types.AssistantProviderMark{assistantTestMark("first", true, false), assistantTestMark("second", true, false)}}, first, second)
	cs := func() *capStore {
		return &capStore{
			restricted: map[string]map[string]bool{capModelProvider: {"first": true, "second": true}},
			grants: []types.CapabilityGrant{{SubjectType: types.CapabilitySubjectUser, Subject: "sub-second",
				Capability: capModelProvider, Value: "second", Effect: types.CapabilityAllow}},
		}
	}
	for _, tc := range []struct {
		name, sub, role, wantState, wantProvider string
	}{
		{"granted only the second", "sub-second", oidc.RoleUser, types.AssistantReady, "second"},
		{"granted none", "sub-none", oidc.RoleUser, types.AssistantUnitDenies, ""},
		{"a super admin is exempt", "sub-admin", oidc.RoleAdmin, types.AssistantReady, "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelProvidersStatusSrv(t, site, cs())
			w := doSSO(t, srv, http.MethodGet, "/api/v1/me/assistant?purpose=compose", ssoSession(t, tc.sub, tc.sub+"@corp.example", tc.role), "")
			var got availabilityAnswer
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || got.State != tc.wantState || got.ProviderID != tc.wantProvider {
				t.Fatalf("GET = %d %s (%v), want %s on %q", w.Code, w.Body, err, tc.wantState, tc.wantProvider)
			}
		})
	}
}
