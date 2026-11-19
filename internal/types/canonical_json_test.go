// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"strings"
	"testing"
)

// TestCanonicalJSONVectors pins the one canonical form the policy digest and the MCP call key share.
func TestCanonicalJSONVectors(t *testing.T) {
	for in, want := range map[string]string{
		` { "b" : 1 , "a" : [ 1.50 , 1e2 , -0 , 0.10 ] } `: `{"a":[1.5,100,0,0.1],"b":1}`,
		`"a/b<&>\n\"\\"`: `"a/b\u003c\u0026\u003e\n\"\\"`,
		`{"x":1e5000,"y":null,"z":[true,false,{}]}`: `{"x":1e5000,"y":null,"z":[true,false,{}]}`,
		`-0`:   `0`,
		`-0.0`: `0`,
		`1.50`: `1.5`,
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
	if _, err := CanonicalJSON([]byte(`{"k":"a/b<&>\n \"\\","k":"last"}`)); !errors.Is(err, ErrCanonicalRepeatedKey) {
		t.Errorf("a repeated key: err = %v, want ErrCanonicalRepeatedKey", err)
	}
}

// TestCanonicalNumberSpellings: two spellings of one value are one string, and two values are two.
func TestCanonicalNumberSpellings(t *testing.T) {
	same := [][2]string{
		{"1e1001", "1" + strings.Repeat("0", 1001)},
		{"0.000001", "1e-6"},
	}
	for _, p := range same {
		a, b := CanonicalNumber(p[0]), CanonicalNumber(p[1])
		if a != b {
			t.Errorf("CanonicalNumber(%s) = %s, CanonicalNumber(%.20s...) = %s; want one spelling", p[0], a, p[1], b)
		}
	}
	for in, want := range map[string]string{
		"1e1001": "1e1001",
		"1e-41":  "1e-41",
		"0":      "0",
		"-0.0":   "0",
	} {
		if got := CanonicalNumber(in); got != want {
			t.Errorf("CanonicalNumber(%s) = %s, want %s", in, got, want)
		}
	}
	if CanonicalNumber("1e-41") == CanonicalNumber("0") {
		t.Error("1e-41 and 0 have one spelling")
	}
}

// TestCanonicalJSONB: JSONB keeps a repeated key's last value, where the strict form refuses it.
func TestCanonicalJSONB(t *testing.T) {
	in := []byte(`{"host":"a","host":"b"}`)
	got, err := CanonicalJSONB(in)
	if err != nil || string(got) != `{"host":"b"}` {
		t.Errorf("CanonicalJSONB(%s) = %s, %v; want {\"host\":\"b\"}", in, got, err)
	}
	if _, err := CanonicalJSON(in); !errors.Is(err, ErrCanonicalRepeatedKey) {
		t.Errorf("CanonicalJSON(%s) err = %v, want ErrCanonicalRepeatedKey", in, err)
	}
	deep := strings.Repeat("[", 100) + strings.Repeat("]", 100)
	if _, err := CanonicalJSONB([]byte(deep)); err != nil {
		t.Errorf("CanonicalJSONB of 100 levels: %v", err)
	}
}
