// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// personalJudgement is a local plan judged at personal, every runner's class before 0180.
func personalJudgement(p placement.DeliveryPolicy) localJudgement {
	return localJudgement{policy: p, trust: types.RunnerTrustPersonal}
}

func trustTarget(c types.RunnerTrustClass) poolTarget {
	id := uuid.New()
	return poolTarget{id: id.String(), runnerID: &id, trust: c}
}

// Create judges at the eligible runners' classes: the promotion's runner's class first, then classes with
// room, then the rest, each highest first; it binds only to the admitted classes.
func TestCandidateTrusts_PreferredThenRoomThenRank(t *testing.T) {
	personal, managed := trustTarget(types.RunnerTrustPersonal), trustTarget(types.RunnerTrustManagedDevice)
	remote := poolTarget{id: "alpha", executor: "alpha"}
	got := candidateTrusts([]poolTarget{personal, managed, remote, trustTarget(types.RunnerTrustPersonal)}, nil)
	if !slices.Equal(got, []types.RunnerTrustClass{types.RunnerTrustManagedDevice, types.RunnerTrustPersonal}) {
		t.Errorf("all with room: %v, want [managed_device personal]", got)
	}
	fullManaged := managed
	fullManaged.full = true
	if got := candidateTrusts([]poolTarget{fullManaged, personal}, nil); !slices.Equal(got, []types.RunnerTrustClass{types.RunnerTrustPersonal, types.RunnerTrustManagedDevice}) {
		t.Errorf("the managed_device runner full: %v, want personal (room) first", got)
	}
	if got := candidateTrusts([]poolTarget{managed, personal}, personal.runnerID); !slices.Equal(got, []types.RunnerTrustClass{types.RunnerTrustPersonal, types.RunnerTrustManagedDevice}) {
		t.Errorf("promoted to the personal runner: %v, want personal first", got)
	}
	if got := candidateTrusts([]poolTarget{remote}, nil); len(got) != 3 {
		t.Errorf("no runner among the targets: %v, want every class", got)
	}
}

// Narrowing keeps the admitted classes and the remote-hosted targets, and recomputes whether every runner
// left is full.
func TestRunPlacementNarrowToTrust_RecomputesRoom(t *testing.T) {
	personal, managed := trustTarget(types.RunnerTrustPersonal), trustTarget(types.RunnerTrustManagedDevice)
	managed.full = true
	limit := 2
	capped := &types.RunnerPoolLimits{MaxConcurrentRuns: &limit}
	p := runPlacement{pooled: true, row: types.RunnerPool{Limits: capped}, eligible: []poolTarget{personal, managed}}
	p.narrowToTrust(types.RunnerTrustManagedDevice)
	if len(p.eligible) != 1 || p.eligible[0].id != managed.id || !p.runnersFull {
		t.Errorf("narrowed to the full managed_device runner: %+v runnersFull=%v, want it alone and full", p.eligible, p.runnersFull)
	}
	p = runPlacement{pooled: true, row: types.RunnerPool{Limits: capped}, eligible: []poolTarget{personal, managed}, runnersFull: true}
	p.narrowToTrust(types.RunnerTrustPersonal)
	if len(p.eligible) != 1 || p.eligible[0].id != personal.id || p.runnersFull {
		t.Errorf("narrowed to the personal runner with room: %+v runnersFull=%v, want it alone with room", p.eligible, p.runnersFull)
	}
	p = runPlacement{pooled: true, eligible: []poolTarget{personal, managed}, waitsForRoom: true}
	p.narrowToTrust(types.RunnerTrustPersonal)
	if p.waitsForRoom {
		t.Error("a narrowed set with room still waits for room")
	}
}

