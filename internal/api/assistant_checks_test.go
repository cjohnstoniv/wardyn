// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantReplaySpy answers the replay read with no rows and records the scope it was asked for.
type assistantReplaySpy struct {
	*assistantToolSpy
	scopes []store.ReplayScope
}

func (f *assistantReplaySpy) RunHostDecisions(_ context.Context, scope store.ReplayScope, _, _ time.Time, _ int, _ []uuid.UUID) ([]store.RunHostDecision, store.ReplayTotals, error) {
	f.scopes = append(f.scopes, scope)
	return nil, store.ReplayTotals{}, nil
}

func assistantChecksBody(kind, target string, draft any) string {
	raw, _ := json.Marshal(draft)
	b, _ := json.Marshal(types.AssistantChecksRequest{TargetKind: kind, Target: target, Draft: raw})
	return string(b)
}

// assistantPersonServer is a site with compose and review on, served over SSO, with the replay read answered.
func assistantPersonServer(t *testing.T) (*assistantReplaySpy, *Server) {
	t.Helper()
	site, _ := assistantTwoProviders()
	site.Assistant.Review = true
	site.Assistant.Providers[0].Review, site.Assistant.Providers[1].Review = true, true
	spy := &assistantReplaySpy{assistantToolSpy: &assistantToolSpy{assistantFake: newAssistantFake(site)}}
	srv, _ := assistantSessionServer(t, spy.assistantFake)
	srv.cfg.Store = spy
	return spy, srv
}

// A request with no signed-in human (the admin token, local mode) or from a service identity is refused 403
// assistant_role_denied on both routes, with the routes' own sentence, before any read: not the sign-in record,
// the caller's unit, the target or any run's host decisions.
func TestAssistantPersonRoutesRefuseNoPerson(t *testing.T) {
	spy, srv := assistantPersonServer(t)
	service := withHumanIdentity(withServiceIdentity(context.Background(), "bot", "sub-sponsor"), "svc-bot", "", oidc.RoleUser, "", nil, false)
	for _, route := range []struct {
		path   string
		handle http.HandlerFunc
	}{{"/api/v1/assistant/checks", srv.handleAssistantChecks}, {"/api/v1/assistant/replay", srv.handleAssistantReplay}} {
		for name, ctx := range map[string]context.Context{"admin token": context.Background(), "service identity": service} {
			before, all := spy.assistantFake.reads(), spy.all()
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(assistantChecksBody("run", "", map[string]any{}))).WithContext(ctx)
			route.handle(w, r)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonAssistantRoleDenied) ||
				!strings.Contains(w.Body.String(), "The assistant's checks answer a signed-in person only.") {
				t.Errorf("%s %s = %d %s, want 403 %s with the routes' sentence", name, route.path, w.Code, w.Body, reasonAssistantRoleDenied)
			}
			if spy.assistantFake.reads() != before || spy.all() != all || len(spy.scopes) != 0 {
				t.Errorf("%s %s read before refusing", name, route.path)
			}
		}
	}
}

// The organisation's switch: checks serve compose and refuse when it is off; the replay serves both purposes and
// refuses only when both are off. run_history_counts gates neither: the replay answers its counts with it off,
// with the fixed basis, over the caller's own runs.
func TestAssistantPersonRoutesSwitches(t *testing.T) {
	spy, srv := assistantPersonServer(t)
	person := "sub-person-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	body := assistantChecksBody("run", "", map[string]any{})

	spy.site.Assistant.RunHistoryCounts = false
	w := doSSO(t, srv, http.MethodPost, "/api/v1/assistant/replay", cookie, body)
	if w.Code != http.StatusOK {
		t.Fatalf("replay with run history off = %d %s, want 200", w.Code, w.Body)
	}
	var ans struct {
		Basis string `json:"basis"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil || ans.Basis != "proposed_rules_only" {
		t.Errorf("replay basis = %s, want proposed_rules_only", w.Body)
	}
	if len(spy.scopes) != 1 || spy.scopes[0].Owner != person || spy.scopes[0].Units != nil {
		t.Errorf("replay scopes = %+v, want the caller's own runs", spy.scopes)
	}

	spy.site.Assistant.Compose = false
	wantReason(t, doSSO(t, srv, http.MethodPost, "/api/v1/assistant/checks", cookie, body), http.StatusConflict, reasonAssistantOff)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/assistant/replay", cookie, body); w.Code != http.StatusOK {
		t.Errorf("replay with review still on = %d %s, want 200", w.Code, w.Body)
	}
	spy.site.Assistant.Review = false
	wantReason(t, doSSO(t, srv, http.MethodPost, "/api/v1/assistant/replay", cookie, body), http.StatusConflict, reasonAssistantOff)
}

// A run of another member is answered as GET /runs/{id} answers it, byte for byte, and its host decisions are
// never read.
func TestAssistantPersonRoutesCheckTheTarget(t *testing.T) {
	spy, srv := assistantPersonServer(t)
	person := "sub-person-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	other := spy.seedRun(t, "sub-other-"+uuid.NewString())
	direct := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+other.String(), cookie, "")
	for _, path := range []string{"/api/v1/assistant/checks", "/api/v1/assistant/replay"} {
		w := doSSO(t, srv, http.MethodPost, path, cookie, assistantChecksBody("run", other.String(), map[string]any{}))
		if w.Code != direct.Code || w.Body.String() != direct.Body.String() {
			t.Errorf("%s on another member's run = %d %s, want GET /runs/{id}'s %d %s", path, w.Code, w.Body, direct.Code, direct.Body)
		}
	}
	if len(spy.scopes) != 0 {
		t.Errorf("host decisions read for a run the caller may not read: %+v", spy.scopes)
	}
	// A member with no unit grant is refused at the unit tier, as PUT /governance/units/{id} refuses them, on
	// both routes, before the unit's chain or any run's host decisions is read.
	target := spy.reads()
	for _, path := range []string{"/api/v1/assistant/checks", "/api/v1/assistant/replay"} {
		w := doSSO(t, srv, http.MethodPost, path, cookie, assistantChecksBody("unit", uuid.NewString(), map[string]any{"name": "u"}))
		wantReason(t, w, http.StatusForbidden, "unit_scope_denied")
	}
	if len(spy.scopes) != 0 || spy.reads() != target {
		t.Errorf("read a unit the caller does not govern: scopes %+v, %d target reads", spy.scopes, spy.reads()-target)
	}
}

// A unit that rules out every marked provider (here: the person is granted neither) refuses each route 409
// assistant_unit_denies: checks for compose, the replay only when both purposes deny.
func TestAssistantPersonRoutesUnitDenies(t *testing.T) {
	spy, srv := assistantPersonServer(t)
	person := "sub-person-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	for _, p := range []string{"a", "b"} {
		spy.capStore.grants = append(spy.capStore.grants, types.CapabilityGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: person + "@corp.example",
			Capability: capModelProvider, Value: p, Effect: types.CapabilityDeny, CreatedBy: "seed"})
	}
	body := assistantChecksBody("run", "", map[string]any{})
	for _, path := range []string{"/api/v1/assistant/checks", "/api/v1/assistant/replay"} {
		wantReason(t, doSSO(t, srv, http.MethodPost, path, cookie, body), http.StatusConflict, reasonAssistantUnitDenies)
	}
}
