// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"net/http"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

const expiresHeader = "X-Wardyn-Session-Expires"

// A same-origin read carrying the console's activity header is use, exactly as a write is: it renews
// a session past half its span and resets the idle clock. Only a request the browser labels
// same-origin counts, so a sibling host or a cross-site page sending the header changes nothing.
func TestSessionActivityHeader(t *testing.T) {
	_, auth := lifeAuth(t, func(c *writoidc.Config) {
		c.SessionRenewCap = 8 * time.Hour
		c.SessionIdleTimeout = 30 * time.Minute
	})
	recent := time.Now().Add(-5 * time.Minute).Unix()
	w, live, _ := activeRequest(auth, lifeCookie(t, auth, 50*time.Minute, 10*time.Minute, recent), http.MethodGet, "same-origin")
	sess, ok := reminted(t, auth, w)
	if !live || !ok {
		t.Fatalf("an active read: live=%v reminted=%v", live, ok)
	}
	if d := time.Until(sess.Expiry); d < 59*time.Minute || d > time.Hour {
		t.Errorf("renewed expiry in %s, want about 1h", d)
	}
	if time.Now().Unix()-sess.LastActive > 2 {
		t.Errorf("the idle clock was not reset: last=%d", sess.LastActive)
	}
	for _, site := range []string{"cross-site", "same-site", ""} {
		w, live, _ := activeRequest(auth, lifeCookie(t, auth, 50*time.Minute, 10*time.Minute, recent), http.MethodGet, site)
		if _, ok := reminted(t, auth, w); ok || !live {
			t.Errorf("Sec-Fetch-Site %q with the header: reminted=%v live=%v", site, ok, live)
		}
	}
}

// Re-minting stays throttled for active reads as for writes: the idle clock moves at most once a
// tenth of the timeout, and the expiry only once the session is past half its span.
func TestSessionActivityThrottled(t *testing.T) {
	_, idle := lifeAuth(t, func(c *writoidc.Config) { c.SessionIdleTimeout = 30 * time.Minute })
	justNow := time.Now().Add(-time.Minute).Unix()
	if w, _, _ := activeRequest(idle, lifeCookie(t, idle, 10*time.Minute, time.Hour, justNow), http.MethodGet, "same-origin"); len(w.Result().Cookies()) != 0 {
		t.Errorf("an active read within a tenth of the timeout re-minted: %v", w.Result().Header.Values("Set-Cookie"))
	}
	_, renew := lifeAuth(t, func(c *writoidc.Config) { c.SessionRenewCap = 8 * time.Hour })
	if w, _, _ := activeRequest(renew, lifeCookie(t, renew, 20*time.Minute, 40*time.Minute, 0), http.MethodGet, "same-origin"); len(w.Result().Cookies()) != 0 {
		t.Errorf("an active read under half the span re-minted: %v", w.Result().Header.Values("Set-Cookie"))
	}
}

// While renewal is on, every answer to a live session names its expiry (the renewed one when the
// request renewed it), so the console can follow a renewal its HttpOnly cookie hides; a refused
// session names none.
func TestSessionExpiresHeader(t *testing.T) {
	_, auth := lifeAuth(t, func(c *writoidc.Config) { c.SessionRenewCap = 8 * time.Hour })
	young := lifeCookie(t, auth, 10*time.Minute, 50*time.Minute, 0)
	held, err := writoidc.DecodeSessionForTest(auth, young)
	if err != nil {
		t.Fatal(err)
	}
	if w, _, _ := lifeRequest(auth, young, http.MethodGet, ""); w.Header().Get(expiresHeader) != held.Expiry.UTC().Format(time.RFC3339) {
		t.Errorf("a plain read named %q, want the cookie's %s", w.Header().Get(expiresHeader), held.Expiry.UTC().Format(time.RFC3339))
	}
	w, _, _ := activeRequest(auth, lifeCookie(t, auth, 50*time.Minute, 10*time.Minute, 0), http.MethodGet, "same-origin")
	sess, ok := reminted(t, auth, w)
	if !ok || w.Header().Get(expiresHeader) != sess.Expiry.UTC().Format(time.RFC3339) {
		t.Errorf("a renewing read named %q, want the renewed %s (reminted=%v)", w.Header().Get(expiresHeader), sess.Expiry.UTC().Format(time.RFC3339), ok)
	}
	if w, _, _ := lifeRequest(auth, lifeCookie(t, auth, 2*time.Hour, -time.Minute, 0), http.MethodGet, ""); w.Header().Get(expiresHeader) != "" {
		t.Errorf("an expired session named %q", w.Header().Get(expiresHeader))
	}
}

// A renewed cookie is the session it renewed: signing out with the original ends the renewed copy.
// And a session Middleware refuses is never re-minted, so no amount of activity turns an ended
// session (cut, expired, idle or role-stale) back into a live cookie.
func TestSessionRenewalNeverOutlivesAnEnd(t *testing.T) {
	t.Run("sign-out with the original ends the renewed copy", func(t *testing.T) {
		ledger := &cutLedger{seq: 3} // lifeCookie records sequence 3; the sign-out's cut draws 4
		_, auth := lifeAuth(t, func(c *writoidc.Config) { c.SessionRenewCap = 8 * time.Hour; c.Revocations = ledger })
		orig := lifeCookie(t, auth, 50*time.Minute, 10*time.Minute, 0)
		w, _, _ := lifeRequest(auth, orig, http.MethodPost, "same-origin")
		if _, ok := reminted(t, auth, w); !ok {
			t.Fatal("not renewed")
		}
		renewed := cookieMap(w.Result().Cookies())["wardyn_session"]
		if so := signOut(auth, orig, ""); so.Code != http.StatusFound || ledger.cutCalls != 1 {
			t.Fatalf("sign-out: %d, cuts %d", so.Code, ledger.cutCalls)
		}
		w, live, reason := activeRequest(auth, renewed, http.MethodPost, "same-origin")
		if _, ok := reminted(t, auth, w); live || ok || reason != "revoked_session" {
			t.Errorf("renewed copy after sign-out: live=%v reminted=%v reason=%q", live, ok, reason)
		}
	})
	for _, tc := range []struct {
		name              string
		set               func(*writoidc.Config)
		issued, expiresIn time.Duration
		reason            string
	}{
		{"cut", func(c *writoidc.Config) { c.Revocations = &cutLedger{seq: 4, cuts: map[string]int64{"sub-life": 4}} }, 50 * time.Minute, 10 * time.Minute, "revoked_session"},
		{"expired", nil, 2 * time.Hour, -time.Minute, "expired_session"},
		{"idle", func(c *writoidc.Config) { c.SessionIdleTimeout = 30 * time.Minute }, 50 * time.Minute, 10 * time.Minute, "expired_session"},
		{"role stamp stale", func(c *writoidc.Config) { c.RoleStampTTL = 40 * time.Minute }, 50 * time.Minute, 10 * time.Minute, "role_stamp_stale"},
	} {
		_, auth := lifeAuth(t, func(c *writoidc.Config) {
			c.SessionRenewCap = 8 * time.Hour
			if tc.set != nil {
				tc.set(c)
			}
		})
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			w, live, reason := activeRequest(auth, lifeCookie(t, auth, tc.issued, tc.expiresIn, 0), method, "same-origin")
			if _, ok := reminted(t, auth, w); live || ok || reason != tc.reason || w.Header().Get(expiresHeader) != "" {
				t.Errorf("%s %s: live=%v reminted=%v reason=%q expiry header %q", tc.name, method, live, ok, reason, w.Header().Get(expiresHeader))
			}
		}
	}
}
