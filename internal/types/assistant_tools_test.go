// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Every tool name has the shape the egress proxy and the relay accept, so no call is refused before the server
// sees it. The pattern is written out here, not read from the constant it checks.
func TestAssistantToolNamesMatchRelayPattern(t *testing.T) {
	re := regexp.MustCompile(`^[a-z_]{1,32}$`)
	specs := AssistantToolSpecs()
	if len(specs) == 0 {
		t.Fatal("no tools")
	}
	seen := map[string]bool{}
	for _, s := range specs {
		if !re.MatchString(s.Name) || !AssistantToolNameValid(s.Name) {
			t.Errorf("tool name %q does not match the relay's pattern", s.Name)
		}
		if seen[s.Name] {
			t.Errorf("tool name %q is listed twice", s.Name)
		}
		seen[s.Name] = true
	}
	if AssistantToolNameValid("Read") || AssistantToolNameValid("read-x") || AssistantToolNameValid(strings.Repeat("a", 33)) {
		t.Error("the name check accepts a name the relay refuses")
	}
}

// Each schema is a JSON object schema that refuses unnamed properties, served under input_schema.
func TestAssistantToolSchemasAreClosed(t *testing.T) {
	for _, s := range AssistantToolSpecs() {
		var schema map[string]any
		if err := json.Unmarshal(s.InputSchema, &schema); err != nil {
			t.Fatalf("%s: schema is not JSON: %v", s.Name, err)
		}
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Errorf("%s: schema = %s, want a closed object", s.Name, s.InputSchema)
		}
		b, err := json.Marshal(s)
		if err != nil || !strings.Contains(string(b), `"input_schema":{`) {
			t.Errorf("%s: encoded as %s, want the schema under input_schema", s.Name, b)
		}
	}
}

// simulate says, in its own description, that one set of rules judges every counted run.
func TestAssistantSimulateDescriptionSaysTheApproximation(t *testing.T) {
	found := false
	for _, s := range AssistantToolSpecs() {
		if s.Name != AssistantToolSimulate {
			continue
		}
		found = true
		if !strings.Contains(s.Description, "counts use the proposed rules, not each run's other layers") {
			t.Errorf("simulate's description = %q", s.Description)
		}
	}
	if !found {
		t.Error("no simulate tool in the list")
	}
}
