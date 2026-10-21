// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY on, PUT updates a profile
// that exists and answers 404 for an id no row holds; off, it still creates.
func TestGovernanceUnitPut_CreatesOnlyWithTheSwitchOff(t *testing.T) {
	existing := types.GovernanceUnit{ID: uuid.New(), Name: "interns"}
	for _, on := range []bool{false, true} {
		st := &govCrudStore{profiles: []types.GovernanceUnit{existing}}
		srv, _ := govCrudServer(t, st)
		srv.cfg.GovernanceUnitPutExistingOnly = on

		absent := uuid.New()
		w := do(t, srv, http.MethodPut, "/api/v1/governance/units/"+absent.String(), adminToken, `{"name":"new","ceiling":{"min_confinement_class":"CC2"}}`)
		switch {
		case on && (w.Code != http.StatusNotFound || errorReason(w) != reasonGovernanceUnitNotFoundByID || st.lastProfile.ID == absent):
			t.Errorf("switch on: PUT to an absent id = %d %s (stored %s), want 404 %s and no row", w.Code, w.Body.String(), st.lastProfile.ID, reasonGovernanceUnitNotFoundByID)
		case !on && (w.Code != http.StatusOK || st.lastProfile.ID != absent):
			t.Errorf("switch off: PUT to an absent id = %d %s, want 200 and a row at that id", w.Code, w.Body.String())
		}
		if w := do(t, srv, http.MethodPut, "/api/v1/governance/units/"+existing.ID.String(), adminToken, `{"name":"interns-2","ceiling":{"min_confinement_class":"CC2"}}`); w.Code != http.StatusOK || st.lastProfile.ID != existing.ID {
			t.Errorf("switch %v: PUT to an existing id = %d %s, want 200", on, w.Code, w.Body.String())
		}
	}
}
