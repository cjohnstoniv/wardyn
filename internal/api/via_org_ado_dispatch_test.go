// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoViaOrgPolicy delivers ado_minted_pat via_org on every trust class.
const adoViaOrgPolicy = `{"classes":{"ado_minted_pat":{"mode":"via_org"}}}`

// A via_org Azure DevOps run records its grants via_org on a person's runner and on an organisation's cluster runner
// alike, with the connection's via_org switch alone (runner_resident stays off); with the switch off it is refused.
func TestADOViaOrg_DispatchRecordsTheGrantViaOrg(t *testing.T) {
	for _, tc := range []struct {
		name    string
		switchV bool
		edit    func(*adoLocalStore)
		want    bool
	}{
		{name: "person's runner, switch on", switchV: true, want: true},
		{name: "org_cluster runner, switch on", switchV: true, want: true,
			edit: func(s *adoLocalStore) {
				s.runner.TrustClass, s.runner.OwnerKind = types.RunnerTrustOrgCluster, types.RunnerOwnerOrg
			}},
		{name: "switch off", switchV: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lf := newADOLocalLane(t, adoViaOrgPolicy, false)
			lf.st.site.WorkspaceProviders.Git[0].Entra.RunnerViaOrg = tc.switchV
			if tc.edit != nil {
				lf.ls.set(tc.edit)
			}
			if got := lf.dispatchLocal(t); got != tc.want {
				t.Fatalf("dispatch admitted = %v (hint %q), want %v", got, lf.st.hint, tc.want)
			}
			if !tc.want {
				return
			}
			if len(lf.st.grants) == 0 {
				t.Fatal("no Azure DevOps grant was recorded")
			}
			for _, g := range lf.st.grants {
				if g.Delivery != types.GrantDeliveryViaOrg {
					t.Errorf("grant %s recorded %q, want via_org", g.ID, g.Delivery)
				}
			}
		})
	}
}

// The connection's switch counts only while the connection still mints (minted_pat): a row whose token mode moved
// since the grant's snapshot, or an intent from a bearer run, never carries the opt-in.
func TestADOViaOrg_OptInIsTheSwitchAndMintedTokenMode(t *testing.T) {
	lf := newADOLocalLane(t, adoViaOrgPolicy, false)
	entra := lf.st.site.WorkspaceProviders.Git[0].Entra
	entra.RunnerViaOrg = true
	raw, err := json.Marshal(lf.ado.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	origin, _, err := lf.srv.localADOGrantOrigin(context.Background(), lf.subject, raw)
	if err != nil || !origin.ViaOrgOptIn || origin.ResidentOptIn {
		t.Fatalf("grant origin %+v err %v, want the via_org opt-in alone", origin, err)
	}
	entra.TokenMode = types.ADOTokenModeBearer
	if origin, _, err = lf.srv.localADOGrantOrigin(context.Background(), lf.subject, raw); err != nil || origin.ViaOrgOptIn {
		t.Fatalf("grant origin %+v err %v after the token mode moved, want no opt-in", origin, err)
	}
	for mode, want := range map[types.ADOTokenMode]bool{types.ADOTokenModeMintedPAT: true, types.ADOTokenModeBearer: false} {
		a := adoEntraRun{tokenMode: mode, viaOrgOptIn: true}
		got, err := lf.srv.localADOOrigin(context.Background(), lf.subject, a)
		if err != nil || got.ViaOrgOptIn != want {
			t.Errorf("intent origin for %s: opt-in %v err %v, want %v", mode, got.ViaOrgOptIn, err, want)
		}
	}
}

// countingADOStore counts the reads one delivery judgement makes.
type countingADOStore struct {
	*adoLocalStore
	siteReads, runnerReads int
}

func (s *countingADOStore) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	s.siteReads++
	return s.adoLocalStore.GetSiteConfig(ctx)
}

func (s *countingADOStore) GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error) {
	s.runnerReads++
	return s.adoLocalStore.GetRunner(ctx, id)
}

