// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package scim

import (
	"encoding/json"
	"slices"
	"testing"
)

// FuzzFilter: ParseFilter never panics, a refusal is always a 400 invalidFilter, and an accepted filter
// names an allowed attribute and survives a render and re-parse unchanged.
func FuzzFilter(f *testing.F) {
	for _, s := range []string{
		`userName eq "bjensen"`, `externalId eq "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef"`, `emails.value eq "a@b.c"`,
		`displayName eq "g"`, `userName eq "a" and externalId eq "b"`, `userName eq "é\"\\"`, `userName eq`, `"`, ``,
		"userName\teq\t\"x\"", `USERNAME EQ "x"`, `userName eq "unterminated\`,
	} {
		f.Add(s)
	}
	allowed := []string{"userName", "externalId", "emails.value", "displayName"}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseFilter(s, allowed...)
		if err != nil {
			if err.Status != 400 || err.ScimType != TypeInvalidFilter || err.Detail == "" {
				t.Fatalf("%q: refusal is %+v", s, err)
			}
			return
		}
		if !slices.Contains(allowed, got.Attr) {
			t.Fatalf("%q: attribute %q is not allowed", s, got.Attr)
		}
		quoted, merr := json.Marshal(got.Value)
		if merr != nil {
			t.Fatal(merr)
		}
		again, err := ParseFilter(got.Attr+" eq "+string(quoted), allowed...)
		if err != nil || again != got {
			t.Fatalf("%q: round trip gave %+v, %v; want %+v", s, again, err, got)
		}
	})
}
