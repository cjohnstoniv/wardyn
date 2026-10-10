// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func TestLocalEligibilityOwnNamespaceAndLateCredential(t *testing.T) {
	path := "ProxyConfig.Injection[0]"
	p := LocalPlan{Spec: runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{Injection: []runner.InjectionGrant{{GrantID: uuid.New(), Rule: egress.InjectionRule{Host: "api.example.com", SecretName: "key"}}}}}, Origins: map[string]CredentialOrigin{path: {Class: ClassOwn, Stored: true, OwnNamespace: true, OwnerOnly: true}}}
	if _, r := LocalEligibility(p); r != nil {
		t.Fatal(r)
	}
	for _, tc := range []struct {
		name   string
		origin CredentialOrigin
	}{
		{"unknown", CredentialOrigin{}},
		{"operator collision", CredentialOrigin{Class: ClassOwn, Stored: true, OwnerOnly: true}},
		{"fallback", CredentialOrigin{Class: ClassOwn, Stored: true, OwnNamespace: true}},
		{"shared", CredentialOrigin{Class: ClassOperator, Stored: true, Delivery: ClassAPIKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.Origins[path] = tc.origin
			if _, r := LocalEligibility(p); r == nil || r.Reason != ReasonPlacementCredential || r.Field != path {
				t.Fatalf("refusal=%v", r)
			}
		})
	}
	p.Origins[path] = CredentialOrigin{Class: ClassOwn, Stored: true, OwnNamespace: true, OwnerOnly: true}
	p.Spec.SecretEnv = map[string]string{"LATE_TOKEN": "a-secret"}
	if _, r := LocalEligibility(p); r == nil || r.Field != "SandboxSpec.SecretEnv[LATE_TOKEN]" {
		t.Fatalf("late phase escaped: %v", r)
	}
}

func TestLocalEligibilityUnimplementedDeliveryCannotAuthorize(t *testing.T) {
	for _, mode := range []Mode{ModeRefuse, ModeViaOrg, ModeRunnerResident} {
		p := LocalPlan{Spec: runner.SandboxSpec{SecretEnv: map[string]string{"TOKEN": "value"}}, Origins: map[string]CredentialOrigin{"SandboxSpec.SecretEnv[TOKEN]": {Class: ClassOperator, Delivery: ClassEnvSecret}}, Delivery: DeliveryPolicy{Classes: map[string]ClassPolicy{ClassEnvSecret: {Mode: mode}}}}
		if _, r := LocalEligibility(p); r == nil || r.Reason != ReasonPlacementCredential {
			t.Fatalf("mode %s: %v", mode, r)
		}
	}
}

func TestLocalEligibilityApprovalGrantAndUnknownKind(t *testing.T) {
	path := "ProxyConfig.Policy.EligibleGrants[0]"
	p := LocalPlan{Spec: runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{Policy: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, RequiresApproval: true}}}}}, Origins: map[string]CredentialOrigin{path: {Class: ClassOperator, Delivery: ClassAPIKey, GrantKind: types.GrantAPIKey}}}
	if _, r := LocalEligibility(p); r == nil || r.Field != path {
		t.Fatalf("approval credential escaped: %v", r)
	}
	p.Origins[path] = CredentialOrigin{Class: ClassOwn, Stored: true, OwnNamespace: true, OwnerOnly: true, GrantKind: "future_key"}
	if _, r := LocalEligibility(p); r == nil {
		t.Fatal("unknown grant kind admitted")
	}
}

func TestLocalEligibilityStripsOperatorConfigWithoutMutating(t *testing.T) {
	li := &types.LLMInspectionSpec{Mode: "block", WorkspaceSecretNames: []string{"operator-key"}, WorkspaceSecretValues: []string{"private"}, DetectSecretPatterns: true}
	p := LocalPlan{OrgConfigKeys: []string{"ORG_KEY"}, Spec: runner.SandboxSpec{Env: map[string]string{"ORG_KEY": "private", "OWN": "value"}, ProxyConfig: runner.ProxyConfig{Policy: types.RunPolicySpec{LLMInspection: li}, UpstreamProxyURL: "https://user:password@parent", TrustedCAPEM: "corporate", InternalHosts: []types.InternalHost{{HostSuffix: "corp"}}, LLMUpstreams: map[string]string{"vendor": "corp"}, MITMHosts: []string{"corp:443"}}}}
	out, r := LocalEligibility(p)
	if r != nil {
		t.Fatal(r)
	}
	if out.Env["ORG_KEY"] != "" || out.Env["OWN"] != "value" || out.ProxyConfig.UpstreamProxyURL != "" || out.ProxyConfig.TrustedCAPEM != "" || len(out.ProxyConfig.InternalHosts)+len(out.ProxyConfig.LLMUpstreams)+len(out.ProxyConfig.MITMHosts) != 0 || len(out.ProxyConfig.Policy.LLMInspection.WorkspaceSecretValues)+len(out.ProxyConfig.Policy.LLMInspection.WorkspaceSecretNames) != 0 {
		t.Fatalf("operator config leaked: %+v", out)
	}
	if p.Spec.Env["ORG_KEY"] != "private" || li.WorkspaceSecretValues[0] != "private" || p.Spec.ProxyConfig.UpstreamProxyURL == "" {
		t.Fatal("mutated admitted snapshot")
	}
	p.UpstreamProxySecretRef = "proxy-key"
	if _, r := LocalEligibility(p); r == nil || r.Field != "upstream_proxy_secret_ref" {
		t.Fatalf("secret ref escaped: %v", r)
	}
}

func TestLocalEligibilityTrustedAndP3RemainIndependentOfReportedConfinement(t *testing.T) {
	p := LocalPlan{TrustedOutput: true, Spec: runner.SandboxSpec{ConfinementClass: types.CC3}}
	if _, r := LocalEligibility(p); r == nil || r.Reason != ReasonPlacementTrustedOutput {
		t.Fatalf("trusted output: %v", r)
	}
	p.TrustedOutput = false
	p.SelfDefinedComponents = []string{"my connector"}
	if _, r := LocalEligibility(p); r == nil || r.Reason != ReasonPlacementComponentSelfDefine {
		t.Fatalf("P3 default: %v", r)
	}
	p.LocalSelfDefinedComponents = true
	if _, r := LocalEligibility(p); r != nil {
		t.Fatal(r)
	}
	if EvidenceFor(Local) != EvidenceRunnerAsserted {
		t.Fatal("runner self-report promoted to attestation")
	}
}

func TestNestedSchemaDoesNotInheritExemptionAndBoundsDepth(t *testing.T) {
	type nested struct{ NewToken string }
	type config struct{ Hidden []map[string]*nested }
	got := nestedUnclassified(reflect.TypeFor[config](), "ProxyConfig.Policy.LLMInspection", 0)
	if len(got) == 0 || !strings.Contains(got[0], "LLMInspection") {
		t.Fatalf("new nested carrier admitted: %v", got)
	}
	if got := nestedUnclassified(reflect.TypeFor[types.LLMInspectionSpec](), "LLMInspection", 33); len(got) != 1 {
		t.Fatalf("depth cap=%v", got)
	}
}
