// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestValidateBasePath pins WARDYN_BASE_PATH's boot refusal: the value's shape
// (it is written into index.html and every cookie Path), an OIDC callback the
// IdP would send outside the base, and a plain-http control-plane URL that
// runs would dial at the host root. Unset refuses nothing.
func TestValidateBasePath(t *testing.T) {
	const cp = "https://wardynd:8443"
	for _, ok := range []string{"", "/wardyn", "/team/wardyn", "/w.v2_~-x"} {
		if err := validateBasePath(ok, "", "", cp); err != nil {
			t.Errorf("validateBasePath(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"wardyn", "/", "/wardyn/", "//wardyn", "/a//b", "/wardyn/..", "/../etc", "/./x",
		"/wardyn?x=1", "/wardyn#top", "/war dyn", `/w"x`, "/w<x", "/w%2F",
	} {
		err := validateBasePath(bad, "", "", cp)
		if err == nil || !strings.Contains(err.Error(), "WARDYN_BASE_PATH") {
			t.Errorf("validateBasePath(%q) = %v, want a refusal naming WARDYN_BASE_PATH", bad, err)
		}
	}
}

func TestValidateBasePathOIDCRedirectUnderTheBase(t *testing.T) {
	const cp, iss = "https://wardynd:8443", "https://idp.example.com"
	for _, ok := range []string{"https://host.example.com/wardyn/auth/callback", "http://localhost:8080/wardyn/auth/callback"} {
		if err := validateBasePath("/wardyn", iss, ok, cp); err != nil {
			t.Errorf("redirect %q = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"https://host.example.com/auth/callback", "https://host.example.com/wardynx/auth/callback", "https://host.example.com/wardyn", ""} {
		err := validateBasePath("/wardyn", iss, bad, cp)
		if err == nil || !strings.Contains(err.Error(), "WARDYN_OIDC_REDIRECT_URL") || !strings.Contains(err.Error(), "/wardyn/auth/callback") {
			t.Errorf("redirect %q = %v, want a refusal naming the variable and the right callback", bad, err)
		}
	}
	// Without OIDC the redirect URL is not in play.
	if err := validateBasePath("/wardyn", "", "https://host.example.com/auth/callback", cp); err != nil {
		t.Errorf("no issuer: %v, want nil", err)
	}
	// Without a base path the redirect keeps its root placement.
	if err := validateBasePath("", iss, "https://host.example.com/auth/callback", cp); err != nil {
		t.Errorf("no base: %v, want nil", err)
	}
}

func TestValidateBasePathPlainHTTPControlPlane(t *testing.T) {
	if err := validateBasePath("/wardyn", "", "", "http://127.0.0.1:8080/wardyn"); err != nil {
		t.Errorf("http control plane under the base = %v, want nil", err)
	}
	err := validateBasePath("/wardyn", "", "", "http://127.0.0.1:8080")
	if err == nil || !strings.Contains(err.Error(), "WARDYN_CONTROL_PLANE_URL") || !strings.Contains(err.Error(), "http://127.0.0.1:8080/wardyn") {
		t.Errorf("http control plane at the root = %v, want a refusal naming the fixed URL", err)
	}
}

// TestADOEntraRedirectURLUnderTheBase: the Azure DevOps callback is derived,
// not operator-set, so it has to carry the base itself.
func TestADOEntraRedirectURLUnderTheBase(t *testing.T) {
	const oidcCB = "https://host.example.com/wardyn/auth/callback"
	if got, want := adoEntraRedirectURL(oidcCB, "/wardyn"), "https://host.example.com/wardyn/api/v1/scm/azure-devops/callback"; got != want {
		t.Errorf("adoEntraRedirectURL = %q, want %q", got, want)
	}
	if got, want := adoEntraRedirectURL("https://host.example.com/auth/callback", ""), "https://host.example.com/api/v1/scm/azure-devops/callback"; got != want {
		t.Errorf("unset base: adoEntraRedirectURL = %q, want %q", got, want)
	}
}
