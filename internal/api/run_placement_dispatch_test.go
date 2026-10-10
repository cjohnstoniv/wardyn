// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type localDispatchStore struct {
	*proxyConfigStore
	store.ComponentStore
	comps []types.RunComponent
}

func (s localDispatchStore) ListRunComponents(context.Context, uuid.UUID) ([]types.RunComponent, error) {
	return s.comps, nil
}

func localDispatchFixture(t *testing.T, p types.Placement, comps ...types.RunComponent) (*Server, localDispatchStore, *recRecorder, types.AgentRun) {
	t.Helper()
	srv, st, audit, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunStarting)
	run.Placement = p
	st.run = run
	ls := localDispatchStore{proxyConfigStore: &proxyConfigStore{dispatchTestStore: st, sealed: map[uuid.UUID][]byte{}}, comps: comps}
	srv.cfg.Store = ls
	srv.cfg.RunConfigKey = make([]byte, kek.DEKSize)
	return srv, ls, audit, run
}

func localOrgNetworkSpec(runID uuid.UUID) runner.SandboxSpec {
	return runner.SandboxSpec{RunID: runID, ProxyConfig: runner.ProxyConfig{
		RunToken: "run-token", UpstreamProxyURL: "https://user:password@parent.corp", TrustedCAPEM: "corporate-ca",
		InternalHosts: []types.InternalHost{{HostSuffix: "corp"}}, LLMUpstreams: map[string]string{"vendor": "corp"},
	}}
}

// After classification the run's stored proxy config is the stripped copy, not
// the config stored before file-secret completion: a revive or proxy restart
// must never read the organisation's upstream proxy, CA or internal hosts.
func TestLocalDispatchStoresTheStrippedProxyConfig(t *testing.T) {
	srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
	ctx := context.Background()
	spec := localOrgNetworkSpec(run.ID)
	if err := srv.keepRunProxyConfig(ctx, run.ID, spec.ProxyConfig); err != nil {
		t.Fatal(err)
	}
	if raw, _ := srv.loadRunProxyConfig(ctx, run.ID); !strings.Contains(string(raw), "parent.corp") {
		t.Fatal("fixture did not store the unstripped config first")
	}
	if !srv.classifyLocalDispatch(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), types.SiteConfig{}, &spec, nil, llmTransport{}, adoEntraRun{}, false) {
		t.Fatal("an all-own plan was refused")
	}
	raw, err := srv.loadRunProxyConfig(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored proxy.Config
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.UpstreamProxyURL != "" || stored.TrustedCAPEM != "" || len(stored.InternalHosts)+len(stored.LLMUpstreams) != 0 {
		t.Fatalf("stored config kept organisation network configuration: %s", raw)
	}
	want, err := runner.BuildProxyConfig(run.ID, spec.ProxyConfig, runner.ProxyListenPort)
	if err != nil || string(raw) != string(want) {
		t.Fatalf("stored config differs from the stripped copy: %v", err)
	}
	if spec.ProxyConfig.UpstreamProxyURL != "" {
		t.Fatal("the dispatched spec was not replaced by the stripped copy")
	}
	_ = ls
}

// A workspace- or source-launched run is a trusted-output operation, which stays
// on the organisation's substrate whatever its task text says: the persisted
// link is the only provenance, and it is the last guard once the entry refusal lifts.
func TestLocalDispatchRefusesTrustedOutputRunsByPersistedLink(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		name string
		link func(*types.AgentRun)
	}{
		{"workspace", func(r *types.AgentRun) { r.WorkspaceID = &id }},
		{"source", func(r *types.AgentRun) { r.SourceID = &id }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, audit, run := localDispatchFixture(t, types.PlacementLocal)
			run.Task = "ordinary task"
			tc.link(&run)
			spec := localOrgNetworkSpec(run.ID)
			if srv.classifyLocalDispatch(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), types.SiteConfig{}, &spec, nil, llmTransport{}, adoEntraRun{}, false) {
				t.Fatal("a trusted-output run was classified for a local runner")
			}
			ev := findAudit(audit.events, run.ID, "run.dispatch", "failure")
			if ev == nil || !strings.Contains(string(ev.Data), string(placement.ReasonPlacementTrustedOutput)) {
				t.Fatalf("no canonical refusal: %s", auditDump(audit.events, run.ID))
			}
		})
	}
}

// Every layer refuses a placement it does not know; classification does not
// pass one through as if it were remote.
func TestUnknownPlacementIsRefusedByEveryLayer(t *testing.T) {
	srv, ls, _, run := localDispatchFixture(t, types.Placement("future"))
	ctx := context.Background()
	spec := localOrgNetworkSpec(run.ID)
	if srv.classifyLocalDispatch(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), types.SiteConfig{}, &spec, nil, llmTransport{}, adoEntraRun{}, false) {
		t.Error("classifyLocalDispatch passed an unknown placement")
	}
	if _, err := srv.createDispatchGrant(ctx, run, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: apiKeyGrantSpec("a.example", "k")}, false); err == nil {
		t.Error("createDispatchGrant wrote for an unknown placement")
	}
	if !srv.unsupportedLocalDispatch(ctx, run) {
		t.Error("unsupportedLocalDispatch let an unknown placement through")
	}
	if ref := srv.reviveOwnerRecheck(ctx, run, &proxy.Config{RunID: run.ID}, types.ActorSystem, "wardynd"); ref == nil || ref.reason != string(placement.ReasonPlacementUnavailable) {
		t.Errorf("revive passed an unknown placement: %v", ref)
	}
	for _, p := range []types.Placement{"", types.PlacementRemote} {
		if !remotePlacement(p) {
			t.Errorf("%q is remote", p)
		}
	}
	if remotePlacement(types.PlacementLocal) || remotePlacement("future") {
		t.Error("local and unknown are not remote")
	}
	_ = ls
}

