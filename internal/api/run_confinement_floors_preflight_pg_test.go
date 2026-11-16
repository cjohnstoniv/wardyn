// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_PreflightHoldsARunToItsConfinementFloor: a profile's data_out_open floor of CC3 refuses a run whose
// data_out is unset (open) and that asks for CC2 on a pool that enforces CC3, through POST /api/v1/runs/preflight
// and POST /api/v1/runs alike, with the same sentence; with no floor, neither answer carries it.
func TestPG_PreflightHoldsARunToItsConfinementFloor(t *testing.T) {
	const refusal = "weaker than the policy minimum CC3"
	paths := []string{"/api/v1/runs/preflight", "/api/v1/runs"}
	e := newPlacementEnv(t)
	pool := e.createPool("Floored", types.RunnerPoolOrganization)
	e.addExecutors(pool.ID, "alpha", "beta")
	body := runBody(`"confinement_class":"CC2","runner_pool_id":"` + pool.ID.String() + `"`)
	for _, path := range paths {
		if w := e.call(e.person, http.MethodPost, path, body); strings.Contains(w.Body.String(), refusal) {
			t.Fatalf("%s with no floor = %d %s; want no floor refusal", path, w.Code, w.Body)
		}
	}
	e.assignProfile("floored", types.GovernanceLimits{ConfinementFloors: &types.ConfinementFloors{DataOutOpen: types.CC3}})
	for _, path := range paths {
		if w := e.call(e.person, http.MethodPost, path, body); w.Code < 400 || !strings.Contains(w.Body.String(), refusal) {
			t.Errorf("%s with a data_out_open floor of CC3 = %d %s; want the policy-minimum refusal", path, w.Code, w.Body)
		}
	}
}
