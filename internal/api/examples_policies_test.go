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
			if filepath.Base(f) == "composer-dev-subscription.template.json" {
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
