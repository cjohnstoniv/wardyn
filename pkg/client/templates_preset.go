// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// PresetDocument converts a stored launch preset into a RunTemplate document: the
// one place a preset becomes a template, so every client converts it the same
// way. The document is partial, named and described as the preset is, and holds
// the preset's request field by field. Whether it is publishable is the server's
// to say, at the save door.
//
// A preset limited to some user types is offered to those types only; a template
// is offered to everyone in its scope. So the conversion refuses such a preset
// unless everyone is true, which is the caller saying it knows the limit is lost.
//
// A preset's inline_policy replaced the caller's default policy whole; in a
// template it is laid onto the default, so a key the preset left out now takes
// the default's value instead of its zero value.
//
// Three things in a preset's request are not content and are dropped: the
// mode_contract marker, which belongs to the request a client sends, and an empty
// string or null, which says nothing (agent, repo, first_use_approval,
// min_confinement_class and a nil allowed_domains are always written, and a
// template reads a key that is present as a choice).
func PresetDocument(p Preset, everyone bool) (TemplateDocument, error) {
	if len(p.UserTypes) > 0 && !everyone {
		return TemplateDocument{}, fmt.Errorf(
			"preset %q is limited to the user types %s, and a template is offered to everyone in its scope: pass everyone (--everyone on the CLI) to publish it to all of them",
			p.Name, strings.Join(p.UserTypes, ", "))
	}
	body, err := json.Marshal(p.Request)
	if err != nil {
		return TemplateDocument{}, fmt.Errorf("preset %q: %w", p.Name, err)
	}
	var intent TemplateIntent
	if err := json.Unmarshal(body, &intent); err != nil {
		return TemplateDocument{}, fmt.Errorf("preset %q: %w", p.Name, err)
	}
	intent.Unset("mode_contract")
	for _, name := range intent.Names() {
		if saysNothing(intent.Raw(name)) {
			intent.Unset(name)
		}
	}
	if raw := intent.Raw("inline_policy"); raw != nil {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			return TemplateDocument{}, fmt.Errorf("preset %q: inline_policy: %w", p.Name, err)
		}
		maps.DeleteFunc(keys, func(_ string, v json.RawMessage) bool { return saysNothing(v) })
		intent.Unset("inline_policy")
		if len(keys) > 0 {
			if err := intent.Set("inline_policy", keys); err != nil {
				return TemplateDocument{}, fmt.Errorf("preset %q: inline_policy: %w", p.Name, err)
			}
		}
	}
	return TemplateDocument{
		APIVersion:  TemplateDocumentVersion,
		Kind:        TemplateDocumentKind,
		Name:        p.Name,
		Description: p.Description,
		Coverage:    TemplateCoveragePartial,
		Intent:      intent,
	}, nil
}

// saysNothing is a value a request writes only because its Go field has no
// omitempty: an empty string or null.
func saysNothing(raw json.RawMessage) bool {
	s := string(raw)
	return s == `""` || s == "null"
}
