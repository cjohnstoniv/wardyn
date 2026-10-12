// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDelegation_PortalRevokeKeepsThePersonSignedIn is the missing proof of #1508 design section 5: revoking a
// portal ends the portal's wdg_ tokens and nothing of the person's. Their cookie session and their own wdn_ token
// keep answering as them, no session cutoff is recorded for them, and another portal's wdg_ for the same person
// still works. It pairs with TestDelegation_HappyPathActsAsThePerson, which proves the delegation itself.
func TestDelegation_PortalRevokeKeepsThePersonSignedIn(t *testing.T) {
	e := newDelegationPG(t)
	revocations := e.h.srv.cfg.SessionRevocations.(*memRevocations)
	portal, cred := e.registerPortal(t, delegGroup)
	other, otherCred := e.registerPortal(t, delegGroup)

	// The person: a signed-in cookie session and a token they minted themselves.
	// IssuedAt is stamped, as a real sign-in's cookie is: a cookie without it is outside any cutoff.
	session := signedAccessCookie(t, oidc.Session{
		V: oidc.SessionCodecVersion, Sub: delegPerson, Email: delegPersonEmail, Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		Groups: []string{delegGroup}, IssuedAt: time.Now().UTC().Add(-time.Minute), Expiry: time.Now().UTC().Add(time.Hour),
	})
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/tokens", session, `{"name":"own","ttl_seconds":3600}`)
	var own types.APIToken
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &own) != nil || own.Token == "" {
		t.Fatalf("the person mints their own token: %d %s, want 201", w.Code, w.Body.String())
	}
	viaPortal := e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup)
	viaOther := e.delegate(t, other, otherCred, delegPerson, delegPersonEmail, delegGroup)

	// principalOf is who GET /me says answered: the cookie when one is given, else the bearer.
	principalOf := func(bearer string, cookie *http.Cookie) (int, string) {
		t.Helper()
		var r *httptest.ResponseRecorder
		if cookie != nil {
			r = doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me", cookie, "")
		} else {
			r = do(t, e.h.srv, http.MethodGet, "/api/v1/me", bearer, "")
		}
		var me struct {
			Principal string `json:"principal"`
		}
		_ = json.Unmarshal(r.Body.Bytes(), &me)
		return r.Code, me.Principal
	}
	assertSignedIn := func(phase string) {
		t.Helper()
		if code, who := principalOf("", session); code != http.StatusOK || who != delegPerson {
			t.Errorf("%s: the person's cookie session answers %d as %q, want 200 as %q", phase, code, who, delegPerson)
		}
		if code, who := principalOf(own.Token, nil); code != http.StatusOK || who != delegPerson {
			t.Errorf("%s: the person's own wdn_ token answers %d as %q, want 200 as %q", phase, code, who, delegPerson)
		}
	}
	assertSignedIn("before the revoke")
	for label, tok := range map[string]string{"the portal's wdg_": viaPortal, "the other portal's wdg_": viaOther} {
		if code, who := principalOf(tok, nil); code != http.StatusOK || who != delegPerson {
			t.Fatalf("before the revoke: %s answers %d as %q, want 200 as %q", label, code, who, delegPerson)
		}
	}

	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/admin/delegates/"+portal.String(), e.secAdmin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke the portal: %d %s, want 204", w.Code, w.Body.String())
	}

	// The portal's token ends; everything of the person's stands.
	if code, _ := principalOf(viaPortal, nil); code != http.StatusUnauthorized {
		t.Errorf("the revoked portal's wdg_ answers %d, want 401", code)
	}
	if w := e.exchange(t, portal, cred, e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("the revoked portal's exchange: %d, want 401", w.Code)
	}
	assertSignedIn("after the revoke")
	if code, who := principalOf(viaOther, nil); code != http.StatusOK || who != delegPerson {
		t.Errorf("another portal's wdg_ for the same person answers %d as %q after this portal's revoke, want 200 as %q", code, who, delegPerson)
	}

	// No cutoff was written for the person: a session or token issued before the revoke is not revoked by it.
	if cut, ok := revocations.cut[delegPerson]; ok {
		t.Errorf("the portal revoke recorded a session cutoff for the person at %v", cut)
	}
	if revoked, _ := revocations.IsSessionRevoked(context.Background(), delegPerson, delegPersonEmail, time.Now().Add(-time.Hour)); revoked {
		t.Error("a session issued an hour ago reads as revoked after the portal revoke")
	}
	// The person's own token row is live, not revoked, and the revoke is the only credential row the audit log gained.
	w = doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", session, "")
	var rows []types.APIToken
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].ID != own.ID || rows[0].RevokedAt != nil {
		t.Errorf("the person's token list after the portal revoke = %s (%v), want their one live token", w.Body.String(), err)
	}
	if got := e.rows("token.revoke"); len(got) != 0 {
		t.Errorf("the portal revoke wrote %d token.revoke rows, want none", len(got))
	}
	if got := e.rows("delegate.revoke"); len(got) != 1 {
		t.Errorf("delegate.revoke rows = %d, want 1", len(got))
	}
}
