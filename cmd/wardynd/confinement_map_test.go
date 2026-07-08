// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestParseConfinementMap(t *testing.T) {
	t.Run("empty_is_nil", func(t *testing.T) {
		m, err := parseConfinementMap("")
		if err != nil || m != nil {
			t.Fatalf("empty => (nil,nil); got (%v,%v)", m, err)
		}
	})

	t.Run("class_runtime_and_substrate_prefix", func(t *testing.T) {
		m, err := parseConfinementMap("CC2=runsc; CC3=oci:kata-qemu")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if m[types.CC2] != "runsc" || m[types.CC3] != "kata-qemu" {
			t.Fatalf("got %v", m)
		}
	})

	// Fail-closed cases: a typo must error at boot, never silently downgrade.
	for _, bad := range []string{
		"CC9=foo",          // unknown class
		"garbage",          // no '='
		"=runsc",           // empty class
		"CC3=",             // empty runtime
		"CC3=smolvm:thing", // non-oci substrate (future driver, not a runtime)
	} {
		t.Run("rejects_"+bad, func(t *testing.T) {
			if _, err := parseConfinementMap(bad); err == nil {
				t.Fatalf("parseConfinementMap(%q) must fail closed", bad)
			}
		})
	}
}
