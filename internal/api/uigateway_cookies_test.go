// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// uiGatewayMode is one of the gateway's two deployments: path mode (one shared
// origin, here under a WARDYN_BASE_PATH) and host mode (an origin per run).
// The cookie rules are the same code in both, and are pinned in both.
type uiGatewayMode struct {
	name, basePath, originTemplate string
}

var uiGatewayModes = []uiGatewayMode{
	{name: "path mode", basePath: "/wardyn"},
	{name: "host mode", originTemplate: "https://run-{run}.ui.example.com"},
}

// relayIn configures h for mode, walks the ticket handoff on the mode's host
// and base, then relays one request carrying extraCookies (raw, after the
// relay's own session cookie) and returns the response.
func (h *uiHarness) relayIn(mode uiGatewayMode, extraCookies string) *httptest.ResponseRecorder {
	h.t.Helper()
	h.srv.cfg.BasePath, h.srv.cfg.UIOriginTemplate = mode.basePath, mode.originTemplate
	gw := h.srv.UIGatewayHandler()
	host := "ui.example.com"
	if mode.originTemplate != "" {
		host = h.srv.uiRunOrigin(h.run.ID)
	}
	base := h.srv.uiBasePath()

	q := url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)}}
	req := h.bound(httptest.NewRequest(http.MethodGet, base+uiEnterPath+"?"+q.Encode(), nil), q)
	req.Host = host
	enter := httptest.NewRecorder()
	gw.ServeHTTP(enter, req)
	if enter.Code != http.StatusFound {
		h.t.Fatalf("enter: %d %s", enter.Code, enter.Body.String())
	}
	var sess *http.Cookie
	for _, c := range enter.Result().Cookies() {
		if c.Name == uiCookieName {
			sess = c
		}
	}
	if sess == nil {
		h.t.Fatal("no relay cookie set")
	}

	req = httptest.NewRequest(http.MethodGet, base+uiRelayPrefix(h.run.ID, "code")+"/ide", nil)
	req.Host = host
	req.AddCookie(sess)
	if extraCookies != "" {
		req.Header.Set("Cookie", req.Header.Get("Cookie")+"; "+extraCookies)
	}
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("relay: %d %s", rec.Code, rec.Body.String())
	}
	return rec
}

// TestUIGateway_InboundCookiePolicy: WARDYN_UI_SANDBOX_STRIP_COOKIES decides
// which non-Wardyn cookies reach the app. A relay host under a shared parent
// domain receives its siblings' Domain= cookies; with an allow list, only the
// app's own names get through, with a deny list the named ones come off. The
// default is unchanged: every cookie but wardyn_*. Whatever the policy, no
// wardyn_* cookie — the relay's own session included — is ever forwarded,
// which "allow:*" proves.
func TestUIGateway_InboundCookiePolicy(t *testing.T) {
	// "app_sess" alone is a nameless cookie whose value merely looks like an
	// allowed name: an allow list must not let it through.
	const sent = "wardyn_session=console; WARDYN_x=y; app_sess=1; sibling_sso=leak; csrf_tok=2; app_sess"
	for _, mode := range uiGatewayModes {
		for _, tc := range []struct{ policy, want string }{
			{"", "app_sess=1; sibling_sso=leak; csrf_tok=2; app_sess"},
			{"allow:*", "app_sess=1; sibling_sso=leak; csrf_tok=2; app_sess"},
			{"allow:app_sess, csrf_*", "app_sess=1; csrf_tok=2"},
			{"deny:sibling_*", "app_sess=1; csrf_tok=2; app_sess"},
			{"deny:*", ""},
		} {
			t.Run(mode.name+"/"+cmp.Or(tc.policy, "default"), func(t *testing.T) {
				seen := make(chan string, 1)
				h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen <- r.Header.Get("Cookie")
					_, _ = io.WriteString(w, "ok")
				}))
				policy, err := ParseUICookiePolicy(tc.policy)
				if err != nil {
					t.Fatal(err)
				}
				h.srv.cfg.UICookiePolicy = policy
				h.relayIn(mode, sent)
				if got := <-seen; got != tc.want {
					t.Fatalf("upstream Cookie = %q, want %q", got, tc.want)
				}
			})
		}
	}
}

