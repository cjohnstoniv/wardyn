// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

// TestRefuseRepeatedJSONKeys: a key written twice in one object is refused at any depth, named by
// its path, also when one spelling escapes a letter; the same key in two objects, and a number past
// float64's range, are fine.
func TestRefuseRepeatedJSONKeys(t *testing.T) {
	for src, want := range map[string]string{
		`{"a":1,"a":2}`: `the body has the key "a" twice`,
		`{"spec":{"denied_domains":[],"denied_domains":["x"]}}`: `the body.spec has the key "denied_domains" twice`,
		`[{"n":1},{"n":1,"n":2}]`:                               `the body[1] has the key "n" twice`,
		`{"a":1,"\u0061":2}`:                                    `the body has the key "a" twice`,
	} {
		if err := RefuseRepeatedJSONKeys([]byte(src), "the body"); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
	for _, src := range []string{`{"a":{"n":1},"b":{"n":1}}`, `{"big":1e400}`, `[]`, `"text"`, `null`} {
		if err := RefuseRepeatedJSONKeys([]byte(src), "the body"); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
}
