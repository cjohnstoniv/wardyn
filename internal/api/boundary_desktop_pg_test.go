// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBoundaryDesktopBlocksCopyOutForARunThatAllowsIt (#2967): a boundary that blocks copy out blocks it
// for a run whose own policy allows it (a super admin's and the admin token's inline policy, which no
// profile clamps), with the boundary's warning; the directions it allows stay open. A run that names no
// desktop, which the boundary leaves as it was, gets no warning.
func TestBoundaryDesktopBlocksCopyOutForARunThatAllowsIt(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	c := seedBoundaryCorpus(t, e)
	w := testBoundary(types.BoundaryHardLimits)
	wall := allDesktopAllowed
	wall.CopyOut = types.TransferBlocked
	w.Spec.Desktop = &wall
	putBoundary(t, e, w)

	super := operatorCtx(boundarySuperSub, "boundary-super@corp.example", oidc.RoleAdmin)
	for name, ctx := range map[string]context.Context{"admin_run_inline": super, "admin_token_inline": context.Background()} {
		t.Run(name, func(t *testing.T) {
			got := resolveBoundaryCase(t, e.srv, c, boundaryCase{name, ctx, func(boundaryCorpus) createRunRequest {
				return createRunRequest{Agent: "claude-code", Repo: "acme/widgets",
					InlinePolicy: &types.RunPolicySpec{MinConfinementClass: types.CC1, Desktop: &allDesktopAllowed}}
			}})
			if !got.OK {
				t.Fatal("the boundary refused the run; it must narrow it")
			}
			if d := got.Spec.Desktop.Effective(); d != wall {
				t.Errorf("run desktop = %+v, want %+v (copy out blocked by the boundary)", d, wall)
			}
			if !slices.Contains(got.Warnings, "the Wardyn boundary narrowed desktop") {
				t.Errorf("warnings = %q, want the boundary's desktop warning", got.Warnings)
			}
		})
	}

	// A run that names no desktop already blocks copy out by default: nothing is narrowed or warned.
	got := resolveBoundaryCase(t, e.srv, c, boundaryCase{"no_desktop", super, func(boundaryCorpus) createRunRequest {
		return createRunRequest{Agent: "claude-code", Repo: "acme/widgets", InlinePolicy: &types.RunPolicySpec{MinConfinementClass: types.CC1}}
	}})
	if !got.OK || slices.Contains(got.Warnings, "the Wardyn boundary narrowed desktop") {
		t.Errorf("a run with no desktop: ok=%v warnings %q, want no desktop warning", got.OK, got.Warnings)
	}
}
