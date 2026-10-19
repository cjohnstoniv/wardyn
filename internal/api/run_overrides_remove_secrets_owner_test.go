// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
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
			refusal := (&Server{}).applySecretOverrides(t.Context(), r, &spec, client.AgentOverrides{RemoveSecrets: []string{"k"}}, e, false)
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

// On stored content a member's removal by name matches only a grant bound to
// their own row: a same-name grant on another row reads exactly as a miss
// (same spec, same edits), while their own content still drops it.
func TestOverrides_RemoveSecretsOnStoredContentMatchesOnlyOwnRowGrants(t *testing.T) {
	const sub = "member-remove-sub"
	secrets := &memSecrets{m: map[string][]byte{}}
	if err := secrets.For(sub).Put(t.Context(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	srv := &Server{cfg: Config{Secrets: secrets}}
	grant := func(ownerOnly bool) types.RunPolicySpec {
		return types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, OwnerOnly: ownerOnly,
			Scope: mustJSON(map[string]any{"host": "api.example", "secret_name": "k"})}}}
	}
	remove := func(spec types.RunPolicySpec, stored bool) (types.RunPolicySpec, *overrideEdits) {
		t.Helper()
		e := &overrideEdits{}
		r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(memberCtxFor(sub))
		if refusal := srv.applySecretOverrides(r.Context(), r, &spec, client.AgentOverrides{RemoveSecrets: []string{"k"}}, e, stored); refusal != nil {
			t.Fatalf("refusal = %+v, want none: the member owns k", refusal)
		}
		return spec, e
	}
	missSpec, missEdits := remove(types.RunPolicySpec{}, true)
	hidden, hiddenEdits := remove(grant(false), true)
	if len(hidden.EligibleGrants) != 1 || !reflect.DeepEqual(hiddenEdits, missEdits) || len(missSpec.EligibleGrants) != 0 {
		t.Errorf("stored same-name grant on another row: grants %v, edits %+v; want it kept and the edits of a miss %+v",
			hidden.EligibleGrants, hiddenEdits, missEdits)
	}
	if own, _ := remove(grant(true), true); len(own.EligibleGrants) != 0 {
		t.Errorf("stored grant bound to the member's own row was kept: %v", own.EligibleGrants)
	}
	if inline, _ := remove(grant(false), false); len(inline.EligibleGrants) != 0 {
		t.Errorf("the member's own inline grant was kept: %v", inline.EligibleGrants)
	}
}
