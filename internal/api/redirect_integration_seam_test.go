// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// redirect_integration_seam_test.go covers the seam between the two surfaces:
// a Corporate-network redirect row taking its token FROM an integration
// instead of naming a bare secret. The rule under test throughout — the
// integration owns the system and its credential; the redirect owns rerouting
// a public endpoint to it.

// seamIntegration is a package feed that authenticates with something OTHER
// than "Authorization: Bearer" — the case the bare-secret path cannot express.
func seamIntegration() types.Integration {
	return types.Integration{
		ID: "corp-artifactory", Name: "Corp Artifactory",
		Kind:   "artifactory",
		Egress: []string{"artifactory.corp.internal"},
		Secrets: []types.IntegrationSecret{{Role: types.IntegrationCredentialToken, SecretName: "artifactory-token",
			Delivery: &types.IntegrationDelivery{Mode: types.DeliveryProxyHeader, Header: "X-JFrog-Art-Api", Format: "%s"}}},
	}
}

func seamSrv(t *testing.T, integs []types.Integration) *Server {
	t.Helper()
	return New(integrationsTestConfig(t, types.SiteConfig{Integrations: integs}, nil))
}

// ─── the legacy path is untouched ────────────────────────────────────────────

// A bare token_secret_ref must resolve exactly as it always has:
// "Authorization: Bearer <secret>". This is the byte-identical-behavior guard
// for every redirect that existed before the seam.
func TestResolveRedirectToken_BareSecretIsUnchanged(t *testing.T) {
	srv := seamSrv(t, nil)
	present := map[string]bool{"mirror-token": true}
	got, why := srv.resolveRedirectToken(context.Background(),
		types.EgressRedirect{To: "artifactory.corp", TokenSecretRef: "mirror-token"}, present)
	if why != "" {
		t.Fatalf("unexpected refusal: %s", why)
	}
	want := redirectToken{secretName: "mirror-token", header: "Authorization", format: "Bearer %s"}
	if got != want {
		t.Errorf("resolved = %+v, want %+v", got, want)
	}
}

// ─── the seam ────────────────────────────────────────────────────────────────

// The integration's OWN header and format come along with the secret name.
// That is the whole gain: the hardcoded Bearer above would send a header this
// feed rejects.
func TestResolveRedirectToken_IntegrationSuppliesHeaderAndFormat(t *testing.T) {
	srv := seamSrv(t, []types.Integration{seamIntegration()})
	present := map[string]bool{"artifactory-token": true}
	got, why := srv.resolveRedirectToken(context.Background(),
		types.EgressRedirect{To: "artifactory.corp.internal", TokenIntegrationRef: "corp-artifactory"}, present)
	if why != "" {
		t.Fatalf("unexpected refusal: %s", why)
	}
	want := redirectToken{secretName: "artifactory-token", header: "X-JFrog-Art-Api", format: "%s"}
	if got != want {
		t.Errorf("resolved = %+v, want the integration's own presentation %+v", got, want)
	}
}

// An integration with a header but no explicit Format means the RAW secret is
// the value — it must be written as "%s", never left empty, because
// injectionRuleFromScope reads an empty format as "Bearer %s".
func TestResolveRedirectToken_EmptyFormatBecomesRawSecret(t *testing.T) {
	integ := seamIntegration()
	integ.Secrets[0].Delivery.Format = ""
	srv := seamSrv(t, []types.Integration{integ})
	got, why := srv.resolveRedirectToken(context.Background(),
		types.EgressRedirect{To: "artifactory.corp.internal", TokenIntegrationRef: "corp-artifactory"},
		map[string]bool{"artifactory-token": true})
	if why != "" || got.format != "%s" {
		t.Errorf("resolved = %+v (why=%q), want format %%s", got, why)
	}
}

// ─── degrade to redirect-only, never fail the run ───────────────────────────

// Every one of these keeps the REROUTING and drops only the token — the same
// posture a dangling token_secret_ref already had. A redirect that still
// reroutes is more useful than a run that fails.
func TestResolveRedirectToken_DegradesToRedirectOnly(t *testing.T) {
	cases := []struct {
		name    string
		integs  []types.Integration
		present map[string]bool
		ref     string
		wantWhy string
	}{
		{
			name: "integration not configured yet", ref: "not-configured-yet",
			wantWhy: "token_integration_ref names no configured integration",
		},
		{
			name: "integration disabled", ref: "corp-artifactory",
			integs: func() []types.Integration {
				i := seamIntegration()
				i.Disabled = true
				return []types.Integration{i}
			}(),
			present: map[string]bool{"artifactory-token": true},
			wantWhy: "the integration named by token_integration_ref is disabled",
		},
		{
			name: "integration delivers no header credential", ref: "corp-postgres",
			integs: []types.Integration{{
				ID: "corp-postgres", Kind: "postgres",
				Egress: []string{"db.corp.internal:5432"},
			}},
			wantWhy: "the integration named by token_integration_ref delivers no header credential",
		},
		{
			name: "the integration's secret is not stored", ref: "corp-artifactory",
			integs:  []types.Integration{seamIntegration()},
			present: nil, // absent
			wantWhy: "the integration's secret is not in the store",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := seamSrv(t, c.integs)
			got, why := srv.resolveRedirectToken(context.Background(),
				types.EgressRedirect{To: "corp.internal", TokenIntegrationRef: c.ref}, c.present)
			if why != c.wantWhy {
				t.Errorf("why = %q, want %q", why, c.wantWhy)
			}
			if got != (redirectToken{}) {
				t.Errorf("a refusal must resolve no token, got %+v", got)
			}
		})
	}
}

// ─── write-time validation ───────────────────────────────────────────────────

func TestValidateSiteConfig_RedirectTokenSources(t *testing.T) {
	red := func(r types.EgressRedirect) types.SiteConfig {
		return types.SiteConfig{EgressRedirects: []types.EgressRedirect{r}}
	}
	cases := []struct {
		name string
		cfg  types.SiteConfig
		ok   bool
	}{
		{"bare secret alone", red(types.EgressRedirect{From: "ghcr.io", To: "registry.corp.internal", TokenSecretRef: "mirror-token"}), true},
		{"integration ref alone", red(types.EgressRedirect{From: "ghcr.io", To: "registry.corp.internal", TokenIntegrationRef: "corp-artifactory"}), true},
		{"neither", red(types.EgressRedirect{From: "ghcr.io", To: "registry.corp.internal"}), true},
		// Naming an integration that does not exist yet is deliberately fine —
		// the row still reroutes and simply carries no token until it does.
		{"integration ref that does not exist yet", red(types.EgressRedirect{From: "ghcr.io", To: "registry.corp.internal", TokenIntegrationRef: "not-configured-yet"}), true},
		{"BOTH sources set", red(types.EgressRedirect{From: "ghcr.io", To: "registry.corp.internal",
			TokenSecretRef: "mirror-token", TokenIntegrationRef: "corp-artifactory"}), false},
		{"malformed integration ref", red(types.EgressRedirect{From: "ghcr.io", To: "registry.corp.internal", TokenIntegrationRef: "Not A Ref!"}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateSiteConfig(c.cfg); (err == nil) != c.ok {
				t.Errorf("validateSiteConfig = %v, want ok=%v", err, c.ok)
			}
		})
	}
}
