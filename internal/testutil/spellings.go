// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Spellings is every way stored bytes could spell s that a reader undoes with
// no key: s itself, its hex in either case, and its base64 in either alphabet,
// padded or not, at each of the three byte alignments it could start on. The
// characters the bytes before and after s reach into are cut off, so a match is
// s's own bytes whatever surrounds them. Use it to assert that something is NOT
// stored: a test that looks only for s misses s base64-encoded.
func Spellings(s string) []string {
	h := hex.EncodeToString([]byte(s))
	out := []string{s, h, strings.ToUpper(h)}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		for shift, cut := range []int{0, 2, 3} {
			b := strings.TrimRight(enc.EncodeToString(append(make([]byte, shift), s...)), "=")
			out = append(out, b[cut:len(b)-2])
		}
	}
	return out
}
