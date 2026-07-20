// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPolicyIO_YAMLEqualsJSON proves the YAML skin is exactly the JSON schema:
// an equivalent YAML and JSON document must decode to the same RunPolicySpec,
// and malformed input must error instead of silently yielding a zero spec.
func TestPolicyIO_YAMLEqualsJSON(t *testing.T) {
	const yamlDoc = `
allowed_domains:
  - api.anthropic.com
  - "*.anthropic.com"
first_use_approval: always_deny
min_confinement_class: CC1
auto_stop_after_sec: 900
`
	const jsonDoc = `{
  "allowed_domains": ["api.anthropic.com", "*.anthropic.com"],
  "first_use_approval": "always_deny",
  "min_confinement_class": "CC1",
  "auto_stop_after_sec": 900
}`

	fromYAML := decodeSpec(t, yamlDoc)
	fromJSON := decodeSpec(t, jsonDoc)
	if !reflect.DeepEqual(fromYAML, fromJSON) {
		t.Fatalf("YAML and JSON decoded to different specs:\n yaml=%+v\n json=%+v", fromYAML, fromJSON)
	}
	if fromYAML.FirstUseApproval.Normalize() != types.FirstUseAlwaysDeny {
		t.Fatalf("first_use_approval not carried through: %q", fromYAML.FirstUseApproval)
	}
	if fromYAML.MinConfinementClass != types.CC1 || fromYAML.AutoStopAfterSec != 900 {
		t.Fatalf("scalar fields lost: %+v", fromYAML)
	}

	if _, err := policyToJSON([]byte("allowed_domains: [oops\nunterminated")); err == nil {
		t.Fatal("expected malformed YAML to error, got nil")
	}
}

func decodeSpec(t *testing.T, doc string) types.RunPolicySpec {
	t.Helper()
	j, err := policyToJSON([]byte(doc))
	if err != nil {
		t.Fatalf("policyToJSON: %v", err)
	}
	var spec types.RunPolicySpec
	if err := json.Unmarshal(j, &spec); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	return spec
}
