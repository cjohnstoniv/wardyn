// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestCanonicalJSON(t *testing.T) {
	for in, want := range map[string]string{
		` { "b" : 1 , "a" : [ 1.50 , 1e2 , -0 , 0.10 ] } `: `{"a":[1.5,100,0,0.1],"b":1}`,
		`{"k":"a/b<&>\n \"\\","k":"last"}`:                 `{"k":"last"}`,
		`"a/b<&>\n\"\\"`:                                   `"a/b<&>` + `\u000a` + `\"\\"`,
		`{"x":1e5000,"y":null,"z":[true,false,{}]}`:        `{"x":1e5000,"y":null,"z":[true,false,{}]}`,
	} {
		got, err := CanonicalJSON([]byte(in))
		if err != nil || string(got) != want {
			t.Errorf("CanonicalJSON(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{``, `{`, `1 2`, `{"a":1} x`} {
		if _, err := CanonicalJSON([]byte(bad)); err == nil {
			t.Errorf("CanonicalJSON(%q) = nil error, want one", bad)
		}
	}
}
