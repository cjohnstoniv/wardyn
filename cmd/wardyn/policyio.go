// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	yaml "gopkg.in/yaml.v3"
)

// policyToJSON accepts a policy document as either JSON or YAML and returns its
// canonical JSON encoding. Downstream readers keep unmarshaling the existing
// json-tagged RunPolicySpec/PolicyRequest structs and the server keeps running
// its strict DisallowUnknownFields validator — YAML is just an input skin, not
// a second schema.
//
// ponytail: YAML is a JSON superset and yaml.v3 decodes mappings into
// map[string]interface{} (JSON-marshalable), so this one bridge covers both
// formats and needs no extension/content sniff.
func policyToJSON(raw []byte) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc any
	if err := dec.Decode(&doc); err != nil && err != io.EOF {
		return nil, fmt.Errorf("parse policy (accepts JSON or YAML): %w", err)
	}
	// A policy is one document; ignoring a trailing document can silently
	// discard restrictions the operator intended to apply.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("policy input must contain exactly one document")
	} else if err != io.EOF {
		return nil, fmt.Errorf("parse policy after first document: %w", err)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-encode policy as JSON: %w", err)
	}
	return out, nil
}
