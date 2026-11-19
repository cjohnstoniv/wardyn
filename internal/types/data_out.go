// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// DataOut is the run's rule for content leaving it: allowed, review (files leave one at a time through
// the Files panel, each with a person's yes) or blocked. "" reads as allowed.
func (r TransferRule) DataOut() TransferRule {
	if r == "" {
		return TransferAllowed
	}
	return r
}

// ValidDataOut is a value data_out may hold: "", allowed, review or blocked.
func ValidDataOut(r TransferRule) bool {
	return r == "" || r == TransferAllowed || r == TransferReview || r == TransferBlocked
}

// StricterTransfer is the stricter of x and y (allowed < review < blocked), an unknown value read as blocked.
func StricterTransfer(x, y TransferRule) TransferRule { return stricterTransfer(x, y) }

// WithDataOut is the desktop rules d (as the run wrote them, nil for none) under data_out r: copy out is
// blocked unless r is allowed; download is the stricter of the two, except that review on a download d
// leaves unset is review (a reviewed download is data_out's one way out). Paste in and upload are untouched.
func WithDataOut(d *DesktopControls, r TransferRule) DesktopControls {
	e := d.Effective()
	if r = r.DataOut(); r == TransferAllowed {
		return e
	}
	e.CopyOut = TransferBlocked
	if r == TransferReview && (d == nil || d.Download == "") {
		e.Download = TransferReview
		return e
	}
	e.Download = StricterTransfer(e.Download, r)
	return e
}
