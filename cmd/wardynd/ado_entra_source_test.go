// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
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
			CapabilityCeiling: []adoscope.Capability{adoscope.CapRead}},
	}}}}
}

var testLogin = newADOEntraLogin("https://login.microsoftonline.com/"+testTenant+"/v2.0",
	testLoginClient, "console-secret", "https://wardyn.corp.example/auth/callback", false)

// AN UNCONFIGURED DEPLOYMENT IS UNCHANGED: no entra row answers found=false,
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
	noOIDC := newADOEntraLogin("", "", "", "", false)
	cfg, _, _ = adoEntraSource(entraSite(testLoginClient), noOIDC)(context.Background())
	if cfg.LoginClientID != "" || cfg.LoginTenantID != "" || cfg.ClientSecret != "" {
		t.Errorf("no OIDC: cfg = %+v, want an empty login half", cfg)
	}
	dex := newADOEntraLogin("https://sso.corp.example/dex", testLoginClient, "s", "", false)
	if cfg, _, _ = adoEntraSource(entraSite(testLoginClient), dex)(context.Background()); cfg.LoginTenantID != "" {
		t.Errorf("a non-Entra issuer produced a login tenant %q", cfg.LoginTenantID)
	}
}
