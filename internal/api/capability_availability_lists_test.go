// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// "Available to" on the per-person lists (design 2.6, answer 1): a resource
// restricted away from a person is left out of the list the server hands them,
// never merely hidden by the console.

// policyListStore answers GET /policies from pols, fetch-all only.
type policyListStore struct {
	*capStore
	pols []types.RunPolicy
}

func (s *policyListStore) ListPolicies(context.Context) ([]types.RunPolicy, error) {
	return slices.Clone(s.pols), nil
}

// pagedPolicyStore is policyListStore as a store.Pager (production PG's
// shape). Only ListPoliciesPage is ever reached on the nil embedded Pager.
type pagedPolicyStore struct {
	*policyListStore
	store.Pager
	used bool
}

func (s *pagedPolicyStore) ListPoliciesPage(_ context.Context, p store.Page) ([]types.RunPolicy, error) {
	s.used = true
	items, _ := pageWindow(slices.Clone(s.pols), p.Offset, p.Limit)
	return items, nil
}

// TestAvailability_RestrictedPolicyLeftOutOfList: GET /policies, on both store
// shapes, leaves a stored policy out for a person its list does not name.
func TestAvailability_RestrictedPolicyLeftOutOfList(t *testing.T) {
	devOnly := types.RunPolicy{ID: uuid.New(), Name: "dev only"} // newest: first in the list
	open := types.RunPolicy{ID: uuid.New(), Name: "open"}
	restricted := func() *capStore {
		return &capStore{
			userTypes:  utKnown,
			restricted: restrictedOne(capPolicy, devOnly.ID.String()),
			grants:     []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, utDev, capPolicy, devOnly.ID.String(), types.CapabilityAllow)},
		}
	}
	// list serves GET /policies{query} as ctx; pagerUsed reports whether the
	// DB-side window ran.
	list := func(t *testing.T, cs *capStore, paged bool, ctx context.Context, query string) (names []string, truncated string, pagerUsed bool) {
		t.Helper()
		ls := &policyListStore{capStore: cs, pols: []types.RunPolicy{devOnly, open}}
		ps := &pagedPolicyStore{policyListStore: ls}
		h := newHarness(t)
		h.srv.cfg.Store = ls
		if paged {
			h.srv.cfg.Store = ps
		}
		w := httptest.NewRecorder()
		h.srv.handleListPolicies(w, httptest.NewRequest(http.MethodGet, "/api/v1/policies"+query, nil).WithContext(ctx))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /policies = %d: %s", w.Code, w.Body.String())
		}
		var got []types.RunPolicy
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v: %s", err, w.Body.String())
		}
		names = []string{}
		for _, p := range got {
			names = append(names, p.Name)
		}
		return names, w.Header().Get("X-Wardyn-Truncated"), ps.used
	}
	for _, paged := range []bool{false, true} {
		t.Run(map[bool]string{false: "fetch-all store", true: "paging store"}[paged], func(t *testing.T) {
			for _, leg := range []struct {
				name, role, typ string
				want            []string
			}{
				{"a type outside the list doesn't see it", oidc.RoleUser, utPM, []string{"open"}},
				{"the listed type sees it", oidc.RoleUser, utDev, []string{"dev only", "open"}},
				{"a security admin outside the list sees every row", oidc.RoleSecurityAdmin, utPM, []string{"dev only", "open"}},
				{"an admin sees every row", oidc.RoleAdmin, utPM, []string{"dev only", "open"}},
			} {
				t.Run(leg.name, func(t *testing.T) {
					if got, _, _ := list(t, restricted(), paged, utCtx(leg.role, leg.typ, []string{}), ""); !slices.Equal(got, leg.want) {
						t.Fatalf("policies = %q, want %q", got, leg.want)
					}
				})
			}
			t.Run("filtered before the window: a hidden row neither fills the page nor flags a next one", func(t *testing.T) {
				got, truncated, pagerUsed := list(t, restricted(), paged, utCtx(oidc.RoleUser, utPM, []string{}), "?limit=1")
				if !slices.Equal(got, []string{"open"}) || truncated != "" || pagerUsed {
					t.Fatalf("limit=1 = %q, truncated=%q, paged at the DB=%v; want [open], no next page, not paged before the filter",
						got, truncated, pagerUsed)
				}
			})
			t.Run("a resolver error hides the restricted row, even from the listed type", func(t *testing.T) {
				cs := restricted()
				cs.restrictErr = errors.New("pg down")
				if got, _, _ := list(t, cs, paged, utCtx(oidc.RoleUser, utDev, []string{}), ""); slices.Contains(got, "dev only") {
					t.Fatalf("policies = %q on a failed restriction read, want the restricted row hidden", got)
				}
			})
		})
	}
}

// TestAvailability_RestrictedModelProviderLeftOutOfSetupStatus: /setup/status's
// model_providers, and the provider_access rows graded from them, leave out a
// provider the caller's type is not listed for.
func TestAvailability_RestrictedModelProviderLeftOutOfSetupStatus(t *testing.T) {
	sc := types.SiteConfig{ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"), endpointProvider())}
	for _, leg := range []struct {
		name, typ string
		want      []string
	}{
		{"a type outside the list doesn't see it", utPM, []string{"anthropic"}},
		{"the listed type sees it", utDev, []string{"anthropic", "corp-gateway"}},
	} {
		t.Run(leg.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.Store = &capStore{userTypes: utKnown, restricted: restrictedOne(capModelProvider, "corp-gateway"),
				grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, utDev, capModelProvider, "corp-gateway", types.CapabilityAllow)}}
			mp, access, _ := h.srv.setupModelProviderState(utCtx(oidc.RoleUser, leg.typ, []string{}), sc, "")
			var ids, accessIDs []string
			for _, p := range mp {
				ids = append(ids, p.ID)
			}
			for _, a := range access {
				accessIDs = append(accessIDs, a.Provider)
			}
			if !slices.Equal(ids, leg.want) || !slices.Equal(accessIDs, leg.want) {
				t.Fatalf("model_providers = %q, provider_access = %q; want %q for both", ids, accessIDs, leg.want)
			}
		})
	}
}
