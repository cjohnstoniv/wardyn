// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// azurePGDispatch dispatches a REAL run (POST /runs through the Postgres-backed server) that chose an
// azure_foundry provider its owner has signed in to, beside the npm and pip artifact redirects the
// duplicate-host guard runs on, and returns the sandbox spec the runner received.
func azurePGDispatch(t *testing.T) runner.SandboxSpec {
	t.Helper()
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"npm-artifactory-token": []byte("s3cr3t-npm-token")}}
	ctx := context.Background()
	if _, err := srv.cfg.Store.PutSiteConfig(ctx, types.SiteConfig{
		ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{{
			ID: "foundry-claude", UID: azUIDAnthropic, Kind: types.ModelProviderAzureFoundry,
			Azure:     &types.AzureSettings{Endpoint: "https://" + azA3Host, Route: types.AzureRouteAnthropic},
			Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: azA3Deployment, FastModel: azA3Fast}},
		}}},
		EgressRedirects: []types.EgressRedirect{
			{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/npm", TokenSecretRef: "npm-artifactory-token", Ecosystem: "npm"},
			{From: "https://pypi.org/", To: "https://artifactory.corp:8443/pypi", TokenSecretRef: "npm-artifactory-token", Ecosystem: "pip"},
		},
	}); err != nil {
		t.Fatalf("seed site config: %v", err)
	}
	t.Cleanup(func() { _, _ = srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{}) })

	// The admin token's runs belong to adminTokenPrincipal; its own sign-in for the row is stored sealed.
	ec, err := azureFoundryCapture(azUIDAnthropic, azureFoundryAudience)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.storeEntraBlob(ctx, adminTokenPrincipal, ec, adoEntraBlob{
		RefreshToken: "refresh-1", Scopes: []string{azFoundryScope}, ExpiresAt: time.Now().Add(time.Hour),
		TenantID: "tenant", ClientID: "client", Subject: adminTokenPrincipal, CapturedAt: time.Now(), Source: adoEntraSourceSignIn,
	}); err != nil {
		t.Fatalf("store the sign-in: %v", err)
	}

	body := `{"agent":"claude-code","repo":"acme/widgets","task":"do the thing","model_provider":"foundry-claude",` +
		`"inline_policy":{"allowed_domains":["registry.npmjs.org"],"min_confinement_class":"CC2"}}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	fr.waitForSandbox(t)
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	return fr.lastSpec
}

// The producer and the consumer of an Azure run's sidecar config, end to end: the config dispatch wrote
// loads on the proxy, whose boot refuses a gate it could not honour, an unpinned rule on the gate host or no
// CA, and the proxy boots with a CA and a resolvable token. Nothing here reaches Azure.
func TestDispatch_AzureFoundryRun_ProxySidecarBoots(t *testing.T) {
	spec := azurePGDispatch(t)
	pc := spec.ProxyConfig
	if pc.MITMCACertPEM == "" || pc.MITMCAKeyPEM == "" {
		t.Fatal("an azure_foundry run dispatched with no per-run CA: the proxy would tunnel the endpoint blind")
	}
	if len(pc.AzureGates) != 1 || pc.AzureGates[0].Host != azA3Host || pc.LLMChannelHosts[azA3Host] != "api.anthropic.com" {
		t.Fatalf("AzureGates = %+v, LLMChannelHosts = %v", pc.AzureGates, pc.LLMChannelHosts)
	}
	if pc.LLMUpstreams != nil {
		t.Fatalf("LLMUpstreams = %v: the Azure host must never be an upstream", pc.LLMUpstreams)
	}
	if spec.Env["ANTHROPIC_FOUNDRY_BASE_URL"] != "https://"+azA3Host+"/anthropic" {
		t.Errorf("ANTHROPIC_FOUNDRY_BASE_URL = %q", spec.Env["ANTHROPIC_FOUNDRY_BASE_URL"])
	}
	if _, set := spec.Env["ANTHROPIC_API_KEY"]; set {
		t.Error("the Messages harness was handed ANTHROPIC_API_KEY")
	}
	if !slices.Contains(pc.Policy.AllowedDomains, azA3Host+":443") {
		t.Errorf("AllowedDomains = %v, want the endpoint", pc.Policy.AllowedDomains)
	}

	// A control-plane stub standing in for the injection-resolve route: the sidecar mints every rule once at boot.
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/internal/injection/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: "Bearer entra-token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(cp.Close)
	pc.ControlPlaneURL = cp.URL
	raw, err := runner.BuildProxyConfig(spec.RunID, pc, 3128)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("LoadConfigBytes over the dispatched ProxyConfig: %v", err)
	}
	cfg.Listen = "127.0.0.1:0"
	psrv, err := proxy.NewServer(context.Background(), cfg, &http.Client{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatalf("proxy.NewServer over an azure_foundry run's own dispatched config: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = psrv.Shutdown(sctx)
	})
}
