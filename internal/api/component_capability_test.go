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
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// componentRefusalBody is the one body every org component refusal answers.
const componentRefusalBody = `{"error":"This component isn't available to you. Ask your admin.","reason":"capability_component"}`

const (
	compOrgID    = "6f0c1d2e-0000-4000-8000-0000000c0301"
	compOtherID  = "6f0c1d2e-0000-4000-8000-0000000c0302"
	compAbsentID = "6f0c1d2e-0000-4000-8000-0000000c0399"
)

// attach asks componentAttachRefusal as ctx's caller and returns the status and
// body it would write ("" when it admits) plus the srv it answered on.
func attach(t *testing.T, st *capStore, ctx context.Context, orgRowID string) (int, string, *Server) {
	t.Helper()
	srv := capServer(st)
	srv.cfg.Audit = &recRecorder{}
	srv.cfg.Now = time.Now
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	refusal := srv.componentAttachRefusal(r, orgRowID)
	if refusal == nil {
		return 0, "", srv
	}
	w := httptest.NewRecorder()
	refusal.write(srv, w, r)
	return w.Code, w.Body.String(), srv
}

// TestFeatureCustomComponent_DefaultOnUnlessFeatureEnforced (F7): defining a
// component of one's own is ON for everyone on a deployment that has not
// enforced `feature`, and one deny row turns it off. On a deployment that HAS
// enforced `feature` (to bound SSH keys or API tokens) it is off until an allow
// row covers it — and an existing `*` allow, which meant "both" before the
// upgrade, now covers it too. Both halves are the documented upgrade behaviour.
func TestFeatureCustomComponent_DefaultOnUnlessFeatureEnforced(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	all := func(value string, e types.CapabilityEffect) types.CapabilityGrant {
		return grant(types.CapabilitySubjectAll, "", capFeature, value, e)
	}
	member := memberCtx([]string{})
	cases := []struct {
		name     string
		enforced bool
		grants   []types.CapabilityGrant
		ctx      context.Context
		allowed  bool
	}{
		{"unenforced, no rows: on for everyone", false, nil, member, true},
		{"unenforced, deny custom_component for everyone: off", false, []types.CapabilityGrant{all(featureCustomComponent, deny)}, member, false},
		{"unenforced, deny for this person only: off for them", false, []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capFeature, featureCustomComponent, deny)}, member, false},
		{"unenforced, a deny on ssh_key does not reach it", false, []types.CapabilityGrant{all(featureSSHKey, deny)}, member, true},
		{"enforced, no allow rows: off", true, nil, member, false},
		{"enforced, the pre-upgrade allow-list names ssh_key and api_token only: off", true, []types.CapabilityGrant{all(featureSSHKey, allow), all(featureAPIToken, allow)}, member, false},
		{"enforced, a pre-upgrade `*` allow: on", true, []types.CapabilityGrant{all(capWildcard, allow)}, member, true},
		{"enforced, an allow naming custom_component: on", true, []types.CapabilityGrant{all(featureCustomComponent, allow)}, member, true},
		{"enforced, allow for everyone but a deny for this person: off", true, []types.CapabilityGrant{all(featureCustomComponent, allow), grant(types.CapabilitySubjectUser, capEmail, capFeature, featureCustomComponent, deny)}, member, false},
		{"enforced, no rows, a super admin: exempt", true, nil, operatorCtx("sub-a", rbacOperator, oidc.RoleAdmin), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &capStore{grants: c.grants, enf: map[string]bool{capFeature: c.enforced}}
			code, body, srv := attach(t, st, c.ctx, "")
			if c.allowed {
				if code != 0 {
					t.Fatalf("refused %d %s, want admitted", code, body)
				}
				return
			}
			if code != http.StatusForbidden {
				t.Fatalf("code = %d (%s), want 403", code, body)
			}
			var got errorBody
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
			if got.Reason != "capability_feature" || got.Error != "Custom components aren't turned on for you. Ask your admin." {
				t.Errorf("body = %+v", got)
			}
			if reasons := auditReasons(t, srv, "authz.denied"); len(reasons) != 1 || reasons[0] != "capability_feature" {
				t.Errorf("authz.denied reasons = %v, want [capability_feature]", reasons)
			}
		})
	}

	// The new value's rows decide nothing about the two it joined.
	for _, enforced := range []bool{false, true} {
		base := []types.CapabilityGrant{all(featureSSHKey, allow)}
		with := append(base, all(featureCustomComponent, deny), all(featureCustomComponent, allow))
		for _, v := range []string{featureSSHKey, featureAPIToken} {
			before, err1 := capServer(&capStore{grants: base, enf: map[string]bool{capFeature: enforced}}).capSeamAllowed(member, capFeature, v)
			after, err2 := capServer(&capStore{grants: with, enf: map[string]bool{capFeature: enforced}}).capSeamAllowed(member, capFeature, v)
			if err1 != nil || err2 != nil || before != after {
				t.Errorf("enforced=%v %s: %v/%v before custom_component rows, %v/%v after", enforced, v, before, err1, after, err2)
			}
		}
	}
}

