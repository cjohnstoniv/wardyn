// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

// The digest every minted token is stored and checked in: lower-case hex of SHA-256, byte for byte.
func TestTokenDigestGolden(t *testing.T) {
	for token, want := range map[string]string{
		"":    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"abc": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	} {
		if got := TokenDigest(token); got != want {
			t.Errorf("TokenDigest(%q) = %s, want %s", token, got, want)
		}
	}
}

// The match answers the last equal digest, -1 for none, and reads every digest whichever one matches.
func TestMatchDigest(t *testing.T) {
	a, b := TokenDigest("a"), TokenDigest("b")
	for _, tc := range []struct {
		name    string
		got     string
		digests []string
		want    int
	}{
		{"none", TokenDigest("c"), []string{a, b}, -1},
		{"empty", a, nil, -1},
		{"first", a, []string{a, b}, 0},
		{"last of two equal", a, []string{a, b, a}, 2},
		{"another length", a[:10], []string{a}, -1},
	} {
		reads := 0
		got := MatchDigest(tc.got, len(tc.digests), func(i int) string { reads++; return tc.digests[i] })
		if got != tc.want || reads != len(tc.digests) {
			t.Errorf("%s: MatchDigest = %d after %d reads; want %d after %d", tc.name, got, reads, tc.want, len(tc.digests))
		}
	}
}
