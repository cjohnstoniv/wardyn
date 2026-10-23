// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const fillHost = "api.corp.test"

// fillPlan is a runner_resident env_secret (or file_secret) whose value
// WARDYN_RUNNER_SECRET_PLACEHOLDERS keeps at the organisation: the sandbox
// field holds the fill placeholder and the run's proxy config the fill
// injection, classified as the class it fills.
func fillPlan(class string, cp ClassPolicy) (LocalPlan, uuid.UUID) {
	grant := uuid.New()
	ph := FillPlaceholderPrefix + grant.String()
	p := LocalPlan{
		Spec: runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{
			Injection: []runner.InjectionGrant{{GrantID: grant, Rule: egress.InjectionRule{Host: fillHost, Header: "Authorization", SecretName: "corp-token", Format: "%s", RequireTLS: true, Placeholder: ph}}},
		}},
		Origins: map[string]CredentialOrigin{
			"ProxyConfig.Injection[0]": {Class: ClassOperator, Delivery: class, GrantKind: types.GrantAPIKey, Stored: true},
		},
		Delivery: DeliveryPolicy{Classes: map[string]ClassPolicy{class: cp}},
		Trust:    types.RunnerTrustPersonal,
	}
	if class == ClassEnvSecret {
		p.Spec.SecretEnv = map[string]string{"TOKEN": ph}
		p.Origins[secretEnvPath] = CredentialOrigin{Class: ClassOperator, Delivery: ClassEnvSecret}
	} else {
		p.Spec.ManagedFiles = []runner.ManagedFile{{Path: runner.ComponentSecretDir + "/token", AgentOwned: true, Content: []byte(ph)}}
		p.Origins["SandboxSpec.ManagedFiles[0]"] = CredentialOrigin{Class: ClassOperator, Delivery: ClassFileSecret}
	}
	return p, grant
}

// The runner gets the placeholder and a via_org route, never the rule: the
// injection leaves the laptop copy and its host is intercepted, so the
// runner's proxy forwards that host's requests to the organisation, which
// fills the placeholder.
func TestLocalEligibility_ResidentFillIsServedViaOrg(t *testing.T) {
	for _, class := range []string{ClassEnvSecret, ClassFileSecret} {
		p, grant := fillPlan(class, ClassPolicy{Mode: ModeRunnerResident})
		out, r := LocalEligibility(p)
		if r != nil {
			t.Fatalf("%s: fill refused: %v", class, r)
		}
		pc := out.ProxyConfig
		if len(pc.Injection) != 0 || !slices.Equal(pc.ViaOrgRoutes, []proxy.ViaOrgRoute{{GrantID: grant, Host: fillHost}}) || !slices.Contains(pc.MITMHosts, fillHost) {
			t.Fatalf("%s: laptop copy injection %+v routes %+v mitm %v, want only a via_org route for the host", class, pc.Injection, pc.ViaOrgRoutes, pc.MITMHosts)
		}
	}
}

// A fill is admitted only where its class is: the class's runner_resident
// mode, posture requirement and blast-radius floor all still apply.
func TestLocalEligibility_ResidentFillNeedsItsClassAdmitted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cp     ClassPolicy
		floor  types.ConfinementClass
		reason Reason
	}{
		{"class refused", ClassPolicy{Mode: ModeRefuse}, "", ReasonPlacementCredential},
		{"posture unmet", ClassPolicy{Mode: ModeRunnerResident, RequirePosture: &PostureRequirement{DiskEncrypted: true}}, "", ReasonRunnerPostureUnmet},
		{"floor", ClassPolicy{Mode: ModeRunnerResident}, types.CC3, ReasonRunnerPostureUnmet},
	} {
		p, _ := fillPlan(ClassEnvSecret, tc.cp)
		p.ConfinementFloor = tc.floor
		delete(p.Origins, secretEnvPath) // judge the injection alone
		p.Spec.SecretEnv = nil
		if _, r := LocalEligibility(p); r == nil || r.Reason != tc.reason || r.Field != "ProxyConfig.Injection[0]" {
			t.Errorf("%s: %+v, want %s on the injection", tc.name, r, tc.reason)
		}
	}
}

// A fill placeholder carries no value, so a runner_resident sandbox field may
// hold one; anything else not a placeholder is still refused.
func TestLocalEligibility_ResidentFieldMayHoldAFillPlaceholder(t *testing.T) {
	p := residentPlan(ClassPolicy{Mode: ModeRunnerResident}, nil)
	p.Spec.SecretEnv["TOKEN"] = FillPlaceholderPrefix + uuid.NewString()
	if _, r := LocalEligibility(p); r != nil {
		t.Fatalf("fill placeholder in a resident variable: %v", r)
	}
	p.Spec.SecretEnv["TOKEN"] = FillPlaceholderPrefix + "not-a-grant"
	if _, r := LocalEligibility(p); r == nil || r.Field != secretEnvPath {
		t.Fatalf("malformed placeholder: %+v, want refused", r)
	}
}
