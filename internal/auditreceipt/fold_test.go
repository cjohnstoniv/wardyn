// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"strings"
	"testing"
)

// TestFold_Vectors pins Fold to digests computed with sha256sum alone, the operator's hand recipe:
// d_0 = sha256(seed), d_i = sha256(d_{i-1} || row_hash_i), every value lowercase hex text. The store's
// fold (store.NewFoldDigest) is pinned to the same step by TestFold_SameStepAsPartitionDigest.
func TestFold_Vectors(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		seed   string
		hashes []string
		want   string
	}{
		{"", nil, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"", []string{a, b}, "aa15cd3f0802376650ebc56322cdec1bf6f6990ed2602dd0648bbd8b10646195"},
		{"seedtext", []string{a}, "61e9a542a3a9d95fa7f77b3c9fb65e300021bd75f92e86c9c8ed371bf8d68bb9"},
	} {
		if got := Fold(tc.seed, tc.hashes); got != tc.want {
			t.Errorf("Fold(%q, %d hashes) = %s, want %s", tc.seed, len(tc.hashes), got, tc.want)
		}
	}
}
