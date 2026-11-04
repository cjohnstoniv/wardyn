// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pctdecode

import "testing"

// The two scope decoders moved here from their own packages; these rows were
// taken from the old copies' output so the move changes nothing.
func TestLenient(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"%41", "A"},
		{"%2f", "/"},
		{"%2F", "/"},
		{"%5c", `\`},
		{"%252F", "%2F"},
		{"%00", "\x00"},
		{"%ff", "\xff"},
		{"%e9", "\xe9"},
		{"é%41", "éA"},
		{"%41%42%43", "ABC"},
		// Malformed escapes stay literal.
		{"%", "%"},
		{"100%", "100%"},
		{"%4", "%4"},
		{"a%2", "a%2"},
		{"%41%", "A%"},
		{"%41%4", "A%4"},
		{"%4G", "%4G"},
		{"%G4", "%G4"},
		{"%zz", "%zz"},
		{"%+1", "%+1"},
		{"%-1", "%-1"},
		{"% 4", "% 4"},
		{"%%", "%%"},
		{"%%%", "%%%"},
		{"%%41", "%A"},
		{"%4%41", "%4A"},
		{"%\x00\x00", "%\x00\x00"},
		{"%é", "%é"},
	} {
		if got := Lenient(tc.in); got != tc.want {
			t.Errorf("Lenient(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUnhex(t *testing.T) {
	for c := 0; c < 256; c++ {
		want := -1
		switch {
		case c >= '0' && c <= '9':
			want = c - '0'
		case c >= 'a' && c <= 'f':
			want = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			want = c - 'A' + 10
		}
		if got := Unhex(byte(c)); got != want {
			t.Errorf("Unhex(%#x) = %d, want %d", c, got, want)
		}
	}
}
