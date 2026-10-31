// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestMacOSBaseAllowed(t *testing.T) {
	for _, tc := range []struct {
		allowed []string
		base    string
		want    bool
	}{
		{nil, "xcode-16", true},
		{nil, "", true},
		{[]string{"Standard"}, "", true},
		{[]string{"Standard"}, "Standard", true},
		{[]string{"Standard"}, "xcode-16", false},
		{[]string{"xcode-16"}, "", false},
	} {
		if got := MacOSBaseAllowed(tc.allowed, tc.base); got != tc.want {
			t.Errorf("MacOSBaseAllowed(%v, %q) = %v", tc.allowed, tc.base, got)
		}
	}
	for name, want := range map[string]bool{"Standard": true, "xcode-16": true, "qa.profiles_2": true, "standard": true, "Xcode": false, "../x": false, "": false, "-x": false} {
		if got := ValidMacOSBaseName(name); got != want {
			t.Errorf("ValidMacOSBaseName(%q) = %v", name, got)
		}
	}
}
