// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// reviveStripGrant is one injection a stored proxy config carries and the
// grant row behind it.
type reviveStripGrant struct {
	host, secret string
	snapshotUID  string // "" = no snapshot, as a grant authored before 0.8.2
}

// reviveWithStoredInjections stores a lost run whose proxy config carries
// grants, revives it through the real run, grant, secret and audit tables —
// as its owner, or with adminRestart through the bulk "Restart with current
// limits" — and returns the revived config's injections by host.
func reviveWithStoredInjections(t *testing.T, adminRestart bool, providers []types.ModelProvider, providerID string,
	grants []reviveStripGrant, secrets map[string]string,
) (map[string]proxy.InjectionConfig, uuid.UUID, *harness) {
	t.Helper()
	h, sec := newRunOwnerPGHarness(t)
	ctx := context.Background()
	st := h.srv.cfg.Store
	const owner = "sub-pg-strip"
	var sc types.SiteConfig
	if len(providers) > 0 {
		sc.ModelProviders = &types.ModelProviders{Providers: providers}
	}
	if _, err := st.PutSiteConfig(ctx, sc); err != nil {
		t.Fatalf("PutSiteConfig: %v", err)
	}
	run, err := st.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
		ModelProviderID: providerID, ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.State != types.RunRunning {
		if ok, err := st.UpdateRunStateIf(ctx, run.ID, run.State, types.RunRunning); err != nil || !ok {
			t.Fatalf("UpdateRunStateIf: %v %v", ok, err)
		}
	}
	if err := st.SetSandboxRef(ctx, run.ID, "wardyn-agent-"+run.ID.String()); err != nil {
		t.Fatalf("SetSandboxRef: %v", err)
	}
	if ok, err := st.(store.RunLoser).MarkRunLost(ctx, run.ID, types.LostOutage, time.Now(), 0); err != nil || !ok {
		t.Fatalf("MarkRunLost: %v %v", ok, err)
	}
	var injections []runner.InjectionGrant
	var hosts []string
	for _, g := range grants {
		scope := map[string]any{"host": g.host, "secret_name": g.secret}
		if g.snapshotUID != "" {
			scope["snapshot"] = providerGrantSnapshot{ProviderUID: g.snapshotUID, OwnerSubject: owner}
		}
		row, err := st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID,
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(scope)}})
		if err != nil {
			t.Fatalf("CreateGrant: %v", err)
		}
		injections = append(injections, runner.InjectionGrant{GrantID: row.ID,
			Rule: egress.InjectionRule{Host: g.host, Header: "x-api-key", Format: "%s", SecretName: g.secret}})
		hosts = append(hosts, g.host)
	}
	for name, v := range secrets {
		if err := sec.Put(ctx, name, []byte(v)); err != nil {
			t.Fatalf("put secret %s: %v", name, err)
		}
	}
	cfg, err := runner.BuildProxyConfig(run.ID, runner.ProxyConfig{
		RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081",
		Policy:    types.RunPolicySpec{AllowedDomains: hosts},
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

	var w *httptest.ResponseRecorder
	if adminRestart {
		w = do(t, h.srv, http.MethodPost, "/api/v1/admin/runs/restart", adminToken, `{"run_ids":["`+run.ID.String()+`"]}`)
	} else {
		w = doSSO(t, h.srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/revive", ssoSession(t, owner, ownerEmail, oidc.RoleUser), "")
	}
	if w.Code != http.StatusOK || rn.replaced != 1 || strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("revive (admin restart %v) = %d %s (replaced %d); want 200 and one new proxy", adminRestart, w.Code, w.Body.String(), rn.replaced)
	}
	revived, err := proxy.LoadConfigBytes(rn.cfg)
	if err != nil {
		t.Fatalf("the revived proxy's config does not load: %v", err)
	}
	byHost := map[string]proxy.InjectionConfig{}
	for _, in := range revived.Injection {
		byHost[in.Host] = in
	}
	return byHost, run.ID, h
}

// strippedAudited reports whether a run.injection.drop row
// (model_credential_not_provider_authored) names the run and the secret.
func strippedAudited(h *harness, runID uuid.UUID, secret string) bool {
	return slices.ContainsFunc(h.audit.snapshot(), func(ev types.AuditEvent) bool {
		return ev.Action == "run.injection.drop" && ev.RunID != nil && *ev.RunID == runID &&
			strings.Contains(string(ev.Data), `"secret_name":"`+secret+`"`) &&
			strings.Contains(string(ev.Data), "model_credential_not_provider_authored")
	})
}

// TestPG_ReviveStripsAModelCredentialNoProviderAuthored (#548, F1): a revive
// replays the config a run was dispatched with, so a run dispatched before
// 0.8.2 still carries the policy's own api_key grant on the operator's
// anthropic-api-key. The revive drops it, audited as dispatch's strip is, and
// keeps a non-model injection: the run revives with no model credential,
// never the operator's. The admin's bulk restart takes the same path.
func TestPG_ReviveStripsAModelCredentialNoProviderAuthored(t *testing.T) {
	for _, adminRestart := range []bool{false, true} {
		got, runID, h := reviveWithStoredInjections(t, adminRestart, nil, "", []reviveStripGrant{
			{host: "api.anthropic.com", secret: "anthropic-api-key"},
			{host: "jira.corp.example", secret: "jira-token"},
		}, map[string]string{"anthropic-api-key": "sk-ant-operator", "jira-token": "jira-secret"})
		if in, ok := got["api.anthropic.com"]; ok {
			t.Errorf("admin restart %v: the revived proxy still injects %s on api.anthropic.com; a legacy model credential must be dropped", adminRestart, in.SecretName)
		}
		if !strippedAudited(h, runID, "anthropic-api-key") {
			t.Errorf("admin restart %v: no run.injection.drop (model_credential_not_provider_authored) naming anthropic-api-key", adminRestart)
		}
		if in, ok := got["jira.corp.example"]; !ok || in.SecretName != "jira-token" {
			t.Errorf("admin restart %v: jira.corp.example injection = %+v (present %v); a non-model injection must survive", adminRestart, in, ok)
		}
	}
}

// TestPG_ReviveKeepsTheProvidersOwnGrant: the grant the run's provider arm
// authored (its snapshot names the provider's UID) survives the revive; a
// model credential whose snapshot names another provider's UID (one deleted
// and recreated since) is dropped and audited.
func TestPG_ReviveKeepsTheProvidersOwnGrant(t *testing.T) {
	uid := uuid.NewString()
	p := mpKeyProvider("anthropic", uid, types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code"})
	own := providerSecretName(uid, providerKeyPart)
	stale := providerSecretName(uuid.NewString(), providerKeyPart)
	got, runID, h := reviveWithStoredInjections(t, false, []types.ModelProvider{p}, p.ID, []reviveStripGrant{
		{host: "api.anthropic.com", secret: own, snapshotUID: uid},
		{host: "api.openai.com", secret: stale, snapshotUID: uuid.NewString()},
	}, map[string]string{own: "sk-ant-own", stale: "sk-stale"})
	if in, ok := got["api.anthropic.com"]; !ok || in.SecretName != own {
		t.Errorf("api.anthropic.com injection = %+v (present %v); the provider's own grant must survive", in, ok)
	}
	if strippedAudited(h, runID, own) {
		t.Error("the provider's own grant was audited as dropped")
	}
	if _, ok := got["api.openai.com"]; ok {
		t.Error("the revived proxy still injects a grant another provider's UID authored")
	}
	if !strippedAudited(h, runID, stale) {
		t.Errorf("no run.injection.drop naming %s", stale)
	}
}
