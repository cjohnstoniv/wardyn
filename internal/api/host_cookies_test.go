// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Under secure cookies the console's cookies carry the __Host- prefix (#1258).
// These pin the console listener's own share of that: the Azure DevOps
// sign-in's one-time cookies, and the UI gateway's strip rules, which must
// treat __Host-wardyn_* as the same reserved namespace as wardyn_*.

// TestHostCookies_ADOSignIn: under secure cookies, and under a base path, the
// Azure DevOps sign-in writes __Host- cookies at Path=/ (clears included) and
// completes; the same state, nonce and verifier planted under the plain names
// by a sibling host are refused and store nothing.
func TestHostCookies_ADOSignIn(t *testing.T) {
	assertHost := func(t *testing.T, step string, cookies []*http.Cookie) {
		t.Helper()
		if len(cookies) == 0 {
			t.Fatalf("%s: set no cookies", step)
		}
		for _, c := range cookies {
			if !strings.HasPrefix(c.Name, "__Host-wardyn_ado_") || !c.Secure || c.Path != "/" || c.Domain != "" {
				t.Errorf("%s: cookie %q Secure=%v Path=%q Domain=%q, want a __Host- name, Secure, Path=/, no Domain",
					step, c.Name, c.Secure, c.Path, c.Domain)
			}
		}
	}
	newSecure := func(t *testing.T) *adoFixture {
		f := newADOFixture(t)
		f.srv.cfg.OIDCSecureCookies = true
		f.srv.cfg.BasePath = "/wardyn"
		return f
	}

	t.Run("round trip", func(t *testing.T) {
		f := newSecure(t)
		subject := f.fake.Subject()
		authURL, cookies := f.signIn(t, subject, "")
		assertHost(t, "sign-in", cookies)
		w := f.callback(t, subject, follow(t, authURL), cookies)
		if w.Code != http.StatusFound {
			t.Fatalf("callback: %d %q", w.Code, w.Body.String())
		}
		assertHost(t, "callback", w.Result().Cookies())
		if _, found := f.stored(t, subject); !found {
			t.Fatal("the sign-in stored nothing")
		}
	})

	t.Run("planted plain names", func(t *testing.T) {
		f := newSecure(t)
		subject := f.fake.Subject()
		authURL, cookies := f.signIn(t, subject, "")
		planted := make([]*http.Cookie, 0, len(cookies))
		for _, c := range cookies {
			planted = append(planted, &http.Cookie{Name: strings.TrimPrefix(c.Name, "__Host-"), Value: c.Value})
		}
		w := f.callback(t, subject, follow(t, authURL), planted)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid state parameter") {
			t.Fatalf("callback on planted cookies: %d %q, want 400 invalid state parameter", w.Code, w.Body.String())
		}
		if _, found := f.stored(t, subject); found {
			t.Fatal("a sign-in completed on planted cookies")
		}
	})
}

// TestHostCookies_GatewayStripsPrefixedConsoleCookies: on a hostname the
// console shares with the gateway, the browser sends the console's
// __Host-wardyn_session to the relay too. It never reaches the app, whatever
// the policy — including "allow:__Host-*", the policy the docs recommend — and
// the app can never set one.
func TestHostCookies_GatewayStripsPrefixedConsoleCookies(t *testing.T) {
	const sent = "__Host-wardyn_session=console; __Secure-wardyn_oidc_state=s; __host-WARDYN_x=y; __Host-app=1"
	for _, mode := range uiGatewayModes {
		for _, policy := range []string{"", "allow:__Host-*", "deny:other"} {
			t.Run(mode.name+"/inbound/"+cmp.Or(policy, "default"), func(t *testing.T) {
				seen := make(chan string, 1)
				h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen <- r.Header.Get("Cookie")
					_, _ = io.WriteString(w, "ok")
				}))
				p, err := ParseUICookiePolicy(policy)
				if err != nil {
					t.Fatal(err)
				}
				h.srv.cfg.UICookiePolicy = p
				h.relayIn(mode, sent)
				if got := <-seen; got != "__Host-app=1" {
					t.Fatalf("upstream Cookie = %q, want only the app's own __Host-app", got)
				}
			})
		}
		t.Run(mode.name+"/outbound", func(t *testing.T) {
			h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, sc := range []string{
					"__Host-wardyn_session=forged; Path=/; Secure",
					"__Secure-wardyn_session=forged; Secure",
					"__HOST-wardyn_oidc_state=forged; Path=/; Secure",
					"__Host-app=1; Path=/; Secure",
				} {
					w.Header().Add("Set-Cookie", sc)
				}
				_, _ = io.WriteString(w, "ok")
			}))
			if got := h.relayIn(mode, "").Header().Values("Set-Cookie"); !slices.Equal(got, []string{"__Host-app=1; Path=/; Secure"}) {
				t.Fatalf("Set-Cookie reaching the browser = %q, want only the app's own __Host-app", got)
			}
		})
	}
}
