// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidAllowedModels(t *testing.T) {
	many := make([]string, MaxAllowedModels+1)
	for i := range many {
		many[i] = fmt.Sprintf("model-%d", i)
	}
	for _, c := range []struct {
		name string
		list []string
		want string
	}{
		{"empty is no limit", nil, ""},
		{"two names", []string{"model-a", "small-model-a"}, ""},
		{"listed twice", []string{"model-a", "model-a"}, `"model-a" is listed twice`},
		{"empty entry", []string{"model-a", ""}, "entry 2 is empty"},
		{"space", []string{" model-a"}, `" model-a" has leading or trailing space`},
		{"control", []string{"model\x00a"}, "entry 1 has a control character"},
		{"too long", []string{strings.Repeat("m", MaxAllowedModelLen+1)}, "entry 1 is longer than 200 characters"},
		{"too many", many, "at most 200 models"},
	} {
		err := ValidAllowedModels(c.list)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
