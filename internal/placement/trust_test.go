// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

var allTrust = []types.RunnerTrustClass{types.RunnerTrustPersonal, types.RunnerTrustManagedDevice, types.RunnerTrustOrgCluster}

// K1 §2: ForTrust takes the trust class's override, else the global row for every trust class, else
// refuse; via_org is the same on every runner; an unknown trust class refuses every other class.
func TestForTrust_Matrix(t *testing.T) {
	resident, refuse := ClassPolicy{Mode: ModeRunnerResident}, ClassPolicy{Mode: ModeRefuse}
	legacy := DeliveryPolicy{Classes: map[string]ClassPolicy{ClassEnvSecret: resident, ClassAPIKey: {Mode: ModeViaOrg}}}
	for _, trust := range allTrust {
		if got := legacy.ForTrust(ClassEnvSecret, trust).Mode; got != ModeRunnerResident {
			t.Errorf("legacy document at %s: env_secret = %s, want the global runner_resident", trust, got)
		}
		if got := legacy.ForTrust(ClassSSHKey, trust).Mode; got != ModeRefuse {
			t.Errorf("legacy document at %s: an absent class = %s, want refuse", trust, got)
		}
	}
	up := DeliveryPolicy{Classes: map[string]ClassPolicy{}, ByTrust: map[types.RunnerTrustClass]TrustPolicy{
		types.RunnerTrustOrgCluster: {Classes: map[string]ClassPolicy{ClassEnvSecret: resident}}}}
	down := DeliveryPolicy{Classes: map[string]ClassPolicy{ClassEnvSecret: resident}, ByTrust: map[types.RunnerTrustClass]TrustPolicy{
		types.RunnerTrustPersonal: {Classes: map[string]ClassPolicy{ClassEnvSecret: refuse}}}}
	for _, tc := range []struct {
		name   string
		p      DeliveryPolicy
		trust  types.RunnerTrustClass
		want   Mode
		reason string
	}{
		{"override up, org_cluster", up, types.RunnerTrustOrgCluster, ModeRunnerResident, "the override admits it"},
		{"override up, managed_device", up, types.RunnerTrustManagedDevice, ModeRefuse, "no override, no global row"},
		{"override up, personal", up, types.RunnerTrustPersonal, ModeRefuse, "no override, no global row"},
		{"override down, personal", down, types.RunnerTrustPersonal, ModeRefuse, "the override refuses it"},
		{"override down, managed_device", down, types.RunnerTrustManagedDevice, ModeRunnerResident, "the global row"},
		{"override down, org_cluster", down, types.RunnerTrustOrgCluster, ModeRunnerResident, "the global row"},
		{"unknown trust", down, "", ModeRefuse, "an unknown class is never guessed"},
		{"invalid trust", down, "robot", ModeRefuse, "an unknown class is never guessed"},
		{"via_org, unknown trust", legacy, "", ModeViaOrg, "via_org is never trust-gated"},
	} {
		class := ClassEnvSecret
		if tc.p.Classes[ClassAPIKey].Mode == ModeViaOrg {
			class = ClassAPIKey
		}
		if got := tc.p.ForTrust(class, tc.trust).Mode; got != tc.want {
			t.Errorf("%s: %s = %s, want %s (%s)", tc.name, class, got, tc.want, tc.reason)
		}
	}
}

