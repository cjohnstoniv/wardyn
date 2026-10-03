// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// expiryTestServer is apiTokenTestServer with a deployment cap on token lifetime.
func expiryTestServer(t *testing.T, maxTTL time.Duration) (*Server, *tokenMemStore, *harness) {
	t.Helper()
	h := newHarness(t)
	st := newTokenMemStore()
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.APITokenMaxTTL = maxTTL
	return New(cfg), st, h
}

func mintBody(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	sess := ssoSession(t, tokenMemberSub, tokenMemberMail, oidc.RoleUser)
	return doSSO(t, srv, http.MethodPost, "/api/v1/me/tokens", sess, body)
}

// TestAPITokenMint_TTLAgainstCap pins the mint semantics: omitted and zero are
// the same request (the console sends neither), the cap fills an unasked
// lifetime and clamps an over-long one, and a clamp is on the audit row.
func TestAPITokenMint_TTLAgainstCap(t *testing.T) {
	const hour = time.Hour
	cases := []struct {
		name        string
		maxTTL      time.Duration
		body        string
		wantLife    time.Duration // 0 = never expires
		wantClamped int64         // ttl_clamped_from_seconds, 0 = absent
	}{
		{"omitted, no cap", 0, `{"name":"ci"}`, 0, 0},
		{"zero, no cap", 0, `{"name":"ci","ttl_seconds":0}`, 0, 0},
		{"omitted, cap", hour, `{"name":"ci"}`, hour, 0},
		{"zero, cap", hour, `{"name":"ci","ttl_seconds":0}`, hour, 0},
		{"explicit, no cap", 0, `{"name":"ci","ttl_seconds":90}`, 90 * time.Second, 0},
		{"explicit under cap", hour, `{"name":"ci","ttl_seconds":90}`, 90 * time.Second, 0},
		{"explicit at cap", hour, `{"name":"ci","ttl_seconds":3600}`, hour, 0},
		{"explicit over cap", hour, `{"name":"ci","ttl_seconds":7200}`, hour, 7200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, h := expiryTestServer(t, tc.maxTTL)
			w := mintBody(t, srv, tc.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("mint: code = %d, want 201; body=%s", w.Code, w.Body.String())
			}
			var created types.APIToken
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if tc.wantLife == 0 {
				if created.ExpiresAt != nil {
					t.Errorf("expires_at = %v, want none", created.ExpiresAt)
				}
				if strings.Contains(w.Body.String(), "expires_at") {
					t.Errorf("a never-expiring token's body names expires_at: %s", w.Body.String())
				}
			} else {
				if created.ExpiresAt == nil {
					t.Fatalf("expires_at = nil, want created_at + %s", tc.wantLife)
				}
				if got := created.ExpiresAt.Sub(created.CreatedAt); got != tc.wantLife {
					t.Errorf("lifetime = %s, want %s", got, tc.wantLife)
				}
			}
			var data map[string]any
			if err := json.Unmarshal(lastAuditEvent(t, h.audit.events, "token.create").Data, &data); err != nil {
				t.Fatal(err)
			}
			got, clamped := data["ttl_clamped_from_seconds"].(float64)
			if int64(got) != tc.wantClamped || clamped != (tc.wantClamped != 0) {
				t.Errorf("token.create ttl_clamped_from_seconds = %v (present %v), want %d", data["ttl_clamped_from_seconds"], clamped, tc.wantClamped)
			}
		})
	}
}

// TestAPITokenMint_NegativeTTLIsRefused: a negative TTL is a 400 before anything
// is minted or audited, whatever the cap says.
func TestAPITokenMint_NegativeTTLIsRefused(t *testing.T) {
	for _, maxTTL := range []time.Duration{0, time.Hour} {
		srv, st, h := expiryTestServer(t, maxTTL)
		for _, body := range []string{`{"name":"ci","ttl_seconds":-1}`, `{"name":"ci","ttl_seconds":9223372036854775807}`} {
			w := mintBody(t, srv, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("cap %s, %s: code = %d, want 400; body=%s", maxTTL, body, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), reasonAPITokenTTLInvalid) {
				t.Errorf("body %q does not carry reason %q", w.Body.String(), reasonAPITokenTTLInvalid)
			}
		}
		if len(st.byID) != 0 {
			t.Errorf("cap %s: %d tokens minted by refused requests, want 0", maxTTL, len(st.byID))
		}
		for _, ev := range h.audit.events {
			if ev.Action == "token.create" {
				t.Errorf("cap %s: refused mint wrote a token.create row", maxTTL)
			}
		}
	}
}

