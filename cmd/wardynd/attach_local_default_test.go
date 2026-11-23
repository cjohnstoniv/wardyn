// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// attachLocalOn is main.go's reading of -attach-local into Config.AttachLocal.
// TestAttachLocalReadingMatchesMain keeps the two the same.
func attachLocalOn(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "on") }

// TestAttachLocalDefaultsOn pins WARDYN_ATTACH_LOCAL's default, on, and the boundaries an upgrade
// meets: unset and empty take the default (FlagEnv ignores an empty value); any set value is kept, so
// off, a whitespace-only value and any other word keep every attach on the relay; and an explicit
// blank flag wins over the environment.
func TestAttachLocalDefaultsOn(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	str := func(s string) *string { return &s }
	cases := []struct {
		name string
		env  *string // nil: unset
		args []string
		raw  string
		on   bool
	}{
		{name: "unset", raw: "on", on: true},
		{name: "empty env", env: str(""), raw: "on", on: true},
		{name: "off", env: str("off"), raw: "off", on: false},
		{name: "whitespace only", env: str("   "), raw: "   ", on: false},
		{name: "on", env: str("on"), raw: "on", on: true},
		{name: "ON with spaces", env: str(" ON "), raw: " ON ", on: true},
		{name: "explicit blank flag", env: str("on"), args: []string{"-attach-local="}, raw: "", on: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.env == nil {
				ensureUnset(t, "WARDYN_ATTACH_LOCAL")
			} else {
				t.Setenv("WARDYN_ATTACH_LOCAL", *c.env)
			}
			os.Args = append([]string{"wardynd-test"}, c.args...)
			resetFlags(t)
			got := *parseBootFlags().attachLocal
			if got != c.raw || attachLocalOn(got) != c.on {
				t.Fatalf("-attach-local = %q (on %v), want %q (on %v)", got, attachLocalOn(got), c.raw, c.on)
			}
		})
	}
}

// TestAttachLocalReadingMatchesMain pins main.go's reading of the flag to attachLocalOn's, so the
// cases above say what wardynd does with each value.
func TestAttachLocalReadingMatchesMain(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`AttachLocal:\s+strings\.EqualFold\(strings\.TrimSpace\(\*f\.attachLocal\), "on"\),`)
	if !re.Match(src) {
		t.Fatal(`main.go no longer sets Config.AttachLocal to strings.EqualFold(strings.TrimSpace(*f.attachLocal), "on"); update attachLocalOn to match`)
	}
}
