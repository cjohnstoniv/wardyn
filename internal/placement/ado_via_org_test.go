// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const adoHost = "dev.azure.com"

var (
	viaOrgADOPolicy = DeliveryPolicy{Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeViaOrg}}}
	adoOrigin       = CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, GrantKind: types.GrantAPIKey}
	adoViaOrgOrigin = CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, GrantKind: types.GrantAPIKey, ViaOrgOptIn: true}
)

// viaOrgADOPlan is a run with an Azure DevOps lane's injection and gate, its origins stamped, on a person's own runner.
func viaOrgADOPlan(origin CredentialOrigin, policy DeliveryPolicy) (LocalPlan, uuid.UUID) {
	grant := uuid.New()
	spec := runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{
		Injection: []runner.InjectionGrant{{GrantID: grant, Rule: egress.InjectionRule{Host: adoHost, Header: "Authorization", SecretName: types.ADOEntraAccessTokenSecret}}},
		ADOGrant:  &proxy.ADOGrantConfig{Organization: "acme", Hosts: []string{adoHost}},
		MITMHosts: []string{adoHost},
	}}
	return LocalPlan{
		Spec: spec, Delivery: policy, Trust: types.RunnerTrustPersonal, OwnerKind: types.RunnerOwnerPerson,
		Posture: &types.RunnerPosture{OS: "linux"},
		Origins: map[string]CredentialOrigin{"ProxyConfig.Injection[0]": origin, "ProxyConfig.ADOGrant": origin},
	}, grant
}

// Value-list rows 2, 3, 5, 6, 7, 8, 11 and 12 as one table: LocalEligibility is the planner's door.
func TestLocalEligibility_ADOViaOrgMatrix(t *testing.T) {
	requirePosture := DeliveryPolicy{Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeViaOrg, RequirePosture: &PostureRequirement{DiskEncrypted: true}}}}
	for _, tc := range []struct {
		name   string
		origin CredentialOrigin
		policy DeliveryPolicy
		edit   func(*LocalPlan)
		reason Reason
		detail string
	}{
		{name: "2 person's runner, switch on", origin: adoViaOrgOrigin, policy: viaOrgADOPolicy},
		{name: "2 person's runner, switch and runner_resident on", origin: CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, ViaOrgOptIn: true, ResidentOptIn: true}, policy: viaOrgADOPolicy},
		{name: "3 org_cluster runner, switch on", origin: adoViaOrgOrigin, policy: viaOrgADOPolicy,
			edit: func(p *LocalPlan) { p.Trust, p.OwnerKind = types.RunnerTrustOrgCluster, types.RunnerOwnerOrg }},
		{name: "5 switch off", origin: adoOrigin, policy: viaOrgADOPolicy, reason: ReasonPlacementCredential, detail: "does not allow its tokens on runners via the organisation"},
		{name: "5 switch off, runner_resident on", origin: CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, ResidentOptIn: true}, policy: viaOrgADOPolicy,
			reason: ReasonPlacementCredential, detail: "does not allow its tokens on runners via the organisation"},
		{name: "6 unknown trust class", origin: adoViaOrgOrigin, policy: viaOrgADOPolicy, edit: func(p *LocalPlan) { p.Trust = "" },
			reason: ReasonPlacementCredential, detail: "never delivered to a runner of unknown trust class"},
		{name: "7 confinement floor", origin: adoViaOrgOrigin, policy: viaOrgADOPolicy, edit: func(p *LocalPlan) { p.ConfinementFloor = types.CC3 },
			reason: ReasonRunnerPostureUnmet, detail: "floor"},
		{name: "8 require_posture met", origin: adoViaOrgOrigin, policy: requirePosture, edit: func(p *LocalPlan) { p.Posture = &types.RunnerPosture{DiskEncrypted: true} }},
		{name: "8 require_posture, runner reports none (a Kubernetes runner)", origin: adoViaOrgOrigin, policy: requirePosture, edit: func(p *LocalPlan) { p.Posture = nil },
			reason: ReasonRunnerPostureUnmet, detail: "no reported posture"},
		{name: "8 require_posture not met", origin: adoViaOrgOrigin, policy: requirePosture,
			reason: ReasonRunnerPostureUnmet, detail: "disk_encrypted"},
		{name: "11 bearer or own_pat leaves the switch off", origin: adoOrigin, policy: viaOrgADOPolicy,
			reason: ReasonPlacementCredential, detail: "or its token mode is not minted_pat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := viaOrgADOPlan(tc.origin, tc.policy)
			if tc.edit != nil {
				tc.edit(&p)
			}
			_, r := LocalEligibility(p)
			if tc.reason == "" {
				if r != nil {
					t.Fatalf("refused: %+v, want admitted", r)
				}
				return
			}
			if r == nil || r.Reason != tc.reason || !strings.Contains(r.Detail, tc.detail) {
				t.Fatalf("refusal %+v, want %s containing %q", r, tc.reason, tc.detail)
			}
		})
	}
}

// Row 4 and 10: resident is unchanged, an org runner never holds the token and a person's runner with the opt-in does.
func TestLocalEligibility_ADOResidentUnchangedByViaOrg(t *testing.T) {
	resident := DeliveryPolicy{Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeRunnerResident}}}
	optIn := CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, ResidentOptIn: true}
	p, _ := viaOrgADOPlan(optIn, resident)
	if _, r := LocalEligibility(p); r != nil {
		t.Fatalf("row 10, person's runner: %+v, want resident admitted", r)
	}
	p.Trust, p.OwnerKind = types.RunnerTrustOrgCluster, types.RunnerOwnerOrg
	if _, r := LocalEligibility(p); r == nil || !strings.Contains(r.Detail, "runner_resident only on a person's own runner") {
		t.Fatalf("row 4, org runner: %+v, want the person-owned refusal", r)
	}
	// A via_org opt-in alone does not make a resident row deliver.
	p, _ = viaOrgADOPlan(CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, ViaOrgOptIn: true}, resident)
	if _, r := LocalEligibility(p); r == nil || !strings.Contains(r.Detail, "opted in to runner_resident") {
		t.Fatalf("resident with only the via_org switch: %+v, want the runner_resident opt-in refusal", r)
	}
}

