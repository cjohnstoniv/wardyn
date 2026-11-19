// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func downloadRule(r types.TransferRule) *types.DesktopControls {
	return &types.DesktopControls{Download: r}
}

// TestLeqDesktopReview: review sits between allowed and blocked. A download held for review is narrower than
// an allowed one and wider than a blocked one (download's default), and an unknown value is never wider.
func TestLeqDesktopReview(t *testing.T) {
	for _, tc := range []struct {
		a, b *types.DesktopControls
		want bool
	}{
		{downloadRule(types.TransferReview), downloadRule(types.TransferAllowed), true},
		{downloadRule(types.TransferAllowed), downloadRule(types.TransferReview), false},
		{downloadRule(types.TransferReview), downloadRule(types.TransferReview), true},
		{downloadRule(types.TransferBlocked), downloadRule(types.TransferReview), true},
		{downloadRule(types.TransferReview), downloadRule(types.TransferBlocked), false},
		{downloadRule(types.TransferReview), nil, false},
		{downloadRule("sometimes"), downloadRule(types.TransferBlocked), true},
	} {
		if got := leqDesktop(tc.a, tc.b); got != tc.want {
			t.Errorf("leqDesktop(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestClampDesktopReview: a member under a ceiling that holds downloads for review cannot allow them, and one
// that asks for review under a ceiling that blocks them is blocked.
func TestClampDesktopReview(t *testing.T) {
	for _, tc := range []struct {
		proposed, ceiling *types.DesktopControls
		want              types.TransferRule
		warns             int
	}{
		{downloadRule(types.TransferAllowed), downloadRule(types.TransferReview), types.TransferReview, 1},
		{downloadRule(types.TransferReview), downloadRule(types.TransferAllowed), types.TransferReview, 0},
		{downloadRule(types.TransferReview), downloadRule(types.TransferBlocked), types.TransferBlocked, 1},
		{downloadRule(types.TransferBlocked), downloadRule(types.TransferReview), types.TransferBlocked, 0},
		{nil, downloadRule(types.TransferReview), types.TransferReview, 0},
	} {
		out := types.RunPolicySpec{Desktop: tc.proposed}
		warns := clampDesktop(&out, types.RunPolicySpec{Desktop: tc.ceiling}, nil)
		if got := out.Desktop.Effective().Download; got != tc.want || len(warns) != tc.warns {
			t.Errorf("clamp(%+v under %+v) = %q %q, want %q and %d warning(s)", tc.proposed, tc.ceiling, got, warns, tc.want, tc.warns)
		}
	}
}

// TestMeetOverlayAcceptsReview: an overlay may hold downloads for review over a base that allows them, may not
// over one that blocks them (download's default), and may not write review on another direction.
func TestMeetOverlayAcceptsReview(t *testing.T) {
	open := Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, Desktop: downloadRule(types.TransferAllowed)}}
	review := Overlay{Ceiling: types.CeilingOverlay{Desktop: downloadRule(types.TransferReview)}}
	if err := ValidateOverlay(open, review); err != nil {
		t.Fatalf("review over an allowed download refused: %v", err)
	}
	got, _, err := ApplyOverlay(open, review)
	if err != nil || got.Ceiling.Desktop.Effective().Download != types.TransferReview {
		t.Fatalf("resolve = %+v %v, want download review", got.Ceiling.Desktop, err)
	}
	shut := Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2}}
	if err := ValidateOverlay(shut, review); err == nil {
		t.Fatal("an overlay opened a blocked download to review")
	}
	got, warns, err := ApplyOverlay(shut, review)
	if err != nil || got.Ceiling.Desktop.Effective().Download != types.TransferBlocked || len(warns) != 1 {
		t.Fatalf("resolve must narrow review back to blocked, with a warning: %+v %q %v", got.Ceiling.Desktop, warns, err)
	}
	for _, d := range []*types.DesktopControls{{CopyOut: types.TransferReview}, {PasteIn: types.TransferReview}, {Upload: types.TransferReview}} {
		if err := ValidateOverlay(open, Overlay{Ceiling: types.CeilingOverlay{Desktop: d}}); err == nil {
			t.Errorf("an overlay wrote review on %+v", d)
		}
	}
}
