// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestBaselineExtendsTheEgressGrade(t *testing.T) {
	internal := types.RunPolicySpec{AllowedDomains: []string{"llm.corp.example"}}
	grade := func(b Baseline) RiskLevel {
		for _, it := range Grade(RunInput{Baseline: b}, internal) {
			if it.Field == "allowed_domains" {
				return it.Level
			}
		}
		t.Fatal("no allowed_domains item")
		return ""
	}
	for _, tc := range []struct {
		name string
		b    Baseline
		open bool
		risk RiskLevel
	}{
		{"zero value grades as before", Baseline{}, true, RiskMedium},
		{"declared host", Baseline{Hosts: []string{"llm.corp.example"}}, false, RiskLow},
		{"a different declared host does not help", Baseline{Hosts: []string{"other.corp.example"}}, true, RiskMedium},
		{"suffix covers a subdomain", Baseline{Suffixes: []string{"corp.example"}}, false, RiskLow},
		{"suffix is label-anchored", Baseline{Suffixes: []string{"orp.example"}}, true, RiskMedium},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := grade(tc.b); got != tc.risk {
				t.Errorf("risk = %s, want %s", got, tc.risk)
			}
			egress := AutonomyPostureOf(internal, types.CC2, tc.b).Egress
			if open := egress == types.AutonomyEgressOpen; open != tc.open {
				t.Errorf("egress = %s, want open=%v", egress, tc.open)
			}
		})
	}
}

func TestBaselineNeverMatchesAWildcardAllowlistEntry(t *testing.T) {
	b := Baseline{Hosts: []string{"llm.corp.example"}, Suffixes: []string{"corp.example"}}
	if b.Has("*.corp.example") {
		t.Error("a wildcard allowlist entry is wider than any host the operator vouched for")
	}
	if !b.Has(" LLM.Corp.Example ") || !b.Has("api.anthropic.com") {
		t.Error("declared and built-in hosts are baseline, case- and space-insensitively")
	}
}

func TestBaselineLiftsTheAPIKeyFloorForADeclaredHost(t *testing.T) {
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{
		Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"llm.corp.example"}`),
	}}}
	if got := RequiredConfinementFloor(spec, Baseline{}); got != types.CC3 {
		t.Fatalf("undeclared internal host: floor = %q, want CC3", got)
	}
	if got := AutonomyPostureOf(spec, types.CC2, Baseline{}).Secrets; got != types.AutonomySecretsPowerful {
		t.Errorf("undeclared internal host: secrets = %s, want powerful", got)
	}
	b := Baseline{Hosts: []string{"llm.corp.example"}}
	if got := RequiredConfinementFloor(spec, b); got != "" {
		t.Errorf("declared host: floor = %q, want none", got)
	}
	if got := AutonomyPostureOf(spec, types.CC2, b).Secrets; got != types.AutonomySecretsBaseline {
		t.Errorf("declared host: secrets = %s, want baseline", got)
	}
	// A write-capable grant keeps its floor whatever the baseline says.
	write := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantCloudSTS}}}
	if got := RequiredConfinementFloor(write, b); got != types.CC3 {
		t.Errorf("write-capable grant: floor = %q, want CC3", got)
	}
}
