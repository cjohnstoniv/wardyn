// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

// TestConfinementFloorsValidate: an empty floor and CC1 to CC3 are valid on every trait; anything else, a
// lower-case class included, is refused naming its field.
func TestConfinementFloorsValidate(t *testing.T) {
	if err := (ConfinementFloors{OwnImage: CC1, SelfDefinedComponent: CC2, DataOutOpen: CC3}).Validate(); err != nil {
		t.Errorf("valid floors: %v", err)
	}
	if err := (ConfinementFloors{}).Validate(); err != nil {
		t.Errorf("empty floors: %v", err)
	}
	for _, tc := range []struct {
		field  string
		floors ConfinementFloors
	}{
		{"own_image", ConfinementFloors{OwnImage: "CC4"}},
		{"self_defined_component", ConfinementFloors{SelfDefinedComponent: "cc2"}},
		{"data_out_open", ConfinementFloors{DataOutOpen: "vault"}},
	} {
		if err := tc.floors.Validate(); err == nil || !strings.HasPrefix(err.Error(), tc.field+": ") {
			t.Errorf("%+v: err = %v, want one naming %s", tc.floors, err, tc.field)
		}
	}
}