// localADODelivery judges the mode and its gate from one read of the site configuration and of the runner.
func TestLocalADODelivery_ReadsTheSiteConfigOnce(t *testing.T) {
	lf := newADOLocalLane(t, adoViaOrgPolicy, false)
	lf.st.site.WorkspaceProviders.Git[0].Entra.RunnerViaOrg = true
	cs := &countingADOStore{adoLocalStore: lf.ls}
	lf.srv.cfg.Store = cs
	origin := placement.CredentialOrigin{Class: placement.ClassBrokered, Delivery: placement.ClassADOMintedPAT, ViaOrgOptIn: true}
	delivery, refusal, err := lf.srv.localADODelivery(context.Background(), lf.run, origin)
	if err != nil || refusal != nil || delivery != types.GrantDeliveryViaOrg {
		t.Fatalf("delivery %q refusal %v err %v, want via_org admitted", delivery, refusal, err)
	}
	if cs.siteReads != 1 || cs.runnerReads != 1 {
		t.Fatalf("read the site configuration %d times and the runner %d times, want once each", cs.siteReads, cs.runnerReads)
	}
}

// An own_pat row cannot carry runner_via_org, as it cannot carry runner_resident: only a token Wardyn creates is
// delivered to a runner. The audit rows of both writers name the rows opted in.
func TestADOViaOrg_OwnPATRowCannotOptIn(t *testing.T) {
	row := func(mutate func(*types.ADOEntraConfig)) string {
		raw, _ := json.Marshal(entraRow(func(r *types.GitProvider) { mutate(r.Entra) }))
		return `{"git":[` + string(raw) + `]}`
	}
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, _ := newProvidersHarness(t, fake)
	if w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken, row(func(c *types.ADOEntraConfig) { c.TokenMode, c.RunnerViaOrg = types.ADOTokenModeOwnPAT, true })); w.Code != http.StatusBadRequest ||
		fake.putSeen != nil || !strings.Contains(w.Body.String(), "runner_via_org") {
		t.Fatalf("PUT = %d %s written=%v, want 400 naming runner_via_org and nothing stored", w.Code, w.Body.String(), fake.putSeen != nil)
	}
	fake = &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, audit := newProvidersHarness(t, fake)
	minted := row(func(c *types.ADOEntraConfig) { c.RunnerViaOrg = true })
	if w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken, minted); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s, want the switch accepted", w.Code, w.Body.String())
	}
	var data struct {
		ViaOrg []string `json:"runner_via_org"`
	}
	rows := audit.snapshot()
	i := slices.IndexFunc(rows, func(ev types.AuditEvent) bool { return ev.Action == "workspace_provider.write" })
	if i < 0 || json.Unmarshal(rows[i].Data, &data) != nil || !slices.Equal(data.ViaOrg, []string{"ado"}) {
		t.Fatalf("workspace_provider.write data = %+v", data)
	}
	fake = &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, siteAudit := newProvidersHarness(t, fake)
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"workspace_providers":`+minted+`}`); w.Code != http.StatusOK {
		t.Fatalf("site-config PUT = %d %s", w.Code, w.Body.String())
	}
	data.ViaOrg = nil
	rows = siteAudit.snapshot()
	i = slices.IndexFunc(rows, func(ev types.AuditEvent) bool { return ev.Action == "site_config.write" })
	if i < 0 || json.Unmarshal(rows[i].Data, &data) != nil || !slices.Equal(data.ViaOrg, []string{"ado"}) {
		t.Fatalf("site_config.write data = %+v", data)
	}
}

// adoViaOrgGrantRow is the api_key grant one Azure DevOps host of a via_org run records.
func adoViaOrgGrantRow(runID uuid.UUID, host string) types.CredentialGrant {
	scope, _ := json.Marshal(map[string]any{"host": host, "header": adoEntraInjectHeader, "format": adoInjectFormat(types.ADOTokenModeMintedPAT),
		"secret_name": types.ADOEntraAccessTokenSecret, "require_tls": true})
	return types.CredentialGrant{ID: uuid.New(), RunID: runID, Delivery: types.GrantDeliveryViaOrg, Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope}}
}

// The sealed row keeps the dispatch-time Azure DevOps gate the laptop copy drops, and the run's engine is configured
// with both the gate and the token injection: a token the stored gate does not cover is never injected.
func TestViaOrgEngineConfig_ADOGateRidesTheSealedRow(t *testing.T) {
	const host = "dev.azure.com"
	srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
	ctx := context.Background()
	grant := adoViaOrgGrantRow(run.ID, host)
	ls.dispatchTestStore.grants = []types.CredentialGrant{grant}
	gate := &proxy.ADOGrantConfig{Organization: "acme", Capabilities: []adoscope.Capability{adoscope.CapCodeRead}, Hosts: []string{host, "other.visualstudio.com"}}
	// The per-run CA rides the laptop copy as dispatch authors it: a stored gate loads only beside one.
	caCert, caKey, err := generateRunCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	laptop := runner.ProxyConfig{RunToken: "run-token", ControlPlaneURL: "http://127.0.0.1:9", MITMCACertPEM: string(caCert), MITMCAKeyPEM: string(caKey),
		ViaOrgRoutes: []proxy.ViaOrgRoute{{GrantID: grant.ID, Host: host}}}

	if got := withADOGate(laptop, gate); got.ADOGrant != gate || laptop.ADOGrant != nil {
		t.Fatalf("withADOGate = %+v (laptop copy %+v), want the gate restored on a copy", got.ADOGrant, laptop.ADOGrant)
	}
	if got := withADOGate(runner.ProxyConfig{ADOGrant: &proxy.ADOGrantConfig{Organization: "kept"}}, gate); got.ADOGrant.Organization != "kept" {
		t.Fatal("withADOGate replaced a gate the config already carries")
	}
	if got := withADOGate(laptop, nil); got.ADOGrant != nil {
		t.Fatal("withADOGate invented a gate")
	}

	if err := srv.keepRunProxyConfig(ctx, run.ID, withADOGate(laptop, gate)); err != nil {
		t.Fatal(err)
	}
	cfg, err := srv.viaOrgEngineConfig(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ADOGrant == nil || !slices.Equal(cfg.ADOGrant.Hosts, []string{host}) || cfg.ADOGrant.Organization != "acme" ||
		!slices.Equal(cfg.ADOGrant.Capabilities, gate.Capabilities) {
		t.Fatalf("engine gate = %+v, want the stored gate narrowed to the host the engine serves", cfg.ADOGrant)
	}
	if len(cfg.Injection) != 1 || cfg.Injection[0].SecretName != types.ADOEntraAccessTokenSecret || cfg.Injection[0].Host != host {
		t.Fatalf("engine injections = %+v, want the Azure DevOps token injection", cfg.Injection)
	}

	// A sealed row with no gate (the laptop copy as classified) yields an engine with no token to leak.
	if err := srv.keepRunProxyConfig(ctx, run.ID, laptop); err != nil {
		t.Fatal(err)
	}
	if cfg, err = srv.viaOrgEngineConfig(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if cfg.ADOGrant != nil || len(cfg.Injection) != 0 || len(cfg.ViaOrgRoutes) != 0 {
		t.Fatalf("a row with no gate built gate %+v, injections %+v, routes %+v, want no Azure DevOps token and no route", cfg.ADOGrant, cfg.Injection, cfg.ViaOrgRoutes)
	}
}

// The organisation serves /wardyn/git/ only for an Azure DevOps grant, with the host read from the path, and
// /wardyn/gh/ only for the GitHub grant; everything else must name the grant's destination as its authority.
func TestViaOrgDestinationAdmits(t *testing.T) {
	ado := viaOrgDestination{host: "dev.azure.com", ado: true}
	header := viaOrgDestination{host: "api.corp.example"}
	gh := viaOrgDestination{host: githubHost, broker: true}
	const sandboxHost = "127.0.0.1:3128" // an origin-form request: the runner reports the sandbox's own Host header
	for _, tc := range []struct {
		name      string
		dest      viaOrgDestination
		rest, own string
		want      bool
	}{
		{"ado git, host from the path", ado, "/wardyn/git/dev.azure.com/acme/p/_git/r/info/refs", sandboxHost, true},
		{"ado git, path host in another spelling", ado, "/wardyn/git/DEV.azure.com./acme/p/_git/r/info/refs", sandboxHost, true},
		{"ado git, a path host the grant is not for", ado, "/wardyn/git/other.visualstudio.com/p/_git/r/info/refs", "dev.azure.com", false},
		{"ado git, the authority names the grant but the path does not", ado, "/wardyn/git/evil.example/p/_git/r/info/refs", "dev.azure.com:443", false},
		{"ado git, no path host", ado, "/wardyn/git/", "dev.azure.com", false},
		{"git route on a header grant that is not Azure DevOps", header, "/wardyn/git/api.corp.example/x", "api.corp.example", false},
		{"git route on the GitHub grant", gh, "/wardyn/git/github.com/x", "github.com", false},
		{"gh route on the GitHub grant", gh, "/wardyn/gh/octo/app/info/refs", sandboxHost, true},
		{"gh route on an Azure DevOps grant", ado, "/wardyn/gh/octo/app/info/refs", "dev.azure.com", false},
		{"gh route on a header grant", header, "/wardyn/gh/octo/app/info/refs", "api.corp.example", false},
		{"other path on the GitHub grant", gh, "/repos/octo/app", "github.com", false},
		{"rest on an Azure DevOps grant, authority is the grant's", ado, "/acme/p/_apis/git/repositories", "dev.azure.com:443", true},
		{"rest on an Azure DevOps grant, another authority", ado, "/acme/p/_apis/git/repositories", "evil.example", false},
		{"rest on a header grant", header, "/v1/x", "api.corp.example", true},
	} {
		if got := viaOrgDestinationAdmits(tc.dest, tc.rest, tc.own); got != tc.want {
			t.Errorf("%s: admitted = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !viaOrgBrokered(ado, "/wardyn/git/dev.azure.com/a/b/_git/r/info/refs") || viaOrgBrokered(ado, "/a/_apis/x") ||
		!viaOrgBrokered(gh, "/wardyn/gh/o/r/info/refs") || viaOrgBrokered(header, "/v1/x") {
		t.Error("viaOrgBrokered: the git and gh routes go to the broker, every other request is a header-injected forward")
	}
}

// A via_org engine bounds one request's hold time one minute under the relay's idle cut.
func TestViaOrgHoldCeiling(t *testing.T) {
	if viaOrgHoldCeiling != 540*time.Second {
		t.Fatalf("hold ceiling %s, want 540s", viaOrgHoldCeiling)
	}
	srv, _, _, _ := localDispatchFixture(t, types.PlacementLocal)
	if got := srv.viaOrgEngineOptions(&viaOrgRun{}).HoldCeiling; got != viaOrgHoldCeiling {
		t.Fatalf("an engine is built with hold ceiling %s, want %s", got, viaOrgHoldCeiling)
	}
}

// A run with a via_org Azure DevOps grant is refused once its runner does not say it forwards /wardyn/git/ or the
// organisation's proxy image names an older release.
func TestViaOrgADORunnerReady(t *testing.T) {
	for _, tc := range []struct {
		name  string
		caps  *runnerwire.Caps
		image string
		want  bool
	}{
		{name: "runner and image current", caps: &runnerwire.Caps{ViaOrgADO: true}, image: "ghcr.io/acme/wardyn-proxy:0.10.1", want: true},
		{name: "image names no release", caps: &runnerwire.Caps{ViaOrgADO: true}, image: "ghcr.io/acme/wardyn-proxy@sha256:" + strings.Repeat("a", 64), want: true},
		{name: "runner does not say so", caps: &runnerwire.Caps{}, want: false},
		{name: "runner not connected", want: false},
		{name: "image older", caps: &runnerwire.Caps{ViaOrgADO: true}, image: "ghcr.io/acme/wardyn-proxy:0.10.0", want: false},
		// The floor is 0.10.1's first release candidate: every 0.10.1 candidate passes, a 0.10.0 candidate does not.
		{name: "first 0.10.1 candidate", caps: &runnerwire.Caps{ViaOrgADO: true}, image: "ghcr.io/acme/wardyn-proxy:0.10.1-rc.1", want: true},
		{name: "later 0.10.1 candidate", caps: &runnerwire.Caps{ViaOrgADO: true}, image: "ghcr.io/acme/wardyn-proxy:0.10.1-rc.7", want: true},
		{name: "late 0.10.0 candidate", caps: &runnerwire.Caps{ViaOrgADO: true}, image: "ghcr.io/acme/wardyn-proxy:0.10.0-rc.30", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
			id := uuid.New()
			run.RunnerID = &id
			ls.dispatchTestStore.grants = []types.CredentialGrant{adoViaOrgGrantRow(run.ID, "dev.azure.com")}
			srv.cfg.ProxyImage = tc.image
			if tc.caps != nil {
				seamConnect(t, srv, id, *tc.caps)
			}
			if got := srv.viaOrgADORunnerReady(context.Background(), run); got != tc.want {
				t.Fatalf("ready = %v (hint %q), want %v", got, ls.dispatchTestStore.failureHint, tc.want)
			}
			if !tc.want && !strings.Contains(ls.dispatchTestStore.failureHint, viaOrgADORunnerTooOld) {
				t.Fatalf("failure hint %q, want %q", ls.dispatchTestStore.failureHint, viaOrgADORunnerTooOld)
			}
		})
	}
	// A run with no via_org Azure DevOps grant is never held to the floor.
	srv, _, _, run := localDispatchFixture(t, types.PlacementLocal)
	id := uuid.New()
	run.RunnerID = &id
	if !srv.viaOrgADORunnerReady(context.Background(), run) {
		t.Fatal("a run with no via_org Azure DevOps grant was refused")
	}
}

// A revive stores the organisation's row: it keeps the via_org Azure DevOps gate the revived laptop copy drops.
func TestRevivedStoredConfig_KeepsTheViaOrgADOGate(t *testing.T) {
	ctx := context.Background()
	run := types.AgentRun{ID: uuid.New(), Placement: types.PlacementLocal}
	// The revived copy keeps the run's CA, as a revive of a real row does: the stored row's gate loads only beside it.
	caCert, caKey, err := generateRunCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := runner.BuildProxyConfig(run.ID, runner.ProxyConfig{RunToken: "run-token", ControlPlaneURL: "http://127.0.0.1:9",
		MITMCACertPEM: string(caCert), MITMCAKeyPEM: string(caKey)}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	gate := &proxy.ADOGrantConfig{Organization: "acme", Hosts: []string{"dev.azure.com"}}
	laptop, rerr := encodeRevivedConfig(ctx, cfg)
	if rerr != nil {
		t.Fatal(rerr)
	}
	storedJSON, rerr := revivedStoredConfig(ctx, run, cfg, inspectionCorpus{}, gate, laptop)
	if rerr != nil {
		t.Fatal(rerr)
	}
	stored, err := proxy.LoadConfigBytes(storedJSON)
	if err != nil || stored.ADOGrant == nil || stored.ADOGrant.Organization != "acme" {
		t.Fatalf("stored row = %+v err %v, want the gate kept", stored.ADOGrant, err)
	}
	if cfg.ADOGrant != nil {
		t.Fatal("the revived laptop copy was given the gate")
	}
	remote := types.AgentRun{ID: run.ID, Placement: types.PlacementRemote}
	if out, rerr := revivedStoredConfig(ctx, remote, cfg, inspectionCorpus{}, gate, laptop); rerr != nil || string(out) != string(laptop) {
		t.Fatalf("a remote run's stored config differs from its own: %v", rerr)
	}
}

// A refreshed laptop copy drops the via_org Azure DevOps gate only when it carries no Azure DevOps token injection: a
// copy that still holds the injection keeps its stored gate, so a revive never hands a runner the token ungated.
func TestLaptopADOGate_KeepsTheGateBesideTheTokenInjection(t *testing.T) {
	stored := &proxy.ADOGrantConfig{Organization: "acme", Hosts: []string{"dev.azure.com"}}
	adoInj := proxy.InjectionConfig{GrantID: uuid.New(), InjectionRule: egress.InjectionRule{Host: "dev.azure.com", SecretName: types.ADOEntraAccessTokenSecret}}
	other := proxy.InjectionConfig{GrantID: uuid.New(), InjectionRule: egress.InjectionRule{Host: "api.vendor.example", SecretName: "vendor-key"}}
	for _, tc := range []struct {
		name       string
		injection  []proxy.InjectionConfig
		classified *proxy.ADOGrantConfig // the re-classified copy's gate
		want       *proxy.ADOGrantConfig
	}{
		{name: "resident row, still resident", injection: []proxy.InjectionConfig{adoInj}, classified: stored, want: stored},
		{name: "resident row, now via_org: the injection keeps its gate", injection: []proxy.InjectionConfig{adoInj, other}, want: stored},
		{name: "via_org row, still via_org: the laptop copy carries none", injection: []proxy.InjectionConfig{other}, want: nil},
		{name: "via_org row, now resident: the classified gate", classified: stored, want: stored},
	} {
		cfg := &proxy.Config{Injection: tc.injection, ADOGrant: stored}
		if got := laptopADOGate(cfg, tc.classified); got != tc.want {
			t.Errorf("%s: gate %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
