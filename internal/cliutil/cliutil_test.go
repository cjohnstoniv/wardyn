// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package cliutil

import "testing"

func TestEnvOr(t *testing.T) {
	t.Setenv("CLIUTIL_TEST_VAR", "")
	if got := EnvOr("CLIUTIL_TEST_VAR", "def"); got != "def" {
		t.Errorf("EnvOr(empty) = %q, want default", got)
	}
	t.Setenv("CLIUTIL_TEST_VAR", "set")
	if got := EnvOr("CLIUTIL_TEST_VAR", "def"); got != "set" {
		t.Errorf("EnvOr(set) = %q, want the env value", got)
	}
}

func TestSplitCSV(t *testing.T) {
	cases := map[string][]string{
		"":          nil,
		"  ":        nil,
		"a,b,c":     {"a", "b", "c"},
		"a, ,b":     {"a", "b"},
		" a , b ,c": {"a", "b", "c"},
		"a.com,":    {"a.com"},
		",,,":       nil,
	}
	for in, want := range cases {
		got := SplitCSV(in)
		if len(got) != len(want) {
			t.Errorf("SplitCSV(%q) = %#v, want %#v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("SplitCSV(%q) = %#v, want %#v", in, got, want)
				break
			}
		}
	}
}
