// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

const (
	keyTenant = "11111111-2222-3333-4444-555555555555"
	keyObject = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	keyPerson = "entra:" + keyTenant + ":" + keyObject
)

// fakeKeying maps one exact Subject key to keyPerson, or answers refused/err;
// it records every lookup and attach.
type fakeKeying struct {
	issuer   string
	refused  bool
	err      error
	lookups  []writoidc.Subject
	attached []writoidc.Subject
}

func (f *fakeKeying) PrincipalFor(_ context.Context, s writoidc.Subject) (string, bool, error) {
	f.lookups = append(f.lookups, s)
	if f.err != nil || f.refused {
		return "", f.refused, f.err
	}
	if s.Issuer == f.issuer && s.TenantID == keyTenant && s.ObjectID == keyObject {
		return keyPerson, false, nil
	}
	return s.Sub, false, nil
}

func (f *fakeKeying) Attached(_ *http.Request, s writoidc.Subject, principal string) {
	if principal != keyPerson {
		panic("attached to " + principal)
	}
	f.attached = append(f.attached, s)
}

// entraIDToken signs an id_token for the callback carrying Entra's tid/oid.
func (e *idpEnv) entraIDToken(t *testing.T, sub, tid, oid string) {
	t.Helper()
	e.latestIDTok = e.sign(t, map[string]any{
		"iss": e.httpSrv.URL, "sub": sub, "aud": e.clientID, "email": "pat@corp.example", "email_verified": true,
		"nonce": roleCallbackNonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		"tid": tid, "oid": oid,
	})
}

// TestCallbackPersonKeying pins the sign-in half of #1195: on Entra a login
// becomes a person set up by object id only when the keying says its exact
// (issuer, tid, oid) is theirs, and then OnLogin — the stamp refresh that ends
// the unknown-groups stamp — and the attach record see that person. Every
// other outcome leaves the person untouched: a login that does not match keeps
// its own sub, a refusal or lookup failure issues no session and never reaches
// OnLogin, a reserved sub is refused before the keying is asked, and a
// non-Entra issuer never asks it at all.
func TestCallbackPersonKeying(t *testing.T) {
	reserved := func(sub string) bool { return sub == "admin-token" }
	for _, c := range []struct {
		name                   string
		entra                  bool
		sub, oid               string
		refused                bool
		err                    error
		wantSub, wantAuthError string
		wantLookups, wantAttch int
		wantDenied             []string
	}{
		{name: "exact key attaches", entra: true, sub: "pairwise-1", oid: keyObject, wantSub: keyPerson, wantLookups: 1, wantAttch: 1},
		{name: "another object id keeps its own sub", entra: true, sub: "pairwise-2", oid: "aaaaaaaa-bbbb-cccc-dddd-000000000000", wantSub: "pairwise-2", wantLookups: 1},
		{name: "refused", entra: true, sub: keyPerson, oid: "aaaaaaaa-bbbb-cccc-dddd-000000000000", refused: true,
			wantAuthError: "sign_in_refused", wantLookups: 1, wantDenied: []string{writoidc.DenialReservedPrincipal}},
		{name: "lookup error fails closed", entra: true, sub: "pairwise-3", oid: keyObject, err: errors.New("pg down"),
			wantAuthError: "role_check_unavailable", wantLookups: 1},
		{name: "reserved sub never reaches the keying", entra: true, sub: "admin-token", oid: keyObject,
			wantAuthError: "sign_in_refused", wantDenied: []string{writoidc.DenialReservedPrincipal}},
		{name: "non-Entra issuer never asks", sub: "dex-sub", oid: keyObject, wantSub: "dex-sub"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			var logins []string
			auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) {
				cfg.OnLogin = func(_ context.Context, f writoidc.LoginFacts) { logins = append(logins, f.Sub) }
			})
			if c.entra {
				writoidc.SetEntraForTest(auth)
			}
			k := &fakeKeying{issuer: env.httpSrv.URL, refused: c.refused, err: c.err}
			auth.AttachPersonKeying(k)
			env.entraIDToken(t, c.sub, keyTenant, c.oid)
			var denied []string
			w, sess := doCallbackVia(t, auth, auth.CallbackHandlerWithDenials(reserved, func(_ *http.Request, reason string) {
				denied = append(denied, reason)
			}))

			if sess.Sub != c.wantSub {
				t.Errorf("session sub = %q, want %q", sess.Sub, c.wantSub)
			}
			if c.wantAuthError != "" {
				if loc := w.Result().Header.Get("Location"); !containsAuthError(loc, c.wantAuthError) {
					t.Errorf("Location = %q, want auth_error=%s", loc, c.wantAuthError)
				}
				if len(logins) != 0 {
					t.Errorf("OnLogin ran for a refused sign-in: %v", logins)
				}
			} else if !slices.Equal(logins, []string{c.wantSub}) {
				t.Errorf("OnLogin subs = %v, want [%s]", logins, c.wantSub)
			}
			if !slices.Equal(denied, c.wantDenied) {
				t.Errorf("denials = %v, want %v", denied, c.wantDenied)
			}
			if len(k.lookups) != c.wantLookups || len(k.attached) != c.wantAttch {
				t.Fatalf("lookups %d, attaches %d; want %d, %d", len(k.lookups), len(k.attached), c.wantLookups, c.wantAttch)
			}
			want := writoidc.Subject{Issuer: env.httpSrv.URL, Sub: c.sub, TenantID: keyTenant, ObjectID: c.oid}
			for _, got := range append(k.lookups, k.attached...) {
				if got != want {
					t.Errorf("keying saw %+v, want the token's exact %+v", got, want)
				}
			}
		})
	}
}

