// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A service identity cannot sign in, so on a sign-in kind its model_credential
// refusal names that instead of the console door; a person's sentence and a
// typed kind's are unchanged. Every sentence here is written out in full, not
// built from the constants, so a reworded remedy fails.

const (
	svcRefusalPrincipal = "service:ci-bot"
	svcRefusalRemedy    = "a service identity cannot sign in, so choose a model provider whose credential it stores as a key or token."
	humanRefusalRemedy  = "connect it from Getting started in the console, or from the banner the console shows on every page."
)

func svcRefusalText(id, state, remedy string) string {
	return "This run's model provider is " + id + ", and " + state + " — " + remedy + " Wardyn does not substitute a different model provider."
}

// svcRefusalCreate drives path as a service identity through the real create handler.
func svcRefusalCreate(t *testing.T, p types.ModelProvider, path string) (int, errorBody) {
	t.Helper()
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(p)}, &capStore{}, nil)
	srv.cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
	srv.cfg.Runner = nil
	body := `{"agent":"claude-code","task":"t","model_provider":"` + p.ID + `"}`
	ctx := withServiceIdentity(withHumanIdentity(context.Background(), svcRefusalPrincipal, "", oidc.RoleUser, types.UserTypeStandard, []string{}, false), "ci-bot", "sponsor@corp.example")
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	if path == "/api/v1/runs/preflight" {
		srv.handlePreflightRun(w, r)
	} else {
		srv.handleCreateRun(w, r)
	}
	var got errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got
}

// svcRefusalDispatch runs the real dispatch LLM phase for a run owned by owner on p, with no credential of
// owner's stored, and returns the failure hint and the run.create failure row's data.
func svcRefusalDispatch(t *testing.T, p types.ModelProvider, owner string) (hint string, row map[string]any) {
	t.Helper()
	h, st, sec := subHarness(t, p)
	h.srv.cfg.AWSSSOProxyInject = true
	st.run.CreatedBy = owner
	for _, part := range []string{providerKeyPart, providerOAuthPart, providerSSOPart} {
		_ = sec.For(subOwner).Delete(context.Background(), providerSecretName(p.UID, part))
	}
	if _, ok := dispatchSub(h, st, &types.RunPolicySpec{}, map[string]string{}, nil); ok {
		t.Fatal("dispatch admitted a run whose owner holds no credential")
	}
	for _, ev := range h.srv.cfg.Audit.(*recRecorder).snapshot() {
		if ev.Action == "run.create" && ev.Outcome == "failure" {
			if err := json.Unmarshal(ev.Data, &row); err != nil {
				t.Fatal(err)
			}
		}
	}
	if row == nil {
		t.Fatal("no run.create failure row was written")
	}
	return st.failed, row
}

func TestServiceSignInProviderRefusal(t *testing.T) {
	setAzureGate(t, true)
	azure := azureProvider()
	azure.UID = uuid.NewString()
	for _, k := range []struct {
		name  string
		p     types.ModelProvider
		state string
	}{
		{"anthropic_subscription", subProvider("claude"), "you are not signed in to Claude for it"},
		{"bedrock_sso", brSSOProvider(), "you are not signed in to AWS for it"},
		{"azure_foundry", azure, "you are not signed in to Azure for it"},
	} {
		t.Run(k.name, func(t *testing.T) {
			want := svcRefusalText(k.p.ID, k.state, svcRefusalRemedy)
			for _, path := range []string{"/api/v1/runs/preflight", "/api/v1/runs"} {
				code, got := svcRefusalCreate(t, k.p, path)
				wantBody := errorBody{Error: want, Provider: k.p.ID, Kind: string(k.p.Kind), Reason: llmRefusalAuditReason}
				if code != http.StatusUnprocessableEntity || got != wantBody {
					t.Errorf("%s as a service = %d %+v\nwant 422 %+v", path, code, got, wantBody)
				}
			}
			hint, row := svcRefusalDispatch(t, k.p, svcRefusalPrincipal)
			if hint != want || row["error"] != want || row["reason"] != llmRefusalAuditReason || row["provider"] != k.p.ID {
				t.Errorf("dispatch hint %q, run.create row %v\nwant %q with reason %s", hint, row, want, llmRefusalAuditReason)
			}
			// A person on the same provider keeps the console sentence, byte for byte.
			wantHuman := svcRefusalText(k.p.ID, k.state, humanRefusalRemedy)
			hint, row = svcRefusalDispatch(t, k.p, subOwner)
			if hint != wantHuman || row["error"] != wantHuman || row["reason"] != llmRefusalAuditReason {
				t.Errorf("a person's dispatch hint %q, row %v\nwant %q", hint, row, wantHuman)
			}
		})
	}
}

// TestServiceSignInProviderRefusal_TypedKindsKeepTheConsoleRemedy: the clause is for the sign-in kinds only. A
// service on a key or token kind with no stored credential is told what it is told today.
func TestServiceSignInProviderRefusal_TypedKindsKeepTheConsoleRemedy(t *testing.T) {
	bearer, key := brBearerProvider(), keyProvider("anthropic", "claude-code")
	key.UID = "uid-key"
	for _, k := range []struct {
		name  string
		p     types.ModelProvider
		state string
	}{
		{"bedrock_bearer", bearer, mpRunNoKey},
		{"anthropic_api_key", key, mpRunNoKey},
	} {
		t.Run(k.name, func(t *testing.T) {
			want := svcRefusalText(k.p.ID, k.state, humanRefusalRemedy)
			code, got := svcRefusalCreate(t, k.p, "/api/v1/runs")
			wantBody := errorBody{Error: want, Provider: k.p.ID, Kind: string(k.p.Kind), Reason: llmRefusalAuditReason}
			if code != http.StatusUnprocessableEntity || got != wantBody {
				t.Errorf("create as a service = %d %+v\nwant 422 %+v", code, got, wantBody)
			}
			if hint, _ := svcRefusalDispatch(t, k.p, svcRefusalPrincipal); hint != want {
				t.Errorf("dispatch hint %q, want %q", hint, want)
			}
		})
	}
}
