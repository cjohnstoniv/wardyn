// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// ─── validDefaultRole: the pure-function table (T-49) ───────────────────────

// TestValidDefaultRole pins validDefaultRole's one deliberate divergence from
// oidc.ValidRole: RoleSecurityAdmin is a MAPPED tier only (see its doc) and
// must never be admissible as the boot-time fallthrough default, while the
// two ladder roles and an unrecognized value behave exactly as ValidRole
// already says.
func TestValidDefaultRole(t *testing.T) {
	for _, tt := range []struct {
		role string
		want bool
	}{
		{oidc.RoleAdmin, true},
		{oidc.RoleUser, true},
		{oidc.RoleSecurityAdmin, false},
		{"junk", false},
		{"", false},
	} {
		if got := validDefaultRole(tt.role); got != tt.want {
			t.Errorf("validDefaultRole(%q) = %v, want %v", tt.role, got, tt.want)
		}
	}
}

// ─── buildOptionalFeatures: the boot-error case, through the real boot path ─

// defaultRoleBootFlags builds the *bootFlags a real buildOptionalFeatures
// call needs to reach the WARDYN_OIDC_DEFAULT_ROLE check: an operator
// allowlist (so validateOperatorPosture, checked right after, never masks
// this assertion) and a real OIDC issuer so a role that passes the check
// keeps going into a genuine discovery round trip rather than stopping for
// an unrelated reason. Mirrors ssoOnlyBootFlags above.
func defaultRoleBootFlags(issuerURL, defaultRole string) *bootFlags {
	recordingSel, recordingDir := "off", ""
	oidcInternalIss, oidcClientID, oidcClientSecret := "", "test-client", ""
	oidcRedirectURL := "http://localhost/auth/callback"
	oidcEmailDomains, oidcRoleMap := "", ""
	oidcOperatorEmails := "ops@example.com"
	allowOIDCNoOperatorList, localMode, memberMode, ssoOnly := false, false, false, false
	dirProvider, dirTenant, dirClientID, dirSecret := "", "", "", ""
	envbuild, scanAIAdvisor := false, false
	sshListen, uiListen := "", ""
	adminToken := ""
	controlURL := "http://127.0.0.1:8080" // loopback: no internal CA to mint
	return &bootFlags{
		recordingSel:            &recordingSel,
		recordingDir:            &recordingDir,
		oidcIssuer:              &issuerURL,
		oidcInternalIss:         &oidcInternalIss,
		oidcClientID:            &oidcClientID,
		oidcClientSecret:        &oidcClientSecret,
		oidcRedirectURL:         &oidcRedirectURL,
		oidcEmailDomains:        &oidcEmailDomains,
		oidcOperatorEmails:      &oidcOperatorEmails,
		allowOIDCNoOperatorList: &allowOIDCNoOperatorList,
		oidcRoleMap:             &oidcRoleMap,
		oidcDefaultRole:         &defaultRole,
		adminToken:              &adminToken,
		localMode:               &localMode,
		memberMode:              &memberMode,
		ssoOnly:                 &ssoOnly,
		dirProvider:             &dirProvider,
		dirTenant:               &dirTenant,
		dirClientID:             &dirClientID,
		dirSecret:               &dirSecret,
		envbuild:                &envbuild,
		scanAIAdvisor:           &scanAIAdvisor,
		sshListen:               &sshListen,
		uiListen:                &uiListen,
		controlURL:              &controlURL,
	}
}

