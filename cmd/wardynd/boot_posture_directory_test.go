// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/directory"
)

// The two legs PF-29 names are the reason this refusal exists at all, so the
// table leads with them: WARDYN_DIRECTORY_PROVIDER=entra against a PUBLIC OIDC
// client, and against no OIDC at all. Neither can perform the client-credentials
// flow Microsoft Graph requires, and internal/directory deliberately cannot
// catch either — NewEntra sees only the credentials it is handed, so its ONLY
// answer is ErrUnconfigured, a lazy 503 at the first keystroke. The
// misconfiguration would then be discovered by an admin typing into a combobox
// rather than by the operator who set the variable.
func TestResolveDirectoryConfig(t *testing.T) {
	const entraIssuer = "https://login.microsoftonline.com/00000000-1111-2222-3333-444444444444/v2.0"

	for _, tc := range []struct {
		name string
		// inputs, in resolveDirectoryConfig's own order
		provider, dirTenant, dirClientID, dirSecret string
		oidcIssuer, oidcClientID, oidcSecret        string
		// wantErr non-empty = a boot refusal whose message must contain it
		wantErr string
		// on success: the credentials the connector must receive
		wantTenant, wantClientID, wantSecret string
	}{
		// ── OFF: the default, and it must stay a total no-op ──
		{
			name: "unset provider is the feature off, not an error",
			// Even with a full OIDC deployment sitting right there.
			oidcIssuer: entraIssuer, oidcClientID: "oidc-app", oidcSecret: "oidc-secret",
		},
		{
			name:     "whitespace-only provider is still off",
			provider: "   ",
		},

		// ── PF-29 leg 1: a PUBLIC OIDC client (PKCE, no secret) ──
		// This is a SUPPORTED, documented OIDC shape (WARDYN_OIDC_CLIENT_SECRET is
		// optional — some IdPs refuse to issue one), which is exactly why it has
		// to be refused here: nothing else in boot would find it odd.
		{
			name:     "entra with a public OIDC client and no dedicated app",
			provider: "entra",
			// A client id but no secret: the PKCE registration.
			oidcIssuer: entraIssuer, oidcClientID: "public-spa",
			wantErr: "PUBLIC client",
		},
		{
			name:       "entra with an OIDC issuer but no client id either",
			provider:   "entra",
			oidcIssuer: entraIssuer,
			wantErr:    "PUBLIC client",
		},

		// ── PF-29 leg 2: no OIDC configured at all ──
		// The admin-token / local-mode deployment: no issuer to derive a tenant
		// from, no client registration to reuse.
		{
			name:     "entra with no OIDC at all",
			provider: "entra",
			wantErr:  "no OIDC issuer is configured",
		},
		{
			name:     "entra with OIDC credentials but no issuer is still leg 2",
			provider: "entra",
			// Credentials without an issuer are not a deployment; the issuer is
			// what makes OIDC configured (of.authn is built from it).
			oidcClientID: "app", oidcSecret: "shh",
			wantErr: "no OIDC issuer is configured",
		},

		// ── the DEFAULT path: reuse the OIDC confidential app ──
		{
			name:     "entra derives the tenant from the OIDC issuer",
			provider: "entra",
			// The one-variable common case: provider alone, everything else
			// inherited.
			oidcIssuer: entraIssuer, oidcClientID: "oidc-app", oidcSecret: "oidc-secret",
			wantTenant:   "00000000-1111-2222-3333-444444444444",
			wantClientID: "oidc-app", wantSecret: "oidc-secret",
		},
		{
			name:         "the v1.0 sts.windows.net issuer form carries a tenant too",
			provider:     "entra",
			oidcIssuer:   "https://sts.windows.net/contoso.onmicrosoft.com/",
			oidcClientID: "oidc-app", oidcSecret: "oidc-secret",
			wantTenant:   "contoso.onmicrosoft.com",
			wantClientID: "oidc-app", wantSecret: "oidc-secret",
		},
		{
			name:       "case-insensitive provider value",
			provider:   "Entra",
			oidcIssuer: entraIssuer, oidcClientID: "oidc-app", oidcSecret: "oidc-secret",
			wantTenant:   "00000000-1111-2222-3333-444444444444",
			wantClientID: "oidc-app", wantSecret: "oidc-secret",
		},
		{
			name:     "a non-Entra issuer has no derivable tenant",
			provider: "entra",
			// Dex/Keycloak also have a first path segment. Deriving "dex" as a
			// tenant would turn a refusable misconfiguration into a runtime 502
			// against a tenant that does not exist.
			oidcIssuer: "https://sso.corp.example/dex", oidcClientID: "oidc-app", oidcSecret: "oidc-secret",
			wantErr: "no Entra tenant can be derived",
		},
		{
			name:         "a sovereign-cloud issuer is refused, not silently pointed at commercial Graph",
			provider:     "entra",
			oidcIssuer:   "https://login.microsoftonline.us/11111111-2222-3333-4444-555555555555/v2.0",
			oidcClientID: "oidc-app", oidcSecret: "oidc-secret",
			wantErr: "no Entra tenant can be derived",
		},

		// ── the DEDICATED app registration override ──
		{
			name:      "all three dedicated vars win over the OIDC ones",
			provider:  "entra",
			dirTenant: "tenant.example", dirClientID: "dir-app", dirSecret: "dir-secret",
			oidcIssuer: entraIssuer, oidcClientID: "oidc-app", oidcSecret: "oidc-secret",
			wantTenant: "tenant.example", wantClientID: "dir-app", wantSecret: "dir-secret",
		},
		{
			name:     "the dedicated app works with no OIDC at all",
			provider: "entra",
			// The admin-token deployment's supported path out of leg 2.
			dirTenant: "tenant.example", dirClientID: "dir-app", dirSecret: "dir-secret",
			wantTenant: "tenant.example", wantClientID: "dir-app", wantSecret: "dir-secret",
		},
		{
			name:     "a half-configured dedicated app is refused, not silently swapped for the OIDC one",
			provider: "entra",
			// Naming ANY dedicated var selects that path. Falling back here would
			// use credentials the operator did not intend to use.
			dirClientID: "dir-app", dirSecret: "dir-secret",
			oidcIssuer: entraIssuer, oidcClientID: "oidc-app", oidcSecret: "oidc-secret",
			wantErr: "WARDYN_DIRECTORY_TENANT is empty",
		},
		{
			name:      "two missing dedicated vars are both named",
			provider:  "entra",
			dirTenant: "tenant.example",
			wantErr:   "WARDYN_DIRECTORY_CLIENT_ID and WARDYN_DIRECTORY_CLIENT_SECRET are empty",
		},

		// ── an unknown provider is a refusal, never a silent disable ──
		{
			name:       "a typo'd provider must not read as off",
			provider:   "entraid",
			oidcIssuer: entraIssuer, oidcClientID: "oidc-app", oidcSecret: "oidc-secret",
			wantErr: "unknown WARDYN_DIRECTORY_PROVIDER",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDirectoryConfig(tc.provider, tc.dirTenant, tc.dirClientID, tc.dirSecret,
				tc.oidcIssuer, tc.oidcClientID, tc.oidcSecret)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want a boot refusal containing %q, got config %+v and nil error", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("refusal = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				// Every refusal in this file is a boot refusal, and a refusal the
				// operator cannot act on is a support ticket: it must say it is
				// refusing to start AND name a way forward.
				if !strings.HasPrefix(err.Error(), "refusing to start:") {
					t.Errorf("refusal does not use the boot-refusal voice: %q", err.Error())
				}
				if !strings.Contains(err.Error(), "WARDYN_DIRECTORY_PROVIDER") &&
					!strings.Contains(err.Error(), "WARDYN_DIRECTORY_TENANT") {
					t.Errorf("refusal names no variable to fix: %q", err.Error())
				}
				// A config must never escape alongside an error — the caller
				// returns early, but a half-built one here would be a live
				// connector the moment someone reorders that.
				if got != (directory.EntraConfig{}) {
					t.Errorf("refused but returned a non-zero config: %+v", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if got.TenantID != tc.wantTenant || got.ClientID != tc.wantClientID || got.ClientSecret != tc.wantSecret {
				t.Fatalf("config = {tenant:%q client:%q secret:%q}, want {tenant:%q client:%q secret:%q}",
					got.TenantID, got.ClientID, got.ClientSecret, tc.wantTenant, tc.wantClientID, tc.wantSecret)
			}
		})
	}
}