// TestAPITokenAuth_ExpiryBoundary: one second before expires_at the token
// authenticates, one second after it gets the 401 a revoked token gets, byte for
// byte, and a token with no expires_at never expires.
func TestAPITokenAuth_ExpiryBoundary(t *testing.T) {
	srv, st, _ := apiTokenTestServer(t)
	expiresAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	seed := func(raw string, exp *time.Time) {
		t.Helper()
		if _, err := st.CreateAPIToken(t.Context(), types.APIToken{
			ID: uuid.New(), Principal: tokenMemberSub, Role: oidc.RoleUser, GroupsTruncated: new(bool), ExpiresAt: exp,
		}, raw); err != nil {
			t.Fatal(err)
		}
	}
	const expiring, forever = apiTokenPrefix + "expiring", apiTokenPrefix + "forever"
	seed(expiring, &expiresAt)
	seed(forever, nil)

	at := func(now time.Time) { st.clock = func() time.Time { return now } }
	at(expiresAt.Add(-time.Second))
	if w := do(t, srv, http.MethodGet, "/api/v1/me", expiring, ""); w.Code != http.StatusOK {
		t.Fatalf("1s before expiry: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	at(expiresAt.Add(time.Second))
	expired := do(t, srv, http.MethodGet, "/api/v1/me", expiring, "")
	if expired.Code != http.StatusUnauthorized {
		t.Fatalf("1s after expiry: code = %d, want 401; body=%s", expired.Code, expired.Body.String())
	}
	if w := do(t, srv, http.MethodGet, "/api/v1/me", forever, ""); w.Code != http.StatusOK {
		t.Errorf("a token with no expires_at, 1s after another's: code = %d, want 200", w.Code)
	}

	// The same status and body a revoked token gets.
	sess := ssoSession(t, tokenMemberSub, tokenMemberMail, oidc.RoleUser)
	// A console session is untouched by token expiry: it keeps its own cookie expiry.
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/me", sess, ""); w.Code != http.StatusOK {
		t.Errorf("session after a token expired: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	raw, created := mintToken(t, srv, sess, "ci")
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/tokens/"+created.ID.String(), sess, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: code = %d", w.Code)
	}
	revoked := do(t, srv, http.MethodGet, "/api/v1/me", raw, "")
	if revoked.Code != expired.Code || revoked.Body.String() != expired.Body.String() {
		t.Errorf("expired = %d %q, revoked = %d %q; they must be indistinguishable",
			expired.Code, expired.Body.String(), revoked.Code, revoked.Body.String())
	}
}

// TestAPITokenMint_ExpiredTokensDoNotCountAgainstTheCap: an expired token is not
// live, so it must not hold one of the principal's mint slots.
func TestAPITokenMint_ExpiredTokensDoNotCountAgainstTheCap(t *testing.T) {
	srv, st, _ := apiTokenTestServer(t)
	past := time.Now().Add(-time.Hour)
	for i := 0; i < apiTokenMaxPerPrincipal; i++ {
		if _, err := st.CreateAPIToken(t.Context(), types.APIToken{
			ID: uuid.New(), Principal: tokenMemberSub, Role: oidc.RoleUser, ExpiresAt: &past,
		}, apiTokenPrefix+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	if w := mintBody(t, srv, `{"name":"ci"}`); w.Code != http.StatusCreated {
		t.Fatalf("mint with %d expired tokens held: code = %d, want 201; body=%s", apiTokenMaxPerPrincipal, w.Code, w.Body.String())
	}
}
