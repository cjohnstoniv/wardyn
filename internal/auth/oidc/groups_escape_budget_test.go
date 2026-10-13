// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// json.Marshal writes & < > as six-byte \u00XX escapes, so a snapshot of groups made of them must still be
// counted at its marshalled size, or the cookie outgrows the budget.
func TestSessionGroupsBudgetCountsHTMLEscapes(t *testing.T) {
	var claim []string
	for i := 0; i < 400; i++ {
		claim = append(claim, strings.Repeat("&<>", 3)+string(rune('a'+i%26))+strings.Repeat("x", i/26))
	}
	kept := oidc.SessionGroupsForTest(nil, claim)
	if len(kept) == 0 || len(kept) == len(claim) {
		t.Fatalf("kept %d of %d groups, want some but not all", len(kept), len(claim))
	}
	b, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > oidc.MaxSessionGroupsBytesForTest {
		t.Errorf("kept groups marshal to %d bytes, over the %d budget", len(b), oidc.MaxSessionGroupsBytesForTest)
	}
}