// TestBuildOptionalFeatures_DefaultRoleBootRefusal is the T-49 end-to-end
// half: that boot actually calls validDefaultRole with the resolved
// WARDYN_OIDC_DEFAULT_ROLE value and fails closed before an oidc.Authenticator
// is ever constructed, on both the tier the design specifically refuses
// (security_admin) and an ordinary typo. A role that passes keeps going —
// proven by a real discovery round trip against a test IdP succeeding and
// wiring a non-nil Authenticator, the same "no hand-set bool" shape as
// TestSSOOnlyPosture_WiredThroughTheRealBootPath above.
func TestBuildOptionalFeatures_DefaultRoleBootRefusal(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	oidcSrv := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{
		PublicKey: priv.Public(), KeyID: "test-key", Algorithm: "RS256",
	}}}
	httpSrv := httptest.NewServer(oidcSrv)
	defer httpSrv.Close()
	oidcSrv.SetIssuer(httpSrv.URL)

	newStore := func() *memSecretStore {
		return &memSecretStore{vals: map[string][]byte{secretSessionKey: []byte("01234567890123456789012345678901")}}
	}

	for _, tt := range []struct {
		name    string
		role    string
		wantErr bool
	}{
		{"security_admin refused", oidc.RoleSecurityAdmin, true},
		{"junk value refused", "junk", true},
		{"admin boots clean", oidc.RoleAdmin, false},
		{"user boots clean", oidc.RoleUser, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := defaultRoleBootFlags(httpSrv.URL, tt.role)
			of, err := buildOptionalFeatures(context.Background(), context.Background(), f, nil, newStore(), false, false)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("buildOptionalFeatures: want a boot refusal for WARDYN_OIDC_DEFAULT_ROLE=%q, got nil error", tt.role)
				}
				for _, want := range []string{"WARDYN_OIDC_DEFAULT_ROLE", tt.role} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("buildOptionalFeatures: unexpected error for WARDYN_OIDC_DEFAULT_ROLE=%q: %v", tt.role, err)
			}
			if of.authn == nil {
				t.Fatal("of.authn is nil — a valid default role should have kept boot going into a real OIDC discovery round trip, not stopped short")
			}
		})
	}
}

// ─── chartMapHasNoAdminPath: the pure-function table (T-49) ─────────────────

// TestChartMapHasNoAdminPath pins the third-tier WARN condition: a chart role
// map that grants security_admin somewhere while leaving NO route to the
// super-admin tier (no admin-valued map entry, no operator allowlist, no
// admin default role). Any one of those three routes present, or no
// security_admin grant at all, must NOT trip it.
func TestChartMapHasNoAdminPath(t *testing.T) {
	for _, tt := range []struct {
		name           string
		roleMap        map[string]string
		operatorEmails []string
		defaultRole    string
		want           bool
	}{
		{
			name:    "security_admin only, no admin route anywhere: WARN",
			roleMap: map[string]string{"Wardyn.Security": oidc.RoleSecurityAdmin},
			want:    true,
		},
		{
			name:    "security_admin plus an admin-valued row: no WARN",
			roleMap: map[string]string{"Wardyn.Security": oidc.RoleSecurityAdmin, "Wardyn.Admin": oidc.RoleAdmin},
			want:    false,
		},
		{
			name:           "security_admin only, operator allowlist set: no WARN",
			roleMap:        map[string]string{"Wardyn.Security": oidc.RoleSecurityAdmin},
			operatorEmails: []string{"ops@example.com"},
			want:           false,
		},
		{
			name:        "security_admin only, admin default role: no WARN",
			roleMap:     map[string]string{"Wardyn.Security": oidc.RoleSecurityAdmin},
			defaultRole: oidc.RoleAdmin,
			want:        false,
		},
		{
			name:    "no security_admin grant at all: no WARN",
			roleMap: map[string]string{"eng-team": oidc.RoleUser},
			want:    false,
		},
		{
			name: "empty map: no WARN",
			want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := chartMapHasNoAdminPath(tt.roleMap, tt.operatorEmails, tt.defaultRole); got != tt.want {
				t.Errorf("chartMapHasNoAdminPath(%v, %v, %q) = %v, want %v",
					tt.roleMap, tt.operatorEmails, tt.defaultRole, got, tt.want)
			}
		})
	}
}
