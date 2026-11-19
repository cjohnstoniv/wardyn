// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"slices"
	"testing"
)

func TestEnvelopeValidate(t *testing.T) {
	many := make([]string, MaxEnvelopeGroups+1)
	for i := range many {
		many[i] = fmt.Sprintf("g-%d", i)
	}
	for _, c := range []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{"none is a valid envelope", nil, []string{}, false},
		{"folded and sorted", []string{" Plat-B ", "plat-a"}, []string{"plat-a", "plat-b"}, false},
		{"at the limit", many[:MaxEnvelopeGroups], many[:MaxEnvelopeGroups], false},
		{"over the limit", many, nil, true},
		{"empty group", []string{"plat-a", "  "}, nil, true},
		{"non-ASCII look-alike", []string{"plat-K"}, nil, true},
		{"duplicate after folding", []string{"Plat-A", "plat-a"}, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateEnvelope(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("ValidateEnvelope(%q) error = %v, want error %v", c.in, err, c.wantErr)
			}
			if !c.wantErr && !slices.Equal(got, slices.Sorted(slices.Values(c.want))) {
				t.Errorf("ValidateEnvelope(%q) = %q, want %q", c.in, got, c.want)
			}
			if !c.wantErr && got == nil {
				t.Error("a valid envelope must return a non-nil list")
			}
		})
	}
}
