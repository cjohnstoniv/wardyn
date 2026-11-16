// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

func TestDerivedOperationKeyFits(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	for key, want := range map[string]bool{
		"step-1":              true,
		"calls:1":             true,
		"call:" + sha + ":1":  true,
		"call:" + sha + ":12": true,
		"call:":               false,
		"call:" + sha:         false,
		"call:" + sha + ":":   false,
		"call:" + sha + ":0":  false,
		"call:" + sha + ":01": false,
		"call:" + sha + ":1a": false,
		"call:" + sha + ":-1": false,
		"call:" + strings.Repeat("cd", 32) + ":1": false,
	} {
		if got := DerivedOperationKeyFits(key, sha); got != want {
			t.Errorf("DerivedOperationKeyFits(%q) = %v, want %v", key, got, want)
		}
	}
	if DerivedOperationKeyFits("call::1", "") {
		t.Error("an empty hash fits a made key")
	}
}
