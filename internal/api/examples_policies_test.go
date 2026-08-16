// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	yaml "gopkg.in/yaml.v3"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestExamplePoliciesValidate keeps every shipped example policy loadable under
// the CURRENT validator — including the domain-shape check, so no example may
// ship a dead allowlist entry (a mid-label wildcard the proxy can never match).
// JSON goes through LoadPolicySpec verbatim; YAML through the same yaml->json
// bridge the CLI uses (cmd/wardyn/policyio.go).
func TestExamplePoliciesValidate(t *testing.T) {
	files, err := filepath.Glob("../../examples/policies/*")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob examples/policies: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		switch filepath.Ext(f) {
		case ".json":
			// The subscription TEMPLATE carries a __comment key and
			// placeholder mount paths — it is generated, never loaded.
			if filepath.Base(f) == "claude-subscription.template.json" {
				continue
			}
			if _, err := LoadPolicySpec(f); err != nil {
				t.Errorf("%s: %v", f, err)
			}
		case ".yaml", ".yml":
			b, rerr := os.ReadFile(f)
			if rerr != nil {
				t.Fatalf("read %s: %v", f, rerr)
			}
			var doc any
			if uerr := yaml.Unmarshal(b, &doc); uerr != nil {
				t.Errorf("%s: parse yaml: %v", f, uerr)
				continue
			}
			j, merr := json.Marshal(doc)
			if merr != nil {
				t.Errorf("%s: re-encode: %v", f, merr)
				continue
			}
			var spec types.RunPolicySpec
			if derr := json.Unmarshal(j, &spec); derr != nil {
				t.Errorf("%s: decode spec: %v", f, derr)
				continue
			}
			if verr := validatePolicySpec(spec); verr != nil {
				t.Errorf("%s: %v", f, verr)
			}
		}
	}
}

// TestCIClaudeLLMExample_MeetsCINonNegotiables pins W16-S1-6: docs/CI.md's
// "Model access for harness mode" section points readers at a model-access
// example for CI. The old pointer (examples/policies/claude-llm.json) is a DEV
// policy that violates every one of the SAME doc's own CI non-negotiables one
// section up ("Writing a CI policy") — deny_with_review instead of
// always_deny, a requires_approval:true grant, and an unbounded (0)
// auto_stop_after_sec. examples/policies/ci-claude-llm.json is ci.json's CI
// baseline plus exactly the api.anthropic.com egress entry and api_key grant
// model access needs — a copy-paste-safe CI example, not a dev ceiling.
func TestCIClaudeLLMExample_MeetsCINonNegotiables(t *testing.T) {
	spec, err := LoadPolicySpec("../../examples/policies/ci-claude-llm.json")
	if err != nil {
		t.Fatalf("load ci-claude-llm.json: %v", err)
	}
	if spec.FirstUseApproval != types.FirstUseAlwaysDeny {
		t.Errorf("first_use_approval = %q, want %q (CI non-negotiable: nothing waits on a human)", spec.FirstUseApproval, types.FirstUseAlwaysDeny)
	}
	if spec.AutoStopAfterSec <= 0 {
		t.Errorf("auto_stop_after_sec = %d, want a positive bound (CI non-negotiable: bound the run)", spec.AutoStopAfterSec)
	}
	foundAPIKey := false
	for _, g := range spec.EligibleGrants {
		if g.RequiresApproval {
			t.Errorf("grant %+v requires approval — CI non-negotiable: no requires_approval:true grants (a human is never there to decide)", g)
		}
		if g.Kind != types.GrantAPIKey {
			continue
		}
		var scope struct {
			Host       string `json:"host"`
			SecretName string `json:"secret_name"`
		}
		if err := json.Unmarshal(g.Scope, &scope); err == nil && scope.Host == "api.anthropic.com" && scope.SecretName == "anthropic-api-key" {
			foundAPIKey = true
		}
	}
	if !foundAPIKey {
		t.Errorf("eligible_grants = %+v, want a no-approval api_key grant for api.anthropic.com/anthropic-api-key", spec.EligibleGrants)
	}
	found := false
	for _, d := range spec.AllowedDomains {
		if d == "api.anthropic.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("allowed_domains = %v, want api.anthropic.com", spec.AllowedDomains)
	}
}

// TestValidatePolicySpecRejectsDeadDomain is the trust-boundary check itself:
// a mid-label wildcard is rejected at policy write time, the supported forms
// are not.
func TestValidatePolicySpecRejectsDeadDomain(t *testing.T) {
	spec := func(d string) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{d}}
	}
	if err := validatePolicySpec(spec("oidc.*.amazonaws.com")); err == nil {
		t.Fatal("mid-label wildcard must be rejected")
	}
	for _, d := range []string{"*.amazonaws.com", "api.anthropic.com", "api.anthropic.com:443"} {
		if err := validatePolicySpec(spec(d)); err != nil {
			t.Errorf("%s must stay valid: %v", d, err)
		}
	}
}