// TestComponentKind_NobodyUntilGranted: an org component's id carries a
// capability_restrictions row from its create, and a restricted value admits
// only an allow naming it — whether or not the kind is enforced, and whatever a
// wildcard allow says. An id with no restriction row is an ordinary narrowing
// value.
func TestComponentKind_NobodyUntilGranted(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	restricted := map[string]map[string]bool{capComponent: {compOrgID: true}}
	cases := []struct {
		name       string
		restricted map[string]map[string]bool
		enforced   bool
		grants     []types.CapabilityGrant
		groups     []string
		id         string
		allowed    bool
	}{
		{"restricted, no rows: nobody", restricted, false, nil, nil, compOrgID, false},
		{"restricted, no rows, asked in uppercase: nobody", restricted, false, nil, nil, strings.ToUpper(compOrgID), false},
		{"restricted, no rows, asked braced: nobody", restricted, false, nil, nil, "{" + compOrgID + "}", false},
		{"not a uuid: refused with the same bytes", restricted, false, nil, nil, "jira-api", false},
		{"restricted, a person allow naming it, asked in uppercase", restricted, false,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capComponent, compOrgID, allow)}, nil, strings.ToUpper(compOrgID), true},
		{"restricted, a wildcard allow for everyone lists nobody", restricted, false,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capComponent, capWildcard, allow)}, nil, compOrgID, false},
		{"restricted, enforced, wildcard allow: still nobody", restricted, true,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capComponent, capWildcard, allow)}, nil, compOrgID, false},
		{"restricted, a person allow naming it", restricted, false,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capComponent, compOrgID, allow)}, nil, compOrgID, true},
		{"restricted, a group allow naming it", restricted, false,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "eng", capComponent, compOrgID, allow)}, []string{"eng"}, compOrgID, true},
		{"restricted, an allow naming ANOTHER component", restricted, false,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capComponent, compOtherID, allow)}, nil, compOrgID, false},
		{"restricted, a group allow and a person deny: deny wins", restricted, false,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "eng", capComponent, compOrgID, allow),
				grant(types.CapabilitySubjectUser, capSub, capComponent, compOrgID, deny)}, []string{"eng"}, compOrgID, false},
		{"unrestricted, unenforced, no rows: allowed like any narrowing kind", restricted, false, nil, nil, compOtherID, true},
		{"unrestricted, enforced, no rows", restricted, true, nil, nil, compOtherID, false},
		{"unrestricted, enforced, wildcard allow", restricted, true,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capComponent, capWildcard, allow)}, nil, compOtherID, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			groups := c.groups
			if groups == nil {
				groups = []string{}
			}
			st := &capStore{grants: c.grants, enf: map[string]bool{capComponent: c.enforced}, restricted: c.restricted}
			code, body, _ := attach(t, st, memberCtx(groups), c.id)
			if (code == 0) != c.allowed {
				t.Fatalf("admitted = %v (%d %s), want %v", code == 0, code, body, c.allowed)
			}
			if code != 0 && (code != http.StatusForbidden || strings.TrimSpace(body) != componentRefusalBody) {
				t.Errorf("refused %d %s, want 403 %s", code, body, componentRefusalBody)
			}
		})
	}
}

