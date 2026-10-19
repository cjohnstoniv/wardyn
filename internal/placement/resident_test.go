// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const secretEnvPath = "SandboxSpec.SecretEnv[TOKEN]"

// residentPlan is an operator env_secret under the given class policy.
func residentPlan(cp ClassPolicy, posture *types.RunnerPosture) LocalPlan {
	return LocalPlan{
		Spec:     runner.SandboxSpec{SecretEnv: map[string]string{"TOKEN": ResidentPlaceholder(uuid.New())}},
		Origins:  map[string]CredentialOrigin{secretEnvPath: {Class: ClassOperator, Delivery: ClassEnvSecret}},
		Delivery: DeliveryPolicy{Classes: map[string]ClassPolicy{ClassEnvSecret: cp}},
		Posture:  posture,
		Trust:    types.RunnerTrustPersonal,
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
	// The detail names every unmet field and the requirement; a posture never judged says why, at launch
	// and on a runner chosen at dispatch or revive that has not reported its posture.
	const requires = " (requires mdm_managed, disk_encrypted, os_min darwin 15.1)"
	const unreported = "posture: no reported posture to judge. At launch no runner is chosen yet, and a runner that has not reported its posture meets no requirement, so a delivery policy with require_posture refuses"
	for _, tc := range []struct {
		name    string
		posture *types.RunnerPosture
		trust   types.RunnerTrustClass
		want    string
	}{
		{"two fields", &types.RunnerPosture{OS: "darwin", OSVersion: "15.2"}, "",
			"mdm_managed, disk_encrypted: the runner's reported posture does not meet env_secret's requirement" + requires},
		{"every field", &types.RunnerPosture{OS: "linux", OSVersion: "24.04"}, "",
			"mdm_managed, disk_encrypted, os_min: the runner's reported posture does not meet env_secret's requirement" + requires},
		{"never reported", nil, "", unreported + requires},
		{"chosen managed runner never reported", nil, types.RunnerTrustManagedDevice, unreported + requires},
	} {
		p := residentPlan(ClassPolicy{Mode: ModeRunnerResident, RequirePosture: req}, tc.posture)
		if tc.trust != "" {
			p.Trust = tc.trust
		}
		_, r := LocalEligibility(p)
		if r == nil || r.Reason != ReasonRunnerPostureUnmet || r.Detail != tc.want {
			t.Errorf("%s: %+v\n want %q", tc.name, r, tc.want)
		}
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
		Trust:             types.RunnerTrustPersonal,
		OwnerKind:         types.RunnerOwnerPerson,
	}
}

// The 2026-10-10 ruling: an Azure DevOps minted token is resident only on a
// connection an admin opted in, even when the class policy says resident; the
// 2026-10-19 ruling (option L) admits an opted-in connection's token, which the
// runner's proxy fetches per request and holds in memory only. Off by default:
// with no policy row, or no opt-in, it refuses.
func TestLocalEligibility_ADOResidentIsOffByDefault(t *testing.T) {
	_, r := LocalEligibility(adoResidentPlan(CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT}))
	if r == nil || r.Reason != ReasonPlacementCredential || r.Field != adoPath || !strings.Contains(r.Detail, "opted in") {
		t.Fatalf("no opt-in: %+v, want the opt-in refusal", r)
	}
	optedIn := CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, ResidentOptIn: true}
	if _, r = LocalEligibility(adoResidentPlan(optedIn)); r != nil {
		t.Fatalf("opted-in connection: %+v, want admitted", r)
	}
	unset := adoResidentPlan(optedIn)
	unset.Delivery = DeliveryPolicy{Classes: map[string]ClassPolicy{}}
	if _, r = LocalEligibility(unset); r == nil || r.Reason != ReasonPlacementCredential || !strings.Contains(r.Detail, "delivery policy: refuse") {
		t.Fatalf("opted-in connection, no policy row: %+v, want the default refusal", r)
	}
}

