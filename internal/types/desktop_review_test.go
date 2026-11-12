// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

// TestTransferStrictnessOrder: allowed < review < blocked, and a value this build does not know ranks as
// blocked, so a rule written by a newer build never loosens a direction.
func TestTransferStrictnessOrder(t *testing.T) {
	if !(TransferStrictness(TransferAllowed) < TransferStrictness(TransferReview) &&
		TransferStrictness(TransferReview) < TransferStrictness(TransferBlocked)) {
		t.Fatal("strictness is not allowed < review < blocked")
	}
	for _, r := range []TransferRule{"sometimes", "REVIEW", ""} {
		if TransferStrictness(r) != TransferStrictness(TransferBlocked) {
			t.Errorf("TransferStrictness(%q) = %d, want blocked's", r, TransferStrictness(r))
		}
	}
}

// TestDesktopMeetReviewNeverLoosens: the meet keeps the stricter rule of each direction, review included,
// and an unknown value meets as blocked.
func TestDesktopMeetReviewNeverLoosens(t *testing.T) {
	for _, tc := range []struct {
		a, b, want TransferRule
	}{
		{TransferAllowed, TransferReview, TransferReview},
		{TransferReview, TransferAllowed, TransferReview},
		{TransferReview, TransferReview, TransferReview},
		{TransferReview, TransferBlocked, TransferBlocked},
		{TransferBlocked, TransferReview, TransferBlocked},
		{TransferAllowed, TransferAllowed, TransferAllowed},
		{TransferAllowed, "sometimes", TransferBlocked},
		{TransferReview, "sometimes", TransferBlocked},
	} {
		a, b := &DesktopControls{Download: tc.a}, &DesktopControls{Download: tc.b}
		if got := a.Meet(b).Download; got != tc.want {
			t.Errorf("download %q meet %q = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
	if got := (&DesktopControls{Download: TransferReview}).Meet(nil).Download; got != TransferBlocked {
		t.Errorf("review meet the default = %q, want blocked (download's default)", got)
	}
}

// TestDesktopReviewOnlyOnDownload: review is a rule for download alone; on the other three directions it is
// refused, as is any value that is not a rule.
func TestDesktopReviewOnlyOnDownload(t *testing.T) {
	if err := ValidateDesktopControls(&DesktopControls{Download: TransferReview}); err != nil {
		t.Fatalf("download review refused: %v", err)
	}
	if err := ValidateDesktopControls(nil); err != nil {
		t.Fatalf("no desktop refused: %v", err)
	}
	for field, d := range map[string]DesktopControls{
		"paste_in": {PasteIn: TransferReview},
		"copy_out": {CopyOut: TransferReview},
		"upload":   {Upload: TransferReview},
		"download": {Download: "sometimes"},
	} {
		err := ValidateDesktopControls(&d)
		if err == nil || !strings.Contains(err.Error(), "desktop."+field) {
			t.Errorf("%s: %v, want a refusal naming desktop.%s", field, err, field)
		}
	}
}
