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

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/scim"
)

const (
	scimPrimaryToken = "scim-primary-token-0123456789abcdef0123456789"
	scimNextToken    = "scim-next-token-0123456789abcdef0123456789abcd"
)

func scimAuthServer(t *testing.T, mut func(*Config)) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.SCIM = &SCIMConfig{Token: scimPrimaryToken, TokenNext: scimNextToken,
		Issuer: "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0", Tenant: "11111111-2222-3333-4444-555555555555"}
	if mut != nil {
		mut(&cfg)
	}
	return New(cfg), h
}

func scimGet(t *testing.T, srv *Server, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/scim/v2/Users?filter="+`userName%20eq%20%22x%22`, nil)
	r.RemoteAddr = "192.0.2.7:4000"
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, r)
	return w
}

// Without a token configured there is no SCIM route at all.
func TestSCIMRoutesAreNotMountedWithoutAToken(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, rbacStore{}))
	if w := scimGet(t, srv, scimPrimaryToken); w.Code != http.StatusNotFound {
		t.Fatalf("GET /scim/v2/Users with SCIM off = %d, want 404", w.Code)
	}
}

// A bearer that is missing, wrong, the admin token or a longer or shorter prefix of the real one is a
// 401 in the SCIM envelope and an auth.fail row from wardyn/scimAuth that says the token was invalid and
// nothing else: not the presented value, not its digest, not which slot it nearly matched.
func TestSCIMAuthRefusesAnyOtherBearer(t *testing.T) {
	srv, h := scimAuthServer(t, nil)
	for name, bearer := range map[string]string{
		"missing":       "",
		"wrong":         "not-the-scim-token-at-all-0123456789abcdef0123",
		"admin token":   adminToken,
		"a prefix":      scimPrimaryToken[:len(scimPrimaryToken)-1],
		"a longer one":  scimPrimaryToken + "x",
		"the other cfg": strings.ToUpper(scimPrimaryToken),
	} {
		t.Run(name, func(t *testing.T) {
			w := scimGet(t, srv, bearer)
			if w.Code != http.StatusUnauthorized || w.Header().Get("Content-Type") != scim.MediaType {
				t.Fatalf("status %d content-type %q, want 401 application/scim+json: %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
			}
			var env struct {
				Schemas []string `json:"schemas"`
				Status  string   `json:"status"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Status != "401" || len(env.Schemas) != 1 || env.Schemas[0] != scim.SchemaError {
				t.Errorf("body %s is not the RFC 7644 error envelope", w.Body.String())
			}
			if bearer != "" && strings.Contains(w.Body.String(), bearer) {
				t.Error("the response echoes the presented bearer")
			}
		})
	}
	rows := authFailReasons(h.audit.snapshot(), scimAuthActor)
	if len(rows) == 0 {
		t.Fatal("no auth.fail row from wardyn/scimAuth")
	}
	for _, r := range rows {
		if r != "invalid_scim_token" {
			t.Errorf("auth.fail reason = %q, want invalid_scim_token", r)
		}
	}
	for _, ev := range h.audit.snapshot() {
		if ev.Actor != scimAuthActor {
			continue
		}
		for _, secret := range []string{scimPrimaryToken, scimNextToken, adminToken, "primary", "next", "slot"} {
			if strings.Contains(string(ev.Data)+ev.Target, secret) {
				t.Errorf("the refusal row carries %q: %s", secret, ev.Data)
			}
		}
	}
}

// Both configured bearers are admitted, each as its own slot, which rides the request context.
func TestSCIMAuthAdmitsEitherSlot(t *testing.T) {
	for slot, token := range map[string]string{scimSlotPrimary: scimPrimaryToken, scimSlotNext: scimNextToken} {
		srv, _ := scimAuthServer(t, nil)
		var got string
		var admitted bool
		h := srv.scimAuth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, admitted = scimCallerSlot(r.Context()) }))
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(httptest.NewRecorder(), r)
		if !admitted || got != slot {
			t.Errorf("token for slot %q admitted=%v as %q", slot, admitted, got)
		}
	}
}

// Admitted requests are limited per slot with a 429 and Retry-After, and a refused bearer, which has no
// slot, shares one bucket however many sources it comes from, so guessing is bounded.
func TestSCIMAuthRateLimits(t *testing.T) {
	frozen := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	srv, _ := scimAuthServer(t, func(c *Config) { c.Now = func() time.Time { return frozen } })

	for i := 0; i < int(scimBurst); i++ {
		if w := scimGet(t, srv, scimPrimaryToken); w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d of the burst was limited", i+1)
		}
	}
	w := scimGet(t, srv, scimPrimaryToken)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("past the burst: %d Retry-After %q, want 429 with one", w.Code, w.Header().Get("Retry-After"))
	}
	if w := scimGet(t, srv, scimNextToken); w.Code == http.StatusTooManyRequests {
		t.Error("the other slot shares the first slot's bucket")
	}

	for i := 0; i < int(scimRefusedBurst); i++ {
		if w := scimGet(t, srv, "wrong-token-"+string(rune('a'+i))); w.Code != http.StatusUnauthorized {
			t.Fatalf("refused request %d = %d, want 401", i+1, w.Code)
		}
	}
	w = scimGet(t, srv, "wrong-token-again")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("past the refused burst: %d Retry-After %q, want 429 with one", w.Code, w.Header().Get("Retry-After"))
	}
}

// A SCIM-authenticated request is never an operator of either tier, whatever else its context holds: the
// no-human arm of isOperator would otherwise read it as the admin token.
func TestSCIMCallerIsNeverAnOperator(t *testing.T) {
	srv, _ := scimAuthServer(t, nil)
	plain := context.Background()
	if !srv.isOperator(plain) || !srv.isSecurityOperator(plain) {
		t.Fatal("control: a context with no human must read as the admin token")
	}
	for _, slot := range []string{scimSlotPrimary, scimSlotNext} {
		ctx := withSCIMCaller(plain, slot)
		if srv.isOperator(ctx) || srv.isSecurityOperator(ctx) || !neverOperator(ctx) {
			t.Errorf("a SCIM caller (%s) answers as an operator", slot)
		}
		admin := withSCIMCaller(withHumanIdentity(plain, "root", "root@corp.example", oidc.RoleAdmin, "standard", nil, false), slot)
		if srv.isOperator(admin) || srv.isSecurityOperator(admin) {
			t.Errorf("a SCIM caller (%s) carrying an admin session answers as an operator", slot)
		}
	}
}
