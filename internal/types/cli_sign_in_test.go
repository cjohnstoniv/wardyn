// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// The user code is compared as a person types it: any letter case, with or without the hyphen and spaces.
func TestCLIUserCodeMatches(t *testing.T) {
	for _, c := range []struct {
		code, typed string
		want        bool
	}{
		{"KQ7M-2XJD", "KQ7M-2XJD", true},
		{"KQ7M-2XJD", "kq7m2xjd", true},
		{"KQ7M-2XJD", " kq7m 2xjd ", true},
		{"KQ7M-2XJD", "KQ7M-2XJE", false},
		{"KQ7M-2XJD", "", false},
		{"", "", false},
		{"KQ7M-2XJD", "KQ7M-2XJDX", false},
	} {
		if got := CLIUserCodeMatches(c.code, c.typed); got != c.want {
			t.Errorf("CLIUserCodeMatches(%q, %q) = %v, want %v", c.code, c.typed, got, c.want)
		}
	}
}

// With the code-entry switch off the create answer and the approve page's data carry no new key.
func TestCLISignInCodeEntryOffAddsNoKey(t *testing.T) {
	for _, v := range []any{CLISignInCreateResponse{}, CLISignInView{}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "code_entry") {
			t.Errorf("%T with the switch off = %s, want no code_entry", v, b)
		}
	}
	for _, v := range []any{CLISignInCreateResponse{CodeEntry: true}, CLISignInView{CodeEntry: true}} {
		b, _ := json.Marshal(v)
		if !strings.Contains(string(b), `"code_entry":true`) {
			t.Errorf("%T with the switch on = %s, want code_entry true", v, b)
		}
	}
}