// TestUIGateway_OutboundSetCookieRules: the app's Set-Cookie headers, several
// per response, each decided on its own. A Domain attribute in any case,
// spacing or value is dropped (an app confined to its own origin never needs
// one, and with one it could plant a cookie every sibling host receives); so
// is a pair with no name, which a browser stores as a nameless cookie and
// sends back verbatim — "=wardyn_ui_sess=forged" would return as the relay's
// own session cookie. Attributes are split the way a browser splits them: a
// "Domain=" inside a quoted value is value text and stays, one after a ';'
// inside quotes is an attribute and goes.
func TestUIGateway_OutboundSetCookieRules(t *testing.T) {
	upstream := []string{
		"app_theme=dark; Path=/",
		"wide=1; Domain=example.com; Path=/",
		"wide2=1; path=/; DOMAIN=.ui.example.com",
		"wide3=1;domain =example.com",
		"wide4=1; Domain=",
		`quoted="Domain=example.com"; Path=/`,
		`smuggled="x; Domain=example.com"`,
		"=wardyn_ui_sess=forged; Path=/",
		"novalue",
		"=x",
		"wardyn_ui_sess=attacker-chosen; Path=/",
		"Wardyn_Session=x",
		`legacy="a b"; Path=/; HttpOnly`,
	}
	want := []string{"app_theme=dark; Path=/", `quoted="Domain=example.com"; Path=/`, `legacy="a b"; Path=/; HttpOnly`}
	for _, mode := range uiGatewayModes {
		t.Run(mode.name, func(t *testing.T) {
			h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, sc := range upstream {
					w.Header().Add("Set-Cookie", sc)
				}
				_, _ = io.WriteString(w, "ok")
			}))
			if got := h.relayIn(mode, "").Header().Values("Set-Cookie"); !slices.Equal(got, want) {
				t.Fatalf("Set-Cookie reaching the browser = %q, want %q", got, want)
			}
		})
	}
}

// TestUIGateway_InterimResponseSetCookieRules: ReverseProxy copies a 1xx's
// headers straight to the browser without running ModifyResponse, so a 103
// Early Hints was a Set-Cookie channel none of the rules above saw. The same
// rules apply to it; the rest of the 1xx (its Link) is untouched.
func TestUIGateway_InterimResponseSetCookieRules(t *testing.T) {
	h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, sc := range []string{"wardyn_ui_sess=forged; Path=/", "wide=1; Domain=example.com", "=wardyn_ui_sess=x", "app_hint=1"} {
			w.Header().Add("Set-Cookie", sc)
		}
		w.Header().Set("Link", "</x.js>; rel=preload; as=script")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Set-Cookie")
		_, _ = io.WriteString(w, "ok")
	}))
	sess := h.openSession()
	gw := httptest.NewServer(h.gateway)
	defer gw.Close()

	var hints []textproto.MIMEHeader
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, hdr textproto.MIMEHeader) error {
		if code == http.StatusEarlyHints {
			hints = append(hints, hdr)
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace),
		http.MethodGet, gw.URL+uiRelayPrefix(h.run.ID, "code")+"/ide", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(sess)
	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(hints) != 1 {
		t.Fatalf("status %d, %d early hints; want 200 after one 103", resp.StatusCode, len(hints))
	}
	if got := hints[0]["Set-Cookie"]; !slices.Equal(got, []string{"app_hint=1"}) {
		t.Fatalf("103 Set-Cookie = %q, want only the app's own host cookie", got)
	}
	if hints[0].Get("Link") == "" {
		t.Fatal("103 lost its Link header")
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("final response after a 103: Referrer-Policy = %q, want no-referrer", got)
	}
}
