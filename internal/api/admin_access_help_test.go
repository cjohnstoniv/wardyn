// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// #484 — the everyone-is-an-admin warning and the admin-written sign-in help.
// Frozen strings: docs/design/admin-access-canon.md.

// TestSetupStatus_SSORBACAdminListIsOK drives the real /setup/status wiring,
// not only the pure check: an operator allowlist alone reads ok (Q457-5).
func TestSetupStatus_SSORBACAdminListIsOK(t *testing.T) {
	for _, tc := range []struct {
		name   string
		emails []string
		want   string
	}{
		{"no role map, no admin list: warn", nil, "warn"},
		{"admin list only: ok", []string{"ops@corp.example"}, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv := New(Config{AdminToken: adminToken, OIDC: newAccessAuth(t, nil, "", tc.emails, nil)})
			code, st := decodeSetup(t, srv, adminToken)
			if code != http.StatusOK {
				t.Fatalf("GET /setup/status = %d", code)
			}
			for _, c := range st.Checks {
				if c.ID == "sso_rbac" {
					if c.Status != tc.want {
						t.Errorf("sso_rbac = %q, want %q (%+v)", c.Status, tc.want, c)
					}
					return
				}
			}
			t.Fatal("sso_rbac row absent with OIDC configured")
		})
	}
}

// A member never reads the warn row: the redaction drops every check,
// whatever it says — unchanged by #484.
func TestRedactSetupStatusForMember_DropsSSORBACWarn(t *testing.T) {
	warn, _ := ssoRBACCheck(true, false, false, false)
	got := redactSetupStatusForUser(SetupStatus{Checks: []SetupCheck{warn}}, false, false)
	if len(got.Checks) != 0 || !got.ChecksRedacted {
		t.Errorf("member checks = %+v (redacted=%v), want none", got.Checks, got.ChecksRedacted)
	}
}

func TestValidateSignInHelp(t *testing.T) {
	for _, tc := range []struct {
		name, text, url string
		want            error
	}{
		{"empty: ok", "", "", nil},
		{"1000 characters: ok", strings.Repeat("a", 1000), "", nil},
		// Counted in runes: 1000 two-byte characters is 2000 bytes and still ok.
		{"1000 multi-byte characters: ok", strings.Repeat("é", 1000), "", nil},
		{"1001 characters: refused", strings.Repeat("a", 1001), "", errSignInHelpTextTooLong},
		{"quotes and apostrophes: ok", `Ask in #it-helpdesk — it's "Wardyn access" you want.`, "", nil},
		{"markup is just text: ok", `<a href="x">click</a>`, "", nil},
		{"line feed: refused", "line one\nline two", "", errSignInHelpTextControl},
		{"carriage return: refused", "a\rb", "", errSignInHelpTextControl},
		{"tab: refused", "a\tb", "", errSignInHelpTextControl},
		{"DEL: refused", "a\x7fb", "", errSignInHelpTextControl},
		{"C1 control (U+0085 NEL): refused", "a\u0085b", "", errSignInHelpTextControl},
		{"bidi override (U+202E): refused", "abc\u202edef", "", errSignInHelpTextControl},
		{"zero-width space (U+200B): refused", "a\u200bb", "", errSignInHelpTextControl},
		{"line separator (U+2028): refused", "a\u2028b", "", errSignInHelpTextControl},
		{"paragraph separator (U+2029): refused", "a\u2029b", "", errSignInHelpTextControl},
		{"https url: ok", "", "https://it.corp.example/request?app=wardyn", nil},
		{"http url: ok", "", "http://helpdesk.corp.example/", nil},
		// The defect #489's review found: an ordinary helpdesk query string.
		{"& in the query: ok", "", "https://corp.service-now.com/sp?id=sc_cat_item&sys_id=abc", nil},
		{"upper-case scheme and host, port, fragment: ok", "", "HTTPS://IT.Corp.Example:8443/a?b=c&d=e#f", nil},
		{"javascript: refused", "", "javascript:alert(1)", errSignInHelpURLScheme},
		{"data: refused", "", "data:text/html,hi", errSignInHelpURLScheme},
		{"ftp: refused", "", "ftp://files.corp.example/", errSignInHelpURLScheme},
		{"no scheme: refused", "", "it.corp.example/request", errSignInHelpURLScheme},
		{"scheme-relative //host: refused", "", "//it.corp.example/request", errSignInHelpURLScheme},
		{"no host: refused", "", "https://", errSignInHelpURLMalformed},
		{"opaque https:host: refused", "", "https:it.corp.example", errSignInHelpURLMalformed},
		{"dotless host: refused", "", "https://intranet/request", errSignInHelpURLMalformed},
		{"userinfo: refused", "", "https://user:pass@it.corp.example/", errSignInHelpURLMalformed},
		{"userinfo host confusion: refused", "", "https://it.corp.example@evil.example/", errSignInHelpURLMalformed},
		{"leading space: refused", "", " https://it.corp.example/", errSignInHelpURLMalformed},
		{"inner space: refused", "", "https://it.corp.example/a b", errSignInHelpURLMalformed},
		{"leading control before javascript: refused", "", "\x01javascript:alert(1)", errSignInHelpURLMalformed},
		{"tab inside: refused", "", "https://it.corp.example/\tx", errSignInHelpURLMalformed},
		{"zero-width space in host: refused", "", "https://it.corp\u200b.example/", errSignInHelpURLMalformed},
		{"bidi override: refused", "", "https://it.corp.example/\u202eevil", errSignInHelpURLMalformed},
		{"over 2048 bytes: refused", "", "https://it.corp.example/" + strings.Repeat("a", 2048), errSignInHelpURLMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateSignInHelp(tc.text, tc.url); got != tc.want {
				t.Errorf("validateSignInHelp = %v, want %v", got, tc.want)
			}
		})
	}
}