func TestJudgeAtTrust_FirstAdmittingClassElseTheFirstRefusal(t *testing.T) {
	p := placement.LocalPlan{
		Spec:    runner.SandboxSpec{SecretEnv: map[string]string{"TOKEN": placement.ResidentPlaceholder(uuid.New())}},
		Origins: map[string]placement.CredentialOrigin{"SandboxSpec.SecretEnv[TOKEN]": {Class: placement.ClassOperator, Delivery: placement.ClassEnvSecret}},
		Delivery: placement.DeliveryPolicy{Classes: map[string]placement.ClassPolicy{}, ByTrust: map[types.RunnerTrustClass]placement.TrustPolicy{
			types.RunnerTrustManagedDevice: {Classes: map[string]placement.ClassPolicy{placement.ClassEnvSecret: {Mode: placement.ModeRunnerResident}}}}},
		Posture: &types.RunnerPosture{OS: "linux"},
	}
	if c, ref := judgeAtTrust(p, placement.TrustClassesByRank()); ref != nil || c != types.RunnerTrustManagedDevice {
		t.Errorf("every class: %q %v, want managed_device", c, ref)
	}
	c, ref := judgeAtTrust(p, []types.RunnerTrustClass{types.RunnerTrustPersonal})
	if c != "" || ref == nil || ref.Reason != placement.ReasonPlacementCredential || !strings.Contains(ref.Detail, "trust class personal") {
		t.Errorf("personal only: %q %+v, want refused naming personal", c, ref)
	}
}

// L1: a run waiting on remote-hosted runners is judged for the person's own at the classes a person's
// runner can hold, never at org_cluster: a plan only org_cluster admits is refused there.
func TestWaitingLocalTrusts_NeverOrgCluster(t *testing.T) {
	p := placement.LocalPlan{
		Spec:    runner.SandboxSpec{SecretEnv: map[string]string{"TOKEN": placement.ResidentPlaceholder(uuid.New())}},
		Origins: map[string]placement.CredentialOrigin{"SandboxSpec.SecretEnv[TOKEN]": {Class: placement.ClassOperator, Delivery: placement.ClassEnvSecret}},
		Delivery: placement.DeliveryPolicy{Classes: map[string]placement.ClassPolicy{}, ByTrust: map[types.RunnerTrustClass]placement.TrustPolicy{
			types.RunnerTrustOrgCluster: {Classes: map[string]placement.ClassPolicy{placement.ClassEnvSecret: {Mode: placement.ModeRunnerResident}}}}},
		Posture: &types.RunnerPosture{OS: "linux"},
	}
	if c, ref := judgeAtTrust(p, waitingLocalTrusts()); c != "" || ref == nil {
		t.Errorf("a plan only org_cluster admits, judged for the person's own runners: %q %v, want refused", c, ref)
	}
}

// Dispatch's trust door: a grant recorded via_org or runner_resident must still be delivered that way at
// the bound runner's class; an own grant is the person's own and is not judged by the policy.
func TestGrantModesRefusal(t *testing.T) {
	classOf := func(types.CredentialGrant) (string, bool) { return placement.ClassAPIKey, true }
	viaOrg := types.CredentialGrant{ID: uuid.New(), Delivery: types.GrantDeliveryViaOrg}
	resident := types.CredentialGrant{ID: uuid.New(), Delivery: types.GrantDeliveryResident}
	own := types.CredentialGrant{ID: uuid.New(), Delivery: types.GrantDeliveryOwn}
	plan := func(mode placement.Mode, trust types.RunnerTrustClass) placement.LocalPlan {
		return placement.LocalPlan{Trust: trust, Delivery: placement.DeliveryPolicy{Classes: map[string]placement.ClassPolicy{placement.ClassAPIKey: {Mode: mode}}}}
	}
	if r := grantModesRefusal(plan(placement.ModeViaOrg, types.RunnerTrustPersonal), []types.CredentialGrant{viaOrg, own}, classOf); r != nil {
		t.Errorf("via_org still via_org: %v", r)
	}
	if r := grantModesRefusal(plan(placement.ModeRefuse, types.RunnerTrustPersonal), []types.CredentialGrant{viaOrg}, classOf); r == nil || r.Reason != placement.ReasonPlacementCredential {
		t.Errorf("via_org now refused: %+v, want placement_credential", r)
	}
	if r := grantModesRefusal(plan(placement.ModeRunnerResident, types.RunnerTrustOrgCluster), []types.CredentialGrant{resident}, classOf); r != nil {
		t.Errorf("runner_resident still resident: %v", r)
	}
	if r := grantModesRefusal(plan(placement.ModeRunnerResident, ""), []types.CredentialGrant{resident}, classOf); r == nil || !strings.Contains(r.Detail, "trust class") {
		t.Errorf("runner_resident at an unknown class: %+v, want refused", r)
	}
	if r := grantModesRefusal(plan(placement.ModeRefuse, types.RunnerTrustPersonal), []types.CredentialGrant{own}, classOf); r != nil {
		t.Errorf("an own grant judged by the policy: %v", r)
	}
	unknown := func(types.CredentialGrant) (string, bool) { return "", false }
	if r := grantModesRefusal(plan(placement.ModeViaOrg, types.RunnerTrustPersonal), []types.CredentialGrant{viaOrg}, unknown); r == nil {
		t.Error("a grant whose class cannot be resolved passed")
	}
}

