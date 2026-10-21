// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestValidDesktopSignins(t *testing.T) {
	for v, want := range map[string]bool{"on": true, "off": true, "": false, "true": false, "ON": false, "yes": false} {
		if got := validDesktopSignins(v); got != want {
			t.Errorf("validDesktopSignins(%q) = %v, want %v", v, got, want)
		}
	}
}