// TestComponentRefusalNamesNothing (F19): a person who is not granted an org
// component learns nothing about it from the refusal. The refused-but-real id
// and an absent id refused by an enforced kind answer the SAME bytes, which
// name neither id; the audit row carries no id either. A granted id is
// admitted, and an absent id on a stock deployment is admitted HERE (the kind
// is unenforced and nothing restricts it) — the component gate's own lookup
// answers it.
func TestComponentRefusalNamesNothing(t *testing.T) {
	member := memberCtx([]string{})
	allowGranted := grant(types.CapabilitySubjectUser, capSub, capComponent, compOtherID, types.CapabilityAllow)
	restricted := map[string]map[string]bool{capComponent: {compOrgID: true, compOtherID: true}}

	if code, body, _ := attach(t, &capStore{grants: []types.CapabilityGrant{allowGranted}, restricted: restricted}, member, compOtherID); code != 0 {
		t.Fatalf("granted: refused %d %s", code, body)
	}
	if code, body, _ := attach(t, &capStore{restricted: restricted}, member, compAbsentID); code != 0 {
		t.Fatalf("absent, stock deployment: refused %d %s, want the gate's lookup to answer it", code, body)
	}

	ungrantedCode, ungranted, srv := attach(t, &capStore{grants: []types.CapabilityGrant{allowGranted}, restricted: restricted}, member, compOrgID)
	absentCode, absent, _ := attach(t, &capStore{enf: map[string]bool{capComponent: true}}, member, compAbsentID)
	if ungrantedCode != http.StatusForbidden || strings.TrimSpace(ungranted) != componentRefusalBody {
		t.Errorf("ungranted = %d %s, want 403 %s", ungrantedCode, ungranted, componentRefusalBody)
	}
	if absentCode != ungrantedCode || absent != ungranted {
		t.Errorf("absent = %d %q, ungranted = %d %q: the two must be byte-identical", absentCode, absent, ungrantedCode, ungranted)
	}
	rec := srv.cfg.Audit.(*recRecorder)
	for _, ev := range rec.snapshot() {
		if strings.Contains(string(ev.Data), compOrgID) || strings.Contains(ev.Target, compOrgID) {
			t.Errorf("audit %s row names the component id: %s %s", ev.Action, ev.Target, ev.Data)
		}
	}
	if reasons := auditReasons(t, srv, "authz.denied"); len(reasons) != 1 || reasons[0] != "capability_component" {
		t.Errorf("authz.denied reasons = %v, want [capability_component]", reasons)
	}
}

// TestComponentGrantValueIsAUUID: a component grant or restriction names one
// row by uuid, canonicalized the way the workspace and policy arms are, at both
// write doors (POST /permissions/grants and the availability path). The feature
// arm takes the new value in any case and stores it folded.
func TestComponentGrantValueIsAUUID(t *testing.T) {
	braced := "{" + strings.ToUpper(compOrgID) + "}"
	for in, want := range map[string]string{compOrgID: compOrgID, braced: compOrgID, "*": "*"} {
		if got, err := canonicalGrantValue(capComponent, in); err != nil || got != want {
			t.Errorf("canonicalGrantValue(component, %q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"jira-api", "6f0c1d2e"} {
		if _, err := canonicalGrantValue(capComponent, bad); err == nil {
			t.Errorf("canonicalGrantValue(component, %q) accepted a value no component id can be", bad)
		}
	}
	if got, err := canonicalGrantValue(capFeature, " Custom_Component "); err != nil || got != featureCustomComponent {
		t.Errorf("canonicalGrantValue(feature, Custom_Component) = %q, %v", got, err)
	}

	target := func(value string) (string, string, bool, int) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("kind", capComponent)
		rctx.URLParams.Add("*", value)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()
		kind, v, ok := availabilityTarget(w, r)
		return kind, v, ok, w.Code
	}
	if kind, v, ok, _ := target(braced); !ok || kind != capComponent || v != compOrgID {
		t.Errorf("availabilityTarget(component, %s) = %q, %q, %v; want the canonical id", braced, kind, v, ok)
	}
	for _, bad := range []string{"jira-api", "*"} {
		if _, _, ok, code := target(bad); ok || code != http.StatusBadRequest {
			t.Errorf("availabilityTarget(component, %q) = ok %v, code %d; want 400", bad, ok, code)
		}
	}
}
