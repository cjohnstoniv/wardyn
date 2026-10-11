// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const secretEnvPath = "SandboxSpec.SecretEnv[TOKEN]"

// residentPlan is an operator env_secret under the given class policy.
func residentPlan(cp ClassPolicy, posture *types.RunnerPosture) LocalPlan {
	return LocalPlan{
		Spec:     runner.SandboxSpec{SecretEnv: map[string]string{"TOKEN": "value"}},
		Origins:  map[string]CredentialOrigin{secretEnvPath: {Class: ClassOperator, Delivery: ClassEnvSecret}},
		Delivery: DeliveryPolicy{Classes: map[string]ClassPolicy{ClassEnvSecret: cp}},
		Posture:  posture,
	}
}

func TestLocalEligibility_ResidentNeedsPolicyAndPosture(t *testing.T) {
	healthy := &types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "darwin", OSVersion: "15.2"}
	req := &PostureRequirement{MDMManaged: true, DiskEncrypted: true, OSMin: map[string]string{"darwin": "15.1"}}
	if _, r := LocalEligibility(residentPlan(ClassPolicy{Mode: ModeRunnerResident, RequirePosture: req}, healthy)); r != nil {
		t.Fatalf("resident class on a runner meeting its posture: %v", r)
	}
	if _, r := LocalEligibility(residentPlan(ClassPolicy{Mode: ModeRefuse}, healthy)); r == nil || r.Reason != ReasonPlacementCredential {
		t.Fatalf("no resident policy: %v, want %s", r, ReasonPlacementCredential)
	}
	for _, tc := range []struct {
		name    string
		posture *types.RunnerPosture
		field   string
	}{
		{"never reported", nil, "posture"},
		{"not managed", &types.RunnerPosture{DiskEncrypted: true, OS: "darwin", OSVersion: "15.2"}, "mdm_managed"},
		{"not encrypted", &types.RunnerPosture{MDMManaged: true, OS: "darwin", OSVersion: "15.2"}, "disk_encrypted"},
		{"old OS", &types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "darwin", OSVersion: "15.0.9"}, "os_min"},
		{"unlisted OS", &types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "linux", OSVersion: "24.04"}, "os_min"},
		{"unparseable version", &types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "darwin", OSVersion: "beta"}, "os_min"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, r := LocalEligibility(residentPlan(ClassPolicy{Mode: ModeRunnerResident, RequirePosture: req}, tc.posture))
			if r == nil || r.Reason != ReasonRunnerPostureUnmet || r.Field != secretEnvPath || r.Detail[:len(tc.field)] != tc.field {
				t.Fatalf("refusal = %+v, want %s naming %s", r, ReasonRunnerPostureUnmet, tc.field)
			}
		})
	}
}

// Q7: for a runner_resident credential a runner-asserted CC3 never satisfies
// the blast-radius floor, however healthy the reported posture.
func TestLocalEligibility_ResidentNeverMeetsTheCC3Floor(t *testing.T) {
	p := residentPlan(ClassPolicy{Mode: ModeRunnerResident}, &types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "linux"})
	if _, r := LocalEligibility(p); r != nil {
		t.Fatalf("no floor: %v", r)
	}
	p.ConfinementFloor = types.CC3
	if _, r := LocalEligibility(p); r == nil || r.Reason != ReasonRunnerPostureUnmet || r.Field != secretEnvPath {
		t.Fatalf("CC3 floor: %+v, want %s", r, ReasonRunnerPostureUnmet)
	}
}

const adoPath = "ProxyConfig.ADOGrant[row-1]"

func adoResidentPlan(origin CredentialOrigin) LocalPlan {
	return LocalPlan{
		CredentialIntents: []CredentialIntent{{Field: adoPath, Origin: origin}},
		Delivery:          DeliveryPolicy{Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeRunnerResident}}},
		Posture:           &types.RunnerPosture{OS: "linux"},
	}
}

// The 2026-10-10 ruling: an Azure DevOps minted token is resident only on a
// connection an admin opted in, even when the class policy says resident.
func TestLocalEligibility_ADOResidentIsOffByDefault(t *testing.T) {
	if _, r := LocalEligibility(adoResidentPlan(CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT})); r == nil || r.Reason != ReasonPlacementCredential || r.Field != adoPath {
		t.Fatalf("no opt-in: %+v, want %s", r, ReasonPlacementCredential)
	}
	if _, r := LocalEligibility(adoResidentPlan(CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, ResidentOptIn: true})); r != nil {
		t.Fatalf("opted-in connection: %v", r)
	}
}

func TestLocalEligibility_GitHubTokenIsNeverResident(t *testing.T) {
	if err := (DeliveryPolicy{Classes: map[string]ClassPolicy{ClassGitHubToken: {Mode: ModeRunnerResident}}}).Validate(); err == nil {
		t.Fatal("a github_token runner_resident policy validated")
	}
	// A stored document that somehow says resident is still refused by the row's rule.
	p := LocalPlan{
		CredentialIntents: []CredentialIntent{{Field: "ProxyConfig.GitGrants[org/repo]", Origin: CredentialOrigin{Class: ClassBrokered, Delivery: ClassGitHubToken, ResidentOptIn: true}}},
		Delivery:          DeliveryPolicy{Classes: map[string]ClassPolicy{ClassGitHubToken: {Mode: ModeRunnerResident}}},
		Posture:           &types.RunnerPosture{MDMManaged: true, DiskEncrypted: true},
	}
	if _, r := LocalEligibility(p); r == nil || r.Reason != ReasonPlacementCredential {
		t.Fatalf("github_token resident: %+v, want %s", r, ReasonPlacementCredential)
	}
}
