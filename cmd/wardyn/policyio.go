// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
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

// errNotOneDocument is what decodeOneJSONStrict returns for anything but a
// single JSON value followed by end of input.
var errNotOneDocument = errors.New("input must contain exactly one document")

// decodeOneJSONStrict decodes exactly one JSON value from r into v, rejecting
// unknown fields, a top-level null, and anything after the value (a second
// document or trailing bytes, valid or not). It exists because a decoder that
// stops after the first value reads `{}` + the real document as "the desired
// state is empty", and a --prune then deletes everything the real document
// named. Empty or whitespace-only input is a parse error (io.EOF).
func decodeOneJSONStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(first), []byte("null")) {
		return errNotOneDocument
	}
	// The next read must hit the end of input: any value or any junk is a
	// second document, and junk fails Decode with a syntax error, not io.EOF.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return errNotOneDocument
	}
	strict := json.NewDecoder(bytes.NewReader(first))
	strict.DisallowUnknownFields()
	return strict.Decode(v)
}
