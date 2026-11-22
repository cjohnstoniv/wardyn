// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (l *portalLife) lookup(t *testing.T, cookie *http.Cookie, issuer, uid string) (int, portalPersonView, string) {
	t.Helper()
	w := l.get(t, cookie, "/api/v1/people/portal-person?issuer="+url.QueryEscape(issuer)+"&person_id="+url.QueryEscape(uid))
	var v portalPersonView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return w.Code, v, errorReason(w)
}

// The People page's two reads. The lookup by (issuer, person id) returns the principal an exchange creates; an
// unbound issuer, or another spelling of a bound one, is a different 404 from an unknown person on a bound
// issuer. The per-person read lists each portal the person used with its assigned unit, a revoked portal kept.
func TestPortalPersonLookup_ByIssuerAndUid_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	unit, err := l.st.UpsertGovernanceUnit(ctx, types.GovernanceUnit{ID: uuid.New(), Name: "team-a", CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}})
	if err != nil {
		t.Fatal(err)
	}
	principal, _ := l.signIn(t, "u1")
	pinned, pinnedCred := l.portal(t, portalTestIssuer, func(d *types.Delegate) { d.RunInUnder = &unit.ID })
	l.token(t, pinned, pinnedCred)
	if w := doSSO(t, l.h.srv, http.MethodDelete, "/api/v1/admin/delegates/"+pinned.ID.String(), l.secAdmin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke the pinned portal = %d %s", w.Code, w.Body.String())
	}

	code, v, _ := l.lookup(t, l.secAdmin, portalTestIssuer, "u1")
	if code != http.StatusOK || v.Principal != principal || v.State != "active" {
		t.Fatalf("lookup = %d %+v, want the principal the exchange created (%s), active", code, v, principal)
	}
	if code, v, _ := l.lookup(t, l.admin, portalTestIssuer, "u1"); code != http.StatusOK || v.Principal != principal {
		t.Errorf("super admin lookup = %d %+v", code, v)
	}
	check := func(name string, used []types.PortalUsed) {
		t.Helper()
		if len(used) != 2 {
			t.Fatalf("%s: portals_used = %+v, want both portals", name, used)
		}
		for _, u := range used {
			switch u.Delegate {
			case l.d.ID:
				if u.RunInUnder != nil || u.RevokedAt != nil {
					t.Errorf("%s: the unpinned live portal = %+v", name, u)
				}
			case pinned.ID:
				if u.RunInUnder == nil || *u.RunInUnder != unit.ID || u.RevokedAt == nil {
					t.Errorf("%s: the pinned revoked portal = %+v, want its unit and revoked_at kept", name, u)
				}
			default:
				t.Errorf("%s: unexpected portal %+v", name, u)
			}
		}
	}
	check("lookup", v.PortalsUsed)
	w := l.get(t, l.secAdmin, "/api/v1/people/"+url.PathEscape(principal)+"/portals")
	var per portalPersonView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &per) != nil || per.Principal != principal {
		t.Fatalf("GET /people/{principal}/portals = %d %s", w.Code, w.Body.String())
	}
	check("per-person read", per.PortalsUsed)

	// An unbound issuer, and another spelling of the bound one, are not "no such person".
	for _, issuer := range []string{"https://other.example/oauth2/default", portalTestIssuer + "/", "HTTPS://IDP.PORTAL.EXAMPLE/oauth2/default"} {
		if code, _, reason := l.lookup(t, l.secAdmin, issuer, "u1"); code != http.StatusNotFound || reason != "portal_issuer_not_bound" {
			t.Errorf("lookup under %s = %d %s, want 404 portal_issuer_not_bound", issuer, code, reason)
		}
	}
	if code, _, reason := l.lookup(t, l.secAdmin, portalTestIssuer, "nobody"); code != http.StatusNotFound || reason != "person_not_found" {
		t.Errorf("lookup of an unknown person = %d %s, want 404 person_not_found", code, reason)
	}
	for _, q := range []string{"", "?issuer=" + url.QueryEscape(portalTestIssuer), "?person_id=u1"} {
		w := l.get(t, l.secAdmin, "/api/v1/people/portal-person"+q)
		if w.Code != http.StatusBadRequest || errorReason(w) != "portal_person_param_invalid" {
			t.Errorf("lookup with query %q = %d %s, want 400 portal_person_param_invalid", q, w.Code, w.Body.String())
		}
	}
	// Not a portal person, and no one below the security tier.
	if w := l.get(t, l.secAdmin, "/api/v1/people/alice@corp.example/portals"); w.Code != http.StatusUnprocessableEntity || errorReason(w) != "portal_person_only" {
		t.Errorf("portals of a console person = %d %s, want 422 portal_person_only", w.Code, w.Body.String())
	}
	user := accessSession(t, "plain", "plain@corp.example", oidc.RoleUser, []string{})
	if code, _, _ := l.lookup(t, user, portalTestIssuer, "u1"); code != http.StatusForbidden {
		t.Errorf("lookup as a plain user = %d, want 403", code)
	}
	if w := l.get(t, user, "/api/v1/people/"+url.PathEscape(principal)+"/portals"); w.Code != http.StatusForbidden {
		t.Errorf("portals read as a plain user = %d, want 403", w.Code)
	}
}

// Asking about a person no portal has introduced answers 404 and writes nothing: no identity, job or audit row
// beyond the refused request's own, for the lookup and for a suspend of the principal an exchange would create.
func TestPortalPersonLookup_NeverSeenIs404AndWritesNothing_PG(t *testing.T) {
	l := newPortalLife(t)
	l.signIn(t, "u1")
	identities := l.count(t, `SELECT count(*) FROM principal_identities`)
	jobs := l.count(t, `SELECT count(*) FROM deprovision_jobs`)
	people := l.count(t, `SELECT count(*) FROM people`)
	audits := len(l.h.audit.snapshot())
	ghost := oidc.PortalPrincipal(portalTestIssuer, "ghost")
	if code, _, reason := l.lookup(t, l.secAdmin, portalTestIssuer, "ghost"); code != http.StatusNotFound || reason != "person_not_found" {
		t.Errorf("lookup of a never-seen person = %d %s, want 404 person_not_found", code, reason)
	}
	if w := l.get(t, l.secAdmin, "/api/v1/people/"+url.PathEscape(ghost)+"/portals"); w.Code != http.StatusNotFound || errorReason(w) != "person_not_found" {
		t.Errorf("portals of a never-seen person = %d %s, want 404 person_not_found", w.Code, w.Body.String())
	}
	for _, action := range []string{"suspend", "reactivate"} {
		w := l.post(t, l.admin, ghost, action, "")
		if w.Code != http.StatusNotFound || errorReason(w) != "person_not_found" {
			t.Errorf("%s of a never-seen person = %d %s, want 404 person_not_found", action, w.Code, w.Body.String())
		}
	}
	for q, want := range map[string]int{
		`SELECT count(*) FROM principal_identities`: identities, `SELECT count(*) FROM deprovision_jobs`: jobs, `SELECT count(*) FROM people`: people,
	} {
		if got := l.count(t, q); got != want {
			t.Errorf("%s = %d after the refused requests, want %d", q, got, want)
		}
	}
	for _, ev := range l.h.audit.snapshot()[audits:] {
		if (ev.Action != "person.suspend" && ev.Action != "person.reactivate") || ev.Outcome != "denied" {
			t.Errorf("a refused request wrote %s (%s)", ev.Action, ev.Outcome)
		}
	}
}
