// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package dockerutil

import (
	"slices"
	"testing"
)

func TestDiscardedLimits(t *testing.T) {
	const mem = "Your kernel does not support memory limit capabilities or the cgroup is not mounted. Limitation discarded."
	for _, tt := range []struct {
		name string
		in   []string
		want []string
	}{
		{"none", nil, nil},
		{"no discard", []string{"Memory limited without swap."}, nil},
		{"discard", []string{mem}, []string{mem}},
		{"case and whitespace", []string{"  LIMIT DISCARDED \n"}, []string{"LIMIT DISCARDED"}},
		{"mixed", []string{"Memory limited without swap.", mem}, []string{mem}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := DiscardedLimits(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("DiscardedLimits(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
