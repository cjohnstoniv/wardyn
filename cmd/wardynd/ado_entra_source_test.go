// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type fakeSiteConfig types.SiteConfig

func (f fakeSiteConfig) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig(f), nil
}

const (
	testLoginClient = "c2e308c7-0000-0000-0000-000000000000"
	testTenant      = "8f026763-0000-0000-0000-000000000000"
)

func entraSite(clientID string) fakeSiteConfig {
	return fakeSiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{{
		ID: "ado", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://dev.azure.com/contoso"},
		Lanes: []types.GitLane{types.GitLaneEntra}, CredentialSource: types.CredentialSourcePerUser,
		Entra: &types.ADOEntraConfig{TenantID: testTenant, ClientID: clientID,
			CapabilityCeiling: []adoscope.Capability{adoscope.CapCodeRead}},
	}}}}
}

var testLogin = newADOEntraLogin("https://login.microsoftonline.com/"+testTenant+"/v2.0",
	testLoginClient, "console-secret", "https://wardyn.corp.example/auth/callback", "", false)

// An unconfigured deployment is unchanged: no entra row answers found=false,
// which is the same refusal the sign-in doors give with no source at all.
func TestADOEntraSource_UnconfiguredIsNotFound(t *testing.T) {
	for name, sc := range map[string]fakeSiteConfig{
		"no providers": {},
		"github only": {WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
			{ID: "gh", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com/acme"}}}}},
	} {
		if cfg, found, err := adoEntraSource(sc, testLogin)(context.Background()); found || err != nil {
			t.Errorf("%s: found=%v err=%v cfg=%+v, want not found", name, found, err, cfg)
		}
	}
}

// The login half comes from the console's OWN OIDC configuration.
func TestADOEntraSource_FillsBothSeams(t *testing.T) {
	cfg, found, err := adoEntraSource(entraSite(testLoginClient), testLogin)(context.Background())
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg.RowID != "ado" || cfg.TenantID != testTenant || cfg.ClientID != testLoginClient ||
		cfg.LoginClientID != testLoginClient || cfg.LoginTenantID != testTenant ||
		cfg.ClientSecret != "console-secret" || len(cfg.Scopes) == 0 ||
		cfg.RedirectURL != "https://wardyn.corp.example/api/v1/scm/azure-devops/callback" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

// A row naming another application never receives the console's secret, and a
// deployment with no Entra OIDC leaves the login half empty (fail closed).
func TestADOEntraSource_FailsClosed(t *testing.T) {
	cfg, _, _ := adoEntraSource(entraSite("99999999-0000-0000-0000-000000000000"), testLogin)(context.Background())
	if cfg.ClientSecret != "" {
		t.Errorf("a foreign application was handed the console's client secret")
	}
	noOIDC := newADOEntraLogin("", "", "", "", "", false)
	cfg, _, _ = adoEntraSource(entraSite(testLoginClient), noOIDC)(context.Background())
	if cfg.LoginClientID != "" || cfg.LoginTenantID != "" || cfg.ClientSecret != "" {
		t.Errorf("no OIDC: cfg = %+v, want an empty login half", cfg)
	}
	dex := newADOEntraLogin("https://sso.corp.example/dex", testLoginClient, "s", "", "", false)
	if cfg, _, _ = adoEntraSource(entraSite(testLoginClient), dex)(context.Background()); cfg.LoginTenantID != "" {
		t.Errorf("a non-Entra issuer produced a login tenant %q", cfg.LoginTenantID)
	}
}

func mintedSite(clientID, tenantID string) fakeSiteConfig {
	sc := entraSite(clientID)
	sc.WorkspaceProviders.Git[0].Entra.TenantID = tenantID
	sc.WorkspaceProviders.Git[0].Entra.TokenMode = types.ADOTokenModeMintedPAT
	return sc
}

// A minted_pat row on the console's own confidential app asks for the two
// token permissions and nothing else, and is handed the console's secret.
func TestADOEntraSource_MintedRowAsksForTheMintScopes(t *testing.T) {
	cfg, found, err := adoEntraSource(mintedSite(testLoginClient, testTenant), testLogin)(context.Background())
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg.TokenMode != types.ADOTokenModeMintedPAT || !slices.Equal(cfg.Scopes, adoscope.MintScopes()) || cfg.ClientSecret != "console-secret" {
		t.Fatalf("cfg = %+v, want minted_pat, exactly the mint scopes, the console secret", cfg)
	}
}

// S1 at first read: a minted_pat row the console cannot redeem with its own
// secret resolves with no secret, which every api door reads as unusable
// (ado_pat_needs_console_app) — never as a public client.
func TestADOEntraSource_S1FirstReadLeavesNoSecret(t *testing.T) {
	noSecret := newADOEntraLogin("https://login.microsoftonline.com/"+testTenant+"/v2.0",
		testLoginClient, "", "https://wardyn.corp.example/auth/callback", "", false)
	for name, tc := range map[string]struct {
		sc    fakeSiteConfig
		login adoEntraLogin
	}{
		"another application": {mintedSite("99999999-0000-0000-0000-000000000000", testTenant), testLogin},
		"another tenant":      {mintedSite(testLoginClient, "77777777-0000-0000-0000-000000000000"), testLogin},
		"no console secret":   {mintedSite(testLoginClient, testTenant), noSecret},
	} {
		cfg, found, err := adoEntraSource(tc.sc, tc.login)(context.Background())
		if err != nil || !found || cfg.TokenMode != types.ADOTokenModeMintedPAT || cfg.ClientSecret != "" {
			t.Errorf("%s: found=%v err=%v secret=%q mode=%q, want a minted row with no secret", name, found, err, cfg.ClientSecret, cfg.TokenMode)
		}
	}
}

// An own_pat row has no sign-in: it never drives the login, never shadows a
// row that does, and never reaches the api's "unusable" warning.
func TestADOEntraSource_SkipsOwnPATRows(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	own := entraSite(testLoginClient).WorkspaceProviders.Git[0]
	own.ID, own.Entra = "own", &types.ADOEntraConfig{TokenMode: types.ADOTokenModeOwnPAT,
		CapabilityCeiling: []adoscope.Capability{adoscope.CapCodeRead}}
	if _, found, err := adoEntraSource(fakeSiteConfig{WorkspaceProviders: &types.WorkspaceProviders{
		Git: []types.GitProvider{own}}}, testLogin)(context.Background()); found || err != nil {
		t.Errorf("an own_pat row alone: found=%v err=%v, want not found", found, err)
	}
	sc := mintedSite(testLoginClient, testTenant)
	sc.WorkspaceProviders.Git = append([]types.GitProvider{own}, sc.WorkspaceProviders.Git...)
	cfg, found, err := adoEntraSource(sc, testLogin)(context.Background())
	if err != nil || !found || cfg.RowID != "ado" {
		t.Errorf("own_pat first: row %q found=%v err=%v, want the minted row", cfg.RowID, found, err)
	}
	// Through the api's own login seam: no widened login, nothing captured,
	// and no "unusable" warning for a row that simply has no sign-in.
	srv := api.New(api.Config{ADOEntra: adoEntraSource(fakeSiteConfig{WorkspaceProviders: &types.WorkspaceProviders{
		Git: []types.GitProvider{own}}}, testLogin)})
	if got := srv.LoginScopes(context.Background()); got != nil {
		t.Errorf("LoginScopes = %v with only an own_pat row, want nil", got)
	}
	srv.CaptureLoginGrant(context.Background(), "a-person", oidc.LoginGrant{RefreshToken: "rt-0123456789abcdef",
		Scope: strings.Join(adoscope.MintScopes(), " ")})
	if logs.Len() != 0 {
		t.Errorf("logged %q", logs.String())
	}
}

// One application holds one consent: a bearer row naming the minted row's
// application would carry the token permissions into runs.
func TestADOEntraSource_BearerAndMintedRowsMayNotShareAnApplication(t *testing.T) {
	sc := mintedSite(testLoginClient, testTenant)
	bearer := entraSite(testLoginClient).WorkspaceProviders.Git[0]
	bearer.ID = "ado-bearer"
	sc.WorkspaceProviders.Git = append(sc.WorkspaceProviders.Git, bearer)
	if _, _, err := adoEntraSource(sc, testLogin)(context.Background()); err == nil || !strings.Contains(err.Error(), "same application") {
		t.Fatalf("err = %v, want the shared-application refusal", err)
	}
	bearer.Entra = &types.ADOEntraConfig{TenantID: testTenant, ClientID: "99999999-0000-0000-0000-000000000000",
		CapabilityCeiling: []adoscope.Capability{adoscope.CapCodeRead}}
	sc.WorkspaceProviders.Git[1] = bearer
	if _, found, err := adoEntraSource(sc, testLogin)(context.Background()); err != nil || !found {
		t.Fatalf("different applications: found=%v err=%v", found, err)
	}
}

func TestADOLoginFacts(t *testing.T) {
	if c, tn, has := testLogin.facts(); c != testLoginClient || tn != testTenant || !has {
		t.Errorf("with a secret: %q %q %v", c, tn, has)
	}
	noSecret := newADOEntraLogin("https://login.microsoftonline.com/"+testTenant+"/v2.0", testLoginClient, "", "", "", false)
	if c, tn, has := noSecret.facts(); c != testLoginClient || tn != testTenant || has {
		t.Errorf("without a secret: %q %q %v", c, tn, has)
	}
	f := &bootFlags{}
	issuer, client, secret, redirect, base, allow := "https://login.microsoftonline.com/"+testTenant+"/v2.0", testLoginClient, "s", "", "", false
	f.oidcIssuer, f.oidcClientID, f.oidcClientSecret, f.oidcRedirectURL, f.basePath, f.allowTestEndpoints = &issuer, &client, &secret, &redirect, &base, &allow
	if c, tn, has := adoLoginFactsFromFlags(f)(); c != testLoginClient || tn != testTenant || !has {
		t.Errorf("from flags: %q %q %v", c, tn, has)
	}
}
