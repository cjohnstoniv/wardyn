// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

func TestSafeImageRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"mirror.corp/wardyn-proxy:0.9.1-arm64":        true,
		"ghcr.io/cjohnstoniv/wardyn-proxy@sha256:abc": true,
		"":                       false,
		"img:1\nWARDYN_X=1":      false,
		"img:1 --privileged":     false,
		strings.Repeat("a", 513): false,
	} {
		if got := SafeImageRef(ref); got != want {
			t.Errorf("SafeImageRef(%q) = %v, want %v", ref, got, want)
		}
	}
}
