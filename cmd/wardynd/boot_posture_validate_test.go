// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestValidateBootPosture pins the wiring, not the rules: each validator has
// its own table test, so this only proves validateBootPosture still calls the
// UI-sandbox, base-path and hybrid refusals that run() relies on before migration.
func TestValidateBootPosture(t *testing.T) {
	for _, tc := range []struct {
		name, listen, uiListen, orgURL, basePath string
		memberMode                               bool
		wantErr                                  string // substring; empty = must succeed
	}{
		{name: "nothing set boots", listen: ":8080"},
		{name: "hybrid without member mode refused", listen: ":8080", orgURL: "https://org.example.com", wantErr: "WARDYN_ORG_URL is set but WARDYN_USER_DESKTOP is not"},
		{name: "UI-sandbox on the console address refused", listen: ":8080", uiListen: ":8080", wantErr: "same address as -listen"},
		{name: "a base path with a trailing slash refused", listen: ":8080", basePath: "/wardyn/", wantErr: "WARDYN_BASE_PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sshListen, originTemplate, enrolToken, allowPlaintext := "", "", "", false
			oidcIssuer, oidcRedirect, controlURL := "", "", "https://wardynd:8443"
			f := &bootFlags{
				basePath:             &tc.basePath,
				oidcIssuer:           &oidcIssuer,
				oidcRedirectURL:      &oidcRedirect,
				controlURL:           &controlURL,
				listen:               &tc.listen,
				uiListen:             &tc.uiListen,
				sshListen:            &sshListen,
				uiOriginTemplate:     &originTemplate,
				allowPlaintextListen: &allowPlaintext,
				orgURL:               &tc.orgURL,
				orgEnrolToken:        &enrolToken,
				memberMode:           &tc.memberMode,
			}
			err := validateBootPosture(f, tlsPosture{})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v does not mention %q", err, tc.wantErr)
			}
		})
	}
}
