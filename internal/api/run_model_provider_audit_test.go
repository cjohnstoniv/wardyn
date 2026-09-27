// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// deniedRows is srv's authz.denied rows, decoded.
func deniedRows(t *testing.T, srv *Server) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
		if ev.Action != authz.AuditAction {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		d["_target"] = ev.Target
		out = append(out, d)
	}
	return out
}

// TestRunModelProviderDoors_AuditEachRefusal is #987: every model-provider
// refusal at create and at Review writes exactly one authz.denied row, while
// the 422 keeps its envelope (provider, kind and the credential-only reason).
// A provider the member is not granted stays the one capability row it was,
// and a field-validation refusal writes none.
func TestRunModelProviderDoors_AuditEachRefusal(t *testing.T) {
	twoKeys := types.SiteConfig{ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"), keyProvider("corp", "claude-code"))}
	disabledDefault := types.SiteConfig{
		ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"), func() types.ModelProvider {
			p := keyProvider("corp", "claude-code")
			p.Disabled = true
			return p
		}()),
		AgentProviders: agentBlock(types.AgentProvider{ID: "claude-code", DefaultProvider: "corp"}),
	}
	codexOnly := types.SiteConfig{ModelProviders: providerBlock(keyProvider("codex", "codex-cli"))}
	envGrant, _ := json.Marshal(map[string]any{"agent": "claude-code", "task": "t", "inline_policy": map[string]any{
		"min_confinement_class": "CC2", "eligible_grants": []types.GrantSpec{envSecretGrant("ANTHROPIC_API_KEY", "operator-model-key")}}})
	enforced := map[string]bool{capModelProvider: true}

	for _, tc := range []struct {
		name       string
		site       types.SiteConfig
		cs         *capStore
		member     bool
		body       string
		wantCode   int
		wantReason string // the one authz.denied row's reason; "" is none
		wantDetail map[string]any
	}{
		// A named human caller, not the admin token: credentialPerson refuses
		// the admin token's "no key" as mpcNoPerson (no sign-in door, since
		// there is no person to sign in), which would drop the remedy this
		// case pins. A member session is a real subject with no stored key.
		{name: "not connected: no key of the caller's", site: twoKeys, cs: &capStore{}, member: true,
			body: `{"agent":"claude-code","task":"t","model_provider":"corp"}`, wantCode: http.StatusUnprocessableEntity,
			wantReason: string(authz.ReasonModelProviderUnavailable),
			wantDetail: map[string]any{"provider": "corp", "kind": string(types.ModelProviderAnthropicAPIKey), "remedy": llmRefusalAuditReason}},
		{name: "off: a disabled default", site: disabledDefault, cs: &capStore{},
			body: `{"agent":"claude-code","task":"t"}`, wantCode: http.StatusUnprocessableEntity,
			wantReason: string(authz.ReasonModelProviderUnavailable),
			wantDetail: map[string]any{"provider": "corp", "kind": string(types.ModelProviderAnthropicAPIKey)}},
		{name: "not available: a provider that does not serve the agent", site: codexOnly, cs: &capStore{},
			body: `{"agent":"claude-code","task":"t","model_provider":"codex"}`, wantCode: http.StatusUnprocessableEntity,
			wantReason: string(authz.ReasonModelProviderUnavailable),
			wantDetail: map[string]any{"provider": "codex", "kind": string(types.ModelProviderAnthropicAPIKey)}},
		{name: "missing: no provider by that name", site: twoKeys, cs: &capStore{},
			body: `{"agent":"claude-code","task":"t","model_provider":"nope"}`, wantCode: http.StatusUnprocessableEntity,
			wantReason: string(authz.ReasonModelProviderUnavailable), wantDetail: map[string]any{"provider": "nope"}},
		{name: "none chosen among several", site: twoKeys, cs: &capStore{},
			body: `{"agent":"claude-code","task":"t"}`, wantCode: http.StatusUnprocessableEntity,
			wantReason: string(authz.ReasonModelProviderUnavailable), wantDetail: map[string]any{}},
		{name: "a policy grant that would set a model credential", site: codexOnly, cs: &capStore{},
			body: string(envGrant), wantCode: http.StatusUnprocessableEntity,
			wantReason: string(authz.ReasonModelProviderUnavailable), wantDetail: map[string]any{}},
		{name: "not granted: the capability row alone", site: twoKeys, cs: &capStore{enf: enforced}, member: true,
			body: `{"agent":"claude-code","task":"t","model_provider":"corp"}`, wantCode: http.StatusForbidden,
			wantReason: string(authz.ReasonCapabilityModelProvider)},
		{name: "a field-validation refusal writes none", site: twoKeys, cs: &capStore{},
			body: `{"agent":"claude-code","task":"t","model_provider":"Corp Gateway"}`, wantCode: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/api/v1/runs/preflight", "/api/v1/runs"} {
				srv := providerRunFixture(t, tc.site, tc.cs, nil)
				// The admin API token is an authorised caller for every case
				// that just needs one to pass the door (#639: an SSO session
				// of admin tier in the Admin view now gets 409 admin_view on
				// a launch door, so an SSO admin session can no longer stand
				// in for "any authorised caller" here). The one case testing
				// the capability-denied 403 needs a real member subject, not
				// the operator-exempt token.
				var w *httptest.ResponseRecorder
				if tc.member {
					w = doSSO(t, srv, http.MethodPost, path, govSession(t, govMemberSub, []string{"eng"}, false), tc.body)
				} else {
					w = do(t, srv, http.MethodPost, path, adminToken, tc.body)
				}
				if w.Code != tc.wantCode {
					t.Fatalf("%s = %d %s, want %d", path, w.Code, w.Body.String(), tc.wantCode)
				}
				rows := deniedRows(t, srv)
				if tc.wantReason == "" {
					if len(rows) != 0 {
						t.Errorf("%s: authz.denied rows = %v, want none", path, rows)
					}
					continue
				}
				if len(rows) != 1 || rows[0]["reason"] != tc.wantReason || rows[0]["_target"] != "runs.model_provider" {
					t.Fatalf("%s: authz.denied rows = %v, want exactly one %s at runs.model_provider", path, rows, tc.wantReason)
				}
				if tc.wantDetail == nil {
					continue
				}
				for _, k := range []string{"provider", "kind", "remedy"} {
					if fmt.Sprint(rows[0][k]) != fmt.Sprint(tc.wantDetail[k]) && !(rows[0][k] == nil && tc.wantDetail[k] == nil) {
						t.Errorf("%s: row %s = %v, want %v", path, k, rows[0][k], tc.wantDetail[k])
					}
				}
				var body errorBody
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if body.Provider != fmt.Sprint(orEmpty(tc.wantDetail["provider"])) || body.Kind != fmt.Sprint(orEmpty(tc.wantDetail["kind"])) ||
					body.Reason != fmt.Sprint(orEmpty(tc.wantDetail["remedy"])) {
					t.Errorf("%s: 422 envelope provider/kind/reason = %q/%q/%q, want the row's %v", path, body.Provider, body.Kind, body.Reason, tc.wantDetail)
				}
			}
		})
	}
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}
