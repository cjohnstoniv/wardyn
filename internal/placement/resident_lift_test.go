// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The runner_resident lift, one class at a time (design 0.9 §7.4): a grant
// record of a built class is admitted resident under a policy that delivers it
// so, and its placeholder leaf is admitted while an inline value is not. A
// class that is not built keeps the exact refusal, record and leaf alike. An
// Azure DevOps minted token is admitted with its connection's opt-in.
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
	// ado_minted_pat (option L): no value is dispatched, so on an opted-in
	// connection its intent and its injection rule are admitted as they are. No
	// opt-in, a refuse policy, a floor, a trust class the policy does not
	// deliver it to, or an unknown trust class each refuses.
	t.Run(ClassADOMintedPAT, func(t *testing.T) {
		const path = "ProxyConfig.Injection[0]"
		resident := DeliveryPolicy{Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeRunnerResident}}}
		optedIn := CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, GrantKind: types.GrantAPIKey, ResidentOptIn: true}
		spec := runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{Injection: []runner.InjectionGrant{{GrantID: grant,
			Rule: egress.InjectionRule{Host: "dev.azure.com", Header: "Authorization", SecretName: types.ADOEntraAccessTokenSecret, RequireTLS: true}}}}}
		plan := func(o CredentialOrigin, pol DeliveryPolicy, trust types.RunnerTrustClass, floor types.ConfinementClass) LocalPlan {
			return LocalPlan{Spec: spec, Origins: map[string]CredentialOrigin{path: o}, Delivery: pol, Trust: trust, ConfinementFloor: floor,
				CredentialIntents: []CredentialIntent{{Field: "ProxyConfig.ADOGrant[row-1]", Origin: o}}}
		}
		if _, r := LocalEligibility(plan(optedIn, resident, types.RunnerTrustPersonal, "")); r != nil {
			t.Fatalf("opted-in connection: %+v, want admitted", r)
		}
		noOptIn := optedIn
		noOptIn.ResidentOptIn = false
		managedOnly := DeliveryPolicy{Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeRefuse}},
			ByTrust: map[types.RunnerTrustClass]TrustPolicy{types.RunnerTrustManagedDevice: {Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeRunnerResident}}}}}
		for name, p := range map[string]LocalPlan{
			"no opt-in":              plan(noOptIn, resident, types.RunnerTrustPersonal, ""),
			"refuse policy":          plan(optedIn, DeliveryPolicy{Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeRefuse}}}, types.RunnerTrustPersonal, ""),
			"CC3 floor":              plan(optedIn, resident, types.RunnerTrustPersonal, types.CC3),
			"personal, managed-only": plan(optedIn, managedOnly, types.RunnerTrustPersonal, ""),
			"unknown trust class":    plan(optedIn, resident, "", ""),
		} {
			if _, r := LocalEligibility(p); r == nil {
				t.Errorf("%s: admitted, want refused", name)
			}
		}
		if _, r := LocalEligibility(plan(optedIn, managedOnly, types.RunnerTrustManagedDevice, "")); r != nil {
			t.Fatalf("managed_device, managed-only policy: %+v, want admitted", r)
		}
	})
}
