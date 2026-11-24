// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateRunnerLabels_Table(t *testing.T) {
	t.Parallel()
	sixteen, seventeen := map[string]string{}, map[string]string{}
	for i := range MaxRunnerLabels + 1 {
		if i < MaxRunnerLabels {
			sixteen[fmt.Sprintf("k%d", i)] = "v"
		}
		seventeen[fmt.Sprintf("k%d", i)] = "v"
	}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		ok     bool
	}{
		{"none", nil, true},
		{"sixteen", sixteen, true},
		{"seventeen", seventeen, false},
		{"prefixed key", map[string]string{"example.com/cluster": "eu-1"}, true},
		{"empty value", map[string]string{"cluster": ""}, true},
		{"two slashes", map[string]string{"a/b/c": "v"}, false},
		{"leading dash", map[string]string{"-x": "v"}, false},
		{"empty key", map[string]string{"": "v"}, false},
		{"64-char name", map[string]string{strings.Repeat("a", 64): "v"}, false},
		{"63-char name", map[string]string{strings.Repeat("a", 63): "v"}, true},
		{"url value", map[string]string{"cluster": "https://x"}, false},
		{"colon value", map[string]string{"cluster": "a:b"}, false},
		{"space value", map[string]string{"cluster": "a b"}, false},
		{"newline value", map[string]string{"cluster": "eu-1\n"}, false},
		{"newline key", map[string]string{"cluster\n": "v"}, false},
		{"NUL value", map[string]string{"cluster": "a\x00b"}, false},
		{"tab value", map[string]string{"cluster": "a\tb"}, false},
		{"non-ASCII value", map[string]string{"cluster": "é"}, false},
		{"non-ASCII key", map[string]string{"é": "v"}, false},
		{"upper prefix", map[string]string{"Example.com/x": "v"}, false},
		{"long prefix", map[string]string{strings.Repeat("a.", 127) + "a/x": "v"}, false},
	} {
		if err := ValidateRunnerLabels(tc.labels); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
