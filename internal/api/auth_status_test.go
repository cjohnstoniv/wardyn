// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── B6-F2 + B6-F5: a rejected session gets its OWN answer ───────────────────

// TestSessionRejectionResponse is the reason→answer table B6-F2 and B6-F5 are
// both one edit of. The 503 is the load-bearing row: a revocation-store outage
// used to fall through to adminAuth and answer 401 "missing bearer token",
// which drove the console's sign-in gate, which drove a successful SSO login
// (the callback never consults revocations), which answered 401 again — a
// sign-in LOOP during a Postgres incident, reported to the operator as a
// credential problem.
func TestSessionRejectionResponse(t *testing.T) {
	cases := []struct {
		reason     string
		wantStatus int
		wantBody   string
		wantOK     bool
	}{
		{sessionRevocationUnavailable, http.StatusServiceUnavailable, sessionUnavailableMsg, true},
		{sessionExpired, http.StatusUnauthorized, sessionExpiredMsg, true},
		{sessionRevoked, http.StatusUnauthorized, sessionRevokedMsg, true},
		{sessionInvalid, http.StatusUnauthorized, sessionInvalidMsg, true},
		// No cookie at all is not a rejection: the bearer lane must keep its
		// generic answer, or every CLI call would start claiming a session.
		{"", 0, "", false},
		{"something_new_upstream", 0, "", false},
	}
	for _, tc := range cases {
		status, msg, ok := sessionRejectionResponse(tc.reason)
		if ok != tc.wantOK || status != tc.wantStatus || msg != tc.wantBody {
			t.Errorf("sessionRejectionResponse(%q) = (%d, %q, %v), want (%d, %q, %v)",
				tc.reason, status, msg, ok, tc.wantStatus, tc.wantBody, tc.wantOK)
		}
	}
	if !strings.Contains(sessionExpiredMsg, "expired") {
		t.Errorf("the expired body must NAME the expiry: %q", sessionExpiredMsg)
	}
}

// expiredSSOSession mints the cookie shape ssoSession does, with an expiry in
// the past — what oidc.Middleware rejects as "expired_session".
func expiredSSOSession(t *testing.T, sub, email, role string) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: email, Role: role,
		Expiry: time.Now().UTC().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	mac := hmac.New(sha256.New, nil)
	mac.Write(payload)
	return &http.Cookie{
		Name:  "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}
}

// TestRejectedSessionAnswersItself drives the two rejection reasons a zero-value
// Authenticator can produce end to end through the REAL router. Before B6-F5,
// an OIDC-only deployment (admin token unset, which cmd/wardynd itself
// recommends) answered every one of them "admin token not configured; public
// API disabled" — a sentence about the deployment's configuration, for a human
// whose only problem is that their session ran out.
func TestRejectedSessionAnswersItself(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	t.Run("expired", func(t *testing.T) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/runs",
			expiredSSOSession(t, "sub-expired", "e@corp.example", oidc.RoleAdmin), "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), sessionExpiredMsg) {
			t.Errorf("body = %s, want it to name the expiry (%q)", w.Body.String(), sessionExpiredMsg)
		}
	})

	t.Run("tampered", func(t *testing.T) {
		bad := ssoSession(t, "sub-tampered", "t@corp.example", oidc.RoleAdmin)
		bad.Value = bad.Value[:len(bad.Value)-4] + "AAAA"
		w := doSSO(t, srv, http.MethodGet, "/api/v1/runs", bad, "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), sessionInvalidMsg) {
			t.Errorf("body = %s, want %q", w.Body.String(), sessionInvalidMsg)
		}
	})

	// NEGATIVE CONTROL: no cookie at all is the ordinary non-browser client and
	// must keep the generic bearer answer — the reason enum is about a session
	// that WAS presented.
	t.Run("no cookie keeps the generic bearer answer", func(t *testing.T) {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/runs", nil, "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "missing bearer token") {
			t.Errorf("body = %s, want the generic \"missing bearer token\"", w.Body.String())
		}
	})

	// A VALID admin bearer alongside a dead cookie still authenticates: the
	// short-circuit must not turn a stale browser cookie into a 401 for the CLI
	// token that came with it.
	t.Run("a valid bearer beats a dead cookie", func(t *testing.T) {
		// /metrics: behind the same humanOrAdminAuth gate, and it needs no store.
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		r.AddCookie(expiredSSOSession(t, "sub-expired", "e@corp.example", oidc.RoleAdmin))
		r.Header.Set("Authorization", "Bearer "+adminToken)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})
}

// errRevocations is a SessionRevocations whose store is down — the condition
// B6-F2 is about, on the lane where it IS reachable end to end.
type errRevocations struct{ err error }

func (e errRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return false, e.err
}

func (e errRevocations) RevokeSub(context.Context, string) error { return nil }
func (e errRevocations) RevokeAll(context.Context) error         { return nil }

// TestAPITokenRevocationOutageIs503 pins the api-token half of B6-F2: the same
// unanswerable revocation check that now 503s on the SSO lane answered 500
// here, so the two lanes disagreed about whether a database incident is the
// client's fault. 503 is the honest status — retry later, nothing is wrong with
// the credential — and the store-error series must move for it.
func TestAPITokenRevocationOutageIs503(t *testing.T) {
	h := newHarness(t)
	st := newTokenMemStore()
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.SessionRevocations = errRevocations{err: store.ErrNotFound}
	srv := New(cfg)

	const raw = "wdn_outage"
	if _, err := st.CreateAPIToken(context.Background(), types.APIToken{
		ID: uuid.New(), Principal: "sub-alice", Email: "alice@corp.example",
		Role: string(oidc.RoleAdmin), Name: "ci", CreatedAt: time.Now().UTC(),
	}, raw); err != nil {
		t.Fatal(err)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/runs", raw, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — an unanswerable revocation check is the deployment's "+
			"problem, not the token's; body=%s", w.Code, w.Body.String())
	}
	body := do(t, srv, http.MethodGet, "/metrics", adminToken, "").Body.String()
	if !strings.Contains(body, "wardyn_auth_store_errors_total 1") {
		t.Errorf("wardyn_auth_store_errors_total did not move during the outage:\n%s", body)
	}
}

// ─── B6-F4: driver text stays in the log, never in the body ──────────────────

// errRunStore fails GetRun with a wrapped pgx-shaped error: the real ones carry
// the DB host, port, user and database name plus the SQLSTATE and the table or
// constraint that refused.
type errRunStore struct {
	store.Store
	err error
}

func (s errRunStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{}, s.err
}

// TestServerErrorsDoNotLeakDriverText pins B6-F4: ~149 sites wrote
// `err.Error()` straight into a 5xx body, and the member tier reaches plenty of
// them — so an ordinary member could read the deployment's database host, port
// and schema out of a transient store failure. The chokepoint logs the error and
// writes only the operator-facing sentence.
func TestServerErrorsDoNotLeakDriverText(t *testing.T) {
	const secret = "host=10.0.0.5 port=5432 user=wardyn database=wardyn"
	h := newHarness(t)
	cfg := baseTestConfig(h, errRunStore{err: errors.New("pgx: dial: " + secret)})
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+uuid.New().String(), adminToken, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Errorf("the 5xx body leaked the driver's connection string:\n%s", w.Body.String())
	}
	if !strings.Contains(logged.String(), secret) {
		t.Errorf("the driver error was dropped instead of logged — the operator now has nothing:\n%s", logged.String())
	}
}
