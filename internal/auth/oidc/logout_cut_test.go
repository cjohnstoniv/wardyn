// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// cutLedger is a store that orders cuts by sequence the way the database one does: a session is revoked
// when a cut for its sub drew a greater sequence than the one its cookie recorded. It counts RevokeSub, the
// call that would also end tokens and keys, which a sign-out must never make.
type cutLedger struct {
	seq        int64
	cuts       map[string]int64
	cutErr     error
	revokedSub int
	cutCalls   int
}

func (l *cutLedger) RevocationSeq(context.Context) (int64, error) { return l.seq, nil }
func (l *cutLedger) IsSessionRevokedSeq(_ context.Context, sub, _ string, seq int64) (bool, error) {
	return l.cuts[sub] > seq, nil
}
func (l *cutLedger) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (l *cutLedger) CutSessions(_ context.Context, sub string) error {
	if l.cutErr != nil {
		return l.cutErr
	}
	l.cutCalls++
	l.seq++
	if l.cuts == nil {
		l.cuts = map[string]int64{}
	}
	l.cuts[sub] = l.seq
	return nil
}
func (l *cutLedger) RevokeSub(context.Context, string) error { l.revokedSub++; return nil }
func (l *cutLedger) RevokeAll(context.Context) error         { return nil }
func (l *cutLedger) SessionStatus(ctx context.Context, sub, email string, issue writoidc.Issue, _ int64) (writoidc.SessionStatus, error) {
	if revoked, _ := writoidc.IsRevoked(ctx, l, sub, email, issue); revoked {
		return writoidc.SessionRevoked, nil
	}
	return writoidc.SessionLive, nil
}

func signedInCookie(t *testing.T, env *idpEnv, auth *writoidc.Authenticator) *http.Cookie {
	t.Helper()
	env.buildIDToken(t, "pairwise-1", "pat@corp.example", roleCallbackNonce, time.Now().Add(time.Hour))
	w, _ := doCallbackVia(t, auth, auth.CallbackHandler)
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.Value != "" {
			return c
		}
	}
	t.Fatalf("sign-in issued no session: %q", w.Result().Header.Get("Location"))
	return nil
}

// signOut sends the sign-out request the route gets: through Middleware, which is what establishes the
// session the cut is allowed to name.
func signOut(auth *writoidc.Authenticator, cookie *http.Cookie, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	auth.Middleware(http.HandlerFunc(auth.LogoutHandler)).ServeHTTP(w, r)
	return w
}

func replay(auth *writoidc.Authenticator, c *http.Cookie) (live bool, reason string) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		live = writoidc.PrincipalFromContext(r.Context()) != ""
		reason = writoidc.SessionRejectedFromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), r)
	return live, reason
}

// Signing out writes the session cut, so a copy of the cookie taken before is refused afterwards, while a
// sign-in made after the sign-out is not ended by it and no token or key is revoked.
func TestSignOutRefusesAReplayOfTheCookie(t *testing.T) {
	env := newIdPEnv(t)
	rev := &cutLedger{seq: 10}
	auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Revocations = rev })
	cookie := signedInCookie(t, env, auth)
	if live, reason := replay(auth, cookie); !live {
		t.Fatalf("a fresh session was refused: %q", reason)
	}

	w := signOut(auth, cookie, "")
	if w.Code != http.StatusFound {
		t.Fatalf("sign-out status = %d, want %d", w.Code, http.StatusFound)
	}

	if live, reason := replay(auth, cookie); live || reason != "revoked_session" {
		t.Errorf("replay of the signed-out cookie: live=%v reason=%q; want refused as revoked_session", live, reason)
	}
	if rev.revokedSub != 0 {
		t.Errorf("sign-out called RevokeSub %d times; it must cut sessions only", rev.revokedSub)
	}
	if live, reason := replay(auth, signedInCookie(t, env, auth)); !live {
		t.Errorf("a sign-in after the sign-out was refused: %q", reason)
	}
}

