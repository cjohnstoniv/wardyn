// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// policyRequirements is the save-time list for a policy: every stored secret
// its api_key, git_pat, ssh_key, env_secret and file_secret grants name, with
// whether the saver's namespace holds it. needed is secretRefsOf's output for
// spec, which has already refused a reserved name, so no sinkReservedSecret name
// can reach the list. Advisory: whose run selects the policy decides where the
// secret is read from, and run-create checks that (#1123).
func (s *Server) policyRequirements(r *http.Request, spec types.RunPolicySpec, needed []neededSecret) []client.ComponentRequirement {
	names := make([]string, 0, len(needed))
	for _, n := range needed {
		names = append(names, n.name)
	}
	// secretRefsOf leaves env_secret out (a missing one is no create-time
	// refusal), but the saver still needs to hear about it.
	for _, g := range spec.EligibleGrants {
		if g.Kind != types.GrantEnvSecret {
			continue
		}
		if _, secretName, err := envSecretScopeFields(g.Scope); err == nil {
			names = append(names, secretName)
		}
	}
	out := []client.ComponentRequirement{}
	if len(names) == 0 {
		return out
	}
	present := s.presentSecretNamesFor(r.Context(), s.secretOwnerFromRequest(r))
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, secretRequirement(name, present[name]))
	}
	return out
}
