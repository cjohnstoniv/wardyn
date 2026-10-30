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
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

var inboundRestricted = map[string]map[string]bool{capFeature: {featureInbound: true}}

func inboundRequest(apps ...types.UIApp) createRunRequest {
	return createRunRequest{InlinePolicy: &types.RunPolicySpec{UIApps: apps}}
}

// inboundAsk asks inboundDeclaredRefusal as ctx's caller and returns the status and body it would write
// (0 and "" when it admits).
func inboundAsk(t *testing.T, st *capStore, ctx context.Context, req createRunRequest) (int, string) {
	t.Helper()
	srv := capServer(st)
	srv.cfg.Audit = &recRecorder{}
	srv.cfg.Now = time.Now
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	refusal := srv.inboundDeclaredRefusal(r, req)
	if refusal == nil {
		return 0, ""
	}
	w := httptest.NewRecorder()
	refusal.write(srv, w, r)
	return w.Code, w.Body.String()
}

// TestInboundDeclaredRefusal: `feature: inbound` is restricted (migration 0239), so a request that declares an
// inbound app is refused until an allow names the value, a `*` allow included, and a deny wins. Only what the
// request itself declares is asked: a stored policy's apps, a non-inbound app and an operator are not.
func TestInboundDeclaredRefusal(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	api := types.UIApp{Name: "api", Port: 8080, Inbound: true}
	member := memberCtx([]string{"eng"})
	named := func(effect types.CapabilityEffect) types.CapabilityGrant {
		return grant(types.CapabilitySubjectAll, "", capFeature, featureInbound, effect)
	}
	overlay := createRunRequest{PolicyOverlay: json.RawMessage(`{"ui_apps":[{"name":"api","port":8080,"inbound":true}]}`)}
	stored := createRunRequest{PolicyID: new(uuid.UUID)}

	for _, tc := range []struct {
		name       string
		restricted map[string]map[string]bool
		grants     []types.CapabilityGrant
		ctx        context.Context
		req        createRunRequest
		refused    bool
	}{
		{"restricted, no grant: refused", inboundRestricted, nil, member, inboundRequest(api), true},
		{"restricted, an `all` allow naming it: admitted", inboundRestricted, []types.CapabilityGrant{named(allow)}, member, inboundRequest(api), false},
		{"restricted, an allow for this person: admitted", inboundRestricted,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capFeature, featureInbound, allow)}, member, inboundRequest(api), false},
		{"restricted, an allow for another person: refused", inboundRestricted,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectUser, "sub-other", capFeature, featureInbound, allow)}, member, inboundRequest(api), true},
		{"restricted, a `*` allow grants nobody: refused", inboundRestricted,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capFeature, capWildcard, allow)}, member, inboundRequest(api), true},
		{"a deny beats the allow", inboundRestricted, []types.CapabilityGrant{named(allow), grant(types.CapabilitySubjectUser, capSub, capFeature, featureInbound, deny)}, member, inboundRequest(api), true},
		{"restriction lifted and the kind not enforced: admitted", nil, nil, member, inboundRequest(api), false},
		{"restricted, a super admin is exempt", inboundRestricted, nil, operatorCtx("sub-a", "a@corp.example", oidc.RoleAdmin), inboundRequest(api), false},
		{"a policy_overlay's inbound app is the request's own", inboundRestricted, nil, member, overlay, true},
		{"restricted, an app that is not inbound: admitted", inboundRestricted, nil, member, inboundRequest(types.UIApp{Name: "web", Port: 3000}), false},
		{"restricted, a request naming only a stored policy: admitted", inboundRestricted, nil, member, stored, false},
		{"restricted, a request with no policy: admitted", inboundRestricted, nil, member, createRunRequest{}, false},
		{"one inbound app among plain ones", inboundRestricted, nil, member, inboundRequest(types.UIApp{Name: "web", Port: 3000}, api), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := inboundAsk(t, &capStore{grants: tc.grants, restricted: tc.restricted}, tc.ctx, tc.req)
			if !tc.refused {
				if code != 0 {
					t.Fatalf("refused %d %s, want admitted", code, body)
				}
				return
			}
			if code != http.StatusForbidden || !strings.Contains(body, `"reason":"capability_feature"`) {
				t.Fatalf("got %d %s, want 403 capability_feature", code, body)
			}
		})
	}
}

