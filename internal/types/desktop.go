// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "cmp"

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
	TransferBlocked TransferRule = "blocked"
)

// ValidTransferRule reports whether r is a value a policy may write ("" is the default).
func ValidTransferRule(r TransferRule) bool {
	return r == "" || r == TransferAllowed || r == TransferBlocked
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

// Meet is the narrower of d and o in every direction.
func (d *DesktopControls) Meet(o *DesktopControls) DesktopControls {
	a, b := d.Effective(), o.Effective()
	block := func(x, y TransferRule) TransferRule {
		if x == TransferBlocked || y == TransferBlocked {
			return TransferBlocked
		}
		return TransferAllowed
	}
	return DesktopControls{
		PasteIn: block(a.PasteIn, b.PasteIn), CopyOut: block(a.CopyOut, b.CopyOut),
		Upload: block(a.Upload, b.Upload), Download: block(a.Download, b.Download),
	}
}
