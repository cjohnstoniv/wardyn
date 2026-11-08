// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"cmp"
	"fmt"
)

// DesktopControls sets what may cross a desktop's edge, one rule per direction (#2967). The
// clipboard rules are enforced at Wardyn's desktop relay, outside the sandbox; the file rules at
// the console's transfer endpoints. Nil, or an empty field, is that direction's default: paste in
// and upload allowed, copy out and download blocked. A layer below can only block more.
type DesktopControls struct {
	PasteIn  TransferRule `json:"paste_in,omitempty"`
	CopyOut  TransferRule `json:"copy_out,omitempty"`
	Upload   TransferRule `json:"upload,omitempty"`
	Download TransferRule `json:"download,omitempty"`
}

// TransferRule is one direction's rule.
type TransferRule string

const (
	TransferAllowed TransferRule = "allowed"
	// TransferReview holds each download for a person's yes on that file (download only): the Files panel asks,
	// the file_download deciders see its name, size and hash, and an approval releases those bytes once. Opt in;
	// download's default stays blocked.
	TransferReview  TransferRule = "review"
	TransferBlocked TransferRule = "blocked"
)

// ValidTransferRule reports whether r is a value a policy may write on paste_in, copy_out or upload ("" is the
// default). Download also takes review (ValidateDesktopControls).
func ValidTransferRule(r TransferRule) bool {
	return r == "" || r == TransferAllowed || r == TransferBlocked
}

// TransferStrictness ranks r: allowed < review < blocked. A value this build does not know ranks as blocked, so
// a rule a newer build wrote never loosens a direction here.
func TransferStrictness(r TransferRule) int {
	switch r {
	case TransferAllowed:
		return 0
	case TransferReview:
		return 1
	}
	return 2
}

// stricterTransfer is the stricter of x and y, an unknown value read as blocked.
func stricterTransfer(x, y TransferRule) TransferRule {
	return [...]TransferRule{TransferAllowed, TransferReview, TransferBlocked}[max(TransferStrictness(x), TransferStrictness(y))]
}

// ValidateDesktopControls refuses a direction that is not a rule: allowed or blocked, and review on download
// alone.
func ValidateDesktopControls(d *DesktopControls) error {
	if d == nil {
		return nil
	}
	for _, dir := range []struct {
		field string
		rule  TransferRule
	}{{"paste_in", d.PasteIn}, {"copy_out", d.CopyOut}, {"upload", d.Upload}} {
		if dir.rule == TransferReview {
			return fmt.Errorf("desktop.%s: review applies to download only", dir.field)
		}
		if !ValidTransferRule(dir.rule) {
			return fmt.Errorf("desktop.%s: %q is not allowed or blocked", dir.field, dir.rule)
		}
	}
	if d.Download != TransferReview && !ValidTransferRule(d.Download) {
		return fmt.Errorf("desktop.download: %q is not allowed, review or blocked", d.Download)
	}
	return nil
}

// Effective is d with every unset direction at its default.
func (d *DesktopControls) Effective() DesktopControls {
	var e DesktopControls
	if d != nil {
		e = *d
	}
	e.PasteIn = cmp.Or(e.PasteIn, TransferAllowed)
	e.CopyOut = cmp.Or(e.CopyOut, TransferBlocked)
	e.Upload = cmp.Or(e.Upload, TransferAllowed)
	e.Download = cmp.Or(e.Download, TransferBlocked)
	return e
}

// Meet is the narrower of d and o in every direction (TransferStrictness).
func (d *DesktopControls) Meet(o *DesktopControls) DesktopControls {
	a, b := d.Effective(), o.Effective()
	return DesktopControls{
		PasteIn: stricterTransfer(a.PasteIn, b.PasteIn), CopyOut: stricterTransfer(a.CopyOut, b.CopyOut),
		Upload: stricterTransfer(a.Upload, b.Upload), Download: stricterTransfer(a.Download, b.Download),
	}
}
