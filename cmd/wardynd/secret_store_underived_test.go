// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// The boot warning for pointer rows the store no longer derives names the
// settings that fix where values live, per store (#2807).
func TestUnderivedPointersWarningNamesTheSettings(t *testing.T) {
	for store, want := range map[string][]string{
		"vaultkv": {"WARDYN_VAULT_KV_MOUNT", "WARDYN_VAULT_KV_PREFIX"},
		"azurekv": {"WARDYN_AZURE_KV_PREFIX", "WARDYN_AZURE_KV_URL"},
	} {
		got := underivedPointersWarning(store)
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("warning for %s = %q; want it to name %s", store, got, w)
			}
		}
	}
}
