// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import "testing"

// In a dotenv template a quoted empty value or a comment alone after '=' is no default: the name
// must be supplied, as with a bare KEY=. A quoted value is a default.
func TestScan_DotenvQuotedEmptyOrCommentOnlyIsRequired(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env.example", `DQ_EMPTY=""`+"\n"+
		`SQ_EMPTY=''`+"\n"+
		`SPACED_EMPTY= ""  # set me`+"\n"+
		"COMMENT_ONLY= # your key\n"+
		`DQ_DEFAULT="value"`+"\n"+
		`SQ_DEFAULT='value'`+"\n")
	got := Scan(dir)
	for name, optional := range map[string]bool{
		"DQ_EMPTY": false, "SQ_EMPTY": false, "SPACED_EMPTY": false, "COMMENT_ONLY": false,
		"DQ_DEFAULT": true, "SQ_DEFAULT": true,
	} {
		if n := needByName(t, got.RequiredSecrets, name); n.Optional != optional {
			t.Errorf("%s optional = %v, want %v", name, n.Optional, optional)
		}
	}
}

// -Xmx values that name the same heap in k, m or g read the same, and a huge value in any unit
// reads as the ceiling: the unit is applied before the cap.
func TestXmxToMiB_AppliesTheUnitBeforeTheCap(t *testing.T) {
	for _, tc := range []struct {
		num, unit string
		want      int
	}{
		{"1048576", "k", 1024}, {"1024", "m", 1024}, {"1", "g", 1024}, {"1", "G", 1024},
		{"2097152", "K", 2048}, {"512", "k", 0},
		{"268435456", "k", maxBuildMemMiB}, {"999999999999999999999999", "k", maxBuildMemMiB},
		{"300000", "m", maxBuildMemMiB}, {"300", "g", maxBuildMemMiB}, {"999999999999999999999999", "g", maxBuildMemMiB},
	} {
		if got := xmxToMiB(tc.num, tc.unit); got != tc.want {
			t.Errorf("xmxToMiB(%q, %q) = %d, want %d", tc.num, tc.unit, got, tc.want)
		}
	}
}
