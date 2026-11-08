// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDesktopDownloadLockHoldsReview: a lock on desktop.download fixes it at review when the locker holds
// downloads for review: a unit below that allows or blocks them is refused naming the locker, one that states
// review or leaves it unset is not.
func TestDesktopDownloadLockHoldsReview(t *testing.T) {
	srv := New(Config{AdminToken: adminToken})
	b := types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{Desktop: &types.DesktopControls{Download: types.TransferReview}},
		LockedKnobs: []string{"desktop.download"}}
	for _, c := range []struct {
		name string
		d    types.DesktopControls
		want string
	}{
		{"allowing", types.DesktopControls{Download: types.TransferAllowed}, "desktop.download is locked by the Wardyn boundary"},
		{"blocking", types.DesktopControls{Download: types.TransferBlocked}, "desktop.download is locked by the Wardyn boundary"},
		{"review", types.DesktopControls{Download: types.TransferReview}, ""},
		{"unset", types.DesktopControls{CopyOut: types.TransferBlocked}, ""},
	} {
		if got := lockRefusal(t, srv, desktopUnit("team", nil, c.d), nil, b); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: refusal %q, want %q", c.name, got, c.want)
		}
	}
}

// TestBoundaryDesktopReviewOnDownloadOnly: a boundary may hold downloads for review, and no other direction.
func TestBoundaryDesktopReviewOnDownloadOnly(t *testing.T) {
	good := types.BoundaryWrite{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{Desktop: &types.DesktopControls{Download: types.TransferReview}}}
	if err := validateBoundary(&good); err != nil {
		t.Fatalf("validateBoundary(download review) = %v", err)
	}
	bad := types.BoundaryWrite{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{Desktop: &types.DesktopControls{CopyOut: types.TransferReview}}}
	if err := validateBoundary(&bad); err == nil || !strings.Contains(err.Error(), "desktop.copy_out") {
		t.Fatalf("validateBoundary(copy_out review) = %v, want desktop.copy_out refused", err)
	}
}
