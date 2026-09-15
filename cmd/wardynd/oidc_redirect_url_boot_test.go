// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestValidateOIDCRedirectURL pins the boot refusal. WARDYN_OIDC_REDIRECT_URL
// is two things: the IdP's callback target (oidc.New only checks it is
// non-empty) and — since 0.7.3 — the second origin internal/api's CSRF guard
// accepts as same-origin. A value that parses to no host silently kills that
// second arm, and behind a TLS-terminating ingress (where the request Host is
// the internal name) EVERY console write then answers 403 "CSRF guard",
// naming no configuration at all. Boot must refuse it while the message can
// still name the variable.
func TestValidateOIDCRedirectURL(t *testing.T) {
	for _, ok := range []string{
		"https://wardyn.example.com/auth/callback",
		"https://wardyn.example.com:8443/auth/callback",
		"http://localhost:8080/auth/callback",
		"  https://wardyn.example.com/auth/callback  ", // surrounding whitespace is the operator's, not a value
	} {
		if err := validateOIDCRedirectURL(ok); err != nil {
			t.Errorf("validateOIDCRedirectURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"",                                    // unset while OIDC is configured
		"wardyn.example.com/auth/callback",    // the common one: no scheme, so no host
		"/auth/callback",                      // path-only
		"https:///auth/callback",              // scheme, empty authority
		"://wardyn.example.com/auth/callback", // typo'd scheme
	} {
		err := validateOIDCRedirectURL(bad)
		if err == nil {
			t.Errorf("validateOIDCRedirectURL(%q) = nil, want a boot refusal", bad)
			continue
		}
		// The message has to name the variable AND the consequence, or the
		// operator reads "invalid URL" and re-reads the IdP docs.
		if !strings.Contains(err.Error(), "WARDYN_OIDC_REDIRECT_URL") || !strings.Contains(err.Error(), "CSRF") {
			t.Errorf("validateOIDCRedirectURL(%q) message does not name the variable and the CSRF consequence: %v", bad, err)
		}
	}
}
