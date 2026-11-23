// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

import "testing"

func TestNextTurn(t *testing.T) {
	cases := []struct {
		name  string
		edits []Edit
		want  string
	}{
		{"none", nil, "hello"},
		{"one", []Edit{{"hosts", "a.example"}}, "The person changed hosts to a.example.\nhello"},
		{"three", []Edit{{"a", "1"}, {"b", "2"}, {"c", "3"}},
			"The person changed a to 1.\nThe person changed b to 2.\nThe person changed c to 3.\nhello"},
	}
	for _, c := range cases {
		if got := NextTurn("hello", c.edits); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