// Two grants of one kind with different provenance are classified each on its
// own exact scope: the operator's grant never inherits the person's own origin.
func TestLocalPolicyGrantOriginsMatchTheExactScopeNotTheKind(t *testing.T) {
	own := types.GrantSpec{Kind: types.GrantAPIKey, OwnerOnly: true, Scope: mustJSON(map[string]any{"host": "own.example", "secret_name": compOwnSecret})}
	operator := types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": "corp.example", "secret_name": compOperatorSecret})}
	for _, order := range [][]types.GrantSpec{{own, operator}, {operator, own}} {
		f := newComponentFixture(t)
		f.srv.cfg.Store = localPlanTestStore{f.st}
		run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal}
		spec := runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{Policy: types.RunPolicySpec{EligibleGrants: slicesCloneGrants(order)}}}
		for _, g := range order {
			f.st.grants = append(f.st.grants, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: g})
		}
		p, err := f.srv.localResolvedPlan(context.Background(), run, false, types.SiteConfig{}, spec, nil, llmTransport{}, adoEntraRun{})
		if err != nil {
			t.Fatal(err)
		}
		operatorAt := 0
		if order[1].Scope != nil && string(order[1].Scope) == string(operator.Scope) {
			operatorAt = 1
		}
		for i := range order {
			path := "ProxyConfig.Policy.EligibleGrants[" + string(rune('0'+i)) + "]"
			if got := p.Origins[path].Class; (i == operatorAt) != (got == placement.ClassOperator) {
				t.Fatalf("grant %d classified %q", i, got)
			}
		}
		if _, ref := placement.LocalEligibility(p); ref == nil || ref.Field != "ProxyConfig.Policy.EligibleGrants["+string(rune('0'+operatorAt))+"]" {
			t.Fatalf("operator grant not refused at its own index: %v", ref)
		}
	}
}

func slicesCloneGrants(in []types.GrantSpec) []types.GrantSpec {
	return append([]types.GrantSpec(nil), in...)
}

// An ssh_key is own only when its known_hosts secret is also the person's: an
// operator-only known_hosts reference makes the whole grant operator material.
func TestLocalSSHKeyKnownHostsMustAlsoBeInTheOwnersNamespace(t *testing.T) {
	f := newComponentFixture(t)
	f.srv.cfg.Secrets.(*memSecrets).owned[capSub]["own-known-hosts"] = []byte("kh")
	ssh := func(knownHosts string) types.GrantSpec {
		scope := map[string]any{"host": "ssh.example", "key_secret_ref": compOwnSecret}
		if knownHosts != "" {
			scope["known_hosts_secret_ref"] = knownHosts
		}
		return types.GrantSpec{Kind: types.GrantSSHKey, Scope: mustJSON(scope)}
	}
	for _, tc := range []struct {
		knownHosts string
		own        bool
	}{{"", true}, {"own-known-hosts", true}, {compOperatorSecret, false}} {
		got, ref := f.srv.localGrantOrigin(context.Background(), capSub, ssh(tc.knownHosts))
		if ref != nil || (got.Class == placement.ClassOwn) != tc.own || got.OwnNamespace != tc.own {
			t.Errorf("known_hosts %q: %+v (%v), want own=%v", tc.knownHosts, got, ref, tc.own)
		}
	}
}

// One origin for an Azure DevOps lane at admission and at dispatch: own only
// for the owner's own PAT proven in their own namespace.
func TestLocalADOOriginIsOwnOnlyForTheOwnersProvenOwnPAT(t *testing.T) {
	f := newComponentFixture(t)
	const rowID = "ado-row-1"
	f.srv.cfg.Secrets.(*memSecrets).owned[capSub][adoOwnPATSecretName(rowID)] = []byte("pat")
	own := adoEntraRun{rowID: rowID, owner: capSub, tokenMode: types.ADOTokenModeOwnPAT}
	if !adoEntraValidRowID(rowID) {
		t.Skip("fixture row id is not valid")
	}
	ctx := context.Background()
	if got := f.srv.localADOOrigin(ctx, capSub, own); got.Class != placement.ClassOwn || !got.OwnNamespace || !got.OwnerOnly {
		t.Fatalf("own PAT: %+v", got)
	}
	for name, a := range map[string]adoEntraRun{
		"minted token":   {rowID: rowID, owner: capSub},
		"another owner":  {rowID: rowID, owner: "someone-else", tokenMode: types.ADOTokenModeOwnPAT},
		"invalid row id": {rowID: "", owner: capSub, tokenMode: types.ADOTokenModeOwnPAT},
	} {
		if got := f.srv.localADOOrigin(ctx, capSub, a); got.Class != placement.ClassBrokered {
			t.Errorf("%s: %+v", name, got)
		}
	}
	missing := adoEntraRun{rowID: "ado-row-2", owner: capSub, tokenMode: types.ADOTokenModeOwnPAT}
	if got := f.srv.localADOOrigin(ctx, capSub, missing); got.Class != placement.ClassOwn || got.OwnNamespace || got.OwnerOnly {
		t.Errorf("own PAT without a stored secret: %+v", got)
	}
}