// The grant is asked at all three request doors alike, so Review and the policy preview cannot say yes to what
// create refuses.
func TestInboundAppDeclaredIsRefusedAtEveryDoor(t *testing.T) {
	const inbound = `"shape":"application","experience":"interactive",` + noRepos + `,"startup":{"kind":"command","command":"npm run dev"},` +
		`"inline_policy":{"ui_apps":[{"name":"web","port":3000,"inbound":true}]}`
	const plain = `"shape":"application","experience":"interactive",` + noRepos + `,"startup":{"kind":"command","command":"npm run dev"},` +
		`"inline_policy":{"ui_apps":[{"name":"web","port":3000}]}`
	store := func(grants ...types.CapabilityGrant) *capStore {
		cs := assignedStore(limitsProfile("open", types.GovernanceLimits{}))
		cs.restricted, cs.grants = inboundRestricted, grants
		return cs
	}
	for _, door := range contractDoors {
		t.Run(door.name, func(t *testing.T) {
			t.Run("refused without the grant", func(t *testing.T) {
				srv, _, _ := govEscapeFixture(t, store())
				w := doSSO(t, srv, http.MethodPost, door.path, govSession(t, "sub-walled", []string{"eng"}, false), modeBody(inbound))
				if w.Code != http.StatusForbidden || errorReason(w) != "capability_feature" {
					t.Fatalf("%s = %d %s, want 403 capability_feature", door.name, w.Code, w.Body.String())
				}
			})
			t.Run("admitted with an `all` allow", func(t *testing.T) {
				allow := grant(types.CapabilitySubjectAll, "", capFeature, featureInbound, types.CapabilityAllow)
				srv, _, _ := govEscapeFixture(t, store(allow))
				w := doSSO(t, srv, http.MethodPost, door.path, govSession(t, "sub-walled", []string{"eng"}, false), modeBody(inbound))
				if errorReason(w) == "capability_feature" {
					t.Fatalf("%s refused a granted inbound app: %s", door.name, w.Body.String())
				}
				if door.name == "create" && w.Code != http.StatusCreated {
					t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
				}
			})
			t.Run("an app that is not inbound needs nothing", func(t *testing.T) {
				srv, _, _ := govEscapeFixture(t, store())
				w := doSSO(t, srv, http.MethodPost, door.path, govSession(t, "sub-walled", []string{"eng"}, false), modeBody(plain))
				if errorReason(w) == "capability_feature" {
					t.Fatalf("%s asked the inbound grant of a plain app: %s", door.name, w.Body.String())
				}
			})
		})
	}
}

func inboundCeiling(l types.GovernanceLimits) governanceCeiling {
	return governanceCeiling{Limits: l, Unit: &ResolvedUnit{ID: uuid.New(), Name: "walled", Limits: l}}
}

