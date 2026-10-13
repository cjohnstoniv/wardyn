// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// viaOrgPlan is a run with an own injection, an operator api_key injection and a brokered GitHub
// grant, every origin stamped, under policy.
func viaOrgPlan(policy DeliveryPolicy) (LocalPlan, uuid.UUID, uuid.UUID, uuid.UUID) {
	own, op, gh := uuid.New(), uuid.New(), uuid.New()
	spec := runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{
		Policy: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
			{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"own.example","secret_name":"mine"}`), OwnerOnly: true},
			{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.corp.example","secret_name":"corp-secret"}`)},
			{Kind: types.GrantGitHubToken},
		}},
		Injection: []runner.InjectionGrant{
			{GrantID: own, Rule: egress.InjectionRule{Host: "own.example", Header: "Authorization", SecretName: "mine"}},
			{GrantID: op, Rule: egress.InjectionRule{Host: "api.corp.example", Header: "X-Corp-Key", SecretName: "corp-secret", PinPath: "/v1/only"}},
		},
		MITMHosts: []string{"own.example", "operator-only.example"},
		GitGrants: map[string]uuid.UUID{"acme/app": gh},
	}}
	ownOrigin := CredentialOrigin{Class: ClassOwn, Delivery: ClassAPIKey, GrantKind: types.GrantAPIKey, Stored: true, OwnNamespace: true, OwnerOnly: true}
	opOrigin := CredentialOrigin{Class: ClassOperator, Delivery: ClassAPIKey, GrantKind: types.GrantAPIKey, Stored: true}
	ghOrigin := CredentialOrigin{Class: ClassBrokered, Delivery: ClassGitHubToken, GrantKind: types.GrantGitHubToken}
	return LocalPlan{Spec: spec, Delivery: policy, Origins: map[string]CredentialOrigin{
		"ProxyConfig.Policy.EligibleGrants[0]": ownOrigin, "ProxyConfig.Policy.EligibleGrants[1]": opOrigin, "ProxyConfig.Policy.EligibleGrants[2]": ghOrigin,
		"ProxyConfig.Injection[0]": ownOrigin, "ProxyConfig.Injection[1]": opOrigin, "ProxyConfig.GitGrants[acme/app]": ghOrigin,
	}}, own, op, gh
}

var viaOrgBoth = DeliveryPolicy{Classes: map[string]ClassPolicy{ClassAPIKey: {Mode: ModeViaOrg}, ClassGitHubToken: {Mode: ModeViaOrg}}}

// The laptop copy keeps only the grant id and host of a via_org grant: the operator rule (header,
// secret name, pins) and the policy grant stay at the organisation, and MITMHosts gains the
// destination while an operator-only host is dropped.
func TestLocalCopy_ViaOrgGrantKeepsOnlyIdAndHost(t *testing.T) {
	p, own, op, gh := viaOrgPlan(viaOrgBoth)
	out, r := LocalEligibility(p)
	if r != nil {
		t.Fatalf("refused: %v", r)
	}
	pc := out.ProxyConfig
	if len(pc.Injection) != 1 || pc.Injection[0].GrantID != own {
		t.Errorf("injections = %+v, want only the own one", pc.Injection)
	}
	want := []proxy.ViaOrgRoute{{GrantID: op, Host: "api.corp.example"}, {GrantID: gh, Host: "github.com"}}
	slices.SortFunc(want, func(a, b proxy.ViaOrgRoute) int { return strings.Compare(a.GrantID.String(), b.GrantID.String()) })
	if !slices.Equal(pc.ViaOrgRoutes, want) {
		t.Errorf("routes = %+v, want %+v", pc.ViaOrgRoutes, want)
	}
	if len(pc.Policy.EligibleGrants) != 1 || !pc.Policy.EligibleGrants[0].OwnerOnly {
		t.Errorf("policy grants = %+v, want only the own one", pc.Policy.EligibleGrants)
	}
	if !slices.Contains(pc.MITMHosts, "api.corp.example") || !slices.Contains(pc.MITMHosts, "own.example") ||
		slices.Contains(pc.MITMHosts, "operator-only.example") || slices.Contains(pc.MITMHosts, "github.com") {
		t.Errorf("MITMHosts = %v, want the own and via_org hosts only", pc.MITMHosts)
	}
	if pc.GitGrants["acme/app"] != gh {
		t.Errorf("git allowlist = %v, want it kept for the /wardyn/gh/ route", pc.GitGrants)
	}
	b, err := runner.BuildProxyConfig(uuid.New(), pc, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"corp-secret", "X-Corp-Key", "/v1/only"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("the laptop config carries %q: %s", leak, b)
		}
	}
	if len(p.Spec.ProxyConfig.Injection) != 2 || len(p.Spec.ProxyConfig.Policy.EligibleGrants) != 3 || len(p.Spec.ProxyConfig.MITMHosts) != 2 {
		t.Error("the strip changed the caller's spec")
	}
}

// Each via_org class needs its own mode: with one class configured the other still refuses.
func TestLocalCopy_ViaOrgNeedsEachClassConfigured(t *testing.T) {
	for _, tc := range []struct {
		policy DeliveryPolicy
		field  string
	}{
		{DeliveryPolicy{}, "ProxyConfig.Injection[1]"},
		{DeliveryPolicy{Classes: map[string]ClassPolicy{ClassAPIKey: {Mode: ModeViaOrg}}}, "ProxyConfig.GitGrants[acme/app]"},
		{DeliveryPolicy{Classes: map[string]ClassPolicy{ClassGitHubToken: {Mode: ModeViaOrg}}}, "ProxyConfig.Injection[1]"},
		{DeliveryPolicy{Classes: map[string]ClassPolicy{ClassAPIKey: {Mode: ModeRunnerResident}, ClassGitHubToken: {Mode: ModeViaOrg}}}, "ProxyConfig.Injection[1]"},
	} {
		p, _, _, _ := viaOrgPlan(tc.policy)
		if _, r := LocalEligibility(p); r == nil || r.Reason != ReasonPlacementCredential || r.Field != tc.field {
			t.Errorf("policy %+v: refusal %v, want %s", tc.policy, r, tc.field)
		}
	}
}
