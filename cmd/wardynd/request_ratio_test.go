// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestParseRequestRatio(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"", 0, false}, {"0.5", 0.5, false}, {"1", 1, false},
		{"1.5", 0, true}, {"0", 0, true}, {"-0.2", 0, true}, {"abc", 0, true}, {"NaN", 0, true},
	} {
		got, err := parseRequestRatio(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parseRequestRatio(%q) = %v, %v; want %v, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}
