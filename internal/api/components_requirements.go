// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The save doors (POST /policies, PUT /policies/{id}, POST/PUT /me/components,
// PUT /components/{id}) answer with `requirements`: what the saved thing needs
// before a run can use it, so an admin or a person finds out at save time and
// not at launch. The list is advisory and never refuses a save.
const (
	requirementKindSecret   = "secret"
	requirementPresent      = "present"
	requirementMissing      = "missing"
	requirementFixAddSecret = "add_secret"
)

// secretRequirement is one secret row: its name, whether the saver's namespace
// holds it, and the fix a missing one asks for. The list carries names and a
// verdict only — never a config value or a header name the definition wrote.
func secretRequirement(name string, present bool) client.ComponentRequirement {
	if present {
		return client.ComponentRequirement{Kind: requirementKindSecret, Name: name, Status: requirementPresent}
	}
	return client.ComponentRequirement{Kind: requirementKindSecret, Name: name, Status: requirementMissing, Fix: requirementFixAddSecret}
}

// componentRequirements is the list for one definition. A secret the person
// supplies is looked up in hasOwn (the saver's namespace); a `shared` one, which
// only an organisation row can carry, in hasOperator. A name used by two
// deliveries is listed once.
func componentRequirements(def types.ComponentDefinition, hasOwn, hasOperator func(string) bool) []client.ComponentRequirement {
	out := []client.ComponentRequirement{}
	seen := map[[2]any]bool{}
	for _, sec := range def.Secrets {
		key := [2]any{sec.SecretName, sec.Shared}
		if seen[key] {
			continue
		}
		seen[key] = true
		has := hasOwn
		if sec.Shared {
			has = hasOperator
		}
		out = append(out, secretRequirement(sec.SecretName, has(sec.SecretName)))
	}
	return out
}

// componentSaveRequirements reads presence for a row being saved: an
// organisation row (Owner "") and the operator's own request look in the
// operator namespace; a person's row looks in that person's own rows only, never
// the operator fallback, so a member cannot learn that the operator holds a
// name. It is the namespace the run gate checks (callerOwnsSecret).
func (s *Server) componentSaveRequirements(r *http.Request, c types.Component) []client.ComponentRequirement {
	ctx := r.Context()
	var operatorNames map[string]bool
	hasOperator := func(name string) bool {
		if operatorNames == nil {
			operatorNames = s.presentSecretNames(ctx)
		}
		return operatorNames[name]
	}
	hasOwn := hasOperator
	if c.Owner != "" && !operatorOwnedRequest(ctx) {
		hasOwn = func(name string) bool { return s.ownsSecretMemoized(ctx, c.Owner, name) }
	}
	return componentRequirements(c.Definition, hasOwn, hasOperator)
}
