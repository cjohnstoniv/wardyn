// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The runner_resident lift, one class at a time (design 0.9 §7.4): a grant
// record of a built class is admitted resident under a policy that delivers it
// so, and its placeholder leaf is admitted while an inline value is not. A
// class that is not built keeps the exact refusal, record and leaf alike.
func TestResidentLift_PerClass(t *testing.T) {
	kinds := map[string]types.GrantKind{ClassEnvSecret: types.GrantEnvSecret, ClassFileSecret: types.GrantFileSecret,
		ClassSSHKey: types.GrantSSHKey, ClassGitPATHelper: types.GrantGitPAT, ClassAPIKey: types.GrantAPIKey}
	grant := uuid.New()
	for _, tc := range []struct {
		class string
		built bool
		// leaf is the class's dispatched placeholder and an inline value at its path.
		path         string
		leaf, inline runner.SandboxSpec
	}{
		{ClassEnvSecret, true, "SandboxSpec.SecretEnv[TOKEN]",
			runner.SandboxSpec{SecretEnv: map[string]string{"TOKEN": ResidentPlaceholder(grant)}},
			runner.SandboxSpec{SecretEnv: map[string]string{"TOKEN": "inline"}}},
		{ClassFileSecret, true, "SandboxSpec.ManagedFiles[0]",
			runner.SandboxSpec{ManagedFiles: []runner.ManagedFile{{Path: "/run/s", AgentOwned: true, Content: []byte(ResidentPlaceholder(grant))}}},
			runner.SandboxSpec{ManagedFiles: []runner.ManagedFile{{Path: "/run/s", AgentOwned: true, Content: []byte("inline")}}}},
		{ClassSSHKey, true, "ProxyConfig.ResidentMints[0]",
			runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{ResidentMints: []proxy.ResidentMint{{GrantID: grant, Kind: types.GrantSSHKey, Token: ResidentPlaceholder(grant), ExpiresAt: time.Now().Add(time.Hour)}}}},
			runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{ResidentMints: []proxy.ResidentMint{{GrantID: grant, Kind: types.GrantSSHKey, Token: "inline", ExpiresAt: time.Now().Add(time.Hour)}}}}},
		{ClassGitPATHelper, true, "ProxyConfig.ResidentMints[0]",
			runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{ResidentMints: []proxy.ResidentMint{{GrantID: grant, Kind: types.GrantGitPAT, Token: ResidentPlaceholder(grant), ExpiresAt: time.Now().Add(time.Hour)}}}},
			runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{ResidentMints: []proxy.ResidentMint{{GrantID: grant, Kind: types.GrantGitPAT, Token: ResidentPlaceholder(grant)}}}}},
		{ClassAPIKey, false, "", runner.SandboxSpec{}, runner.SandboxSpec{}},
	} {
		t.Run(tc.class, func(t *testing.T) {
			policy := DeliveryPolicy{Classes: map[string]ClassPolicy{tc.class: {Mode: ModeRunnerResident}}}
			origin := CredentialOrigin{Class: ClassOperator, Delivery: tc.class, GrantKind: kinds[tc.class], Stored: true}
			record := classifyCredential(Entries, "ProxyConfig.Policy.EligibleGrants", origin, LocalPlan{Delivery: policy, Trust: types.RunnerTrustPersonal}, reflect.Value{})
			resident, ref := ResidentGrant(origin, policy, types.RunnerTrustPersonal, "")
			if !tc.built {
				if record == nil || record.Detail != "runner_resident delivery of this credential is not built yet" || resident || ref == nil {
					t.Fatalf("an unbuilt class: record %+v, resident %v %+v, want the exact refusal", record, resident, ref)
				}
				return
			}
			if record != nil || !resident || ref != nil {
				t.Fatalf("a built class: record %+v, resident %v %+v, want admitted", record, resident, ref)
			}
			if ViaOrgGrant(origin, policy, types.RunnerTrustPersonal) {
				t.Fatal("a resident-mode class was answered via_org")
			}
			if ok, ref := ResidentGrant(origin, DeliveryPolicy{Classes: map[string]ClassPolicy{tc.class: {Mode: ModeRefuse}}}, types.RunnerTrustPersonal, ""); ok || ref != nil {
				t.Fatalf("a refused class: resident %v %+v, want not resident", ok, ref)
			}
			if ok, ref := ResidentGrant(origin, policy, types.RunnerTrustPersonal, types.CC3); ok || ref == nil || ref.Reason != ReasonRunnerPostureUnmet {
				t.Fatalf("a CC3-floor run: resident %v %+v, want %s at create", ok, ref, ReasonRunnerPostureUnmet)
			}
			plan := func(spec runner.SandboxSpec) LocalPlan {
				return LocalPlan{Spec: spec, Origins: map[string]CredentialOrigin{tc.path: origin}, Delivery: policy, Trust: types.RunnerTrustPersonal}
			}
			if _, r := LocalEligibility(plan(tc.leaf)); r != nil {
				t.Fatalf("the placeholder leaf: %+v", r)
			}
			if _, r := LocalEligibility(plan(tc.inline)); r == nil || r.Reason != ReasonPlacementCredential || r.Field != tc.path {
				t.Fatalf("an inline leaf (or a mint with no expiry): %+v, want %s at %s", r, ReasonPlacementCredential, tc.path)
			}
		})
	}
}