// TestBoundUIAppsStripsInboundUnderDenyInteractive: deny_interactive drops the inbound apps whole and keeps the
// rest, with a warning; a profile without it, or an operator, keeps them; deny_ui_apps still strips everything.
func TestBoundUIAppsStripsInboundUnderDenyInteractive(t *testing.T) {
	web, api := types.UIApp{Name: "web", Port: 3000}, types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"group:payments"}}
	srv := New(Config{Audit: &recRecorder{}})
	member := memberCtx([]string{"eng"})

	for _, tc := range []struct {
		name     string
		ceiling  governanceCeiling
		ctx      context.Context
		want     []types.UIApp
		wantWarn string
	}{
		{"deny_interactive drops the inbound app and keeps the plain one", inboundCeiling(types.GovernanceLimits{DenyInteractive: true}), member,
			[]types.UIApp{web}, "dropped 1 inbound ui_app(s)"},
		{"a profile without it keeps both", inboundCeiling(types.GovernanceLimits{}), member, []types.UIApp{web, api}, ""},
		{"no profile keeps both", governanceCeiling{}, member, []types.UIApp{web, api}, ""},
		{"deny_ui_apps still strips every app", inboundCeiling(types.GovernanceLimits{DenyUIApps: true, DenyInteractive: true}), member, nil, "dropped 2 ui_app(s)"},
		{"an ungoverned caller keeps both", inboundCeiling(types.GovernanceLimits{DenyInteractive: true}),
			operatorCtx("sub-a", "a@corp.example", oidc.RoleAdmin), []types.UIApp{web, api}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []types.UIApp{web, api}
			spec := types.RunPolicySpec{UIApps: in}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(tc.ctx)
			warns := srv.boundUIApps(tc.ctx, r, &spec, tc.ceiling, true)
			if len(spec.UIApps) != len(tc.want) {
				t.Fatalf("ui_apps = %+v, want %+v", spec.UIApps, tc.want)
			}
			for i := range tc.want {
				if spec.UIApps[i].Name != tc.want[i].Name || spec.UIApps[i].Inbound != tc.want[i].Inbound {
					t.Fatalf("ui_apps = %+v, want %+v", spec.UIApps, tc.want)
				}
			}
			if (tc.wantWarn == "") != (len(warns) == 0) || (tc.wantWarn != "" && !strings.Contains(warns[0], tc.wantWarn)) {
				t.Fatalf("warnings = %v, want one containing %q", warns, tc.wantWarn)
			}
			if len(in) != 2 || !in[1].Inbound {
				t.Fatalf("the caller's slice was edited in place: %+v", in)
			}
		})
	}
}

// An application is its app: under deny_interactive an inbound one is refused 422 on the application's own reason
// rather than created without it, and a profile that allows interactive sessions does not refuse it.
func TestApplicationRunRefusedWhenDenyInteractiveStripsItsInboundApp(t *testing.T) {
	srv := New(Config{Audit: &recRecorder{}})
	ctx := memberCtx([]string{"eng"})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	req := createRunRequest{
		Shape: client.RunShapeApplication, Experience: client.ExperienceInteractive,
		Startup: &client.RunStartup{Kind: client.StartupCommand, Command: "npm run dev"},
	}
	spec := types.RunPolicySpec{UIApps: []types.UIApp{{Name: "api", Port: 8080, Inbound: true}}}
	req.InlinePolicy = &spec

	refusal := srv.applicationUIAppsRefusal(r, req, spec, inboundCeiling(types.GovernanceLimits{DenyInteractive: true}))
	if refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonRunApplicationUIAppsDenied || refusal.body.Error != applicationInboundDeniedMsg {
		t.Fatalf("refusal = %+v, want 422 %s with the inbound sentence", refusal, reasonRunApplicationUIAppsDenied)
	}
	if got := srv.applicationUIAppsRefusal(r, req, spec, inboundCeiling(types.GovernanceLimits{})); got != nil {
		t.Fatalf("a profile that allows interactive sessions refused: %+v", got)
	}
	plain := types.RunPolicySpec{UIApps: []types.UIApp{{Name: "web", Port: 3000}}}
	req.InlinePolicy = &plain
	if got := srv.applicationUIAppsRefusal(r, req, plain, inboundCeiling(types.GovernanceLimits{DenyInteractive: true})); got != nil {
		t.Fatalf("deny_interactive refused an application whose app is not inbound: %+v", got)
	}

	// The refusal keys on what the application declared: an inbound app it inherits is stripped, not refused.
	resolved := types.RunPolicySpec{UIApps: []types.UIApp{{Name: "web", Port: 3000}, {Name: "api", Port: 8080, Inbound: true}}}
	ceiling := inboundCeiling(types.GovernanceLimits{DenyInteractive: true})
	if got := srv.applicationUIAppsRefusal(r, req, resolved, ceiling); got != nil {
		t.Fatalf("deny_interactive refused an application over an inherited inbound app: %+v", got)
	}
	srv.boundUIApps(ctx, r, &resolved, ceiling, true)
	if len(resolved.UIApps) != 1 || resolved.UIApps[0].Name != "web" {
		t.Fatalf("ui_apps = %+v, want the inherited inbound app stripped", resolved.UIApps)
	}
}
