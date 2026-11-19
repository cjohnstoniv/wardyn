// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDenyListRemovalIsHeld: taking an id off a profile's deny list is a widening, so a human's
// update waits for a second human like any widening; adding an id is a narrowing and applies at
// once. Both lists behave alike.
func TestDenyListRemovalIsHeld(t *testing.T) {
	const idA, idB = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	e := newGovEnv(t)
	body := func(name, field string, ids ...string) string {
		b, _ := json.Marshal(map[string]any{
			"name":    name,
			"ceiling": map[string]any{"allowed_domains": []string{"pypi.org"}, "min_confinement_class": "CC2"},
			"limits":  map[string]any{field: ids},
		})
		return string(b)
	}
	for _, field := range []string{"deny_components", "deny_model_providers"} {
		t.Run(field, func(t *testing.T) {
			limits := func(ids ...string) types.GovernanceLimits {
				if field == "deny_components" {
					return types.GovernanceLimits{DenyComponents: ids}
				}
				return types.GovernanceLimits{DenyModelProviders: ids}
			}
			got := func(u types.GovernanceUnit) []string {
				if field == "deny_components" {
					return u.Limits.DenyComponents
				}
				return u.Limits.DenyModelProviders
			}
			unit := e.seedProfile("walls-"+field, "pypi.org")
			unit.Limits = limits(idA)
			unit, err := e.pg.UpsertGovernanceUnit(context.Background(), unit)
			if err != nil {
				t.Fatalf("seed list: %v", err)
			}
			path := "/api/v1/governance/units/" + unit.ID.String()

			if w := e.call(e.alice, http.MethodPut, path, body(unit.Name, field, idA, idB)); w.Code != http.StatusOK {
				t.Fatalf("adding an id = %d %s; want 200 applied directly", w.Code, w.Body)
			}
			if l := got(e.profile(unit.ID)); len(l) != 2 {
				t.Errorf("the added id did not apply: %v", l)
			}
			removal := e.pending(e.call(e.alice, http.MethodPut, path, body(unit.Name, field, idA)))
			if l := got(e.profile(unit.ID)); len(l) != 2 {
				t.Errorf("a held removal was applied: %v", l)
			}
			// A unit holds one pending change at a time: the proposer withdraws the removal first.
			if w := e.call(e.alice, http.MethodPost, rejectPath(removal.ID), ""); w.Code != http.StatusOK {
				t.Fatalf("withdrawing the held removal = %d %s, want 200", w.Code, w.Body)
			}
			e.pending(e.call(e.alice, http.MethodPut, path, body(unit.Name, field)))
			if l := got(e.profile(unit.ID)); len(l) != 2 {
				t.Errorf("a held emptying was applied: %v", l)
			}
		})
	}
}
