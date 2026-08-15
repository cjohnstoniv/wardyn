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

// TestReadPolicyFile_RejectsUnknownSpecField is the W14-S1-2 regression:
// readPolicyFile (backing `policy create -f` / `policy update -f`) used to
// json.Unmarshal the spec leniently, so a misspelled field silently vanished
// instead of failing at authoring time. Covers both the bare-spec shape and
// the full-body {"name":...,"spec":{...}} shape.
func TestReadPolicyFile_RejectsUnknownSpecField(t *testing.T) {
	dir := t.TempDir()

	bareSpec := dir + "/bare.json"
	writeFile(t, bareSpec, `{"allowed_domains":["example.com"],"min_confinement_klass":"CC1"}`)
	if _, err := readPolicyFile(bareSpec, "my-policy"); err == nil {
		t.Fatal("bare spec with unknown field: expected an error, got nil")
	}

	fullBody := dir + "/full.json"
	writeFile(t, fullBody, `{"name":"my-policy","spec":{"allowed_domains":["example.com"],"min_confinement_klass":"CC1"}}`)
	if _, err := readPolicyFile(fullBody, ""); err == nil {
		t.Fatal("full body with unknown spec field: expected an error, got nil")
	}
}

// TestReadPolicyFile_ToleratesStrayTopLevelKeys guards the negative: a file
// produced by `policy get --json` (id/created_at/updated_at alongside
// name/spec) must still round-trip into `policy update -f` — only the SPEC is
// decoded strict, not the enclosing body.
func TestReadPolicyFile_ToleratesStrayTopLevelKeys(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/dump.json"
	writeFile(t, file, `{"id":"11111111-1111-1111-1111-111111111111","name":"my-policy",`+
		`"spec":{"allowed_domains":["example.com"],"min_confinement_class":"CC1"},`+
		`"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)

	body, err := readPolicyFile(file, "")
	if err != nil {
		t.Fatalf("readPolicyFile: %v", err)
	}
	if body.Name != "my-policy" || body.Spec.MinConfinementClass != types.CC1 {
		t.Errorf("body = %+v, want name=my-policy min_confinement_class=CC1", body)
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
