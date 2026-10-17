// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrKeyMissing is TopLevelString's answer for a body with no such key.
var ErrKeyMissing = errors.New("adoscope: the body has no such top-level key")

// TopLevelString returns the JSON string at the exact top-level key of an
// object body, for a caller that must act on one field the server also reads
// (the Azure route gate's deployment `model`).
//
// It shares the classifier's duplicate-key walk (walkUnique): a body that
// repeats a key anywhere, in any case, is refused, because the gate and the
// server need not agree which of two `model` values is the last. Only the exact
// spelling of key counts, so `Model` alone is a missing key, not a hit. A body
// that is not one JSON object, or whose value is not a non-empty string, is
// refused too. The body is streamed, never copied into a second structure.
func TopLevelString(body []byte, key string) (string, error) {
	if err := uniqueKeys(body); err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", errors.New("adoscope: body is not one JSON object")
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("adoscope: body is not decodable JSON: %w", err)
		}
		if k != key {
			if err := skipValue(dec); err != nil {
				return "", err
			}
			continue
		}
		var s string
		if err := dec.Decode(&s); err != nil || s == "" {
			return "", fmt.Errorf("adoscope: top-level %q is not a non-empty string", key)
		}
		return s, nil
	}
	return "", ErrKeyMissing
}

// skipValue consumes exactly one JSON value from dec.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("adoscope: body is not decodable JSON: %w", err)
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}
