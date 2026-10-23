// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import "testing"

func TestValidMethodEntry(t *testing.T) {
	for _, ok := range []string{"GET", "post", " Put ", "GTE", "M-SEARCH", "PROPFIND"} {
		if err := ValidMethodEntry(ok); err != nil {
			t.Errorf("ValidMethodEntry(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "  ", "GET /", "GE T", "GET,POST", "ÜBER"} {
		if err := ValidMethodEntry(bad); err == nil {
			t.Errorf("ValidMethodEntry(%q) = nil, want a refusal", bad)
		}
	}
}
