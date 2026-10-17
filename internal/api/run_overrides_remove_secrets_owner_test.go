// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// remove_secrets is refused for a member by the operator test itself, not by
// the owner string happening to be non-empty: a member whose owner resolves
// empty is refused, and an operator is not asked for ownership.
func TestOverrides_RemoveSecretsAsksEveryNonOperatorForOwnership(t *testing.T) {
	grants := func() types.RunPolicySpec {
		return types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey,
			Scope: mustJSON(map[string]any{"host": "api.example", "secret_name": "k"})}}}
	}
	member := func(r *http.Request) *http.Request {
		ctx := withOIDCRole(withOIDCHuman(r.Context(), "member-sub"), oidc.RoleUser)
		// A member whose request actor carries no principal: its secret owner is empty.
		return r.WithContext(withTicketActor(ctx, ticketActor{actorType: types.ActorHuman, role: oidc.RoleUser}))
	}
	operator := func(r *http.Request) *http.Request {
		return r.WithContext(withOIDCRole(withOIDCHuman(r.Context(), "admin-sub"), oidc.RoleAdmin))
	}
	for _, tc := range []struct {
		name    string
		as      func(*http.Request) *http.Request
		refused bool
	}{
		{"a member whose owner resolves empty", member, true},
		{"an operator", operator, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := grants()
			e := &overrideEdits{}
			r := tc.as(httptest.NewRequest(http.MethodPost, "/", nil))
			refusal := (&Server{}).applySecretOverrides(t.Context(), r, &spec, client.AgentOverrides{RemoveSecrets: []string{"k"}}, e)
			if tc.refused {
				if refusal == nil || refusal.body.Reason != reasonOverrideRefused || len(spec.EligibleGrants) != 1 {
					t.Fatalf("refusal = %+v, grants = %v, want %s and the grant kept", refusal, spec.EligibleGrants, reasonOverrideRefused)
				}
				return
			}
			if refusal != nil || len(spec.EligibleGrants) != 0 {
				t.Fatalf("an operator's removal = %+v, grants = %v, want the grant dropped", refusal, spec.EligibleGrants)
			}
		})
	}
}
