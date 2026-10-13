// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// T14, over the real run, run_harnesses, grant, secret and audit tables: a
// revive keeps the grant each harness's provider authored, not only the one
// the run's own agent's provider did. A grant of a provider the run's set
// does not name is still dropped. Guarded by WARDYN_TEST_PG; skipped cleanly
// when unset.
func TestPG_Revive_KeepsEveryHarnessProviderGrant(t *testing.T) {
	const owner = "sub-pg-harness-lanes"
	h, sec := newRunOwnerPGHarness(t)
	ctx := context.Background()
	st := h.srv.cfg.Store
	a := mpKeyProvider("anthropic", uuid.NewString(), types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code"})
	b := mpKeyProvider("openai", uuid.NewString(), types.ModelProviderOpenAIAPIKey, types.ProviderHarness{Harness: "codex-cli"})
	if _, err := st.PutSiteConfig(ctx, types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{a, b}}}); err != nil {
		t.Fatalf("PutSiteConfig: %v", err)
	}
	run, err := st.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
		ModelProviderID: a.ID, ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.State != types.RunRunning {
		if ok, err := st.UpdateRunStateIf(ctx, run.ID, run.State, types.RunRunning); err != nil || !ok {
			t.Fatalf("UpdateRunStateIf: %v %v", ok, err)
		}
	}
	if err := st.(store.RunHarnessStore).PutRunHarnesses(ctx, run.ID, []types.RunHarness{
		{Harness: "claude-code", ModelProviderID: a.ID, Starts: true}, {Harness: "codex-cli", ModelProviderID: b.ID},
	}); err != nil {
		t.Fatalf("PutRunHarnesses: %v", err)
	}
	if err := st.SetSandboxRef(ctx, run.ID, "wardyn-agent-"+run.ID.String()); err != nil {
		t.Fatalf("SetSandboxRef: %v", err)
	}
	if ok, err := st.(store.RunLoser).MarkRunLost(ctx, run.ID, types.LostOutage, time.Now(), 0); err != nil || !ok {
		t.Fatalf("MarkRunLost: %v %v", ok, err)
	}
	stale := providerSecretName(uuid.NewString(), providerKeyPart)
	var injections []runner.InjectionGrant
	for _, g := range []struct{ host, secret, uid, harness string }{
		{"api.anthropic.com", providerSecretName(a.UID, providerKeyPart), a.UID, "claude-code"},
		{"api.openai.com", providerSecretName(b.UID, providerKeyPart), b.UID, "codex-cli"},
		{"gw.corp.example", stale, uuid.NewString(), "codex-cli"},
	} {
		row, err := st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey,
			Scope: mustJSON(map[string]any{"host": g.host, "secret_name": g.secret,
				"snapshot": providerGrantSnapshot{ProviderUID: g.uid, OwnerSubject: owner, Harness: g.harness}})}})
		if err != nil {
			t.Fatalf("CreateGrant: %v", err)
		}
		injections = append(injections, runner.InjectionGrant{GrantID: row.ID,
			Rule: egress.InjectionRule{Host: g.host, Header: "x-api-key", Format: "%s", SecretName: g.secret}})
		if err := sec.For(owner).Put(ctx, g.secret, []byte("sk-own-"+g.uid)); err != nil {
			t.Fatalf("put secret: %v", err)
		}
	}
	cfg, err := runner.BuildProxyConfig(run.ID, runner.ProxyConfig{
		RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081",
		Policy:    types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "api.openai.com", "gw.corp.example"}},
		Injection: injections,
	}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.RunConfigKey = make([]byte, 32)
	if err := h.srv.storeRunProxyConfig(ctx, run.ID, cfg); err != nil {
		t.Fatalf("store the run's proxy config: %v", err)
	}
	rn := &pgReviveRunner{fakeRunner: &fakeRunner{}}
	h.srv.cfg.Runner = rn
	h.srv.cfg.ControlPlaneURL = "http://127.0.0.1:8080"

	w := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/revive", ssoSession(t, owner, ownerEmail, oidc.RoleUser), "")
	if w.Code != http.StatusOK || rn.replaced != 1 {
		t.Fatalf("revive = %d %s (replaced %d), want 200 and one new proxy", w.Code, w.Body.String(), rn.replaced)
	}
	revived, err := proxy.LoadConfigBytes(rn.cfg)
	if err != nil {
		t.Fatalf("the revived proxy's config does not load: %v", err)
	}
	byHost := map[string]string{}
	for _, in := range revived.Injection {
		byHost[in.Host] = in.SecretName
	}
	for _, p := range []types.ModelProvider{a, b} {
		host := map[string]string{a.ID: "api.anthropic.com", b.ID: "api.openai.com"}[p.ID]
		if byHost[host] != providerSecretName(p.UID, providerKeyPart) || strippedAudited(h, run.ID, byHost[host]) {
			t.Errorf("%s: revived injection on %s = %q; the grant its provider authored must survive", p.ID, host, byHost[host])
		}
	}
	if _, ok := byHost["gw.corp.example"]; ok || !strippedAudited(h, run.ID, stale) {
		t.Error("a grant of a provider the run's set does not name survived the revive, or its drop was not audited")
	}
}
