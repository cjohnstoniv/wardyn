// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// P2-4 (0.8 testing plan T-23): the session-codec-v2 boundary as the REST
// router actually sees it, not just oidc.Middleware in isolation. A pre-0.8
// (codec v1) cookie, or a v2 cookie with no user type, must never authenticate
// a request AND must never turn into a 5xx — decodeSession's fail-closed
// refusal (internal/auth/oidc/session_codec.go) has to survive the trip
// through humanOrAdminAuth, adminAuth's fallthrough, and every handler in
// between without a nil-UserType or unknown-version value reaching one of
// them. A defect here is a caller getting authenticated on a session field
// this binary does not define, or a panic (500) an attacker can trigger by
// simply holding an old cookie past an upgrade.
//
// The payloads below are hand-rolled JSON rather than an oidc.Session so V and
// the "ut" key can be set independently of what a live encodeSession would
// ever write; they are signed by signedSessionCookie, the same signer
// ssoSession uses.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestSessionCodecBoundary_NeverAuthenticatesNeverServerErrors is the HTTP-level
// pin for P2-4: every shape of "not a current session" reaches GET /api/v1/me
// as a plain 401 (adminAuth's "missing bearer token" fallthrough — no
// Authorization header rides along), never a 5xx, and never trips
// panicFails' recovered-panic guard (doSSO wraps every call in it).
func TestSessionCodecBoundary_NeverAuthenticatesNeverServerErrors(t *testing.T) {
	srv := rbacServer(t)
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)

	cases := []struct {
		name    string
		payload string
	}{
		{
			// The literal shape a real pre-0.8 binary wrote: no "ut" key at all,
			// "role":"member" (the retired tier word), no "v" key (decodes to 0).
			name:    "real pre-0.8 cookie, no v key, no ut key",
			payload: fmt.Sprintf(`{"sub":"sub-legacy","email":"legacy@corp.example","role":"member","expiry":%q}`, exp),
		},
		{
			// A codec-1 cookie forged to ALSO carry a user type — isolates the
			// version check from the empty-UserType check: even a fully-populated
			// v1 payload must not pass.
			name:    "codec v1 cookie with a forged ut",
			payload: fmt.Sprintf(`{"v":1,"sub":"sub-07","email":"m@corp.example","role":"member","ut":"standard","expiry":%q}`, exp),
		},
		{
			// A current-version cookie with no type at all — the corrupt/short
			// payload case, distinct from the version mismatch above.
			name:    "codec v2 cookie with no ut",
			payload: fmt.Sprintf(`{"v":%d,"sub":"sub-x","email":"x@corp.example","role":%q,"expiry":%q}`, oidc.SessionCodecVersion, oidc.RoleUser, exp),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/me", signedSessionCookie([]byte(c.payload)), "")
			if w.Code >= 500 {
				t.Fatalf("GET /me with %s = %d (a 5xx): %s", c.name, w.Code, w.Body.String())
			}
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("GET /me with %s = %d, want 401 (the session must not authenticate): %s", c.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestSessionCodecBoundary_CurrentCookieStillAuthenticates is the positive
// control for the table above: a codec-v2 cookie with a user type DOES
// authenticate, both as ssoSession writes it and as a hand-rolled payload in
// the table's own shape, so the 401s above are the version/type guard firing,
// not a broken signer or a mistyped payload key.
func TestSessionCodecBoundary_CurrentCookieStillAuthenticates(t *testing.T) {
	srv := rbacServer(t)
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	cookies := map[string]*http.Cookie{
		"ssoSession": ssoSession(t, "sub-ok", "ok@corp.example", oidc.RoleUser),
		"hand-rolled codec v2 payload with ut": signedSessionCookie([]byte(fmt.Sprintf(
			`{"v":%d,"sub":"sub-ok","email":"ok@corp.example","role":%q,"ut":"standard","expiry":%q}`,
			oidc.SessionCodecVersion, oidc.RoleUser, exp))),
	}
	for name, cookie := range cookies {
		t.Run(name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/me", cookie, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET /me with a current session = %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}
