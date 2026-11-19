// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestDataOutValidValues(t *testing.T) {
	for _, r := range []TransferRule{"", TransferAllowed, TransferReview, TransferBlocked} {
		if !ValidDataOut(r) {
			t.Errorf("ValidDataOut(%q) = false, want true", r)
		}
	}
	for _, r := range []TransferRule{"open", "Allowed", "block"} {
		if ValidDataOut(r) {
			t.Errorf("ValidDataOut(%q) = true, want false", r)
		}
	}
}

func TestDataOutEmptyIsAllowed(t *testing.T) {
	if got := TransferRule("").DataOut(); got != TransferAllowed {
		t.Errorf(`"".DataOut() = %q, want allowed`, got)
	}
	// TransferStrictness ranks "" as blocked, so StricterTransfer of "" and review is blocked: every
	// comparison goes through DataOut() first.
	if got := StricterTransfer(TransferRule("").DataOut(), TransferReview); got != TransferReview {
		t.Errorf(`the stricter of "" and review = %q, want review (not blocked)`, got)
	}
}

func TestWithDataOut(t *testing.T) {
	blocked, allowed, review := TransferBlocked, TransferAllowed, TransferReview
	for _, tc := range []struct {
		name string
		d    *DesktopControls
		r    TransferRule
		want DesktopControls
	}{
		{"absent leaves the defaults", nil, "", DesktopControls{PasteIn: allowed, CopyOut: blocked, Upload: allowed, Download: blocked}},
		{"allowed leaves the rules as written", &DesktopControls{CopyOut: allowed, Download: allowed}, allowed,
			DesktopControls{PasteIn: allowed, CopyOut: allowed, Upload: allowed, Download: allowed}},
		{"review: download unset becomes review, copy out blocked", nil, review,
			DesktopControls{PasteIn: allowed, CopyOut: blocked, Upload: allowed, Download: review}},
		{"review: an explicit download blocked wins", &DesktopControls{Download: blocked}, review,
			DesktopControls{PasteIn: allowed, CopyOut: blocked, Upload: allowed, Download: blocked}},
		{"review: download allowed becomes review", &DesktopControls{CopyOut: allowed, Download: allowed}, review,
			DesktopControls{PasteIn: allowed, CopyOut: blocked, Upload: allowed, Download: review}},
		{"review: paste in and upload are untouched", &DesktopControls{PasteIn: blocked, Upload: blocked}, review,
			DesktopControls{PasteIn: blocked, CopyOut: blocked, Upload: blocked, Download: review}},
		{"blocked blocks copy out and download", &DesktopControls{CopyOut: allowed, Download: allowed}, blocked,
			DesktopControls{PasteIn: allowed, CopyOut: blocked, Upload: allowed, Download: blocked}},
		{"a value this build does not know reads as blocked", nil, "sealed",
			DesktopControls{PasteIn: allowed, CopyOut: blocked, Upload: allowed, Download: blocked}},
	} {
		if got := WithDataOut(tc.d, tc.r); got != tc.want {
			t.Errorf("%s: WithDataOut = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestBoundaryOverlayCarriesDataOut(t *testing.T) {
	r := TransferReview
	if got := (BoundarySpec{DataOut: &r}).Overlay().DataOut; got == nil || *got != TransferReview {
		t.Errorf("BoundarySpec.Overlay().DataOut = %v, want review", got)
	}
	if got := (BoundarySpec{}).Overlay().DataOut; got != nil {
		t.Errorf("an unset boundary data_out overlays as %v, want nil", got)
	}
}

func TestRestrictionOverlayCarriesDataOut(t *testing.T) {
	r := TransferBlocked
	if got := (RestrictionSpec{DataOut: &r}).Overlay().DataOut; got == nil || *got != TransferBlocked {
		t.Errorf("RestrictionSpec.Overlay().DataOut = %v, want blocked", got)
	}
	if got := (RestrictionSpec{}).Overlay().DataOut; got != nil {
		t.Errorf("an unset restriction data_out overlays as %v, want nil", got)
	}
}
