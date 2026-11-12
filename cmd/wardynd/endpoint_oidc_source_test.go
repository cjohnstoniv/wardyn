// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// The endpoint sign-in's callback lives on the console's own browser-facing origin, under its base path; the
// door is offered only when the console has an OIDC login to bind a sign-in to.
func TestEndpointOIDCEnvFor(t *testing.T) {
	const issuer = "https://login.corp.example/realm"
	for _, tc := range []struct {
		name, issuer, redirect, base string
		wantURL                      string
		wantOK                       bool
	}{
		{"console origin", issuer, "https://wardyn.corp.example/api/v1/auth/callback", "", "https://wardyn.corp.example" + api.EndpointOIDCCallbackPath, true},
		{"under a base path", issuer, "https://wardyn.corp.example/console/api/v1/auth/callback", "/console", "https://wardyn.corp.example/console" + api.EndpointOIDCCallbackPath, true},
		{"surrounding space", " " + issuer + " ", " https://wardyn.corp.example/x ", "", "https://wardyn.corp.example" + api.EndpointOIDCCallbackPath, true},
		{"no console login", "", "https://wardyn.corp.example/x", "", "", false},
		{"no redirect url", issuer, "", "", "", false},
		{"redirect with no origin", issuer, "/api/v1/auth/callback", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ok := endpointOIDCEnvFor(tc.issuer, tc.redirect, tc.base)
			if ok != tc.wantOK || env.CallbackURL != tc.wantURL {
				t.Fatalf("got %q ok=%v, want %q ok=%v", env.CallbackURL, ok, tc.wantURL, tc.wantOK)
			}
			if ok && env.ConsoleIssuer != issuer {
				t.Errorf("console issuer = %q, want %q", env.ConsoleIssuer, issuer)
			}
		})
	}
}

func TestEndpointOIDCFromFlags(t *testing.T) {
	issuer, redirect, base := "https://login.corp.example/realm", "https://wardyn.corp.example/x", ""
	f := &bootFlags{oidcIssuer: &issuer, oidcRedirectURL: &redirect, basePath: &base}
	env, ok := endpointOIDCFromFlags(f)()
	if !ok || env.ConsoleIssuer != issuer || env.CallbackURL != "https://wardyn.corp.example"+api.EndpointOIDCCallbackPath {
		t.Fatalf("from flags: %+v ok=%v", env, ok)
	}
}