// A sign-out whose cut cannot be written still clears the cookie but answers 503, so it is not read as a
// confirmed sign-out; a request with no valid cookie has nothing to cut.
func TestSignOutThatCannotCutSaysSo(t *testing.T) {
	env := newIdPEnv(t)
	rev := &cutLedger{seq: 10}
	auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Revocations = rev })
	cookie := signedInCookie(t, env, auth)

	rev.cutErr = errors.New("pg down")
	w := signOut(auth, cookie, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("sign-out with a failing cut = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		cleared = cleared || (c.Name == "wardyn_session" && c.MaxAge == -1)
	}
	if !cleared {
		t.Error("the cookie was not cleared when the cut failed")
	}

	rev.cutErr = nil
	w = signOut(auth, nil, "")
	if w.Code != http.StatusFound || len(rev.cuts) != 0 {
		t.Errorf("sign-out with no cookie = %d with cuts %v; want a redirect and no cut", w.Code, rev.cuts)
	}
}

// A cookie that only verifies cuts nobody. An expired, a revoked and a role-stale copy still carry a valid
// signature, and a request that also presents a bearer credential skips the rejected-cookie answer, so
// someone holding any token and a stale copy of another person's cookie must not end that person's sessions.
// Each is cleared and redirected like any sign-out.
func TestSignOutWithACookieThatIsNotLiveWritesNoCut(t *testing.T) {
	for _, c := range []struct {
		name   string
		bearer string
		stale  func(t *testing.T, auth *writoidc.Authenticator, rev *cutLedger) *http.Cookie
	}{
		{"expired", "", func(t *testing.T, auth *writoidc.Authenticator, _ *cutLedger) *http.Cookie {
			return forgedCookie(t, auth, time.Now().Add(-time.Minute), time.Now().Add(-time.Hour))
		}},
		{"expired, with a bearer token", "someone-elses-token", func(t *testing.T, auth *writoidc.Authenticator, _ *cutLedger) *http.Cookie {
			return forgedCookie(t, auth, time.Now().Add(-time.Minute), time.Now().Add(-time.Hour))
		}},
		{"revoked", "", func(t *testing.T, auth *writoidc.Authenticator, rev *cutLedger) *http.Cookie {
			cookie := forgedCookie(t, auth, time.Now().Add(time.Hour), time.Now())
			rev.seq, rev.cuts = 5, map[string]int64{"victim": 6}
			return cookie
		}},
		{"revoked, with a bearer token", "someone-elses-token", func(t *testing.T, auth *writoidc.Authenticator, rev *cutLedger) *http.Cookie {
			cookie := forgedCookie(t, auth, time.Now().Add(time.Hour), time.Now())
			rev.seq, rev.cuts = 5, map[string]int64{"victim": 6}
			return cookie
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			rev := &cutLedger{seq: 5}
			auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Revocations = rev })
			cookie := c.stale(t, auth, rev)
			before := rev.cutCalls
			w := signOut(auth, cookie, c.bearer)
			if w.Code != http.StatusFound {
				t.Errorf("sign-out status = %d, want %d", w.Code, http.StatusFound)
			}
			if rev.cutCalls != before {
				t.Errorf("a cookie that is not a live session wrote %d cut(s) for its sub", rev.cutCalls-before)
			}
			if rev.revokedSub != 0 {
				t.Errorf("RevokeSub was called %d times", rev.revokedSub)
			}
		})
	}
}

// forgedCookie is a validly signed cookie for the sub "victim", as a copy kept past its life would be.
func forgedCookie(t *testing.T, auth *writoidc.Authenticator, expiry, issuedAt time.Time) *http.Cookie {
	t.Helper()
	cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
		Sub: "victim", Email: "victim@corp.example", Role: writoidc.RoleUser, UserType: "standard",
		Expiry: expiry, IssuedAt: issuedAt, RevocationSeq: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cookie
}

// With no revocation store configured a live session signs out to the redirect, with no cut to write and
// nothing to dereference.
func TestSignOutWithoutARevocationStoreRedirects(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)
	cookie := forgedCookie(t, auth, time.Now().Add(time.Hour), time.Now())
	if live, reason := replay(auth, cookie); !live {
		t.Fatalf("test premise: the session is not live: %q", reason)
	}
	w := signOut(auth, cookie, "")
	if w.Code != http.StatusFound {
		t.Errorf("sign-out status = %d, want %d", w.Code, http.StatusFound)
	}
}
