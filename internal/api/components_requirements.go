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
// only an organisation row can carry, in hasOperator. A name is listed once (the
// wire row has no shared flag); when its reads disagree it reads missing.
func componentRequirements(def types.ComponentDefinition, hasOwn, hasOperator func(string) bool) []client.ComponentRequirement {
	out := []client.ComponentRequirement{}
	at := map[string]int{}
	for _, sec := range def.Secrets {
		has := hasOwn
		if sec.Shared {
			has = hasOperator
		}
		present := has(sec.SecretName)
		if i, dup := at[sec.SecretName]; dup {
			if !present {
				out[i] = secretRequirement(sec.SecretName, false)
			}
			continue
		}
		at[sec.SecretName] = len(out)
		out = append(out, secretRequirement(sec.SecretName, present))
	}
	return out
}

// componentSaveRequirements reads presence for a row being saved. A `shared`
// secret is the operator's, read in the operator namespace. Any other secret is
// read as the run gate reads it (callerOwnsSecret): a person's row looks in that
// person's own rows only, never the operator fallback, so a member cannot learn
// that the operator holds a name; an organisation row (Owner "") looks in the
// operator's names for an operator-owned request and otherwise in the saver's
// own rows.
func (s *Server) componentSaveRequirements(r *http.Request, c types.Component) []client.ComponentRequirement {
	ctx := r.Context()
	adm := &componentAdmission{caller: c.Owner}
	if c.Owner == "" {
		adm.caller = runIdentitySubject(ctx, principalFromRequest(r))
	}
	hasOperator := func(name string) bool { return s.operatorSecretNames(ctx, adm)[name] }
	hasOwn := func(name string) bool { return s.callerOwnsSecret(r, adm, name) }
	return componentRequirements(c.Definition, hasOwn, hasOperator)
}