// The refusal reaches the caller verbatim through PUT /site-config, and the
// write never lands.
func TestHandlePutSiteConfig_SignInHelpRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"1001 characters", `{"sign_in_help_text":"` + strings.Repeat("a", 1001) + `"}`,
			"sign_in_help_text: longer than 1,000 characters — it renders under a refusal on the sign-in page"},
		{"line break", `{"sign_in_help_text":"one\ntwo"}`,
			"sign_in_help_text: contains a line break, control character or invisible formatting character — it renders as one plain paragraph on the sign-in page"},
		{"bad scheme", `{"sign_in_help_url":"javascript:alert(1)"}`,
			"sign_in_help_url: must be an http:// or https:// address — it is shown to people who have not signed in"},
		{"malformed", `{"sign_in_help_url":"https://user@it.corp.example/"}`,
			"sign_in_help_url: must be a plain web address with a real host name — no spaces, sign-in details or hidden characters — it is shown to people who have not signed in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSiteConfigStore{}
			srv, _ := newSiteConfigHarness(t, fake)
			w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, tc.body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("PUT = %d %s, want 400 carrying %q", w.Code, w.Body.String(), tc.want)
			}
			if fake.putSeen != nil {
				t.Error("a refused write reached the store")
			}
		})
	}
}

// An older body that does not name the pair carries it forward; a body that
// names it as "" clears it.
func TestHandlePutSiteConfig_SignInHelpCarryForward(t *testing.T) {
	stored := types.SiteConfig{SignInHelpText: "Ask in #it-helpdesk.", SignInHelpURL: "https://it.corp.example/"}

	fake := &fakeSiteConfigStore{cfg: stored}
	srv, _ := newSiteConfigHarness(t, fake)
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"scm_hosts":["github.example.com"]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
	}
	if fake.putSeen.SignInHelpText != stored.SignInHelpText || fake.putSeen.SignInHelpURL != stored.SignInHelpURL {
		t.Errorf("silence erased the help pair: %+v", fake.putSeen)
	}

	fake = &fakeSiteConfigStore{cfg: stored}
	srv, _ = newSiteConfigHarness(t, fake)
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"sign_in_help_text":"","sign_in_help_url":""}`); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
	}
	if fake.putSeen.SignInHelpText != "" || fake.putSeen.SignInHelpURL != "" {
		t.Errorf("naming the pair as empty did not clear it: %+v", fake.putSeen)
	}
}

// site_config.write carries the pair in the clear — public by design, so the
// log holds nothing /healthz does not already publish — and an '&' link saves.
func TestHandlePutSiteConfig_SignInHelpAudited(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)
	const link = "https://corp.service-now.com/sp?id=sc_cat_item&sys_id=abc"
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
		`{"sign_in_help_text":"Ask in #it-helpdesk.","sign_in_help_url":"`+link+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	for _, ev := range audit.events {
		if ev.Action != "site_config.write" {
			continue
		}
		var d struct {
			Text string `json:"sign_in_help_text"`
			URL  string `json:"sign_in_help_url"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Text != "Ask in #it-helpdesk." || d.URL != link {
			t.Errorf("site_config.write datum = %+v, want the saved pair", d)
		}
		return
	}
	t.Fatal("no site_config.write event")
}

// healthzHelpStore answers the two reads /healthz makes.
type healthzHelpStore struct{ fakeSiteConfigStore }

func (*healthzHelpStore) LatestAuditEventByAction(context.Context, string) (types.AuditEvent, error) {
	return types.AuditEvent{}, store.ErrNotFound
}

func TestHealthz_SignInHelp(t *testing.T) {
	for _, tc := range []struct {
		name              string
		stored            types.SiteConfig
		wantText, wantURL any
	}{
		{"both set: both published",
			types.SiteConfig{SignInHelpText: `Ask in #it-helpdesk — it's quick.`, SignInHelpURL: "https://it.corp.example/request"},
			`Ask in #it-helpdesk — it's quick.`, "https://it.corp.example/request"},
		{"unset: both omitted", types.SiteConfig{}, nil, nil},
		// A stored value that no longer passes is dropped on read, each on its own.
		{"invalid stored text dropped, url kept",
			types.SiteConfig{SignInHelpText: "line\nbreak", SignInHelpURL: "https://it.corp.example/"},
			nil, "https://it.corp.example/"},
		{"invalid stored url dropped, text kept",
			types.SiteConfig{SignInHelpText: "Ask IT.", SignInHelpURL: "javascript:alert(1)"},
			"Ask IT.", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &healthzHelpStore{fakeSiteConfigStore{cfg: tc.stored}}
			srv := New(Config{Store: st})
			w := do(t, srv, http.MethodGet, "/healthz", "", "")
			if w.Code != http.StatusOK {
				t.Fatalf("healthz = %d", w.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if got := body["sign_in_help_text"]; got != tc.wantText {
				t.Errorf("sign_in_help_text = %#v, want %#v", got, tc.wantText)
			}
			if got := body["sign_in_help_url"]; got != tc.wantURL {
				t.Errorf("sign_in_help_url = %#v, want %#v", got, tc.wantURL)
			}
		})
	}

	// An unreadable store is not an outage of liveness: /healthz still answers.
	srv := New(Config{Store: &healthzHelpStore{fakeSiteConfigStore{getErr: store.ErrNotFound}}})
	if w := do(t, srv, http.MethodGet, "/healthz", "", ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "sign_in_help") {
		t.Errorf("healthz with an unreadable site config = %d %s", w.Code, w.Body.String())
	}
}
