// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// M5: the golden-id test (setup_check_ids_test.go) pins that these two rows
// EXIST under one fixture, but an id alone doesn't pin its Status — a check
// that always returned "warn" (or always "ok") would pass that test
// unnoticed. These table tests assert Status (and presence) directly against
// the pure check functions, no HTTP/Server plumbing required.

func TestSsoRBACCheck(t *testing.T) {
	cases := []struct {
		name              string
		oidcConfigured    bool
		roleMapConfigured bool
		wantOK            bool
		wantStatus        string
	}{
		{"OIDC off: absent regardless of role map", false, true, false, ""},
		{"OIDC off, map unset: still absent", false, false, false, ""},
		{"OIDC on, map set: ok", true, true, true, "ok"},
		{"OIDC on, map unset: warn", true, false, true, "warn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk, ok := ssoRBACCheck(tc.oidcConfigured, tc.roleMapConfigured)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if chk.ID != "sso_rbac" {
				t.Errorf("ID = %q, want %q", chk.ID, "sso_rbac")
			}
			if chk.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", chk.Status, tc.wantStatus)
			}
			if tc.wantStatus == "warn" && chk.Fix == "" {
				t.Error("warn status must carry a Fix")
			}
		})
	}
}

func TestTlsCookiePostureCheck(t *testing.T) {
	cases := []struct {
		name           string
		oidcConfigured bool
		redirectURL    string
		secureCookies  bool
		wantOK         bool
		wantStatus     string
	}{
		{"OIDC off: absent", false, "https://wardyn.example.com/auth/callback", false, false, ""},
		{"http redirect: absent (nothing to warn about)", true, "http://localhost/auth/callback", false, false, ""},
		{"http redirect even with secureCookies true: absent", true, "http://localhost/auth/callback", true, false, ""},
		{"https + secureCookies true: ok", true, "https://wardyn.example.com/auth/callback", true, true, "ok"},
		{"https + secureCookies false: warn (the ingress footgun)", true, "https://wardyn.example.com/auth/callback", false, true, "warn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk, ok := tlsCookiePostureCheck(tc.oidcConfigured, tc.redirectURL, tc.secureCookies)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if chk.ID != "tls_cookie_posture" {
				t.Errorf("ID = %q, want %q", chk.ID, "tls_cookie_posture")
			}
			if chk.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", chk.Status, tc.wantStatus)
			}
			if tc.wantStatus == "warn" && chk.Fix == "" {
				t.Error("warn status must carry a Fix")
			}
		})
	}
}
