// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

// The file name is joined onto a fixed directory, so it must be one path
// element that cannot climb out of it. Pinned on the pattern itself, which is
// the whole rule Validate applies to the name.
func TestComponentFileToken(t *testing.T) {
	for _, ok := range []string{"a", "service-account.json", "9.pem", "a_b.c-d", "a" + strings.Repeat("b", 62)} {
		if !componentFileTokenRE.MatchString(ok) {
			t.Errorf("file token refuses %q", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", ".env", "-x", "_x", "a/b", "../a", "a\\b", "A", "a b", "a\n", "a" + strings.Repeat("b", 63)} {
		if componentFileTokenRE.MatchString(bad) {
			t.Errorf("file token accepts %q", bad)
		}
	}
}
