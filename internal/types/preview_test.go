// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestPreviewVisibilityValidAndAtMost(t *testing.T) {
	all := []PreviewVisibility{PreviewPrivate, PreviewOrg, PreviewPublic}
	for _, v := range all {
		if !v.Valid() {
			t.Errorf("%q is not Valid", v)
		}
	}
	for _, v := range []PreviewVisibility{"", "Private", "team", "public "} {
		if v.Valid() {
			t.Errorf("%q is Valid", v)
		}
		for _, c := range all {
			if v.AtMost(c) {
				t.Errorf("%q.AtMost(%q) = true for a value that is not a visibility", v, c)
			}
			if c.AtMost(v) {
				t.Errorf("%q.AtMost(%q) = true under a ceiling that is not a visibility", c, v)
			}
		}
	}
	for i, v := range all {
		for j, c := range all {
			if got, want := v.AtMost(c), i <= j; got != want {
				t.Errorf("%q.AtMost(%q) = %v, want %v", v, c, got, want)
			}
		}
	}
}