// TestVerifySubjectTokenPersonKeying: a portal's token exchange admits through
// the same keying as the callback, so it acts as the same person.
func TestVerifySubjectTokenPersonKeying(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)
	writoidc.SetEntraForTest(auth)
	k := &fakeKeying{issuer: env.httpSrv.URL}
	auth.AttachPersonKeying(k)
	c := env.subjectClaims()
	c["tid"], c["oid"] = keyTenant, keyObject
	r := httptest.NewRequest(http.MethodPost, "/api/v1/token", nil)
	sess, denied := auth.VerifySubjectToken(r, env.sign(t, c), testPortalClient, nil)
	if denied != "" || sess.Sub != keyPerson || len(k.attached) != 0 {
		t.Fatalf("exchange = %q, %q, attaches %d; want the object-id person, no attach before the caller admits it", sess.Sub, denied, len(k.attached))
	}
	auth.RecordAttach(r, sess)
	auth.RecordAttach(r, writoidc.Session{Sub: "someone-else"})
	if len(k.attached) != 1 || k.attached[0].Sub != "person-sub" {
		t.Fatalf("attaches after RecordAttach = %+v, want exactly this exchange's", k.attached)
	}
}

// TestCallbackLoginGrantGoesToThePerson: the downstream credential a sign-in
// captures is stored under the principal it signed in as, so a person's runs
// find it, not under the pairwise sub.
func TestCallbackLoginGrantGoesToThePerson(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleMappingAuth(t, nil, "", nil, nil)
	writoidc.SetEntraForTest(auth)
	auth.AttachPersonKeying(&fakeKeying{issuer: env.httpSrv.URL})
	sink := &stubSink{}
	auth.AttachLoginGrantSink(sink)
	env.tokenSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "rt", "id_token": env.latestIDTok,
		})
	})
	env.entraIDToken(t, "pairwise-1", keyTenant, keyObject)
	if _, sess := doCallbackVia(t, auth, auth.CallbackHandler); sess.Sub != keyPerson || !slices.Equal(sink.subjects, []string{keyPerson}) {
		t.Fatalf("session %q, grant stored under %v; want both %s", sess.Sub, sink.subjects, keyPerson)
	}
}
