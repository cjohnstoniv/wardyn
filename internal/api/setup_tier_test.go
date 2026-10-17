// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
)

func TestSetupTier(t *testing.T) {
	enrolled := func() federation.Status { return federation.Status{} }
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"bare daemon", Config{}, TierLocalOnly},
		{"local mode", Config{LocalMode: true}, TierLocalOnly},
		{"local mode with sso", Config{LocalMode: true, OIDC: &oidc.Authenticator{}}, TierLocalOnly},
		{"member mode with sso", Config{MemberMode: true, OIDC: &oidc.Authenticator{}}, TierLocalOnly},
		{"token-only server", Config{AdminToken: "x"}, TierOrg},
		{"sso", Config{OIDC: &oidc.Authenticator{}}, TierOrg},
		{"org-enrolled laptop", Config{OrgFederation: enrolled}, TierRunner},
		{"enrolled beats sso", Config{OIDC: &oidc.Authenticator{}, OrgFederation: enrolled}, TierRunner},
	} {
		if got := (&Server{cfg: tc.cfg}).setupTier(); got != tc.want {
			t.Errorf("%s: tier = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The tier rides every caller's setup status: redaction for a user must not drop it.
func TestSetupStatusKeepsTierForUser(t *testing.T) {
	if got := redactSetupStatusForUser(SetupStatus{Tier: TierOrg}).Tier; got != TierOrg {
		t.Errorf("redacted tier = %q, want %q", got, TierOrg)
	}
}
