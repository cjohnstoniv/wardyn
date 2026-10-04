// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keydomain

import (
	"maps"
	"testing"
)

// Each declared domain is described by where its key is, trimmed; a domain that names neither kind
// has no description.
func TestGapCovKeyDescriptions(t *testing.T) {
	f := File{
		"acme": {Transit: &Transit{Key: "  acme-keys "}},
		"beta": {AzureKV: &AzureKV{Key: " https://beta.vault.azure.net/keys/wrap ", SigningKey: "https://beta.vault.azure.net/keys/sign"}},
		"none": {},
	}
	want := map[string]string{
		"acme": "Transit key acme-keys",
		"beta": "Key Vault key https://beta.vault.azure.net/keys/wrap",
	}
	if got := f.KeyDescriptions(); !maps.Equal(got, want) {
		t.Fatalf("KeyDescriptions = %v, want %v", got, want)
	}
	if got := (File{}).KeyDescriptions(); len(got) != 0 {
		t.Fatalf("no domains described as %v", got)
	}
}
