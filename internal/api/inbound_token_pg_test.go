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

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mintPersonal has as mint a token of its own with body and returns the answer.
func (e svcEnv) mintPersonal(t *testing.T, as *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/tokens", as, body)
}

// verify runs the shared resolver on raw for the surface inbound names.
func (e svcEnv) verify(raw string, inbound bool) (verifiedAPIToken, tokenVerdict) {
	v, _, verdict := e.h.srv.verifyAPIToken(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), raw, inbound)
	return v, verdict
}

// expiresWithin fails unless tok expires no later than want from now and no earlier than a minute before it.
func expiresWithin(t *testing.T, what string, tok types.APIToken, want time.Duration) {
	t.Helper()
	now := time.Now()
	if tok.ExpiresAt == nil || tok.ExpiresAt.After(now.Add(want+time.Minute)) || tok.ExpiresAt.Before(now.Add(want-time.Minute)) {
		t.Errorf("%s expires_at = %v, want about %v from now", what, tok.ExpiresAt, want)
	}
}

// TestInboundToken_MintBothKinds: a person and a service's sponsor each mint an inbound token; it always
// expires, at most 90 days or the deployment's lower cap; omitting kind is an api token, and no other kind
// is minted here.
func TestInboundToken_MintBothKinds(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)

	personal := decodeToken(t, e.mintPersonal(t, e.sponsor, `{"name":"in","kind":"inbound"}`))
	if !personal.Inbound || personal.Principal != svcSponsor {
		t.Errorf("personal inbound mint = %+v, want inbound for %s", personal, svcSponsor)
	}
	expiresWithin(t, "personal inbound token with no ttl", personal, serviceTokenMaxTTL)
	long := decodeToken(t, e.mintPersonal(t, e.sponsor, `{"name":"long","kind":"inbound","ttl_seconds":31536000}`))
	expiresWithin(t, "personal inbound token asking a year", long, serviceTokenMaxTTL)
	api := decodeToken(t, e.mintPersonal(t, e.sponsor, `{"name":"api"}`))
	if api.Inbound || api.ExpiresAt != nil {
		t.Errorf("a mint with no kind = %+v, want an api token with no expiry (no deployment cap)", api)
	}
	if rows := e.auditRows("token.create"); len(rows) == 0 || !strings.Contains(string(rows[0].Data), `"kind":"inbound"`) {
		t.Errorf("token.create rows = %+v, want the first to record kind inbound", rows)
	}

	svc := decodeToken(t, e.mintReq(t, e.sponsor, svcName, `{"name":"in","kind":"inbound"}`))
	if !svc.Inbound || svc.Principal != svcPrin {
		t.Errorf("service inbound mint = %+v, want inbound for %s", svc, svcPrin)
	}
	expiresWithin(t, "service inbound token with no ttl", svc, serviceTokenMaxTTL)
	if rows := e.auditRows("service_identity.token.create"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"kind":"inbound"`) {
		t.Errorf("service_identity.token.create rows = %+v, want one recording kind inbound", rows)
	}

	e.h.srv.cfg.APITokenMaxTTL = 24 * time.Hour
	capped := decodeToken(t, e.mintPersonal(t, e.sponsor, `{"name":"capped","kind":"inbound"}`))
	expiresWithin(t, "personal inbound token under a 24 h deployment cap", capped, 24*time.Hour)
	cappedSvc := decodeToken(t, e.mintReq(t, e.sponsor, svcName, `{"name":"capped","kind":"inbound"}`))
	expiresWithin(t, "service inbound token under a 24 h deployment cap", cappedSvc, 24*time.Hour)

	for _, kind := range []string{types.APITokenKindCLIAccess, types.APITokenKindCLIAdmin, "Inbound", "bogus"} {
		body := `{"name":"x","kind":"` + kind + `"}`
		if w := e.mintPersonal(t, e.sponsor, body); w.Code != http.StatusBadRequest || respReason(w) != reasonInvalidRequestBody {
			t.Errorf("personal mint of kind %q: %d %s, want 400 %s", kind, w.Code, w.Body.String(), reasonInvalidRequestBody)
		}
		if w := e.mintReq(t, e.sponsor, svcName, body); w.Code != http.StatusBadRequest || respReason(w) != reasonInvalidRequestBody {
			t.Errorf("service mint of kind %q: %d %s, want 400 %s", kind, w.Code, w.Body.String(), reasonInvalidRequestBody)
		}
	}
}

// TestInboundToken_RefusedOnTheConsole: an inbound token, a person's or a service's, gets the 401 an unknown
// token gets on every API route, while the same owner's api token works; the gateway's side of the shared
// resolver takes the inbound token and nothing else.
func TestInboundToken_RefusedOnTheConsole(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	personalIn := decodeToken(t, e.mintPersonal(t, e.sponsor, `{"name":"in","kind":"inbound"}`))
	personalAPI := decodeToken(t, e.mintPersonal(t, e.sponsor, `{"name":"api"}`))
	svcIn := decodeToken(t, e.mintReq(t, e.sponsor, svcName, `{"name":"in","kind":"inbound"}`))
	svcAPI := e.mustMint(t, e.sponsor, svcName)

	for _, tok := range []types.APIToken{personalAPI, svcAPI} {
		if w := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", ""); w.Code != http.StatusOK {
			t.Fatalf("%s's api token on GET /me: %d %s, want 200", tok.Principal, w.Code, w.Body.String())
		}
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/me", ""},
		{http.MethodGet, "/api/v1/runs", ""},
		{http.MethodGet, "/api/v1/me/tokens", ""},
		{http.MethodPost, "/api/v1/me/tokens", `{"name":"child"}`},
		{http.MethodPost, "/api/v1/runs/preflight", `{}`},
	} {
		unknown := e.as(t, apiTokenPrefix+"not-a-token", route.method, route.path, route.body)
		if unknown.Code != http.StatusUnauthorized {
			t.Fatalf("an unknown token on %s %s: %d, want 401", route.method, route.path, unknown.Code)
		}
		for _, tok := range []types.APIToken{personalIn, svcIn} {
			w := e.as(t, tok.Token, route.method, route.path, route.body)
			if w.Code != http.StatusUnauthorized || w.Body.String() != unknown.Body.String() {
				t.Errorf("%s's inbound token on %s %s: %d %s, want the unknown token's 401 %s",
					tok.Principal, route.method, route.path, w.Code, w.Body.String(), unknown.Body.String())
			}
		}
	}

	if v, verdict := e.verify(personalIn.Token, true); verdict != tokenVerified || v.Principal != svcSponsor || v.service != nil {
		t.Errorf("the gateway resolver on a person's inbound token = %v %+v, want verified as %s", verdict, v, svcSponsor)
	}
	if v, verdict := e.verify(svcIn.Token, true); verdict != tokenVerified || v.service == nil || v.service.Name != svcName {
		t.Errorf("the gateway resolver on a service's inbound token = %v %+v, want verified as %s", verdict, v, svcName)
	}
	for _, raw := range []string{personalAPI.Token, svcAPI.Token, "admin-token-not-wdn"} {
		if _, verdict := e.verify(raw, true); verdict != tokenRefused {
			t.Errorf("the gateway resolver on a non-inbound bearer: %v, want refused", verdict)
		}
	}
	if _, verdict := e.verify(personalIn.Token, false); verdict != tokenRefused {
		t.Errorf("the API resolver on an inbound token: %v, want refused", verdict)
	}
}

// TestInboundToken_ListFlag: both token lists say which rows are inbound, and only those.
func TestInboundToken_ListFlag(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	decodeToken(t, e.mintPersonal(t, e.sponsor, `{"name":"in","kind":"inbound"}`))
	decodeToken(t, e.mintPersonal(t, e.sponsor, `{"name":"api"}`))
	decodeToken(t, e.mintReq(t, e.sponsor, svcName, `{"name":"in","kind":"inbound"}`))
	e.mustMint(t, e.sponsor, svcName)

	for _, path := range []string{"/api/v1/me/tokens", "/api/v1/me/service-identities/" + svcName + "/tokens"} {
		w := doSSO(t, e.h.srv, http.MethodGet, path, e.sponsor, "")
		var rows []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || w.Code != http.StatusOK || len(rows) != 2 {
			t.Fatalf("GET %s: %d %s (%v), want two rows", path, w.Code, w.Body.String(), err)
		}
		for _, row := range rows {
			flag, has := row["inbound"]
			switch row["name"] {
			case "in":
				if flag != true {
					t.Errorf("GET %s: the inbound row carries inbound=%v, want true", path, flag)
				}
			default:
				if has {
					t.Errorf("GET %s: the api row %v carries inbound, want the key absent", path, row["name"])
				}
			}
		}
	}
}

// TestInboundToken_CountsTowardTheCap: inbound tokens share the 20 live tokens of their owner, a person's
// and a service's.
func TestInboundToken_CountsTowardTheCap(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	for i := range apiTokenMaxPerPrincipal {
		kind := types.APITokenKindAPI
		if i%2 == 0 {
			kind = types.APITokenKindInbound
		}
		decodeToken(t, e.mintPersonal(t, e.sponsor, `{"name":"t","kind":"`+kind+`"}`))
		decodeToken(t, e.mintReq(t, e.sponsor, svcName, `{"name":"t","kind":"`+kind+`"}`))
	}
	for _, kind := range []string{types.APITokenKindAPI, types.APITokenKindInbound} {
		body := `{"name":"over","kind":"` + kind + `"}`
		if w := e.mintPersonal(t, e.sponsor, body); w.Code != http.StatusUnprocessableEntity || respReason(w) != reasonAPITokenCapReached {
			t.Errorf("personal mint of kind %s past the cap: %d %s, want 422 %s", kind, w.Code, w.Body.String(), reasonAPITokenCapReached)
		}
		if w := e.mintReq(t, e.sponsor, svcName, body); w.Code != http.StatusUnprocessableEntity || respReason(w) != reasonAPITokenCapReached {
			t.Errorf("service mint of kind %s past the cap: %d %s, want 422 %s", kind, w.Code, w.Body.String(), reasonAPITokenCapReached)
		}
	}
}

// TestInboundToken_SponsorChangeRevokes: a sponsor change revokes a service's inbound tokens with its api
// ones, so the gateway resolver refuses them at once.
func TestInboundToken_SponsorChangeRevokes(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	in := decodeToken(t, e.mintReq(t, e.sponsor, svcName, `{"name":"in","kind":"inbound"}`))
	if _, verdict := e.verify(in.Token, true); verdict != tokenVerified {
		t.Fatalf("the inbound token before the change: %v, want verified", verdict)
	}
	w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, `{"sponsor":"`+svcSponsor2+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("sponsor change: %d %s", w.Code, w.Body.String())
	}
	if _, verdict := e.verify(in.Token, true); verdict != tokenRefused {
		t.Errorf("the inbound token after the change: %v, want refused", verdict)
	}
	toks, err := e.st.ListAPITokensByPrincipal(context.Background(), svcPrin)
	if err != nil || len(toks) != 1 || toks[0].RevokedAt == nil || !toks[0].Inbound {
		t.Errorf("the identity's tokens after the change = %+v (%v), want the one inbound token revoked", toks, err)
	}
	if rows := e.auditRows("service_identity.update"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"tokens_revoked":1`) {
		t.Errorf("service_identity.update rows = %+v, want one with 1 token revoked", rows)
	}
}