func TestDeliveryPolicyValidate_ByTrust(t *testing.T) {
	resident := ClassPolicy{Mode: ModeRunnerResident}
	over := func(trust types.RunnerTrustClass, classes map[string]ClassPolicy) map[types.RunnerTrustClass]TrustPolicy {
		return map[types.RunnerTrustClass]TrustPolicy{trust: {Classes: classes}}
	}
	ok := []DeliveryPolicy{
		{Classes: map[string]ClassPolicy{}, ByTrust: over(types.RunnerTrustOrgCluster, map[string]ClassPolicy{ClassEnvSecret: resident})},
		{Classes: map[string]ClassPolicy{ClassEnvSecret: resident}, ByTrust: over(types.RunnerTrustPersonal, map[string]ClassPolicy{ClassEnvSecret: {Mode: ModeRefuse}})},
		{Classes: map[string]ClassPolicy{}, ByTrust: over(types.RunnerTrustManagedDevice, map[string]ClassPolicy{ClassEnvSecret: {Mode: ModeRunnerResident, RequirePosture: &PostureRequirement{MDMManaged: true}}})},
	}
	for i, p := range ok {
		if err := p.Validate(); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := map[string]DeliveryPolicy{
		"unknown trust class":             {Classes: map[string]ClassPolicy{}, ByTrust: over("robot", map[string]ClassPolicy{ClassEnvSecret: resident})},
		"mode outside the class's bounds": {Classes: map[string]ClassPolicy{}, ByTrust: over(types.RunnerTrustOrgCluster, map[string]ClassPolicy{ClassGitHubToken: resident})},
		"fixed class":                     {Classes: map[string]ClassPolicy{}, ByTrust: over(types.RunnerTrustOrgCluster, map[string]ClassPolicy{ClassCloudSTS: {Mode: ModeRefuse}})},
		"via_org per trust class":         {Classes: map[string]ClassPolicy{}, ByTrust: over(types.RunnerTrustOrgCluster, map[string]ClassPolicy{ClassAPIKey: {Mode: ModeViaOrg}})},
		"override of a via_org class":     {Classes: map[string]ClassPolicy{ClassAPIKey: {Mode: ModeViaOrg}}, ByTrust: over(types.RunnerTrustPersonal, map[string]ClassPolicy{ClassAPIKey: {Mode: ModeRefuse}})},
		"require_posture under org_cluster": {Classes: map[string]ClassPolicy{}, ByTrust: over(types.RunnerTrustOrgCluster,
			map[string]ClassPolicy{ClassEnvSecret: {Mode: ModeRunnerResident, RequirePosture: &PostureRequirement{DiskEncrypted: true}}})},
		"require_posture without runner_resident": {Classes: map[string]ClassPolicy{}, ByTrust: over(types.RunnerTrustPersonal,
			map[string]ClassPolicy{ClassEnvSecret: {Mode: ModeRefuse, RequirePosture: &PostureRequirement{DiskEncrypted: true}}})},
	}
	for name, p := range bad {
		err := p.Validate()
		if !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%s: validated (%v)", name, err)
		}
		if name != "unknown trust class" && err != nil && !strings.Contains(err.Error(), "by_trust.") {
			t.Errorf("%s: %v does not name the override", name, err)
		}
	}
}

// The class comparison at the eligibility door: a runner_resident credential under an org_cluster-only
// override is admitted on an org_cluster runner and refused, naming the class, on every other.
func TestLocalEligibility_ResidentFollowsTheTrustClass(t *testing.T) {
	p := residentPlan(ClassPolicy{Mode: ModeRefuse}, &types.RunnerPosture{OS: "linux"})
	p.Delivery.ByTrust = map[types.RunnerTrustClass]TrustPolicy{types.RunnerTrustOrgCluster: {Classes: map[string]ClassPolicy{ClassEnvSecret: {Mode: ModeRunnerResident}}}}
	for _, trust := range append(allTrust, "") {
		p.Trust = trust
		_, r := LocalEligibility(p)
		if trust == types.RunnerTrustOrgCluster {
			if r != nil {
				t.Errorf("org_cluster: %v, want admitted", r)
			}
			continue
		}
		if r == nil || r.Reason != ReasonPlacementCredential || r.Field != secretEnvPath || !strings.Contains(r.Detail, "trust class "+trustName(trust)) {
			t.Errorf("%q: %+v, want placement_credential naming the trust class", trust, r)
		}
	}
}

func TestTrustClassesByRank(t *testing.T) {
	got := TrustClassesByRank()
	if len(got) != 3 || got[0] != types.RunnerTrustOrgCluster || got[1] != types.RunnerTrustManagedDevice || got[2] != types.RunnerTrustPersonal {
		t.Fatalf("rank order = %v", got)
	}
}
