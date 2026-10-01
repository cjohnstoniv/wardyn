// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteReadsStore is an integStore whose GetSiteConfig counts its calls and
// fails from call failFrom on (0 = never).
type siteReadsStore struct {
	*integStore
	calls    atomic.Int32
	failFrom int32
}

func (s *siteReadsStore) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	if n := s.calls.Add(1); s.failFrom > 0 && n >= s.failFrom {
		return types.SiteConfig{}, errors.New("pq: connection refused")
	}
	return s.integStore.GetSiteConfig(ctx)
}

// recordDoor POSTs a record launch for ws and returns the status and body. A
// member cannot reach the route (it is operator-only), so the member rows call
// the handler with the member's identity on the context: the capability
// resolver is what a not-granted refusal asks, and the route gate is not.
func recordDoor(t *testing.T, srv *Server, bearer string, ws *types.Workspace, member bool) (int, string) {
	t.Helper()
	path := "/api/v1/workspaces/" + ws.ID.String() + "/record"
	if !member {
		w := do(t, srv, http.MethodPost, path, bearer, `{"name":"capture"}`)
		return w.Code, w.Body.String()
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"name":"capture"}`))
	rc := chi.NewRouteContext()
	rc.URLParams.Add("id", ws.ID.String())
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rc)
	ctx = withOIDCHuman(ctx, govMemberSub)
	ctx = withOIDCEmail(ctx, govMemberSub+"@corp.example")
	ctx = withOIDCRole(ctx, oidc.RoleUser)
	ctx = withOIDCGroups(ctx, []string{"eng"})
	w := httptest.NewRecorder()
	srv.handleRecordWorkspace(w, r.WithContext(ctx))
	return w.Code, w.Body.String()
}

// TestRecordDoorAnswersTheProviderRefusalAsCreateDoes (#797): one choice, two
// doors, one answer. The record door's refusal is byte-equal to POST /runs's —
// status, body and authz.denied row — and it names no provider the caller is
// not granted.
func TestRecordDoorAnswersTheProviderRefusalAsCreateDoes(t *testing.T) {
	enforced := map[string]bool{capModelProvider: true}
	twoKeys := types.SiteConfig{ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"), keyProvider("corp", "claude-code"))}
	corpOff := keyProvider("corp", "claude-code")
	corpOff.Disabled = true
	sources := []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}}
	pinTo := func(ref string) *types.Workspace {
		return &types.Workspace{ID: uuid.New(), Name: "hello", Status: types.WorkspaceScanned, Sources: sources,
			LLMCred: &types.WorkspaceLLMCred{ProviderRef: ref}}
	}
	plain := &types.Workspace{ID: uuid.New(), Name: "plain", Status: types.WorkspaceScanned, Sources: sources}
	onlyAnthropic := []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capModelProvider, "anthropic", types.CapabilityAllow)}

	for _, tc := range []struct {
		name         string
		site         types.SiteConfig
		cs           *capStore
		ws           *types.Workspace
		member       bool
		want         int
		wantReason   string
		wantProvider string // "" = the body names no provider and no kind
		wantKind     types.ModelProviderKind
	}{
		{name: "the pinned provider is off", ws: pinTo("corp"), cs: &capStore{},
			site: types.SiteConfig{ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"), corpOff)},
			want: http.StatusUnprocessableEntity, wantReason: "model_provider_unavailable",
			wantProvider: "corp", wantKind: types.ModelProviderAnthropicAPIKey},
		{name: "the pin names no provider", ws: pinTo("ghost"), cs: &capStore{}, site: twoKeys,
			want: http.StatusUnprocessableEntity, wantReason: "model_provider_unavailable", wantProvider: "ghost"},
		{name: "the caller has no credential of their own", ws: pinTo("corp"), cs: &capStore{}, site: twoKeys,
			want: http.StatusUnprocessableEntity, wantReason: llmRefusalAuditReason,
			wantProvider: "corp", wantKind: types.ModelProviderAnthropicAPIKey},
		{name: "a pin the caller is not granted", ws: pinTo("corp"), member: true, site: twoKeys,
			cs:   &capStore{enf: enforced, grants: onlyAnthropic},
			want: http.StatusForbidden, wantReason: "capability_model_provider"},
		{name: "nothing granted while one provider serves", ws: plain, member: true,
			site: types.SiteConfig{ModelProviders: providerBlock(keyProvider("corp", "claude-code"))},
			cs:   &capStore{enf: enforced},
			want: http.StatusForbidden, wantReason: "capability_model_provider"},
		{name: "several providers serve and none is chosen", ws: plain, cs: &capStore{}, site: twoKeys,
			want: http.StatusUnprocessableEntity, wantReason: "model_provider_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			create := providerRunFixture(t, tc.site, tc.cs, tc.ws)
			body := fmt.Sprintf(`{"agent":"claude-code","task":"t","workspace_id":%q}`, tc.ws.ID)
			var cw *httptest.ResponseRecorder
			if tc.member {
				cw = doSSO(t, create, http.MethodPost, "/api/v1/runs", govSession(t, govMemberSub, []string{"eng"}, false), body)
			} else {
				cw = do(t, create, http.MethodPost, "/api/v1/runs", providerAdminToken(create, "sub-admit-admin"), body)
			}
			record := providerRunFixture(t, tc.site, tc.cs, tc.ws)
			code, rbody := recordDoor(t, record, providerAdminToken(record, "sub-admit-admin"), tc.ws, tc.member)

			if cw.Code != tc.want || code != tc.want {
				t.Fatalf("create = %d, record = %d, want both %d\ncreate: %s\nrecord: %s", cw.Code, code, tc.want, cw.Body.String(), rbody)
			}
			if rbody != cw.Body.String() {
				t.Errorf("record body differs from create's:\nrecord: %s\ncreate: %s", rbody, cw.Body.String())
			}
			var b errorBody
			_ = json.Unmarshal([]byte(rbody), &b)
			if b.Provider != tc.wantProvider || b.Kind != string(tc.wantKind) || b.Reason != tc.wantReason {
				t.Errorf("record reason/provider/kind = %q/%q/%q, want %q/%q/%q",
					b.Reason, b.Provider, b.Kind, tc.wantReason, tc.wantProvider, tc.wantKind)
			}
			if got, want := deniedRowSummary(t, record), deniedRowSummary(t, create); got != want {
				t.Errorf("record authz.denied rows = %s, create's = %s", got, want)
			}
			if tc.member && (strings.Contains(rbody, "corp") || strings.Contains(rbody, "anthropic")) {
				t.Errorf("a refusal for a caller not granted the provider names one: %s", rbody)
			}
		})
	}

	// Create answers 503 with the bare sentence when the provider block cannot
	// be read (TestRunModelProviderDoors drives that arm directly); the record
	// door must too, and carry no provider or kind. The first launch is a dry
	// pass that counts the site-config reads up to the refusal, so the second
	// can fail the last one — the provider choice's own.
	t.Run("the site config cannot be read", func(t *testing.T) {
		site := twoKeys
		mk := func(failFrom int32) (*Server, *siteReadsStore, string) {
			srv := providerRunFixture(t, site, &capStore{}, plain)
			bearer := providerAdminToken(srv, "sub-admit-admin")
			cs := &siteReadsStore{integStore: srv.cfg.Store.(*integStore), failFrom: failFrom}
			srv.cfg.Store = cs
			return srv, cs, bearer
		}
		dry, dryStore, dryBearer := mk(0)
		if code, body := recordDoor(t, dry, dryBearer, plain, false); code != http.StatusUnprocessableEntity {
			t.Fatalf("dry record = %d %s, want the 422 choice refusal", code, body)
		}
		srv, _, bearer := mk(dryStore.calls.Load())
		code, rbody := recordDoor(t, srv, bearer, plain, false)

		ref := providerRunFixture(t, site, &capStore{}, plain)
		ref.cfg.Store = siteErrStore{ref.cfg.Store.(*integStore)}
		w := httptest.NewRecorder()
		if _, ok := ref.enforceRunModelProvider(w, httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil),
			createRunRequest{Agent: "claude-code", Task: "t"}, types.RunPolicySpec{}, nil); ok {
			t.Fatal("an unreadable provider block admitted the run")
		}
		if code != http.StatusServiceUnavailable || rbody != w.Body.String() || w.Code != code {
			t.Errorf("record = %d %s, create = %d %s, want the same 503", code, rbody, w.Code, w.Body.String())
		}
		var b errorBody
		_ = json.Unmarshal([]byte(rbody), &b)
		if b != (errorBody{Error: mpRunUnreadable}) {
			t.Errorf("record body = %+v, want only %q", b, mpRunUnreadable)
		}
	})
}

// deniedRowSummary is every authz.denied row's reason, provider and kind, in order.
func deniedRowSummary(t *testing.T, srv *Server) string {
	t.Helper()
	rec := srv.cfg.Audit.(*recRecorder)
	var out []string
	for _, ev := range rec.snapshot() {
		if ev.Action != "authz.denied" {
			continue
		}
		var d struct{ Reason, Provider, Kind, Remedy string }
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("unmarshal authz.denied: %v", err)
		}
		out = append(out, fmt.Sprintf("%s/%s/%s/%s", d.Reason, d.Provider, d.Kind, d.Remedy))
	}
	return strings.Join(out, ",")
}
