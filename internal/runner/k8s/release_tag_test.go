// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import "testing"

// releaseTagCases is the shared table of image tags: cmd/wardyn-runnerd/proxy_image_test.go holds the
// same rows for releaseVersion, so the two patterns are checked on the same inputs.
var releaseTagCases = map[string]bool{
	"1.2.3":          true,
	"0.10.0":         true,
	"0.10.0-rc.2":    true,
	"10.20.30-rc.0":  true,
	"":               false,
	"1.2":            false,
	"1.2.3.4":        false,
	"v1.2.3":         false,
	"1.2.3-rc":       false,
	"1.2.3-rc.":      false,
	"1.2.3-rc.x":     false,
	"1.2.3-beta.1":   false,
	"1.2.3+abc123":   false,
	"1.2.3-rc.1+abc": false,
	" 1.2.3":         false,
	"1.2.3\n":        false,
	"latest":         false,
	"main":           false,
}

func TestReleaseTagRETable(t *testing.T) {
	for tag, want := range releaseTagCases {
		if got := releaseTagRE.MatchString(tag); got != want {
			t.Errorf("releaseTagRE.MatchString(%q) = %v, want %v", tag, got, want)
		}
	}
}