// B1: an admitted runner_resident value leaves the org only through
// DeliverResident, so the dispatched leaf must already be its placeholder.
func TestLocalEligibility_ResidentValueMustBeItsPlaceholder(t *testing.T) {
	grant := uuid.New()
	cp := ClassPolicy{Mode: ModeRunnerResident}
	inline := residentPlan(cp, nil)
	inline.Spec.SecretEnv = map[string]string{"TOKEN": "an-inline-secret"}
	if _, r := LocalEligibility(inline); r == nil || r.Reason != ReasonPlacementCredential || r.Field != secretEnvPath {
		t.Fatalf("inline env value: %+v, want %s", r, ReasonPlacementCredential)
	}
	const filePath = "SandboxSpec.ManagedFiles[0]"
	file := func(content string) LocalPlan {
		return LocalPlan{
			Spec:     runner.SandboxSpec{ManagedFiles: []runner.ManagedFile{{Path: "/run/secret", AgentOwned: true, Content: []byte(content)}}},
			Origins:  map[string]CredentialOrigin{filePath: {Class: ClassOperator, Delivery: ClassFileSecret}},
			Delivery: DeliveryPolicy{Classes: map[string]ClassPolicy{ClassFileSecret: cp}},
			Trust:    types.RunnerTrustPersonal,
		}
	}
	if _, r := LocalEligibility(file("an-inline-file")); r == nil || r.Reason != ReasonPlacementCredential || r.Field != filePath {
		t.Fatalf("inline file content: %+v, want %s", r, ReasonPlacementCredential)
	}
	if _, r := LocalEligibility(file(ResidentPlaceholder(grant))); r != nil {
		t.Fatalf("placeholder file content: %v", r)
	}
	if _, r := LocalEligibility(residentPlan(cp, nil)); r != nil {
		t.Fatalf("placeholder env value: %v", r)
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

// The grant-record call shape H12's via_org admission (ViaOrgGrant) uses: a
// nil answer must never stand for via_org under a resident policy. A record of
// a class whose resident delivery is not built stays refused under any
// posture; a built class's record is admitted resident and is still never
// via_org.
func TestClassifyCredential_ResidentGrantRecordNeverStandsForViaOrg(t *testing.T) {
	healthy := &types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "linux"}
	for _, class := range []string{ClassAPIKey, ClassEnvSecret} {
		origin := CredentialOrigin{Class: ClassOperator, Delivery: class, GrantKind: types.GrantKind(classGrantKind[class]), Stored: true}
		policy := DeliveryPolicy{Classes: map[string]ClassPolicy{class: {Mode: ModeRunnerResident}}}
		p := LocalPlan{Delivery: policy, Posture: healthy, Trust: types.RunnerTrustPersonal}
		r := classifyCredential(Entries, "ProxyConfig.Policy.EligibleGrants", origin, p, reflect.Value{})
		switch {
		case !residentBuilt[class] && (r == nil || r.Reason != ReasonPlacementCredential):
			t.Errorf("%s resident grant record: %+v, want %s", class, r, ReasonPlacementCredential)
		case residentBuilt[class] && r != nil:
			t.Errorf("%s resident grant record of a built class: %+v, want admitted", class, r)
		}
		if ViaOrgGrant(origin, policy, types.RunnerTrustPersonal) {
			t.Errorf("%s resident grant record answered via_org", class)
		}
	}
}

// ViaOrgGrant answers via_org only when the stored policy delivers the class
// via_org: a nil from classifyCredential alone (here an allow row the table
// does not carry today) never stands for via_org.
func TestViaOrgGrant_RequiresTheViaOrgMode(t *testing.T) {
	origin := CredentialOrigin{Class: ClassOperator, Delivery: ClassAPIKey, GrantKind: types.GrantAPIKey, Stored: true}
	policy := func(m Mode) DeliveryPolicy {
		return DeliveryPolicy{Classes: map[string]ClassPolicy{ClassAPIKey: {Mode: m}}}
	}
	if !ViaOrgGrant(origin, policy(ModeViaOrg), types.RunnerTrustPersonal) {
		t.Fatal("an operator api_key under a via_org policy is not via_org")
	}
	saved := slices.Clone(Table)
	t.Cleanup(func() { Table = saved })
	Table = append([]Entry{{Struct: StructGrantKind, Field: "api_key", Variant: "test allow", Class: ClassOperator, Rule: RuleAllow, Delivery: ClassAPIKey}}, saved...)
	for _, m := range []Mode{ModeRefuse, ModeRunnerResident} {
		if ViaOrgGrant(origin, policy(m), types.RunnerTrustPersonal) {
			t.Errorf("an admitted operator api_key under a %s policy answered via_org", m)
		}
	}
}

var classGrantKind = map[string]string{ClassAPIKey: "api_key", ClassEnvSecret: "env_secret"}

// ResidentAdmits across the design's matrix (§4.2.2, §7.4): class, policy
// mode, the Azure DevOps opt-in, the blast-radius floor and posture. Every
// env_secret case under a resident policy also goes through LocalEligibility,
// which must reach the same answer: the dispatch classify and the org's
// delivery re-check are one function.
func TestResidentAdmits_Matrix(t *testing.T) {
	healthy := &types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "darwin", OSVersion: "15.2"}
	req := &PostureRequirement{MDMManaged: true, DiskEncrypted: true, OSMin: map[string]string{"darwin": "15.1"}}
	env := CredentialOrigin{Class: ClassOperator, Delivery: ClassEnvSecret}
	resident := ClassPolicy{Mode: ModeRunnerResident}
	for _, tc := range []struct {
		name    string
		origin  CredentialOrigin
		cp      ClassPolicy
		floor   types.ConfinementClass
		posture *types.RunnerPosture
		reason  Reason // "" admits
		detail  string
	}{
		{"resident, no requirement, no posture", env, resident, "", nil, "", ""},
		{"resident, requirement met", env, ClassPolicy{Mode: ModeRunnerResident, RequirePosture: req}, "", healthy, "", ""},
		{"policy refuses the class", env, ClassPolicy{Mode: ModeRefuse}, "", healthy, ReasonPlacementCredential, "not delivered runner_resident"},
		{"no policy for the class", env, ClassPolicy{}, "", healthy, ReasonPlacementCredential, "not delivered runner_resident"},
		{"class is never resident", CredentialOrigin{Class: ClassBrokered, Delivery: ClassGitHubToken}, resident, "", healthy, ReasonPlacementCredential, "never runner_resident"},
		{"Azure DevOps without the opt-in", CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT}, resident, "", healthy, ReasonPlacementCredential, "opted in"},
		{"Azure DevOps opted in", CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, ResidentOptIn: true}, resident, "", healthy, "", ""},
		{"CC3 floor", env, resident, types.CC3, healthy, ReasonRunnerPostureUnmet, "confinement_class"},
		{"requirement, no posture reported", env, ClassPolicy{Mode: ModeRunnerResident, RequirePosture: req}, "", nil, ReasonRunnerPostureUnmet, "posture"},
		{"requirement, disk not encrypted", env, ClassPolicy{Mode: ModeRunnerResident, RequirePosture: req},
			"", &types.RunnerPosture{MDMManaged: true, OS: "darwin", OSVersion: "15.2"}, ReasonRunnerPostureUnmet, "disk_encrypted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := residentPlan(ClassPolicy{}, tc.posture)
			p.Delivery = DeliveryPolicy{Classes: map[string]ClassPolicy{tc.origin.Delivery: tc.cp}}
			p.ConfinementFloor = tc.floor
			p.OwnerKind = types.RunnerOwnerPerson
			r := ResidentAdmits(tc.origin, p, "runner_resident."+tc.origin.Delivery)
			switch {
			case tc.reason == "" && r != nil:
				t.Fatalf("refused: %+v", r)
			case tc.reason != "" && (r == nil || r.Reason != tc.reason || !strings.Contains(r.Detail, tc.detail) || r.Field != "runner_resident."+tc.origin.Delivery):
				t.Fatalf("refusal = %+v, want %s naming %q", r, tc.reason, tc.detail)
			}
			if tc.origin.Delivery != ClassEnvSecret || tc.cp.Mode != ModeRunnerResident {
				return // classify reaches the gate only for a class its policy makes resident
			}
			_, lr := LocalEligibility(p)
			if (lr == nil) != (r == nil) || (lr != nil && (lr.Reason != r.Reason || lr.Detail != r.Detail)) {
				t.Fatalf("LocalEligibility = %+v, ResidentAdmits = %+v: the two doors disagree", lr, r)
			}
		})
	}
}
