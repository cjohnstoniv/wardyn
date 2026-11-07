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

// TestSeqDigest_Vectors pins SeqDigest to digests computed with printf and sha256sum alone (the recipe in its
// doc comment), starting from s_0 = sha256("wardyn-audit-seq-digest/1").
func TestSeqDigest_Vectors(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		seqs   []int64
		hashes []string
		want   string
	}{
		{nil, nil, "e5a6baecd1274051718eb4b4191bbb6e93cfd62e431e9e606fa90458ee394c67"},
		{[]int64{1, 12}, []string{a, b}, "01cd142272da2957949e2bca3acdcd441f9d06a7eb57b9bb507fbe42ef6f215b"},
		{[]int64{3}, []string{""}, "f38767be285f1d2961b2d799cb97f36e2affaeec1e0bd70fb0d3a75e93cd7b6e"},
		{[]int64{1}, []string{a, b}, ""},
	} {
		if got := SeqDigest(tc.seqs, tc.hashes); got != tc.want {
			t.Errorf("SeqDigest(%v, %d hashes) = %q, want %q", tc.seqs, len(tc.hashes), got, tc.want)
		}
	}
	// The pairing is what is digested: the same hashes under swapped seqs differ.
	if SeqDigest([]int64{4, 7}, []string{a, b}) == SeqDigest([]int64{7, 4}, []string{a, b}) {
		t.Fatal("swapped seqs give the same digest")
	}
}
