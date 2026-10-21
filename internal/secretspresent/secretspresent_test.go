// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretspresent

import (
	"slices"
	"testing"
)

func TestEvaluate(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Inputs
		want []Reason
	}{
		{"nothing", Inputs{}, nil},
		{"resident grant", Inputs{ResidentGrants: 1}, []Reason{ResidentGrant}},
		{"takeover", Inputs{TakeoverHappened: true}, []Reason{Takeover}},
		{"sign-in store", Inputs{SigninBegun: true}, []Reason{SigninStore}},
		{"unreadable fails closed", Inputs{Unreadable: true}, []Reason{Unknown}},
		{"all", Inputs{ResidentGrants: 2, TakeoverHappened: true, SigninBegun: true, Unreadable: true},
			[]Reason{Unknown, ResidentGrant, Takeover, SigninStore}},
	} {
		present, got := Evaluate(tc.in)
		if present != (len(tc.want) > 0) || !slices.Equal(got, tc.want) {
			t.Errorf("%s: Evaluate = %v %v, want %v", tc.name, present, got, tc.want)
		}
	}
}
