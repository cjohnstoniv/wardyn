// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	openCopyOut = &types.DesktopControls{CopyOut: types.TransferAllowed}
	shutPasteIn = &types.DesktopControls{PasteIn: types.TransferBlocked}
)

// TestDesktopLeq: unset is the default (copy out and download blocked), so a policy that opens copy
// out is wider than one that says nothing, and blocking more is always narrower.
func TestDesktopLeq(t *testing.T) {
	for _, tc := range []struct {
		a, b *types.DesktopControls
		want bool
	}{
		{nil, nil, true},
		{openCopyOut, nil, false},
		{nil, openCopyOut, true},
		{shutPasteIn, nil, true},
		{nil, shutPasteIn, false},
		{&types.DesktopControls{CopyOut: types.TransferBlocked}, nil, true},
	} {
		base := types.RunPolicySpec{MinConfinementClass: types.CC2}
		a, b := base, base
		a.Desktop, b.Desktop = tc.a, tc.b
		if got := Leq(a, b, types.GovernanceLimits{}, types.GovernanceLimits{}); got != tc.want {
			t.Errorf("Leq(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestDesktopClampBlocksWhatTheCeilingBlocks: a member cannot open copy out under a ceiling that
// leaves it at the default, nor paste in under one that blocks it; a proposal already within the
// ceiling is left exactly as written, so a policy with no desktop setting stays without one.
func TestDesktopClampBlocksWhatTheCeilingBlocks(t *testing.T) {
	for _, tc := range []struct {
		proposed, ceiling, want *types.DesktopControls
		warns                   int
	}{
		{nil, nil, nil, 0},
		{nil, openCopyOut, nil, 0},
		{openCopyOut, openCopyOut, openCopyOut, 0},
		{openCopyOut, nil, &types.DesktopControls{PasteIn: "allowed", CopyOut: "blocked", Upload: "allowed", Download: "blocked"}, 1},
		{nil, shutPasteIn, &types.DesktopControls{PasteIn: "blocked", CopyOut: "blocked", Upload: "allowed", Download: "blocked"}, 1},
	} {
		out := types.RunPolicySpec{Desktop: tc.proposed}
		warns := clampDesktop(&out, types.RunPolicySpec{Desktop: tc.ceiling}, nil)
		if !reflect.DeepEqual(out.Desktop, tc.want) || len(warns) != tc.warns {
			t.Errorf("clamp(%+v under %+v) = %+v %q, want %+v and %d warning(s)", tc.proposed, tc.ceiling, out.Desktop, warns, tc.want, tc.warns)
		}
	}
}

// TestDesktopMeet: an overlay may block a direction; one that opens a direction the base blocks is
// refused at write and narrowed back at resolve.
func TestDesktopMeet(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2}}
	got, _, err := ApplyOverlay(base, Overlay{Ceiling: types.CeilingOverlay{Desktop: shutPasteIn}})
	if err != nil || got.Ceiling.Desktop.Effective().PasteIn != types.TransferBlocked {
		t.Fatalf("an overlay blocking paste in: %+v %v", got.Ceiling.Desktop, err)
	}
	opened := Overlay{Ceiling: types.CeilingOverlay{Desktop: openCopyOut}}
	if err := ValidateOverlay(base, opened); err == nil {
		t.Fatal("a write opened copy out over a base that blocks it by default")
	}
	got, warns, err := ApplyOverlay(base, opened)
	if err != nil || got.Ceiling.Desktop.Effective().CopyOut != types.TransferBlocked || len(warns) != 1 {
		t.Fatalf("resolve must narrow an opened copy out back to blocked, with a warning: %+v %q %v", got.Ceiling.Desktop, warns, err)
	}
	bad := &types.DesktopControls{Download: "sometimes"}
	if err := ValidateOverlay(base, Overlay{Ceiling: types.CeilingOverlay{Desktop: bad}}); err == nil {
		t.Fatal("an overlay wrote a rule that is neither allowed nor blocked")
	}
}
