// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_BoundaryOverDenyAllProfile_AnswersPlacementDenied (#3446): a profile that denies every placement
// refuses a launch with 403 placement_denied, with or without a Wardyn boundary over it. The meet re-judged
// the base's deny_placements and answered 500 once a boundary narrowed anything.
func TestPG_BoundaryOverDenyAllProfile_AnswersPlacementDenied(t *testing.T) {
	const walled = "walled"
	for _, withBoundary := range []bool{false, true} {
		name := map[bool]string{false: "no boundary", true: "boundary in hard_limits"}[withBoundary]
		t.Run(name, func(t *testing.T) {
			e := newPlacementEnv(t)
			remote := e.createPool("Remote", types.RunnerPoolOrganization)
			e.addExecutors(remote.ID, "alpha", "beta")
			e.orgDefaults(fmt.Sprintf(`{"pool_id":%q}`, remote.ID))
			// Stored directly: the write door refuses a deny-all profile, so this is a row an older write or a
			// base change left behind.
			e.assignProfile(walled, types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementLocal, types.PlacementRemote}})
			if withBoundary {
				if _, err := e.pg.PutBoundary(context.Background(), testBoundary(types.BoundaryHardLimits), "seed"); err != nil {
					t.Fatalf("put boundary: %v", err)
				}
			}
			first := sameEverywhere(t, name, e.answers(e.person, runBody("")), http.StatusForbidden, string(placement.ReasonPlacementDenied))
			if !strings.Contains(first, walled) {
				t.Errorf("the refusal %s does not name the profile %q", first, walled)
			}
		})
	}
}
