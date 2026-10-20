// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func walls(sandbox, approvals string) *types.HarnessInnerWalls {
	return &types.HarnessInnerWalls{SandboxMode: sandbox, Approvals: approvals}
}

// TestInnerWallsLeq: a set axis narrows an unset one, and two different choices are incomparable,
// so a composed profile can add an inner wall and never drop or swap one.
func TestInnerWallsLeq(t *testing.T) {
	wardyn, none := types.InnerApprovalsWardyn, types.InnerApprovalsNone
	for _, tc := range []struct {
		a, b *types.HarnessInnerWalls
		want bool
	}{
		{nil, nil, true},
		{walls("", wardyn), nil, true},
		{nil, walls("", wardyn), false},
		{walls(types.InnerSandboxOff, wardyn), walls("", wardyn), true},
		{walls("", none), walls("", wardyn), false},
		{walls("", wardyn), walls(types.InnerSandboxOff, ""), false},
	} {
		base := types.RunPolicySpec{MinConfinementClass: types.CC2}
		a, b := base, base
		a.HarnessInnerWalls, b.HarnessInnerWalls = tc.a, tc.b
		if got := Leq(a, b, types.GovernanceLimits{}, types.GovernanceLimits{}); got != tc.want {
			t.Errorf("Leq(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestInnerWallsClampKeepsTheCeilingsChoice: a member's inline policy cannot drop or swap an axis
// the ceiling chooses, and keeps its own choice on an axis the ceiling leaves unset.
func TestInnerWallsClampKeepsTheCeilingsChoice(t *testing.T) {
	wardyn, none := types.InnerApprovalsWardyn, types.InnerApprovalsNone
	for _, tc := range []struct {
		proposed, ceiling, want *types.HarnessInnerWalls
		warns                   int
	}{
		{nil, nil, nil, 0},
		{walls("", none), nil, walls("", none), 0},
		{nil, walls("", wardyn), walls("", wardyn), 0},
		{walls("", none), walls("", wardyn), walls("", wardyn), 1},
		{walls(types.InnerSandboxOff, ""), walls("", wardyn), walls(types.InnerSandboxOff, wardyn), 0},
	} {
		out := types.RunPolicySpec{HarnessInnerWalls: tc.proposed}
		warns := clampInnerWalls(&out, types.RunPolicySpec{HarnessInnerWalls: tc.ceiling}, nil)
		got := out.HarnessInnerWalls
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) || len(warns) != tc.warns {
			t.Errorf("clamp(%+v under %+v) = %+v %q, want %+v and %d warning(s)", tc.proposed, tc.ceiling, got, warns, tc.want, tc.warns)
		}
	}
}

// TestInnerWallsMeet: an overlay adds an axis the base leaves unset, agrees with one it sets, and a
// different choice, or a value no policy may choose, refuses the composition.
func TestInnerWallsMeet(t *testing.T) {
	wardyn, none := types.InnerApprovalsWardyn, types.InnerApprovalsNone
	for _, tc := range []struct {
		base, over, want *types.HarnessInnerWalls
		fails            bool
	}{
		{nil, walls("", wardyn), walls("", wardyn), false},
		{walls(types.InnerSandboxOff, ""), walls("", wardyn), walls(types.InnerSandboxOff, wardyn), false},
		{walls("", wardyn), walls("", wardyn), walls("", wardyn), false},
		{walls("", wardyn), walls("", none), nil, true},
		{nil, walls(types.InnerSandboxReadOnly, ""), nil, true},
	} {
		base := Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, HarnessInnerWalls: tc.base}}
		got, _, err := ApplyOverlay(base, Overlay{Ceiling: types.CeilingOverlay{HarnessInnerWalls: tc.over}})
		if tc.fails {
			if err == nil {
				t.Errorf("meet(%+v, %+v) = %+v, want a refusal", tc.base, tc.over, got.Ceiling.HarnessInnerWalls)
			}
			continue
		}
		if w := got.Ceiling.HarnessInnerWalls; err != nil || w == nil || *w != *tc.want {
			t.Errorf("meet(%+v, %+v) = %+v %v, want %+v", tc.base, tc.over, w, err, tc.want)
		}
	}
}
