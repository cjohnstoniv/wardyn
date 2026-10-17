// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// objectIDThroughMiddleware is the object id Middleware publishes for cookie,
// and whether the cookie authenticated at all.
func objectIDThroughMiddleware(t *testing.T, auth *writoidc.Authenticator, cookie *http.Cookie) (oid string, authenticated bool) {
	t.Helper()
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		oid = writoidc.ObjectIDFromContext(r.Context())
		authenticated = writoidc.PrincipalFromContext(r.Context()) != ""
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(cookie)
	auth.Middleware(next).ServeHTTP(httptest.NewRecorder(), r)
	return oid, authenticated
}

// An Entra sign-in stores the token's object id in the session; any other
// issuer never does, whatever its token carries, and a value that is not an
// object id is not stored.
func TestCallbackStoresTheEntraObjectID(t *testing.T) {
	for _, c := range []struct {
		name, oid, want string
		entra           bool
	}{
		{"entra stores it", keyObject, keyObject, true},
		{"entra folds case", strings.ToUpper(keyObject), keyObject, true},
		{"entra with no oid", "", "", true},
		{"entra with a value that is not an object id", strings.Repeat("x", 4000), "", true},
		{"a non-Entra issuer never sets it", keyObject, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newRoleMappingAuth(t, nil, writoidc.RoleUser, nil, nil)
			if c.entra {
				writoidc.SetEntraForTest(auth)
			}
			env.entraIDToken(t, "pairwise-1", keyTenant, c.oid)
			w, sess := doCallbackVia(t, auth, auth.CallbackHandler)
			if sess.Sub == "" {
				t.Fatalf("no session: Location %q", w.Result().Header.Get("Location"))
			}
			if sess.ObjectID != c.want {
				t.Errorf("session object id = %q, want %q", sess.ObjectID, c.want)
			}
		})
	}
}

// A cookie written before the field existed still authenticates, carrying no
// object id, and one carrying it publishes it.
func TestSessionObjectIDIsOptionalInTheCookie(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)

	old := []byte(fmt.Sprintf(`{"v":%d,"sub":"sub-x","email":"x@example.com","role":%q,"ut":"standard","expiry":%q,"groups":[]}`,
		writoidc.SessionCodecVersion, writoidc.RoleUser, exp))
	if oid, ok := objectIDThroughMiddleware(t, auth, writoidc.EncodeRawSessionForTest(auth, old)); !ok || oid != "" {
		t.Fatalf("a cookie with no object id: authenticated %v, object id %q; want authenticated, none", ok, oid)
	}

	cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
		Sub: "sub-x", Email: "x@example.com", Role: writoidc.RoleUser, UserType: "standard", ObjectID: keyObject,
		Expiry: time.Now().Add(time.Hour), Groups: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if oid, ok := objectIDThroughMiddleware(t, auth, cookie); !ok || oid != keyObject {
		t.Fatalf("a cookie with an object id: authenticated %v, object id %q; want %q", ok, oid, keyObject)
	}
}

// The object id is under the cookie's MAC like every other field: changing it
// leaves the signature wrong and the cookie is not a session.
func TestTamperedSessionObjectIDFailsTheMAC(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)
	cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
		Sub: "sub-x", Email: "x@example.com", Role: writoidc.RoleUser, UserType: "standard", ObjectID: keyObject,
		Expiry: time.Now().Add(time.Hour), Groups: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(cookie.Value, ".", 2)
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || !strings.Contains(string(payload), keyObject) {
		t.Fatalf("setup: payload %q does not carry the object id (%v)", payload, err)
	}
	forged := strings.Replace(string(payload), keyObject, "00000000-0000-0000-0000-000000000000", 1)
	cookie.Value = base64.RawURLEncoding.EncodeToString([]byte(forged)) + "." + parts[1]
	if oid, ok := objectIDThroughMiddleware(t, auth, cookie); ok || oid != "" {
		t.Fatalf("a cookie with a tampered object id authenticated (object id %q)", oid)
	}
}
