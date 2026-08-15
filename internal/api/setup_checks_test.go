// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
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
// TestSiteConfigCheck_DanglingSecretRef pins W26-S1-2: on base 763beb5,
// siteConfigCheck graded "info" ("every run inherits it") purely off whether
// UpstreamProxySecretRef/EgressRedirects/ScmHosts were SET — never whether the
// secret they name is actually present. After the documented reset+apply
// recovery (`wardyn site-config get > f` before a reset, `wardyn site-config
// apply f` after) with the referenced secret never restored, that read as
// fully configured while the credentialed path was dead. It must now grade
// "warn" and name the missing secret.
func TestSiteConfigCheck_DanglingSecretRef(t *testing.T) {
	cases := []struct {
		name       string
		sc         types.SiteConfig
		present    map[string]bool
		wantStatus string
		wantNames  []string // must all appear in Detail when wantStatus == "warn"
	}{
		{"unconfigured", types.SiteConfig{}, nil, "info", nil},
		{
			"upstream proxy secret present: ok",
			types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"},
			map[string]bool{"corp-proxy-url": true},
			"info", nil,
		},
		{
			"upstream proxy secret DANGLING: warn",
			types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"},
			map[string]bool{},
			"warn", []string{"corp-proxy-url"},
		},
		{
			"egress redirect token secret DANGLING: warn",
			types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "ghcr.io", To: "registry.corp.internal/ghcr-remote", TokenSecretRef: "ghcr-token"},
			}},
			map[string]bool{},
			"warn", []string{"ghcr-token"},
		},
		{
			"plain upstream_proxy_url (no secret ref) never dangles",
			types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:3128"},
			map[string]bool{},
			"info", nil,
		},
		{
			"scm hosts only, no secret refs at all: ok",
			types.SiteConfig{ScmHosts: []string{"dev.azure.com"}},
			map[string]bool{},
			"info", nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := siteConfigCheck(tc.sc, tc.present)
			if chk.Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q (detail=%q)", chk.Status, tc.wantStatus, chk.Detail)
			}
			for _, name := range tc.wantNames {
				if !strings.Contains(chk.Detail, name) {
					t.Errorf("Detail = %q, want it to name dangling secret %q", chk.Detail, name)
				}
			}
			if tc.wantStatus == "warn" && chk.Fix == "" {
				t.Error("warn status must carry a Fix")
			}
		})
	}
}

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

// TestRunnerCheckCC1OnlyFixIsDriverAware (W4-S1-5/W27-S1-4): a CC1-only host's
// Fix used to unconditionally read "run `wardyn setup wall` (or `wardyn setup
// vault`)" — a DOCKER host command that means nothing on a k8s runner, where
// the actual lever is pinning a cluster-registered RuntimeClass via Helm
// (k8s.runtimeClasses.CC2/.CC3). The docker driver keeps the original command;
// only k8s swaps to the Helm-shaped fix.
func TestRunnerCheckCC1OnlyFixIsDriverAware(t *testing.T) {
	cases := []struct {
		name         string
		driver       string
		wantWardyn   bool // fix names the `wardyn setup wall/vault` docker command
		wantHelm     bool // fix names k8s.runtimeClasses via helm upgrade --set
	}{
		{"docker driver: the host-side `wardyn setup` command", "docker", true, false},
		{"k8s driver: the Helm RuntimeClass pin, never the docker command", "k8s", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := runnerCheck(SetupRunner{Driver: tc.driver, ConfinementClasses: []string{"CC1"}})
			if chk.Status != "info" {
				t.Fatalf("Status = %q, want %q", chk.Status, "info")
			}
			hasWardyn := strings.Contains(chk.Fix, "wardyn setup wall")
			hasHelm := strings.Contains(chk.Fix, "k8s.runtimeClasses")
			if hasWardyn != tc.wantWardyn {
				t.Errorf("fix names `wardyn setup wall` = %v, want %v (fix: %q)", hasWardyn, tc.wantWardyn, chk.Fix)
			}
			if hasHelm != tc.wantHelm {
				t.Errorf("fix names k8s.runtimeClasses = %v, want %v (fix: %q)", hasHelm, tc.wantHelm, chk.Fix)
			}
		})
	}
}