// The dispatch and revive doors read the bound runner's stored class: a run admitted at org_cluster whose
// runner is downgraded is refused at dispatch for a credential only org_cluster takes, and at revive with
// runner_trust_changed; a run with no recorded class is refused at revive alike.
func TestLocalTrust_DispatchAndReviveDoors(t *testing.T) {
	f := newComponentFixture(t)
	runnerID := uuid.New()
	run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal, RunnerID: &runnerID}
	env := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"TOKEN","secret_name":"ci-token"}`)}}
	st := residentFactsStore{localPlanTestStore: localPlanTestStore{f.st},
		policy:  json.RawMessage(`{"classes":{},"by_trust":{"org_cluster":{"classes":{"env_secret":{"mode":"runner_resident"}}}}}`),
		posture: types.RunnerPosture{OS: "linux", Evidence: types.PostureEvidenceOSProbe},
		trust:   types.RunnerTrustOrgCluster, recorded: types.RunnerTrustOrgCluster}
	f.srv.cfg.Store = st
	dispatch := func() *placement.Refusal {
		t.Helper()
		f.st.grants = []types.CredentialGrant{env}
		spec := runner.SandboxSpec{RunID: run.ID, SecretEnv: map[string]string{"TOKEN": placement.ResidentPlaceholder(env.ID)}}
		p, err := f.srv.localResolvedPlan(context.Background(), run, false, types.SiteConfig{}, spec, nil, llmTransport{})
		if err != nil {
			t.Fatal(err)
		}
		if r, err := f.srv.dispatchTrustRefusal(context.Background(), run, p); err != nil || r != nil {
			return r
		}
		_, r := placement.LocalEligibility(p)
		return r
	}
	revive := func() *placement.Refusal {
		t.Helper()
		f.st.grants = nil
		cfg := proxy.Config{RunID: run.ID}
		err := f.srv.refreshLocalDeploymentConfig(context.Background(), run, types.SiteConfig{}, &cfg)
		r, _ := err.(*placement.Refusal)
		if err != nil && r == nil {
			t.Fatalf("revive: %v", err)
		}
		return r
	}
	if r := dispatch(); r != nil {
		t.Fatalf("dispatch at org_cluster: %v", r)
	}
	if r := revive(); r != nil {
		t.Fatalf("revive at the admitted class: %v", r)
	}
	st.trust = types.RunnerTrustPersonal // the org runner cannot be personal; a person runner admitted at managed_device can
	f.srv.cfg.Store = st
	if r := dispatch(); r == nil || r.Reason != placement.ReasonPlacementCredential || !strings.Contains(r.Detail, "trust class personal") {
		t.Errorf("dispatch after a downgrade: %+v, want placement_credential naming personal", r)
	}
	if r := revive(); r == nil || r.Reason != placement.ReasonRunnerTrustChanged {
		t.Errorf("revive after a downgrade: %+v, want runner_trust_changed", r)
	}
	st.trust, st.unrecorded = types.RunnerTrustOrgCluster, true
	f.srv.cfg.Store = st
	if r := revive(); r == nil || r.Reason != placement.ReasonRunnerTrustChanged {
		t.Errorf("revive with no recorded class: %+v, want runner_trust_changed", r)
	}
}
