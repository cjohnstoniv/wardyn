// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	azTestUID      = "11111111-aaaa-4aaa-8aaa-000000000001"
	azTestOtherUID = "11111111-aaaa-4aaa-8aaa-000000000003"
)

func azureModelSite(rows ...types.ModelProvider) fakeSiteConfig {
	return fakeSiteConfig{ModelProviders: &types.ModelProviders{Providers: rows}}
}

func azureFoundryRow() types.ModelProvider {
	return types.ModelProvider{ID: "foundry", UID: azTestUID, Kind: types.ModelProviderAzureFoundry,
		Azure: &types.AzureSettings{Endpoint: "https://res.services.ai.azure.com", Route: types.AzureRouteAnthropic}}
}

// TestAzureFoundryEntraByRow_ResolvesByUidOnly: only a provider with that uid
// and the azure_foundry kind answers, and always with the console's own sign-in
// application, never a per-row one.
func TestAzureFoundryEntraByRow_ResolvesByUidOnly(t *testing.T) {
	src := azureFoundryEntraByRow(azureModelSite(azureFoundryRow(),
		types.ModelProvider{ID: "plain", UID: azTestOtherUID, Kind: types.ModelProviderAnthropicAPIKey}), testLogin)
	cfg, found, err := src(context.Background(), azTestUID)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg.RowID != azTestUID || cfg.TenantID != testTenant || cfg.ClientID != testLoginClient ||
		cfg.LoginClientID != testLoginClient || cfg.LoginTenantID != testTenant || cfg.ClientSecret != "console-secret" ||
		cfg.RedirectURL != "https://wardyn.corp.example/api/v1/scm/azure-devops/callback" {
		t.Fatalf("cfg = %+v", cfg)
	}
	for name, uid := range map[string]string{"another kind": azTestOtherUID, "unknown": "nope", "empty": ""} {
		if cfg, found, err := src(context.Background(), uid); found || err != nil {
			t.Errorf("%s: found=%v err=%v cfg=%+v, want not found", name, found, err, cfg)
		}
	}
	if _, found, err := azureFoundryEntraByRow(fakeSiteConfig{}, testLogin)(context.Background(), azTestUID); found || err != nil {
		t.Errorf("no providers: found=%v err=%v", found, err)
	}
}

// TestAzureFoundryEntraByRow_NoConsoleLoginFailsClosed: a deployment without
// Entra console login yields an empty login half, which both api legs refuse.
func TestAzureFoundryEntraByRow_NoConsoleLoginFailsClosed(t *testing.T) {
	noOIDC := newADOEntraLogin("", "", "", "", "", false)
	cfg, found, err := azureFoundryEntraByRow(azureModelSite(azureFoundryRow()), noOIDC)(context.Background(), azTestUID)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg.LoginClientID != "" || cfg.LoginTenantID != "" || cfg.ClientSecret != "" || cfg.ClientID != "" {
		t.Errorf("no OIDC: cfg = %+v, want an empty application", cfg)
	}
}

// TestADOEntraSourceIgnoresAzureFoundryRows: the console-login door reads the
// Azure DevOps source, which never sees a model provider. An azure_foundry row
// beside an Azure DevOps row changes nothing about it, and the row alone is not
// an Azure DevOps configuration at all.
func TestADOEntraSourceIgnoresAzureFoundryRows(t *testing.T) {
	plain, _, err := adoEntraSource(entraSite(testLoginClient), testLogin)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	both := types.SiteConfig(entraSite(testLoginClient))
	both.ModelProviders = &types.ModelProviders{Providers: []types.ModelProvider{azureFoundryRow()}}
	with, found, err := adoEntraSource(fakeSiteConfig(both), testLogin)(context.Background())
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if with.RowID != plain.RowID || len(with.Scopes) != len(plain.Scopes) || with.ClientID != plain.ClientID {
		t.Errorf("an azure_foundry row changed the Azure DevOps source: %+v vs %+v", with, plain)
	}
	if cfg, found, err := adoEntraSource(azureModelSite(azureFoundryRow()), testLogin)(context.Background()); found || err != nil {
		t.Errorf("an azure_foundry row alone is an Azure DevOps configuration: found=%v err=%v cfg=%+v", found, err, cfg)
	}
}
