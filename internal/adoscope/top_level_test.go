// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"errors"
	"strings"
	"testing"
)

func TestTopLevelString(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
		err              bool
		missing          bool
	}{
		{name: "plain", body: `{"model":"m1","messages":[{"model":"nested"}]}`, want: "m1"},
		{name: "after a large nested value", body: `{"messages":[{"a":[1,2,{"b":null}]}],"model":"m1"}`, want: "m1"},
		{name: "escaped", body: `{"model":"m1"}`, want: "m1"},
		{name: "missing", body: `{"messages":[]}`, err: true, missing: true},
		{name: "wrong case is missing", body: `{"Model":"m1"}`, err: true, missing: true},
		{name: "nested only is missing", body: `{"x":{"model":"m1"}}`, err: true, missing: true},
		{name: "duplicate", body: `{"model":"m1","model":"m2"}`, err: true},
		{name: "duplicate in another case", body: `{"model":"m1","MODEL":"m2"}`, err: true},
		{name: "duplicate in a nested object", body: `{"model":"m1","x":{"a":1,"a":2}}`, err: true},
		{name: "not a string", body: `{"model":1}`, err: true},
		{name: "empty string", body: `{"model":""}`, err: true},
		{name: "array body", body: `[{"model":"m1"}]`, err: true},
		{name: "two documents", body: `{"model":"m1"}{"model":"m2"}`, err: true},
		{name: "truncated", body: `{"model":"m1"`, err: true},
		{name: "empty", body: ``, err: true},
		{name: "too deep", body: `{"model":"m1","x":` + strings.Repeat("[", maxJSONDepth+2) + strings.Repeat("]", maxJSONDepth+2) + `}`, err: true},
	} {
		got, err := TopLevelString([]byte(c.body), "model")
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%s: TopLevelString = %q, %v; want %q, err=%v", c.name, got, err, c.want, c.err)
		}
		if c.missing && !errors.Is(err, ErrKeyMissing) {
			t.Errorf("%s: err = %v, want ErrKeyMissing", c.name, err)
		}
	}
}
