// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"regexp"
	"testing"
)

// TestPushHoldRemovalsDefaultsOff pins WARDYN_PUSH_HOLD_REMOVALS through the real flag parser: off when
// unset or empty, on when set to true. It also pins main.go's line, so the reading is what api.Config gets.
func TestPushHoldRemovalsDefaultsOff(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	on := "true"
	for _, c := range []struct {
		name string
		env  *string // nil: unset
		want bool
	}{
		{"unset", nil, false},
		{"empty", new(string), false},
		{"true", &on, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ensureUnset(t, "WARDYN_PUSH_HOLD_REMOVALS")
			if c.env != nil {
				t.Setenv("WARDYN_PUSH_HOLD_REMOVALS", *c.env)
			}
			os.Args = []string{"wardynd-test"}
			resetFlags(t)
			if got := *parseBootFlags().pushHoldRemovals; got != c.want {
				t.Fatalf("WARDYN_PUSH_HOLD_REMOVALS (%s) reads as on=%v, want %v", c.name, got, c.want)
			}
		})
	}
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s*PushHoldRemovals:\s+\*f\.pushHoldRemovals,$`).Match(src) {
		t.Error("main.go no longer sets api.Config.PushHoldRemovals from -push-hold-removals")
	}
}
