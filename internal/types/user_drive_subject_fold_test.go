// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"
	"unicode"
)

// DriveSubjectFoldsOntoASCII is true exactly for the non-ASCII runes that
// lower-case into a-z, found by walking every Unicode code point rather than
// from a hand list, and only in the part of the subject the template derives a
// home from.
func TestDriveSubjectFoldsOntoASCII(t *testing.T) {
	var folding []rune
	for r := rune(unicode.MaxASCII + 1); r <= unicode.MaxRune; r++ {
		if l := unicode.ToLower(r); l >= 'a' && l <= 'z' {
			folding = append(folding, r)
		}
	}
	if len(folding) < 2 {
		t.Fatalf("the walk found %q; want at least U+0130 and U+212A", folding)
	}
	seen := map[rune]bool{}
	for _, r := range folding {
		seen[r] = true
		for _, tmpl := range []HomeTemplate{HomeTemplateHash, HomeTemplateSub, HomeTemplateEmailLocal} {
			if !DriveSubjectFoldsOntoASCII(tmpl, "ab"+string(r)+"c@corp.example") {
				t.Errorf("%s: a subject holding U+%04X (folds to %q) was not flagged", tmpl, r, unicode.ToLower(r))
			}
		}
	}
	if !seen['İ'] || !seen['K'] {
		t.Errorf("the walk missed U+0130 or U+212A: %q", folding)
	}
	for _, s := range []string{"kelly", "Kelly", "sub-0042", "kelly@corp.example", "anaé", "ß", "中文"} {
		if DriveSubjectFoldsOntoASCII(HomeTemplateHash, s) {
			t.Errorf("%q was flagged; it folds onto no ASCII letter", s)
		}
	}
	// email_local derives from the local part only.
	if DriveSubjectFoldsOntoASCII(HomeTemplateEmailLocal, "kelly@Korp.example") {
		t.Error("email_local flagged a look-alike in the domain, which names no home")
	}
	if !DriveSubjectFoldsOntoASCII(HomeTemplateHash, "kelly@Korp.example") {
		t.Error("hash missed a look-alike anywhere in the subject it hashes")
	}
}