// Rows 1, 8 and 9 at the policy's doors: Validate, CheckBuildable.
func TestDeliveryPolicy_ADOViaOrg(t *testing.T) {
	var none DeliveryPolicy
	if err := viaOrgADOPolicy.Validate(); err != nil {
		t.Errorf("row 1 Validate: %v", err)
	}
	if err := viaOrgADOPolicy.CheckBuildable(none); err != nil {
		t.Errorf("row 1 CheckBuildable: %v, want accepted", err)
	}
	withPosture := DeliveryPolicy{Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeViaOrg, RequirePosture: &PostureRequirement{MDMManaged: true}}}}
	if err := withPosture.Validate(); err != nil {
		t.Errorf("row 8 Validate: %v, want accepted", err)
	}
	if err := withPosture.CheckBuildable(none); err != nil {
		t.Errorf("row 8 CheckBuildable: %v", err)
	}
	// require_posture stays refused for a via_org row of any other class, and for a mode other than resident or ADO via_org.
	for name, p := range map[string]DeliveryPolicy{
		"api_key via_org": {Classes: map[string]ClassPolicy{ClassAPIKey: {Mode: ModeViaOrg, RequirePosture: &PostureRequirement{MDMManaged: true}}}},
		"ado refuse":      {Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeRefuse, RequirePosture: &PostureRequirement{MDMManaged: true}}}},
	} {
		if err := p.Validate(); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%s: %v, want ErrInvalidPolicy", name, err)
		}
	}
	// Row 9: never per trust class.
	for _, trust := range []types.RunnerTrustClass{types.RunnerTrustPersonal, types.RunnerTrustOrgCluster} {
		byTrust := DeliveryPolicy{ByTrust: map[types.RunnerTrustClass]TrustPolicy{trust: {Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeViaOrg}}}}}
		if err := byTrust.Validate(); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("row 9 by_trust.%s: %v, want ErrInvalidPolicy", trust, err)
		}
	}
	override := DeliveryPolicy{
		Classes: viaOrgADOPolicy.Classes,
		ByTrust: map[types.RunnerTrustClass]TrustPolicy{types.RunnerTrustPersonal: {Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeRunnerResident}}}},
	}
	if err := override.Validate(); !errors.Is(err, ErrInvalidPolicy) {
		t.Errorf("row 9 override of a global via_org row: %v, want ErrInvalidPolicy", err)
	}
}

// Row 21: an ADO origin is never an eligible_grants entry delivered via_org, even on a runner the plan would admit.
func TestViaOrgGrant_ADOOriginIsNeverAnEligibleGrant(t *testing.T) {
	for _, trust := range []types.RunnerTrustClass{"", types.RunnerTrustPersonal, types.RunnerTrustOrgCluster} {
		for _, o := range []CredentialOrigin{adoOrigin, adoViaOrgOrigin} {
			if ViaOrgGrant(o, viaOrgADOPolicy, trust) {
				t.Errorf("trust %q origin %+v: ViaOrgGrant true", trust, o)
			}
		}
	}
}

// The laptop copy of a via_org ADO plan carries a route per host and no gate or token rule; a resident plan is unchanged.
func TestLocalCopy_ADOViaOrgCarriesRoutesOnly(t *testing.T) {
	p, grant := viaOrgADOPlan(adoViaOrgOrigin, viaOrgADOPolicy)
	out, r := LocalEligibility(p)
	if r != nil {
		t.Fatalf("refused: %+v", r)
	}
	pc := out.ProxyConfig
	if pc.ADOGrant != nil {
		t.Errorf("laptop copy keeps the gate: %+v", pc.ADOGrant)
	}
	if slices.ContainsFunc(pc.Injection, func(in runner.InjectionGrant) bool { return in.Rule.SecretName == types.ADOEntraAccessTokenSecret }) {
		t.Errorf("laptop copy keeps the token injection: %+v", pc.Injection)
	}
	if want := []proxy.ViaOrgRoute{{GrantID: grant, Host: adoHost}}; !slices.Equal(pc.ViaOrgRoutes, want) {
		t.Errorf("routes = %+v, want %+v", pc.ViaOrgRoutes, want)
	}
	if !slices.Contains(pc.MITMHosts, adoHost) {
		t.Errorf("MITMHosts = %v, want %s", pc.MITMHosts, adoHost)
	}
	if p.Spec.ProxyConfig.ADOGrant == nil || len(p.Spec.ProxyConfig.Injection) != 1 {
		t.Error("the strip changed the caller's spec")
	}

	resident := DeliveryPolicy{Classes: map[string]ClassPolicy{ClassADOMintedPAT: {Mode: ModeRunnerResident}}}
	rp, _ := viaOrgADOPlan(CredentialOrigin{Class: ClassBrokered, Delivery: ClassADOMintedPAT, ResidentOptIn: true}, resident)
	rout, r := LocalEligibility(rp)
	if r != nil {
		t.Fatalf("resident refused: %+v", r)
	}
	if rout.ProxyConfig.ADOGrant == nil || len(rout.ProxyConfig.Injection) != 1 || len(rout.ProxyConfig.ViaOrgRoutes) != 0 {
		t.Errorf("a resident plan's spec changed: %+v", rout.ProxyConfig)
	}
}
