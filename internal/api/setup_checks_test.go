// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
)

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

// TestK8sEgressContainmentCheck: absent on every non-k8s driver (docker's L0
// structural containment has no analogous row); on a k8s driver, FAIL for
// both "unenforced" (the opt-out is a standing risk acceptance, never a
// success) and "" (indeterminate — an old daemon build, or in principle any
// unproven state; see the field's doc for why a genuinely indeterminate LIVE
// canary can never reach here). Never confuses Indeterminate with Enforcing.
func TestK8sEgressContainmentCheck(t *testing.T) {
	cases := []struct {
		name             string
		driver           string
		netpolProven     string
		wantOK           bool
		wantStatus       string
		wantFixHasOptOut bool // the fix string must name the opt-out env var (unenforced only)
	}{
		{"docker driver: absent regardless of the field", "docker", "enforced", false, "", false},
		{"no driver: absent", "none", "", false, "", false},
		{"k8s enforced: ok, no fix needed", "k8s", "enforced", true, "ok", false},
		{"k8s unenforced (opted out): fail, fix names the opt-out to unset", "k8s", "unenforced", true, "fail", true},
		{"k8s indeterminate (field absent): fail, never reads as enforcing", "k8s", "", true, "fail", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk, ok := k8sEgressContainmentCheck(tc.driver, tc.netpolProven)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if chk.ID != "k8s_egress_containment" {
				t.Errorf("ID = %q, want %q", chk.ID, "k8s_egress_containment")
			}
			if chk.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", chk.Status, tc.wantStatus)
			}
			if chk.Status == "fail" && chk.Fix == "" {
				t.Error("fail status must carry a Fix")
			}
			if strings.Contains(chk.Detail, "Enforcing") && chk.Status != "ok" {
				t.Errorf("a non-ok row must never claim Enforcing in its Detail: %q", chk.Detail)
			}
			hasOptOut := strings.Contains(chk.Fix, "WARDYN_K8S_ALLOW_UNENFORCED_NETPOL")
			if hasOptOut != tc.wantFixHasOptOut {
				t.Errorf("fix mentions the opt-out env var = %v, want %v (fix: %q)", hasOptOut, tc.wantFixHasOptOut, chk.Fix)
			}
			// Every dual-form env-var mention carries the helm form too.
			if strings.Contains(chk.Fix, "WARDYN_") && !strings.Contains(chk.Fix, "helm: env.") {
				t.Errorf("fix mentions a WARDYN_ env var with no dual helm form: %q", chk.Fix)
			}
		})
	}
}
