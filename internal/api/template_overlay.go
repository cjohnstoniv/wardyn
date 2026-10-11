// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// templatePolicyOverlay applies a template's inline_policy to a source policy.
//
// A request's inline_policy REPLACES its source policy, so a template's partial
// policy cannot be sent as one: every key it leaves out would become the zero
// value, not the baseline. The overlay lays only the keys the template
// presents onto the readable source (the policy_id it names, or the
// organisation's default) and returns the complete editable copy, which then
// goes through the same fold and ceiling as any inline policy. A key that is
// present and empty, false or zero is a choice and overwrites the source (an
// empty allowed_methods means every method; auto_stop_after_sec 0 means never);
// a key that is absent keeps the source's value. A nested object (llm_inspection,
// resources) replaces the source's whole object. An overlay that names no key is
// refused: it says nothing, and an empty block must not read as "the defaults".
// A null is refused too, since it would clear the source's value and no document
// the decoder accepts holds one.
func templatePolicyOverlay(source types.RunPolicySpec, overlay json.RawMessage) (types.RunPolicySpec, error) {
	var present map[string]json.RawMessage
	if err := json.Unmarshal(overlay, &present); err != nil {
		return types.RunPolicySpec{}, errors.New("inline_policy must be an object")
	}
	if len(present) == 0 {
		return types.RunPolicySpec{}, errors.New("inline_policy names no policy field")
	}
	base, err := json.Marshal(source)
	if err != nil {
		return types.RunPolicySpec{}, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return types.RunPolicySpec{}, err
	}
	for key, value := range present {
		if _, ok := templateRule(templatePolicyRules, key); !ok {
			return types.RunPolicySpec{}, fmt.Errorf("%s is not a policy field", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return types.RunPolicySpec{}, fmt.Errorf("%s is null: leave it out to keep the source's value", key)
		}
		merged[key] = value
	}
	body, err := json.Marshal(merged)
	if err != nil {
		return types.RunPolicySpec{}, err
	}
	var out types.RunPolicySpec
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return out, dec.Decode(&out)
}
