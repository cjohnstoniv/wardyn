// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The pure rule behind T15-T16: the organisation's default wins only when it is at or above the floor
// and offered; the blast-radius raise to CC3 still follows and makes the class the server's.
func TestDefaultBarrierRule(t *testing.T) {
	all := []types.ConfinementClass{types.CC1, types.CC2, types.CC3}
	for _, tc := range []struct {
		name         string
		offered      []types.ConfinementClass
		floor, org   types.ConfinementClass
		want         types.ConfinementClass
		wantSource   runnerpool.BarrierDefaultSource
		needsVaultFn bool
	}{
		{"none set", all, types.CC1, "", types.CC3, runnerpool.BarrierDefaultServer, false},
		{"offered and above the floor", all, types.CC1, types.CC2, types.CC2, runnerpool.BarrierDefaultOrganisation, false},
		{"at the floor", all, types.CC2, types.CC2, types.CC2, runnerpool.BarrierDefaultOrganisation, false},
		{"below the floor", all, types.CC2, types.CC1, types.CC3, runnerpool.BarrierDefaultServer, false},
		{"not offered", []types.ConfinementClass{types.CC1, types.CC3}, types.CC1, types.CC2, types.CC3, runnerpool.BarrierDefaultServer, false},
		{"raised to Vault by what the run holds", all, types.CC1, types.CC2, types.CC3, runnerpool.BarrierDefaultServer, true},
	} {
		spec := types.RunPolicySpec{MinConfinementClass: tc.floor}
		if tc.needsVaultFn {
			spec.EligibleGrants = []types.GrantSpec{{Kind: types.GrantCloudSTS}}
			if composer.RequiredConfinementFloor(spec, composer.Baseline{}) != types.CC3 {
				t.Fatalf("%s: fixture holds nothing that needs Vault", tc.name)
			}
		}
		got, source, err := enforcedConfinement(spec, "", tc.offered, tc.org, composer.Baseline{})
		if err != nil || got != tc.want || source != tc.wantSource {
			t.Errorf("%s: enforced %q from %q (%v), want %q from %q", tc.name, got, source, err, tc.want, tc.wantSource)
		}
	}
	if got, source, _ := enforcedConfinement(types.RunPolicySpec{MinConfinementClass: types.CC1}, types.CC1, all, types.CC2, composer.Baseline{}); got != types.CC1 || source != "" {
		t.Errorf("a named class = %q from %q, want CC1 chosen by nobody", got, source)
	}
}
