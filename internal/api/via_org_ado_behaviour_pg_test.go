// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An Azure DevOps minted token on a run on a runner, driven through the real dispatch classification
// (classifyLocalDispatch), the real revive (reviveRunProxy) and the revive's re-classification
// (refreshLocalDeploymentConfig), on the runner lease fixture: a fresh Postgres and the real runner side.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// viaOrgADOFix is a local run of the lease fixture's runner on an Azure DevOps repository: the minted_pat
// connection in the stored site configuration, the delivery policy stored, and one Azure DevOps grant per
// lane host, recorded under the delivery the run was dispatched with.
type viaOrgADOFix struct {
	*runnerLeaseFixture
	sc            types.SiteConfig
	run           types.AgentRun
	hosts         []string
	gate          *proxy.ADOGrantConfig
	grants        []types.CredentialGrant // one per host, in hosts order
	caCert, caKey string
}

func newViaOrgADOFix(t *testing.T, delivery types.GrantDelivery, policy string, edit func(*types.ADOEntraConfig)) *viaOrgADOFix {
	t.Helper()
	fx := &viaOrgADOFix{runnerLeaseFixture: newRunnerLeaseFixture(t)}
	f := fx.runnerLeaseFixture
	row := adoEntraTestRow()
	row.Entra.TokenMode = types.ADOTokenModeMintedPAT
	edit(row.Entra)
	fx.sc = types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}, WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{row}}}
	if _, err := f.st.PutSiteConfig(f.ctx, fx.sc); err != nil {
		t.Fatal(err)
	}
	fx.policy(t, policy)
	run := f.localRun(t, 0)
	// The repository resolves the run's Azure DevOps lane at every classification (localIdentityLanes).
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET repo = $2 WHERE id = $1`, run.ID, adoTestRepo); err != nil {
		t.Fatal(err)
	}
	fx.reload(t, run.ID)
	certPEM, keyPEM, err := generateRunCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fx.caCert, fx.caKey = string(certPEM), string(keyPEM)
	fx.hosts = adoEntraHosts("contoso")
	fx.gate = &proxy.ADOGrantConfig{Organization: "contoso", Capabilities: slices.Clone(row.Entra.DefaultProfile), Hosts: fx.hosts}
	snapshot := adoEntraScopeSnapshot{ProviderRowID: row.ID, Organisation: "contoso", OwnerSubject: fx.run.CreatedBy,
		CredentialSource: string(row.CredentialSource), TenantID: adoTestTenant, ClientID: adoTestClient,
		TokenMode: string(types.ADOTokenModeMintedPAT), Capabilities: row.Entra.DefaultProfile}
	for _, host := range fx.hosts {
		scope := mustJSON(map[string]any{"host": host, "header": adoEntraInjectHeader, "format": adoInjectFormat(types.ADOTokenModeMintedPAT),
			"secret_name": types.ADOEntraAccessTokenSecret, "require_tls": true, "snapshot": snapshot})
		g, err := f.st.CreateGrant(f.ctx, types.CredentialGrant{ID: uuid.New(), RunID: fx.run.ID, CreatedAt: time.Now().UTC(), Delivery: delivery,
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: adoEntraGrantTTLSeconds}})
		if err != nil {
			t.Fatal(err)
		}
		fx.grants = append(fx.grants, g)
	}
	return fx
}

func (fx *viaOrgADOFix) policy(t *testing.T, raw string) {
	t.Helper()
	if _, err := fx.st.PutCredentialDeliveryPolicy(fx.ctx, json.RawMessage(raw), "seed", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func (fx *viaOrgADOFix) reload(t *testing.T, id uuid.UUID) {
	t.Helper()
	run, err := fx.st.GetRun(fx.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	fx.run = run
}

// laneSpec is the proxy config the Azure DevOps lane authors at dispatch (authorADOEntraLane): one token
// injection per lane host bound to its grant, the host:443 interception entry for each, the REST gate, and
// the run's CA, beside which alone the gate loads.
func (fx *viaOrgADOFix) laneSpec(t *testing.T) runner.SandboxSpec {
	t.Helper()
	pc := runner.ProxyConfig{RunToken: "run-token", ControlPlaneURL: "http://127.0.0.1:8081", MITMCACertPEM: fx.caCert, MITMCAKeyPEM: fx.caKey,
		ADOGrant: fx.gate}
	for i, host := range fx.hosts {
		rule, err := injectionRuleFromScope(fx.grants[i].Spec.Scope)
		if err != nil {
			t.Fatal(err)
		}
		pc.Injection = append(pc.Injection, runner.InjectionGrant{GrantID: fx.grants[i].ID, Rule: rule})
		pc.MITMHosts = append(pc.MITMHosts, host+":443")
	}
	return runner.SandboxSpec{RunID: fx.run.ID, ProxyConfig: pc}
}

// dispatch classifies spec as dispatch does, after the final spec writer, and fails the test on a refusal.
func (fx *viaOrgADOFix) dispatch(t *testing.T, spec *runner.SandboxSpec) {
	t.Helper()
	ceiling := ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded())
	if !fx.srv.classifyLocalDispatch(fx.ctx, fx.run, ceiling, fx.sc, spec, nil, llmTransport{}, false) {
		t.Fatalf("a via_org Azure DevOps run was refused at dispatch: %s", auditDump(fx.srv.cfg.Audit.(*syncAudit).snapshot(), fx.run.ID))
	}
}

// wantEngineGate asserts the organisation's engine, built from the run's stored row as a fresh replica builds
// it, holds the Azure DevOps gate over every lane host and the token injection for each.
func (fx *viaOrgADOFix) wantEngineGate(t *testing.T, when string) {
	t.Helper()
	cfg, err := fx.srv.viaOrgEngineConfig(fx.ctx, fx.run.ID)
	if err != nil {
		t.Fatalf("%s: engine config: %v", when, err)
	}
	if cfg.ADOGrant == nil || cfg.ADOGrant.Organization != "contoso" || !slices.Equal(slices.Sorted(slices.Values(cfg.ADOGrant.Hosts)), slices.Sorted(slices.Values(fx.hosts))) {
		t.Fatalf("%s: engine gate = %+v, want the dispatch-time gate over %v", when, cfg.ADOGrant, fx.hosts)
	}
	if len(cfg.Injection) != len(fx.hosts) {
		t.Fatalf("%s: engine injections = %d, want the token on each of the %d lane hosts", when, len(cfg.Injection), len(fx.hosts))
	}
}

// adoLaneInjections counts the injections that carry the Azure DevOps token.
func adoLaneInjections(cfg *proxy.Config) int {
	n := 0
	for _, in := range cfg.Injection {
		if isADOLaneRule(in.InjectionRule) {
			n++
		}
	}
	return n
}

// Dispatch classifies the spec it is about to launch, not an empty one: each via_org Azure DevOps injection
// leaves the runner's copy as a route to the organisation, with its interception entry, and the gate leaves
// it too. The sealed row is the organisation's: it keeps the gate the runner's copy dropped, so the engine
// still serves the token behind it.
func TestPG_ViaOrgADO_DispatchSealsTheGateTheLaptopCopyDrops(t *testing.T) {
	fx := newViaOrgADOFix(t, types.GrantDeliveryViaOrg, adoViaOrgPolicy, func(c *types.ADOEntraConfig) { c.RunnerViaOrg = true })
	spec := fx.laneSpec(t)
	fx.dispatch(t, &spec)

	pc := spec.ProxyConfig
	if pc.ADOGrant != nil || len(pc.Injection) != 0 {
		t.Fatalf("runner's copy kept gate %+v and %d injections, want neither", pc.ADOGrant, len(pc.Injection))
	}
	if len(pc.ViaOrgRoutes) != len(fx.hosts) {
		t.Fatalf("runner's copy has %d via_org routes, want one per lane host (%d): the dispatched spec was not the one classified", len(pc.ViaOrgRoutes), len(fx.hosts))
	}
	for _, g := range fx.grants {
		if !slices.ContainsFunc(pc.ViaOrgRoutes, func(r proxy.ViaOrgRoute) bool { return r.GrantID == g.ID }) {
			t.Errorf("no via_org route for grant %s", g.ID)
		}
	}
	if !slices.Contains(pc.MITMHosts, "dev.azure.com:443") {
		t.Errorf("runner's copy interception entries %v, want dev.azure.com:443 kept for its route", pc.MITMHosts)
	}

	raw, err := fx.srv.loadRunProxyConfig(fx.ctx, fx.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored proxy.Config
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ADOGrant == nil || stored.ADOGrant.Organization != "contoso" {
		t.Fatalf("sealed row gate = %+v, want the dispatch-time gate", stored.ADOGrant)
	}
	if len(stored.ViaOrgRoutes) != len(fx.hosts) || len(stored.Injection) != 0 {
		t.Fatalf("sealed row has %d routes and %d injections, want the runner's copy's routes", len(stored.ViaOrgRoutes), len(stored.Injection))
	}
	fx.wantEngineGate(t, "after dispatch")
}

// A revive sends the runner a copy re-classified with no Azure DevOps gate, and stores the organisation's row
// with the gate it read after the ceiling: the engine a fresh replica builds from the stored row still holds
// the gate, revive after revive.
func TestPG_ViaOrgADO_ReviveKeepsTheGateOnTheStoredRow(t *testing.T) {
	fx := newViaOrgADOFix(t, types.GrantDeliveryViaOrg, adoViaOrgPolicy, func(c *types.ADOEntraConfig) { c.RunnerViaOrg = true })
	fx.running(t, &fx.run, nil)
	fx.placed(t, &fx.run)
	spec := fx.laneSpec(t)
	fx.dispatch(t, &spec)  // stores the sealed row, as dispatch does once the sandbox is placed
	for round := range 2 { // the second revive starts from the row the first one stored
		if lost, err := fx.st.MarkRunLost(fx.ctx, fx.run.ID, types.LostReboot, time.Now(), 0); err != nil || !lost {
			t.Fatalf("round %d: mark lost = %v, %v", round, lost, err)
		}
		stored, err := fx.st.GetRun(fx.ctx, fx.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		fx.fake.Calls = nil
		if _, rerr := fx.srv.reviveRunProxy(fx.ctx, stored, types.ActorSystem, "test", true); rerr != nil {
			t.Fatalf("round %d: revive = %+v", round, rerr)
		}
		sent := slices.IndexFunc(fx.fake.Calls, func(c string) bool { return strings.HasPrefix(c, "replace_proxy ") })
		if sent < 0 || strings.Contains(fx.fake.Calls[sent], `"ado_grant"`) || !strings.Contains(fx.fake.Calls[sent], `"via_org_routes"`) {
			t.Fatalf("round %d: the runner's copy carries the gate, lacks the routes, or no proxy was replaced: %v", round, fx.fake.Calls)
		}
		fx.wantEngineGate(t, fmt.Sprintf("after revive round %d", round+1))
	}
}

// A run dispatched runner_resident keeps the token injection on its runner's copy. With the delivery policy since
// moved to via_org (the connection allows both), the revive's re-classification drops the gate, but a copy that
// still holds the injection keeps its stored gate: the token never reaches the runner without it.
func TestPG_ViaOrgADO_ResidentRunRevivedUnderViaOrgKeepsItsGate(t *testing.T) {
	fx := newViaOrgADOFix(t, types.GrantDeliveryResident, adoResidentPolicy, func(c *types.ADOEntraConfig) { c.RunnerResident, c.RunnerViaOrg = true, true })
	// The stored row as a runner_resident dispatch keeps it: the lane as authored, nothing stripped.
	rendered, err := runner.BuildProxyConfig(fx.run.ID, fx.laneSpec(t).ProxyConfig, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	refresh := func(when string) *proxy.Config {
		t.Helper()
		var cfg proxy.Config
		if err := json.Unmarshal(rendered, &cfg); err != nil {
			t.Fatal(err)
		}
		if err := fx.srv.refreshLocalDeploymentConfig(fx.ctx, fx.run, fx.sc, &cfg); err != nil {
			t.Fatalf("%s: revive re-classification refused: %v", when, err)
		}
		if n := adoLaneInjections(&cfg); n != len(fx.hosts) {
			t.Fatalf("%s: %d Azure DevOps injections kept, want %d", when, n, len(fx.hosts))
		}
		return &cfg
	}

	if cfg := refresh("policy still runner_resident"); cfg.ADOGrant == nil || cfg.ADOGrant.Organization != "contoso" {
		t.Fatalf("policy still runner_resident: gate = %+v, want the stored gate", cfg.ADOGrant)
	}
	fx.policy(t, adoViaOrgPolicy)
	if cfg := refresh("policy now via_org"); cfg.ADOGrant == nil || cfg.ADOGrant.Organization != "contoso" ||
		!slices.Equal(cfg.ADOGrant.Hosts, fx.hosts) {
		t.Fatalf("policy now via_org: gate = %+v beside the token injection, want the stored gate kept", cfg.ADOGrant)
	}
}
